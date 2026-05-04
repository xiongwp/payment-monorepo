// Package domain: 结算服务的数据模型。形状与 accounting-system 的 day-cut
// 控制 / record 表语义对齐：
//
//   SettlementRun       ≅ DayCutControl（一次运行的状态机）
//   SettlementRecord    ≅ AccountBalanceSnapshot（单 merchant 单次结算的明细）
//
// 这些 struct 既用作 GORM 实体也作 service 层 DTO；DTO 与实体合并是为了避免
// 早期骨架阶段 N 套类型转换。生产化时如有 read-only 字段集差异再分裂。
package domain

import "time"

// SettlementStatus 结算状态。int8 节省存储，与 accounting-system 风格一致。
type SettlementStatus int8

const (
	SettlementStatusPending    SettlementStatus = 1
	SettlementStatusProcessing SettlementStatus = 2
	SettlementStatusCompleted  SettlementStatus = 3
	SettlementStatusFailed     SettlementStatus = 4
	SettlementStatusSkipped    SettlementStatus = 5 // 商户当日无可结算金额
)

// SettlementFrequency 结算频率，影响"今天该不该结算这个商户"。
type SettlementFrequency string

const (
	FrequencyT0     SettlementFrequency = "T+0"
	FrequencyT1     SettlementFrequency = "T+1"
	FrequencyWeekly SettlementFrequency = "WEEKLY"
)

// SettlementRun 单个分片在某次结算运行中的状态。每次 trigger 会在 100 个
// settlement_run_NN 表里各 INSERT 一行（每个分片一行），独立追踪进度。
//
// 联合唯一键：(DBIndex, TableIndex, SettleDate, RunID)。同一 (settle_date,
// run_id) 全分片均完成 → 该 run 总体 COMPLETED；任一分片 FAILED → 总体 FAILED。
//
// 重跑：admin 触发同 SettleDate 时 RunID 自增；新 run 跳过那些 (settle_date,
// merchant_id) 已经 COMPLETED 的 record（幂等），只重跑 PROCESSING / FAILED。
type SettlementRun struct {
	ID         int64            `db:"id"`
	DBIndex    int              `db:"database_index"`
	TableIndex int              `db:"table_index"`
	SettleDate string           `db:"settle_date"` // YYYY-MM-DD，业务日
	RunID      int              `db:"run_id"`
	Currency   string           `db:"currency"`
	Status     SettlementStatus `db:"status"`
	StartedAt  *time.Time       `db:"started_at"`
	FinishedAt *time.Time       `db:"finished_at"`
	ErrorMsg   string           `db:"error_msg"`
	// LastProcessedMerchantID 续跑游标。下次拉 WHERE id > cursor ORDER BY id ASC LIMIT N。
	LastProcessedMerchantID string    `db:"last_processed_merchant_id"`
	CreatedAt               time.Time `db:"created_at"`
	UpdatedAt               time.Time `db:"updated_at"`
}

// SettlementRecord 单商户单次结算明细。
//
// 幂等键：(SettleDate, MerchantID, RunID)。
// 跨 RunID 通过 (SettleDate, MerchantID) 检索唯一已成功记录 → 决定本商户是否
// 跳过当日重跑（即使 RunID 不同）。
type SettlementRecord struct {
	ID           int64            `db:"id"`
	SettleDate   string           `db:"settle_date"`
	MerchantID   string           `db:"merchant_id"`
	RunID        int              `db:"run_id"`
	Currency     string           `db:"currency"`
	Status       SettlementStatus `db:"status"`
	// AmountMinor 结算金额，accounting 内部 storage 单位（minor × 100）；
	// 透传不做本地浮点。
	AmountMinor int64 `db:"amount_minor"`
	// VoucherNo accounting-system 返回的双分录 voucher_no，作为对账锚点。
	VoucherNo string    `db:"voucher_no"`
	ErrorMsg  string    `db:"error_msg"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}
