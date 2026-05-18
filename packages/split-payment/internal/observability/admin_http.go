// admin_http.go — split-payment 的 admin HTTP server.
//
// SP-AC-7 L1: 之前 split-payment 是纯 gRPC, 没有 health endpoint, k8s probe 探不到.
// 加一个轻量 HTTP server 暴露:
//   GET /healthz     — liveness, 永远返 200 (只检查进程在)
//   GET /readiness   — readiness, 由 ReadyCheckFunc 自查依赖, 不就绪 → 503
//   GET /metrics     — Prometheus pull 端点
//
// 端口由 env SPLIT_ADMIN_HTTP_PORT 控制 (默认 9099, 跟 gRPC 9098 错开).
package observability

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// ReadyCheckFunc 一个就绪探针. 返 nil = 健康, 返 err = 503.
// 调用方注册多个 (e.g. DB ping, accounting gRPC channel ready, Kafka producer ready).
type ReadyCheckFunc func(ctx context.Context) error

// AdminServer 暴露 health / metrics.
type AdminServer struct {
	port   string
	log    *zap.Logger
	checks []namedCheck

	// draining 由 BeginDrain 设为 true, readiness 立即返 503, 用于优雅停机.
	draining atomic.Bool

	srv *http.Server
}

type namedCheck struct {
	name string
	fn   ReadyCheckFunc
}

// NewAdminServer 起一个 admin server. port 空 → 默认 9099.
func NewAdminServer(port string, log *zap.Logger) *AdminServer {
	if port == "" {
		port = "9099"
	}
	return &AdminServer{port: port, log: log}
}

// AddReadyCheck 注册一条就绪探针. 多次调用累加, 任一返 err → readiness 整体 503.
func (a *AdminServer) AddReadyCheck(name string, fn ReadyCheckFunc) {
	a.checks = append(a.checks, namedCheck{name: name, fn: fn})
}

// BeginDrain 标记进入优雅停机阶段, readiness 立刻返 503, 让 LB 摘流.
func (a *AdminServer) BeginDrain() {
	a.draining.Store(true)
}

// Run 启动 admin HTTP server, 阻塞直到 ctx.Done 或 ListenAndServe 出错.
func (a *AdminServer) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/readiness", a.handleReadiness)
	mux.Handle("/metrics", promhttp.Handler())

	a.srv = &http.Server{
		Addr:              ":" + a.port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if a.log != nil {
		a.log.Info("split-payment admin HTTP listening",
			zap.String("port", a.port),
			zap.Int("ready_checks", len(a.checks)))
	}
	// shutdown 监听 ctx.Done
	go func() {
		<-ctx.Done()
		a.BeginDrain()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.srv.Shutdown(shutCtx)
	}()
	if err := a.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// handleHealth — 永远 200 (只要进程在跑 admin server 就视为活).
// liveness probe 用, 不应检查下游依赖 (那是 readiness 的事).
func (a *AdminServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadiness — 跑所有 ReadyCheck, 任一失败返 503 + 错误明细.
// 进入 draining 阶段直接返 503 (不再 check 下游, 直接告诉 LB 摘流).
func (a *AdminServer) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if a.draining.Load() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	for _, c := range a.checks {
		if err := c.fn(ctx); err != nil {
			http.Error(w, c.name+": "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}
