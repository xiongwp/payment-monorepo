// Command server 启动 order-core gRPC 服务。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/xiongwp/order-core/internal/accounting"
	"github.com/xiongwp/order-core/internal/channel"
	"github.com/xiongwp/order-core/internal/channel/paymentcoreclient"
	"github.com/xiongwp/order-core/internal/idgen"
	"github.com/xiongwp/order-core/internal/metrics"
	"github.com/xiongwp/order-core/internal/repo"
	"github.com/xiongwp/order-core/internal/server"
	"github.com/xiongwp/order-core/internal/service"
	"github.com/xiongwp/order-core/internal/sharding"
	"github.com/xiongwp/order-core/internal/webhook"
)

func main() {
	metrics.Register()
	// OTel 初始化：OTEL_EXPORTER_OTLP_ENDPOINT 空 → no-op；非空 → 走 OTLP gRPC
	// 推 span 到 collector（Jaeger / Tempo / Grafana Agent）。shutdown 在进程
	// 退出前 flush pending span。
	otelShutdown, otelErr := trace.InitOTel(context.Background(), "order-core", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
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
			newLeaderToolkit,
			newLogger,
			newRouter,
			newDBManager,
			newIDGen,
			// 渠道
			newChannelRegistry,
			newProviderRegistry,
			// repos
			repoPaymentIntent,
			repoCharge,
			repoRefund,
			repoPayAction,
			repoNotifyLog,
			repoInboundWebhook,
			repoAccountingOutbox,
			repoAuditLog,
			newWebhookDispatcher,
			// services
			svcPaymentIntent,
			svcCharge,
			svcRefund,
			svcPayAction,
			svcRefundReconcile,
			svcAccountingOutbox,
			newAccountingClient,
			svcWebhook,
			repoLedger,
			svcLedger,
			repoDispute,
			svcDispute,
			// server / workers
			newServer,
			newExpireWorker,
			newRefundRetryWorker,
			newChargeExpireWorker,
			newReconcileWorker,
			newAccountingOutboxWorker,
			newAccountingOutboxArchiver,
		),
		fx.Invoke(startGRPC, startExpireWorker, startRefundRetryWorker, startChargeExpireWorker, startReconcileWorker, startMetricsHTTP, startWebhookRetryWorker, startAccountingOutboxWorker, startAccountingOutboxArchiver, wireInlineAccountingDelivery, startServiceRegistrar),
	)
	app.Run()
}

// ─── config / logger / infra ─────────────────────────────────────────────────

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("ORDERCORE")
	// 把 yaml 键里的 "." 映射到 env 的 "_"，例如
	// payment_core.endpoint → ORDERCORE_PAYMENT_CORE_ENDPOINT
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// AutomaticEnv 的已知 quirk：某些 viper 版本只为 yaml 里**已存在**的 key 路径
	// 解析 env override，完全缺席的 key 即便 env 设了也读到空。线上 docker
	// 部署严重依赖 env 注入这两条（yaml 里写空占位也能 work，但 BindEnv 是
	// 最不依赖版本行为的兜底）。
	for _, k := range []string{
		"accounting.endpoint",
		"accounting.admin_http_addr",
		"accounting.admin_http_token",
		"payment_core.endpoint",
		"risk.endpoint",
		"kms.endpoint",
		"registry.endpoints",
	} {
		_ = v.BindEnv(k)
	}
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/order-core")
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
	}
	return v, nil
}

func newLogger() (*zap.Logger, error) {
	if os.Getenv("ORDERCORE_LOG_DEV") == "true" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

func newRouter(v *viper.Viper) (*sharding.Router, error) {
	db := v.GetInt("sharding.db_count")
	tbl := v.GetInt("sharding.table_per_db")
	// P2-2: 防止 0 值导致 router 内部除零 panic
	if db <= 0 || tbl <= 0 {
		return nil, fmt.Errorf("sharding config invalid: db_count=%d table_per_db=%d (both must be > 0)", db, tbl)
	}
	return sharding.NewRouterWithConfig(db, tbl), nil
}

func newDBManager(v *viper.Viper, logger *zap.Logger) (*repo.Manager, error) {
	// 把 zap 注入 gorm，使 repo 每次 SQL 都打 trace（sql + rows + 毫秒数）
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
		logger.Warn("database.meta not set; shards[0] will be used for meta (leaf_alloc)")
		mgr, err = repo.NewManager(shards)
	}
	if err != nil {
		return nil, err
	}
	logger.Info("database ready", zap.Int("shards", mgr.ShardCount()))

	// 启动 DB 连接池采集 goroutine（每 30s 写一次 Stats() → Prometheus）。
	// 进程结束时跟随 process 自然退出（用 background ctx；后续若想跟 fx 生命周期
	// 一起优雅关闭，可改成 lc.Append({OnStart, OnStop})）。
	mgr.StartPoolMetrics(context.Background(), 30*time.Second, logger)

	// Self-healing schema: apply embedded meta-DB migration on startup so
	// stale volumes don't leave `webhook_deliveries` / `admin_audit_log` /
	// `gl_*` missing. Every CREATE uses IF NOT EXISTS → no-op on fresh DBs.
	// Disabled by setting database.skip_auto_migrate=true.
	if !v.GetBool("database.skip_auto_migrate") && meta.DSN != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := mgr.ApplyMetaMigration(ctx, logger); err != nil {
			logger.Warn("meta migration had failures (continuing)", zap.Error(err))
		}
		// 紧跟着建 meta 影子表（webhook_deliveries_shadow / gl_*_shadow / admin_audit_log_shadow）
		// init_shadow.sql 早跑过则全 no-op；幂等。
		if err := mgr.ApplyMetaShadowTables(ctx, logger); err != nil {
			logger.Warn("meta shadow tables apply had failures (continuing)", zap.Error(err))
		}
	}

	// Shard 自愈迁移：给现存 accounting_outbox_NN 表加 claim_token 列 + idx_claim 索引
	// （新建的库已在 init 模板里带了）。INFORMATION_SCHEMA 检查后 ALTER，幂等可重复跑。
	// ApplyShadowTables 紧跟其后：每张分片业务表都建一份 _shadow 副本（CREATE … LIKE）。
	if !v.GetBool("database.skip_auto_migrate") {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tablePerDB := v.GetInt("sharding.table_per_db")
		if tablePerDB <= 0 {
			tablePerDB = 10
		}
		if err := mgr.ApplyShardMigrations(ctx, tablePerDB, logger); err != nil {
			logger.Warn("shard migrations had failures (continuing)", zap.Error(err))
		}
		if err := mgr.ApplyShadowTables(ctx, tablePerDB, logger); err != nil {
			logger.Warn("shadow tables apply had failures (continuing)", zap.Error(err))
		}
	}
	return mgr, nil
}

func newIDGen(mgr *repo.Manager, logger *zap.Logger) (idgen.IDGenerator, error) {
	return idgen.New(mgr.GetMeta(), logger)
}

// newChannelRegistry 根据配置决定 "payment-core" 这个 channel 走真 gRPC 还是 mock。
//
//   payment_core.endpoint 非空 / registry.endpoints 非空 → 用 paymentcoreclient.Dial
//                                  拨到真 payment-core（registry 优先 etcd resolver）
//   都为空                                                → 退回 NewMockPaymentCoreChannel()，本地/单测用。
//
// ENV 形态：ORDERCORE_PAYMENT_CORE_ENDPOINT=payment-core:9090 也生效（viper 自动映射）。
// ORDERCORE_REGISTRY_ENDPOINTS=etcd:2379 → 走 etcd resolver 拿所有 payment-core 副本。
func newChannelRegistry(v *viper.Viper, logger *zap.Logger) channel.PaymentChannelRegistry {
	reg := channel.NewDefaultPaymentChannelRegistry()

	endpoint := v.GetString("payment_core.endpoint")
	registry := v.GetStringSlice("registry.endpoints")
	if endpoint == "" && len(registry) == 0 {
		reg.Register("payment-core", service.NewMockPaymentCoreChannel())
		logger.Info("channel registry: mock payment-core registered (set payment_core.endpoint or registry.endpoints to switch to real gRPC)")
		return reg
	}

	timeout := v.GetDuration("payment_core.rpc_timeout")
	cli, err := paymentcoreclient.Dial("payment-core", registry, endpoint, timeout)
	if err != nil {
		// 启动期 Dial 几乎不会失败（NewClient 是惰性的），真挂了就 fatal。
		logger.Fatal("dial payment-core failed",
			zap.String("endpoint", endpoint), zap.Error(err))
	}
	reg.Register("payment-core", cli)
	logger.Info("channel registry: real payment-core registered",
		zap.String("endpoint", endpoint),
		zap.Duration("rpc_timeout", timeout))
	return reg
}

// newProviderRegistry PayAction provider 注册表（OTP / 3DS / 支付密码）。mock 阶段可空，
// 真实部署里注入 bank OTP / Adyen 3DS / 账户系统密码 store 实现。
func newProviderRegistry() *service.ProviderRegistry {
	return service.NewProviderRegistry()
}

// ─── repositories ────────────────────────────────────────────────────────────

func repoPaymentIntent(mgr *repo.Manager, r *sharding.Router) repo.PaymentIntentRepository {
	return repo.NewPaymentIntentRepository(mgr, r)
}
func repoCharge(mgr *repo.Manager, r *sharding.Router) repo.ChargeRepository {
	return repo.NewChargeRepository(mgr, r)
}
func repoRefund(mgr *repo.Manager, r *sharding.Router) repo.RefundRepository {
	return repo.NewRefundRepository(mgr, r)
}
func repoPayAction(mgr *repo.Manager, r *sharding.Router) repo.PayActionRepository {
	return repo.NewPayActionRepository(mgr, r)
}
func repoNotifyLog(mgr *repo.Manager, r *sharding.Router) repo.NotifyLogRepository {
	return repo.NewNotifyLogRepository(mgr, r)
}
func repoInboundWebhook(mgr *repo.Manager, r *sharding.Router) repo.InboundWebhookRepository {
	return repo.NewInboundWebhookRepository(mgr, r)
}
func repoAuditLog(mgr *repo.Manager) repo.AdminAuditRepository {
	return repo.NewAdminAuditRepository(mgr)
}

// newWebhookDispatcher 出站 webhook 投递器（wave C）。
// 绑定到 meta DB，因为 webhook_deliveries 和 merchants 都在 meta。
func newWebhookDispatcher(mgr *repo.Manager, logger *zap.Logger) *webhook.Dispatcher {
	return webhook.NewDispatcher(mgr.GetMeta(), webhook.DispatcherConfig{}, logger.Named("webhook"))
}

// startWebhookRetryWorker runs the outbound-webhook retry loop, waking every
// 30s to process pending/failed deliveries with next_retry_at <= now.
func startWebhookRetryWorker(lc fx.Lifecycle, d *webhook.Dispatcher, t *LeaderToolkit, logger *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			t.RunWorker(ctx, "webhook-retry", func(leaderCtx context.Context) {
				d.RunRetryWorker(leaderCtx, 30_000_000_000) // 30s
			})
			logger.Info("webhook retry worker started (leader-gated)")
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

// ─── services ────────────────────────────────────────────────────────────────

func svcPaymentIntent(
	pi repo.PaymentIntentRepository,
	ch repo.ChargeRepository,
	ar repo.PayActionRepository,
	g idgen.IDGenerator,
	r *sharding.Router,
	reg channel.PaymentChannelRegistry,
	accounting service.AccountingOutboxService,
	v *viper.Viper,
	logger *zap.Logger,
) service.PaymentIntentService {
	chName := v.GetString("channel.default_name")
	if chName == "" {
		chName = "payment-core"
	}
	return service.NewPaymentIntentService(pi, ch, ar, g, r, reg, chName, accounting, logger)
}
func svcCharge(ch repo.ChargeRepository, r *sharding.Router) service.ChargeService {
	return service.NewChargeService(ch, r)
}
func svcRefund(
	pi repo.PaymentIntentRepository,
	ch repo.ChargeRepository,
	rf repo.RefundRepository,
	accounting service.AccountingOutboxService,
	g idgen.IDGenerator,
	r *sharding.Router,
	logger *zap.Logger,
) service.RefundService {
	return service.NewRefundService(pi, ch, rf, accounting, g, r, logger)
}

func svcPayAction(
	piSvc service.PaymentIntentService,
	piRepo repo.PaymentIntentRepository,
	ar repo.PayActionRepository,
	cr repo.ChargeRepository,
	registry *service.ProviderRegistry,
	g idgen.IDGenerator,
	r *sharding.Router,
	v *viper.Viper,
	logger *zap.Logger,
) service.PayActionService {
	ttl := v.GetDuration("pay_action.default_ttl")
	return service.NewPayActionService(piSvc, piRepo, ar, cr, registry, g, r, ttl, logger)
}

func svcRefundReconcile(
	piSvc service.PaymentIntentService,
	rfSvc service.RefundService,
	piRepo repo.PaymentIntentRepository,
	chRepo repo.ChargeRepository,
	rfRepo repo.RefundRepository,
	accounting service.AccountingOutboxService,
	g idgen.IDGenerator,
	r *sharding.Router,
	logger *zap.Logger,
) *service.RefundReconcileService {
	return service.NewRefundReconcileService(piSvc, rfSvc, piRepo, chRepo, rfRepo, accounting, g, r, logger)
}

func svcWebhook(
	reg channel.PaymentChannelRegistry,
	piSvc service.PaymentIntentService,
	rfSvc service.RefundService,
	reconcile *service.RefundReconcileService,
	piRepo repo.PaymentIntentRepository,
	chRepo repo.ChargeRepository,
	rfRepo repo.RefundRepository,
	inboundRepo repo.InboundWebhookRepository,
	g idgen.IDGenerator,
	accounting service.AccountingOutboxService,
	logger *zap.Logger,
) service.WebhookService {
	return service.NewWebhookService(reg, piSvc, rfSvc, reconcile, piRepo, chRepo, rfRepo, inboundRepo, g, accounting, logger)
}
func repoLedger(mgr *repo.Manager) repo.LedgerRepository {
	return repo.NewLedgerRepository(mgr)
}
func svcLedger(r repo.LedgerRepository, g idgen.IDGenerator, logger *zap.Logger) service.LedgerService {
	return service.NewLedgerService(r, g, logger)
}
func repoDispute(mgr *repo.Manager, r *sharding.Router) repo.DisputeRepository {
	return repo.NewDisputeRepository(mgr, r)
}
func svcDispute(r repo.DisputeRepository, g idgen.IDGenerator, rf service.RefundService, logger *zap.Logger) service.DisputeService {
	return service.NewDisputeService(r, g, rf, logger)
}

func repoAccountingOutbox(mgr *repo.Manager, r *sharding.Router) repo.AccountingOutboxRepository {
	return repo.NewAccountingOutboxRepository(mgr, r)
}

func svcAccountingOutbox(r repo.AccountingOutboxRepository, g idgen.IDGenerator, logger *zap.Logger) service.AccountingOutboxService {
	return service.NewAccountingOutboxService(r, g, logger.Named("accounting-outbox"))
}

func newAccountingClient(v *viper.Viper, lc fx.Lifecycle, logger *zap.Logger) service.AccountingClient {
	addr := v.GetString("accounting.endpoint")
	registry := v.GetStringSlice("registry.endpoints")
	// addr 和 registry 都空 → outbox 工作模式关闭。否则按 registry 优先 + addr fallback 拨号。
	if addr == "" && len(registry) == 0 {
		logger.Info("accounting endpoint and registry both unset; outbox worker will skip (feature flag off)")
		return nil
	}
	cli, err := accounting.New(accounting.Config{
		Addr:              addr,
		RegistryEndpoints: registry,
		ServiceName:       "accounting-service",
		Timeout:           v.GetDuration("accounting.rpc_timeout"),
	})
	if err != nil {
		logger.Error("accounting client init failed; falling back to nil",
			zap.String("addr", addr), zap.Error(err))
		return nil
	}
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { return cli.Close() }})

	// Onboarding 改为人工运维操作（accounting-admin-web 上点 "新增 business_type"
	// + 建 fleet）。order-core 启动期**不再**调 accounting-system admin HTTP；
	// 直接从 yaml 读静态映射 (payment_method → business_type i32)。运维必须保证：
	//   1. accounting-admin-web 已注册 GCASH_RECEIVABLE / MAYA_RECEIVABLE / ... 拿到 ID
	//   2. yaml accounting.channels 列出每条 (name, business_type) 对，与 accounting 侧一致
	// 写法（替代之前 {name, code, description} 三元组）：
	//   accounting:
	//     channels:
	//       - {name: gcash,     business_type: 101}
	//       - {name: shopeepay, business_type: 102}
	type chanRow struct {
		Name         string `mapstructure:"name"`
		BusinessType int32  `mapstructure:"business_type"`
	}
	var chans []chanRow
	if err := v.UnmarshalKey("accounting.channels", &chans); err != nil {
		logger.Error("decode accounting.channels failed", zap.Error(err))
		return cli
	}
	mp := make(map[string]int32, len(chans))
	for _, c := range chans {
		if c.Name == "" || c.BusinessType <= 0 {
			logger.Warn("accounting.channels entry skipped (need name + business_type > 0)",
				zap.String("name", c.Name), zap.Int32("business_type", c.BusinessType))
			continue
		}
		mp[c.Name] = c.BusinessType
	}
	cli.SetCounterBusinessTypes(mp)
	logger.Info("accounting counter business types loaded from static config",
		zap.Int("count", len(mp)),
		zap.Any("map", mp))

	// 预热 fleet 平台账户缓存：100 个 slot × channel 数。hydrates acctCache 后，
	// 运行时 DoubleEntryBooking 的 counter accountNo 查询全是本地 map 命中，
	// 第一次记账就不用打 GetAccount RPC。预热失败只 warn，不阻断启动。
	// 走 gRPC（GetAccount），不依赖 admin HTTP。
	if len(mp) > 0 {
		currency := v.GetString("accounting.currency")
		if currency == "" {
			currency = "PHP"
		}
		warmCtx, warmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer warmCancel()
		if err := cli.PrewarmFleetAccounts(warmCtx, mp, currency); err != nil {
			logger.Warn("accounting fleet account cache prewarm had errors (hot path will fall back to GetAccount on miss)",
				zap.Error(err))
		} else {
			logger.Info("accounting fleet account cache prewarmed",
				zap.Int("channels", len(mp)))
		}
	}
	return cli
}

// wireInlineAccountingDelivery 把 accounting client + worker 注入给 AccountingOutboxService，
// 使得 Confirm / Webhook 同步分支在 outbox Insert 后：
//   1. 立即尝试 inline 投递（best-effort，见 accounting_outbox_service.tryInlineDeliver）
//   2. 若 inline 未启用或失败，调 worker.Wake() 让 worker 立刻跑一轮 Tick，
//      投递延迟从 "最坏 5s poll" 压到毫秒级
// client 为 nil（endpoint 未配置）时仍然 wire waker，使 worker 的 5s poll 也被 nudge，
// 只是不走 inline 分支；兼容 feature flag off 场景。
func wireInlineAccountingDelivery(svc service.AccountingOutboxService, client service.AccountingClient, worker *service.AccountingOutboxWorker, logger *zap.Logger) {
	type inlineConfigurable interface {
		SetInlineClient(service.AccountingClient)
		SetWaker(service.OutboxWaker)
	}
	cfg, ok := svc.(inlineConfigurable)
	if !ok {
		return
	}
	if client != nil {
		cfg.SetInlineClient(client)
		logger.Info("accounting outbox: inline delivery enabled")
	}
	if worker != nil {
		cfg.SetWaker(worker)
		logger.Info("accounting outbox: worker wake signal wired")
	}
}

// newAccountingOutboxWorker 轮询 outbox 投递到 accounting-system。
func newAccountingOutboxWorker(outbox repo.AccountingOutboxRepository, client service.AccountingClient, v *viper.Viper, logger *zap.Logger) *service.AccountingOutboxWorker {
	cfg := service.AccountingOutboxWorkerConfig{
		BatchSize:    v.GetInt("accounting_outbox.batch_size"),
		PollInterval: v.GetDuration("accounting_outbox.poll_interval"),
		MaxAttempts:  v.GetInt("accounting_outbox.max_attempts"),
		BaseBackoff:  v.GetDuration("accounting_outbox.base_backoff"),
		MaxBackoff:   v.GetDuration("accounting_outbox.max_backoff"),
	}
	return service.NewAccountingOutboxWorker(outbox, client, logger.Named("accounting-outbox-worker"), cfg)
}

func startAccountingOutboxWorker(lc fx.Lifecycle, w *service.AccountingOutboxWorker, t *LeaderToolkit) {
	if w == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			t.RunWorker(ctx, "accounting-outbox", w.Run)
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

func newAccountingOutboxArchiver(outbox repo.AccountingOutboxRepository, v *viper.Viper, logger *zap.Logger) *service.AccountingOutboxArchiver {
	cfg := service.AccountingOutboxArchiverConfig{
		Retain:       v.GetDuration("accounting_outbox.archive_retain"),
		PollInterval: v.GetDuration("accounting_outbox.archive_interval"),
		Batch:        v.GetInt("accounting_outbox.archive_batch"),
	}
	return service.NewAccountingOutboxArchiver(outbox, logger.Named("accounting-outbox-archiver"), cfg)
}

func startAccountingOutboxArchiver(lc fx.Lifecycle, w *service.AccountingOutboxArchiver, t *LeaderToolkit) {
	if w == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			t.RunWorker(ctx, "accounting-outbox-archiver", w.Run)
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

// ─── server / workers ────────────────────────────────────────────────────────

func newServer(
	pi service.PaymentIntentService,
	ch service.ChargeService,
	rf service.RefundService,
	act service.PayActionService,
	wh service.WebhookService,
	ledger service.LedgerService,
	dispute service.DisputeService,
	auditR repo.AdminAuditRepository,
	dbMgr *repo.Manager,
	whDisp *webhook.Dispatcher,
	v *viper.Viper,
	logger *zap.Logger,
) (*server.Server, error) {
	tokens := map[string]string{}
	for _, t := range v.GetStringSlice("auth.tokens") {
		tokens[t] = "ok"
	}
	allowUnauth := v.GetBool("auth.allow_unauthenticated")
	return server.NewServer(server.Deps{
		PISvc:                    pi,
		ChargeSvc:                ch,
		RefundSvc:                rf,
		ActionSvc:                act,
		WebhookSvc:               wh,
		LedgerSvc:                ledger,
		DisputeSvc:               dispute,
		AuditRepo:                auditR,
		DBMgr:                    dbMgr,
		WebhookDisp:              whDisp,
		AuthTokens:               tokens,
		AuthAllowUnauthenticated: allowUnauth,
		RateLimitRPS:             v.GetFloat64("rate_limit.rps"),
		RateBurst:                v.GetInt("rate_limit.burst"),
		Logger:                   logger,
	})
}

func startMetricsHTTP(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger, mgr *repo.Manager) {
	addr := v.GetString("metrics.addr")
	if addr == "" {
		addr = ":9090"
	}
	probes := []metrics.HealthProbe{
		func() (string, bool, string) {
			db, err := mgr.GetMeta().DB()
			if err != nil {
				return "meta_db", false, err.Error()
			}
			if err := db.Ping(); err != nil {
				return "meta_db", false, err.Error()
			}
			return "meta_db", true, ""
		},
		func() (string, bool, string) {
			for i := 0; i < mgr.ShardCount(); i++ {
				sh, err := mgr.GetShard(i)
				if err != nil {
					return "shard_" + iToA(i), false, err.Error()
				}
				d, err := sh.DB()
				if err != nil {
					return "shard_" + iToA(i), false, err.Error()
				}
				if err := d.Ping(); err != nil {
					return "shard_" + iToA(i), false, err.Error()
				}
			}
			return "shards", true, ""
		},
	}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			metrics.StartServer(addr, logger, probes...)
			return nil
		},
	})
}

func iToA(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func newExpireWorker(svc service.PaymentIntentService, v *viper.Viper, logger *zap.Logger) *service.ExpireWorker {
	interval := v.GetDuration("expire_worker.interval")
	limit := v.GetInt("expire_worker.limit")
	return service.NewExpireWorker(svc, interval, limit, logger)
}

func newRefundRetryWorker(
	rf repo.RefundRepository,
	rfSvc service.RefundService,
	reg channel.PaymentChannelRegistry,
	v *viper.Viper,
	logger *zap.Logger,
) *service.RefundRetryWorker {
	interval := v.GetDuration("refund_retry_worker.interval")
	limit := v.GetInt("refund_retry_worker.limit")
	chName := v.GetString("channel.default_name")
	if chName == "" {
		chName = "payment-core"
	}
	return service.NewRefundRetryWorker(rf, rfSvc, reg, chName, interval, limit, logger)
}

func startGRPC(lc fx.Lifecycle, s *server.Server, v *viper.Viper, logger *zap.Logger) {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9091
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
		OnStop: func(stopCtx context.Context) error {
			// gRPC graceful：拒新连接 + 等 in-flight RPC 完成，超时 25s 强 Stop
			gsCtx, gsCancel := context.WithTimeout(stopCtx, 25*time.Second)
			defer gsCancel()
			err := s.Stop(gsCtx)
			cancel() // 兜底
			return err
		},
	})
}

func startExpireWorker(lc fx.Lifecycle, w *service.ExpireWorker, t *LeaderToolkit) {
	if w == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			t.RunWorker(ctx, "expire", w.Start)
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

func startRefundRetryWorker(lc fx.Lifecycle, w *service.RefundRetryWorker, t *LeaderToolkit) {
	if w == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			t.RunWorker(ctx, "refund-retry", w.Start)
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

func newChargeExpireWorker(
	chargeRepo repo.ChargeRepository,
	piSvc service.PaymentIntentService,
	reconcile *service.RefundReconcileService,
	registry channel.PaymentChannelRegistry,
	v *viper.Viper,
	logger *zap.Logger,
) *service.ChargeExpireWorker {
	return service.NewChargeExpireWorker(
		chargeRepo, piSvc,
		reconcile, registry, v.GetString("reconcile_worker.channel_name"),
		v.GetDuration("charge_expire_worker.interval"),
		v.GetInt("charge_expire_worker.limit"),
		logger.Named("charge-expire"),
	)
}

func startChargeExpireWorker(lc fx.Lifecycle, w *service.ChargeExpireWorker, t *LeaderToolkit) {
	if w == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			t.RunWorker(ctx, "charge-expire", w.Start)
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

func newReconcileWorker(
	chargeRepo repo.ChargeRepository,
	piSvc service.PaymentIntentService,
	reconcileSvc *service.RefundReconcileService,
	reg channel.PaymentChannelRegistry,
	v *viper.Viper,
	logger *zap.Logger,
) *service.ReconcileWorker {
	return service.NewReconcileWorker(
		chargeRepo, piSvc, reconcileSvc, reg,
		v.GetString("reconcile_worker.channel_name"),
		v.GetDuration("reconcile_worker.interval"),
		v.GetDuration("reconcile_worker.stale_after"),
		v.GetInt("reconcile_worker.limit"),
		logger.Named("reconcile"),
	)
}

func startReconcileWorker(lc fx.Lifecycle, w *service.ReconcileWorker, t *LeaderToolkit) {
	if w == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			t.RunWorker(ctx, "reconcile", w.Start)
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

// 日切对账由专门的 reconplatform 系统统一处理（直接读 order-core / accounting-system
// 的 DB 快照），order-core 不再内置 ReconciliationWorker。原本的 admin HTTP
// FleetTotalBalance 调用一并废弃 — 系统内禁止 HTTP 互调（admin 控制台 / 配置推送
// 除外）。如需在 order-core 重新加日切，请改走 accounting-system 的 gRPC API
// （GetAccount + 余额聚合），不要再引入 admin HTTP。
