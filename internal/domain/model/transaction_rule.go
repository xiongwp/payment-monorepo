package model

import "time"

// AccountTypeInfo 账户类型信息（全局配置表）
type AccountTypeInfo struct {
	ID               int64     `gorm:"column:id;primaryKey"       json:"id"`
	AccountType      string    `gorm:"column:account_type"        json:"account_type"`        // 账户类型唯一码，如 "USER_WALLET"
	AccountTypeName  string    `gorm:"column:account_type_name"   json:"account_type_name"`
	AccountTypeDesc  string    `gorm:"column:account_type_desc"   json:"account_type_desc"`
	OwnerType        int       `gorm:"column:owner_type"          json:"owner_type"`           // 账户所有者类型，对应 account.account_type int
	IsPlatform       int8      `gorm:"column:is_platform"         json:"is_platform"`          // 1=平台内部类型，0=业务账户类型
	BalanceDirection string    `gorm:"column:balance_direction"   json:"balance_direction"`    // C=贷方正常余额, D=借方正常余额
	Description      string    `gorm:"column:description"         json:"description"`
	Extra            string    `gorm:"column:extra"               json:"extra,omitempty"`
	CreateTime       time.Time `gorm:"column:create_time"         json:"create_time"`
	UpdateTime       time.Time `gorm:"column:update_time"         json:"update_time"`
}

func (AccountTypeInfo) TableName() string { return "account_type_info" }

// TransactionRule 交易规则（全局配置表）
// 通过 product_code + event_code 确定记账科目和方向
type TransactionRule struct {
	ID               int64     `gorm:"column:id;primaryKey"`
	ProductCode      string    `gorm:"column:product_code"`      // 产品编码
	EventCode        string    `gorm:"column:event_code"`        // 事件编码
	HashKey          string    `gorm:"column:hash_key"`          // 唯一索引键
	CreditSubjectID  string    `gorm:"column:credit_subject_id"` // 贷方科目，对应 account_type_info.account_type
	DebitSubjectID   string    `gorm:"column:debit_subject_id"`  // 借方科目，对应 account_type_info.account_type
	FromDirection    string    `gorm:"column:from_direction"`    // from方账户方向: debit/credit
	ToDirection      string    `gorm:"column:to_direction"`      // to方账户方向: debit/credit
	TransactionType  int       `gorm:"column:transaction_type"`  // 交易类型
	BookkeepingMode  string    `gorm:"column:bookkeeping_mode"`  // 记账模式
	Extra            string    `gorm:"column:extra"`
	CreateTime       time.Time `gorm:"column:create_time"`
	UpdateTime       time.Time `gorm:"column:update_time"`
}

func (TransactionRule) TableName() string { return "transaction_rule" }

// MerchantInfo 商户信息（全局配置表）
type MerchantInfo struct {
	ID                 int64     `gorm:"column:id;primaryKey"`
	MerchantID         int64     `gorm:"column:merchant_id"`
	MerchantName       string    `gorm:"column:merchant_name"`
	AccountIDC         string    `gorm:"column:account_idc"`
	ExternalMerchantID string    `gorm:"column:external_merchant_id"`
	Mid                string    `gorm:"column:mid"`
	BusinessID         string    `gorm:"column:business_id"`
	Extra              string    `gorm:"column:extra"`
	CreateTime         time.Time `gorm:"column:create_time"`
	UpdateTime         time.Time `gorm:"column:update_time"`
}

func (MerchantInfo) TableName() string { return "merchant_info" }

// TransactionOrder 交易订单主表（分库分表，按 business_no 后2位路由，n=businessNo%100, db=n/10, table=n）
// 幂等键：(order_no, business_type, business_no)
// 设计：主表仅保留状态字段（行宽小→缓冲池命中率高）；扩展数据存 TransactionOrderExtra。
// Extra 字段在 Go 中保留（gorm:"-"），由 Repository 层从 transaction_order_extra 表加载，
// 服务层可透明读写，无需感知底层分表存储。
type TransactionOrder struct {
	ID            int64     `gorm:"column:id;primaryKey;autoIncrement"`
	OrderNo       string    `gorm:"column:order_no"`        // 外部幂等订单号（由调用方提供）
	BusinessNo    string    `gorm:"column:business_no"`     // 业务订单号（分片键）
	BusinessType  string    `gorm:"column:business_type"`   // 业务类型
	// 业务元数据（固定大小 VARCHAR，不影响行宽性能；供 transaction_service 使用）
	ProductCode   string    `gorm:"column:product_code"`
	EventCode     string    `gorm:"column:event_code"`
	FromPartyID   int64     `gorm:"column:from_party_id"`
	FromPartyType string    `gorm:"column:from_party_type"` // "user" | "merchant" | "platform"
	ToPartyID     int64     `gorm:"column:to_party_id"`
	ToPartyType   string    `gorm:"column:to_party_type"`
	Amount        string    `gorm:"column:amount"`          // string 存储避免精度丢失
	Currency      string    `gorm:"column:currency"`
	Status        int8      `gorm:"column:status"`          // 见 TransactionOrderStatus* 常量
	RetryCount    int       `gorm:"column:retry_count"`
	MaxRetryCount int       `gorm:"column:max_retry_count"`
	VoucherNo     string    `gorm:"column:voucher_no"`      // 记账凭证号（成功后写入）
	ErrorMessage  string    `gorm:"column:error_message"`
	Description   string    `gorm:"column:description"`
	CreatedAt     time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt     time.Time `gorm:"column:updated_at;autoUpdateTime"`
	// Extra 由 Repository 从 transaction_order_extra 表加载，不是主表 DB column
	Extra         string    `gorm:"-"`
}

// TransactionOrderExtra 交易订单扩展字段表（与主表同分片）
// Extra 存储：req_hash/voucher_no/tx_ids/req_params JSON，最长 4096 字符
// 热路径（状态查询/更新）不访问此表；仅创建、重试、审计时读写
const TransactionOrderExtraMaxLen = 4096

type TransactionOrderExtra struct {
	ID           int64     `gorm:"column:id;primaryKey;autoIncrement"`
	OrderNo      string    `gorm:"column:order_no"`
	BusinessNo   string    `gorm:"column:business_no"`
	BusinessType string    `gorm:"column:business_type"`
	Extra        string    `gorm:"column:extra"`           // JSON，最长 4096 字符
	CreatedAt    time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt    time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// BatchOrder 原子批量记账订单（全局表）
// 批次内各单笔记账通过 TCC 保证整体原子性
type BatchOrder struct {
	ID           int64     `gorm:"column:id;primaryKey;autoIncrement"`
	BatchID      string    `gorm:"column:batch_id"`        // 批次幂等ID
	BusinessNo   string    `gorm:"column:business_no"`
	ItemCount    int       `gorm:"column:item_count"`
	Status       int8      `gorm:"column:status"`          // 同 TransactionOrderStatus*
	ErrorMessage string    `gorm:"column:error_message"`
	Description  string    `gorm:"column:description"`
	Extra        string    `gorm:"column:extra"`           // JSON 批次请求参数快照，最长 4096 字符
	CreatedAt    time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt    time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (BatchOrder) TableName() string { return "batch_order" }

// TransactionOrder 状态常量
const (
	TransactionOrderStatusPending    int8 = 0 // 待处理
	TransactionOrderStatusProcessing int8 = 1 // 处理中
	TransactionOrderStatusSuccess    int8 = 2 // 成功
	TransactionOrderStatusFailed     int8 = 3 // 失败
)

// PartyType 参与方类型
const (
	PartyTypeUser     = "user"
	PartyTypeMerchant = "merchant"
	PartyTypePlatform = "platform" // 平台方（owner_type=3/4），不绑定具体 party_id
)

// BookkeepingDirection 记账方向
const (
	BookkeepingDirectionDebit  = "debit"
	BookkeepingDirectionCredit = "credit"
)

// TransactionOrderTypeFreeze 冻结订单类型（区别于 TCC 记账订单）
// 状态机：PENDING(0) → PROCESSING/FROZEN(1) → SUCCESS(2) / FAILED(3)
// 语义：FROZEN(1)=资金已冻结; SUCCESS(2)=解冻并扣款; FAILED(3)=解冻并返还
const TransactionOrderTypeFreeze = "freeze"
