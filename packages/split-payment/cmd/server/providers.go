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

	"github.com/xiongwp/split-payment/internal/clients"
	"github.com/xiongwp/split-payment/internal/config"
	"github.com/xiongwp/split-payment/internal/database"
	"github.com/xiongwp/split-payment/internal/sharding"
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
		newDBManagerFx, // DB-split: 1 meta + 10 shards
		newShardingRouterFx,
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

// newAccountingGRPCClientFx Kitex client → accounting-system TransactionService.
//
// 切 Kitex 后 Kitex 自管 connection pool + LB + keepalive, 不再需要 *grpc.ClientConn
// Provider. 老 newAccountingConnFx + buildClientCreds 全部移除 (mTLS 不要 — 内部 mesh).
//
// registry.endpoints (etcd) 配了的话, 未来切 kitexutil.NewEtcdResolver 做服务发现;
// 当前直接 client.WithHostPorts(fallback) 一条路径.
func newAccountingGRPCClientFx(cfg *config.Config, log *zap.Logger) (*clients.AccountingGRPCClient, error) {
	cli, err := clients.NewAccountingGRPCClient(cfg.Accounting.GRPCAddr)
	if err != nil {
		log.Error("kitex dial accounting failed",
			zap.String("addr", cfg.Accounting.GRPCAddr),
			zap.Strings("registry_endpoints", cfg.Registry.Endpoints),
			zap.Error(err))
		return nil, fmt.Errorf("dial accounting (kitex): %w", err)
	}
	log.Info("accounting Kitex client ready",
		zap.String("addr", cfg.Accounting.GRPCAddr),
		zap.Int("registry_endpoint_count", len(cfg.Registry.Endpoints)))
	// Kitex client 自带 connection pool, 无需 lifecycle close hook.
	return cli, nil
}

// ─── DB-split: Multi-shard manager + router ───────────────────────────

// newDBManagerFx 打开 1 个 metaDB + N 个 shardDB（DB-split 重构）。
//
// 配置：cfg.Database.MetaDB.DSN + cfg.Database.Shards[10]。
//
// 跟 newDBFx (legacy 单 DSN) 共存 —— 过渡期老 repo 还用 newDBFx 给的 *sql.DB，
// 新 repo (RunRepo / GraphRepo) 用 *database.Manager。
//
// MetaDB.DSN 空 → 返 nil（memory / 老配置模式）；其他重要错（shard 个数不对、
// 连接失败）直接 fail-fast。
func newDBManagerFx(cfg *config.Config, log *zap.Logger, lc fx.Lifecycle) (*database.Manager, error) {
	if cfg.Database.MetaDB.DSN == "" {
		log.Info("database.meta_database.dsn 空 — 跳过 Manager 构造 (走 legacy 单 DSN 或 memory)")
		return nil, nil
	}
	if len(cfg.Database.Shards) == 0 {
		log.Info("database.databases 空 — 跳过 Manager 构造")
		return nil, nil
	}

	// 把 config 类型转 database.Config（同 schema 不同 package）
	mgrCfg := database.Config{
		MetaDB: database.DBConfig{
			Name:            "split_payment_meta",
			DSN:             cfg.Database.MetaDB.DSN,
			MaxOpenConns:    cfg.Database.MetaDB.MaxOpenConns,
			MaxIdleConns:    cfg.Database.MetaDB.MaxIdleConns,
			ConnMaxLifetime: cfg.Database.MetaDB.ConnMaxLifetime,
		},
		ShardDBs: make([]database.DBConfig, len(cfg.Database.Shards)),
	}
	for i, s := range cfg.Database.Shards {
		mgrCfg.ShardDBs[i] = database.DBConfig{
			Name:            s.Name,
			DSN:             s.DSN,
			MaxOpenConns:    s.MaxOpenConns,
			MaxIdleConns:    s.MaxIdleConns,
			ConnMaxLifetime: s.ConnMaxLifetime,
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mgr, err := database.NewManager(ctx, mgrCfg)
	if err != nil {
		return nil, fmt.Errorf("split-payment DBManager init: %w", err)
	}

	log.Info("split-payment DBManager ready",
		zap.String("meta_dsn_host", maskDSN(cfg.Database.MetaDB.DSN)),
		zap.Int("shard_count", len(cfg.Database.Shards)))

	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			if err := mgr.Close(); err != nil {
				log.Warn("DBManager close error on shutdown", zap.Error(err))
				return err
			}
			return nil
		},
	})
	return mgr, nil
}

// newShardingRouterFx 默认 10×10 router（跟 accounting + 新 DDL 100 张全局表对齐）。
func newShardingRouterFx() *sharding.Router {
	return sharding.NewRouter()
}
