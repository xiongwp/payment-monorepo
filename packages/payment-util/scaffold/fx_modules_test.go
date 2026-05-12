package scaffold

import (
	"context"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// TestConfigModule_LoadsDefaults — Config 注入正确, 默认填到位.
func TestConfigModule_LoadsDefaults(t *testing.T) {
	var cfg *Config
	app := fxtest.New(t,
		fx.Supply(FxOpts{ServiceName: "test-svc", ConfigPath: ""}),
		ConfigModule,
		fx.Populate(&cfg),
	)
	defer app.RequireStart().RequireStop()

	if cfg.ServiceName != "test-svc" {
		t.Errorf("ServiceName = %q, want test-svc", cfg.ServiceName)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080 (default)", cfg.Addr)
	}
}

// TestLoggerModule — Logger 拿 cfg 构造正确.
func TestLoggerModule_BuildsZapLogger(t *testing.T) {
	var log *zap.Logger
	app := fxtest.New(t,
		fx.Supply(FxOpts{ServiceName: "test-svc"}),
		ConfigModule,
		LoggerModule,
		fx.Populate(&log),
	)
	defer app.RequireStart().RequireStop()

	if log == nil {
		t.Fatal("logger nil")
	}
	log.Info("hello from fxtest")
}

// TestDecorate_ReplacesLogger — fx.Decorate 用 mock logger 替换.
//
// 这是 fx 最强的卖点 — 业务代码无变化, 测试时只换一个 dep.
func TestDecorate_ReplacesLogger(t *testing.T) {
	mockLogger := zaptest.NewLogger(t)

	var got *zap.Logger
	app := fxtest.New(t,
		fx.Supply(FxOpts{ServiceName: "test-svc"}),
		ConfigModule,
		LoggerModule,
		// 替换默认 logger 为 mock (生产代码无感知)
		fx.Decorate(func() *zap.Logger { return mockLogger }),
		fx.Populate(&got),
	)
	defer app.RequireStart().RequireStop()

	if got != mockLogger {
		t.Error("logger not replaced by Decorate")
	}
}

// TestLifecycle_HookFires — fx.Lifecycle hook 启停跑了.
func TestLifecycle_HookFires(t *testing.T) {
	started := false
	stopped := false

	app := fxtest.New(t,
		fx.Supply(FxOpts{ServiceName: "test-svc"}),
		ConfigModule,
		LoggerModule,
		fx.Invoke(func(lc fx.Lifecycle) {
			lc.Append(fx.Hook{
				OnStart: func(_ context.Context) error { started = true; return nil },
				OnStop:  func(_ context.Context) error { stopped = true; return nil },
			})
		}),
	)

	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Error("OnStart never fired")
	}

	if err := app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Error("OnStop never fired")
	}
}

// TestMissingDep_FailsFast — fx 启动时检查依赖图; missing dep 立即 fail.
func TestMissingDep_FailsFast(t *testing.T) {
	// 业务依赖一个没人 Provide 的类型 — fx 应当 Start 失败
	type missing struct{}

	app := fx.New(
		fx.Supply(FxOpts{ServiceName: "test-svc"}),
		ConfigModule,
		LoggerModule,
		fx.Invoke(func(_ missing) {}), // 没人 Provide missing → 失败
		fx.NopLogger,                  // 静默 fx 内部日志
	)
	err := app.Err()
	if err == nil {
		t.Error("expected missing dep error, got nil")
	}
}

// TestFxOptsInjection — FxOpts 通过 fx.Supply 注入到所有 module.
func TestFxOptsInjection(t *testing.T) {
	var opts FxOpts
	app := fxtest.New(t,
		fx.Supply(FxOpts{ServiceName: "my-svc", ConfigPath: "/etc/cfg.yaml"}),
		fx.Populate(&opts),
	)
	defer app.RequireStart().RequireStop()

	if opts.ServiceName != "my-svc" {
		t.Errorf("ServiceName = %q", opts.ServiceName)
	}
}

// TestModuleComposition_AllStandard — 标准 CoreModule 所有 dep 都能拿到.
//
// 不实际 listen (fiber start hook 会 panic 因为没 free port; 我们用 NopLogger 静默).
func TestModuleComposition_AllStandard(t *testing.T) {
	var (
		cfg *Config
		log *zap.Logger
	)
	app := fxtest.New(t,
		fx.Supply(FxOpts{ServiceName: "compose-test"}),
		ConfigModule,
		LoggerModule,
		// 不装 DB / Fiber / GRPC — 太重 + 需要外部资源
		fx.Populate(&cfg, &log),
	)
	defer app.RequireStart().RequireStop()

	if cfg == nil || log == nil {
		t.Fatal("composed deps nil")
	}
}

// TestStartTimeout — 慢启动 hook 触发 timeout.
func TestStartTimeout(t *testing.T) {
	app := fx.New(
		fx.Supply(FxOpts{ServiceName: "slow"}),
		ConfigModule,
		LoggerModule,
		fx.Invoke(func(lc fx.Lifecycle) {
			lc.Append(fx.Hook{
				OnStart: func(ctx context.Context) error {
					// 模拟 2s 启动
					select {
					case <-time.After(2 * time.Second):
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			})
		}),
		fx.StartTimeout(100*time.Millisecond), // 短超时
		fx.NopLogger,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := app.Start(ctx); err == nil {
		t.Error("expected timeout error")
	}
}
