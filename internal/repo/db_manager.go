// Package repo 定义仓储接口与基础设施（DB Manager + 分片仓储）。
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

// sqlLogger 由上层通过 SetSQLLogger 注入；未设置时 gorm 用默认 stdout logger
var sqlLogger *zap.Logger

// SetSQLLogger 在 cmd 启动期注入 zap；之后每次 openConn 都会把 zap logger 挂到 gorm 上
func SetSQLLogger(z *zap.Logger) { sqlLogger = z }

// DBConfig 单个 MySQL 实例配置
type DBConfig struct {
	Name            string `mapstructure:"name"`
	DSN             string `mapstructure:"dsn"`
	MaxOpenConns    int    `mapstructure:"max_open_conns"`
	MaxIdleConns    int    `mapstructure:"max_idle_conns"`
	ConnMaxLifetime int    `mapstructure:"conn_max_lifetime"` // 秒
}

// Manager 数据库管理器：N 个分库 + 1 个非分片 metaDB（可选）
type Manager struct {
	shards []*gorm.DB
	meta   *gorm.DB
}

// NewManager 仅分库
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

// NewManagerWithMeta 分库 + meta
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

// GetShard 取第 idx 个分库
func (m *Manager) GetShard(idx int) (*gorm.DB, error) {
	if idx < 0 || idx >= len(m.shards) {
		return nil, fmt.Errorf("repo: invalid shard index %d (have %d)", idx, len(m.shards))
	}
	return m.shards[idx], nil
}

// AllShards 所有分库
func (m *Manager) AllShards() []*gorm.DB { return m.shards }

// GetMeta meta 库；未配置时返回 shards[0] 兜底
func (m *Manager) GetMeta() *gorm.DB {
	if m.meta != nil {
		return m.meta
	}
	return m.shards[0]
}

// ShardCount 分库数
func (m *Manager) ShardCount() int { return len(m.shards) }

// Close 关闭所有连接
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
		// PrepareStmt 开启后 GORM 对每条 SQL 维护 server-side prepared handle，
		// 高频相同 SQL（热路径 payment_intent / charge 读写）不再每次 parse+plan，
		// MySQL 8 下 p99 延迟可下 15-30%。代价是 stmt cache 内存，sharded 100
		// 表下每连接 ~2-3MB，可忽略。
		PrepareStmt: true,
		// SkipDefaultTransaction 关掉 Create/Update 的隐式单条事务包装；我们
		// 所有需要事务的路径（PI FSM / accounting outbox / ledger）都显式
		// db.Transaction(...) 包装，隐式 tx 纯成本。
		SkipDefaultTransaction: true,
	}
	if sqlLogger != nil {
		gormCfg.Logger = NewZapGormLogger(sqlLogger.With(zap.String("db", cfg.Name)))
	}
	db, err := gorm.Open(mysql.Open(cfg.DSN), gormCfg)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.Name, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB %s: %w", cfg.Name, err)
	}
	// P1: 默认值兜底，防止 config 漏填导致无限连接
	maxOpen := cfg.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 100
	}
	maxIdle := cfg.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 50
	}
	maxLife := cfg.ConnMaxLifetime
	if maxLife <= 0 {
		maxLife = 3600
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(time.Duration(maxLife) * time.Second)
	// 启动重试 ping，应对 docker 慢启动
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
