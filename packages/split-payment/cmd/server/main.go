// split-payment server — Money Flow Graph 编排服务入口。
//
// 起:
//   ACCOUNTING_GRPC_ADDR=accounting-system:9091 \
//   SPLIT_HTTP_PORT=8098 \
//   go run ./cmd/server
//
// 端点:
//   GET    /healthz
//   GET    /metrics
//   /api/moneyflow/*  — graph CRUD + dry-run + runs search
//
// 后台:
//   Kafka 订阅业务事件 → workflow.Engine.Handle → translator → accounting

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"reconcile-system/packages/split-payment/internal/adminhttp"
	"reconcile-system/packages/split-payment/internal/clients"
	"reconcile-system/packages/split-payment/internal/domain"
	"reconcile-system/packages/split-payment/internal/repo"
	"reconcile-system/packages/split-payment/internal/workflow"

	_ "github.com/go-sql-driver/mysql" // MF-1: mysql driver
	"github.com/twmb/franz-go/pkg/kgo" // SP-11 refund kafka subscriber
	accountingv1 "github.com/xiongwp/accounting-system/api/proto/accounting/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()

	port := envOr("SPLIT_HTTP_PORT", "8098")
	accAddr := envOr("ACCOUNTING_GRPC_ADDR", "accounting-system:9091")

	// 1. accounting client (gRPC)
	conn, err := grpc.NewClient(
		"dns:///"+accAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
	)
	if err != nil {
		log.Fatal("dial accounting", zap.Error(err))
	}
	defer conn.Close()

	accClient := clients.NewAccountingClient(accountingv1.NewAccountingServiceClient(conn))

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
		// SP-4 Stripe-style 资源对象 API (nil = memory 模式不挂)
		stripeAPI *adminhttp.StripeAPIServer
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
		db.SetMaxOpenConns(20)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(30 * time.Minute)
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
		// 把 Stripe API server 也挂到 mux (会在下方 Register).
		stripeAPI = &adminhttp.StripeAPIServer{
			Accounts: accRepo, Transfers: trRepo, Fees: feeRepo,
			Payouts: poRepo, Reversals: rvRepo, Log: log,
		}
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
		GraphRepo:    graphRepo,
		RunRepo:      runRepo,
		Accounting:   accClient,
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
	}

	// SP-3A: 接持久化 saga (MySQL 模式 + env SPLIT_PAYMENT_SAGA=1 才启).
	// 默认 dev 走老路径方便调试,生产强烈建议开 saga (失败可恢复 + 自动 compensate).
	if envOr("SPLIT_PAYMENT_SAGA", "") == "1" && engTrRepo != nil {
		sagaDeps := workflow.StepDeps{
			Accounting:      accClient,
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
			riskCli = clients.NewHTTPRiskClient(u, envOr("RISK_AUTH_TOKEN", ""))
			log.Info("risk client: http", zap.String("url", u))
		}
		if u := envOr("AML_HTTP_URL", ""); u != "" {
			amlCli = clients.NewHTTPAMLClient(u, envOr("AML_AUTH_TOKEN", ""))
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
		engine.FX = clients.NewHTTPFXClient(u, envOr("FX_AUTH_TOKEN", ""))
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

	// 5. HTTP server (admin API + dry-run + health)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	// adminhttp.Server 接受 GraphRepo / RunRepo 接口 — workflow 包同名接口的子集.
	// 这里把 workflow.* 实例换成 adminhttp 视角 (Save/GetByKey/List + Search).
	// memory / mysql 实现都满足.
	adminGraphs, _ := graphRepo.(adminhttp.GraphRepo)
	adminRuns, _ := runRepo.(adminhttp.RunRepo)
	(&adminhttp.Server{Graphs: adminGraphs, Runs: adminRuns, Log: log}).Register(mux)
	if stripeAPI != nil {
		stripeAPI.Register(mux)
		log.Info("stripe-style API registered (/api/connected_accounts, /api/transfers, /api/application_fees, /api/payouts)")
	}
	// SP-FIN-4: 4-eyes approval reviews API (仅 MySQL 模式)
	if reviewRepo, ok := runRepo.(adminhttp.ReviewableRunRepo); ok {
		reviewsServer := &adminhttp.ReviewsServer{
			Runs:     reviewRepo,
			Executor: engine,
			Events:   eventPub,
			Log:      log,
		}
		reviewsServer.Register(mux)
		log.Info("4-eyes reviews API registered (/api/moneyflow/reviews)")
	}

	// SP-FIN-5: Stripe API 兼容层 (Idempotency-Key + /v1/ alias).
	// 内存 store 适合单节点 dev; 生产替换为 Redis 实现.
	idemStore := adminhttp.NewMemoryIdempotencyStore()
	finalHandler := adminhttp.WithStripeCompat(mux, idemStore)
	log.Info("stripe compat enabled: idempotency-key + /v1/* alias")

	srv := &http.Server{
		Addr: ":" + port, Handler: finalHandler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Info("split-payment listening",
		zap.String("addr", srv.Addr), zap.String("accounting", accAddr))

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", zap.Error(err))
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
		go cron.Run(ctx)
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
	shutCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	_ = srv.Shutdown(shutCtx)
}

// ─── helpers ──────────────────────────────────────────────────────────

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
// 失败处理:
//   - 单条解析失败 → log error 跳过 (DLQ 留 Phase 3)
//   - HandleRefund 返 err → log + 不 commit, 下次重试
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
	groupID := envOr("SPLIT_PAYMENT_REFUND_GROUP", "split-payment-refund-handler")
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
	log.Info("refund subscriber started",
		zap.Strings("brokers", brokers),
		zap.String("topic", topic),
		zap.String("group", groupID))

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
				log.Warn("refund event parse failed",
					zap.String("topic", rec.Topic), zap.Error(err))
				toCommit = append(toCommit, rec) // bad msg 跳过
				continue
			}
			if err := engine.HandleRefund(ctx, ev, trRepo, feeRepo, rvRepo); err != nil {
				log.Error("HandleRefund failed",
					zap.String("refund_id", ev.RefundID),
					zap.String("charge_id", ev.ChargeID),
					zap.Error(err))
				continue // 不 commit, 重试
			}
			toCommit = append(toCommit, rec)
		}
		if len(toCommit) > 0 {
			if err := cl.CommitRecords(ctx, toCommit...); err != nil {
				log.Warn("refund commit failed", zap.Error(err))
			}
		}
	}
}
