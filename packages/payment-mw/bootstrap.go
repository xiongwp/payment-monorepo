// bootstrap.go — 通用 cmd/server main helper，抽取 8 服务重复代码。
//
// 用法 (替换 cmd/server/main.go 几十行重复代码):
//
//   func main() {
//       mw.Bootstrap(mw.BootstrapConfig{
//           ServiceName: "billing-system",
//           DefaultPort: "8080",
//           SetupRoutes: func(mux *http.ServeMux, log *zap.Logger) error {
//               // 装业务路由
//               api := adminhttp.New(...)
//               api.Mount(mux)
//               return nil
//           },
//           OnStart: func(ctx context.Context, log *zap.Logger) error {
//               // 启后台 cron / consumer
//               go runDailyAggregator(ctx, agg, log)
//               return nil
//           },
//       })
//   }
//
// 自动包含:
//   - zap.NewProduction() logger
//   - mw.Chain: Recover / RequestLog / Trace / Metrics / CORS / RateLimit
//   - /healthz + /metrics (Prometheus)
//   - graceful shutdown (SIGTERM 30s drain)
//   - panic recovery
//   - mTLS 自动启用（环境变量配齐时）

package mw

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// BootstrapConfig 启动配置。
type BootstrapConfig struct {
	ServiceName string             // billing-system / refund-engine 等
	DefaultPort string             // 默认 "8080"，被 env <SVC>_HTTP_PORT 覆盖
	PortEnvKey  string             // env key 名（默认 ServiceName 大写 + "_HTTP_PORT"）
	SetupRoutes func(mux *http.ServeMux, log *zap.Logger) error
	OnStart     func(ctx context.Context, log *zap.Logger) error // optional 后台 worker
	OnShutdown  func(ctx context.Context) error                  // optional cleanup
	AuthCfg     AuthConfig                                       // 鉴权配置（PublicPaths / Keys）
	RateLimit   RateLimitConfig                                  // 限流配置
	CORSOrigins []string                                         // CORS allow origins
	DisableAuth bool                                             // dev 模式可关
}

// Bootstrap 启动 service — 抽取 8 服务 main.go 重复代码。
func Bootstrap(cfg BootstrapConfig) {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	port := cfg.DefaultPort
	if port == "" {
		port = "8080"
	}
	envKey := cfg.PortEnvKey
	if envKey == "" {
		envKey = strings_ToUpperASCII(cfg.ServiceName) + "_HTTP_PORT"
	}
	if v := os.Getenv(envKey); v != "" {
		port = v
	}

	mux := http.NewServeMux()
	// healthz + metrics 是 public path
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.Handle("/metrics", promhttp.Handler())

	if err := cfg.SetupRoutes(mux, logger); err != nil {
		logger.Fatal("setup routes", zap.Error(err))
	}

	// 默认 public path 加 /healthz /metrics
	publics := append([]string{"/healthz", "/metrics"}, cfg.AuthCfg.PublicPaths...)
	cfg.AuthCfg.PublicPaths = publics

	// 默认 CORS allow biz-admin-web
	if len(cfg.CORSOrigins) == 0 {
		cfg.CORSOrigins = []string{"*"} // dev 默认；生产收紧
	}

	mws := []Middleware{
		Recover(logger),
		Trace(),
		Metrics(cfg.ServiceName),
		RequestLog(logger),
		CORS(cfg.CORSOrigins),
		RateLimit(cfg.RateLimit),
	}
	if !cfg.DisableAuth {
		mws = append(mws, Auth(cfg.AuthCfg))
	}
	handler := Chain(mws...)(mux)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// mTLS 自动检测
	mtls := LoadMTLSFromEnv()
	if mtls != nil {
		tlsCfg, err := mtls.ServerTLS()
		if err != nil {
			logger.Fatal("mtls server tls", zap.Error(err))
		}
		srv.TLSConfig = tlsCfg
		srv.Addr = ":8443"
		logger.Info("server started (mTLS)",
			zap.String("service", cfg.ServiceName), zap.String("addr", srv.Addr))
	} else {
		logger.Info("server started (plain HTTP)",
			zap.String("service", cfg.ServiceName), zap.String("addr", srv.Addr))
	}

	// 后台 worker
	if cfg.OnStart != nil {
		if err := cfg.OnStart(ctx, logger); err != nil {
			logger.Fatal("OnStart", zap.Error(err))
		}
	}

	go func() {
		var err error
		if mtls != nil {
			err = srv.ListenAndServeTLS("", "") // cert/key 在 TLSConfig 里
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Error("listen", zap.Error(err))
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down gracefully (30s drain)...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()

	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Warn("server shutdown error", zap.Error(err))
	}
	if cfg.OnShutdown != nil {
		if err := cfg.OnShutdown(shutCtx); err != nil {
			logger.Warn("OnShutdown error", zap.Error(err))
		}
	}
	logger.Info("shutdown complete")
}

// strings_ToUpperASCII — 不依赖 strings 包（payment-mw 已 import strings 但
// 这里独立 helper 也行）。简化：直接 import 不行因为已有 strings import 冲突，
// 这里手写。
func strings_ToUpperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		} else if c == '-' {
			b[i] = '_'
		}
	}
	return string(b)
}

// EnvOr 通用环境变量 helper（避免每个 cmd/server 都重写）。
func EnvOr(k, dflt string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return dflt
}

var _ = fmt.Sprintf // 防 unused import 警告（如果 fmt 未来要用）
