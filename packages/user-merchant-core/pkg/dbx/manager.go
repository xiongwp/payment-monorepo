// Package dbx 共享 MySQL GORM Manager（抽自 user-merchant-core/internal/repo）。
//
// 单主 + N 只读 replica；Manager.GetMeta() 写路径，GetMetaRO() 读路径（空 replica
// 退回主库）。自带 PrepareStmt、连接池兜底、zap gorm logger、duplicate-key 判断。
package dbx

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	sqlmysql "github.com/go-sql-driver/mysql"
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

// Manager 数据库管理器：meta 主库 + 可选只读 replica + 可选 N 个 shard 库。
//
// **分库分表（10 库 × 每库 10 表 = 100 张全局分片表）**：
// 跟 accounting-system / order-core / payment-channel layout 完全对齐。
// 业务表（users / merchants / merchant_secrets / admin_audit_log 等）按
// user_id / merchant_id 路由到 shards[0..9]；leaf_alloc / 字典表（roles /
// permissions）留在 meta DB。
//
// 路由规则见 internal/sharding/router.go：
//
//	globalTbl = id % 100
//	dbIdx     = globalTbl / 10
//	tblName   = "<base>_<globalTbl:02d>"  (e.g. users_42)
type Manager struct {
	meta     *gorm.DB
	replicas []*gorm.DB
	shards   []*gorm.DB
	rrIdx    atomic.Uint32
}

// NewManager 主库（必填）+ 可选 replica（0..N 个）。
//
// 不带 shard，历史调用兼容：单 meta + replicas 模式。新代码用 NewShardedManager。
func NewManager(meta DBConfig, replicas ...DBConfig) (*Manager, error) {
	return NewShardedManager(meta, nil, replicas...)
}

// NewShardedManager 主库 + N 个 shard 库 + 可选 replica。
//
// shards 顺序与 dbIndex 一一对应（shards[0] = accounting_db_0 / paychan_db_0 风格命名，
// 由 cmd/server 配置决定）。空 / nil 退化为单 meta 模式（适合 dev / 单库测试）。
func NewShardedManager(meta DBConfig, shards []DBConfig, replicas ...DBConfig) (*Manager, error) {
	if meta.DSN == "" {
		return nil, fmt.Errorf("dbx: database.meta.dsn required")
	}
	db, err := openConn(meta)
	if err != nil {
		return nil, fmt.Errorf("meta db: %w", err)
	}
	m := &Manager{meta: db}
	for _, r := range replicas {
		if r.DSN == "" {
			continue
		}
		rdb, err := openConn(r)
		if err != nil {
			return nil, fmt.Errorf("replica %s: %w", r.Name, err)
		}
		m.replicas = append(m.replicas, rdb)
	}
	for i, s := range shards {
		if s.DSN == "" {
			return nil, fmt.Errorf("dbx: shard[%d] DSN empty (sharded mode requires all shards configured)", i)
		}
		sdb, err := openConn(s)
		if err != nil {
			return nil, fmt.Errorf("shard[%d] %s: %w", i, s.Name, err)
		}
		m.shards = append(m.shards, sdb)
	}
	return m, nil
}

// GetMeta 主库连接（写路径 + 强一致读）。
func (m *Manager) GetMeta() *gorm.DB { return m.meta }

// GetMetaRO 只读请求走 replica；空时回退主库。
func (m *Manager) GetMetaRO() *gorm.DB {
	if len(m.replicas) == 0 {
		return m.meta
	}
	i := int(m.rrIdx.Add(1)-1) % len(m.replicas)
	return m.replicas[i]
}

// GetShard 第 idx 个分库（0-based）。idx 越界或未配置 shards 时返回 meta（dev 模式兼容）。
func (m *Manager) GetShard(idx int) *gorm.DB {
	if len(m.shards) == 0 {
		return m.meta
	}
	if idx < 0 || idx >= len(m.shards) {
		return m.shards[0]
	}
	return m.shards[idx]
}

// ShardCount 配置的分库数量；0 表示未启用分片。
func (m *Manager) ShardCount() int { return len(m.shards) }

// AllShards 所有分库连接（schema migrator 用）。
func (m *Manager) AllShards() []*gorm.DB { return m.shards }

// Close 关闭所有连接
func (m *Manager) Close() error {
	closers := append([]*gorm.DB{m.meta}, m.replicas...)
	closers = append(closers, m.shards...)
	for _, db := range closers {
		if db == nil {
			continue
		}
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	return nil
}

func openConn(cfg DBConfig) (*gorm.DB, error) {
	gormCfg := &gorm.Config{PrepareStmt: true}
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
	sqlDB.SetConnMaxIdleTime(10 * time.Minute)
	const maxRetries = 6
	backoff := time.Second
	for i := 0; i < maxRetries; i++ {
		if err = sqlDB.Ping(); err == nil {
			return db, nil
		}
		if i < maxRetries-1 {
			log.Printf("[dbx] ping %s failed (%d/%d), retry in %s: %v", cfg.Name, i+1, maxRetries, backoff, err)
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("ping %s: %w", cfg.Name, err)
}

// IsDupKey 判断 MySQL 是否为唯一索引冲突（errno 1062）。
func IsDupKey(err error) bool {
	if err == nil {
		return false
	}
	var me *sqlmysql.MySQLError
	if errors.As(err, &me) && me.Number == 1062 {
		return true
	}
	return strings.Contains(err.Error(), "Duplicate entry")
}
