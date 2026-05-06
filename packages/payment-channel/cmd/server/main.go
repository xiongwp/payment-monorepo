// Command server 启动 payment-channel gRPC + 回调 HTTP 服务。
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
	"github.com/xiongwp/payment-util/configcenter"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/xiongwp/payment-channel/internal/adapter/bdo"
	"github.com/xiongwp/payment-channel/internal/adapter/billease"
	"github.com/xiongwp/payment-channel/internal/adapter/bpi"
	"github.com/xiongwp/payment-channel/internal/adapter/coinsph"
	"github.com/xiongwp/payment-channel/internal/adapter/dragonpay"
	"github.com/xiongwp/payment-channel/internal/adapter/gcash"
	"github.com/xiongwp/payment-channel/internal/adapter/grabpay"
	"github.com/xiongwp/payment-channel/internal/adapter/instapay"
	"github.com/xiongwp/payment-channel/internal/adapter/landbank"
	"github.com/xiongwp/payment-channel/internal/adapter/maya"
	"github.com/xiongwp/payment-channel/internal/adapter/metrobank"
	"github.com/xiongwp/payment-channel/internal/adapter/paymongo"
	"github.com/xiongwp/payment-channel/internal/adapter/pesonet"
	"github.com/xiongwp/payment-channel/internal/adapter/shopeepay"
	"github.com/xiongwp/payment-channel/internal/adapter/xendit"
	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/idgen"
	"github.com/xiongwp/payment-channel/internal/metrics"
	"github.com/xiongwp/payment-channel/internal/repo"
	"github.com/xiongwp/payment-channel/internal/server"
	"github.com/xiongwp/payment-channel/internal/service"
	"github.com/xiongwp/payment-channel/internal/sharding"
)

func main() {
	metrics.Register()
	otelShutdown, otelErr := trace.InitOTel(context.Background(), "payment-channel", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if otelErr != nil {
		fmt.Fprintln(os.Stderr, "otel init:", otelErr)
	}
	defer func() {
		if otelShutdown != nil {
			_ = otelShutdown(context.Background())
		}
	}()
	app := fx.New(
		fx.Provide(
			loadConfig,
			newLogger,
			// config-center: adapter timeouts / rate_limit / retry policy
			// 等动态配置走这里。namespace="payment-channel"。
			configcenter.FxProvider("payment-channel"),
			newRouter,
			newDBManager,
			newIDGen,
			newIDIssuer,
			newRegistry,
			// repos
			repoAcquirerTx,
			repoWebhookRaw,
			// services
			svcAcquirer,
			newForwarder,
			svcWebhook,
			newRetryWorker,
			newPendingQueryWorker,
			// server
			newServer,
			newWebhookHTTP,
		),
		fx.Invoke(startGRPC, startWebhookHTTP, startRetryWorker, startPendingQueryWorker, startMetricsHTTP, startServiceRegistrar, applyShadowTables),
	)
	app.Run()
}

// ─── infra ─────────────────────────────────────────────────────────────

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("PAYCHAN")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// AutomaticEnv 仅自动绑定已在 yaml 出现的 key；BindEnv 兜底保证
	// PAYCHAN_REGISTRY_ENDPOINTS 能被 GetStringSlice("registry.endpoints") 读到。
	_ = v.BindEnv("registry.endpoints")
	_ = v.BindEnv("env")
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/payment-channel")
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

// assertProdSafety 在 env=prod 下做几条 fail-fast 校验：
//
//  1. auth.allow_unauthenticated 必须 false（dev / staging 才允许 true）
//  2. 任何 channel.*.base_url 不允许指向 mockserver（生产必须接真渠道）
//  3. mockserver 自身的 image 不允许在 prod 启动（在 deploy override 一层禁掉）
//
// 任一不满足直接返回错误，let viper 的调用方上抛 main fatal exit。
func assertProdSafety(v *viper.Viper) error {
	env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
	if env != "prod" && env != "production" {
		return nil // 非生产环境跳过这套校验
	}
	if v.GetBool("auth.allow_unauthenticated") {
		return fmt.Errorf("PROD-SAFETY: auth.allow_unauthenticated=true is forbidden in env=prod (set tokens or mTLS instead)")
	}
	channels := v.GetStringMap("channel")
	for name, raw := range channels {
		cfg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		baseURL, _ := cfg["base_url"].(string)
		if baseURL == "" {
			continue
		}
		bl := strings.ToLower(baseURL)
		if strings.Contains(bl, "mockserver") || strings.Contains(bl, "127.0.0.1") || strings.Contains(bl, "localhost") {
			return fmt.Errorf("PROD-SAFETY: channel.%s.base_url=%q points to mock/local in env=prod (must use real channel endpoint)", name, baseURL)
		}
	}
	// rate limit：prod 必须显式配
	if v.GetFloat64("rate_limit.rps") <= 0 {
		return fmt.Errorf("PROD-SAFETY: rate_limit.rps bootstrap must be > 0 in env=prod (config-center 不可达兜底；recommend 2000)")
	}
	return configcenter.AssertProdMandatory(v)
}

func newLogger() (*zap.Logger, error) {
	if os.Getenv("PAYCHAN_LOG_DEV") == "true" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

func newRouter(v *viper.Viper) *sharding.Router {
	return sharding.NewRouterWithConfig(
		v.GetInt("sharding.db_count"),
		v.GetInt("sharding.table_per_db"),
	)
}

func newDBManager(v *viper.Viper, logger *zap.Logger) (*repo.Manager, error) {
	repo.SetSQLLogger(logger.Named("sql"))
	var shards []repo.DBConfig
	if err := v.UnmarshalKey("database.shards", &shards); err != nil {
		return nil, err
	}
	if len(shards) == 0 {
		return nil, fmt.Errorf("database.shards is required")
	}
	var meta repo.DBConfig
	_ = v.UnmarshalKey("database.meta", &meta)

	var mgr *repo.Manager
	var err error
	if meta.DSN != "" {
		mgr, err = repo.NewManagerWithMeta(meta, shards)
	} else {
		logger.Warn("database.meta not set; shards[0] used for meta")
		mgr, err = repo.NewManager(shards)
	}
	if err != nil {
		return nil, diagnoseDBError(err, shards, meta)
	}
	logger.Info("database ready", zap.Int("shards", mgr.ShardCount()))
	return mgr, nil
}

// diagnoseDBError wraps the raw mysql/gorm error with actionable context
// when the most common cause hits: the configured DB hostname isn't
// resolvable. Happens constantly when dev runs `docker-compose up
// payment-channel` without bringing up the shard containers.
//
// The wrapping is pure string work — no network introspection — so it can
// run safely inside the fx DI chain without adding startup latency.
func diagnoseDBError(raw error, shards []repo.DBConfig, meta repo.DBConfig) error {
	msg := raw.Error()
	dnsFail := strings.Contains(msg, "no such host") || strings.Contains(msg, "lookup")
	if !dnsFail {
		return raw
	}
	// Extract the first hostname for a useful diagnostic.
	host := firstHostFromDSN(shards[0].DSN)
	metaHost := firstHostFromDSN(meta.DSN)
	hint := fmt.Sprintf(
		"\n\nDB host %q (and meta %q) did not resolve in Docker DNS.\n"+
			"This means `shared-db` is not attached to the shared-db-net docker network.\n\n"+
			"Fix (run once on the host):\n"+
			"  scripts/init-shared-db.sh                # create network + attach shared-db + load schemas\n"+
			"or manually:\n"+
			"  docker network create shared-db-net\n"+
			"  docker network connect shared-db-net shared-db\n"+
			"  docker network inspect shared-db-net --format '{{range .Containers}}{{.Name}} {{end}}'\n"+
			"  # → should include \"shared-db\"\n",
		host, metaHost)
	return fmt.Errorf("%w%s", raw, hint)
}

// firstHostFromDSN parses the MySQL DSN `user:pass@tcp(host:port)/db?...`
// and returns the host portion. Returns "" on malformed input rather than
// erroring — this is purely for diagnostic strings.
func firstHostFromDSN(dsn string) string {
	i := strings.Index(dsn, "@tcp(")
	if i < 0 {
		return ""
	}
	rest := dsn[i+5:]
	j := strings.IndexByte(rest, ':')
	if j < 0 {
		if k := strings.IndexByte(rest, ')'); k >= 0 {
			return rest[:k]
		}
		return rest
	}
	return rest[:j]
}

func newIDGen(mgr *repo.Manager, logger *zap.Logger) (idgen.IDGenerator, error) {
	return idgen.New(mgr.GetMeta(), logger)
}

func newIDIssuer(gen idgen.IDGenerator, r *sharding.Router) repo.IDIssuer {
	return repo.NewIDIssuer(gen, r)
}

// genDevRSAKeyPEM 生成一把内存 RSA 私钥，PKCS#1 PEM 格式。仅用于 sandbox/dev
// 自动 bootstrap —— 每次启动都是新 key，不能用在生产。
func genDevRSAKeyPEM(bits int) (string, error) {
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return "", err
	}
	der := x509.MarshalPKCS1PrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})), nil
}

// ─── adapter registry ──────────────────────────────────────────────────

func newRegistry(v *viper.Viper, logger *zap.Logger) channel.Registry {
	reg := channel.NewRegistry()

	// 启动期根据配置开启哪几个 adapter。生产环境建议把不用的保持默认 disabled。
	// gcash: 非 prod 模式自动默认 base_url 指向本地 mockserver，避免 fresh clone
	// 跑不通。config 里显式设置的值会覆盖这个默认。
	gcashBase := v.GetString("channel.gcash.base_url")
	gcashEnv := v.GetString("channel.gcash.env")
	if gcashBase == "" && gcashEnv != "prod" {
		gcashBase = "http://127.0.0.1:9400"
		logger.Info("gcash base_url defaulting to local mockserver (set channel.gcash.env=prod to disable)",
			zap.String("base_url", gcashBase))
	}
	// sandbox/dev：没配 merchant_priv 时生成一把内存 RSA 给 mockserver 签名
	// （mockserver 不校验签名，只是不让 sign() 方法报错）。prod 环境永远用配置里的。
	gcashPriv := v.GetString("channel.gcash.merchant_priv")
	if gcashPriv == "" && gcashEnv != "prod" {
		if pem, err := genDevRSAKeyPEM(2048); err == nil {
			gcashPriv = pem
			logger.Info("gcash merchant_priv auto-generated (dev only; set channel.gcash.merchant_priv in prod)")
		} else {
			logger.Error("gcash auto-gen dev key failed", zap.Error(err))
		}
	}
	if g, err := gcash.New(gcash.Config{
		Env:          gcashEnv,
		PartnerID:    v.GetString("channel.gcash.partner_id"),
		MerchantPriv: gcashPriv,
		GCashPubKey:  v.GetString("channel.gcash.gcash_pub_key"),
		NotifyURL:    v.GetString("channel.gcash.notify_url"),
		BaseURL:      gcashBase,
	}); err == nil {
		reg.Register(g)
	} else {
		// 关键：即使带着无效 PEM 也要有 gcash 条目，否则请求会被 AcquirerService
		// 以 "not registered" 拒掉。降级为空配置重试（清掉 PEM 字段），保证注册成功。
		logger.Error("gcash adapter init failed — falling back to empty-credential registration",
			zap.Error(err),
			zap.String("hint", "fix channel.gcash.merchant_priv / gcash_pub_key PEM format"))
		if g2, err2 := gcash.New(gcash.Config{
			Env:       v.GetString("channel.gcash.env"),
			PartnerID: v.GetString("channel.gcash.partner_id"),
			NotifyURL: v.GetString("channel.gcash.notify_url"),
			BaseURL:   gcashBase,
		}); err2 == nil {
			reg.Register(g2)
			logger.Warn("gcash registered with empty credentials — signing calls will fail until PEM fixed")
		} else {
			logger.Error("gcash still failed to register even with empty creds", zap.Error(err2))
		}
	}

	reg.Register(maya.New(maya.Config{
		Env:          v.GetString("channel.maya.env"),
		PublicKey:    v.GetString("channel.maya.public_key"),
		SecretKey:    v.GetString("channel.maya.secret_key"),
		RedirectOK:   v.GetString("channel.maya.redirect_ok"),
		RedirectFail: v.GetString("channel.maya.redirect_fail"),
		RedirectCxl:  v.GetString("channel.maya.redirect_cancel"),
		BaseURL:      v.GetString("channel.maya.base_url"),
	}))
	reg.Register(grabpay.New(grabpay.Config{
		Env:           v.GetString("channel.grabpay.env"),
		PartnerID:     v.GetString("channel.grabpay.partner_id"),
		PartnerSecret: v.GetString("channel.grabpay.partner_secret"),
		MerchantID:    v.GetString("channel.grabpay.merchant_id"),
		NotifyURL:     v.GetString("channel.grabpay.notify_url"),
		BaseURL:      v.GetString("channel.grabpay.base_url"),
	}))
	reg.Register(coinsph.New(coinsph.Config{
		Env:        v.GetString("channel.coinsph.env"),
		MerchantID: v.GetString("channel.coinsph.merchant_id"),
		SecretKey:  v.GetString("channel.coinsph.secret_key"),
		NotifyURL:  v.GetString("channel.coinsph.notify_url"),
		BaseURL:      v.GetString("channel.coinsph.base_url"),
	}))
	reg.Register(instapay.New(instapay.Config{
		Env:          v.GetString("channel.instapay.env"),
		ClientID:     v.GetString("channel.instapay.client_id"),
		ClientSecret: v.GetString("channel.instapay.client_secret"),
		PartnerID:    v.GetString("channel.instapay.partner_id"),
		SenderAcct:   v.GetString("channel.instapay.sender_account"),
		SenderName:   v.GetString("channel.instapay.sender_name"),
		BaseURL:      v.GetString("channel.instapay.base_url"),
	}))
	reg.Register(pesonet.New(pesonet.Config{
		Env:          v.GetString("channel.pesonet.env"),
		ClientID:     v.GetString("channel.pesonet.client_id"),
		ClientSecret: v.GetString("channel.pesonet.client_secret"),
		PartnerID:    v.GetString("channel.pesonet.partner_id"),
		SenderAcct:   v.GetString("channel.pesonet.sender_account"),
		SenderName:   v.GetString("channel.pesonet.sender_name"),
		BaseURL:      v.GetString("channel.pesonet.base_url"),
	}))

	// 主要银行（直连 Partner / Developer API）
	reg.Register(bdo.New(bdo.Config{
		Env:          v.GetString("channel.bdo.env"),
		ClientID:     v.GetString("channel.bdo.client_id"),
		ClientSecret: v.GetString("channel.bdo.client_secret"),
		PartnerID:    v.GetString("channel.bdo.partner_id"),
		PartnerSec:   v.GetString("channel.bdo.partner_secret"),
		BaseURL:      v.GetString("channel.bdo.base_url"),
	}))
	reg.Register(bpi.New(bpi.Config{
		Env:          v.GetString("channel.bpi.env"),
		ClientID:     v.GetString("channel.bpi.client_id"),
		ClientSecret: v.GetString("channel.bpi.client_secret"),
		PartnerID:    v.GetString("channel.bpi.partner_id"),
		SenderAcct:   v.GetString("channel.bpi.sender_account"),
		SenderName:   v.GetString("channel.bpi.sender_name"),
		BaseURL:      v.GetString("channel.bpi.base_url"),
	}))
	reg.Register(metrobank.New(metrobank.Config{
		Env:          v.GetString("channel.metrobank.env"),
		ClientID:     v.GetString("channel.metrobank.client_id"),
		ClientSecret: v.GetString("channel.metrobank.client_secret"),
		PartnerID:    v.GetString("channel.metrobank.partner_id"),
		PartnerSec:   v.GetString("channel.metrobank.partner_secret"),
		BaseURL:      v.GetString("channel.metrobank.base_url"),
	}))
	reg.Register(landbank.New(landbank.Config{
		Env:        v.GetString("channel.landbank.env"),
		MerchantID: v.GetString("channel.landbank.merchant_id"),
		SecretKey:  v.GetString("channel.landbank.secret_key"),
		ReturnURL:  v.GetString("channel.landbank.return_url"),
		BaseURL:      v.GetString("channel.landbank.base_url"),
	}))

	// 主要支付公司（PSP / 聚合）
	reg.Register(paymongo.New(paymongo.Config{
		Env:           v.GetString("channel.paymongo.env"),
		SecretKey:     v.GetString("channel.paymongo.secret_key"),
		WebhookSecret: v.GetString("channel.paymongo.webhook_secret"),
		BaseURL:      v.GetString("channel.paymongo.base_url"),
	}))
	reg.Register(xendit.New(xendit.Config{
		Env:               v.GetString("channel.xendit.env"),
		SecretKey:         v.GetString("channel.xendit.secret_key"),
		VerificationToken: v.GetString("channel.xendit.verification_token"),
		BaseURL:      v.GetString("channel.xendit.base_url"),
	}))
	reg.Register(dragonpay.New(dragonpay.Config{
		Env:         v.GetString("channel.dragonpay.env"),
		MerchantID:  v.GetString("channel.dragonpay.merchant_id"),
		MerchantKey: v.GetString("channel.dragonpay.merchant_key"),
		NotifyURL:   v.GetString("channel.dragonpay.notify_url"),
		BaseURL:      v.GetString("channel.dragonpay.base_url"),
	}))

	// 钱包 / BNPL
	reg.Register(shopeepay.New(shopeepay.Config{
		Env:           v.GetString("channel.shopeepay.env"),
		PartnerID:     v.GetString("channel.shopeepay.partner_id"),
		PartnerKey:    v.GetString("channel.shopeepay.partner_key"),
		MerchantExtID: v.GetString("channel.shopeepay.merchant_ext_id"),
		StoreExtID:    v.GetString("channel.shopeepay.store_ext_id"),
		NotifyURL:     v.GetString("channel.shopeepay.notify_url"),
		BaseURL:      v.GetString("channel.shopeepay.base_url"),
	}))
	reg.Register(billease.New(billease.Config{
		Env:          v.GetString("channel.billease.env"),
		ClientID:     v.GetString("channel.billease.client_id"),
		ClientSecret: v.GetString("channel.billease.client_secret"),
		CallbackURL:  v.GetString("channel.billease.callback_url"),
		SuccessURL:   v.GetString("channel.billease.success_url"),
		FailureURL:   v.GetString("channel.billease.failure_url"),
		CancelURL:    v.GetString("channel.billease.cancel_url"),
		BaseURL:      v.GetString("channel.billease.base_url"),
	}))

	logger.Info("adapter registry ready", zap.Strings("adapters", reg.Names()))
	return reg
}

// ─── repos / services ─────────────────────────────────────────────────

func repoAcquirerTx(mgr *repo.Manager, r *sharding.Router) repo.AcquirerTxRepository {
	return repo.NewAcquirerTxRepository(mgr, r)
}

func repoWebhookRaw(mgr *repo.Manager, r *sharding.Router) repo.WebhookRawRepository {
	return repo.NewWebhookRawRepository(mgr, r)
}

func svcAcquirer(reg channel.Registry, tx repo.AcquirerTxRepository, idg repo.IDIssuer, logger *zap.Logger) *service.AcquirerService {
	return service.NewAcquirerService(reg, tx, idg, logger)
}

func newForwarder(logger *zap.Logger) service.Forwarder {
	// 默认 noop。生产接入 order-core 时注入 http/grpc Forwarder 实现。
	return &service.NoopForwarder{Logger: logger}
}

func svcWebhook(reg channel.Registry, wh repo.WebhookRawRepository, f service.Forwarder, logger *zap.Logger) *service.WebhookService {
	return service.NewWebhookService(reg, wh, f, logger)
}

func newRetryWorker(svc *service.AcquirerService, tx repo.AcquirerTxRepository, v *viper.Viper, logger *zap.Logger) *service.CallRetryWorker {
	return service.NewCallRetryWorker(svc,
		tx,
		v.GetDuration("call_retry_worker.interval"),
		v.GetInt("call_retry_worker.limit"),
		logger)
}

func newPendingQueryWorker(reg channel.Registry, tx repo.AcquirerTxRepository, v *viper.Viper, logger *zap.Logger) *service.PendingQueryWorker {
	return service.NewPendingQueryWorker(reg, tx,
		v.GetDuration("pending_query_worker.interval"),
		v.GetDuration("pending_query_worker.older_than"),
		v.GetDuration("pending_query_worker.query_throttle"),
		v.GetInt("pending_query_worker.limit"),
		v.GetInt("pending_query_worker.max_attempts"),
		logger)
}

// ─── server ───────────────────────────────────────────────────────────

func newServer(svc *service.AcquirerService, v *viper.Viper, cli *configcenter.Client, logger *zap.Logger) *server.Server {
	tokens := map[string]string{}
	for _, t := range v.GetStringSlice("auth.tokens") {
		tokens[t] = "ok"
	}
	// rate_limit 走 config-center；不可达 → 用 yaml bootstrap 兜底。
	// admin 改 namespace=payment-channel 的 rate_limit.rps / rate_limit.burst 实时推送。
	rps := v.GetFloat64("rate_limit.rps")
	burst := v.GetInt("rate_limit.burst")
	if cli != nil {
		if v := cli.GetFloat64(context.Background(), "rate_limit.rps", rps); v > 0 {
			rps = v
		}
		if v := cli.GetInt(context.Background(), "rate_limit.burst", burst); v > 0 {
			burst = v
		}
	}
	return server.NewServer(server.Deps{
		AcquirerSvc:          svc,
		AuthTokens:           tokens,
		AllowUnauthenticated: v.GetBool("auth.allow_unauthenticated"),
		RateLimitRPS:         rps,
		RateBurst:            burst,
		Logger:               logger,
	})
}

func newWebhookHTTP(svc *service.WebhookService, logger *zap.Logger) *server.WebhookHTTPServer {
	return server.NewWebhookHTTPServer(svc, logger)
}

func startGRPC(lc fx.Lifecycle, s *server.Server, v *viper.Viper, logger *zap.Logger) {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9092
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				if err := s.ListenAndServe(ctx, port); err != nil {
					logger.Error("grpc exited", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

func startWebhookHTTP(lc fx.Lifecycle, s *server.WebhookHTTPServer, v *viper.Viper, logger *zap.Logger) {
	addr := v.GetString("server.webhook_http_addr")
	if addr == "" {
		addr = ":9192"
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				if err := s.ListenAndServe(ctx, addr); err != nil {
					logger.Info("webhook http stopped", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

func startRetryWorker(lc fx.Lifecycle, w *service.CallRetryWorker) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error { go w.Start(ctx); return nil },
		OnStop:  func(_ context.Context) error { cancel(); return nil },
	})
}

func startPendingQueryWorker(lc fx.Lifecycle, w *service.PendingQueryWorker) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error { go w.Start(ctx); return nil },
		OnStop:  func(_ context.Context) error { cancel(); return nil },
	})
}

// applyShadowTables 启动期自愈：跨 10 分库给业务表建 _shadow 副本，给
// paychan_meta 建 leaf_alloc_shadow。init-shared-db.sh 已经导入过则全 no-op；
// 升级 / 漏 init 的环境靠这里兜底。失败不阻断启动。
func applyShadowTables(lc fx.Lifecycle, mgr *repo.Manager, router *sharding.Router, logger *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := repo.ApplyMetaShadowTables(ctx, mgr, logger); err != nil {
				logger.Warn("apply meta shadow tables", zap.Error(err))
			}
			if err := repo.ApplyShadowTables(ctx, mgr, router.TablePerDB(), logger); err != nil {
				logger.Warn("apply shadow tables", zap.Error(err))
			}
			return nil
		},
	})
}

func startMetricsHTTP(lc fx.Lifecycle, v *viper.Viper, mgr *repo.Manager, logger *zap.Logger) {
	addr := v.GetString("metrics.addr")
	if addr == "" {
		addr = ":9093"
	}
	// readiness DB probe：随机挑 shard 0 ping。所有分片都不可达大概率是网络
	// /认证问题，所以 single-shard ping 的信号已足。
	dbPing := func(ctx context.Context) error {
		gdb, err := mgr.GetShard(0)
		if err != nil {
			return err
		}
		sqlDB, err := gdb.DB()
		if err != nil {
			return err
		}
		return sqlDB.PingContext(ctx)
	}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			metrics.StartServer(addr, logger, dbPing)
			return nil
		},
		// SIGTERM 到来 → /readyz 503 → k8s 摘流量 → 然后 gRPC GracefulStop
		OnStop: func(_ context.Context) error {
			metrics.BeginDrain()
			logger.Info("payment-channel draining: /readyz now returns 503")
			return nil
		},
	})
}
