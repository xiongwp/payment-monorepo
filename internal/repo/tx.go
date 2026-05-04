package repo

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/xiongwp/order-core/internal/sharding"
)

// TxRunner 在同一个分片内跨多张表跑事务的辅助。
//
// 用法：
//
//	err := txRunner.Run(ctx, piID, func(txMgr *TxManager) error {
//	    if err := txMgr.PI().Create(ctx, pi); err != nil { return err }
//	    if err := txMgr.Charge().Create(ctx, ch); err != nil { return err }
//	    return nil
//	})
//
// 约束：所有参与对象必须路由到同一分片（pi_id 前缀相同）。
// 跨分片事务本系统**不支持**（设计上刻意避免），跨分片写入必须通过 outbox /
// 补偿事务保证最终一致。
type TxRunner struct {
	mgr    *Manager
	router *sharding.Router
}

// NewTxRunner 构造
func NewTxRunner(mgr *Manager, r *sharding.Router) *TxRunner {
	return &TxRunner{mgr: mgr, router: r}
}

// TxManager 事务里的仓储 accessor
type TxManager struct {
	tx     *gorm.DB
	router *sharding.Router
	tblIdx int
}

// DB 返回当前事务句柄
func (m *TxManager) DB() *gorm.DB { return m.tx }

// Table 返回本分片里某张表的名字（如 "payment_intent_15"）
func (m *TxManager) Table(base string) string {
	return m.router.GetTableName(base, m.tblIdx)
}

// Run 在 piID 所在分片开事务，把 TxManager 传给 fn
func (r *TxRunner) Run(ctx context.Context, piID string, fn func(tm *TxManager) error) error {
	if piID == "" {
		return fmt.Errorf("tx: pi_id required")
	}
	dbIdx, tblIdx := r.router.RouteByPrefixedID(piID)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(&TxManager{tx: tx, router: r.router, tblIdx: tblIdx})
	})
}
