// Package domain — GDPR / CCPA 数据主体权利的领域类型.
//
// 法规背景:
//   - GDPR Art. 15 (右获取/access)  — 用户问"你有我的什么数据" → SAR / DSAR
//   - GDPR Art. 17 (右删除/erasure) — 用户要求删除 → RTBF (right to be forgotten)
//   - GDPR Art. 20 (右携带/portability) — 数据机器可读导出
//   - CCPA §1798.105/110/130 — California 类似
//   - 法定时限: 30 天 (可延 60 天, 必须告知)
//
// 跨服务编排:
//   - 一个 request 触发到全平台 service registry 里 "支持 subject access" 的服务
//     的 /internal/data-rights/export 或 /erase
//   - 收集结果 → 合 zip + 加密 → 发邮件 / SFTP / S3 链接
//   - 失败 / 跳过的服务 ops review (可能因法律保留不能删 — 反洗钱 7 年)

package domain

import "time"

// RequestType 请求种类
type RequestType string

const (
	RequestAccess        RequestType = "access"        // GDPR 15 / CCPA 110: 导出所有数据
	RequestErasure       RequestType = "erasure"       // GDPR 17 / CCPA 105: 删除
	RequestPortability   RequestType = "portability"   // GDPR 20: 机器可读 JSON / CSV
	RequestRectification RequestType = "rectification" // GDPR 16: 改正错误数据
	RequestRestriction   RequestType = "restriction"   // GDPR 18: 限制处理 (不删但不再用)
	RequestObjection     RequestType = "objection"     // GDPR 21: 反对 (营销退订)
)

// SubjectType 数据主体身份
type SubjectType string

const (
	SubjectMerchant SubjectType = "merchant"   // 商户 (KYB)
	SubjectCustomer SubjectType = "customer"   // 终端消费者 (KYC)
	SubjectStaff    SubjectType = "staff"      // 员工 (ops/admin)
)

// Jurisdiction 适用法律 — 决定 SLA / 应答模板
type Jurisdiction string

const (
	JurEU  Jurisdiction = "EU"  // GDPR
	JurUK  Jurisdiction = "UK"  // UK GDPR
	JurCA  Jurisdiction = "CA"  // CCPA (California)
	JurBR  Jurisdiction = "BR"  // LGPD (Brazil)
	JurCN  Jurisdiction = "CN"  // PIPL
	JurOther Jurisdiction = "other"
)

// State 跨服务工单状态机.
//
//   received   → 收到请求, 校验身份
//   verifying  → 验证身份 (邮箱 / 短信 / KYC)
//   collecting → fan-out 到各 service, 等响应
//   review     → 全部回来, ops 审查 (有没有需要保留的数据? 7y 反洗钱)
//   approved   → ops 批准, 执行 (export 收集 / erasure 真删)
//   fulfilled  → 完成, 邮件商户 / 用户结果
//   rejected   → ops 拒绝 (e.g. 法律保留期内不能删)
//   failed     → 系统故障, 部分服务挂了
//   expired    → 30 天过期未处理 (ops 失职; 法定违规需上报)
type State string

const (
	StateReceived   State = "received"
	StateVerifying  State = "verifying"
	StateCollecting State = "collecting"
	StateReview     State = "review"
	StateApproved   State = "approved"
	StateFulfilled  State = "fulfilled"
	StateRejected   State = "rejected"
	StateFailed     State = "failed"
	StateExpired    State = "expired"
)

// Request 用户提交的 DSAR
type Request struct {
	RequestID    string       `json:"request_id"`           // dsar_<32hex>
	Type         RequestType  `json:"type"`
	Subject      Subject      `json:"subject"`
	Jurisdiction Jurisdiction `json:"jurisdiction"`
	State        State        `json:"state"`
	SubmittedAt  time.Time    `json:"submitted_at"`
	DeadlineAt   time.Time    `json:"deadline_at"`           // 自动算 +30d
	Verification Verification `json:"verification"`

	// 关联工单
	ServiceStatuses []ServiceStatus `json:"service_statuses"` // 每个 service 的进度

	// fulfillment
	ApprovedBy   string    `json:"approved_by,omitempty"`
	ApprovedAt   time.Time `json:"approved_at,omitempty"`
	FulfilledAt  time.Time `json:"fulfilled_at,omitempty"`
	RejectReason string    `json:"reject_reason,omitempty"`
	ExportURL    string    `json:"export_url,omitempty"`     // S3 presigned 7d
	ExportSHA256 string    `json:"export_sha256,omitempty"`
}

// Subject 数据主体身份
type Subject struct {
	Type       SubjectType `json:"type"`
	ID         string      `json:"id"`               // merchant_id / customer_id / staff_id (主键)
	Email      string      `json:"email,omitempty"`  // 验证用; 不存明文长期 (hash)
	Phone      string      `json:"phone,omitempty"`
	LegalName  string      `json:"legal_name,omitempty"`
	Country    string      `json:"country,omitempty"` // ISO-2; 决定 Jurisdiction default
}

// Verification 身份验证证据 (法律强制 — 不验直接给数据是大事故)
type Verification struct {
	Method        string    `json:"method"`         // email_otp / sms_otp / kyc_recheck / passport_upload
	VerifiedAt    time.Time `json:"verified_at,omitempty"`
	VerifierID    string    `json:"verifier_id,omitempty"` // ops 复核员
	EvidenceRef   string    `json:"evidence_ref,omitempty"` // S3 doc ref / 短信 audit id
}

// ServiceStatus 一个 downstream service 的处理状态
type ServiceStatus struct {
	Service    string    `json:"service"`        // "user-merchant-core", "payment-core", ...
	Endpoint   string    `json:"endpoint"`       // 调的 URL
	State      State     `json:"state"`
	Attempts   int       `json:"attempts"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	Error      string    `json:"error,omitempty"`
	ExportSize int64     `json:"export_size,omitempty"`     // 字节
	ExportSHA  string    `json:"export_sha,omitempty"`
	Held       bool      `json:"held,omitempty"`             // 数据被法律保留期挡住, 不能删
	HoldReason string    `json:"hold_reason,omitempty"`
}

// LegalHold 法律保留 — RTBF 不能 100% 删的兜底.
//
// 例:
//   - 反洗钱要 7y 交易记录 → erasure 完成但 AML log 留到 7y 后
//   - 财务税务 SOX 要 7y → 一样
//   - 反欺诈黑名单 → 永久
//
// 服务的 /internal/erase 端点应当返回 partial=true + held_fields=[...]
// data-rights 把它聚合到 request 的 ServiceStatus.Held.
type LegalHold struct {
	Reason      string    `json:"reason"`     // "AML 7y retention" / "Tax 7y retention" / "Permanent blacklist"
	UntilDate   time.Time `json:"until_date"`
	Fields      []string  `json:"fields"`     // 受保留的字段名
}

// ServiceRegistry 注册哪些服务支持 data-rights endpoints.
// 真生产配置中心读; dev hard-coded.
type ServiceRegistry struct {
	Services []ServiceEntry `json:"services"`
}

type ServiceEntry struct {
	Name             string      `json:"name"`
	BaseURL          string      `json:"base_url"`            // 内网 URL
	SupportsAccess   bool        `json:"supports_access"`
	SupportsErasure  bool        `json:"supports_erasure"`
	SubjectTypes     []SubjectType `json:"subject_types"`     // 只对这些 subject 有数据
	AuthHeader       string      `json:"auth_header"`         // X-Service-Token 之类
}
