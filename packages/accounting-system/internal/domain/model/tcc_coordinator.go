package model

import "time"

// TccCoordinatorPhase TCC 全局事务阶段
type TccCoordinatorPhase int8

const (
	TccPhaseTrying     TccCoordinatorPhase = 1 // Try 阶段：所有分支正在 Try
	TccPhaseConfirming TccCoordinatorPhase = 2 // Confirm 阶段：Try 全部成功，正在 Confirm
	TccPhaseConfirmed  TccCoordinatorPhase = 3 // 已全部 Confirm
	TccPhaseCancelled  TccCoordinatorPhase = 4 // 已全部 Cancel
)

// TccCoordinator TCC 全局协调者记录。分库 + 分表（tcc_coordinator_00..99），
// 路由规则与 account_transaction / tcc_transaction 一致，按 tcc_id（= voucher_no）
// 数字 hash 路由到 (dbIndex, globalTableIndex)。
// RecoveryWorker 依 phase 判断是 Cancel（TRYING 超时）还是 RetryConfirm（CONFIRMING 超时）。
//
// 注意：不定义 TableName() — 所有查询必须通过 repo 用 router.GetTableName("tcc_coordinator", tblIdx)
// 计算出实际表名并 .Table(name) 显式指定，GORM 默认表名会与分片规则冲突。
type TccCoordinator struct {
	TccID       string              `gorm:"column:tcc_id;primaryKey"`
	Phase       TccCoordinatorPhase `gorm:"column:phase"`
	BusinessNo  string              `gorm:"column:business_no"`
	BranchCount int                 `gorm:"column:branch_count"`
	// CutDate 日切归属日。booking 入口处 computeCutDate 计算后写入；
	// 同一 voucher 的所有 account_transaction.cut_date 必然 == 本字段。
	// 日切 drain 等待 SELECT WHERE cut_date <= X AND phase IN (TRYING, CONFIRMING) → 0
	// 之后 cut_date = X 的所有 entry 已 final，扫描安全。
	CutDate     string              `gorm:"column:cut_date"`
	// Currency 本笔 booking 的币种（PHP/USD/...）。booking 入口写入；TCC Recovery
	// 路径补 confirm 时读本字段作为流水的 currency，与原 booking 严格一致。
	// 空字符串 = 旧代码遗留数据：recovery 拒绝处理（fail-loud）等待 ops 手动
	// UPDATE 修补，**绝不 silently 用某个默认币种**（避免与原 booking 不一致 →
	// trial balance 按 currency 过滤单边出现 → 不平）。
	Currency    string              `gorm:"column:currency"`
	CreatedAt   time.Time           `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt   time.Time           `gorm:"column:updated_at;autoUpdateTime"`
}
