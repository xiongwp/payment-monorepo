// Package domain — KYC/KYB 实体 + 状态机。
//
// KYB (Know Your Business) 是商户 onboarding 必经合规步骤。流程:
//
//   1. 商户提交申请 (公司名 / BR 编号 / 法人姓名 / 行业 / 预期月交易额)
//   2. 上传文件: BR 营业执照 / 法人 ID 正反面 / 公司章程 / 银行账户证明
//   3. 系统调第三方 (Sumsub / Veriff / Onfido / Jumio) verify 文件 + OCR
//   4. 行业 / 国家 / 月交易额触发 risk_tier 评级
//   5. PEP (Politically Exposed Person) + Sanctions list 比对
//   6. 风控 ops 审批 (大额 / 跨境必须人工)
//   7. 通过 → 商户激活 (允许接收 charge)
//
// 持续 monitoring:
//   - 每月扫一次 PEP / 制裁名单更新，命中老商户 → flag review
//   - 异常交易触发重审 (例如 GMV 异常 / 高 chargeback 率)

package domain

import "time"

// Case 一笔 KYC 审核案例。
type Case struct {
	ID              int64     `db:"id" json:"id"`
	CaseNum         string    `db:"case_num" json:"case_num"`           // KYB-2026-001
	MerchantID      string    `db:"merchant_id" json:"merchant_id"`
	CaseType        CaseType  `db:"case_type" json:"case_type"`         // initial / annual_review / triggered
	BusinessName    string    `db:"business_name" json:"business_name"`
	BusinessNumber  string    `db:"business_number" json:"business_number"`  // BR 编号
	BusinessCountry string    `db:"business_country" json:"business_country"`
	IndustryCode    string    `db:"industry_code" json:"industry_code"`  // MCC (Merchant Category Code)
	ExpectedGMVMonth int64    `db:"expected_gmv_month" json:"expected_gmv_month"` // 预期月交易额（美分）
	Status          Status    `db:"status" json:"status"`
	RiskTier        RiskTier  `db:"risk_tier" json:"risk_tier"`
	RiskScore       int       `db:"risk_score" json:"risk_score"`        // 0-100，越高风险越大
	Reviewer        string    `db:"reviewer" json:"reviewer,omitempty"`  // ops email
	RejectReason    string    `db:"reject_reason" json:"reject_reason,omitempty"`
	ApprovedAt      *time.Time `db:"approved_at" json:"approved_at,omitempty"`
	SubmittedAt     time.Time `db:"submitted_at" json:"submitted_at"`
	ExternalCaseID  string    `db:"external_case_id" json:"external_case_id,omitempty"`  // Sumsub/Veriff case_id
	TraceID         string    `db:"trace_id" json:"trace_id,omitempty"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time `db:"updated_at" json:"updated_at"`
}

// CaseType 案例类型。
type CaseType string

const (
	CaseInitial      CaseType = "initial"        // 首次 onboarding
	CaseAnnualReview CaseType = "annual_review"  // 年度复审 (法规要求)
	CaseTriggered    CaseType = "triggered"      // 异常触发 (GMV 异常 / 高 CB / 制裁名单更新)
)

// Status 案例状态机。
//
//   pending_docs        商户还在上传文件
//     │
//     submitted          商户提交完，等系统/人工 review
//       │
//       in_review        ops 正在审核 / 第三方 API 跑中
//         │
//         approved       通过，商户可激活
//         rejected       拒绝（不可逆，商户需重新申请）
//         needs_more     需要补充材料（→ pending_docs）
type Status string

const (
	StatusPendingDocs Status = "pending_docs"
	StatusSubmitted   Status = "submitted"
	StatusInReview    Status = "in_review"
	StatusApproved    Status = "approved"
	StatusRejected    Status = "rejected"
	StatusNeedsMore   Status = "needs_more"
)

// RiskTier 风险等级 (决定费率 / reserve / limit)。
type RiskTier string

const (
	TierLow     RiskTier = "low"      // 实体店 / 低风险行业，按基础费率
	TierMedium  RiskTier = "medium"
	TierHigh    RiskTier = "high"     // 高风险行业 (CBD / 成人 / forex)，留 10% reserve
	TierBlocked RiskTier = "blocked"  // 制裁名单 / PEP / 高风险国家
)

// Document 上传的文件。
type Document struct {
	ID         int64     `db:"id" json:"id"`
	CaseID     int64     `db:"case_id" json:"case_id"`
	Type       DocType   `db:"type" json:"type"`
	FileURL    string    `db:"file_url" json:"file_url"`        // S3 URL
	FileMime   string    `db:"file_mime" json:"file_mime"`
	FileSize   int64     `db:"file_size" json:"file_size"`
	OCRResult  string    `db:"ocr_result" json:"ocr_result,omitempty"`  // JSON 结构化
	Verified   bool      `db:"verified" json:"verified"`
	UploadedBy string    `db:"uploaded_by" json:"uploaded_by"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

// DocType 文件类型。
type DocType string

const (
	DocBusinessRegistration DocType = "business_registration"
	DocLegalRepresentativeID DocType = "legal_rep_id"
	DocCompanyArticles      DocType = "company_articles"
	DocBankStatement        DocType = "bank_statement"
	DocProofOfAddress       DocType = "proof_of_address"
	DocTaxCertificate       DocType = "tax_certificate"
)

// VerificationEvent 第三方验证结果。
type VerificationEvent struct {
	ID         int64     `db:"id" json:"id"`
	CaseID     int64     `db:"case_id" json:"case_id"`
	Provider   string    `db:"provider" json:"provider"`        // sumsub / veriff / onfido / jumio
	CheckType  string    `db:"check_type" json:"check_type"`    // doc_verify / face_match / pep / sanctions
	Result     string    `db:"result" json:"result"`            // pass / fail / inconclusive
	Score      int       `db:"score" json:"score"`              // 0-100
	Details    string    `db:"details" json:"details,omitempty"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

// ValidTransition 状态机白名单。
func ValidTransition(from, to Status) bool {
	terminals := from == StatusApproved || from == StatusRejected
	if terminals {
		return false
	}
	switch from {
	case StatusPendingDocs:
		return to == StatusSubmitted || to == StatusRejected
	case StatusSubmitted:
		return to == StatusInReview || to == StatusNeedsMore
	case StatusInReview:
		return to == StatusApproved || to == StatusRejected || to == StatusNeedsMore
	case StatusNeedsMore:
		return to == StatusPendingDocs || to == StatusSubmitted
	}
	return false
}
