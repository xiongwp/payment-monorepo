package model

import (
	"errors"
	"time"
)

// AccountType 账户类型
type AccountType int8

const (
	AccountTypeUser                     AccountType = 1 // 用户账户
	AccountTypeMerchant                 AccountType = 2 // 商户账户
	AccountTypeMerchantPendingSettle    AccountType = 3 // 商户待结算账户
	AccountTypePlatform                 AccountType = 4 // 平台损益账户
	AccountTypeTransitChannelReceivable AccountType = 5 // 中间账户渠道应收款
	AccountTypeTransitChannelPayable    AccountType = 6 // 中间账户渠道应付款
	AccountTypeTransactionFee           AccountType = 7 // 平台手续费账户
	AccountTypeChargeFee                AccountType = 8 // 平台服务费账户
	AccountTypeTransit                  AccountType = 9 // 中间账户
)

// AccountBusinessType 账户业务类型
// userId + AccountBusinessType 为全局唯一键，不允许重复创建
// 对应 SQL SMALLINT（-32768~32767），最大支持 999（3位业务编码）
type AccountBusinessType int16

const (
	AccountBusinessTypeUserBalance              AccountBusinessType = 1 // 用户余额账户
	AccountBusinessTypeMerchantBalance          AccountBusinessType = 2 // 商户结算账户
	AccountBusinessTypeMerchantPendingSettle    AccountBusinessType = 3 // 商户待结算余额账户
	AccountBusinessTypePlatformProfitLoss       AccountBusinessType = 4 // 平台损益账户（预置，不可通过 API 创建）
	AccountBusinessTypeTransitChannelReceivable AccountBusinessType = 5 // 中间账户渠道应收款（预置，不可通过 API 创建）
	AccountBusinessTypeTransitChannelPayable    AccountBusinessType = 6 // 中间账户渠道应付款（预置，不可通过 API 创建）
	AccountBusinessTypeTransactionFee           AccountBusinessType = 7 // 平台手续费账户
	AccountBusinessTypeChargeFee                AccountBusinessType = 8 // 平台服务费账户
	AccountBusinessTypeTransit                  AccountBusinessType = 9 // 平台中间账户
)

// ErrAccountAlreadyExists userId + accountBusinessType 重复时返回此错误（gRPC 层映射为 409）
var ErrAccountAlreadyExists = errors.New("account already exists")

// AccountBusinessTypeInfo 账户业务类型配置（存 account_meta.account_business_type_info）。
//
// 承担 business_type registry 职责：(business_type 数字码) ↔ (代码名 / 账户类型)
// 的映射注册。系统初始化预置 1-9 默认类型；新渠道（Alipay / Gcash 等）先通过
// RegisterBusinessType 登记一条，再到"系统账户"页走 CreatePlatformAccountFleet
// 创建 100 个分片账户。
//
// 注意：Category 不在本表存储。category 由 account_type 1:1 派生（asset/expense/
// liability/equity/revenue），放表里既冗余又容易被写错。需要 category 时用
// service.CategoryForAccountType(account_type) 推导；admin-web 前端用相同的
// 常量映射 account_type → category 来显示。
type AccountBusinessTypeInfo struct {
	ID               int64               `db:"id"                 gorm:"column:id;primaryKey"                                         json:"id"`
	BusinessType     AccountBusinessType `db:"business_type"      gorm:"column:business_type;uniqueIndex:uk_business_type"            json:"business_type"`
	BusinessTypeCode string              `db:"business_type_code" gorm:"column:business_type_code;uniqueIndex:uk_business_type_code" json:"business_type_code"`
	AccountType      AccountType         `db:"account_type"       gorm:"column:account_type"                                          json:"account_type"`
	Description      *string             `db:"description"        gorm:"column:description"                                           json:"description"`
	Enabled          int8                `db:"enabled"            gorm:"column:enabled"                                               json:"enabled"`
	CreatedAt        time.Time           `db:"created_at"         gorm:"column:created_at"                                            json:"created_at"`
	UpdatedAt        time.Time           `db:"updated_at"         gorm:"column:updated_at"                                            json:"updated_at"`
}

// TableName GORM 表名约定。
func (AccountBusinessTypeInfo) TableName() string { return "account_business_type_info" }

// AccountCategory 账户分类（会计科目）
type AccountCategory string

const (
	AccountCategoryAsset     AccountCategory = "ASSET"     // 资产
	AccountCategoryLiability AccountCategory = "LIABILITY" // 负债
	AccountCategoryEquity    AccountCategory = "EQUITY"    // 所有者权益
	AccountCategoryRevenue   AccountCategory = "REVENUE"   // 收入
	AccountCategoryExpense   AccountCategory = "EXPENSE"   // 费用
)

// AccountStatus 账户状态
type AccountStatus int8

const (
	AccountStatusDisabled AccountStatus = 0 // 禁用
	AccountStatusActive   AccountStatus = 1 // 正常
	AccountStatusFrozen   AccountStatus = 2 // 冻结
)

// Account 账户模型
// 金额字段单位：ISO 最小货币单位 × 100（参见 currency 包）
type Account struct {
	ID                  int64               `db:"id"                    json:"id"`
	AccountNo           string              `db:"account_no"            json:"account_no"`
	UserID              int64               `db:"user_id"               json:"user_id"`
	AccountType         AccountType         `db:"account_type"          json:"account_type"`
	AccountCategory     AccountCategory     `db:"account_category"      json:"account_category"`
	AccountBusinessType AccountBusinessType `db:"account_business_type" json:"account_business_type"`
	Currency            string              `db:"currency"              json:"currency"`
	Balance             int64               `db:"balance"               json:"balance"`
	FrozenBalance       int64               `db:"frozen_balance"        json:"frozen_balance"`
	AvailableBalance    int64               `db:"available_balance"     json:"available_balance"`
	Status              AccountStatus       `db:"status"                json:"status"`
	Version             int64               `db:"version"               json:"version"`
	CreatedAt           time.Time           `db:"created_at"            json:"created_at"`
	UpdatedAt           time.Time           `db:"updated_at"            json:"updated_at"`
}

// BusinessType 业务类型
type BusinessType string

const (
	BusinessTypeTransfer   BusinessType = "TRANSFER"   // 转账
	BusinessTypePayment    BusinessType = "PAYMENT"    // 支付
	BusinessTypeRefund     BusinessType = "REFUND"     // 退款
	BusinessTypeWithdraw   BusinessType = "WITHDRAW"   // 提现
	BusinessTypeDeposit    BusinessType = "DEPOSIT"    // 充值
	BusinessTypeCommission BusinessType = "COMMISSION" // 佣金
)

// TransactionStatus 交易状态
type TransactionStatus int8

const (
	TransactionStatusFailed     TransactionStatus = 0 // 失败
	TransactionStatusSuccess    TransactionStatus = 1 // 成功
	TransactionStatusProcessing TransactionStatus = 2 // 处理中
)

// TransactionBookingType 记账方式
type TransactionBookingType int8

const (
	// TransactionBookingTypeSync 同步记账：在事务内直接更新 account.balance，
	// balance_before/balance_after 精确反映账户实际余额。
	// 适用：用户账户、商户账户、调账操作。
	TransactionBookingTypeSync TransactionBookingType = 1

	// TransactionBookingTypeBuffered 缓冲记账：余额增量写入 account_balance_buffer，
	// 由后台 flush worker 批量刷新，balance_before/balance_after 为逻辑计算值，
	// 可能与 account.balance 存在短暂偏差。
	// 适用：平台账户、中间账户等高并发账户。
	TransactionBookingTypeBuffered TransactionBookingType = 2
)

// AccountTransaction 账户流水
// 金额字段单位：ISO 最小货币单位 × 100（参见 currency 包）
type AccountTransaction struct {
	ID                  int64                  `db:"id"`
	TransactionID       string                 `db:"transaction_id"`
	ParentTransactionID *string                `db:"parent_transaction_id"`
	AccountNo           string                 `db:"account_no"`
	BusinessNo          string                 `db:"business_no"`
	BusinessType        BusinessType           `db:"business_type"`
	DebitAmount         int64                  `db:"debit_amount"`
	CreditAmount        int64                  `db:"credit_amount"`
	BalanceBefore       int64                  `db:"balance_before"`
	BalanceAfter        int64                  `db:"balance_after"`
	BookingType         TransactionBookingType `db:"booking_type" gorm:"column:booking_type"`
	Currency            string                 `db:"currency"`
	TransactionDate     string                 `db:"transaction_date"`
	TransactionTime     time.Time              `db:"transaction_time"`
	Description         *string                `db:"description"`
	Status              TransactionStatus      `db:"status"`
	// CutDate 日切归属日（业务日）。booking 入口处由 computeCutDate(now,
	// scheduled_time, tz) 一次性确定，并 propagate 到所有 entry + tcc_coordinator。
	// 同一 voucher 所有 entry 共享同一个 CutDate → 试算平衡天然成立（不依赖 finalize 时刻）。
	// "YYYY-MM-DD" 字符串与 transaction_date 一致格式。
	CutDate             string                 `db:"cut_date" gorm:"column:cut_date"`
	RetryCount          int                    `db:"retry_count"`
	ExtInfo             *string                `db:"ext_info"`
	CreatedAt           time.Time              `db:"created_at"`
	UpdatedAt           time.Time              `db:"updated_at"`
}

// AccountingVoucher 复式记账凭证
// 金额字段单位：ISO 最小货币单位 × 100（参见 currency 包）
type AccountingVoucher struct {
	ID           int64        `db:"id"`
	VoucherNo    string       `db:"voucher_no"`
	BusinessNo   string       `db:"business_no"`
	BusinessType BusinessType `db:"business_type"`
	TotalDebit   int64        `db:"total_debit"`
	TotalCredit  int64        `db:"total_credit"`
	Currency     string       `db:"currency"`
	VoucherDate  string       `db:"voucher_date"`
	Status       int8         `db:"status"`
	Description  *string      `db:"description"`
	CreatedAt    time.Time    `db:"created_at"`
	UpdatedAt    time.Time    `db:"updated_at"`
}

// AccountBalanceSnapshot 账户余额快照
// 金额字段单位：ISO 最小货币单位 × 100（参见 currency 包）
//
// JSON tags 匹配 admin-web 前端的 AccountBalanceSnapshot 接口
// （snake_case）。没 tag 时默认走 Go 字段名，前端所有列都读不到。
type AccountBalanceSnapshot struct {
	ID           int64  `db:"id"            json:"id"`
	AccountNo    string `db:"account_no"    json:"account_no"`
	SnapshotDate string `db:"snapshot_date" json:"snapshot_date"`
	// RunID 关联日切版本号，同一日期每次重跑时递增。试算平衡使用最近一次完成的版本。
	RunID             int       `db:"run_id"                           gorm:"column:run_id" json:"run_id"`
	BeginningBalance  int64     `db:"beginning_balance"                                     json:"beginning_balance"`
	EndingBalance     int64     `db:"ending_balance"                                        json:"ending_balance"`
	TotalDebit        int64     `db:"total_debit"                                           json:"total_debit"`
	TotalCredit       int64     `db:"total_credit"                                          json:"total_credit"`
	TransactionCount  int       `db:"transaction_count"                                     json:"transaction_count"`
	Currency          string    `db:"currency"                                              json:"currency"`
	// LastTransactionID 该账户本日最后一笔流水ID，仅供审计/对账使用。
	LastTransactionID string    `db:"last_transaction_id" json:"last_transaction_id,omitempty"`
	CreatedAt         time.Time `db:"created_at"          json:"created_at"`
}

// DayCutControl 日切控制（cut_date tag 版）
//
// 扫描完全靠 `cut_date` 标签：booking 入口处 computeCutDate 已经决定每条
// account_transaction.cut_date，日切只需 WHERE cut_date = X AND status=SUCCESS
// AND id > last_processed_id ORDER BY id ASC LIMIT N。
//
// 关键正确性论证：
//  1. 同一 voucher 所有 entry 在 booking 入口共享同一 cut_date（read once + propagate
//     到所有 per-shard Confirm tx）。
//  2. 跨分片 voucher: cut_date 经 bookingParams 透传，每个分片 INSERT 都带相同标签。
//  3. 高并发: 任意时刻触发 cut，drain 等待 tcc_coordinator 上 cut_date<=X AND
//     phase IN (TRYING, CONFIRMING) 的行 → 0；之后 scan 是 final state。
//  4. 不依赖墙钟时间 / id watermark / commit-visibility race。
type DayCutControl struct {
	ID            int64  `db:"id"`
	DatabaseIndex int    `db:"database_index"`
	TableIndex    int    `db:"table_index"`
	TableName     string `db:"table_name"`
	CutDate       string `db:"cut_date"`
	RunID         int    `db:"run_id" gorm:"column:run_id"`
	// CutTime 日切完成时的服务器墙钟时间，仅供审计。
	CutTime *time.Time `db:"cut_time" gorm:"-"`
	// LastProcessedID 本 run 在分片上已处理到的 account_transaction.id（含）。
	// chunk 内分页游标：下次 chunk 带 WHERE cut_date=? AND status=1 [AND currency=?]
	// AND id > last_processed_id ORDER BY id ASC LIMIT N。
	// 0 = 本 run 尚未开跑。每个 chunk 处理完后与 snapshot upsert 在同一事务原子推进。
	LastProcessedID uint64 `db:"last_processed_id" gorm:"column:last_processed_id"`
	// LastTransactionID 仅供审计：本 run 完成时最后处理的 transaction_id。
	LastTransactionID string `db:"last_transaction_id" gorm:"column:last_transaction_id"`
	// Currency 本 run 的币种过滤。"" = 全部币种；非空 = 仅该币种账户。
	Currency     string     `db:"currency" gorm:"column:currency;default:''"`
	Status       int8       `db:"status"`
	StartTime    *time.Time `db:"start_time"`
	EndTime      *time.Time `db:"end_time"`
	ErrorMessage *string    `db:"error_message"`
	CreatedAt    time.Time  `db:"created_at"`
	UpdatedAt    time.Time  `db:"updated_at"`
}

// DayCutStatus 日切状态
const (
	DayCutStatusPending    int8 = 0 // 未开始
	DayCutStatusProcessing int8 = 1 // 进行中
	DayCutStatusCompleted  int8 = 2 // 已完成
	DayCutStatusFailed     int8 = 3 // 失败
)

// AsyncTask 异步任务
//
// 幂等键：RequestID。AsyncRecordEntry / Kafka 消费者重复触发时按 RequestID
// 唯一约束（数据库索引 uniq_request_id）防止重复入账。同 RequestID 的二次
// CreateTask 直接 ON DUPLICATE KEY 跳过，返回首次结果。
type AsyncTask struct {
	ID            int64      `db:"id"`
	TaskID        string     `db:"task_id"`
	TaskType      string     `db:"task_type"`
	BusinessNo    string     `db:"business_no"`
	// RequestID 业务侧幂等键，必填。唯一索引 uniq_request_id_type 在 (request_id,
	// task_type) 联合上保证：同一 request_id 不同 task_type 各自独立；同一
	// task_type 同一 request_id 必然命中已有任务。
	RequestID     string     `db:"request_id"     gorm:"column:request_id;type:varchar(64);not null;uniqueIndex:uniq_request_id_type,priority:1"`
	TaskData      string     `db:"task_data"`
	Status        int8       `db:"status"`
	RetryCount    int        `db:"retry_count"`
	MaxRetryCount int        `db:"max_retry_count"`
	NextRetryTime *time.Time `db:"next_retry_time"`
	ErrorMessage  *string    `db:"error_message"`
	CreatedAt     time.Time  `db:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at"`
}

// AsyncTaskStatus 异步任务状态
const (
	AsyncTaskStatusPending       int8 = 0 // 待处理
	AsyncTaskStatusProcessing    int8 = 1 // 处理中
	AsyncTaskStatusSuccess       int8 = 2 // 成功
	AsyncTaskStatusFailed        int8 = 3 // 失败
	AsyncTaskStatusPendingManual int8 = 4 // 待人工处理（超过最大重试次数）
)

// AsyncTaskType 异步任务类型
const (
	AsyncTaskTypeAccounting string = "ACCOUNTING" // 记账任务
	AsyncTaskTypeSnapshot   string = "SNAPSHOT"   // 快照任务
	AsyncTaskTypeDayCut     string = "DAY_CUT"    // 日切任务
)

// AccountBalanceBuffer 账户余额缓冲聚合记录
//
// 用于平台/中间账户及 buffer_account_config 中配置的账户：
//   - 写流水时不立即更新 account.balance，而是在此表累计 pending_delta
//   - 后台 flush worker 按 flush_scheduled_at 触发（任务记录），或 pending_count >= BufferFlushThreshold 时立即触发
//   - flush_scheduled_at：首次写入时设定（now + flush_interval_level + jitter），执行后自动延后一个间隔
//   - 分片规则与 account 表相同（按 account_no 前3字符路由）
//
// pending_delta 单位：ISO 最小货币单位 × 100（参见 currency 包）
type AccountBalanceBuffer struct {
	AccountNo        string     `gorm:"column:account_no;primaryKey"`
	PendingDelta     int64      `gorm:"column:pending_delta"`
	PendingCount     int        `gorm:"column:pending_count"`
	FlushScheduledAt *time.Time `gorm:"column:flush_scheduled_at"` // nil = flush immediately (backward compat)
	LastUpdatedAt    time.Time  `gorm:"column:last_updated_at;autoUpdateTime"`
}

// BufferFlushThreshold 触发余额刷新的最小待刷新笔数（达到此数量立即刷新，不等待时间间隔）
const BufferFlushThreshold = 100

// BufferFlushInterval 平台/中间账户的默认强制刷新间隔（秒）
// buffer_account_config 中配置了 flush_interval_level 的账户使用各自的间隔。
const BufferFlushInterval = 30 // 秒

// BufferBatchSize worker 每次扫描处理的最大账户数，防止大量账户同时到期时产生突发负载。
// 未处理的账户会在下一个 tick（30 秒后）继续处理。
const BufferBatchSize = 100
