// db.go — GORM 统一封装.
//
// 标准化:
//   - MySQL driver (跟 monorepo 一致)
//   - 连接池配置 (MaxOpen / MaxIdle / Lifetime)
//   - GORM slow log threshold
//   - 自动 AutoMigrate (dev 用; prod 强制 false, 走 migration tool)
//   - prometheus 指标 (open_conns / wait_count / wait_duration)
//
// 用法:
//   db, err := scaffold.NewDB(cfg.DB, log)
//   defer db.Close()
//
//   // 业务 repo
//   var req domain.Request
//   if err := db.GORM().WithContext(ctx).First(&req, "request_id = ?", id).Error; err != nil { ... }

package scaffold

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB 包装 GORM + 原生 *sql.DB.
type DB struct {
	gormDB *gorm.DB
	sqlDB  *sql.DB
	log    *zap.Logger
}

// NewDB 打开 + 配置 + AutoMigrate (如 cfg.AutoMigrate).
func NewDB(cfg DBConfig, log *zap.Logger, models ...interface{}) (*DB, error) {
	if cfg.DSN == "" {
		return nil, errors.New("scaffold/db: DSN required")
	}

	gormLogger := logger.New(
		zapWriter{log: log},
		logger.Config{
			SlowThreshold:             cfg.SlowThreshold,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		},
	)

	gormDB, err := gorm.Open(mysql.Open(cfg.DSN), &gorm.Config{
		Logger:                                   gormLogger,
		PrepareStmt:                              true, // 缓存 prepared stmt
		DisableForeignKeyConstraintWhenMigrating: true,
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("gorm open: %w", err)
	}

	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	// ping 验证连通
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("db ping: %w", err)
	}

	d := &DB{gormDB: gormDB, sqlDB: sqlDB, log: log}

	if cfg.AutoMigrate && len(models) > 0 {
		log.Info("AutoMigrate", zap.Int("model_count", len(models)))
		if err := gormDB.AutoMigrate(models...); err != nil {
			return nil, fmt.Errorf("auto migrate: %w", err)
		}
	}

	return d, nil
}

// GORM 返回 *gorm.DB 给业务用.
func (d *DB) GORM() *gorm.DB { return d.gormDB }

// SQL 返回原生 *sql.DB (outbox / payment-util/outbox.Append 需要).
func (d *DB) SQL() *sql.DB { return d.sqlDB }

// Close 关闭连接池.
func (d *DB) Close() error {
	if d.sqlDB == nil {
		return nil
	}
	return d.sqlDB.Close()
}

// RegisterMetrics 注册连接池指标.
func (d *DB) RegisterMetrics(reg prometheus.Registerer, namespace string) {
	if namespace == "" {
		namespace = "db"
	}
	openConns := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace, Name: "open_connections",
		Help: "current open db connections",
	}, func() float64 {
		return float64(d.sqlDB.Stats().OpenConnections)
	})
	idleConns := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace, Name: "idle_connections",
	}, func() float64 {
		return float64(d.sqlDB.Stats().Idle)
	})
	waitCount := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace, Name: "wait_count_total",
	}, func() float64 {
		return float64(d.sqlDB.Stats().WaitCount)
	})
	reg.MustRegister(openConns, idleConns, waitCount)
}

// zapWriter 让 GORM logger 写到 zap.
type zapWriter struct{ log *zap.Logger }

func (w zapWriter) Printf(format string, args ...interface{}) {
	w.log.Warn(fmt.Sprintf(format, args...))
}
