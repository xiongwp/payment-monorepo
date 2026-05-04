package model

import "time"

// OutboxStatus 结算 Outbox 状态机
//
//	PENDING    → 已写入 MySQL（WAL），Redis 尚未更新（进程崩溃或 Redis 暂时不可用）
//	REDIS_DONE → Redis 已更新，MySQL 流水 / 账户余额待同步
//	MYSQL_DONE → MySQL 流水 + 账户余额已写入，全链路完成
//	FAILED     → 重试耗尽，需人工介入
type OutboxStatus int8

const (
	OutboxStatusPending   OutboxStatus = 0
	OutboxStatusRedisDone OutboxStatus = 1
	OutboxStatusMySQLDone OutboxStatus = 2
	OutboxStatusFailed    OutboxStatus = 3
)

// SettlementOutbox 热路径事务预写日志（Transactional Outbox）
//
// 设计保证：
//   - 在 Redis Lua 更新之前同步写入本表；若写入失败则 Redis 不执行，整笔请求返回错误
//   - 一旦本表 INSERT 成功，无论 Redis / Kafka / 进程是否崩溃，
//     OutboxWorker 均会最终将交易持久化到 MySQL（account_transaction + account balance）
//   - voucher_no 唯一索引保证幂等写入
//
// 存储：固定使用 DB0（非分片），是全局唯一的 WAL 中心。
type SettlementOutbox struct {
	ID              int64        `gorm:"column:id;primaryKey;autoIncrement"            json:"id"`
	VoucherNo       string       `gorm:"column:voucher_no;uniqueIndex;size:64;not null" json:"voucher_no"`
	EventData       string       `gorm:"column:event_data;type:longtext;not null"       json:"event_data"` // JSON: SettlementEvent
	TransactionDate string       `gorm:"column:transaction_date;size:10;not null"       json:"transaction_date"` // 交易日期 YYYY-MM-DD，供日切按天过滤 outbox delta
	// CutDate 日切归属日（YYYY-MM-DD）。**入口处 booking 落 outbox 那一刻定死**，
	// 之后任何 worker / 重投 / 多 pod replay 都直接读本字段，**永远不重算**。
	// 抗时钟漂移、抗 DB 重启、抗 SystemConfig 修改：cut_date 入库即不可变。
	// 与 transaction_date 区别：transaction_date 是日历日（按本地时区 0 点切），
	// cut_date 是业务日切归属日（可配置 cut_hour，例如菲律宾 17:00 UTC）。
	// trial balance 按 cut_date 过滤要求同一 voucher 全部 entry 共享同一 cut_date —
	// 由本字段在写 outbox 那一刻一次锁定保证。
	CutDate         string       `gorm:"column:cut_date;size:10;not null;default:''"    json:"cut_date"`
	Status          OutboxStatus `gorm:"column:status;not null;default:0;index"         json:"status"`
	RetryCount      int          `gorm:"column:retry_count;not null;default:0"          json:"retry_count"`
	ErrorMsg        string       `gorm:"column:error_msg;type:text"                     json:"error_msg"`
	CreatedAt       time.Time    `gorm:"column:created_at;not null;index"               json:"created_at"`
	UpdatedAt       time.Time    `gorm:"column:updated_at;not null"                     json:"updated_at"`
}

func (SettlementOutbox) TableName() string { return "settlement_outbox" }
