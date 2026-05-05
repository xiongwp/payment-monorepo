// Package repo 提供 card-center 的持久化层。10 库分片，跟其它服务同形。
package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/xiongwp/card-center/internal/sharding"
)

// DBConfig 单 MySQL 实例配置
type DBConfig struct {
	Name            string `mapstructure:"name"`
	DSN             string `mapstructure:"dsn"`
	MaxOpenConns    int    `mapstructure:"max_open_conns"`
	MaxIdleConns    int    `mapstructure:"max_idle_conns"`
	ConnMaxLifetime int    `mapstructure:"conn_max_lifetime"`
}

// Manager 1 meta + 10 shard
type Manager struct {
	meta   *gorm.DB
	shards []*gorm.DB
	router *sharding.Router
	logger *zap.Logger
}

// NewManager 构造
func NewManager(meta DBConfig, shards []DBConfig, router *sharding.Router, logger *zap.Logger) (*Manager, error) {
	if meta.DSN == "" {
		return nil, fmt.Errorf("repo: meta.dsn required")
	}
	if len(shards) != sharding.ShardDBCount {
		return nil, fmt.Errorf("repo: need %d shards, got %d", sharding.ShardDBCount, len(shards))
	}
	mDB, err := openConn(meta, logger)
	if err != nil {
		return nil, fmt.Errorf("meta: %w", err)
	}
	sDBs := make([]*gorm.DB, len(shards))
	for i, c := range shards {
		db, err := openConn(c, logger)
		if err != nil {
			return nil, fmt.Errorf("shard[%d]: %w", i, err)
		}
		sDBs[i] = db
	}
	return &Manager{meta: mDB, shards: sDBs, router: router, logger: logger}, nil
}

func (m *Manager) Meta() *gorm.DB              { return m.meta }
func (m *Manager) Shard(idx int) *gorm.DB      { return m.shards[idx] }
func (m *Manager) Router() *sharding.Router    { return m.router }
func (m *Manager) ShardCount() int             { return len(m.shards) }
func (m *Manager) AllShards() []*gorm.DB       { return m.shards }

func (m *Manager) Close() error {
	for _, db := range append([]*gorm.DB{m.meta}, m.shards...) {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	return nil
}

func openConn(cfg DBConfig, logger *zap.Logger) (*gorm.DB, error) {
	gormCfg := &gorm.Config{PrepareStmt: true}
	db, err := gorm.Open(mysql.Open(cfg.DSN), gormCfg)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.Name, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	maxOpen := cfg.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 50
	}
	maxIdle := cfg.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 10
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(time.Hour)
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Second)
	}
	const retries = 6
	backoff := time.Second
	for i := 0; i < retries; i++ {
		if err := sqlDB.Ping(); err == nil {
			return db, nil
		} else {
			if i < retries-1 {
				log.Printf("[card-center repo] ping %s failed (%d/%d): %v", cfg.Name, i+1, retries, err)
				time.Sleep(backoff)
				backoff *= 2
			}
		}
	}
	return nil, fmt.Errorf("ping %s exhausted retries", cfg.Name)
}

// HashToken sha256(token) hex —— 用作 token_hash 入库（DB 不存 token 本身做唯一索引也可，但 sha 让索引短）。
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
