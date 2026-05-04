// Package repo 定义仓储接口与基础设施（DB Manager + 分片仓储），与 order-core
// 完全同构。
package repo

import (
	"fmt"
	"log"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var sqlLogger *zap.Logger

func SetSQLLogger(z *zap.Logger) { sqlLogger = z }

type DBConfig struct {
	Name            string `mapstructure:"name"`
	DSN             string `mapstructure:"dsn"`
	MaxOpenConns    int    `mapstructure:"max_open_conns"`
	MaxIdleConns    int    `mapstructure:"max_idle_conns"`
	ConnMaxLifetime int    `mapstructure:"conn_max_lifetime"`
}

type Manager struct {
	shards []*gorm.DB
	meta   *gorm.DB
}

func NewManager(shards []DBConfig) (*Manager, error) {
	if len(shards) == 0 {
		return nil, fmt.Errorf("repo: at least one shard required")
	}
	dbs, err := openShards(shards)
	if err != nil {
		return nil, err
	}
	return &Manager{shards: dbs}, nil
}

func NewManagerWithMeta(meta DBConfig, shards []DBConfig) (*Manager, error) {
	mgr, err := NewManager(shards)
	if err != nil {
		return nil, err
	}
	if meta.DSN != "" {
		mdb, err := openConn(meta)
		if err != nil {
			return nil, fmt.Errorf("meta db: %w", err)
		}
		mgr.meta = mdb
	}
	return mgr, nil
}

func (m *Manager) GetShard(idx int) (*gorm.DB, error) {
	if idx < 0 || idx >= len(m.shards) {
		return nil, fmt.Errorf("repo: invalid shard index %d (have %d)", idx, len(m.shards))
	}
	return m.shards[idx], nil
}

func (m *Manager) AllShards() []*gorm.DB { return m.shards }

func (m *Manager) GetMeta() *gorm.DB {
	if m.meta != nil {
		return m.meta
	}
	return m.shards[0]
}

func (m *Manager) ShardCount() int { return len(m.shards) }

func (m *Manager) Close() error {
	if m.meta != nil {
		if sqlDB, err := m.meta.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	for _, db := range m.shards {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	return nil
}

func openShards(cfgs []DBConfig) ([]*gorm.DB, error) {
	out := make([]*gorm.DB, len(cfgs))
	for i, c := range cfgs {
		db, err := openConn(c)
		if err != nil {
			return nil, err
		}
		out[i] = db
	}
	return out, nil
}

func openConn(cfg DBConfig) (*gorm.DB, error) {
	gormCfg := &gorm.Config{
		// PrepareStmt 在进程内缓存每条 SQL 的 prepared statement，MySQL 不用反复
		// 解析 SQL 文本；对 acquirer_tx 这种每请求都写的热表收益最大。实测在
		// 本机 MySQL 下可以省 15-25% 的单条写入耗时（纯 SQL 解析 + 计划时间）。
		PrepareStmt: true,
		// SkipDefaultTransaction 关掉 Create/Update 的隐式 tx（我们自己编排事务）。
		SkipDefaultTransaction: true,
	}
	if sqlLogger != nil {
		gormCfg.Logger = NewZapGormLogger(sqlLogger.With(zap.String("db", cfg.Name)))
	}
	// Retry gorm.Open itself: DNS failures at startup (e.g. docker-compose
	// where the shard container is still registering its DNS entry) surface
	// as "lookup foo on 127.0.0.11:53: no such host" from the mysql driver's
	// eager dial. Without this loop, payment-channel crashes instead of
	// waiting a few seconds for Docker DNS to converge.
	var db *gorm.DB
	var err error
	const openRetries = 8
	openBackoff := 500 * time.Millisecond
	for i := 0; i < openRetries; i++ {
		db, err = gorm.Open(mysql.Open(cfg.DSN), gormCfg)
		if err == nil {
			break
		}
		if i < openRetries-1 {
			log.Printf("[repo] open %s failed (%d/%d), retry in %s: %v", cfg.Name, i+1, openRetries, openBackoff, err)
			time.Sleep(openBackoff)
			if openBackoff < 8*time.Second {
				openBackoff *= 2
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.Name, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB %s: %w", cfg.Name, err)
	}
	// 合理默认值，即使 config 里没显式设置也不会踩默认 0 = unlimited 的坑。
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	} else {
		sqlDB.SetMaxOpenConns(100)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	} else {
		sqlDB.SetMaxIdleConns(50)
	}
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Second)
	} else {
		sqlDB.SetConnMaxLifetime(time.Hour)
	}
	// idle 连接最多空闲 10 分钟就被回收，避免 MySQL 主动 kill 后客户端还拿着坏连接。
	sqlDB.SetConnMaxIdleTime(10 * time.Minute)
	const maxRetries = 6
	backoff := time.Second
	for i := 0; i < maxRetries; i++ {
		if err = sqlDB.Ping(); err == nil {
			return db, nil
		}
		if i < maxRetries-1 {
			log.Printf("[repo] ping %s failed (%d/%d), retry in %s: %v", cfg.Name, i+1, maxRetries, backoff, err)
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("ping %s: %w", cfg.Name, err)
}
