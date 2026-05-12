// fx_modules.go — uber/fx 模块化 DI.
//
// scaffold 的核心组件 (Config / Logger / DB / Fiber app / gRPC / OTel) 全部用 fx
// 注入. 业务侧 fx.Invoke 拿现成 deps, 不用手动 new.
//
// 优点 vs 原来:
//   - 测试时 fx.Decorate 替换 DB / Logger 不改业务代码
//   - 启停 lifecycle 自动 (fx.Lifecycle hook)
//   - 模块化 (业务可以 fx.Module 拆自己的 deps)
//   - 显式依赖图 (fx visualize)
//
// 业务侧 main.go (fx 风格):
//
//   func main() {
//       scaffold.RunFx(scaffold.FxOpts{
//           ServiceName:  "data-rights",
//           ConfigPath:   "config/config.yaml",
//           MigrationDir: "migrations",
//           Models:       []any{&domain.RequestGormModel{}},
//           OpenAPISpec:  openapiYAML,
//
//           // 业务侧 fx Module — 装 repo / service / handler
//           AppModules: []fx.Option{
//               fx.Provide(repo.NewGormRepo),
//               fx.Provide(service.New),
//               fx.Provide(handler.New),
//               fx.Invoke(handler.Mount),  // 注册路由
//           },
//       })
//   }

package scaffold

import (
	"context"
	"os"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
)

// ── 各 fx.Module ──

// ConfigModule provides *Config.
var ConfigModule = fx.Module("scaffold-config",
	fx.Provide(func(opts FxOpts) (*Config, error) {
		cfg, err := LoadConfig(opts.ConfigPath)
		if err != nil {
			return nil, err
		}
		cfg.ServiceName = opts.ServiceName
		return cfg, nil
	}),
)

// LoggerModule provides *zap.Logger.
var LoggerModule = fx.Module("scaffold-logger",
	fx.Provide(func(cfg *Config) *zap.Logger {
		return NewLogger(*cfg)
	}),
	fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
		// fx 内部事件 (provide/invoke/start) 走 zap (Debug 级)
		return &fxevent.ZapLogger{Logger: log.Named("fx")}
	}),
)

// OTelModule provides shutdown func; 启动期初始化, 退出期 shutdown.
var OTelModule = fx.Module("scaffold-otel",
	fx.Invoke(func(lc fx.Lifecycle, cfg *Config, log *zap.Logger) error {
		shutdown, err := initOTel(cfg.OTel, cfg.ServiceName, log)
		if err != nil {
			log.Warn("OTel init failed", zap.Error(err))
			return nil
		}
		lc.Append(fx.Hook{
			OnStop: func(ctx context.Context) error {
				sCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				return shutdown(sCtx)
			},
		})
		return nil
	}),
)

// DBModule provides *DB (空 DSN 则 nil — 业务侧自己处理).
var DBModule = fx.Module("scaffold-db",
	fx.Provide(func(lc fx.Lifecycle, opts FxOpts, cfg *Config, log *zap.Logger) (*DB, error) {
		if cfg.DB.DSN == "" {
			return nil, nil
		}
		db, err := NewDB(cfg.DB, log, opts.Models...)
		if err != nil {
			return nil, err
		}
		lc.Append(fx.Hook{
			OnStop: func(_ context.Context) error {
				return db.Close()
			},
		})
		return db, nil
	}),
)

// FiberModule provides *App (fiber + 中间件 + 公共路由) + 启动 lifecycle.
var FiberModule = fx.Module("scaffold-fiber",
	fx.Provide(func(cfg *Config, log *zap.Logger, db *DB) *App {
		return NewApp(cfg, log, db)
	}),
	fx.Invoke(func(lc fx.Lifecycle, app *App, cfg *Config, opts FxOpts, log *zap.Logger) {
		// OpenAPI 自动挂
		if len(opts.OpenAPISpec) > 0 {
			MountSwagger(app, opts.OpenAPISpec, opts.OpenAPIInfo)
		}
		lc.Append(fx.Hook{
			OnStart: func(_ context.Context) error {
				go func() {
					log.Info("fiber listening", zap.String("addr", cfg.Addr))
					if err := app.Fiber.Listen(cfg.Addr); err != nil && err != fiber.ErrGracefulClose {
						log.Error("fiber listen", zap.Error(err))
					}
				}()
				return nil
			},
			OnStop: func(ctx context.Context) error {
				return app.Fiber.ShutdownWithContext(ctx)
			},
		})
	}),
)

// GRPCModule (optional) provides *grpc.Server.
var GRPCModule = fx.Module("scaffold-grpc",
	fx.Invoke(func(lc fx.Lifecycle, opts FxOpts, cfg *Config, log *zap.Logger) error {
		if opts.GRPC == nil {
			return nil
		}
		stop, err := startGRPC(*opts.GRPC, cfg, log)
		if err != nil {
			return err
		}
		lc.Append(fx.Hook{
			OnStop: func(_ context.Context) error {
				stop()
				return nil
			},
		})
		return nil
	}),
)

// CoreModule 总组装 — 标准 scaffold 全包.
var CoreModule = fx.Options(
	ConfigModule,
	LoggerModule,
	OTelModule,
	DBModule,
	FiberModule,
	GRPCModule,
)

// ── 业务侧入口 ──

// FxOpts 跟 Opts 等价, 但走 fx DI 路径.
type FxOpts struct {
	ServiceName  string
	ConfigPath   string
	MigrationDir string
	Models       []any
	OpenAPISpec  []byte
	OpenAPIInfo  OpenAPIInfo
	GRPC         *GRPCOpts

	// 业务侧 Modules — 装 repo / service / handler. fx.Provide / fx.Invoke / fx.Module 任意.
	AppModules []fx.Option

	// 可选: 跳过 default modules (替换 logger / db etc. 测试用)
	Decorations []fx.Option
}

// RunFx 真正 main 入口. 跟 Run() 行为等价, 内部用 fx.
func RunFx(opts FxOpts) {
	if opts.ServiceName == "" {
		panic("scaffold.RunFx: ServiceName required")
	}

	// migrate 子命令 — fx 不需要起
	if opts.MigrationDir != "" && len(os.Args) > 1 && os.Args[1] == "migrate" {
		// 跑 migrate 不走 fx, 直接 quick path
		cfg, err := LoadConfig(opts.ConfigPath)
		if err == nil {
			cfg.ServiceName = opts.ServiceName
			log := NewLogger(*cfg)
			runMigrate(MigrationConfig{Dir: opts.MigrationDir}, cfg.DB, log, os.Args[2:])
		}
		return
	}

	app := fx.New(
		// 把 opts 当 dep 注入, 各 module 用
		fx.Supply(opts),
		// 标准 scaffold modules
		CoreModule,
		// 业务侧 Modules
		fx.Options(opts.AppModules...),
		// Decorations (test override)
		fx.Options(opts.Decorations...),
		// 短启停超时 (大部分场景 ≤ 5s)
		fx.StartTimeout(30*time.Second),
		fx.StopTimeout(30*time.Second),
	)

	// fx.Run 是阻塞: Start → 等 signal → Stop
	app.Run()
}
