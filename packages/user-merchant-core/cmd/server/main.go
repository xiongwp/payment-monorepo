// Command server 启动 user-merchant-core gRPC 服务。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/fx"
	"go.uber.org/zap"

	accv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
	accountingservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingservice"
	riskv1 "github.com/xiongwp/risk-manage/kitex_gen/risk/v1"
	riskservice "github.com/xiongwp/risk-manage/kitex_gen/risk/v1/riskservice"
	"github.com/xiongwp/payment-util/kitexutil"

	"github.com/xiongwp/user-merchant-core/internal/authpkg"
	"github.com/xiongwp/user-merchant-core/internal/cache"
	"github.com/xiongwp/user-merchant-core/internal/healthz"
	"github.com/xiongwp/user-merchant-core/internal/cardcenterclient"
	"github.com/xiongwp/user-merchant-core/internal/idgen"
	"github.com/xiongwp/user-merchant-core/internal/kmsclient"
	"github.com/xiongwp/user-merchant-core/internal/metrics"
	"github.com/xiongwp/user-merchant-core/internal/repo"
	"github.com/xiongwp/user-merchant-core/internal/server"
	"github.com/xiongwp/user-merchant-core/internal/service"
	"github.com/xiongwp/user-merchant-core/internal/sharding"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/xiongwp/payment-util/configcenter"

	"github.com/xiongwp/user-merchant-core/pkg/configx"
	"github.com/xiongwp/user-merchant-core/pkg/dbx"
	"github.com/xiongwp/user-merchant-core/internal/auditstore"
	"github.com/xiongwp/user-merchant-core/pkg/tracex"
)

func main() {
	metrics.Register()
	// fx 默认 15 秒 Start/Stop 超时；我们的 Stop 要做 DB + gRPC graceful drain，
	// 放宽到 30 秒避免 SIGTERM 到来时 drain 被硬截断。
	app := fx.New(
		fx.StartTimeout(30*time.Second),
		fx.StopTimeout(30*time.Second),
		fx.Provide(
			loadConfig,
			newLogger,
			// config-center: JWT TTL / OTP / bcrypt cost / retention 等动态配置
			configcenter.FxProvider("user-merchant-core"),
			newDBManager,
			newShardRouter,
			newIDGen,
			newMerchantCache,
			newSecretCache,
			newIntrospectCache,
			// repos
			repoMerchant,
			repoMerchantSecret,
			repoIdempotency,
			repoAudit,
			repoUser,
			repoUserCard,
			// services
			newKMSClient,
			newCardCenterClient, // 给 user_card service revoke / 异步 best-effort 调用 (PAN 单跳后仅 DeleteCard 用)
			svcMerchant,
			svcMerchantSecret,
			newAuthIssuer,
			newMailer,
			newRiskClient,
			newAccountingClient,
			svcUser,
			svcUserCard,
			// store adapters (repo → grpcutil 接口)
			newIdempotencyStore,
			newAuditStore,
			// server
			newServer,
		),
		fx.Invoke(startGRPC, startMetricsHTTP, warmupMerchantCache, startRetentionSweeper, initOTel, applyShadowTables, startServiceRegistrar),
	)
	app.Run()
}

// ─── config / logger / infra ─────────────────────────────────────────────────

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("USERMERCHANTCORE")
	// 把 yaml 键里的 "." 映射到 env 的 "_"，例如
	// kms.endpoint → USERMERCHANTCORE_KMS_ENDPOINT
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// AutomaticEnv 仅自动绑定已在 yaml 出现的 key；BindEnv 兜底保证下面这些
	// 在 base config.yaml 没列的 key 也能被 USERMERCHANTCORE_<KEY> env 读到。
	for _, k := range []string{
		"risk.endpoint", "accounting.endpoint", "kms.endpoint",
		"registry.endpoints", "env",
		// 服务自注册（被 card-center / api-gateway / order-core / BFF 调用）
		"registry.service_name", "registry.advertise_host", "registry.ttl",
		"server.grpc_port",
	} {
		_ = v.BindEnv(k)
	}
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/user-merchant-core")
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
	}
	// 启动期配置校验：缺 DSN / 负超时 / 错误枚举都在这里 fail fast，
	// 比到第一个 RPC 才 panic 好调试。
	// introspect cache 默认值（10K TPS 下 30s/100k 是甜蜜点；可配置覆盖）。
	v.SetDefault("cache.introspect.enabled", true)
	v.SetDefault("cache.introspect.size", 100_000)
	v.SetDefault("cache.introspect.ttl", 30*time.Second)

	if err := configx.Run(v,
		configx.Required("database.meta.dsn"),
		configx.DurationAtLeast("timeouts.default", 100*time.Millisecond),
		configx.NonNegativeInt("cache.merchant.size"),
		configx.NonNegativeInt("cache.secret.size"),
		configx.NonNegativeInt("cache.introspect.size"),
		configx.When(configx.KeySet("kms.endpoint"),
			configx.PositiveDuration("kms.rpc_timeout")),
	); err != nil {
		return nil, err
	}
	if err := assertProdSafety(v); err != nil {
		return nil, err
	}
	return v, nil
}

// assertProdSafety env=prod 下的 fail-fast 安全校验。
//
// 必须满足：auth.allow_unauthenticated=false + kms.endpoint 配置 + accounting.endpoint 配置。
func assertProdSafety(v *viper.Viper) error {
	env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
	if env != "prod" && env != "production" {
		return nil
	}
	if v.GetBool("auth.allow_unauthenticated") {
		return fmt.Errorf("PROD-SAFETY: auth.allow_unauthenticated=true is forbidden in env=prod")
	}
	// JWT 算法：prod 强制 RS256（私钥签 + 公钥验）。HS256 共享密钥泄漏 = 全站伪造。
	if alg := strings.ToUpper(strings.TrimSpace(v.GetString("auth.jwt_alg"))); alg != "RS256" {
		return fmt.Errorf("PROD-SAFETY: auth.jwt_alg must be RS256 in env=prod (got %q)", alg)
	}
	for _, k := range []string{"auth.jwt_private_key_path", "auth.jwt_public_key_path"} {
		if strings.TrimSpace(v.GetString(k)) == "" {
			return fmt.Errorf("PROD-SAFETY: %s must be configured (RS256 keys required)", k)
		}
	}
	if strings.TrimSpace(v.GetString("kms.endpoint")) == "" {
		return fmt.Errorf("PROD-SAFETY: kms.endpoint must be configured in env=prod")
	}
	if strings.TrimSpace(v.GetString("accounting.endpoint")) == "" && len(v.GetStringSlice("registry.endpoints")) == 0 {
		return fmt.Errorf("PROD-SAFETY: accounting.endpoint must be configured in env=prod")
	}
	// rate limit：per-merchant default rps 必须配
	if v.GetFloat64("rate_limit.per_merchant.default_rps") <= 0 {
		return fmt.Errorf("PROD-SAFETY: rate_limit.per_merchant.default_rps bootstrap must be > 0 in env=prod (config-center 不可达兜底；recommend 100)")
	}
	return configcenter.AssertProdMandatory(v)
}

func newLogger() (*zap.Logger, error) {
	if os.Getenv("USERMERCHANTCORE_LOG_DEV") == "true" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

func newDBManager(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger) (*repo.Manager, error) {
	// 把 zap 注入 gorm，使 repo 每次 SQL 都打 trace（sql + rows + 毫秒数）
	repo.SetSQLLogger(logger.Named("sql"))
	var meta repo.DBConfig
	if err := v.UnmarshalKey("database.meta", &meta); err != nil {
		return nil, err
	}
	var replicas []repo.DBConfig
	if err := v.UnmarshalKey("database.replicas", &replicas); err != nil {
		return nil, err
	}
	// ── 10 shard DB（按 dbIdx 0..9，对应 user_merchant_db_0..9）──
	// USERMERCHANTCORE_DATABASE_SHARD_<N>_DSN 任意一个为空时退化到单 meta 模式
	// （dev / 单元测试），生产 docker-compose 给齐 10 条 DSN。
	shards := make([]repo.DBConfig, 0, sharding.ShardDBCount)
	for i := 0; i < sharding.ShardDBCount; i++ {
		key := fmt.Sprintf("database.shard_%d", i)
		var s repo.DBConfig
		if err := v.UnmarshalKey(key, &s); err != nil {
			return nil, err
		}
		if s.DSN == "" {
			break
		}
		if s.Name == "" {
			s.Name = fmt.Sprintf("shard-%d", i)
		}
		shards = append(shards, s)
	}
	if len(shards) > 0 && len(shards) < sharding.ShardDBCount {
		return nil, fmt.Errorf("dbx: shard DSN incomplete (got %d, want %d) — set all USERMERCHANTCORE_DATABASE_SHARD_<0..9>_DSN or none", len(shards), sharding.ShardDBCount)
	}
	mgr, err := repo.NewShardedManager(meta, shards, replicas...)
	if err != nil {
		return nil, err
	}
	// 把池统计导出给 Prometheus；/metrics 上能看到每个 DB 实例的 open/in_use/
	// idle/wait 等，运维能判断是否该调大 MaxOpenConns。
	prometheus.MustRegister(dbx.NewPoolCollector(mgr))
	logger.Info("database ready",
		zap.String("meta", meta.Name),
		zap.Int("replicas", len(replicas)),
		zap.Int("shards", mgr.ShardCount()))
	// 优雅停机：fx 反向触发 OnStop；Manager.Close 会把主库 + 全部 replica 的
	// *sql.DB 池关掉。排在 gRPC server GracefulStop 之后，确保不会关掉仍在被
	// in-flight 请求使用的连接。
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			if err := mgr.Close(); err != nil {
				logger.Warn("db close", zap.Error(err))
			}
			return nil
		},
	})
	return mgr, nil
}

func newIDGen(mgr *repo.Manager, logger *zap.Logger) (idgen.IDGenerator, error) {
	return idgen.New(mgr.GetMeta(), logger)
}

// newMerchantCache 构造共享的 LRU+TTL merchant 缓存。cache.size / cache.ttl
// 为 0 或缺省时走默认值（10k / 60s）。MetricsHook 桥接到 Prometheus，让运维
// 能在 Grafana 上看命中率。
func newMerchantCache(v *viper.Viper, logger *zap.Logger) *cache.MerchantCache {
	size := v.GetInt("cache.merchant.size")
	ttl := v.GetDuration("cache.merchant.ttl")
	c := cache.New(size, ttl, promMerchantCacheHook{})
	logger.Info("merchant cache ready",
		zap.Int("size", size),
		zap.Duration("ttl", ttl))
	return c
}

// promMerchantCacheHook 把 cache.MetricsHook 桥到本服务 Prometheus 指标。
type promMerchantCacheHook struct{}

func (promMerchantCacheHook) Lookup(index string, hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	metrics.MerchantCacheLookupTotal.WithLabelValues(index, result).Inc()
}

func (promMerchantCacheHook) Size(index string, n int) {
	metrics.MerchantCacheSize.WithLabelValues(index).Set(float64(n))
}

// ─── sharding router ─────────────────────────────────────────────────────────

// newShardRouter 跟 accounting-system / order-core / payment-channel 同形：
// 10 库 × 10 表 = 100 张全局分片表。dev 单库模式下 mgr.ShardCount()==0；这时
// router 仍构造，但 repo 层的 shardForUser/shardForMerchant helper 会 fallback
// 到 mgr.GetMeta() + 不带 _NN 后缀的表名。
func newShardRouter() *sharding.Router {
	return sharding.NewRouter()
}

// applyShadowTables 启动期自愈跨 10 个分库的影子表（CREATE TABLE LIKE 主表）。
// 主表不存在 / 已存在都跳过，单表错误不阻断；fx.Invoke 让它在 fx.Provide 阶段
// 完成 DI 后立即跑。
func applyShadowTables(lc fx.Lifecycle, mgr *repo.Manager, logger *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := repo.ApplyShadowTables(ctx, mgr, sharding.ShardTablePerDB, logger); err != nil {
				logger.Warn("apply shadow tables", zap.Error(err))
			}
			return nil // 失败不阻断启动；shadow 流量未启用时无影响
		},
	})
}

// ─── repositories ────────────────────────────────────────────────────────────

func repoMerchant(mgr *repo.Manager, router *sharding.Router) repo.MerchantRepository {
	return repo.NewMerchantRepository(mgr, router)
}

func repoMerchantSecret(mgr *repo.Manager, router *sharding.Router) repo.MerchantSecretRepository {
	return repo.NewMerchantSecretRepository(mgr, router)
}

func repoIdempotency(mgr *repo.Manager) repo.IdempotencyRepository {
	return repo.NewIdempotencyRepository(mgr)
}

func repoAudit(mgr *repo.Manager, router *sharding.Router) repo.AuditRepository {
	return repo.NewAuditRepository(mgr, router)
}

func repoUser(mgr *repo.Manager, router *sharding.Router) repo.UserRepository {
	return repo.NewUserRepository(mgr, router)
}

func repoUserCard(mgr *repo.Manager, router *sharding.Router) repo.UserCardRepository {
	return repo.NewUserCardRepository(mgr, router)
}

// newCardCenterClient 构造 card-center mTLS gRPC client；endpoint 为空 → 返 nil。
//
// 注意：PAN 单跳后，user-merchant-core.UserCardService.AttachCard **不再调** Tokenize。
// cardCenter 仅在 DeleteCard 路径用作"通知 card-center 把 stored_token 标 revoked"
// 的 best-effort 异步调用。endpoint 为空 / dev 不配也能跑（DeleteCard 仍会软删 user_card 表，
// 只是 card-center 那边不知道 token 已撤销）。
func newCardCenterClient(v *viper.Viper, logger *zap.Logger) *cardcenterclient.Client {
	endpoint := v.GetString("card_center.endpoint")
	registry := v.GetStringSlice("registry.endpoints")
	if endpoint == "" && len(registry) == 0 {
		logger.Info("card_center.endpoint not set; UserCardService DeleteCard 不会通知 card-center revoke (dev OK)")
		return nil
	}
	cli, err := cardcenterclient.New(cardcenterclient.Config{
		Endpoint:          endpoint,
		RegistryEndpoints: registry,
		BearerToken:       v.GetString("card_center.bearer_token"),
		RPCTimeout:        v.GetDuration("card_center.rpc_timeout"),
	})
	if err != nil {
		logger.Warn("card-center client init failed; degrading to nil", zap.Error(err))
		return nil
	}
	logger.Info("card-center client connected",
		zap.String("endpoint", endpoint), zap.Strings("registry", registry))
	return cli
}

// svcUserCard 装配 *service.UserCardService。
func svcUserCard(
	r repo.UserCardRepository,
	cc *cardcenterclient.Client,
	logger *zap.Logger,
) *service.UserCardService {
	return service.NewUserCardService(r, cc, logger)
}

func newIdempotencyStore(r repo.IdempotencyRepository) auditstore.IdempotencyStore {
	return server.NewIdempotencyStore(r)
}

func newAuditStore(r repo.AuditRepository) auditstore.AuditStore {
	return server.NewAuditStore(r)
}

// ─── services ────────────────────────────────────────────────────────────────

func svcMerchant(r repo.MerchantRepository, g idgen.IDGenerator, c *cache.MerchantCache, logger *zap.Logger) service.MerchantService {
	return service.NewMerchantService(r, g, c, logger)
}

// newKMSClient dials kms-manage if configured. kms.endpoint empty → nil client
// (secret service returns error if anyone tries to Put/Get).
func newKMSClient(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger) service.KMSClient {
	endpoint := v.GetString("kms.endpoint")
	if endpoint == "" {
		logger.Warn("kms-manage not configured; merchant-secret service disabled")
		return nil
	}
	c, err := kmsclient.Dial(endpoint, v.GetString("kms.bearer_token"), v.GetDuration("kms.rpc_timeout"))
	if err != nil {
		logger.Error("dial kms-manage failed; merchant-secret disabled", zap.Error(err))
		return nil
	}
	logger.Info("kms-manage client ready", zap.String("endpoint", endpoint))
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			if err := c.Close(); err != nil {
				logger.Warn("kms conn close", zap.Error(err))
			}
			return nil
		},
	})
	return c
}

func svcMerchantSecret(r repo.MerchantSecretRepository, kms service.KMSClient, c *cache.SecretCache, logger *zap.Logger) service.MerchantSecretService {
	return service.NewMerchantSecretService(r, kms, c, logger)
}

// newSecretCache: decrypted secret bucket cache. size 默认 1024，ttl 默认 5m。
func newSecretCache(v *viper.Viper, logger *zap.Logger) *cache.SecretCache {
	size := v.GetInt("cache.secret.size")
	ttl := v.GetDuration("cache.secret.ttl")
	c := cache.NewSecretCache(size, ttl, promSecretCacheHook{})
	logger.Info("merchant secret cache ready", zap.Int("size", size), zap.Duration("ttl", ttl))
	return c
}

type promSecretCacheHook struct{}

func (promSecretCacheHook) Lookup(hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	metrics.MerchantSecretPlaintextCacheLookupTotal.WithLabelValues(result).Inc()
}

// ─── server ──────────────────────────────────────────────────────────────────

func newServer(
	mch service.MerchantService,
	mchSecret service.MerchantSecretService,
	user *service.UserService,
	userCard *service.UserCardService,
	mchCache *cache.MerchantCache,
	idemStore auditstore.IdempotencyStore,
	auditStr auditstore.AuditStore,
	auditRepo repo.AuditRepository,
	v *viper.Viper,
	logger *zap.Logger,
) *server.Server {
	// AuthTokens / RateLimit / PerKey / Timeouts 等老 gRPC interceptor 配置已删 —
	// Kitex 切换后这些走 server.WithMiddleware (kitexutil.* MW) 配置, 不再由
	// Server 结构体持有.

	// Mutation method 白名单：幂等键 + 审计只对这些方法开启。
	// AuthenticateByAPIKey / Get / List / BatchGet 是纯读，不在内。
	mutations := map[string]struct{}{
		"/usermerchant.v1.MerchantService/Create":          {},
		"/usermerchant.v1.MerchantService/Update":          {},
		"/usermerchant.v1.MerchantService/RotateApiKey":    {},
		"/usermerchant.v1.MerchantService/SubmitKyc":       {},
		"/usermerchant.v1.MerchantService/StartReview":     {},
		"/usermerchant.v1.MerchantService/Approve":         {},
		"/usermerchant.v1.MerchantService/Reject":          {},
		"/usermerchant.v1.MerchantService/RequestMoreInfo": {},
		"/usermerchant.v1.MerchantService/Suspend":         {},
		"/usermerchant.v1.MerchantService/Unsuspend":       {},
		"/usermerchant.v1.MerchantService/Terminate":       {},
		"/usermerchant.v1.MerchantService/AddDocument":     {},
		"/usermerchant.v1.MerchantService/ReviewDocument":  {},
		"/usermerchant.v1.MerchantSecretService/Put":       {},
		"/usermerchant.v1.MerchantSecretService/Delete":    {},
	}

	return server.NewServer(server.Deps{
		MerchantSvc:        mch,
		MerchantSecretSvc:  mchSecret,
		UserSvc:            user,
		UserCardSvc:        userCard,
		AuditRepo:          auditRepo,
		MerchantCache:      mchCache,
		MerchantDefaultRPS: v.GetFloat64("rate_limit.per_merchant.default_rps"),
		IdempotencyStore:   idemStore,
		MutationMethods:    mutations,
		AuditStore:         auditStr,
		Logger:             logger,
	})
}

// initOTel OTLP/gRPC 导出器初始化；endpoint 空就 no-op。shutdown 挂 OnStop，
// 让 fx 关闭时把 batch 导完。
func initOTel(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger) error {
	cfg := tracex.OTelConfig{
		Endpoint:     v.GetString("otel.endpoint"),
		ServiceName:  "user-merchant-core",
		Env:          v.GetString("otel.env"),
		SampleRatio:  v.GetFloat64("otel.sample_ratio"),
		ExportTimeout: v.GetDuration("otel.export_timeout"),
		Insecure:     v.GetBool("otel.insecure"),
	}
	shutdown, err := tracex.InitOTel(context.Background(), cfg)
	if err != nil {
		logger.Warn("otel init failed; continuing without tracing", zap.Error(err))
		return nil // 不阻断启动
	}
	if cfg.Endpoint != "" {
		logger.Info("otel exporter ready",
			zap.String("endpoint", cfg.Endpoint),
			zap.Float64("sample_ratio", cfg.SampleRatio))
	}
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			return shutdown(ctx)
		},
	})
	return nil
}

// startRetentionSweeper 定时真删软删超期商户。合规默认 7 年；scan 间隔 24h。
// retention.enabled=false 时跳过（CI / dev 环境通常关）。
//
// config-center 优先：admin 改 namespace=user-merchant-core 下
//   retention.duration / retention.interval 即时下发；本 worker 启动期 snapshot
//   到 sweeper 内部，热更需重启（NewRetentionSweeper 当前没暴露 SetConfig）。
func startRetentionSweeper(lc fx.Lifecycle, r repo.MerchantRepository, v *viper.Viper, cli *configcenter.Client, logger *zap.Logger) {
	if !v.GetBool("retention.enabled") {
		return
	}
	// retention 100% 走 config-center；不可达 → hardcoded（7y / 24h）。
	duration := 7 * 365 * 24 * time.Hour
	interval := 24 * time.Hour
	if cli != nil {
		ctx := context.Background()
		duration = cli.GetDuration(ctx, "retention.duration", duration)
		interval = cli.GetDuration(ctx, "retention.interval", interval)
	}
	sweeper := service.NewRetentionSweeper(r, duration, interval, logger.Named("retention"))
	swCtx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go sweeper.Start(swCtx)
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

// warmupMerchantCache 服务启动后把 active 商户预加载到缓存；limit 配置项
// cache.merchant.warmup <=0 跳过，避免大规模商户集群拉爆内存或启动耗时。
func warmupMerchantCache(lc fx.Lifecycle, svc service.MerchantService, v *viper.Viper, logger *zap.Logger) {
	limit := v.GetInt("cache.merchant.warmup")
	if limit <= 0 {
		return
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := svc.WarmupCache(ctx, limit); err != nil {
				logger.Warn("merchant cache warmup failed", zap.Error(err))
				return nil // warmup 失败不阻塞启动
			}
			return nil
		},
	})
}

func startMetricsHTTP(lc fx.Lifecycle, mgr *repo.Manager, v *viper.Viper, logger *zap.Logger) {
	addr := v.GetString("metrics.addr")
	if addr == "" {
		addr = ":9291"
	}
	// /healthz 现在会做 meta DB ping；DB 挂了返回 503，k8s liveness/readiness 可立即重启。
	dbPing := healthz.NewDBPinger(mgr)
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			metrics.StartServer(addr, logger, dbPing)
			return nil
		},
	})
}

func startGRPC(lc fx.Lifecycle, s *server.Server, v *viper.Viper, logger *zap.Logger) {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9191
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				defer close(done)
				if err := s.ListenAndServe(ctx, port); err != nil {
					logger.Error("grpc exited", zap.Error(err))
				}
			}()
			return nil
		},
		// 真正等 GracefulStop 返回再继续反向 OnStop —— 这样 DB 关掉时不会
		// 打断仍在处理的 RPC。fx.StopTimeout 兜底 30s 防止 drain 挂死。
		OnStop: func(stopCtx context.Context) error {
			cancel()
			select {
			case <-done:
				logger.Info("grpc drained")
			case <-stopCtx.Done():
				logger.Warn("grpc drain timeout; continuing shutdown")
			}
			return nil
		},
	})
}

// ─── User-side services ─────────────────────────────────────────────────────

// newAuthIssuer 装配 JWT issuer。
//
// 算法选择：
//   - auth.jwt_alg = "RS256" → 用 auth.jwt_private_key_path / auth.jwt_public_key_path（生产）
//   - auth.jwt_alg = "HS256" 或缺省 → 用 auth.jwt_secret（dev / staging）
//
// assertProdSafety 在 env=prod 已经强制 jwt_alg == RS256；这里 fail-soft 不做二次校验。
func newAuthIssuer(v *viper.Viper, cli *configcenter.Client) (*authpkg.Issuer, error) {
	alg := authpkg.Algorithm(v.GetString("auth.jwt_alg"))
	if alg == "" {
		alg = authpkg.AlgHS256
	}
	// jwt.ttl 100% 走 config-center；不可达 → hardcoded 24h。
	// admin /admin/ns/user-merchant-core 改 jwt.ttl 下次 Issue 生效（Issuer 没暴露 SetTTL）。
	ttl := 24 * time.Hour
	if cli != nil {
		ttl = cli.GetDuration(context.Background(), "jwt.ttl", ttl)
	}
	cfg := authpkg.IssuerConfig{
		Algorithm:  alg,
		TTL:        ttl,
		IssuerName: "user-merchant-core",
		KID:        v.GetString("auth.jwt_kid"),
	}
	switch alg {
	case authpkg.AlgRS256:
		cfg.PrivateKeyPEMPath = v.GetString("auth.jwt_private_key_path")
		cfg.PublicKeyPEMPath = v.GetString("auth.jwt_public_key_path")
	default:
		secret := v.GetString("auth.jwt_secret")
		if secret == "" {
			secret = "dev-jwt-secret-change-me-in-prod"
		}
		cfg.Secret = secret
	}
	return authpkg.NewIssuerFromConfig(cfg)
}

func newMailer(logger *zap.Logger) service.Mailer {
	// 生产换 SMTPMailer / SendGridMailer / TwilioMailer
	return &service.LogMailer{Logger: logger}
}

// newRiskClient 构造 risk-manage Kitex client.
// endpoint / registry 都空 → NoopRiskClient (Screen 全 ALLOW, Report no-op).
// registry 非空走 etcd resolver; 否则用 endpoint 直连.
func newRiskClient(_ fx.Lifecycle, v *viper.Viper, logger *zap.Logger) service.RiskClient {
	endpoint := v.GetString("risk.endpoint")
	registry := v.GetStringSlice("registry.endpoints")
	if endpoint == "" && len(registry) == 0 {
		logger.Info("risk.endpoint and registry.endpoints both unset; using NoopRiskClient (all-allow, no graph writes)")
		return service.NoopRiskClient{}
	}
	api := kitexutil.MustKitexClient(riskservice.NewClient("risk-manage"))
	logger.Info("risk client constructed (Kitex)",
		zap.String("endpoint", endpoint), zap.Strings("registry", registry))
	return &grpcRiskAdapter{api: api, timeout: v.GetDuration("risk.rpc_timeout")}
}

// grpcRiskAdapter 把 Kitex riskservice.Client 适配到 service.RiskClient 接口.
type grpcRiskAdapter struct {
	api     riskservice.Client
	timeout time.Duration
}

func (a *grpcRiskAdapter) Screen(ctx context.Context, req *riskv1.ScreenRequest) (*riskv1.ScreenResponse, error) {
	if a.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.timeout)
		defer cancel()
	}
	return a.api.Screen(ctx, req)
}

func (a *grpcRiskAdapter) Report(ctx context.Context, req *riskv1.ReportRequest) error {
	if a.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.timeout)
		defer cancel()
	}
	_, err := a.api.Report(ctx, req)
	return err
}

func svcUser(
	r repo.UserRepository,
	g idgen.IDGenerator,
	issuer *authpkg.Issuer,
	mailer service.Mailer,
	risk service.RiskClient,
	accounting service.AccountingClient,
	introspectCache *cache.IntrospectCache,
	v *viper.Viper,
	logger *zap.Logger,
) *service.UserService {
	currency := v.GetString("auth.default_currency")
	s := service.NewUserService(r, g, issuer, mailer, risk, accounting, currency, logger)
	if introspectCache != nil {
		s.SetIntrospectCache(introspectCache)
	}
	return s
}

// newIntrospectCache JWT introspect 进程内缓存。
// 默认 100k cap / 30s TTL（参考 docs/CAPACITY_10K_TPS.md §2.4）；
// 配置项：cache.introspect.size / cache.introspect.ttl。
//
// size <= 0 取默认 100_000；ttl <= 0 取默认 30s。
// 配 0 显式关掉 cache 也允许（cache.introspect.enabled=false）。
func newIntrospectCache(v *viper.Viper, logger *zap.Logger) *cache.IntrospectCache {
	if !v.GetBool("cache.introspect.enabled") &&
		v.IsSet("cache.introspect.enabled") {
		logger.Info("introspect cache disabled by config; IntrospectToken will hit DB on every call")
		return nil
	}
	size := v.GetInt("cache.introspect.size")
	ttl := v.GetDuration("cache.introspect.ttl")
	c := cache.NewIntrospectCache(size, ttl, promIntrospectCacheHook{})
	logger.Info("introspect cache ready",
		zap.Int("size", size), zap.Duration("ttl", ttl))
	return c
}

type promIntrospectCacheHook struct{}

func (promIntrospectCacheHook) Lookup(hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	metrics.IntrospectCacheLookupTotal.WithLabelValues(result).Inc()
}

// newAccountingClient 构造 accounting-system Kitex client.
// endpoint / registry 都空 → NoopAccountingClient (Register 流程拿不到 account_no,
// 绑定步骤被 service 层 skip, 首次支付兜底重试).
// registry 非空走 etcd resolver; 否则用 endpoint 直连.
func newAccountingClient(_ fx.Lifecycle, v *viper.Viper, logger *zap.Logger) service.AccountingClient {
	endpoint := v.GetString("accounting.endpoint")
	registry := v.GetStringSlice("registry.endpoints")
	if endpoint == "" && len(registry) == 0 {
		logger.Info("accounting.endpoint and registry.endpoints both unset; using NoopAccountingClient (no account binding on Register)")
		return service.NoopAccountingClient{}
	}
	api := kitexutil.MustKitexClient(accountingservice.NewClient("accounting-system"))
	logger.Info("accounting client constructed (Kitex)",
		zap.String("endpoint", endpoint), zap.Strings("registry", registry))
	return &grpcAccountingAdapter{api: api, timeout: v.GetDuration("accounting.rpc_timeout")}
}

// grpcAccountingAdapter 适配 Kitex accountingservice.Client 到 service.AccountingClient.
type grpcAccountingAdapter struct {
	api     accountingservice.Client
	timeout time.Duration
}

func (a *grpcAccountingAdapter) CreateAccount(ctx context.Context, req *accv1.CreateAccountRequest) (*accv1.CreateAccountResponse, error) {
	if a.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.timeout)
		defer cancel()
	}
	return a.api.CreateAccount(ctx, req)
}
