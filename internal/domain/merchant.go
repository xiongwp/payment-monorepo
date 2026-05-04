package domain

import (
	"errors"
	"time"
)

// ErrMerchantNotFound merchant 不存在
var ErrMerchantNotFound = errors.New("merchant not found")

// ErrMerchantKYCInvalidTransition KYC 状态机非法跃迁
var ErrMerchantKYCInvalidTransition = errors.New("invalid kyc status transition")

// ─── KYC 状态机 ───────────────────────────────────────────────────────────────

// KYCStatus KYC 审核状态
type KYCStatus string

const (
	KYCStatusPending       KYCStatus = "pending"         // 商户刚注册，未提交资料
	KYCStatusSubmitted     KYCStatus = "submitted"       // 已提交全部材料，排队审核
	KYCStatusReviewing     KYCStatus = "reviewing"       // 审核中（人工/自动）
	KYCStatusNeedsMoreInfo KYCStatus = "needs_more_info" // 需补材料 → 商户重传后回 submitted
	KYCStatusApproved      KYCStatus = "approved"        // 通过；商户可以收单
	KYCStatusRejected      KYCStatus = "rejected"        // 终态（材料不实/风控拒绝）
	KYCStatusSuspended     KYCStatus = "suspended"       // 临时暂停（可恢复 → approved）
	KYCStatusTerminated    KYCStatus = "terminated"      // 终态（合规终止 / 商户主动注销）
)

// MerchantStatus 业务可用状态（与 KYC 解耦：approved+suspended 是 status=suspended）
type MerchantStatus string

const (
	MerchantStatusPending    MerchantStatus = "pending"
	MerchantStatusActive     MerchantStatus = "active"
	MerchantStatusSuspended  MerchantStatus = "suspended"
	MerchantStatusTerminated MerchantStatus = "terminated"
)

// kycTransitions FSM 表（来源 → 允许去到的集合）。
// 真实生产系统这种表应该跟法务/合规 review 一遍。
var kycTransitions = map[KYCStatus]map[KYCStatus]bool{
	KYCStatusPending: {
		KYCStatusSubmitted: true,
		KYCStatusRejected:  true, // 极端情况：注册时即被风控拒绝
	},
	KYCStatusSubmitted: {
		KYCStatusReviewing:     true,
		KYCStatusNeedsMoreInfo: true,
		KYCStatusRejected:      true,
	},
	KYCStatusReviewing: {
		KYCStatusApproved:      true,
		KYCStatusRejected:      true,
		KYCStatusNeedsMoreInfo: true,
	},
	KYCStatusNeedsMoreInfo: {
		KYCStatusSubmitted: true, // 商户补完材料重新提交
		KYCStatusRejected:  true, // 长时间不补 → 拒绝
	},
	KYCStatusApproved: {
		KYCStatusSuspended:  true, // 风控/合规暂停
		KYCStatusTerminated: true,
		KYCStatusReviewing:  true, // 重新核查（如 EDD 升级）
	},
	KYCStatusSuspended: {
		KYCStatusApproved:   true, // 解除暂停
		KYCStatusTerminated: true,
		KYCStatusReviewing:  true,
	},
	// rejected/terminated 是终态，无出边
}

// CanTransition 判断 KYC 状态跃迁是否合法
func (s KYCStatus) CanTransition(to KYCStatus) bool {
	allowed, ok := kycTransitions[s]
	if !ok {
		return false
	}
	return allowed[to]
}

// IsApproved approved 是商户能收单的唯一前提
func (s KYCStatus) IsApproved() bool { return s == KYCStatusApproved }

// IsTerminal 是否终态（不再变化）
func (s KYCStatus) IsTerminal() bool {
	return s == KYCStatusRejected || s == KYCStatusTerminated
}

// ─── 实体 ─────────────────────────────────────────────────────────────────────

// Merchant 商户主表实体（meta 库，非分片）
type Merchant struct {
	ID           string `gorm:"column:id;primaryKey;type:varchar(32)"      json:"id"`
	Name         string `gorm:"column:name;type:varchar(128)"              json:"name"`
	LegalName    string `gorm:"column:legal_name;type:varchar(256)"        json:"legal_name,omitempty"`
	Country      string `gorm:"column:country;type:char(2)"                json:"country"`
	BusinessType string `gorm:"column:business_type;type:varchar(16)"      json:"business_type"`
	TaxID        string `gorm:"column:tax_id;type:varchar(64)"             json:"tax_id,omitempty"`
	ContactEmail string `gorm:"column:contact_email;type:varchar(128)"     json:"contact_email"`
	ContactPhone string `gorm:"column:contact_phone;type:varchar(32)"      json:"contact_phone,omitempty"`
	Website      string `gorm:"column:website;type:varchar(256)"           json:"website,omitempty"`
	MCC          string `gorm:"column:mcc;type:varchar(8)"                 json:"mcc,omitempty"`

	// API 鉴权 — 只存 hash，明文只在签发时返回一次。
	LiveKeyHash string `gorm:"column:live_key_hash;type:varchar(64)"      json:"-"`
	TestKeyHash string `gorm:"column:test_key_hash;type:varchar(64)"      json:"-"`

	// 出站 webhook
	WebhookURL    string `gorm:"column:webhook_url;type:varchar(512)"       json:"webhook_url,omitempty"`
	WebhookSecret string `gorm:"column:webhook_secret;type:varchar(64)"     json:"-"`

	// KYC
	KYCStatus     KYCStatus  `gorm:"column:kyc_status;type:varchar(24)"         json:"kyc_status"`
	KYCLevel      int        `gorm:"column:kyc_level"                           json:"kyc_level"`
	KYCReason     string     `gorm:"column:kyc_reason;type:varchar(512)"        json:"kyc_reason,omitempty"`
	KYCReviewer   string     `gorm:"column:kyc_reviewer;type:varchar(64)"       json:"kyc_reviewer,omitempty"`
	KYCReviewedAt *time.Time `gorm:"column:kyc_reviewed_at"                     json:"kyc_reviewed_at,omitempty"`

	// 风控 / 限流
	RiskTier     string `gorm:"column:risk_tier;type:varchar(16)"          json:"risk_tier"`
	RateLimitRPS int    `gorm:"column:rate_limit_rps"                      json:"rate_limit_rps"`

	// 结算
	SettleCurrency string `gorm:"column:settle_currency;type:char(3)"        json:"settle_currency"`
	SettleMethod   string `gorm:"column:settle_method;type:varchar(16)"      json:"settle_method,omitempty"`
	SettleAccount  string `gorm:"column:settle_account;type:varchar(128)"    json:"settle_account,omitempty"`
	SettleBank     string `gorm:"column:settle_bank;type:varchar(128)"       json:"settle_bank,omitempty"`
	SettleHolder   string `gorm:"column:settle_holder;type:varchar(128)"     json:"settle_holder,omitempty"`

	Status   MerchantStatus `gorm:"column:status;type:varchar(16)"             json:"status"`
	Metadata Metadata       `gorm:"column:metadata;type:json"                  json:"metadata,omitempty"`
	// autoCreateTime / autoUpdateTime 让 GORM 在 INSERT/UPDATE 时自动填；MySQL 8
	// 严格模式下 Go time.Time{} 会被序列化为 "0000-00-00"，直接触发 1292 错误。
	Created time.Time `gorm:"column:created;autoCreateTime"             json:"created"`
	Updated time.Time `gorm:"column:updated;autoUpdateTime"             json:"updated"`
	// DeletedAt 软删标记；非 nil 后默认查询会过滤掉。retention sweeper 会把
	// DeletedAt + retention 窗口之前的行真删。
	DeletedAt *time.Time `gorm:"column:deleted_at"                     json:"-"`
}

// TableName GORM
func (Merchant) TableName() string { return "merchants" }

// MerchantKYCDocument KYC 文档元信息（文件正文走对象存储）
type MerchantKYCDocument struct {
	ID           string     `gorm:"column:id;primaryKey;type:varchar(32)"  json:"id"`
	MerchantID   string     `gorm:"column:merchant_id;type:varchar(32)"    json:"merchant_id"`
	DocType      string     `gorm:"column:doc_type;type:varchar(32)"       json:"doc_type"`
	DocNumber    string     `gorm:"column:doc_number;type:varchar(128)"    json:"doc_number,omitempty"`
	FileURL      string     `gorm:"column:file_url;type:varchar(512)"      json:"file_url"`
	MimeType     string     `gorm:"column:mime_type;type:varchar(64)"      json:"mime_type,omitempty"`
	SizeBytes    int        `gorm:"column:size_bytes"                      json:"size_bytes,omitempty"`
	UploadedBy   string     `gorm:"column:uploaded_by;type:varchar(64)"    json:"uploaded_by,omitempty"`
	ReviewStatus string     `gorm:"column:review_status;type:varchar(16)"  json:"review_status"`
	ReviewNote   string     `gorm:"column:review_note;type:varchar(512)"   json:"review_note,omitempty"`
	ExpiresAt    *time.Time `gorm:"column:expires_at"                      json:"expires_at,omitempty"`
	Created      time.Time  `gorm:"column:created;autoCreateTime"          json:"created"`
	Updated      time.Time  `gorm:"column:updated;autoUpdateTime"          json:"updated"`
}

// TableName GORM
func (MerchantKYCDocument) TableName() string { return "merchant_kyc_document" }

// MerchantKYCAudit KYC 状态流转日志
type MerchantKYCAudit struct {
	ID         int64     `gorm:"column:id;primaryKey;autoIncrement"   json:"id"`
	MerchantID string    `gorm:"column:merchant_id;type:varchar(32)"  json:"merchant_id"`
	FromStatus KYCStatus `gorm:"column:from_status;type:varchar(24)"  json:"from_status"`
	ToStatus   KYCStatus `gorm:"column:to_status;type:varchar(24)"    json:"to_status"`
	Reason     string    `gorm:"column:reason;type:varchar(512)"      json:"reason,omitempty"`
	Actor      string    `gorm:"column:actor;type:varchar(64)"        json:"actor,omitempty"`
	Created    time.Time `gorm:"column:created;autoCreateTime"        json:"created"`
}

// TableName GORM
func (MerchantKYCAudit) TableName() string { return "merchant_kyc_audit" }
