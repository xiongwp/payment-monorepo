// Command server 启动 card-center gRPC over mTLS。
//
// 部署在隔离 DC（SAQ-D scope）：
//   - 仅 mTLS 入站
//   - 出站只到 KMS / Kafka audit / 自有 MySQL（card_center_db_*）
//   - 启动期校验 env=prod 时 TLS / KMS / Kafka 全部必填
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	kitexserver "github.com/cloudwego/kitex/server"
	"github.com/spf13/viper"
	"go.uber.org/fx"
	"go.uber.org/zap"
	usermerchantv1 "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1"

	cardcenterservice "github.com/xiongwp/card-center/kitex_gen/cardcenter/v1/cardcenterservice"

	"github.com/xiongwp/card-center/internal/audit"
	"github.com/xiongwp/card-center/internal/httpsauth"
	"github.com/xiongwp/card-center/internal/kmsclient"
	"github.com/xiongwp/card-center/internal/repo"
	"github.com/xiongwp/card-center/internal/server"
	"github.com/xiongwp/card-center/internal/service"
	"github.com/xiongwp/card-center/internal/sharding"
	"github.com/xiongwp/card-center/internal/vault"
	"github.com/xiongwp/payment-util/audit/kafkago"
	"github.com/xiongwp/payment-util/configcenter"
	"github.com/xiongwp/payment-util/obsbootstrap"
	"github.com/xiongwp/payment-util/serviceregistry"
	"github.com/xiongwp/payment-util/shadow"
	"github.com/xiongwp/payment-util/trace"
)

func main() {
	app := fx.New(
		fx.StartTimeout(30*time.Second),
		fx.StopTimeout(30*time.Second),
		fx.Provide(
			loadConfig,
			newLogger,
			// config-center: per-user tokenize 限流 / HTTPS CORS / session TTL / Luhn 严格 mode
			configcenter.FxProvider("card-center"),
			newRouter,
			newDBManager,
			newKMSClient,
			newVault,
			newStoredCardRepo,
			newPaymentTokenRepo,
			newAuditEmitter,
			newService,
			newGRPCServer,
			// HTTPS REST 入口（前端 SDK 直连用）+ 用户登录态校验
			newUserMerchantConn,
			newHTTPSVerifier,
			newRESTServer,
		),
		fx.Invoke(startGRPC, startHTTPS, startServiceRegistrar),
	)
	app.Run()
}

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("CARDCENTER")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	for _, k := range []string{"env", "kms.endpoint", "kms.registry_endpoints",
		"kms.insecure", "kms.bearer_token",
		"kms.rpc_timeout", "kms.client_cert", "kms.client_key", "kms.server_ca",
		"tls.cert", "tls.key", "tls.client_ca",
		"audit.kafka_brokers", "database.meta.dsn",
		// HTTPS 入口 + 用户登录态校验上游（mTLS gRPC 直连 user-merchant-core）
		"https.enabled", "https.port", "https.cert", "https.key",
		"https.dev_no_tls", "https.cors.allowed_origin",
		"auth.user_merchant.endpoint", "auth.user_merchant.insecure",
		"auth.user_merchant.registry_endpoints",
		"auth.user_merchant.client_cert",
		"auth.user_merchant.client_key",
		"auth.user_merchant.server_ca",
		// 服务自注册到 etcd（被 card-payment / api-gateway / BFF 调用）
		"registry.endpoints", "registry.service_name", "registry.advertise_host", "registry.ttl",
		"server.grpc_port",
	} {
		_ = v.BindEnv(k)
	}
	// 10 个 shard DSN 显式 BindEnv（viper.UnmarshalKey 不读 env 嵌套子键）
	for i := 0; i < 10; i++ {
		_ = v.BindEnv(fmt.Sprintf("database.shard_%d.dsn", i))
		_ = v.BindEnv(fmt.Sprintf("database.shard_%d.name", i))
		_ = v.BindEnv(fmt.Sprintf("database.shard_%d.max_open_conns", i))
		_ = v.BindEnv(fmt.Sprintf("database.shard_%d.max_idle_conns", i))
		_ = v.BindEnv(fmt.Sprintf("database.shard_%d.conn_max_lifetime", i))
	}
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/card-center")
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
			return fmt.Errorf("PROD-SAFETY: %s must be configured (mTLS-only)", k)
		}
	}
	if strings.TrimSpace(v.GetString("kms.endpoint")) == "" &&
		len(splitCSV(v.GetString("kms.registry_endpoints"))) == 0 &&
		len(v.GetStringSlice("kms.registry_endpoints")) == 0 {
		return fmt.Errorf("PROD-SAFETY: kms.endpoint or kms.registry_endpoints must be configured")
	}
	if v.GetBool("kms.bypass_hardened") {
		return fmt.Errorf("PROD-SAFETY: kms.bypass_hardened=true forbidden in env=prod (dev-only diagnostic flag)")
	}
	if len(v.GetStringSlice("audit.kafka_brokers")) == 0 {
		return fmt.Errorf("PROD-SAFETY: audit.kafka_brokers must be configured (audit cannot be lost)")
	}
	if strings.TrimSpace(v.GetString("database.meta.dsn")) == "" {
		return fmt.Errorf("PROD-SAFETY: database.meta.dsn required")
	}
	for i := 0; i < sharding.ShardDBCount; i++ {
		if strings.TrimSpace(v.GetString(fmt.Sprintf("database.shard_%d.dsn", i))) == "" {
			return fmt.Errorf("PROD-SAFETY: database.shard_%d.dsn required", i)
		}
	}
	// HTTPS 入口 + 登录态校验（mTLS gRPC 直连 user-merchant-core）
	if v.GetBool("https.enabled") {
		// 生产严禁明文 HTTP
		if v.GetBool("https.dev_no_tls") {
			return fmt.Errorf("PROD-SAFETY: https.dev_no_tls=true is forbidden in env=prod")
		}
		for _, k := range []string{
			"https.cert", "https.key",
			"auth.user_merchant.endpoint",
			"auth.user_merchant.client_cert",
			"auth.user_merchant.client_key",
			"auth.user_merchant.server_ca",
		} {
			if strings.TrimSpace(v.GetString(k)) == "" {
				return fmt.Errorf("PROD-SAFETY: https.enabled=true requires %s", k)
			}
		}
	}
	return configcenter.AssertProdMandatory(v)
}

func newLogger() (*zap.Logger, error) {
	if os.Getenv("CARDCENTER_LOG_DEV") == "true" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

func newRouter() *sharding.Router { return sharding.NewRouter() }

func newDBManager(v *viper.Viper, router *sharding.Router, logger *zap.Logger) (*repo.Manager, error) {
	// 注意：用 v.GetString 而不是 v.UnmarshalKey。
	// viper 的 UnmarshalKey 不会触发 AutomaticEnv 的查表，导致
	// CARDCENTER_DATABASE_META_DSN 等 env 变量被忽略，构造函数报 meta.dsn required。
	// 显式 GetString 能命中 BindEnv 注册的键。
	meta := repo.DBConfig{
		Name:            v.GetString("database.meta.name"),
		DSN:             v.GetString("database.meta.dsn"),
		MaxOpenConns:    v.GetInt("database.meta.max_open_conns"),
		MaxIdleConns:    v.GetInt("database.meta.max_idle_conns"),
		ConnMaxLifetime: v.GetInt("database.meta.conn_max_lifetime"),
	}
	if meta.Name == "" {
		meta.Name = "card_center_meta"
	}
	shards := make([]repo.DBConfig, sharding.ShardDBCount)
	for i := 0; i < sharding.ShardDBCount; i++ {
		prefix := fmt.Sprintf("database.shard_%d", i)
		// 每个 shard DSN 显式 BindEnv（loadConfig 没枚举 0..9，这里补）
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
			shards[i].Name = fmt.Sprintf("card_center_db_%d", i)
		}
	}
	return repo.NewManager(meta, shards, router, logger)
}

func newKMSClient(v *viper.Viper) (vault.KMS, error) {
	// kms.registry_endpoints 是优先项（走 etcd:///kms-manage 服务发现，绕开
	// docker DNS 那种 alias 错绑问题）；空时退回 kms.endpoint 静态 DNS。
	// 接受逗号分隔的字符串（CARDCENTER_KMS_REGISTRY_ENDPOINTS=etcd:2379 之类）
	// 或 yaml 列表，用 viper.GetStringSlice 兼容（字符串里有逗号会被 viper 自动拆）。
	registry := splitCSV(v.GetString("kms.registry_endpoints"))
	if len(registry) == 0 {
		// 兜底：yaml 写成 list 的情况
		registry = v.GetStringSlice("kms.registry_endpoints")
	}
	cfg := kmsclient.Config{
		Endpoint:          v.GetString("kms.endpoint"),
		RegistryEndpoints: registry,
		BearerToken:       v.GetString("kms.bearer_token"),
		RPCTimeout:        v.GetDuration("kms.rpc_timeout"),
		ClientCert:        v.GetString("kms.client_cert"),
		ClientKey:         v.GetString("kms.client_key"),
		ServerCA:          v.GetString("kms.server_ca"),
		Insecure:          v.GetBool("kms.insecure"),
	}
	if cfg.Endpoint == "" && len(cfg.RegistryEndpoints) == 0 {
		return nil, errors.New("kms.endpoint or kms.registry_endpoints required")
	}
	return kmsclient.New(cfg)
}

// splitCSV 把 "a,b,c" / "a, b , c" / "" 拆成 []string；空 token 自动丢。
// 给"环境变量是逗号分隔字符串、想当 list 用"的场景用。
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

func newVault(kms vault.KMS) *vault.Vault { return vault.NewVault(kms, time.Now) }

func newStoredCardRepo(mgr *repo.Manager) repo.StoredCardRepo {
	return repo.NewStoredCardRepo(mgr)
}

func newPaymentTokenRepo(mgr *repo.Manager) repo.PaymentTokenRepo {
	return repo.NewPaymentTokenRepo(mgr)
}

// newAuditEmitter 构造 audit emitter。
//
// 双写：Kafka producer (PCI Req 10 卸载存档) + audit_log DB (强一致 7 年留存)。
// brokers 配上时 wire 真的 kafka-go 生产者；空时按环境决定行为：
//   - dev: 退化 DB-only + warn
//   - prod: fail-fast（PCI Req 10 不容许只 DB）
func newAuditEmitter(lc fx.Lifecycle, mgr *repo.Manager, v *viper.Viper, logger *zap.Logger) (service.AuditEmitter, error) {
	topic := v.GetString("audit.topic")
	if topic == "" {
		topic = "card-center.audit"
	}
	brokers := splitCSV(v.GetString("audit.kafka_brokers"))
	if len(brokers) == 0 {
		brokers = v.GetStringSlice("audit.kafka_brokers")
	}
	env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
	isProd := env == "prod" || env == "production"

	var producer audit.KafkaProducer
	if len(brokers) > 0 {
		p, err := kafkago.New(kafkago.Config{
			Brokers:      brokers,
			Acks:         v.GetString("audit.kafka_acks"),
			WriteTimeout: v.GetDuration("audit.kafka_write_timeout"),
			BatchTimeout: v.GetDuration("audit.kafka_batch_timeout"),
			Compression:  v.GetString("audit.kafka_compression"),
		}, logger)
		if err != nil {
			if isProd {
				return nil, fmt.Errorf("PROD-SAFETY: audit kafka producer init: %w", err)
			}
			logger.Warn("audit kafka producer init failed, dev fallback DB-only", zap.Error(err))
		} else {
			producer = p
			lc.Append(fx.Hook{
				OnStop: func(_ context.Context) error { return p.Close() },
			})
			logger.Info("audit kafka producer wired",
				zap.Strings("brokers", brokers),
				zap.String("topic", topic))
		}
	}

	if isProd && producer == nil {
		return nil, fmt.Errorf("PROD-SAFETY: audit.kafka_brokers required in env=prod (PCI DSS Req 10 forbids DB-only audit)")
	}
	if producer == nil {
		logger.Warn("audit emitter running DB-only (no Kafka brokers); dev mode acceptable")
	}
	// audit_log 已分 10 库 × 100 表，传整个 mgr 让 emitter 按 user_id 路由
	// + 维护 per-shard 链式签名（详见 internal/audit/audit.go）。
	return audit.New(producer, topic, mgr, logger), nil
}

func newService(v *vault.Vault, sr repo.StoredCardRepo, pr repo.PaymentTokenRepo, ae service.AuditEmitter, logger *zap.Logger) *service.Service {
	return service.NewService(v, sr, pr, ae, logger)
}

// newGRPCServer 构造 grpc.Server。
//
// 生产路径：mTLS + clientCN 白名单 interceptor。env=prod 已经在 assertProdSafety
// 强制要求 tls.cert/key/client_ca。
//
// dev 路径：tls.cert/key 都没配时，**自动降级到明文 listener** 让 card-center 能起来。
// 适合本机 / docker-compose 调试。assertProdSafety 在 env=prod 下会拦截这种降级。
func newGRPCServer(v *viper.Viper, svc *service.Service, logger *zap.Logger) (kitexserver.Server, *server.Server, error) {
	allowMap := v.GetStringMapStringSlice("auth.client_cn")
	allow := server.NewClientCNAllowList(allowMap)

	opts := []grpc.ServerOption{
		// TODO: kitexutil MW 三件套 (Trace / Shadow / ClientCN) — 等 kitexutil port 完成后接.
		// 当前 gRPC interceptor 等价 MW 都标 TODO 待实现:
		//   - trace.UnaryServerInterceptor(logger) → kitexutil.TraceMW(logger)
		//   - shadow.UnaryServerInterceptor() → kitexutil.ShadowMW()
		//   - server.UnaryClientCNInterceptor(allow) → kitexutil.MTLSClientCNMW(allow)  [mTLS 已不需要, 可删]
	}
	// mTLS 已不需要 (内部 service mesh 明文跑), tls.cert/key 配置废弃.
	_ = opts

	addr, _ := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", v.GetInt("server.grpc_port")))
	bs := server.NewServer(svc, logger)
	srv := cardcenterservice.NewServer(bs,
		kitexserver.WithServiceAddr(addr),
	)
	return srv, bs, nil
}

func startGRPC(lc fx.Lifecycle, srv kitexserver.Server, _ *server.Server, v *viper.Viper, logger *zap.Logger) error {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9443
	}
	logger.Info("card-center Kitex listening", zap.Int("port", port))
	go func() {
		if err := srv.Run(); err != nil {
			logger.Error("kitex serve", zap.Error(err))
		}
	}()
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			done := make(chan struct{})
			go func() {
				_ = srv.Stop()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				srv.Stop()
			}
			return nil
		},
	})
	return nil
}

// ─── HTTPS REST + 登录态校验 (mTLS gRPC 直连 user-merchant-core) ────────────

// newUserMerchantConn 建 mTLS gRPC 连接到 user-merchant-core。
//
// 当 https.enabled=false 时返 (nil, nil) — fx 接受 nil provide，下游 newRESTServer
// 也会因为 verifier 为 nil 而跳过 HTTPS 启动。这样开发模式不需要任何 mTLS 配置就能跑。
func newUserMerchantConn(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger) (*grpc.ClientConn, error) {
	if !v.GetBool("https.enabled") {
		logger.Info("https.enabled=false; skipping user-merchant-core dial")
		return nil, nil
	}
	endpoint := v.GetString("auth.user_merchant.endpoint")
	registry := splitCSV(v.GetString("auth.user_merchant.registry_endpoints"))
	if len(registry) == 0 {
		registry = v.GetStringSlice("auth.user_merchant.registry_endpoints")
	}
	// 跟全局 registry.endpoints 共用同一套 etcd cluster：上面单独配置是为了
	// 个别服务（card-center 隔离 DC 内）允许跨 DC 走专用 registry，配置兼容。
	if len(registry) == 0 {
		registry = v.GetStringSlice("registry.endpoints")
		if len(registry) == 0 {
			registry = splitCSV(v.GetString("registry.endpoints"))
		}
	}
	if endpoint == "" && len(registry) == 0 {
		return nil, errors.New("https.enabled=true but auth.user_merchant.endpoint / registry_endpoints both empty")
	}
	// dev：auth.user_merchant.insecure=true → 明文 gRPC，跳过 mTLS。
	insecureMode := v.GetBool("auth.user_merchant.insecure")
	var dialOpts []grpc.DialOption
	if insecureMode {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecuregrpc.NewCredentials()))
		logger.Info("card-center → user-merchant-core: INSECURE mode (dev)")
	} else {
		tlsCfg, err := buildClientMTLS(
			v.GetString("auth.user_merchant.client_cert"),
			v.GetString("auth.user_merchant.client_key"),
			v.GetString("auth.user_merchant.server_ca"),
		)
		if err != nil {
			return nil, fmt.Errorf("user-merchant client tls: %w", err)
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	}
	// DialWithFallback：registry 非空时走 etcd:///user-merchant-core（拿真实存活
	// 副本，绕开 docker DNS alias 错绑），空时退回静态 endpoint DNS 直连。
	// 两条路径都自动获得 round_robin LB + 10s/3s keepalive + 配套 server 端
	// HardenedServerOptions 的 EnforcementPolicy。
	conn, err := serviceregistry.DialWithFallback(registry, "user-merchant-core", endpoint, dialOpts...)
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { return conn.Close() }})
	logger.Info("card-center → user-merchant-core dialed (for HTTPS auth)",
		zap.String("endpoint", endpoint))
	return conn, nil
}

// newHTTPSVerifier 装配 Verifier。当前唯一实现：UserMerchantVerifier。
func newHTTPSVerifier(v *viper.Viper, conn *grpc.ClientConn, logger *zap.Logger) httpsauth.Verifier {
	if !v.GetBool("https.enabled") || conn == nil {
		return nil
	}
	uc := kitexutil.MustKitexClient(userservice.NewClient("user-merchant-core"))
	return httpsauth.NewUserMerchantVerifier(uc, logger)
}

// newRESTServer 装配 HTTPS REST handler。
//
// rate limit 默认 5 tokenize / 分钟 per user_id。yaml: ratelimit.tokenize.burst /
// ratelimit.tokenize.refill_seconds 可调。
func newRESTServer(v *viper.Viper, cli *configcenter.Client, svc *service.Service, vfy httpsauth.Verifier, logger *zap.Logger) *httpsauth.RESTServer {
	if !v.GetBool("https.enabled") || vfy == nil {
		return nil
	}
	// tokenize 限流 100% 走 config-center；不可达 → hardcoded safe default
	// (burst=10, refill=2s ≈ 30 tokens/分钟稳定速率)。
	burst := 10
	refill := 2 * time.Second
	if cli != nil {
		ctx := context.Background()
		burst = cli.GetInt(ctx, "ratelimit.tokenize.burst", burst)
		refill = cli.GetDuration(ctx, "ratelimit.tokenize.refill", refill)
	}
	rl := httpsauth.NewMemoryBucket(burst, refill)
	rl.Cleanup(10*time.Minute, time.Hour)
	logger.Info("card-center HTTPS tokenize rate limit",
		zap.Int("burst", burst), zap.Duration("refill_every", refill))
	return httpsauth.NewRESTServer(svc, vfy, logger).WithRateLimiter(rl)
}

// startHTTPS 起 HTTPS REST listener（独立端口），监听 https.port，绑 cert/key。
//
// 部署纪律：
//   - 这个端口对前端可见（PCI scope SAQ-D 的入口之一）
//   - cert 用面向 SDK 的 cert（公网证书 / 内部 CA 颁的客户端可信 cert）
//   - 上 CDN / WAF；Authorization: Bearer 鉴权由 middleware 完成
func startHTTPS(lc fx.Lifecycle, v *viper.Viper, rest *httpsauth.RESTServer, logger *zap.Logger) {
	if rest == nil {
		logger.Info("card-center HTTPS REST disabled (https.enabled=false or verifier missing)")
		return
	}
	port := v.GetInt("https.port")
	if port == 0 {
		port = 8443
	}
	certPath := v.GetString("https.cert")
	keyPath := v.GetString("https.key")

	// dev_no_tls：true 时跑明文 HTTP（不需要证书，浏览器无 self-signed warning）。
	// env=prod 由 assertProdSafety 拦截 dev_no_tls=true，绝不允许生产明文。
	devNoTLS := v.GetBool("https.dev_no_tls")
	mode := "TLS"
	if devNoTLS {
		mode = "DEV-PLAINTEXT"
	}

	// CORS：dev 时浏览器从 api-gateway origin (http://localhost:18080) 跨域 fetch
	// card-center (http://localhost:8443)，必须放开 Origin + Credentials。
	allowedOrigin := v.GetString("https.cors.allowed_origin")
	if allowedOrigin == "" {
		allowedOrigin = "http://localhost:18080" // dev 默认
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           corsMiddleware(rest.Handler(), allowedOrigin, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			logger.Info("card-center HTTPS REST listening",
				zap.Int("port", port), zap.String("mode", mode),
				zap.String("cors_allowed_origin", allowedOrigin))
			go func() {
				var err error
				if devNoTLS {
					err = srv.ListenAndServe()
				} else {
					err = srv.ListenAndServeTLS(certPath, keyPath)
				}
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("https serve", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return srv.Shutdown(ctx)
		},
	})

	// SP-AC-7 SHARED: 通用可观测性 admin server (/healthz, /readyz, /metrics, /debug/pprof/*, /admin/log-level).
	adminPort := v.GetString("admin_http.port")
	if adminPort == "" {
		adminPort = "9099"
	}
	logLevel := zap.NewAtomicLevelAt(zap.InfoLevel)
	adminSrv := obsbootstrap.NewAdminServer(obsbootstrap.AdminConfig{
		ServiceName: "card-center",
		Port:        adminPort,
		Logger:      logger,
		LogLevel:    logLevel,
	})
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			adminCtx, adminCancel := context.WithCancel(context.Background())
			lc.Append(fx.Hook{OnStop: func(_ context.Context) error { adminCancel(); return nil }})
			go func() {
				if err := adminSrv.Run(adminCtx); err != nil {
					logger.Error("admin http exited", zap.Error(err))
				}
			}()
			return nil
		},
	})
}

// corsMiddleware 给 dev 跨域 fetch 用。生产路径（同域 SDK 或 reverse proxy）
// 不该依赖这个；CORS allowed_origin 必须显式配，不接受 "*" + credentials（浏览器禁止）。
func corsMiddleware(next http.Handler, allowedOrigin string, logger *zap.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (origin == allowedOrigin || allowedOrigin == "*") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "300")
		}
		// 预检直接 204
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// buildClientMTLS 共用：mTLS client 的 tls.Config（cert + server CA 校验）
func buildClientMTLS(certPath, keyPath, serverCA string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if certPath != "" && keyPath != "" {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("client keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	if serverCA != "" {
		pool := x509.NewCertPool()
		ca, err := os.ReadFile(serverCA)
		if err != nil {
			return nil, fmt.Errorf("server_ca: %w", err)
		}
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("server_ca PEM parse failed")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
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
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
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
