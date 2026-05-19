// split-payment server — Money Flow Graph 编排服务入口 (SP-AC-7 pure gRPC).
//
// FX-4 stage-1 wrap: 顶层 fx.New(Module).Run(), 业务装配仍 inline 在 wireAll.
// 后续 stage 把 Engine / Workers / gRPCServer 逐步抽 Provider 后 wireAll 会变薄.
//
// 起:
//
//	./split-payment    (env / yaml 加载, fx 接管 lifecycle)
//
// 暴露:
//   - gRPC :9098 — split_payment.v1.AdminService (Graph CRUD / DryRun)
//   - admin HTTP :9099 — /healthz + /metrics + pprof
//
// 后台 (fx.Lifecycle 管理 ctx + Stop):
//   - Kafka 订阅业务事件 → workflow.Engine.Handle → translator → accounting (gRPC)
//   - Payout cron / refund subscriber / saga recovery 等内部 worker
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"reconcile-system/packages/split-payment/internal/clients"
	"reconcile-system/packages/split-payment/internal/config"
	"reconcile-system/packages/split-payment/internal/domain"
	"reconcile-system/packages/split-payment/internal/grpcsvc"
	"reconcile-system/packages/split-payment/internal/observability"
	"reconcile-system/packages/split-payment/internal/repo"
	"reconcile-system/packages/split-payment/internal/workflow"

	_ "github.com/go-sql-driver/mysql"             // MF-1: mysql driver
	kitexserver "github.com/cloudwego/kitex/server" // KX-11: Kitex server
	"github.com/twmb/franz-go/pkg/kgo"              // SP-11 refund kafka subscriber
	"go.uber.org/fx"
	"go.uber.org/zap"

	adminservice "reconcile-system/packages/split-payment/kitex_gen/split_payment/v1/adminservice"
)

// main fx.New(Module).Run() — Module 见 providers.go.
// wireAll 是 fx.Invoke 入口, 负责把 inject 的 cfg/log/db/conn/grpcCli 接到现有业务装配上.
func main() {
	fx.New(
		Module,
		fx.Invoke(wireAll),
	).Run()
}

// wireAll 装配业务逻辑 — 接收 fx Providers 给的依赖, 把原 main() body 搬进来 (内部
// 仍是过程式; FX-3 stage 后续逐步抽 Provider 后这个函数会变薄).
//
// 跟 recon-admin "stage 1 wrap" 同款手法, 保留过程式细节, 顶层走 fx.New.
func wireAll(
	lc fx.Lifecycle,
	cfg *config.Config,
	log *zap.Logger,
	logLevel zap.AtomicLevel,
	db *sql.DB,
	accountingGRPCCli *clients.AccountingGRPCClient,
) error {
	// SP-AC-7 P10: OTel trace context propagation (W3C traceparent). 当前用 noop tracer.
	shutdownTracer := observability.InitTracer("split-payment")

	// 主 ctx — fx.Lifecycle 管理: OnStop 时 cancel 让所有 goroutine 优雅退出.
	ctx, cancel := context.WithCancel(context.Background())

	accAddr := cfg.Accounting.GRPCAddr

	// 2. repos — MF-1: 优先 MySQL (SPLIT_PAYMENT_DSN 配了就走), fallback memory.
	//
	//   SPLIT_PAYMENT_DSN 例:
	//     "split_user:pwd@tcp(shared-meta:3306)/split_payment?parseTime=true&charset=utf8mb4"
	//
	// memory mode: 单进程,重启丢全部 graph (适合 dev / 单测).
	// mysql  mode: 持久 + 多副本共享.
	// db / pool 已由 newDBFx Provider open + size + ping; 这里只消费 db (nil = memory 模式).
	var (
		graphRepo workflow.GraphRepo
		runRepo   workflow.RunRepo
		// SP-6 typed repos 注到 engine 用 (nil = 跑老路径不持 typed 对象)
		engAccRepo workflow.AccountRepo
		engTrRepo  workflow.TransferRepo
		engFeeRepo workflow.AppFeeRepo
		engPoRepo  workflow.PayoutRepo
		// SP-9 refund handler 用的扩展 repo
		refundTrRepo  workflow.TransferReverseRepo
		refundFeeRepo workflow.AppFeeRefundRepo
		refundRvRepo  workflow.ReversalExtRepo
		// SP-10 PayoutCron 用
		cronAccRepo workflow.AccountListerRepo
		cronPoRepo  workflow.PayoutInserterRepo
	)
	if db != nil {
		// Schema 由 packages/split-payment/database/metadb/init/*.sql 在 MySQL 容器
		// 启动时自动灌入 (跟 card-center / order-core 一致); 应用层不再做 DDL.
		graphRepo = repo.NewMySQLGraphRepo(db)
		runRepo = repo.NewMySQLRunRepo(db)

		accRepo := repo.NewAccountRepo(db)
		trRepo := repo.NewTransferRepo(db)
		feeRepo := repo.NewAppFeeRepo(db)
		poRepo := repo.NewPayoutRepo(db)
		rvRepo := repo.NewReversalRepo(db)
		// SP-AC-7: Stripe-style 外部 HTTP API (StripeAPIServer) 已下线 —
		// split-payment 转为纯 gRPC 内部服务. typed repos 仍然喂给 engine 做持久化.
		_ = rvRepo // reserved for future gRPC Reversal service
		// SP-6: 注到 engine 走 capability gate + typed 对象持久化
		engAccRepo = accRepo
		engTrRepo = trRepo
		engFeeRepo = feeRepo
		engPoRepo = poRepo
		// SP-9: refund handler 用
		refundTrRepo = trRepo
		refundFeeRepo = feeRepo
		refundRvRepo = rvRepo
		// SP-10: payout cron 用
		cronAccRepo = accRepo
		cronPoRepo = poRepo
		log.Info("repos: mysql + stripe entities ready", zap.String("dsn_host", maskDSN(cfg.Database.DSN)))
	} else {
		mg := repo.NewMemoryGraphRepo()
		mr := repo.NewMemoryRunRepo()
		// 启动期 seed 示例 graphs (从 examples/ 目录读) — 仅 memory 模式;
		// MySQL 模式由 admin UI / migrate 工具填.
		seedExampleGraphs(mg, cfg.Seed.GraphDir, log)
		graphRepo = mg
		runRepo = mr
		log.Info("repos: memory (set database.dsn to use MySQL)")
	}

	// SP-8: optional Kafka event publisher.
	// kafka.brokers 配了就连 Kafka, 没配走 NoopEventPublisher (dev / 单节点).
	var eventPub workflow.EventPublisher = workflow.NoopEventPublisher{}
	if brokers := cfg.Kafka.Brokers; len(brokers) > 0 {
		evCfg := workflow.DefaultKafkaEventConfig(brokers)
		if cfg.Kafka.EventTopic != "" {
			evCfg.Topic = cfg.Kafka.EventTopic
		}
		evCfg.LiveMode = cfg.Env != "dev"
		kp, kerr := workflow.NewKafkaEventPublisher(evCfg, log)
		if kerr != nil {
			log.Warn("kafka event publisher init failed (using noop)",
				zap.Strings("brokers", brokers),
				zap.String("topic", evCfg.Topic),
				zap.Error(kerr))
		} else {
			eventPub = kp
			defer kp.Close()
			log.Info("event publisher: kafka",
				zap.String("topic", evCfg.Topic),
				zap.Bool("livemode", evCfg.LiveMode))
		}
	} else {
		log.Info("event publisher: noop (set kafka.brokers to enable)")
	}

	// 4. workflow engine — SP-6 + SP-3A
	engine := &workflow.Engine{
		GraphRepo: graphRepo,
		RunRepo:   runRepo,
		// SP-AC-7: 老 Accounting *clients.AccountingClient 字段删除, 业务路径走 AccountingMeta.
		Audit:        logAudit{log: log},
		Log:          log,
		AccountRepo:  engAccRepo,
		TransferRepo: engTrRepo,
		AppFeeRepo:   engFeeRepo,
		PayoutRepo:   engPoRepo,
		Events:       eventPub,

		// SP-9 refund handler 用的 repo 引用
		TransferReverseRepo: refundTrRepo,
		AppFeeRefundRepo:    refundFeeRepo,
		ReversalInsertRepo:  refundRvRepo,
		// SP-AC-7 R1: refund 写两表用 sql.Tx 包. DSN 配了才有 db, memory 模式 ReversalApply=nil 退化两步.
	}
	if db != nil {
		engine.ReversalApply = &reversalApplyAdapter{db: db}

		// SP-AC-7 L5: Event 发布走 outbox + 后台 drain worker.
		// 业务路径写 outbox (跟主数据可同 tx, 至少一次保证); worker 异步推 Kafka.
		// Kafka 没配的话退化为只入 outbox 不推送 (后续配上 Kafka 自动 catch-up).
		// 表 event_outbox 由 metadb/init/5_outbox.sql 启动期已建.
		if eventPub != nil {
			evOutbox := &repo.EventOutbox{DB: db}
			engine.Events = &workflow.OutboxEventPublisher{
				Outbox: &eventOutboxEnqAdapter{ob: evOutbox},
				Log:    log,
			}
			if kp, ok := eventPub.(*workflow.KafkaEventPublisher); ok {
				outboxWk := &workflow.EventOutboxWorker{
					Cfg:    workflow.DefaultEventOutboxConfig(),
					Outbox: &eventOutboxClaimAdapter{ob: evOutbox},
					Sender: &kafkaEventSenderAdapter{pub: kp},
					Log:    log,
				}
				go outboxWk.Run(ctx)
				log.Info("event outbox worker started; engine.Events now goes through transactional outbox")
			} else {
				log.Warn("eventPub is not KafkaEventPublisher (noop?); outbox will accumulate without drain")
			}
		}

		// SP-AC-7 R5: Reversal 失败 outbox 重试.
		// 表 reversal_retry_outbox 由 metadb/init/5_outbox.sql 启动期已建.
		revOutbox := &repo.ReversalOutbox{DB: db, Log: log}
		engine.ReversalRetry = &reversalOutboxAdapter{ob: revOutbox}
		retryWk := &workflow.ReversalRetryWorker{
			Cfg:     workflow.DefaultReversalRetryConfig(),
			Outbox:  &reversalOutboxClaimAdapter{ob: revOutbox},
			Applier: engine.ReversalApply,
			Log:     log,
		}
		go retryWk.Run(ctx)
		log.Info("reversal retry worker started")
	}

	// SP-3A: 接持久化 saga (MySQL 模式 + saga.enabled 才启).
	// 默认 dev 走老路径方便调试,生产强烈建议开 saga (失败可恢复 + 自动 compensate).
	if cfg.Saga.Enabled && engTrRepo != nil {
		sagaDeps := workflow.StepDeps{
			// SP-AC-7: Accounting 字段删除 (saga step 当前实现只翻状态)
			TransferRepo:    engTrRepo,
			AppFeeRepo:      engFeeRepo,
			PayoutRepo:      engPoRepo,
			TransferReverse: refundTrRepo,
			ReversalInsert:  refundRvRepo,
			Events:          eventPub,
		}
		var sagaStore workflow.SagaStore
		if db != nil {
			// 复用上面已 Open 的 *sql.DB, 别再 open 第二条连接 (避免 pool 翻倍 + 漏关).
			sagaStore = repo.NewMySQLSagaStore(db)
		} else {
			sagaStore = workflow.NewMemorySagaStore()
		}
		engine.Saga = &workflow.SagaCoordinator{
			Store:   sagaStore,
			Factory: workflow.NewDefaultStepFactory(sagaDeps),
			Logger:  zapSagaLogger{log: log},
		}
		engine.SagaDeps = &sagaDeps
		log.Info("saga mode: enabled (saga.enabled=true)")

		// 启动期 resume 未完成的 saga (进程崩溃恢复)
		go func() {
			n, err := engine.Saga.ResumeUnfinished(context.Background(), 100)
			if err != nil {
				log.Warn("saga resume failed", zap.Error(err))
				return
			}
			if n > 0 {
				log.Info("saga resumed unfinished instances", zap.Int("count", n))
			}
		}()
	} else {
		log.Info("saga mode: disabled (set saga.enabled=true / SPLIT_PAYMENT_SAGA=1 to enable persistent saga)")
	}

	// SP-3B: Risk + AML gate (optional, dev 默认走 AlwaysAllow 占位).
	// 生产由 main.go 注入真实 RiskClient (gRPC 调 risk-manage / aml-screening 服务).
	{
		riskCfg := workflow.DefaultRiskGateConfig()
		if cfg.Risk.AMLThresholdMinor > 0 {
			riskCfg.AMLThresholdMinor = cfg.Risk.AMLThresholdMinor
		}
		if cfg.Risk.FailOpen {
			riskCfg.FailSafeReject = false
		}
		// SP-FIN-1: 真实 HTTP client (yaml 配了 URL 走真实, 否则 AlwaysAllow 占位).
		var riskCli workflow.RiskClient = workflow.AlwaysAllowRisk{}
		var amlCli workflow.AMLClient = workflow.AlwaysAllowAML{}
		if u := cfg.Risk.HTTPURL; u != "" {
			riskCli = workflow.NewHTTPRiskClient(u, cfg.Risk.AuthToken)
			log.Info("risk client: http", zap.String("url", u))
		}
		if u := cfg.Risk.AMLHTTPURL; u != "" {
			amlCli = workflow.NewHTTPAMLClient(u, cfg.Risk.AMLAuthToken)
			log.Info("aml client: http", zap.String("url", u))
		}
		engine.RiskGate = &workflow.RiskGate{
			Cfg:  riskCfg,
			Risk: riskCli,
			AML:  amlCli,
			Log:  log,
		}
		log.Info("risk gate: enabled (using AlwaysAllow placeholder; wire real gRPC clients in main.go)",
			zap.Int64("aml_threshold_cents", riskCfg.AMLThresholdMinor),
			zap.Bool("fail_safe_reject", riskCfg.FailSafeReject))
	}

	// SP-AC-7: accounting gRPC TransactionService 客户端 (multi-leg + 元数据).
	// 复用已有的 accounting-system gRPC conn (跟 AccountingClient 同一条连接).
	// HTTP 不再用于业务调用 — 只剩 ops/admin UI.
	if conn != nil {
		// accountingGRPCCli 由 fx Provider 注入 (newAccountingGRPCClientFx), Kitex client.
		if accountingGRPCCli != nil {
			engine.AccountingMeta = accountingGRPCAdapter{cli: accountingGRPCCli}
		}
		log.Info("accounting meta client: gRPC (TransactionService)")
	} else {
		log.Info("accounting meta client: disabled (accounting gRPC conn nil)")
	}

	// SP-3C + SP-FIN-1: FX client (fx.http_url 配了走真实, 否则 static 占位).
	if u := cfg.FX.HTTPURL; u != "" {
		engine.FX = workflow.NewHTTPFXClient(u, cfg.FX.AuthToken)
		log.Info("fx client: http", zap.String("url", u))
	} else {
		engine.FX = workflow.StaticFXClient{
			Rates: map[string]float64{
				"USD-EUR": 0.92, "EUR-USD": 1.09,
				"USD-CNY": 7.20, "CNY-USD": 0.139,
				"USD-GBP": 0.79, "GBP-USD": 1.27,
				"USD-JPY": 150.0, "JPY-USD": 0.0067,
				"EUR-CNY": 7.83, "CNY-EUR": 0.128,
			},
		}
		log.Info("fx client: static rates (set FX_HTTP_URL to use real fx-service)")
	}

	// 5. SP-AC-7: split-payment 转为纯 gRPC 内部服务.
	//    旧 HTTP server (adminhttp.Server + StripeAPIServer + ReviewsServer + Stripe compat)
	//    全部下线. admin-web BFF 现在通过 gRPC AdminService 调本服务.
	//
	//    graphRepo 是 workflow.GraphRepo 类型, 我们的 grpcsvc.GraphRepo 接口形态一致
	//    (Save/GetByKey/List), MemoryGraphRepo / MySQLGraphRepo 都满足两边.
	sgGraphs, _ := graphRepo.(grpcsvc.GraphRepo)
	if sgGraphs == nil {
		log.Fatal("graphRepo does not satisfy grpcsvc.GraphRepo (missing Save/GetByKey/List)")
	}
	// runRepo / engine 仍然在用 (kafka subscriber / saga / payout cron 都引用), 但
	// 不再通过 HTTP 暴露给前端. 4-eyes approval / runs/search 等查询如需要, 后续
	// 在 grpcsvc.AdminService 里加 RPC 方法.
	_ = runRepo // 当前 gRPC AdminService 还没加 ListRuns / GetRun / ApproveRun 等方法

	log.Info("split-payment internal gRPC service starting",
		zap.Int("grpc_port", cfg.Server.GRPCPort),
		zap.String("accounting", accAddr))

	// SP-AC-7: gRPC AdminService — admin-web BFF 通过此端口调.
	// engine.AccountingMeta 是 workflow.AccountingMetaCaller 接口实例, 把它适配成
	// grpcsvc.AccountingMetaCaller (同形态, 不同包) 供 TriggerEvent 真落账用.
	var grpcAcct grpcsvc.AccountingMetaCaller
	if engine.AccountingMeta != nil {
		grpcAcct = grpcsvcAcctAdapter{inner: engine.AccountingMeta}
	}
	go runAdminGRPCServer(ctx, cfg, log, sgGraphs, grpcAcct)

	// SP-AC-7 L1+P9: split-payment admin HTTP — /healthz + /readiness + /metrics.
	// 跟 gRPC :9098 错开 (默认 :9099), admin.http_port yaml 可覆盖.
	adminSrv := observability.NewAdminServer(fmt.Sprintf("%d", cfg.Admin.HTTPPort), log, logLevel)
	// readiness 探针: MySQL ping (DSN 配了才探).
	if db != nil {
		adminSrv.AddReadyCheck("mysql", func(c context.Context) error {
			return db.PingContext(c)
		})
	}
	// readiness 探针: accounting Kitex client 不暴露 channel state, 简化为 nil-check.
	// 真要做 health check 走 accountingGRPCCli 的 ListAccountTypes 一次 ping.
	adminSrv.AddReadyCheck("accounting_grpc", func(c context.Context) error {
		if accountingGRPCCli == nil {
			return fmt.Errorf("accounting client nil")
		}
		return nil
	})
	go func() {
		if err := adminSrv.Run(ctx); err != nil {
			log.Error("admin http server exited", zap.Error(err))
		}
	}()

	// SP-AC-7 O2: DB stats + outbox depth gauge collectors.
	if db != nil {
		observability.StartDBStatsCollector(ctx, db, 30*time.Second, log)
		scrapers := map[string]observability.OutboxScraper{
			"event_outbox":          eventOutboxScraper(db),
			"reversal_retry_outbox": reversalOutboxScraper(db),
		}
		observability.StartOutboxMetricsCollector(ctx, scrapers, 30*time.Second, log)
	}

	// SP-FIN-2: PayoutDispatchWorker — pending → in_transit 状态机.
	// 调 clearing-settlement 服务 (env CLEARING_HTTP_URL 配了走真实, 否则 Noop).
	if cronPoRepo != nil {
		clear := workflow.ClearingClient(workflow.NoopClearingClient{Log: log})
		// TODO: 真实 ClearingClient → 新增 internal/clients/clearing.go HTTP impl.
		// 占位 Noop 行为: 标 in_transit 假装已发送.
		_ = cfg.Clearing.HTTPURL // reserved for future HTTP impl
		dispatchPayoutRepo, _ := cronPoRepo.(workflow.PendingPayoutsRepo)
		if dispatchPayoutRepo != nil {
			disp := &workflow.PayoutDispatchWorker{
				Cfg:      workflow.DefaultPayoutDispatchConfig(),
				Payouts:  dispatchPayoutRepo,
				Clearing: clear,
				Events:   eventPub,
				Log:      log,
			}
			go disp.Run(ctx)
		}
	}

	// SP-10: PayoutCron 后台 goroutine, 周期扫账户余额 → 自动创建 Payout.
	// 仅 MySQL 模式启用 (cronAccRepo / cronPoRepo nil → 跳过).
	if cronAccRepo != nil && cronPoRepo != nil {
		cron := &workflow.PayoutCron{
			Cfg:      workflow.DefaultPayoutCronConfig(),
			Accounts: cronAccRepo,
			Payouts:  cronPoRepo,
			Balances: workflow.StubBalanceQuerier{}, // Phase 3 接 accounting gRPC
			Events:   eventPub,
			Log:      log,
		}
		// LiveMode 由 yaml 控制, 默认 dev=false 只 log 不真创建
		if cfg.Workers.PayoutCron.LiveMode {
			cron.Cfg.LiveMode = true
		}
		if cfg.Workers.PayoutCron.Interval > 0 {
			cron.Cfg.Interval = cfg.Workers.PayoutCron.Interval
		}
		// SP-AC-7 X3: 多副本 lease, 同一时刻只有一个副本跑 cron.
		// memory 模式 (db nil) 退化为无锁直跑 (单副本 OK).
		// 表 cron_lease 由 metadb/init/6_cron_lease.sql 启动期已建.
		if db != nil {
			lease := &workflow.CronLease{
				DB:     db,
				Name:   "payout_cron",
				Holder: workflow.DefaultHolder(),
				TTL:    30 * time.Second,
				Log:    log,
			}
			go lease.RunWithLease(ctx, cron.Run)
		} else {
			go cron.Run(ctx)
		}

		// SP-AC-7 PROD2: Daily trial balance reconciliation worker (lease protected).
		recWk := &workflow.ReconcileWorker{
			Cfg:  workflow.DefaultReconcileConfig(),
			DB:   db,
			Sink: &workflow.LogAlertSink{Log: log},
			Log:  log,
		}
		recLease := &workflow.CronLease{
			DB: db, Name: "reconcile_worker",
			Holder: workflow.DefaultHolder(), TTL: 5 * time.Minute, Log: log,
		}
		go recLease.RunWithLease(ctx, recWk.Run)

		// SP-AC-7 PH3-7: HoldUnstickWorker 真实接线.
		// - Plans: MySQLRunRepo 现已实现 ListExpiredHolds + MarkHoldReleased (memory 模式
		//   退化到 NoopPendingHoldsRepo, 不发钱).
		// - Releaser: MetaHoldReleaser 走 AccountingMetaCaller.CreateTransaction
		//   (跟主链路同一条 gRPC, 复用 circuit breaker + token auth).
		var holdPlans workflow.PendingHoldsRepo = workflow.NoopPendingHoldsRepo{}
		if mysqlRunRepo, ok := runRepo.(workflow.PendingHoldsRepo); ok {
			holdPlans = mysqlRunRepo
			log.Info("hold unstick worker: using MySQLRunRepo as PendingHoldsRepo")
		} else {
			log.Warn("hold unstick worker: memory mode — using NoopPendingHoldsRepo")
		}
		var holdReleaser workflow.HoldReleaser
		if engine.AccountingMeta != nil {
			holdReleaser = workflow.NewMetaHoldReleaser(engine.AccountingMeta, log)
		}
		holdLease := &workflow.CronLease{
			DB: db, Name: "hold_unstick_worker",
			Holder: workflow.DefaultHolder(), TTL: 30 * time.Second, Log: log,
		}
		holdWorker := &workflow.HoldUnstickWorker{
			Cfg:      workflow.DefaultHoldUnstickConfig(),
			Plans:    holdPlans,
			Releaser: holdReleaser,
			Events:   eventPub,
			Log:      log,
		}
		if db != nil {
			go holdLease.RunWithLease(ctx, holdWorker.Run)
			log.Info("hold unstick worker started (lease-protected)")
		} else {
			go holdWorker.Run(ctx)
			log.Info("hold unstick worker started (no lease — memory mode)")
		}
	}

	// SP-9: Kafka subscriber 订 refund-engine 的 refund.completed 事件 → engine.HandleRefund.
	// 复用上面的 brokers. kafka.refund.topic yaml 可改默认 topic.
	if len(cfg.Kafka.Brokers) > 0 && refundTrRepo != nil && refundRvRepo != nil {
		go runRefundSubscriber(ctx, cfg, engine, log,
			refundTrRepo, refundFeeRepo, refundRvRepo)
	} else {
		log.Info("refund subscriber: disabled (set kafka.brokers + MySQL mode to enable)")
	}

	// 事件驱动入口:
	//
	//  - 生产: Kafka subscriber 订阅 payment.events,每条 BusinessEvent 调
	//    engine.Handle(ctx, ev) 推进分账流。kafka 消费由 payment-util/kafkamq 提供。
	//  - dev / demo: HTTP /api/moneyflow/trigger 手动触发,见 internal/handler/trigger.go。

	// fx.Lifecycle 收尾: SIGTERM 时 fx 触发 OnStop, cancel 让所有 goroutine ctx.Done().
	// gRPC server / Kafka subscriber / cron worker 都靠 ctx 退. shutdownTracer 也走这条.
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			log.Info("split-payment shutting down")
			cancel()
			// 给 in-flight RPC + Kafka commit 500ms 收尾
			time.Sleep(500 * time.Millisecond)
			shutdownTracer(context.Background())
			return nil
		},
	})
	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────

// parseLogLevel — SP-AC-7 O4: env 字符串 → zap level.
func parseLogLevel(s string) zap.AtomicLevel {
	switch strings.ToLower(s) {
	case "debug":
		return zap.NewAtomicLevelAt(zap.DebugLevel)
	case "warn", "warning":
		return zap.NewAtomicLevelAt(zap.WarnLevel)
	case "error":
		return zap.NewAtomicLevelAt(zap.ErrorLevel)
	default:
		return zap.NewAtomicLevelAt(zap.InfoLevel)
	}
}

// envOr / envInt 已删除 — A 方案 (CFG-2): 所有可配置项走 cfg.X.Y (yaml + env override).
// 历史 env 名 (SPLIT_PAYMENT_DSN / SPLIT_GRPC_PORT 等) 由 internal/config.bindLegacyEnv
// 绑到对应 cfg key, 保持 docker-compose 平滑迁移.

func seedExampleGraphs(r *repo.MemoryGraphRepo, dir string, log *zap.Logger) {
	if dir == "" {
		dir = "./examples/moneyflow-graphs" // 跟 setDefaults 同步, double safety.
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Info("seed dir not found, skipping", zap.String("dir", dir))
		return
	}
	for _, e := range entries {
		if e.IsDir() || !endsWith(e.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			continue
		}
		var g domain.Graph
		if err := json.Unmarshal(body, &g); err != nil {
			log.Warn("parse seed graph", zap.String("file", e.Name()), zap.Error(err))
			continue
		}
		if _, err := r.Save(context.Background(), &g); err == nil {
			log.Info("seeded graph", zap.String("key", g.Key))
		}
	}
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// maskDSN 把 DSN 里的密码遮掉, 仅露 host:port 用于 log.
//
//	"user:pwd@tcp(host:3306)/db" → "user:***@host:3306/db"
//	解析失败返 "***".
func maskDSN(dsn string) string {
	// 简化:找 @tcp(...) 部分
	at := -1
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return "***"
	}
	left := dsn[:at]
	colon := -1
	for i := 0; i < len(left); i++ {
		if left[i] == ':' {
			colon = i
			break
		}
	}
	if colon < 0 {
		return left + "@" + dsn[at+1:]
	}
	return left[:colon] + ":***@" + dsn[at+1:]
}

// runAdminGRPCServer — SP-AC-7 启动 split-payment gRPC AdminService.
//
// admin-web BFF 通过这个 gRPC 端口调 Graph CRUD / DryRun / TriggerEvent.
// 监听端口由 cfg.Server.GRPCPort 控制 (默认 9098).
//
// ruleSync 来自 cfg.Accounting.HTTPURL (e.g. http://accounting-service:8888),
// SaveGraph 时把派生的 rules POST 到 /admin/transaction-rules. 空 → 关掉同步.
func runAdminGRPCServer(ctx context.Context, cfg *config.Config, log *zap.Logger, graphs grpcsvc.GraphRepo, acct grpcsvc.AccountingMetaCaller) {
	port := cfg.Server.GRPCPort
	addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Error("split Kitex resolve addr failed", zap.Int("port", port), zap.Error(err))
		return
	}
	var ruleSync grpcsvc.AccountingRuleSyncer
	var orderReset grpcsvc.AccountingOrderResetter
	if base := cfg.Accounting.HTTPURL; base != "" {
		base = strings.TrimRight(base, "/")
		// SP-AC-7 O3: 加 circuit breaker — accounting admin HTTP 连续 5 次失败 → 30s 熔断.
		cb := observability.NewCircuitBreaker("accounting_admin_http", observability.CircuitConfig{
			FailureThreshold: 5, SuccessThreshold: 2, OpenDuration: 30 * time.Second,
		})
		ruleSync = &cbRuleSyncer{inner: &httpRuleSyncer{baseURL: base, log: log}, cb: cb}
		orderReset = &cbOrderResetter{inner: &httpOrderResetter{baseURL: base, log: log}, cb: cb}
		log.Info("split-payment: accounting admin HTTP wired",
			zap.String("accounting_http", base),
			zap.String("for", "SaveGraph saga + TriggerEvent retry"))
		go reconcileGraphRules(ctx, graphs, ruleSync, log)
	} else {
		log.Warn("split-payment: accounting.http_url empty, saga + retry features disabled")
	}
	// SP-AC-7 S1+S2 token auth — TODO: 接 kitexutil.AuthMW(cfg.Server.AdminToken).
	// 当前 stub: AdminToken 不验, 等 kitexutil 真接 metainfo.GetValue 后展开.
	authToken := cfg.Server.AdminToken
	if authToken != "" {
		log.Info("split-payment Kitex: admin token configured (TODO: wire kitexutil.AuthMW)")
	} else {
		log.Warn("split-payment Kitex: AUTH DISABLED — set SPLIT_PAYMENT_ADMIN_TOKEN env var in production")
	}
	// mTLS 已不需要 (内部 mesh 明文). Kitex MW 链 (Recover / AccessLog / Metrics / Auth)
	// 待 kitexutil port 完成后 server.WithMiddleware(...) 接.
	// SP-AC-7 S6 + PROD3: 资金审计 — Zap (本地 stdout) + Kafka 独立 topic (隔离权限/留存).
	// Kafka 不可达 → ChainAuditSink 会自动跳过, 退化为仅 zap.
	auditSinks := []grpcsvc.AuditSink{&grpcsvc.ZapAuditSink{Log: log.Named("audit")}}
	if brokers := cfg.Kafka.Brokers; len(brokers) > 0 {
		auditTopic := cfg.Kafka.AuditTopic
		if auditTopic == "" {
			auditTopic = "split-payment.audit"
		}
		kAudit, kErr := grpcsvc.NewKafkaAuditSink(brokers, auditTopic, log)
		if kErr != nil {
			log.Warn("kafka audit sink init failed; falling back to zap only",
				zap.Strings("brokers", brokers),
				zap.String("topic", auditTopic),
				zap.Error(kErr))
		} else {
			defer kAudit.Close()
			auditSinks = append(auditSinks, kAudit)
			log.Info("kafka audit sink wired", zap.String("topic", auditTopic))
		}
	}
	auditSink := &grpcsvc.ChainAuditSink{Sinks: auditSinks}

	// Kitex server — adminservice.NewServer 把 grpcsvc.Server (实现 grpcsvc.AdminServiceServer
	// interface) 注册到 Kitex. 老 grpc.NewServer + RegisterAdminServiceServer 替换为单行.
	impl := grpcsvc.NewServer(graphs, acct, ruleSync, orderReset, auditSink, log)
	srv := adminservice.NewServer(impl, kitexserver.WithServiceAddr(addr))

	log.Info("split-payment Kitex AdminService listening", zap.Int("port", port))
	go func() { <-ctx.Done(); _ = srv.Stop() }()
	if err := srv.Run(); err != nil {
		log.Error("split kitex serve", zap.Error(err))
	}
}

// adminTokenInterceptor 已删 — Kitex 切换后用 server.WithMiddleware + metainfo.GetValue.
// 待 kitexutil.AuthMW 接通后再加.

// reconcileGraphRules 启动期 self-heal: 扫所有 status=active 的 graph,
// 把 DeriveRulesFromGraph 派生的 rule 调一次 UpsertRules.
//
// 触发场景:
//   - 历史 graph 由 dev 脚本 / 手工 SQL 直接 INSERT 进 moneyflow_graphs 表, 绕过 SaveGraph saga
//   - SaveGraph saga 时段 accounting 不可达 (graph 落了 split-payment 但 rule 没推过去)
//   - DSN 切换 / 数据迁移后 rule 与 graph 脱节
//
// 行为:
//   - 仅 status=active 的 graph 参与 (draft / archived 跳过, 避免污染)
//   - UpsertRules 是 ON DUPLICATE KEY UPDATE 语义 — 重复调幂等, 不会破坏现有 rule
//   - 单 graph 失败不阻断后续 graph (best-effort), 只 log; 总错数 > 0 时启动后 metrics
//     里也会留下 trail
//   - 异步执行 — gRPC 上线不等它完成 (大量 graph 时同步几百次会拖慢启动)
//
// 这是兜底, 不是替代 SaveGraph saga; saga 在 SaveGraph 时是 fail-fast (abort + 回滚),
// 这里只是开机自检 + 自愈历史漂移.
//
// 用 grpcsvc.GraphRepo 而不是 workflow.GraphRepo: 调用点在 runAdminGRPCServer 函数内
// 入参类型是 grpcsvc.GraphRepo (两者都有 List(ctx, status) 方法, 此处只需 List).
func reconcileGraphRules(ctx context.Context, graphs grpcsvc.GraphRepo, sync grpcsvc.AccountingRuleSyncer, log *zap.Logger) {
	if graphs == nil || sync == nil {
		return
	}
	// 防止 ctx 已经在主流程 cancel: 用 1min 上限独立超时, 不绑主 ctx 的 cancel.
	// 这是 best-effort, 启动期不该卡主流程超过 1 分钟.
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	list, err := graphs.List(rctx, "active")
	if err != nil {
		log.Warn("startup rule reconcile: list active graphs failed", zap.Error(err))
		return
	}
	if len(list) == 0 {
		log.Info("startup rule reconcile: no active graphs")
		return
	}

	var totalRules, syncedGraphs, failedGraphs int
	for _, g := range list {
		rules := grpcsvc.DeriveRulesFromGraph(g)
		if len(rules) == 0 {
			continue
		}
		if err := sync.UpsertRules(rctx, rules); err != nil {
			log.Warn("startup rule reconcile: upsert failed",
				zap.String("graph_key", g.Key),
				zap.Int("rule_count", len(rules)),
				zap.Error(err))
			failedGraphs++
			continue
		}
		totalRules += len(rules)
		syncedGraphs++
	}
	log.Info("startup rule reconcile complete",
		zap.Int("active_graphs", len(list)),
		zap.Int("synced_graphs", syncedGraphs),
		zap.Int("failed_graphs", failedGraphs),
		zap.Int("total_rules_upserted", totalRules))
}

// httpRuleSyncer — POST {rules:[...]} 到 accounting /admin/transaction-rules.
//
// 任一 rule upsert 失败 accounting 返 BadGateway, 这里反序列化出 error + succeeded
// 转成 caller error. 全部成功 accounting 自动调用 Reload, snapshot 立即可见.
type httpRuleSyncer struct {
	baseURL string
	log     *zap.Logger
}

func (h *httpRuleSyncer) UpsertRules(ctx context.Context, rules []grpcsvc.RuleSpec) error {
	if len(rules) == 0 {
		return nil
	}
	// 用 anonymous struct 避免依赖 accounting model 包.
	type ruleDTO struct {
		ProductCode     string `json:"product_code"`
		EventCode       string `json:"event_code"`
		HashKey         string `json:"hash_key"`
		DebitSubjectID  string `json:"debit_subject_id"`
		CreditSubjectID string `json:"credit_subject_id"`
		FromDirection   string `json:"from_direction"`
		ToDirection     string `json:"to_direction"`
		TransactionType int    `json:"transaction_type"`
		BookkeepingMode string `json:"bookkeeping_mode"`
	}
	dtos := make([]ruleDTO, 0, len(rules))
	for _, r := range rules {
		dtos = append(dtos, ruleDTO{
			ProductCode:     r.ProductCode,
			EventCode:       r.EventCode,
			HashKey:         r.HashKey,
			DebitSubjectID:  r.DebitSubjectID,
			CreditSubjectID: r.CreditSubjectID,
			FromDirection:   r.FromDirection,
			ToDirection:     r.ToDirection,
			TransactionType: r.TransactionType,
			BookkeepingMode: r.BookkeepingMode,
		})
	}
	body, _ := json.Marshal(map[string]any{"rules": dtos})
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		h.baseURL+"/admin/transaction-rules", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build http req: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("accounting unreachable: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("accounting HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	if h.log != nil {
		h.log.Info("rule sync to accounting OK",
			zap.Int("count", len(rules)),
			zap.String("response", string(respBody)))
	}
	return nil
}

// DeleteRules SP-AC-7 R2: SaveGraph saga 补偿. DELETE /admin/transaction-rules + body {hash_keys}.
func (h *httpRuleSyncer) DeleteRules(ctx context.Context, hashKeys []string) error {
	if len(hashKeys) == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"hash_keys": hashKeys})
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodDelete,
		h.baseURL+"/admin/transaction-rules", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build delete req: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("accounting unreachable: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("accounting HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	if h.log != nil {
		h.log.Info("rule delete (saga compensation) OK",
			zap.Int("count", len(hashKeys)),
			zap.String("response", string(respBody)))
	}
	return nil
}

// httpOrderResetter — TriggerEvent 遇到卡 Processing 时, POST accounting
// /admin/transaction-orders/{order_no}/reset 主动解锁.
//
// 异常场景在 accounting 端处理 (action=skipped_success / phantom_fixed / unstuck / ...),
// 这里只透传 HTTP error.
type httpOrderResetter struct {
	baseURL string
	log     *zap.Logger
}

func (h *httpOrderResetter) ResetOrder(ctx context.Context, orderNo, businessNo string, force bool) error {
	if orderNo == "" {
		return fmt.Errorf("order_no required")
	}
	if businessNo == "" {
		businessNo = orderNo // accounting orderRepo route by businessNo, fallback to orderNo
	}
	body, _ := json.Marshal(map[string]any{
		"business_no": businessNo,
		"force":       force,
	})
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := h.baseURL + "/admin/transaction-orders/" + orderNo + "/reset"
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build http req: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("accounting unreachable: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("accounting HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	if h.log != nil {
		h.log.Info("order reset request OK",
			zap.String("order_no", orderNo),
			zap.Bool("force", force),
			zap.String("response", string(respBody)))
	}
	return nil
}

// reversalApplyAdapter — SP-AC-7 R1: 实现 workflow.ReversalApplier 接口, 调 repo.ApplyReversalAtomic.
type reversalApplyAdapter struct{ db *sql.DB }

func (a *reversalApplyAdapter) Apply(ctx context.Context, rv *domain.Reversal, deltaReversed int64) error {
	return repo.ApplyReversalAtomic(ctx, a.db, rv, deltaReversed)
}

// ─── SP-AC-7 O3: Circuit-breaker wrappers ────────────────────────────────────
type cbRuleSyncer struct {
	inner *httpRuleSyncer
	cb    *observability.CircuitBreaker
}

func (w *cbRuleSyncer) UpsertRules(ctx context.Context, rules []grpcsvc.RuleSpec) error {
	return w.cb.Do(ctx, func() error { return w.inner.UpsertRules(ctx, rules) })
}

func (w *cbRuleSyncer) DeleteRules(ctx context.Context, hashKeys []string) error {
	return w.cb.Do(ctx, func() error { return w.inner.DeleteRules(ctx, hashKeys) })
}

type cbOrderResetter struct {
	inner *httpOrderResetter
	cb    *observability.CircuitBreaker
}

func (w *cbOrderResetter) ResetOrder(ctx context.Context, orderNo, businessNo string, force bool) error {
	return w.cb.Do(ctx, func() error { return w.inner.ResetOrder(ctx, orderNo, businessNo, force) })
}

// ─── SP-AC-7 O2: Outbox metric scrapers ──────────────────────────────────────
// 周期性 SELECT COUNT(*) / MAX(age) 把 outbox 状态推 Prometheus gauge.
func eventOutboxScraper(db *sql.DB) observability.OutboxScraper {
	return func(ctx context.Context) (observability.OutboxStat, error) {
		var s observability.OutboxStat
		row := db.QueryRowContext(ctx, `
			SELECT
			  COUNT(IF(status='pending',1,NULL)),
			  COUNT(IF(status='dead_letter',1,NULL)),
			  COALESCE(TIMESTAMPDIFF(SECOND, MIN(CASE WHEN status='pending' THEN created_at END), NOW()), 0)
			FROM event_outbox`)
		return s, row.Scan(&s.PendingDepth, &s.DeadLetterCount, &s.OldestAgeSeconds)
	}
}

func reversalOutboxScraper(db *sql.DB) observability.OutboxScraper {
	return func(ctx context.Context) (observability.OutboxStat, error) {
		var s observability.OutboxStat
		row := db.QueryRowContext(ctx, `
			SELECT
			  COUNT(IF(status='pending',1,NULL)),
			  COUNT(IF(status='dead_letter',1,NULL)),
			  COALESCE(TIMESTAMPDIFF(SECOND, MIN(CASE WHEN status='pending' THEN created_at END), NOW()), 0)
			FROM reversal_retry_outbox`)
		return s, row.Scan(&s.PendingDepth, &s.DeadLetterCount, &s.OldestAgeSeconds)
	}
}

// ─── SP-AC-7 L5: Event outbox adapters ──────────────────────────────────────
type eventOutboxEnqAdapter struct{ ob *repo.EventOutbox }

func (a *eventOutboxEnqAdapter) Enqueue(ctx context.Context, eventType string, payloadJSON []byte) error {
	return a.ob.Enqueue(ctx, eventType, payloadJSON)
}

type eventOutboxClaimAdapter struct{ ob *repo.EventOutbox }

func (a *eventOutboxClaimAdapter) Claim(ctx context.Context, limit int) ([]workflow.EventOutboxJob, error) {
	rows, err := a.ob.Claim(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]workflow.EventOutboxJob, 0, len(rows))
	for _, r := range rows {
		out = append(out, workflow.EventOutboxJob{
			ID: r.ID, EventType: r.EventType, PayloadJSON: r.PayloadJSON,
			RetryCount: r.RetryCount, MaxRetry: r.MaxRetry,
		})
	}
	return out, nil
}

func (a *eventOutboxClaimAdapter) MarkSent(ctx context.Context, id int64) error {
	return a.ob.MarkSent(ctx, id)
}

func (a *eventOutboxClaimAdapter) MarkDeadLetter(ctx context.Context, id int64, lastErr string) error {
	return a.ob.MarkDeadLetter(ctx, id, lastErr)
}

func (a *eventOutboxClaimAdapter) UpdateError(ctx context.Context, id int64, lastErr string) error {
	return a.ob.UpdateError(ctx, id, lastErr)
}

// kafkaEventSenderAdapter 把 workflow.KafkaEventPublisher 适配成 workflow.EventSender.
type kafkaEventSenderAdapter struct{ pub *workflow.KafkaEventPublisher }

func (a *kafkaEventSenderAdapter) Send(ctx context.Context, eventType string, payload []byte) error {
	return a.pub.SendRaw(ctx, eventType, payload)
}

// reversalOutboxAdapter — SP-AC-7 R5: 实现 workflow.ReversalRetryEnqueuer 接口.
type reversalOutboxAdapter struct{ ob *repo.ReversalOutbox }

func (a *reversalOutboxAdapter) Enqueue(ctx context.Context, reversalID, transferID string, deltaMinor int64, lastErr error) error {
	return a.ob.Enqueue(ctx, reversalID, transferID, deltaMinor, lastErr)
}

// reversalOutboxClaimAdapter — workflow.ReversalOutboxClaimer 接口适配, 把 repo.ReversalOutboxRow → workflow.ReversalOutboxJob.
type reversalOutboxClaimAdapter struct{ ob *repo.ReversalOutbox }

func (a *reversalOutboxClaimAdapter) Claim(ctx context.Context, limit int) ([]workflow.ReversalOutboxJob, error) {
	rows, err := a.ob.Claim(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]workflow.ReversalOutboxJob, 0, len(rows))
	for _, r := range rows {
		out = append(out, workflow.ReversalOutboxJob{
			ID: r.ID, ReversalID: r.ReversalID, TransferID: r.TransferID,
			DeltaMinor: r.DeltaMinor, RetryCount: r.RetryCount, MaxRetry: r.MaxRetry,
		})
	}
	return out, nil
}

func (a *reversalOutboxClaimAdapter) MarkDone(ctx context.Context, id int64) error {
	return a.ob.MarkDone(ctx, id)
}

func (a *reversalOutboxClaimAdapter) MarkDeadLetter(ctx context.Context, id int64, lastErr string) error {
	return a.ob.MarkDeadLetter(ctx, id, lastErr)
}

func (a *reversalOutboxClaimAdapter) UpdateError(ctx context.Context, id int64, lastErr string) error {
	return a.ob.UpdateError(ctx, id, lastErr)
}

// grpcsvcAcctAdapter — workflow.AccountingMetaCaller ↔ grpcsvc.AccountingMetaCaller 适配.
// 两边接口形态一致 (CreateTransaction(ctx, *domain.TransactionRequest) → resp), 只是
// resp 类型名不同 (前者 workflow.AccountingTxResp, 后者 grpcsvc.AccountingTxResp).
type grpcsvcAcctAdapter struct{ inner workflow.AccountingMetaCaller }

func (a grpcsvcAcctAdapter) CreateTransaction(ctx context.Context, req *domain.TransactionRequest) (*grpcsvc.AccountingTxResp, error) {
	r, err := a.inner.CreateTransaction(ctx, req)
	if err != nil || r == nil {
		return nil, err
	}
	return &grpcsvc.AccountingTxResp{VoucherNo: r.VoucherNo, Status: r.Status, Error: r.Error}, nil
}

// accountingGRPCAdapter — SP-AC-7 把 clients.AccountingGRPCClient 适配成 workflow.AccountingMetaCaller.
// 仅做返回类型转换 (clients.CreateTransactionResponse → workflow.AccountingTxResp).
type accountingGRPCAdapter struct {
	cli *clients.AccountingGRPCClient
}

func (a accountingGRPCAdapter) CreateTransaction(ctx context.Context, req *domain.TransactionRequest) (*workflow.AccountingTxResp, error) {
	resp, err := a.cli.CreateTransaction(ctx, req)
	if err != nil {
		return nil, err
	}
	return &workflow.AccountingTxResp{
		VoucherNo: resp.VoucherNo,
		Status:    resp.Status,
		Error:     resp.ErrorMessage,
	}, nil
}

// mTLS dead — 内部 mesh + Kitex 切换后客户端不再需要 dial credentials.
// 老 buildClientCreds / buildServerCreds 已删.

// zapSagaLogger 适配 zap 到 workflow.Logger 接口 (kv 风格).
type zapSagaLogger struct{ log *zap.Logger }

func (z zapSagaLogger) Info(msg string, kv ...any)  { z.log.Sugar().Infow(msg, kv...) }
func (z zapSagaLogger) Warn(msg string, kv ...any)  { z.log.Sugar().Warnw(msg, kv...) }
func (z zapSagaLogger) Error(msg string, kv ...any) { z.log.Sugar().Errorw(msg, kv...) }

// logAudit 简单把审计落 zap 日志 (生产换 audit-log service 客户端)。
type logAudit struct{ log *zap.Logger }

func (l logAudit) Write(_ context.Context, ev map[string]any) error {
	b, _ := json.Marshal(ev)
	l.log.Info("AUDIT", zap.ByteString("event", b))
	return nil
}

// splitCSV / trimSpaces 已删除 — viper 直接 unmarshal "a,b,c" → []string;
// kafka.brokers 在 yaml 写数组形式, env override 写 CSV, viper 自动处理.

// runRefundSubscriber SP-9: 订 refund-engine 的 refund.completed Kafka topic, 调 engine.HandleRefund.
//
// 用 franz-go 跟其它 Kafka client 风格对齐 (跟 reconplatform / KafkaEventPublisher 一致).
// 单个进程一个 consumer group, 多副本部署时按 partition 自动分担.
//
// SP-AC-7 L4: 失败处理升级:
//   - 解析失败的 bad msg → 写 DLQ topic + commit + metric, 不阻塞队列
//   - HandleRefund 业务失败 → 累计 retry header, 超 maxRetry → DLQ + commit; 否则不 commit 下次再试
func runRefundSubscriber(
	ctx context.Context,
	cfg *config.Config,
	engine *workflow.Engine,
	log *zap.Logger,
	trRepo workflow.TransferReverseRepo,
	feeRepo workflow.AppFeeRefundRepo,
	rvRepo workflow.ReversalExtRepo,
) {
	brokers := cfg.Kafka.Brokers
	topic := cfg.Kafka.Refund.Topic
	if topic == "" {
		topic = "recon.refund.events"
	}
	dlqTopic := cfg.Kafka.Refund.DLQTopic
	if dlqTopic == "" {
		dlqTopic = topic + ".dlq"
	}
	groupID := cfg.Kafka.Refund.Group
	if groupID == "" {
		groupID = "split-payment-refund-handler"
	}
	maxRetry := cfg.Kafka.Refund.MaxRetry
	if maxRetry <= 0 {
		maxRetry = 5
	}
	if len(brokers) == 0 {
		return
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(groupID),
		kgo.ConsumeTopics(topic),
		kgo.DisableAutoCommit(),
		kgo.SessionTimeout(30*time.Second),
	)
	if err != nil {
		log.Error("refund subscriber init failed", zap.Error(err))
		return
	}
	defer cl.Close()
	// SP-AC-7 L4: 独立 DLQ producer (跟 consumer 共 broker, 不同语义).
	dlqCl, derr := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.RecordRetries(3),
		kgo.RequestTimeoutOverhead(5*time.Second),
	)
	if derr != nil {
		log.Error("refund DLQ producer init failed; bad messages will only be logged", zap.Error(derr))
	} else {
		defer dlqCl.Close()
	}
	log.Info("refund subscriber started",
		zap.Strings("brokers", brokers),
		zap.String("topic", topic),
		zap.String("dlq_topic", dlqTopic),
		zap.String("group", groupID),
		zap.Int("max_retry", maxRetry))

	for {
		if ctx.Err() != nil {
			return
		}
		fetches := cl.PollFetches(ctx)
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, fe := range errs {
				log.Warn("refund kafka fetch err",
					zap.String("topic", fe.Topic),
					zap.Error(fe.Err))
			}
		}
		var toCommit []*kgo.Record
		iter := fetches.RecordIter()
		for !iter.Done() {
			rec := iter.Next()
			var ev workflow.RefundEvent
			if err := json.Unmarshal(rec.Value, &ev); err != nil {
				log.Error("refund event parse failed → DLQ",
					zap.String("topic", rec.Topic), zap.Error(err))
				observability.RefundEventCount.WithLabelValues("parse_error").Inc()
				sendToDLQ(ctx, dlqCl, dlqTopic, rec, "parse_error: "+err.Error(), log)
				toCommit = append(toCommit, rec)
				continue
			}
			if err := engine.HandleRefund(ctx, ev, trRepo, feeRepo, rvRepo); err != nil {
				retryCount := getRetryHeader(rec)
				log.Error("HandleRefund failed",
					zap.String("refund_id", ev.RefundID),
					zap.String("charge_id", ev.ChargeID),
					zap.Int("retry", retryCount),
					zap.Error(err))
				observability.RefundEventCount.WithLabelValues("handle_error").Inc()
				if retryCount >= maxRetry {
					log.Error("HandleRefund exhausted retries → DLQ",
						zap.String("refund_id", ev.RefundID),
						zap.Int("max_retry", maxRetry))
					sendToDLQ(ctx, dlqCl, dlqTopic, rec,
						fmt.Sprintf("max_retry_exceeded(%d): %v", maxRetry, err), log)
					toCommit = append(toCommit, rec)
					continue
				}
				continue // 还没到上限, 不 commit, 下次重试
			}
			observability.RefundEventCount.WithLabelValues("ok").Inc()
			toCommit = append(toCommit, rec)
		}
		if len(toCommit) > 0 {
			if err := cl.CommitRecords(ctx, toCommit...); err != nil {
				log.Warn("refund commit failed", zap.Error(err))
			}
		}
	}
}

// sendToDLQ — SP-AC-7 L4: 把坏 / 重试上限的消息推到 DLQ topic 留底, 带 forensics header.
// dlqCl nil → 仅 log warn (DLQ producer 起不来时不阻塞主流程).
func sendToDLQ(ctx context.Context, dlqCl *kgo.Client, dlqTopic string, orig *kgo.Record, reason string, log *zap.Logger) {
	if dlqCl == nil {
		log.Warn("DLQ producer nil; bad msg dropped after log",
			zap.String("orig_topic", orig.Topic), zap.String("reason", reason))
		return
	}
	rec := &kgo.Record{
		Topic: dlqTopic,
		Key:   orig.Key,
		Value: orig.Value,
		Headers: []kgo.RecordHeader{
			{Key: "x-orig-topic", Value: []byte(orig.Topic)},
			{Key: "x-orig-partition", Value: []byte(fmt.Sprintf("%d", orig.Partition))},
			{Key: "x-orig-offset", Value: []byte(fmt.Sprintf("%d", orig.Offset))},
			{Key: "x-dlq-reason", Value: []byte(reason)},
			{Key: "x-dlq-ts", Value: []byte(time.Now().UTC().Format(time.RFC3339))},
		},
	}
	dlqCl.Produce(ctx, rec, func(_ *kgo.Record, err error) {
		if err != nil {
			log.Error("DLQ produce failed",
				zap.String("dlq_topic", dlqTopic), zap.String("reason", reason), zap.Error(err))
		}
	})
}

// getRetryHeader — 从 record header 找 x-retry-count, 没有视为 0.
// 注: Kafka 不允许修改已 produce 的 record header, 实际累计需要 producer 端在重投时
// 主动写入新 record 带 incremented header. 当前实现读到的是 producer 提供的次数;
// 后续 outbox 重投 worker 应在 republish 时 ++ 这个 header.
func getRetryHeader(rec *kgo.Record) int {
	for _, h := range rec.Headers {
		if h.Key == "x-retry-count" {
			var n int
			_, _ = fmt.Sscanf(string(h.Value), "%d", &n)
			return n
		}
	}
	return 0
}
