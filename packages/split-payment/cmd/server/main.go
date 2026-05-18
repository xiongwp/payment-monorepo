// split-payment server — Money Flow Graph 编排服务入口 (SP-AC-7 pure gRPC).
//
// 起:
//   ACCOUNTING_GRPC_ADDR=accounting-system:9091 \
//   SPLIT_GRPC_PORT=9098 \
//   go run ./cmd/server
//
// 暴露:
//   gRPC :9098  — split_payment.v1.AdminService (Graph CRUD / DryRun)
//                  admin-web BFF 通过这个端口调
//
// 后台:
//   Kafka 订阅业务事件 → workflow.Engine.Handle → translator → accounting (gRPC)
//   Payout cron / refund subscriber / saga recovery 等内部 worker
//
// SP-AC-7 改造: HTTP server 全部下线 (旧路径: adminhttp.Server + StripeAPIServer +
// ReviewsServer + Stripe 兼容层). 业务调用一律走 gRPC, 通信对端 (admin-web / accounting-system)
// 跟着切换. 外部 Stripe API 兼容如需保留, 后续在独立的 stripe-gateway 服务里做.

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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"reconcile-system/packages/split-payment/internal/clients"
	"reconcile-system/packages/split-payment/internal/domain"
	"reconcile-system/packages/split-payment/internal/grpcsvc"
	"reconcile-system/packages/split-payment/internal/observability"
	"reconcile-system/packages/split-payment/internal/repo"
	"reconcile-system/packages/split-payment/internal/workflow"

	_ "github.com/go-sql-driver/mysql" // MF-1: mysql driver
	"github.com/twmb/franz-go/pkg/kgo" // SP-11 refund kafka subscriber
	"github.com/xiongwp/payment-util/serviceregistry" // SP-AC-7 L2+X2: hardened gRPC dial
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()

	// SP-AC-7 P10: OTel trace context propagation (W3C traceparent). 当前用 noop tracer,
	// 不外发, 仅保证 ctx 传递. 接 OTLP exporter 时改 observability.InitTracer 内部即可.
	shutdownTracer := observability.InitTracer("split-payment")
	defer shutdownTracer(context.Background())

	// SP-AC-7: HTTP server 已废除, split-payment 现是纯 gRPC 内部服务.
	// SPLIT_GRPC_PORT 由 runAdminGRPCServer 读取 (默认 9098).
	accAddr := envOr("ACCOUNTING_GRPC_ADDR", "accounting-system:9091")

	// 1. accounting client (gRPC).
	//
	// SP-AC-7 L2+X2: 之前裸 grpc.NewClient + passthrough + insecure → 无 retry / 无 keepalive /
	// 无 LB; 改用 payment-util/serviceregistry.DialWithFallback 拿一组 hardenedOptions:
	//   - round_robin LB (多副本 accounting-service 真均摊)
	//   - 幂等 RPC 自动重试瞬态 UNAVAILABLE / DEADLINE_EXCEEDED
	//   - HTTP/2 keepalive 10s+3s 探活, 副本被 kill 后 ~13s 内 client 端 detect
	//
	// REGISTRY_ENDPOINTS (etcd) 配了就走真服务发现; 没配则降级直连 fallback addr (dev 模式).
	registryEndpoints := splitCSV(envOr("REGISTRY_ENDPOINTS", ""))
	conn, err := serviceregistry.DialWithFallback(
		registryEndpoints, "accounting-service", accAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatal("dial accounting", zap.Error(err))
	}
	defer conn.Close()

	// SP-AC-7: legacy AccountingClient stub 已删除, 业务调用一律走 AccountingGRPCClient → 新 TransactionService.

	// 2. repos — MF-1: 优先 MySQL (SPLIT_PAYMENT_DSN 配了就走), fallback memory.
	//
	//   SPLIT_PAYMENT_DSN 例:
	//     "split_user:pwd@tcp(shared-meta:3306)/split_payment?parseTime=true&charset=utf8mb4"
	//
	// memory mode: 单进程,重启丢全部 graph (适合 dev / 单测).
	// mysql  mode: 持久 + 多副本共享.
	var (
		graphRepo workflow.GraphRepo
		runRepo   workflow.RunRepo
		// SP-6 typed repos 注到 engine 用 (nil = 跑老路径不持 typed 对象)
		engAccRepo  workflow.AccountRepo
		engTrRepo   workflow.TransferRepo
		engFeeRepo  workflow.AppFeeRepo
		engPoRepo   workflow.PayoutRepo
		// SP-9 refund handler 用的扩展 repo
		refundTrRepo  workflow.TransferReverseRepo
		refundFeeRepo workflow.AppFeeRefundRepo
		refundRvRepo  workflow.ReversalExtRepo
		// SP-10 PayoutCron 用
		cronAccRepo workflow.AccountListerRepo
		cronPoRepo  workflow.PayoutInserterRepo
	)
	if dsn := os.Getenv("SPLIT_PAYMENT_DSN"); dsn != "" {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			log.Fatal("open mysql", zap.Error(err))
		}
		// SP-AC-7 P4: pool size 调大并 env 化 — 之前 20/5 在多副本高 QPS 下偏小,
		// MySQL idle 连接复用率不够, 高峰期会大量打开 + tear down.
		maxOpen := envInt("SPLIT_PAYMENT_DB_MAX_OPEN", 100)
		maxIdle := envInt("SPLIT_PAYMENT_DB_MAX_IDLE", 20)
		db.SetMaxOpenConns(maxOpen)
		db.SetMaxIdleConns(maxIdle)
		db.SetConnMaxLifetime(30 * time.Minute)
		log.Info("split-payment DB pool sized",
			zap.Int("max_open", maxOpen), zap.Int("max_idle", maxIdle))
		if err := db.PingContext(context.Background()); err != nil {
			log.Fatal("ping mysql", zap.Error(err))
		}
		ensureCtx, ensureCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := repo.EnsureSchema(ensureCtx, db); err != nil {
			log.Fatal("ensure schema", zap.Error(err))
		}
		ensureCancel()
		graphRepo = repo.NewMySQLGraphRepo(db)
		runRepo = repo.NewMySQLRunRepo(db)
		// SP-4: Stripe-style 实体表 (connected_accounts / transfers / fees / payouts / reversals)
		ensure2, ensureCancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		if err := repo.EnsureStripeSchema(ensure2, db); err != nil {
			log.Fatal("ensure stripe schema", zap.Error(err))
		}
		ensureCancel2()
		// SP-7: graph versioning schema
		ensure3, ensureCancel3 := context.WithTimeout(context.Background(), 10*time.Second)
		if err := repo.EnsureVersioningSchema(ensure3, db); err != nil {
			log.Warn("ensure versioning schema (continuing)", zap.Error(err))
		}
		ensureCancel3()
		// SP-3A: saga store schema
		ensure4, ensureCancel4 := context.WithTimeout(context.Background(), 10*time.Second)
		if err := repo.EnsureSagaSchema(ensure4, db); err != nil {
			log.Warn("ensure saga schema (continuing)", zap.Error(err))
		}
		ensureCancel4()

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
		log.Info("repos: mysql + stripe entities ready", zap.String("dsn_host", maskDSN(dsn)))
	} else {
		mg := repo.NewMemoryGraphRepo()
		mr := repo.NewMemoryRunRepo()
		// 启动期 seed 示例 graphs (从 examples/ 目录读) — 仅 memory 模式;
		// MySQL 模式由 admin UI / migrate 工具填.
		seedExampleGraphs(mg, log)
		graphRepo = mg
		runRepo = mr
		log.Info("repos: memory (set SPLIT_PAYMENT_DSN to use MySQL)")
	}

	// SP-8: optional Kafka event publisher.
	// SPLIT_PAYMENT_KAFKA_BROKERS 配了就连 Kafka, 没配走 NoopEventPublisher (dev / 单节点).
	var eventPub workflow.EventPublisher = workflow.NoopEventPublisher{}
	if brokers := envOr("SPLIT_PAYMENT_KAFKA_BROKERS", ""); brokers != "" {
		evCfg := workflow.DefaultKafkaEventConfig(splitCSV(brokers))
		evCfg.Topic = envOr("SPLIT_PAYMENT_EVENT_TOPIC", evCfg.Topic)
		evCfg.LiveMode = envOr("RECON_ENV", "dev") != "dev"
		kp, kerr := workflow.NewKafkaEventPublisher(evCfg, log)
		if kerr != nil {
			log.Warn("kafka event publisher init failed (using noop)",
				zap.Error(kerr))
		} else {
			eventPub = kp
			defer kp.Close()
			log.Info("event publisher: kafka",
				zap.String("topic", evCfg.Topic),
				zap.Bool("livemode", evCfg.LiveMode))
		}
	} else {
		log.Info("event publisher: noop (set SPLIT_PAYMENT_KAFKA_BROKERS to enable)")
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
		if err := repo.EnsureEventOutboxSchema(ctx, db); err != nil {
			log.Warn("event_outbox schema migration failed; events 走原 fire-and-forget 模式", zap.Error(err))
		} else if eventPub != nil {
			evOutbox := &repo.EventOutbox{DB: db}
			// 把 engine.Events 换成 outbox publisher
			engine.Events = &workflow.OutboxEventPublisher{
				Outbox: &eventOutboxEnqAdapter{ob: evOutbox},
				Log:    log,
			}
			// 起 worker drain outbox → 真 Kafka.
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
		if err := repo.EnsureReversalOutboxSchema(ctx, db); err != nil {
			log.Warn("reversal_retry_outbox schema migration failed; retry queue disabled", zap.Error(err))
		} else {
			revOutbox := &repo.ReversalOutbox{DB: db, Log: log}
			engine.ReversalRetry = &reversalOutboxAdapter{ob: revOutbox}
			// 起 worker 周期消费.
			retryWk := &workflow.ReversalRetryWorker{
				Cfg:     workflow.DefaultReversalRetryConfig(),
				Outbox:  &reversalOutboxClaimAdapter{ob: revOutbox},
				Applier: engine.ReversalApply,
				Log:     log,
			}
			go retryWk.Run(ctx)
			log.Info("reversal retry worker started")
		}
	}

	// SP-3A: 接持久化 saga (MySQL 模式 + env SPLIT_PAYMENT_SAGA=1 才启).
	// 默认 dev 走老路径方便调试,生产强烈建议开 saga (失败可恢复 + 自动 compensate).
	if envOr("SPLIT_PAYMENT_SAGA", "") == "1" && engTrRepo != nil {
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
		if dsn := os.Getenv("SPLIT_PAYMENT_DSN"); dsn != "" {
			db, _ := sql.Open("mysql", dsn)
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
		log.Info("saga mode: enabled (SPLIT_PAYMENT_SAGA=1)")

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
		log.Info("saga mode: disabled (set SPLIT_PAYMENT_SAGA=1 to enable persistent saga)")
	}

	// SP-3B: Risk + AML gate (optional, dev 默认走 AlwaysAllow 占位).
	// 生产由 main.go 注入真实 RiskClient (gRPC 调 risk-manage / aml-screening 服务).
	{
		riskCfg := workflow.DefaultRiskGateConfig()
		if v := envOr("SPLIT_PAYMENT_AML_THRESHOLD_CENTS", ""); v != "" {
			var n int64
			fmt.Sscanf(v, "%d", &n)
			if n > 0 {
				riskCfg.AMLThresholdMinor = n
			}
		}
		if envOr("SPLIT_PAYMENT_RISK_FAIL_OPEN", "") == "1" {
			riskCfg.FailSafeReject = false
		}
		// SP-FIN-1: 真实 HTTP client (env 配了 URL 走真实, 否则 AlwaysAllow 占位).
		var riskCli workflow.RiskClient = workflow.AlwaysAllowRisk{}
		var amlCli workflow.AMLClient = workflow.AlwaysAllowAML{}
		if u := envOr("RISK_HTTP_URL", ""); u != "" {
			riskCli = workflow.NewHTTPRiskClient(u, envOr("RISK_AUTH_TOKEN", ""))
			log.Info("risk client: http", zap.String("url", u))
		}
		if u := envOr("AML_HTTP_URL", ""); u != "" {
			amlCli = workflow.NewHTTPAMLClient(u, envOr("AML_AUTH_TOKEN", ""))
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
		accountingGRPCCli := clients.NewAccountingGRPCClient(conn)
		engine.AccountingMeta = accountingGRPCAdapter{cli: accountingGRPCCli}
		log.Info("accounting meta client: gRPC (TransactionService)")
	} else {
		log.Info("accounting meta client: disabled (accounting gRPC conn nil)")
	}

	// SP-3C + SP-FIN-1: FX client (env FX_HTTP_URL 配了走真实, 否则 static 占位).
	if u := envOr("FX_HTTP_URL", ""); u != "" {
		engine.FX = workflow.NewHTTPFXClient(u, envOr("FX_AUTH_TOKEN", ""))
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Info("split-payment internal gRPC service starting",
		zap.String("grpc_port", envOr("SPLIT_GRPC_PORT", "9098")),
		zap.String("accounting", accAddr))

	// SP-AC-7: gRPC AdminService — admin-web BFF 通过此端口调.
	// engine.AccountingMeta 是 workflow.AccountingMetaCaller 接口实例, 把它适配成
	// grpcsvc.AccountingMetaCaller (同形态, 不同包) 供 TriggerEvent 真落账用.
	var grpcAcct grpcsvc.AccountingMetaCaller
	if engine.AccountingMeta != nil {
		grpcAcct = grpcsvcAcctAdapter{inner: engine.AccountingMeta}
	}
	go runAdminGRPCServer(ctx, log, sgGraphs, grpcAcct)

	// SP-AC-7 L1+P9: split-payment admin HTTP — /healthz + /readiness + /metrics.
	// 跟 gRPC :9098 错开 (默认 :9099), env SPLIT_ADMIN_HTTP_PORT 可覆盖.
	adminSrv := observability.NewAdminServer(envOr("SPLIT_ADMIN_HTTP_PORT", "9099"), log)
	// readiness 探针: MySQL ping (DSN 配了才探).
	if db != nil {
		adminSrv.AddReadyCheck("mysql", func(c context.Context) error {
			return db.PingContext(c)
		})
	}
	// readiness 探针: accounting gRPC channel 是否就绪 (state != IDLE/CONNECTING/SHUTDOWN).
	adminSrv.AddReadyCheck("accounting_grpc", func(c context.Context) error {
		state := conn.GetState().String()
		if state == "SHUTDOWN" {
			return fmt.Errorf("accounting gRPC channel state=%s", state)
		}
		return nil
	})
	go func() {
		if err := adminSrv.Run(ctx); err != nil {
			log.Error("admin http server exited", zap.Error(err))
		}
	}()

	// SP-FIN-2: PayoutDispatchWorker — pending → in_transit 状态机.
	// 调 clearing-settlement 服务 (env CLEARING_HTTP_URL 配了走真实, 否则 Noop).
	if cronPoRepo != nil {
		clear := workflow.ClearingClient(workflow.NoopClearingClient{Log: log})
		// TODO: 真实 ClearingClient → 新增 internal/clients/clearing.go HTTP impl.
		// 占位 Noop 行为: 标 in_transit 假装已发送.
		_ = envOr("CLEARING_HTTP_URL", "") // reserved for future HTTP impl
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
		// LiveMode 由 env 控制, 默认 dev=false 只 log 不真创建
		if envOr("SPLIT_PAYMENT_PAYOUT_LIVE", "") == "1" {
			cron.Cfg.LiveMode = true
		}
		if interval := envOr("SPLIT_PAYMENT_PAYOUT_CRON_INTERVAL", ""); interval != "" {
			if d, derr := time.ParseDuration(interval); derr == nil {
				cron.Cfg.Interval = d
			}
		}
		// SP-AC-7 X3: 多副本 lease, 同一时刻只有一个副本跑 cron.
		// memory 模式 (db nil) 退化为无锁直跑 (单副本 OK).
		if db != nil {
			if err := workflow.EnsureCronLeaseSchema(ctx, db); err != nil {
				log.Warn("cron_lease schema migration failed; falling back to no-lease (可能重复扫)", zap.Error(err))
				go cron.Run(ctx)
			} else {
				lease := &workflow.CronLease{
					DB:     db,
					Name:   "payout_cron",
					Holder: workflow.DefaultHolder(),
					TTL:    30 * time.Second,
					Log:    log,
				}
				go lease.RunWithLease(ctx, cron.Run)
			}
		} else {
			go cron.Run(ctx)
		}

		// SP-AC-7 L8: HoldUnstickWorker — 之前实现完整但 main.go 0 caller.
		// 当前 RunRepo 没暴露 ListExpiredHolds (需要 moneyflow_runs.hold_until 字段, 后续 schema migration),
		// 先挂 NoopPendingHoldsRepo, worker 结构性启动但 tick 时无事可做. 后接真 repo 即可.
		holdWorker := &workflow.HoldUnstickWorker{
			Cfg:      workflow.DefaultHoldUnstickConfig(),
			Plans:    workflow.NoopPendingHoldsRepo{},
			Releaser: nil, // SP-FIN-3 接 accounting; nil 等价于仅发 hold.released 事件
			Events:   eventPub,
			Log:      log,
		}
		log.Warn("HoldUnstickWorker started with NoopPendingHoldsRepo — hold release 暂未生效, 等 RunRepo.ListExpiredHolds 实现")
		go holdWorker.Run(ctx)
	}

	// SP-9: Kafka subscriber 订 refund-engine 的 refund.completed 事件 → engine.HandleRefund.
	// 复用上面的 brokers env. SPLIT_PAYMENT_REFUND_TOPIC 配可改默认 topic.
	if envOr("SPLIT_PAYMENT_KAFKA_BROKERS", "") != "" && refundTrRepo != nil && refundRvRepo != nil {
		go runRefundSubscriber(ctx, engine, log,
			refundTrRepo, refundFeeRepo, refundRvRepo)
	} else {
		log.Info("refund subscriber: disabled (set SPLIT_PAYMENT_KAFKA_BROKERS + MySQL mode to enable)")
	}

	// 事件驱动入口:
	//
	//  - 生产: Kafka subscriber 订阅 payment.events,每条 BusinessEvent 调
	//    engine.Handle(ctx, ev) 推进分账流。kafka 消费由 payment-util/kafkamq 提供。
	//  - dev / demo: HTTP /api/moneyflow/trigger 手动触发,见 internal/handler/trigger.go。

	<-ctx.Done()
	log.Info("shutting down")
	// gRPC server 在 runAdminGRPCServer goroutine 里监听 ctx.Done() 自己 GracefulStop,
	// 这里只要等几百毫秒让正在跑的 RPC / Kafka subscriber 收尾即可.
	time.Sleep(500 * time.Millisecond)
}

// ─── helpers ──────────────────────────────────────────────────────────

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		var n int
		_, err := fmt.Sscanf(v, "%d", &n)
		if err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func seedExampleGraphs(r *repo.MemoryGraphRepo, log *zap.Logger) {
	dir := envOr("MONEYFLOW_SEED_DIR", "./examples/moneyflow-graphs")
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
// 监听端口由 env SPLIT_GRPC_PORT 控制 (默认 9098).
//
// ruleSync 来自 env ACCOUNTING_HTTP_URL (e.g. http://accounting-service:8888),
// SaveGraph 时把派生的 rules POST 到 /admin/transaction-rules. 空 → 关掉同步.
func runAdminGRPCServer(ctx context.Context, log *zap.Logger, graphs grpcsvc.GraphRepo, acct grpcsvc.AccountingMetaCaller) {
	port := envOr("SPLIT_GRPC_PORT", "9098")
	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Error("split gRPC listen failed", zap.Error(err))
		return
	}
	var ruleSync grpcsvc.AccountingRuleSyncer
	var orderReset grpcsvc.AccountingOrderResetter
	if base := envOr("ACCOUNTING_HTTP_URL", ""); base != "" {
		base = strings.TrimRight(base, "/")
		ruleSync = &httpRuleSyncer{baseURL: base, log: log}
		orderReset = &httpOrderResetter{baseURL: base, log: log}
		log.Info("split-payment: accounting admin HTTP wired",
			zap.String("accounting_http", base),
			zap.String("for", "SaveGraph saga + TriggerEvent retry"))
	} else {
		log.Warn("split-payment: ACCOUNTING_HTTP_URL empty, saga + retry features disabled")
	}
	// SP-AC-7 S1+S2: token auth interceptor.
	//   - env SPLIT_PAYMENT_ADMIN_TOKEN 配了 → 所有 gRPC 调用必须带 metadata X-Admin-Token 等值
	//   - 空 → DEV 模式 ⚠ log warn 提醒生产应该配
	authToken := envOr("SPLIT_PAYMENT_ADMIN_TOKEN", "")
	var opts []grpc.ServerOption
	// SP-AC-7 L3+P1: gRPC server keepalive + 限流, 防超长闲连接 / 巨型 payload 打挂进程.
	//   - KeepaliveEnforcementPolicy 配合 client 端 hardenedOptions (10s ping), 否则会 GOAWAY enhance_your_calm.
	//   - MaxConcurrentStreams 64 防恶意客户端打开过多并发流耗光资源.
	//   - MaxRecvMsgSize 16MB 兼容大 graph spec_json (默认 4MB 不够大 graph).
	opts = append(opts,
		grpc.MaxConcurrentStreams(64),
		grpc.MaxRecvMsgSize(16*1024*1024),
		serviceregistry.HardenedServerOptions()[0], // KeepaliveEnforcementPolicy
	)
	if authToken != "" {
		opts = append(opts, grpc.UnaryInterceptor(adminTokenInterceptor(authToken)))
		log.Info("split-payment gRPC: admin token auth enabled")
	} else {
		log.Warn("split-payment gRPC: AUTH DISABLED — set SPLIT_PAYMENT_ADMIN_TOKEN env var in production")
	}
	srv := grpc.NewServer(opts...)
	// SP-AC-7 S6: 资金审计 - 默认 zap sink, 生产应换 KafkaAuditSink (推送到独立审计 topic).
	auditSink := &grpcsvc.ZapAuditSink{Log: log.Named("audit")}
	grpcsvc.RegisterAdminServiceServer(srv, grpcsvc.NewServer(graphs, acct, ruleSync, orderReset, auditSink, log))
	log.Info("split-payment gRPC AdminService listening", zap.String("port", port))
	go func() { <-ctx.Done(); srv.GracefulStop() }()
	if err := srv.Serve(lis); err != nil {
		log.Error("split gRPC serve", zap.Error(err))
	}
}

// adminTokenInterceptor 校验 metadata `x-admin-token` 是否匹配预期 token.
// 不匹配 → grpc.Unauthenticated. metadata header 名小写: gRPC 规范要求.
func adminTokenInterceptor(expectedToken string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing metadata")
		}
		tokens := md.Get("x-admin-token")
		if len(tokens) == 0 || tokens[0] != expectedToken {
			return nil, status.Error(codes.Unauthenticated, "invalid or missing X-Admin-Token")
		}
		return handler(ctx, req)
	}
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

// splitCSV "a,b, c" → ["a","b","c"]; 用于 brokers 配置.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	cur := ""
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ',' {
			if cur != "" {
				out = append(out, trimSpaces(cur))
				cur = ""
			}
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, trimSpaces(cur))
	}
	return out
}

func trimSpaces(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

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
	engine *workflow.Engine,
	log *zap.Logger,
	trRepo workflow.TransferReverseRepo,
	feeRepo workflow.AppFeeRefundRepo,
	rvRepo workflow.ReversalExtRepo,
) {
	brokers := splitCSV(envOr("SPLIT_PAYMENT_KAFKA_BROKERS", ""))
	topic := envOr("SPLIT_PAYMENT_REFUND_TOPIC", "recon.refund.events")
	dlqTopic := envOr("SPLIT_PAYMENT_REFUND_DLQ_TOPIC", topic+".dlq")
	groupID := envOr("SPLIT_PAYMENT_REFUND_GROUP", "split-payment-refund-handler")
	maxRetry := envInt("SPLIT_PAYMENT_REFUND_MAX_RETRY", 5)
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
