// Package obsbootstrap — 一站式可观测性 bootstrap. 让任何 Go 服务一行起齐:
//
//	admin := obsbootstrap.NewAdminServer(obsbootstrap.AdminConfig{
//	    ServiceName: "payment-core",
//	    Port:        "9099",
//	    Logger:      logger,
//	    LogLevel:    logLevel, // zap.AtomicLevel, 让 /admin/log-level 能动态调
//	})
//	admin.AddReadyCheck("mysql", func(c context.Context) error { return db.PingContext(c) })
//	go admin.Run(ctx)
//
// 暴露端点:
//
//	GET  /healthz          — liveness, 永远 200
//	GET  /readyz           — readiness, 跑全部 ready_checks, 失败 → 503 + JSON 详情
//	GET  /metrics          — Prometheus pull
//	GET  /debug/pprof/*    — CPU / heap / goroutine / trace profile
//	GET  /admin/log-level  — 当前 zap level
//	POST /admin/log-level  — {"level":"debug"} 动态调级
//
// 设计原则:
//   - 跟 healthx.Liveness / Readiness 接口对齐, 但额外多 metrics + pprof + log-level
//   - 不强制 healthx 改动, 复用其 Probe 类型
//   - 进入 Drain (优雅停机) 时 readiness 立刻返 503, LB 摘流
package obsbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/pprof"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ReadyCheckFunc 单个就绪探针. 返 nil = 就绪.
type ReadyCheckFunc func(ctx context.Context) error

// AdminConfig.
type AdminConfig struct {
	// ServiceName 用于 metric label / OTel resource. 必填.
	ServiceName string
	// Port admin HTTP 端口, 默认 "9099".
	Port string
	// Logger zap 实例 (建议跟主 logger 共享).
	Logger *zap.Logger
	// LogLevel 给 /admin/log-level 动态调用. nil → log-level 端点不可用.
	LogLevel zap.AtomicLevel
	// HTTPTimeout — Read/Write/Idle timeouts. 0 → 默认 10s/30s/30s.
	HTTPTimeout time.Duration
}

// AdminServer 暴露 healthz / readyz / metrics / pprof / log-level.
type AdminServer struct {
	cfg AdminConfig

	checks   []namedCheck
	draining atomic.Bool

	srv *http.Server
}

type namedCheck struct {
	name string
	fn   ReadyCheckFunc
}

// NewAdminServer 构造但不启动.
func NewAdminServer(cfg AdminConfig) *AdminServer {
	if cfg.Port == "" {
		cfg.Port = "9099"
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "unknown-service"
	}
	return &AdminServer{cfg: cfg}
}

// AddReadyCheck 注册依赖探针 (DB ping / gRPC channel / Redis 等).
func (a *AdminServer) AddReadyCheck(name string, fn ReadyCheckFunc) {
	a.checks = append(a.checks, namedCheck{name: name, fn: fn})
}

// BeginDrain 切到优雅停机阶段, readyz 立刻返 503.
func (a *AdminServer) BeginDrain() {
	a.draining.Store(true)
}

// Run 阻塞跑 admin server. ctx.Done → graceful shutdown.
func (a *AdminServer) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/readyz", a.handleReadiness)
	mux.HandleFunc("/readiness", a.handleReadiness) // alias 跟 split-payment 原命名兼容
	mux.Handle("/metrics", promhttp.Handler())
	// pprof 调试端点 — 生产应该走 admin token 保护 (这里 dev mode 直暴露).
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/admin/log-level", a.handleLogLevel)

	timeout := a.cfg.HTTPTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	a.srv = &http.Server{
		Addr:              ":" + a.cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       timeout,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	if a.cfg.Logger != nil {
		a.cfg.Logger.Info("obs admin server started",
			zap.String("service", a.cfg.ServiceName),
			zap.String("port", a.cfg.Port),
			zap.Int("ready_checks", len(a.checks)))
	}
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

func (a *AdminServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// readinessResult 输出格式跟 healthx 风格保持一致.
type readinessResult struct {
	Status string                   `json:"status"`
	Probes map[string]readyEntry    `json:"probes,omitempty"`
}

type readyEntry struct {
	OK  bool   `json:"ok"`
	Err string `json:"err,omitempty"`
}

// handleReadiness 并发跑所有 ReadyCheck, 任一失败返 503.
func (a *AdminServer) handleReadiness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if a.draining.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(readinessResult{Status: "draining"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	type chRes struct {
		name string
		res  readyEntry
	}
	ch := make(chan chRes, len(a.checks))
	for _, c := range a.checks {
		c := c
		go func() {
			err := c.fn(ctx)
			r := readyEntry{OK: err == nil}
			if err != nil {
				r.Err = err.Error()
			}
			ch <- chRes{name: c.name, res: r}
		}()
	}
	results := readinessResult{Status: "ok", Probes: map[string]readyEntry{}}
	failed := false
	for range a.checks {
		r := <-ch
		results.Probes[r.name] = r.res
		if !r.res.OK {
			failed = true
		}
	}
	if failed {
		results.Status = "fail"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(results)
}

// handleLogLevel: GET 查; POST {"level":"debug|info|warn|error"} 调.
func (a *AdminServer) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		_, _ = w.Write([]byte(a.cfg.LogLevel.String()))
	case http.MethodPost, http.MethodPut:
		var body struct {
			Level string `json:"level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var lvl zapcore.Level
		if err := lvl.UnmarshalText([]byte(body.Level)); err != nil {
			http.Error(w, "invalid level (debug/info/warn/error): "+err.Error(), http.StatusBadRequest)
			return
		}
		a.cfg.LogLevel.SetLevel(lvl)
		if a.cfg.Logger != nil {
			a.cfg.Logger.Info("log level changed via admin HTTP",
				zap.String("service", a.cfg.ServiceName),
				zap.String("new_level", lvl.String()))
		}
		_, _ = w.Write([]byte("ok: " + lvl.String()))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
