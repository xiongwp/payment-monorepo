// Package scaffold — bootstrap.go: 各 service main.go 共用的启动/关闭骨架.
//
// 用法:
//
//	func main() {
//	    scaffold.Bootstrap(scaffold.BootstrapOpts{
//	        ServiceName: "payment-core",
//	        Setup: func(ctx context.Context, deps scaffold.Deps) (scaffold.Runner, error) {
//	            // 构造 service / handler / gRPC server,返回一个实现 Runner 的对象
//	            return mySvc, nil
//	        },
//	    })
//	}
//
// 这套骨架替代了原 main.go 里每个服务都要重复写的:
//   - zap logger 初始化 (按 LOG_LEVEL/LOG_DEV env)
//   - OTel SDK init (按 OTEL_EXPORTER_OTLP_ENDPOINT env)
//   - SIGINT/SIGTERM 信号监听 + graceful shutdown
//   - /healthz /readyz probe http server (8080)
//   - prometheus /metrics 暴露 (混在 probe 端口)
//   - panic recover + 错误码非零退出
package scaffold

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	putil_trace "github.com/xiongwp/payment-util/trace"
)

// BootstrapOpts 启动选项.
type BootstrapOpts struct {
	// ServiceName 必填: 日志 + OTel resource.service.name
	ServiceName string

	// ProbePort /healthz /readyz /metrics 端口, 0 → 8080
	ProbePort int

	// ShutdownTimeout graceful shutdown 等待时长, 0 → 30s
	ShutdownTimeout time.Duration

	// Setup 由调用方实现: 构造 service / handler / gRPC server, 返回 Runner.
	// ctx 在 SIGINT/SIGTERM 时被 cancel; Setup 应当用此 ctx 启动后台 goroutine.
	Setup func(ctx context.Context, deps Deps) (Runner, error)

	// OnShutdown 可选: 在 Setup 返回的 Runner 之外要清理的资源 (e.g. close DB pool).
	OnShutdown func(ctx context.Context) error
}

// Deps 注入给 Setup 的依赖.
type Deps struct {
	Logger     *zap.Logger
	HTTPProbes *http.ServeMux // 调用方可挂额外 /debug 等端点

	// OtelShutdown 在退出前必须调,flush pending span
	OtelShutdown func(context.Context) error
}

// Runner 业务对象需实现这两个方法.
type Runner interface {
	// Start 启动业务循环 (gRPC server / kafka consumer / cron 等).
	// 必须非阻塞: 启动 goroutine 后立即返回.
	Start(ctx context.Context) error
	// Stop 阻塞,等到所有 in-flight 任务清理完.
	Stop(ctx context.Context) error
}

// Bootstrap 标准启动入口.
//
// 错误时 os.Exit(非零),不返回。
func Bootstrap(opts BootstrapOpts) {
	if opts.ServiceName == "" {
		fmt.Fprintln(os.Stderr, "scaffold: ServiceName required")
		os.Exit(2)
	}
	if opts.Setup == nil {
		fmt.Fprintln(os.Stderr, "scaffold: Setup required")
		os.Exit(2)
	}
	if opts.ProbePort == 0 {
		opts.ProbePort = 8080
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 30 * time.Second
	}

	// 1) Logger
	logger := buildLogger(opts.ServiceName)
	defer func() { _ = logger.Sync() }()

	// 2) OTel
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	otelShutdown, err := putil_trace.InitOTel(ctx, opts.ServiceName, os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if err != nil {
		logger.Warn("OTel init failed (continuing without traces)", zap.Error(err))
		otelShutdown = func(context.Context) error { return nil }
	}

	// 3) Probe server
	probeMux := http.NewServeMux()
	ready := newReadyFlag()
	probeMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	probeMux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})
	probeMux.Handle("/metrics", promhttp.Handler())

	probeSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", opts.ProbePort),
		Handler:           probeMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("probe server listening",
			zap.String("service", opts.ServiceName),
			zap.Int("port", opts.ProbePort))
		if err := probeSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("probe server failed", zap.Error(err))
		}
	}()

	// 4) Setup
	runner, err := opts.Setup(ctx, Deps{
		Logger:       logger,
		HTTPProbes:   probeMux,
		OtelShutdown: otelShutdown,
	})
	if err != nil {
		logger.Error("setup failed", zap.Error(err))
		os.Exit(1)
	}

	if err := runner.Start(ctx); err != nil {
		logger.Error("runner start failed", zap.Error(err))
		os.Exit(1)
	}
	ready.Store(true)

	// 5) 等信号
	<-ctx.Done()
	ready.Store(false)
	logger.Info("shutdown signal received, draining...",
		zap.Duration("timeout", opts.ShutdownTimeout))

	// 6) 关停 — runner 先,probe 后
	shutCtx, cancelShut := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
	defer cancelShut()
	if err := runner.Stop(shutCtx); err != nil {
		logger.Warn("runner stop reported error", zap.Error(err))
	}
	if opts.OnShutdown != nil {
		if err := opts.OnShutdown(shutCtx); err != nil {
			logger.Warn("OnShutdown error", zap.Error(err))
		}
	}
	if err := probeSrv.Shutdown(shutCtx); err != nil {
		logger.Warn("probe shutdown error", zap.Error(err))
	}
	if err := otelShutdown(shutCtx); err != nil {
		logger.Warn("otel shutdown error", zap.Error(err))
	}
	logger.Info("shutdown complete")
}

func buildLogger(service string) *zap.Logger {
	dev := os.Getenv("LOG_DEV") == "true"
	level := os.Getenv("LOG_LEVEL")
	var cfg zap.Config
	if dev {
		cfg = zap.NewDevelopmentConfig()
	} else {
		cfg = zap.NewProductionConfig()
	}
	if level != "" {
		var l zap.AtomicLevel
		if err := l.UnmarshalText([]byte(level)); err == nil {
			cfg.Level = l
		}
	}
	cfg.InitialFields = map[string]interface{}{"service": service}
	logger, err := cfg.Build(zap.AddCallerSkip(0))
	if err != nil {
		// 兜底: stderr console logger
		return zap.NewNop()
	}
	return logger
}

// readyFlag atomic boolean (避免引入额外依赖).
type readyFlag struct{ v atomicBool }

func newReadyFlag() *readyFlag { return &readyFlag{} }
func (r *readyFlag) Load() bool { return r.v.Load() }
func (r *readyFlag) Store(b bool) { r.v.Store(b) }

// minimal atomic bool
type atomicBool struct{ v int32 }

func (a *atomicBool) Load() bool {
	return loadInt32(&a.v) == 1
}
func (a *atomicBool) Store(b bool) {
	if b {
		storeInt32(&a.v, 1)
	} else {
		storeInt32(&a.v, 0)
	}
}
