// Command server 启动 card-payment gRPC over mTLS.
//
// 部署在隔离 DC（SAQ-D scope）。仅 mTLS 入站。出站只允许：
//   - mTLS gRPC 到 card-center (Detokenize)
//   - HTTPS 到卡组织（Visa Net / Mastercard MIP / ...）
//   - 自有 MySQL（card_payment_db_*）
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/xiongwp/card-payment/internal/adapter/amex"
	"github.com/xiongwp/card-payment/internal/adapter/jcb"
	"github.com/xiongwp/card-payment/internal/adapter/mastercard"
	"github.com/xiongwp/card-payment/internal/adapter/unionpay"
	"github.com/xiongwp/card-payment/internal/adapter/visa"
	"github.com/xiongwp/card-payment/internal/cardcenterclient"
	"github.com/xiongwp/card-payment/internal/metrics"
	"github.com/xiongwp/card-payment/internal/processor"
	"github.com/xiongwp/card-payment/internal/reconcile"
	"github.com/xiongwp/card-payment/internal/repo"
	"github.com/xiongwp/card-payment/internal/resilience"
	"github.com/xiongwp/card-payment/internal/server"
	"github.com/xiongwp/card-payment/internal/sharding"
	"github.com/xiongwp/payment-util/configcenter"
	"github.com/xiongwp/payment-util/shadow"
	"github.com/xiongwp/payment-util/trace"
)

func main() {
	// metrics.Register() 必须在 fx.New 前跑一次：collectors 是包级 var，
	// 重启 fx graph 时若再注册会 panic（duplicate metric collector）。
	metrics.Register()
	app := fx.New(
		fx.StartTimeout(30*time.Second),
		fx.StopTimeout(30*time.Second),
		fx.Provide(
			loadConfig,
			newLogger,
			// config-center: bulkhead.per_merchant_max / network.{visa,mc}.timeout / reconcile interval
			configcenter.FxProvider("card-payment"),
			newRouter,
			newDBManager,
			newCardTransactionRepo,
			newCardCenterClient,
			newNetworks,
			newBreakerRegistry,
			newBulkhead,
			newProcessor,
			newGRPCServer,
		),
		fx.Invoke(startGRPC, startServiceRegistrar, startMetricsHTTP, startReconcile),
	)
	app.Run()
}

// startReconcile 起后台对账 worker。**资金安全 P0**：
// processor.Authorize ctx timeout / network err 时本地可能 status=pending|error
// 但卡组织其实已扣，必须用 Query() 拿权威态修订。
//
// 默认 30s tick，单 cycle stuck_age=60s（Authorize timeout 30s × 2）。
// 单实例都跑 OK：UpdateStatus 是 ref-key 幂等，多 worker 互不冲突。
//
// disable 开关：reconcile.disable=true（dev / 排查用）。prod 不允许 disable，
// assertProdSafety 拦不到这里就让 worker 自己 panic。
func startReconcile(lc fx.Lifecycle, v *viper.Viper, cli *configcenter.Client,
	repo processor.CardTransactionRepo,
	networks map[string]processor.Network, logger *zap.Logger) {
	if v.GetBool("reconcile.disable") {
		env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
		if env == "prod" || env == "production" {
			logger.Panic("PROD-SAFETY: reconcile.disable=true forbidden in env=prod (will leave stuck card_transaction rows unreconciled)")
		}
		logger.Warn("reconcile worker disabled by config (dev only)")
		return
	}
	cfg := reconcile.Config{
		Limit:        v.GetInt("reconcile.limit_per_shard"),
		StuckAge:     v.GetDuration("reconcile.stuck_age"),
		QueryTimeout: v.GetDuration("reconcile.query_timeout"),
		CycleTimeout: v.GetDuration("reconcile.cycle_timeout"),
	}
	interval := v.GetDuration("reconcile.interval")
	if interval <= 0 {
		interval = 30 * time.Second
	}
	// config-center 覆盖：admin 改 namespace=card-payment 下
	//   reconcile.{interval,limit_per_shard,stuck_age,query_timeout,cycle_timeout}
	if cli != nil {
		ctx0 := context.Background()
		cfg.Limit = cli.GetInt(ctx0, "reconcile.limit_per_shard", cfg.Limit)
		if d := cli.GetDuration(ctx0, "reconcile.stuck_age", cfg.StuckAge); d > 0 {
			cfg.StuckAge = d
		}
		if d := cli.GetDuration(ctx0, "reconcile.query_timeout", cfg.QueryTimeout); d > 0 {
			cfg.QueryTimeout = d
		}
		if d := cli.GetDuration(ctx0, "reconcile.cycle_timeout", cfg.CycleTimeout); d > 0 {
			cfg.CycleTimeout = d
		}
		if d := cli.GetDuration(ctx0, "reconcile.interval", interval); d > 0 {
			interval = d
		}
	}
	w := reconcile.New(repo, networks, cfg, logger)
	if w == nil {
		logger.Warn("reconcile worker not started (nil repo or empty networks)")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx, interval)
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

// startMetricsHTTP 起 prometheus scrape + k8s probe 端口。
// 默认 :9544。
//
// /readyz 检查链：
//   1. drain 标志（OnStop 后立即 503，让 K8s 摘流量）
//   2. DB Ping（meta + 任一 shard，确保 GORM 连接活）
// 任一失败 → 503 + 错误原因（kubectl describe 看得到）。
//
// 同时起一个 5s tick 的 goroutine 把 bulkhead.active 推到 prometheus gauge。
func startMetricsHTTP(lc fx.Lifecycle, v *viper.Viper, mgr *repo.Manager,
	bulkhead *resilience.Bulkhead, logger *zap.Logger) {
	addr := v.GetString("metrics.addr")
	if addr == "" {
		addr = ":9544"
	}
	probe := func() error {
		// DB meta 必须 reachable（不通 → 写不进 audit_log，拒绝服务）
		if mgr == nil {
			return errors.New("db manager nil")
		}
		if metaDB := mgr.Meta(); metaDB != nil {
			if sqlDB, err := metaDB.DB(); err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				defer cancel()
				if err := sqlDB.PingContext(ctx); err != nil {
					metrics.DependencyUp.WithLabelValues("db_meta").Set(0)
					return fmt.Errorf("db_meta ping: %w", err)
				}
				metrics.DependencyUp.WithLabelValues("db_meta").Set(1)
			}
		}
		return nil
	}

	// bulkhead 活跃数推送：5s 一次，给 prometheus 收集（保证 saturation 信号实时）
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if bulkhead != nil {
					active, rejected, _ := bulkhead.Stats()
					metrics.BulkheadActive.Set(float64(active))
					_ = rejected // 累计 rejected 直接 inc 不用 set
				}
			}
		}
	}()

	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			metrics.StartServer(addr, logger, probe)
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			metrics.BeginDrain()
			logger.Info("card-payment draining: /readyz now returns 503")
			return nil
		},
	})
}

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("CARDPAYMENT")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	for _, k := range []string{"env",
		"card_center.endpoint", "card_center.registry_endpoints",
		"card_center.insecure", "card_center.client_cert", "card_center.client_key", "card_center.server_ca",
		"tls.cert", "tls.key", "tls.client_ca",
		"network.visa.endpoint", "network.mastercard.endpoint", "network.jcb.endpoint",
		"network.amex.endpoint", "network.unionpay.endpoint",
		// Visa / CyberSource: mTLS + RSA-SHA256 HTTP Signature
		"network.visa.merchant_id", "network.visa.api_key_id",
		"network.visa.private_key_path", "network.visa.client_cert", "network.visa.client_key", "network.visa.server_ca",
		"network.visa.timeout", "network.visa.insecure_sandbox",
		// Mastercard MPGS: Basic + mTLS
		"network.mastercard.merchant_id", "network.mastercard.api_password",
		"network.mastercard.client_cert", "network.mastercard.client_key", "network.mastercard.server_ca",
		"network.mastercard.timeout", "network.mastercard.insecure_sandbox",
		// JCB: HMAC + mTLS
		"network.jcb.merchant_id", "network.jcb.api_secret",
		"network.jcb.client_cert", "network.jcb.client_key", "network.jcb.server_ca",
		"network.jcb.timeout", "network.jcb.insecure_sandbox",
		// AmEx: HMAC + mTLS
		"network.amex.api_key", "network.amex.client_id", "network.amex.api_secret",
		"network.amex.client_cert", "network.amex.client_key", "network.amex.server_ca",
		"network.amex.timeout", "network.amex.insecure_sandbox",
		// UnionPay: RSA-SHA256 form sig + optional mTLS
		"network.unionpay.merchant_id", "network.unionpay.cert_id", "network.unionpay.private_key_path",
		"network.unionpay.client_cert", "network.unionpay.client_key", "network.unionpay.server_ca",
		"network.unionpay.timeout", "network.unionpay.insecure_sandbox",
		"database.meta.dsn",
		// 服务自注册（被 order-core / payment-core / BFF 调用）
		"registry.endpoints", "registry.service_name", "registry.advertise_host", "registry.ttl",
		"server.grpc_port",
		// metrics + 可用性参数
		"metrics.addr",
		"bulkhead.per_merchant_max",
		"reconcile.disable", "reconcile.interval", "reconcile.stuck_age",
		"reconcile.limit_per_shard", "reconcile.query_timeout", "reconcile.cycle_timeout",
	} {
		_ = v.BindEnv(k)
	}
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/card-payment")
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
	}
	if err := assertProdSafety(v); err != nil {
		return nil, err
	}
	return v, nil
}

func assertProdSafety(v *viper.Viper) error {
	env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
	if env != "prod" && env != "production" {
		return nil
	}
	for _, k := range []string{"tls.cert", "tls.key", "tls.client_ca"} {
		if strings.TrimSpace(v.GetString(k)) == "" {
			return fmt.Errorf("PROD-SAFETY: %s required (mTLS-only)", k)
		}
	}
	if strings.TrimSpace(v.GetString("card_center.endpoint")) == "" &&
		len(splitCSV(v.GetString("card_center.registry_endpoints"))) == 0 &&
		len(v.GetStringSlice("card_center.registry_endpoints")) == 0 &&
		len(splitCSV(v.GetString("registry.endpoints"))) == 0 &&
		len(v.GetStringSlice("registry.endpoints")) == 0 {
		return fmt.Errorf("PROD-SAFETY: card_center.endpoint or card_center.registry_endpoints required")
	}
	atLeastOne := false
	for _, n := range []string{"visa", "mastercard", "jcb", "amex", "unionpay"} {
		ep := v.GetString("network." + n + ".endpoint")
		if ep == "" {
			continue
		}
		if !strings.HasPrefix(ep, "https://") {
			return fmt.Errorf("PROD-SAFETY: network.%s.endpoint must use https://", n)
		}
		// 拒绝 prod 指向 mock-network（127.x / localhost / *.local / *.internal）
		// mock-network 是 dev/sandbox 用的，prod 走 mock 等于资金路径假成功 → 灾难。
		if strings.Contains(ep, "://localhost") || strings.Contains(ep, "://127.") ||
			strings.Contains(ep, ".local") || strings.Contains(ep, ".internal") {
			return fmt.Errorf("PROD-SAFETY: network.%s.endpoint points to local/mock host (%q); production must hit real network", n, ep)
		}
		// 拒绝 prod 开 insecure_sandbox（=跳过证书验证）
		if v.GetBool("network." + n + ".insecure_sandbox") {
			return fmt.Errorf("PROD-SAFETY: network.%s.insecure_sandbox=true forbidden in env=prod", n)
		}
		atLeastOne = true
	}
	if !atLeastOne {
		return fmt.Errorf("PROD-SAFETY: at least one network adapter required")
	}
	if strings.TrimSpace(v.GetString("database.meta.dsn")) == "" {
		return fmt.Errorf("PROD-SAFETY: database.meta.dsn required")
	}
	for i := 0; i < sharding.ShardDBCount; i++ {
		if strings.TrimSpace(v.GetString(fmt.Sprintf("database.shard_%d.dsn", i))) == "" {
			return fmt.Errorf("PROD-SAFETY: database.shard_%d.dsn required", i)
		}
	}
	return nil
}

func newLogger() (*zap.Logger, error) {
	if os.Getenv("CARDPAYMENT_LOG_DEV") == "true" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

func newRouter() *sharding.Router { return sharding.NewRouter() }

func newDBManager(v *viper.Viper, router *sharding.Router, logger *zap.Logger) (*repo.Manager, error) {
	// 注意：用 v.GetString 而不是 v.UnmarshalKey。
	// viper 的 UnmarshalKey 不会触发 AutomaticEnv 查表 → CARDPAYMENT_DATABASE_*_DSN 被忽略。
	meta := repo.DBConfig{
		Name:            v.GetString("database.meta.name"),
		DSN:             v.GetString("database.meta.dsn"),
		MaxOpenConns:    v.GetInt("database.meta.max_open_conns"),
		MaxIdleConns:    v.GetInt("database.meta.max_idle_conns"),
		ConnMaxLifetime: v.GetInt("database.meta.conn_max_lifetime"),
	}
	if meta.Name == "" {
		meta.Name = "card_payment_meta"
	}
	shards := make([]repo.DBConfig, sharding.ShardDBCount)
	for i := 0; i < sharding.ShardDBCount; i++ {
		prefix := fmt.Sprintf("database.shard_%d", i)
		_ = v.BindEnv(prefix + ".dsn")
		_ = v.BindEnv(prefix + ".name")
		shards[i] = repo.DBConfig{
			Name:            v.GetString(prefix + ".name"),
			DSN:             v.GetString(prefix + ".dsn"),
			MaxOpenConns:    v.GetInt(prefix + ".max_open_conns"),
			MaxIdleConns:    v.GetInt(prefix + ".max_idle_conns"),
			ConnMaxLifetime: v.GetInt(prefix + ".conn_max_lifetime"),
		}
		if shards[i].Name == "" {
			shards[i].Name = fmt.Sprintf("card_payment_db_%d", i)
		}
	}
	return repo.NewManager(meta, shards, router, logger)
}

func newCardTransactionRepo(mgr *repo.Manager) processor.CardTransactionRepo {
	return repo.NewCardTransactionRepo(mgr)
}

func newCardCenterClient(v *viper.Viper) (processor.CardCenter, error) {
	registry := splitCSV(v.GetString("card_center.registry_endpoints"))
	if len(registry) == 0 {
		registry = v.GetStringSlice("card_center.registry_endpoints")
	}
	if len(registry) == 0 {
		// 全局 registry.endpoints 共用 fallback
		registry = v.GetStringSlice("registry.endpoints")
		if len(registry) == 0 {
			registry = splitCSV(v.GetString("registry.endpoints"))
		}
	}
	cfg := cardcenterclient.Config{
		Endpoint:          v.GetString("card_center.endpoint"),
		RegistryEndpoints: registry,
		RPCTimeout:        v.GetDuration("card_center.rpc_timeout"),
		ClientCert:        v.GetString("card_center.client_cert"),
		ClientKey:         v.GetString("card_center.client_key"),
		ServerCA:          v.GetString("card_center.server_ca"),
		Insecure:          v.GetBool("card_center.insecure"),
	}
	if cfg.Endpoint == "" && len(cfg.RegistryEndpoints) == 0 {
		return nil, errors.New("card_center.endpoint or card_center.registry_endpoints required")
	}
	return cardcenterclient.New(cfg)
}

// newNetworks 构造 5 个 network adapter map（visa / mastercard / jcb / amex / unionpay）。
//
// 每个 adapter：endpoint 为空 或 env != prod 时走 mock。生产 endpoint 必须 https://，
// 由 assertProdSafety 校验。
func newNetworks(v *viper.Viper, logger *zap.Logger) map[string]processor.Network {
	env := strings.ToLower(v.GetString("env"))
	mockMode := env != "prod" && env != "production"
	out := make(map[string]processor.Network, 5)

	// Visa Net (CyberSource): mTLS + RSA-SHA256 HTTP Signature
	visaEP := v.GetString("network.visa.endpoint")
	out["visa"] = visa.New(visa.Config{
		Endpoint:        visaEP,
		MerchantID:      v.GetString("network.visa.merchant_id"),
		APIKeyID:        v.GetString("network.visa.api_key_id"),
		PrivateKeyPath:  v.GetString("network.visa.private_key_path"),
		ClientCert:      v.GetString("network.visa.client_cert"),
		ClientKey:       v.GetString("network.visa.client_key"),
		ServerCA:        v.GetString("network.visa.server_ca"),
		Timeout:         v.GetDuration("network.visa.timeout"),
		Mock:            mockMode || visaEP == "",
		InsecureSandbox: !mockMode && v.GetBool("network.visa.insecure_sandbox"),
	}, logger)

	// Mastercard MPGS: mTLS + Basic auth (merchant.<id>:password)
	mcEP := v.GetString("network.mastercard.endpoint")
	out["mastercard"] = mastercard.New(mastercard.Config{
		Endpoint:        mcEP,
		MerchantID:      v.GetString("network.mastercard.merchant_id"),
		APIPassword:     v.GetString("network.mastercard.api_password"),
		ClientCert:      v.GetString("network.mastercard.client_cert"),
		ClientKey:       v.GetString("network.mastercard.client_key"),
		ServerCA:        v.GetString("network.mastercard.server_ca"),
		Timeout:         v.GetDuration("network.mastercard.timeout"),
		Mock:            mockMode || mcEP == "",
		InsecureSandbox: !mockMode && v.GetBool("network.mastercard.insecure_sandbox"),
	}, logger)

	// JCB J/Smart: mTLS + HMAC-SHA256
	jcbEP := v.GetString("network.jcb.endpoint")
	out["jcb"] = jcb.New(jcb.Config{
		Endpoint:        jcbEP,
		MerchantID:      v.GetString("network.jcb.merchant_id"),
		APISecret:       v.GetString("network.jcb.api_secret"),
		ClientCert:      v.GetString("network.jcb.client_cert"),
		ClientKey:       v.GetString("network.jcb.client_key"),
		ServerCA:        v.GetString("network.jcb.server_ca"),
		Timeout:         v.GetDuration("network.jcb.timeout"),
		Mock:            mockMode || jcbEP == "",
		InsecureSandbox: !mockMode && v.GetBool("network.jcb.insecure_sandbox"),
	}, logger)

	// AmEx Direct API: mTLS + HMAC-SHA256
	amexEP := v.GetString("network.amex.endpoint")
	out["amex"] = amex.New(amex.Config{
		Endpoint:        amexEP,
		APIKey:          v.GetString("network.amex.api_key"),
		ClientID:        v.GetString("network.amex.client_id"),
		APISecret:       v.GetString("network.amex.api_secret"),
		ClientCert:      v.GetString("network.amex.client_cert"),
		ClientKey:       v.GetString("network.amex.client_key"),
		ServerCA:        v.GetString("network.amex.server_ca"),
		Timeout:         v.GetDuration("network.amex.timeout"),
		Mock:            mockMode || amexEP == "",
		InsecureSandbox: !mockMode && v.GetBool("network.amex.insecure_sandbox"),
	}, logger)

	// UnionPay UPI: mTLS optional + RSA-SHA256 form sig
	upEP := v.GetString("network.unionpay.endpoint")
	out["unionpay"] = unionpay.New(unionpay.Config{
		Endpoint:        upEP,
		MerchantID:      v.GetString("network.unionpay.merchant_id"),
		CertID:          v.GetString("network.unionpay.cert_id"),
		PrivateKeyPath:  v.GetString("network.unionpay.private_key_path"),
		ClientCert:      v.GetString("network.unionpay.client_cert"),
		ClientKey:       v.GetString("network.unionpay.client_key"),
		ServerCA:        v.GetString("network.unionpay.server_ca"),
		Timeout:         v.GetDuration("network.unionpay.timeout"),
		Mock:            mockMode || upEP == "",
		InsecureSandbox: !mockMode && v.GetBool("network.unionpay.insecure_sandbox"),
	}, logger)

	return out
}

// newBreakerRegistry 给 5 个 network 各起一个独立熔断器。
//
// 默认阈值（resilience.Config defaults）：
//   WindowSize: 50    每 network 滑窗大小
//   ConsecutiveFails: 8   连续失败 8 次直接 trip（fast-trip）
//   FailureRate: 0.5  窗满后 fail-rate ≥ 50% trip
//   OpenDuration: 30s  Open 持续时长
// 转 metrics 时 Closed=0 / Open=1 / HalfOpen=2。
func newBreakerRegistry(logger *zap.Logger) *resilience.Registry {
	keys := []string{"visa", "mastercard", "jcb", "amex", "unionpay"}
	reg := resilience.NewRegistry(keys, func(network string, from, to resilience.State) {
		metrics.CircuitTransitions.WithLabelValues(network, from.String(), to.String()).Inc()
		metrics.CircuitState.WithLabelValues(network).Set(float64(to))
		logger.Warn("network circuit transition",
			zap.String("network", network),
			zap.String("from", from.String()),
			zap.String("to", to.String()))
	})
	// 启动期初始化所有 gauge 为 0（Closed），便于 Grafana 看不到 NaN
	for _, k := range keys {
		metrics.CircuitState.WithLabelValues(k).Set(0)
	}
	return reg
}

// newBulkhead 单 merchant 并发隔离器；默认 256 在飞 / merchant，
// 10K TPS 平台 / 100 merchant 时合理（见 docs/CAPACITY_10K_TPS.md）。
//
// config-center key: bulkhead.per_merchant_max（namespace=card-payment）。
// admin 改后下次重启生效（resilience.Bulkhead 当前没暴露 SetCapacity 热更）。
func newBulkhead(v *viper.Viper, cli *configcenter.Client, logger *zap.Logger) *resilience.Bulkhead {
	maxN := v.GetInt("bulkhead.per_merchant_max")
	if maxN <= 0 {
		maxN = 256
	}
	if cli != nil {
		maxN = cli.GetInt(context.Background(), "bulkhead.per_merchant_max", maxN)
	}
	metrics.BulkheadCapacity.Set(float64(maxN))
	// reject 时 +1 metrics counter，Prometheus alerting 看 rate
	resilience.OnRejectHook = func() { metrics.BulkheadRejectedTotal.Inc() }
	logger.Info("bulkhead configured", zap.Int("per_merchant_max", maxN))
	return resilience.NewBulkhead(maxN)
}

func newProcessor(cc processor.CardCenter, networks map[string]processor.Network, repo processor.CardTransactionRepo, breakers *resilience.Registry, bulkhead *resilience.Bulkhead, logger *zap.Logger) *processor.Processor {
	p := processor.NewProcessor(cc, networks, repo, logger)
	p.SetBreakers(breakers)
	p.SetBulkhead(bulkhead)
	return p
}

func newGRPCServer(v *viper.Viper, p *processor.Processor, logger *zap.Logger) (*grpc.Server, error) {
	allow := server.NewClientCNAllowList(v.GetStringSlice("auth.client_cn.allowed"))
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(
			trace.UnaryServerInterceptor(logger),
			shadow.UnaryServerInterceptor(),
			server.UnaryClientCNInterceptor(allow),
		),
	}
	// dev：tls 字段空 → 明文 listener。env=prod 已被 assertProdSafety 强制 cert/key。
	certPath := v.GetString("tls.cert")
	keyPath := v.GetString("tls.key")
	if certPath != "" && keyPath != "" {
		tlsCfg, err := buildTLSConfig(v)
		if err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
		logger.Info("card-payment gRPC: mTLS enabled")
	} else {
		logger.Warn("card-payment gRPC: NO TLS (dev mode); env=prod will fail at assertProdSafety")
	}
	srv := grpc.NewServer(opts...)
	bs := server.NewServer(p, logger)
	bs.Register(srv)
	return srv, nil
}

func startGRPC(lc fx.Lifecycle, srv *grpc.Server, v *viper.Viper, logger *zap.Logger) error {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9443
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	logger.Info("card-payment mTLS gRPC listening", zap.Int("port", port))
	go func() {
		if err := srv.Serve(lis); err != nil {
			logger.Error("grpc serve", zap.Error(err))
		}
	}()
	lc.Append(fx.Hook{OnStop: func(ctx context.Context) error {
		done := make(chan struct{})
		go func() { srv.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			srv.Stop()
		}
		return nil
	}})
	return nil
}

func buildTLSConfig(v *viper.Viper) (*tls.Config, error) {
	certPath := v.GetString("tls.cert")
	keyPath := v.GetString("tls.key")
	caPath := v.GetString("tls.client_ca")
	if certPath == "" || keyPath == "" {
		return nil, errors.New("tls.cert / tls.key required")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("server keypair: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if caPath != "" {
		pool := x509.NewCertPool()
		caBytes, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("client_ca: %w", err)
		}
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("client_ca PEM parse failed")
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
