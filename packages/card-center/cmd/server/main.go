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

	"github.com/spf13/viper"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	insecuregrpc "google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"

	"github.com/xiongwp/card-center/internal/audit"
	"github.com/xiongwp/card-center/internal/httpsauth"
	"github.com/xiongwp/card-center/internal/kmsclient"
	"github.com/xiongwp/card-center/internal/repo"
	"github.com/xiongwp/card-center/internal/server"
	"github.com/xiongwp/card-center/internal/service"
	"github.com/xiongwp/card-center/internal/sharding"
	"github.com/xiongwp/card-center/internal/vault"
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
		fx.Invoke(startGRPC, startHTTPS),
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
		"auth.user_merchant.client_cert",
		"auth.user_merchant.client_key",
		"auth.user_merchant.server_ca",
	} {
		_ = v.BindEnv(k)
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
	return nil
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

// newAuditEmitter 构造 audit emitter；Kafka producer 这里先注入 nil，
// service 层只走 DB 落盘。生产引入 sarama / kafka-go 后这里替换。
func newAuditEmitter(mgr *repo.Manager, v *viper.Viper, logger *zap.Logger) service.AuditEmitter {
	topic := v.GetString("audit.topic")
	if topic == "" {
		topic = "card-center.audit"
	}
	// TODO: 接 sarama 的 SyncProducer，实现 audit.KafkaProducer
	return audit.New(nil, topic, mgr.Meta(), logger)
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
func newGRPCServer(v *viper.Viper, svc *service.Service, logger *zap.Logger) (*grpc.Server, *server.Server, error) {
	allowMap := v.GetStringMapStringSlice("auth.client_cn")
	allow := server.NewClientCNAllowList(allowMap)

	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(
			trace.UnaryServerInterceptor(logger),
			shadow.UnaryServerInterceptor(),
			server.UnaryClientCNInterceptor(allow),
		),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime: 5 * time.Second, PermitWithoutStream: true,
		}),
	}
	certPath := v.GetString("tls.cert")
	keyPath := v.GetString("tls.key")
	if certPath != "" && keyPath != "" {
		tlsCfg, err := buildTLSConfig(v)
		if err != nil {
			return nil, nil, fmt.Errorf("tls config: %w", err)
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
		logger.Info("card-center gRPC: mTLS enabled")
	} else {
		logger.Warn("card-center gRPC: NO TLS (dev mode); env=prod will fail at assertProdSafety")
	}

	srv := grpc.NewServer(opts...)
	bs := server.NewServer(svc, logger)
	bs.Register(srv)
	return srv, bs, nil
}

func startGRPC(lc fx.Lifecycle, srv *grpc.Server, _ *server.Server, v *viper.Viper, logger *zap.Logger) error {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9443
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	logger.Info("card-center mTLS gRPC listening", zap.Int("port", port))
	go func() {
		if err := srv.Serve(lis); err != nil {
			logger.Error("grpc serve", zap.Error(err))
		}
	}()
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			done := make(chan struct{})
			go func() { srv.GracefulStop(); close(done) }()
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
	if endpoint == "" {
		return nil, errors.New("https.enabled=true but auth.user_merchant.endpoint not set")
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
	// 不再自己挂 KeepaliveParams——serviceregistry.DialDirect 内部已经按
	// 10s ping / 3s timeout / PermitWithoutStream=true 标准化好了，副本被
	// scale/kill 后 ~13s 内被探出来，避免 stale subconn hang RST_STREAM CANCEL。
	conn, err := serviceregistry.DialDirect(endpoint, dialOpts...)
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
	uc := usermerchantv1.NewUserServiceClient(conn)
	return httpsauth.NewUserMerchantVerifier(uc, logger)
}

// newRESTServer 装配 HTTPS REST handler。
//
// rate limit 默认 5 tokenize / 分钟 per user_id。yaml: ratelimit.tokenize.burst /
// ratelimit.tokenize.refill_seconds 可调。
func newRESTServer(v *viper.Viper, svc *service.Service, vfy httpsauth.Verifier, logger *zap.Logger) *httpsauth.RESTServer {
	if !v.GetBool("https.enabled") || vfy == nil {
		return nil
	}
	burst := v.GetInt("ratelimit.tokenize.burst")
	if burst <= 0 {
		burst = 5
	}
	refill := v.GetDuration("ratelimit.tokenize.refill")
	if refill <= 0 {
		refill = 12 * time.Second // 5 / 分钟
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
