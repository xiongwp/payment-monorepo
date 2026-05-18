// providers.go — split-payment fx 装配根 (跟 order-core / accounting-system / card-center 同款风格).
//
// 现状 (FX-1 stage):
//   - 已就绪: Config / Logger / LogLevel / DB / AccountingConn / AccountingGRPCClient Provider
//   - 未切换: main.go 仍走过程式启动 (Module 当前未被 fx.New 引用)
//   - 后续阶段: Engine / Repos / Workers / gRPCServer 逐步切进 Module, 最后把 main() 替换成
//     fx.New(Module).Run() (跟 order-core/cmd/server/main.go:44 同款形态).
//
// 切换策略 (跟用户对齐 "保持系统的代码风格一致"):
//   - 不破坏现有 main.go 过程式启动 (备份过渡期共存)
//   - 每个 Provider 加进来都可独立编译 + lint
//   - 阶段完成才删除对应过程式代码段
//
// 命名: fx Provider 函数全部以 newXxx 开头 (跟 order-core / accounting-system 一致); 跟
// main.go 原有 helper (parseLogLevel / buildClientCreds 等) 区分开, 避免符号冲突.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"reconcile-system/packages/split-payment/internal/clients"
	"reconcile-system/packages/split-payment/internal/config"

	"github.com/xiongwp/payment-util/serviceregistry"
)

// Module — split-payment fx 装配根. 当前 stage 1+2, 后续逐步扩.
//
// 用法 (FX-4 完成后):
//
//	func main() {
//	    fx.New(Module).Run()
//	}
//
// fxevent 走 zap 跟其它服务保持一致的启动日志格式 (跟 accounting-system/cmd/server/main.go 同款).
var Module = fx.Options(
	fx.Provide(
		newCfg,
		newLogLevelFx,
		newLoggerFx,
		newDBFx,
		newAccountingConnFx,
		newAccountingGRPCClientFx,
	),
	fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
		return &fxevent.ZapLogger{Logger: log.Named("fx")}
	}),
	// 后续阶段在此追加:
	//   fx.Provide(newGraphRepo, newRunRepo, newEngine, newRiskGate, newEventPub, ...)
	//   fx.Invoke(startGRPCServer, startOutboxWorker, startPayoutCron, ...)
)

// ─── stage 1 Providers: 配置 + 日志 ────────────────────────────────────

// newCfg 启动期一次性加载 yaml + env override (见 internal/config).
//
// 跟 main.go 当前 config.Load("") 行为一致, 但暴露成 fx Provider 让 Logger / DB / Engine 等
// 下游 Provider 注入 *config.Config.
func newCfg() (*config.Config, error) {
	return config.Load("")
}

// newLogLevelFx 暴露 AtomicLevel 给 /admin/log-level 端点动态调级.
//
// 单独 Provider 让其它需要"实时切日志级别"的 component (gRPC interceptor / worker / admin HTTP)
// 可以注入这个 AtomicLevel.
func newLogLevelFx(cfg *config.Config) zap.AtomicLevel {
	return parseLogLevel(cfg.Log.Level)
}

// newLoggerFx zap.NewProductionConfig + Level 来自 cfg, 跟现有 main.go 行为一致.
//
// fx.Lifecycle.OnStop 注册 Sync() — 让 zap 在 SIGTERM 时把缓冲日志落盘.
func newLoggerFx(level zap.AtomicLevel, lc fx.Lifecycle) (*zap.Logger, error) {
	logCfg := zap.NewProductionConfig()
	logCfg.Level = level
	logger, err := logCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("zap build: %w", err)
	}
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			_ = logger.Sync()
			return nil
		},
	})
	return logger, nil
}

// ─── stage 2 Providers: DB + accounting gRPC client ─────────────────────

// newDBFx 启动期 Open + Ping MySQL; pool 参数全走 cfg.Database (yaml + env override).
//
// 行为对齐当前 main.go: cfg.Database.DSN 空 → 返 (nil, nil), 调用方根据 nil 切 memory 路径
// (跟 SP-AC-7 一致, repos / saga / outbox / cron lease 都 db == nil 时退化).
//
// 跟 main.go 当前实现的区别: lifecycle OnStop 注册 Close() — fx 退出时优雅关连接池.
func newDBFx(cfg *config.Config, log *zap.Logger, lc fx.Lifecycle) (*sql.DB, error) {
	dsn := cfg.Database.DSN
	if dsn == "" {
		log.Info("database.dsn empty — split-payment 走 memory 模式 (单进程, 重启丢 graph)")
		return nil, nil
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Error("open mysql failed",
			zap.String("dsn_host", maskDSN(dsn)), zap.Error(err))
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	db.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	lifetime := cfg.Database.ConnMaxLifetime
	if lifetime <= 0 {
		lifetime = 30 * time.Minute // 跟 setDefaults 同步 fallback.
	}
	db.SetConnMaxLifetime(lifetime)

	// 启动期 ping — fail-fast 让 fx.New 直接报错而不是 worker 静默挂.
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		log.Error("mysql ping failed",
			zap.String("dsn_host", maskDSN(dsn)), zap.Error(err))
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	log.Info("split-payment DB pool sized + ping OK",
		zap.String("dsn_host", maskDSN(dsn)),
		zap.Int("max_open", cfg.Database.MaxOpenConns),
		zap.Int("max_idle", cfg.Database.MaxIdleConns),
		zap.Duration("conn_max_lifetime", lifetime))

	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			if err := db.Close(); err != nil {
				log.Warn("mysql close error on shutdown", zap.Error(err))
				return err
			}
			return nil
		},
	})
	return db, nil
}

// newAccountingConnFx dial accounting-system gRPC (mTLS or insecure dev).
//
// SP-AC-7 L2+X2: 走 serviceregistry.DialWithFallback 拿一组 hardenedOptions:
//   - round_robin LB (多副本 accounting-service 真均摊)
//   - 幂等 RPC 自动重试瞬态 UNAVAILABLE / DEADLINE_EXCEEDED
//   - HTTP/2 keepalive 10s+3s 探活
//
// registry.endpoints (etcd) 配了走真服务发现; 没配则降级直连 fallback addr (dev).
//
// lifecycle OnStop 注册 conn.Close() — 退出时优雅断流, 避免上游 ELB 半开连接.
func newAccountingConnFx(cfg *config.Config, log *zap.Logger, lc fx.Lifecycle) (*grpc.ClientConn, error) {
	clientCreds, err := buildClientCreds(log)
	if err != nil {
		log.Error("build mTLS client credentials failed", zap.Error(err))
		return nil, fmt.Errorf("buildClientCreds: %w", err)
	}
	conn, err := serviceregistry.DialWithFallback(
		cfg.Registry.Endpoints,
		"accounting-service",
		cfg.Accounting.GRPCAddr,
		clientCreds,
	)
	if err != nil {
		log.Error("dial accounting gRPC failed",
			zap.String("fallback_addr", cfg.Accounting.GRPCAddr),
			zap.Strings("registry_endpoints", cfg.Registry.Endpoints),
			zap.Error(err))
		return nil, fmt.Errorf("dial accounting: %w", err)
	}
	log.Info("accounting gRPC connection established",
		zap.String("fallback_addr", cfg.Accounting.GRPCAddr),
		zap.Int("registry_endpoint_count", len(cfg.Registry.Endpoints)))

	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			if err := conn.Close(); err != nil {
				log.Warn("accounting gRPC conn close error", zap.Error(err))
				return err
			}
			return nil
		},
	})
	return conn, nil
}

// newAccountingGRPCClientFx 把 accounting *grpc.ClientConn 包装成业务 client.
//
// 业务路径 (engine.AccountingMeta + grpcsvc.AccountingMetaCaller) 通过 accountingGRPCAdapter
// 把 *clients.AccountingGRPCClient 适配到 workflow.AccountingMetaCaller — 现已在
// main.go 用过程式方式构造, 后续 stage 3 把 engine 装进 Module 时这层 Provider 自然就被注入.
func newAccountingGRPCClientFx(conn *grpc.ClientConn) *clients.AccountingGRPCClient {
	if conn == nil {
		return nil
	}
	return clients.NewAccountingGRPCClient(conn)
}
