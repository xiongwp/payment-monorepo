package model

import (
	"time"
)

// TccStatus TCC 分支状态
type TccStatus int8

const (
	TccStatusTrying    TccStatus = 0 // Try 阶段已完成（冻结资金）
	TccStatusConfirmed TccStatus = 1 // Confirm 阶段已完成（余额已变更）
	TccStatusCancelled TccStatus = 2 // Cancel 阶段已完成（冻结已释放）
)

// TccTransaction TCC 分布式事务分支记录
// 每笔 DoubleEntryBooking 的每个分录对应一条 TCC 分支记录，与账户存储在同一分片。
//
// 状态流转：
//
//	TRYING → CONFIRMED  (正常路径)
//	TRYING → CANCELLED  (Try 阶段有分支失败，回滚路径)
//
// 冻结语义：
//
//	Try 阶段：若该分录导致余额减少，则冻结 frozen_amount（降低 available_balance）
//	Confirm 阶段：真正修改 balance，对余额增加方向同步更新 available_balance
//	Cancel 阶段：释放 frozen_amount（恢复 available_balance）
//
// 金额字段单位：ISO 最小货币单位 × 100（参见 currency 包）
type TccTransaction struct {
	ID           int64     `db:"id"            json:"id"`
	TccID        string    `db:"tcc_id"        json:"tcc_id"`        // 全局 TCC ID（= voucherNo）
	BranchID     string    `db:"branch_id"     json:"branch_id"`     // 分支 ID（= transactionID）
	AccountNo    string    `db:"account_no"    json:"account_no"`    // 账户号
	BalanceDelta int64     `db:"balance_delta" json:"balance_delta"` // Confirm 时对 balance 的增量（负=减少）
	FrozenAmount int64     `db:"frozen_amount" json:"frozen_amount"` // Try 时冻结的金额（0 = 无需冻结）
	Status       TccStatus `db:"status"        json:"status"`
	CreatedAt    time.Time `db:"created_at"    json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at"    json:"updated_at"`
}
