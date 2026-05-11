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

	// 2. repos
	graphRepo := repo.NewMemoryGraphRepo()
	runRepo := repo.NewMemoryRunRepo()

	// 3. 启动期 seed 示例 graphs (从 examples/ 目录读)
	seedExampleGraphs(graphRepo, log)

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
	(&adminhttp.Server{Graphs: graphRepo, Runs: runRepo, Log: log}).Register(mux)

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

	// TODO: Kafka subscriber loop, 收到 BusinessEvent → engine.Handle(ctx, ev)
	//        生产用 kafka-go / sarama; demo 期间可暴露 /api/moneyflow/trigger 手动触发
	_ = engine // engine 实际生产由 Kafka subscriber 驱动

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

// logAudit 简单把审计落 zap 日志 (生产换 audit-log service 客户端)。
type logAudit struct{ log *zap.Logger }

func (l logAudit) Write(_ context.Context, ev map[string]any) error {
	b, _ := json.Marshal(ev)
	l.log.Info("AUDIT", zap.ByteString("event", b))
	return nil
}
