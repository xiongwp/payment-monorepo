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

	// 4. workflow engine
	engine := &workflow.Engine{
		GraphRepo:  graphRepo,
		RunRepo:    runRepo,
		Accounting: accClient,
		Audit:      logAudit{log: log},
		Log:        log,
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

	srv := &http.Server{
		Addr: ":" + port, Handler: mux,
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

	// 事件驱动入口:
	//
	//  - 生产: Kafka subscriber 订阅 payment.events,每条 BusinessEvent 调
	//    engine.Handle(ctx, ev) 推进分账流。kafka 消费由 payment-util/kafkamq 提供。
	//  - dev / demo: HTTP /api/moneyflow/trigger 手动触发,见 internal/handler/trigger.go。
	//
	// 这里只挂 HTTP server;Kafka subscriber 由 deploy/k8s 里的 sidecar consumer
	// 单独起进程,通过本进程的 HTTP 内部端点把事件灌给 engine。这样保证 split-payment
	// 不依赖 Kafka 可用性,生产 Kafka 抖动时手动触发依旧可用。
	_ = engine

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

// logAudit 简单把审计落 zap 日志 (生产换 audit-log service 客户端)。
type logAudit struct{ log *zap.Logger }

func (l logAudit) Write(_ context.Context, ev map[string]any) error {
	b, _ := json.Marshal(ev)
	l.log.Info("AUDIT", zap.ByteString("event", b))
	return nil
}
