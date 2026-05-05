package repo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/xiongwp/card-payment/internal/processor"
	"github.com/xiongwp/card-payment/internal/sharding"
)

type DBConfig struct {
	Name            string `mapstructure:"name"`
	DSN             string `mapstructure:"dsn"`
	MaxOpenConns    int    `mapstructure:"max_open_conns"`
	MaxIdleConns    int    `mapstructure:"max_idle_conns"`
	ConnMaxLifetime int    `mapstructure:"conn_max_lifetime"`
}

type Manager struct {
	meta   *gorm.DB
	shards []*gorm.DB
	router *sharding.Router
	logger *zap.Logger
}

func NewManager(meta DBConfig, shards []DBConfig, router *sharding.Router, logger *zap.Logger) (*Manager, error) {
	if meta.DSN == "" {
		return nil, errors.New("repo: meta.dsn required")
	}
	if len(shards) != sharding.ShardDBCount {
		return nil, fmt.Errorf("repo: need %d shards, got %d", sharding.ShardDBCount, len(shards))
	}
	mDB, err := openConn(meta)
	if err != nil {
		return nil, fmt.Errorf("meta: %w", err)
	}
	sDBs := make([]*gorm.DB, len(shards))
	for i, c := range shards {
		db, err := openConn(c)
		if err != nil {
			return nil, fmt.Errorf("shard[%d]: %w", i, err)
		}
		sDBs[i] = db
	}
	return &Manager{meta: mDB, shards: sDBs, router: router, logger: logger}, nil
}

func (m *Manager) Meta() *gorm.DB           { return m.meta }
func (m *Manager) Shard(idx int) *gorm.DB   { return m.shards[idx] }
func (m *Manager) Router() *sharding.Router { return m.router }
func (m *Manager) ShardCount() int          { return len(m.shards) }

func (m *Manager) Close() error {
	for _, db := range append([]*gorm.DB{m.meta}, m.shards...) {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	return nil
}

func openConn(cfg DBConfig) (*gorm.DB, error) {
	db, err := gorm.Open(gormmysql.Open(cfg.DSN), &gorm.Config{PrepareStmt: true})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.Name, err)
	}
	sqlDB, _ := db.DB()
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
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Second)
	} else {
		sqlDB.SetConnMaxLifetime(time.Hour)
	}
	const retries = 6
	backoff := time.Second
	for i := 0; i < retries; i++ {
		if err := sqlDB.Ping(); err == nil {
			return db, nil
		} else if i < retries-1 {
			log.Printf("[card-payment repo] ping %s failed: %v", cfg.Name, err)
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("ping %s exhausted", cfg.Name)
}

// CardTransactionRepo 实现 processor.CardTransactionRepo
type CardTransactionRepo struct{ mgr *Manager }

const tblCardTx = "card_transaction"

func NewCardTransactionRepo(mgr *Manager) *CardTransactionRepo {
	return &CardTransactionRepo{mgr: mgr}
}

var _ processor.CardTransactionRepo = (*CardTransactionRepo)(nil)

func (r *CardTransactionRepo) shard(ctx context.Context, piID string) (*gorm.DB, string) {
	dbIdx, gtbl := r.mgr.router.RouteByPIID(piID)
	return r.mgr.Shard(dbIdx), r.mgr.router.TableName(ctx, tblCardTx, gtbl)
}

func (r *CardTransactionRepo) Insert(ctx context.Context, tx *processor.CardTransaction) error {
	if tx.PIID == "" {
		return errors.New("Insert: pi_id required")
	}
	db, tbl := r.shard(ctx, tx.PIID)
	return db.WithContext(ctx).Table(tbl).Create(tx).Error
}

// UpdateStatus 跨分片定位 networkRefNo（性能差但接口已定）。
// 推荐 caller 改用 UpdateStatusByPI，避免 100 张表扫描。
func (r *CardTransactionRepo) UpdateStatus(ctx context.Context, networkRefNo, status, declineCode string) error {
	for _, s := range r.mgr.router.AllShards() {
		db := r.mgr.Shard(s.DBIndex)
		tbl := r.mgr.router.TableName(ctx, tblCardTx, s.TableIndex)
		res := db.WithContext(ctx).Table(tbl).
			Where("network_ref_no = ?", networkRefNo).
			Updates(map[string]any{
				"status":       status,
				"decline_code": declineCode,
				"updated_at":   time.Now(),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			return nil
		}
	}
	return errors.New("update status: not found")
}

func (r *CardTransactionRepo) GetByPI(ctx context.Context, piID string) (*processor.CardTransaction, error) {
	db, tbl := r.shard(ctx, piID)
	var row processor.CardTransaction
	err := db.WithContext(ctx).Table(tbl).
		Where("pi_id = ?", piID).
		Order("created_at DESC").Limit(1).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &row, err
}

func (r *CardTransactionRepo) GetByNetworkRef(ctx context.Context, networkRefNo string) (*processor.CardTransaction, error) {
	for _, s := range r.mgr.router.AllShards() {
		db := r.mgr.Shard(s.DBIndex)
		tbl := r.mgr.router.TableName(ctx, tblCardTx, s.TableIndex)
		var row processor.CardTransaction
		err := db.WithContext(ctx).Table(tbl).
			Where("network_ref_no = ?", networkRefNo).
			Take(&row).Error
		if err == nil {
			return &row, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	return nil, nil
}
