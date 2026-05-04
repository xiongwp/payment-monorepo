package database

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// DBConfig 数据库配置
type DBConfig struct {
	Name               string `mapstructure:"name"`
	DSN                string `mapstructure:"dsn"`
	ReadDSN            string `mapstructure:"read_dsn"`           // 只读副本 DSN（可选）
	MaxOpenConns       int    `mapstructure:"max_open_conns"`
	MaxIdleConns       int    `mapstructure:"max_idle_conns"`
	ConnMaxLifetime    int    `mapstructure:"conn_max_lifetime"`
	ConnMaxIdleTime    int    `mapstructure:"conn_max_idle_time"` // 空闲连接最长保留时间（秒），0 表示不设置
}

// Manager 数据库管理器
//
// 双库架构：
//   - metaDB      account_meta 库（全局元数据：account_type_info、transaction_rule，不分片）
//   - databases   accounting_db_0 ~ accounting_db_9（按路由规则分库分表）
type Manager struct {
	metaDB        *gorm.DB   // account_meta 写主库
	metaReadDB    *gorm.DB   // account_meta 只读副本（未配置时与 metaDB 相同）
	databases     []*gorm.DB // 分库写主库列表
	readDatabases []*gorm.DB // 分库只读副本（未配置时与主库相同）
}

// openConn 创建并配置单条 GORM 连接（含启动重试，应对 Docker 中 MySQL 慢启动）
// gormLog 为可选 GORM 日志器；传 nil 时使用 GORM 默认（Silent，不输出 SQL 日志）。
func openConn(cfg DBConfig, gormLog gormlogger.Interface) (*gorm.DB, error) {
	gormCfg := &gorm.Config{PrepareStmt: true, SkipDefaultTransaction: true}
	if gormLog != nil {
		gormCfg.Logger = gormLog
	}
	db, err := gorm.Open(mysql.Open(cfg.DSN), gormCfg)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.Name, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB for %s: %w", cfg.Name, err)
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Second)
	}
	// ConnMaxIdleTime 控制空闲连接在池中保留的最长时间。
	// 数据库重启后，旧的空闲连接已断开但仍在池中，下次使用时会收到 "connection reset by peer"。
	// 设置此值后，database/sql 会主动丢弃超期空闲连接，保证重连时获取到新的有效连接。
	// 默认 10 分钟（若配置 0 则使用默认值）。
	idleTimeout := time.Duration(cfg.ConnMaxIdleTime) * time.Second
	if idleTimeout <= 0 {
		idleTimeout = 10 * time.Minute
	}
	sqlDB.SetConnMaxIdleTime(idleTimeout)

	// 指数退避重试 Ping（最多 6 次，共等待约 63 秒）
	// 应对 Docker Compose 中 MySQL 容器比应用容器启动慢的场景
	const maxRetries = 6
	backoff := time.Second
	for i := 0; i < maxRetries; i++ {
		if err = sqlDB.Ping(); err == nil {
			return db, nil
		}
		if i < maxRetries-1 {
			log.Printf("[database] ping %s failed (attempt %d/%d), retrying in %s: %v",
				cfg.Name, i+1, maxRetries, backoff, err)
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("ping %s after %d attempts: %w", cfg.Name, maxRetries, err)
}

// NewManager 创建分库管理器（向后兼容，无独立 account_meta 库时使用 db[0] 代替）
// gormLog 可为 nil（不记录 SQL 日志），建议生产环境传入 logging.GormLogger。
func NewManager(configs []DBConfig, gormLog gormlogger.Interface) (*Manager, error) {
	mgr, err := buildShardManager(configs, gormLog)
	if err != nil {
		return nil, err
	}
	// 兼容旧行为：meta DB 指向第一个分库
	mgr.metaDB = mgr.databases[0]
	mgr.metaReadDB = mgr.readDatabases[0]
	return mgr, nil
}

// NewManagerWithMeta 创建带独立元数据库的管理器
//
//	metaCfg — account_meta 库配置（存放 account_type_info / transaction_rule）
//	shards  — accounting_db_0 ~ accounting_db_N 配置列表
//	gormLog — GORM 日志器（nil = 不记录 SQL 日志）
func NewManagerWithMeta(metaCfg DBConfig, shards []DBConfig, gormLog gormlogger.Interface) (*Manager, error) {
	mgr, err := buildShardManager(shards, gormLog)
	if err != nil {
		return nil, err
	}

	metaDB, err := openConn(metaCfg, gormLog)
	if err != nil {
		return nil, fmt.Errorf("meta db: %w", err)
	}
	metaReadDB := metaDB
	if metaCfg.ReadDSN != "" {
		gormCfg := &gorm.Config{PrepareStmt: true, SkipDefaultTransaction: true}
		if gormLog != nil {
			gormCfg.Logger = gormLog
		}
		rdb, err := gorm.Open(mysql.Open(metaCfg.ReadDSN), gormCfg)
		if err != nil {
			return nil, fmt.Errorf("meta read replica: %w", err)
		}
		sqlRDB, _ := rdb.DB()
		sqlRDB.SetMaxOpenConns(metaCfg.MaxOpenConns)
		sqlRDB.SetMaxIdleConns(metaCfg.MaxIdleConns)
		sqlRDB.SetConnMaxLifetime(time.Duration(metaCfg.ConnMaxLifetime) * time.Second)
		metaReadDB = rdb
	}

	mgr.metaDB = metaDB
	mgr.metaReadDB = metaReadDB
	return mgr, nil
}

// buildShardManager 内部：仅初始化分库连接列表
func buildShardManager(configs []DBConfig, gormLog gormlogger.Interface) (*Manager, error) {
	databases := make([]*gorm.DB, len(configs))
	readDatabases := make([]*gorm.DB, len(configs))

	for i, cfg := range configs {
		db, err := openConn(cfg, gormLog)
		if err != nil {
			return nil, err
		}
		databases[i] = db

		if cfg.ReadDSN == "" {
			readDatabases[i] = db
			continue
		}
		gormCfg := &gorm.Config{PrepareStmt: true, SkipDefaultTransaction: true}
		if gormLog != nil {
			gormCfg.Logger = gormLog
		}
		rdb, err := gorm.Open(mysql.Open(cfg.ReadDSN), gormCfg)
		if err != nil {
			return nil, fmt.Errorf("read replica %s: %w", cfg.Name, err)
		}
		sqlRDB, err := rdb.DB()
		if err != nil {
			return nil, fmt.Errorf("get read replica sql.DB for %s: %w", cfg.Name, err)
		}
		sqlRDB.SetMaxOpenConns(cfg.MaxOpenConns)
		sqlRDB.SetMaxIdleConns(cfg.MaxIdleConns)
		sqlRDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Second)
		readDatabases[i] = rdb
	}

	return &Manager{
		databases:     databases,
		readDatabases: readDatabases,
	}, nil
}

// GetMetaDB 获取 account_meta 元数据库写主库
func (m *Manager) GetMetaDB() (*gorm.DB, error) {
	if m.metaDB == nil {
		return nil, fmt.Errorf("meta db not configured")
	}
	return m.metaDB, nil
}

// GetMetaReadDB 获取 account_meta 只读副本（无副本时返回主库）
func (m *Manager) GetMetaReadDB() (*gorm.DB, error) {
	if m.metaReadDB != nil {
		return m.metaReadDB, nil
	}
	return m.GetMetaDB()
}

// GetDB 获取指定索引的分库写主库
func (m *Manager) GetDB(index int) (*gorm.DB, error) {
	if index < 0 || index >= len(m.databases) {
		return nil, fmt.Errorf("invalid database index: %d", index)
	}
	return m.databases[index], nil
}

// GetReadDB 获取分库只读副本（无副本时返回主库）
func (m *Manager) GetReadDB(index int) (*gorm.DB, error) {
	if index < 0 || index >= len(m.readDatabases) {
		return nil, fmt.Errorf("invalid database index: %d", index)
	}
	return m.readDatabases[index], nil
}

// GetAllDBs 获取所有分库连接
func (m *Manager) GetAllDBs() []*gorm.DB {
	return m.databases
}

// SQLDBs 返回每个分片底层 *sql.DB，按 "shard0", "shard1", ... 命名。
// 给 healthx.NewMultiDBStatsCollector 用，暴露 Prometheus 池指标。
// metaDB 也包含进来，命名 "meta"。
//
// 任一 DB 取 *sql.DB 失败时跳过该 entry（不阻塞启动；指标少一条总比启动崩溃好）。
func (m *Manager) SQLDBs() map[string]*sql.DB {
	out := make(map[string]*sql.DB, len(m.databases)+1)
	for i, gdb := range m.databases {
		if gdb == nil {
			continue
		}
		sqlDB, err := gdb.DB()
		if err != nil || sqlDB == nil {
			continue
		}
		out[fmt.Sprintf("shard%d", i)] = sqlDB
	}
	if m.metaDB != nil {
		if sqlDB, err := m.metaDB.DB(); err == nil && sqlDB != nil {
			out["meta"] = sqlDB
		}
	}
	return out
}

// Close 关闭所有数据库连接
func (m *Manager) Close() error {
	// 关闭独立 meta DB（避免与 db[0] 重复关闭）
	if m.metaDB != nil && (len(m.databases) == 0 || m.metaDB != m.databases[0]) {
		if sqlDB, err := m.metaDB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	for i, db := range m.databases {
		sqlDB, err := db.DB()
		if err != nil {
			return fmt.Errorf("get sql.DB for shard %d: %w", i, err)
		}
		if err := sqlDB.Close(); err != nil {
			return fmt.Errorf("close shard %d: %w", i, err)
		}
	}
	return nil
}

// DBCount 获取分库数量
func (m *Manager) DBCount() int {
	return len(m.databases)
}

// PingPrimary 健康探针用：只 ping meta DB（即"主写库"），不轮询所有 100 个分库。
//
// 设计取舍：
//   - readiness 探针默认每 10s 触发一次。如果 ping 全部 10 个分库 → 100 次 ping/分钟。
//     高吞吐场景 meta DB 故障率 ≈ 分库故障率（共享 MySQL 集群），ping 一个就够代表性。
//   - 给 ctx 短超时（300ms），probe 不能因为 DB 慢而拖慢自己。
func (m *Manager) PingPrimary(ctx context.Context) error {
	if m.metaDB == nil {
		return fmt.Errorf("meta db not configured")
	}
	sqlDB, err := m.metaDB.DB()
	if err != nil {
		return fmt.Errorf("get sql.DB: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	return sqlDB.PingContext(pingCtx)
}

// WarmPools 启动时预热所有连接池：开少量持续连接到每个池中，让 idle slot 有内容，
// 避免首批请求每条都付 TCP+鉴权延迟。
//
// 重要边界：
//   - 不再"开满 MaxOpenConns"。曾经的实现一次性 ping 50 conns × (10 shards + meta) =
//     550 个并发连接；MySQL 默认 max_connections=151，loadtest 启动瞬间就把它打爆，
//     之后所有正常连接都报 "Too many connections"。
//   - 改为开 perPoolWarm = min(8, MaxOpenConns/4)，并发上限 perPoolWarm，阶梯式分库执行，
//     总并发 ≤ perPoolWarm 而非 N_pools × MaxOpenConns。
//   - 单 ping 失败仅 Warn，不中断；ctx 截止仍尽可能多地完成已发的 ping。
func (m *Manager) WarmPools(ctx context.Context) {
	const maxPerPool = 8 // 上限：单 DB 最多预热多少个连接（MaxIdleConns 实际有效值往往就 ~8-20）

	warm := func(name string, gdb *gorm.DB) {
		sqlDB, err := gdb.DB()
		if err != nil {
			log.Printf("[database] warm %s: get sql.DB failed: %v", name, err)
			return
		}
		stats := sqlDB.Stats()
		target := maxPerPool
		if stats.MaxOpenConnections > 0 && stats.MaxOpenConnections/4 < target {
			// 不超过 1/4 的 MaxOpen，给业务流量留余量
			target = stats.MaxOpenConnections / 4
			if target < 1 {
				target = 1
			}
		}
		warmConcurrent(ctx, sqlDB, target, name)
	}

	// 串行遍历分库（不再为所有库同时开连接），每个库内部并发开 target 条。
	// 这样总并发 ≈ target，即使有 100 个库也安全。代价是启动时间略长（预热是异步 goroutine 调用，
	// 不阻塞 fx OnStart）。
	for i, gdb := range m.databases {
		if ctx.Err() != nil {
			return
		}
		warm(fmt.Sprintf("shard-%d", i), gdb)
	}
	if m.metaDB != nil && (len(m.databases) == 0 || m.metaDB != m.databases[0]) {
		if ctx.Err() == nil {
			warm("meta", m.metaDB)
		}
	}
}

// warmConcurrent 在单个 *sql.DB 上并发开 n 条 ping 连接然后归还。
// n 应当远小于 MySQL max_connections / N_dbs，否则 loadtest 启动时就把 server 打爆。
func warmConcurrent(ctx context.Context, sqlDB *sql.DB, n int, name string) {
	if n <= 0 {
		return
	}
	var wg sync.WaitGroup
	wg.Add(n)
	start := time.Now()
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := sqlDB.PingContext(pingCtx); err != nil {
				log.Printf("[database] warm %s: ping failed: %v", name, err)
			}
		}()
	}
	wg.Wait()
	log.Printf("[database] warm %s: %d conns warmed in %s", name, n, time.Since(start))
}
