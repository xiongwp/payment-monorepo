// Package repo 定义仓储接口与基础设施（DB Manager + 分片仓储）。
package repo

import (
	"context"
	"fmt"
	"log"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/xiongwp/order-core/internal/metrics"
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

// namedDB 跟踪每个 *gorm.DB 对应的 cfg.Name，用作 Prometheus label。
type namedDB struct {
	name string
	db   *gorm.DB
}

// Manager 数据库管理器：N 个分库 + 1 个非分片 metaDB（可选）
type Manager struct {
	shards     []*gorm.DB
	meta       *gorm.DB
	shardNames []string
	metaName   string
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
	names := make([]string, len(shards))
	for i, c := range shards {
		names[i] = c.Name
	}
	return &Manager{shards: dbs, shardNames: names}, nil
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
		mgr.metaName = meta.Name
	}
	return mgr, nil
}

// StartPoolMetrics 启动周期采集 goroutine，把 sql.DB.Stats() 写入 Prometheus gauge。
// ctx 取消时退出。interval 建议 15-30s，过短无意义（Prometheus 抓取间隔本就 ≥10s）。
//
// 关键监控点（写在 metrics.go 顶部 doc 里）：
//   - in_use / max_open > 0.8 持续 5min → 即将打爆，需扩容或排查慢查询
//   - wait_count 增速 > 100/min → 已经在排队等连接
//   - max_idle_closed 增速过快 → MaxIdleConns 设小了，连接频繁重建
func (m *Manager) StartPoolMetrics(ctx context.Context, interval time.Duration, logger *zap.Logger) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	all := make([]namedDB, 0, len(m.shards)+1)
	for i, db := range m.shards {
		all = append(all, namedDB{name: m.shardNames[i], db: db})
	}
	if m.meta != nil {
		all = append(all, namedDB{name: m.metaName, db: m.meta})
	}

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		// 首次立即采一次，避免 30s 内空指标
		collectPoolStats(all, logger)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				collectPoolStats(all, logger)
			}
		}
	}()
}

// collectPoolStats 把每个 DB 的 sql.DB.Stats() 写入 metrics gauge。
// 单点错误（取不到 sqlDB）不影响其他 DB 的采集。
func collectPoolStats(dbs []namedDB, logger *zap.Logger) {
	for _, n := range dbs {
		sqlDB, err := n.db.DB()
		if err != nil {
			if logger != nil {
				logger.Warn("pool metrics: failed to get sql.DB", zap.String("db", n.name), zap.Error(err))
			}
			continue
		}
		s := sqlDB.Stats()
		metrics.DBPoolMaxOpen.WithLabelValues(n.name).Set(float64(s.MaxOpenConnections))
		metrics.DBPoolOpen.WithLabelValues(n.name).Set(float64(s.OpenConnections))
		metrics.DBPoolInUse.WithLabelValues(n.name).Set(float64(s.InUse))
		metrics.DBPoolIdle.WithLabelValues(n.name).Set(float64(s.Idle))
		metrics.DBPoolWaitCount.WithLabelValues(n.name).Set(float64(s.WaitCount))
		metrics.DBPoolWaitDuration.WithLabelValues(n.name).Set(s.WaitDuration.Seconds())
		metrics.DBPoolMaxIdleClosed.WithLabelValues(n.name).Set(float64(s.MaxIdleClosed))
		metrics.DBPoolMaxLifetimeClosed.WithLabelValues(n.name).Set(float64(s.MaxLifetimeClosed))
	}
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
	// 默认值兜底，防止 config 漏填导致无限连接。
	// 历史值是 100/50，但一个 pod × 10 分片 × 100 = 1000 物理连接，
	// 3 个 pod 就能撞 MySQL max_connections 默认上限。降到 30/10：
	// 1 pod × 10 分片 × 30 = 300，3 pod = 900，仍在常规配置范围内。
	// 真要扛高 QPS 应横向加 pod，而不是单 pod 持有过多连接。
	maxOpen := cfg.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 30
	}
	maxIdle := cfg.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 10
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
