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

// Manager 数据库管理器：meta 主库 + 可选只读 replica（round-robin）。
type Manager struct {
	meta     *gorm.DB
	replicas []*gorm.DB
	rrIdx    atomic.Uint32
}

// NewManager 主库（必填）+ 可选 replica（0..N 个）。
func NewManager(meta DBConfig, replicas ...DBConfig) (*Manager, error) {
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

// Close 关闭所有连接
func (m *Manager) Close() error {
	closers := append([]*gorm.DB{m.meta}, m.replicas...)
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
