// run.go — Main entry point. 业务 main.go 只调这一个函数.

package scaffold

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

// Opts 业务 main.go 的入参. 大部分字段可空, 走 cfg default.
type Opts struct {
	// 必填
	ServiceName    string
	ConfigPath     string                          // 可空; 没有就纯 env
	RegisterRoutes func(app *App, cfg *Config) error // 业务侧路由注册

	// 可选 — 没填 scaffold 自己 noop
	Models       []interface{}                    // GORM AutoMigrate
	BeforeStart  func(app *App, cfg *Config) error // db / migrate 之后, listen 之前
	AfterStop    func(app *App)                   // graceful shutdown 之后清理

	// gRPC server (跟 fiber 同 process, 不同端口); nil 不起 gRPC.
	GRPC *GRPCOpts

	// MigrationDir — 非空时 argv[1]="migrate" 进 migration 模式 (跑完 exit).
	MigrationDir string

	// OpenAPI: 非空自动暴露 /openapi.json + /docs (swagger-ui) + /redoc
	OpenAPISpec []byte
	OpenAPIInfo OpenAPIInfo

	// 业务侧自己的 config 类型 (嵌 scaffold.Config)
	// 如果填了, scaffold 把 yaml/env 也 hydrate 进去.
	CustomConfig interface{}
}

// Run 跑完整生命周期.
//
// 标准流程:
//   1. 加载 config (yaml + env)
//   2. 构建 logger
//   3. 打开 DB (如 cfg.DB.DSN 非空)
//   4. 创建 fiber App + 装中间件 + 公共路由
//   5. 调 opts.BeforeStart (业务侧钩子)
//   6. 调 opts.RegisterRoutes 注册业务路由
//   7. 启动 fiber listen (异步)
//   8. 等 SIGTERM / SIGINT
//   9. graceful shutdown (timeout from cfg)
//  10. 调 opts.AfterStop
func Run(opts Opts) {
	if opts.ServiceName == "" {
		panic("scaffold.Run: ServiceName required")
	}
	if opts.RegisterRoutes == nil {
		panic("scaffold.Run: RegisterRoutes required")
	}

	cfg, err := LoadConfig(opts.ConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scaffold: load config: %v\n", err)
		os.Exit(1)
	}
	cfg.ServiceName = opts.ServiceName

	log := NewLogger(*cfg)
	defer log.Sync()

	// OTel — cfg.OTel.Endpoint 设置后启用
	otelShutdown, err := initOTel(cfg.OTel, cfg.ServiceName, log)
	if err != nil {
		log.Warn("OTel init failed (continuing without tracing)", zap.Error(err))
		otelShutdown = func(context.Context) error { return nil }
	}
	defer func() {
		sCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = otelShutdown(sCtx)
	}()

	// CLI: migrate 子命令 — 跑完 exit, 不起 fiber/grpc
	if opts.MigrationDir != "" && len(os.Args) > 1 && os.Args[1] == "migrate" {
		runMigrate(MigrationConfig{Dir: opts.MigrationDir}, cfg.DB, log, os.Args[2:])
		return
	}

	log.Info("starting", zap.String("addr", cfg.Addr), zap.Bool("dev", cfg.LogDev))

	// DB (optional)
	var db *DB
	if cfg.DB.DSN != "" {
		db, err = NewDB(cfg.DB, log, opts.Models...)
		if err != nil {
			log.Fatal("db init", zap.Error(err))
		}
		defer db.Close()
		log.Info("db connected",
			zap.Int("max_open", cfg.DB.MaxOpenConns),
			zap.Duration("conn_lifetime", cfg.DB.ConnMaxLifetime))
	}

	app := NewApp(cfg, log, db)

	if opts.BeforeStart != nil {
		if err := opts.BeforeStart(app, cfg); err != nil {
			log.Fatal("before start", zap.Error(err))
		}
	}

	if err := opts.RegisterRoutes(app, cfg); err != nil {
		log.Fatal("register routes", zap.Error(err))
	}

	if len(opts.OpenAPISpec) > 0 {
		MountSwagger(app, opts.OpenAPISpec, opts.OpenAPIInfo)
		log.Info("openapi docs", zap.String("url", cfg.Addr+"/docs"))
	}

	// gRPC server (optional)
	var grpcStop func()
	if opts.GRPC != nil {
		stop, err := startGRPC(*opts.GRPC, cfg, log)
		if err != nil {
			log.Fatal("grpc start", zap.Error(err))
		}
		grpcStop = stop
	}

	// 监听
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		if err := app.Fiber.Listen(cfg.Addr); err != nil && err != fiber.ErrGracefulClose {
			log.Fatal("listen", zap.Error(err))
		}
	}()

	<-ctx.Done()
	log.Info("shutdown begin")

	if grpcStop != nil {
		grpcStop()
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutCancel()

	if err := app.Fiber.ShutdownWithContext(shutCtx); err != nil {
		log.Warn("shutdown", zap.Error(err))
	}

	if opts.AfterStop != nil {
		opts.AfterStop(app)
	}

	log.Info("bye")
	_ = time.Sleep // unused guard
}
