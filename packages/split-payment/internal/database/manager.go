// Package database — split-payment 分库连接管理
//
// 双库架构（跟 accounting-system 风格对齐）：
//   - metaDB        split_payment_meta 库（moneyflow_graphs / versions /
//                   connected_accounts / cron_lease，全局元数据，不分片）
//   - shardDBs[10]  split_payment_db_0 .. split_payment_db_9（高频流水分片）
//
// Repo 层不直接持有 *sql.DB；持有 *Manager，每次操作先 Manager.Meta() 或
// Manager.Shard(router.RouteByString(key).dbIdx) 拿对应连接，再拼分片表名。
package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// DBConfig 单条连接配置。
type DBConfig struct {
	Name            string        `mapstructure:"name"`
	DSN             string        `mapstructure:"dsn"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
}

// Config 顶层数据库配置。
type Config struct {
	MetaDB    DBConfig   `mapstructure:"meta_database"`
	ShardDBs  []DBConfig `mapstructure:"databases"`
}

// Manager 数据库管理器。
type Manager struct {
	meta     *sql.DB
	shards   []*sql.DB
}

// NewManager 用配置打开 1 个 metaDB + N 个 shardDB（要求 len(cfg.ShardDBs) == 10
// 跟 router 默认 dbCount 对齐；不一致返错避免静默错路由）。
func NewManager(ctx context.Context, cfg Config) (*Manager, error) {
	if cfg.MetaDB.DSN == "" {
		return nil, fmt.Errorf("meta_database.dsn required")
	}
	if len(cfg.ShardDBs) == 0 {
		return nil, fmt.Errorf("databases (shard list) required")
	}
	if len(cfg.ShardDBs) != 10 {
		return nil, fmt.Errorf("databases 必须正好 10 个（跟 sharding.Router dbCount 对齐），got %d", len(cfg.ShardDBs))
	}

	meta, err := open(ctx, cfg.MetaDB)
	if err != nil {
		return nil, fmt.Errorf("open meta %s: %w", cfg.MetaDB.Name, err)
	}

	shards := make([]*sql.DB, len(cfg.ShardDBs))
	for i, sc := range cfg.ShardDBs {
		db, err := open(ctx, sc)
		if err != nil {
			// 清理已开的连接
			_ = meta.Close()
			for j := 0; j < i; j++ {
				_ = shards[j].Close()
			}
			return nil, fmt.Errorf("open shard[%d] %s: %w", i, sc.Name, err)
		}
		shards[i] = db
	}

	return &Manager{meta: meta, shards: shards}, nil
}

func open(ctx context.Context, cfg DBConfig) (*sql.DB, error) {
	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		return nil, err
	}
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	// 启动期 ping 一次，失败立即报错（不要等到第一次 query 才发现 dsn 拼错）
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return db, nil
}

// Meta 拿 metaDB 连接（moneyflow_graphs / versions / cron_lease / connected_accounts）。
func (m *Manager) Meta() *sql.DB { return m.meta }

// Shard 拿第 dbIdx 个 shard 连接（流水表）。dbIdx 必须 in [0, len(shards))。
func (m *Manager) Shard(dbIdx int) *sql.DB {
	if dbIdx < 0 || dbIdx >= len(m.shards) {
		// 超界打 panic 而不是 silently 路由错 —— 调用方传错 dbIdx 是 logic bug
		panic(fmt.Sprintf("shard index %d out of range [0, %d)", dbIdx, len(m.shards)))
	}
	return m.shards[dbIdx]
}

// ShardCount 返回 shard 数（一般 = 10）。
func (m *Manager) ShardCount() int { return len(m.shards) }

// AllShards 返回全部 shard 连接列表，便于跨 shard scan（outbox worker 之类）。
func (m *Manager) AllShards() []*sql.DB { return m.shards }

// Close 关全部连接。
func (m *Manager) Close() error {
	var firstErr error
	if m.meta != nil {
		if err := m.meta.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, s := range m.shards {
		if s != nil {
			if err := s.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
