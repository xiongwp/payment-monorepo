// Package domain — AML 筛查的核心领域类型.
//
// 三大场景:
//   1) screen subject — 入网 (KYB/KYC), 大额出款, 高风险跨境 trigger
//   2) bulk-list ingest — 每日同步 OFAC SDN / EU consolidated / UK HMT / PEP 库
//   3) hit lifecycle — match → review → cleared|frozen → escalated 状态机
//
// 设计原则:
//   - 所有匹配结果落表, 不丢; 审计可追溯
//   - subject 自身**不存** secondary identifier (DOB / national_id) 明文 →
//     哈希一份做匹配 key, 原文留在调用方 KYC 库
//   - hit confidence 0..100; >= threshold (默认 85) 阻断业务

package domain

import "time"

// SubjectType 被筛查实体的种类
type SubjectType string

const (
	SubjectIndividual SubjectType = "individual" // 个人 (KYC)
	SubjectEntity     SubjectType = "entity"     // 公司/组织 (KYB)
	SubjectAddress    SubjectType = "address"    // 收款地址 (跨境出款)
	SubjectVessel     SubjectType = "vessel"     // 船只 (制裁名单也覆盖)
	SubjectAircraft   SubjectType = "aircraft"
)

// ListSource 名单来源
type ListSource string

const (
	SourceOFACSDN         ListSource = "ofac_sdn"         // 美国财政部 OFAC 制裁名单
	SourceOFACConsolidated ListSource = "ofac_cons"       // OFAC 综合名单 (非 SDN)
	SourceEUConsolidated  ListSource = "eu_cons"          // EU 综合制裁
	SourceUKHMT           ListSource = "uk_hmt"           // UK HM Treasury
	SourceUN              ListSource = "un_sc"            // UN 安理会
	SourcePEP             ListSource = "pep"              // 政治敏感人物 (商用库)
	SourceAdverseMedia    ListSource = "adverse_media"    // 负面媒体 (高级筛)
	SourceInternalBlock   ListSource = "internal_block"   // 自有黑名单
)

// AllExternalSources 所有非内部源 (bulk-list ingest 范围)
var AllExternalSources = []ListSource{
	SourceOFACSDN, SourceOFACConsolidated, SourceEUConsolidated,
	SourceUKHMT, SourceUN, SourcePEP, SourceAdverseMedia,
}

// ListEntry 名单中的单条目 — 制裁 / PEP / 负面媒体共用
type ListEntry struct {
	ID          string       `json:"id"`           // 源给的稳定 ID, 例: OFAC-12345
	Source      ListSource   `json:"source"`
	EntityType  SubjectType  `json:"entity_type"`
	PrimaryName string       `json:"primary_name"` // canonical 名 (已标准化)
	Aliases     []string     `json:"aliases"`      // a.k.a. 别名 (含标准化后版)
	DOB         string       `json:"dob"`          // YYYY-MM-DD 或 YYYY (个人)
	BirthPlace  string       `json:"birth_place"`
	Nationality []string     `json:"nationality"`  // ISO 3166-1 alpha-2 列表
	Addresses   []string     `json:"addresses"`    // 标准化地址
	IDDocuments []IDDoc      `json:"id_documents"` // 护照/身份证号 (哈希)
	Program     string       `json:"program"`      // 制裁项目代码 (CYBER2 / IRAN / NK …)
	Remarks     string       `json:"remarks"`
	ListedAt    time.Time    `json:"listed_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Raw         string       `json:"-"`            // 原始 XML / JSON 节, 留 audit
}

// IDDoc 证件 - 名单库都用 hash 不存明文
type IDDoc struct {
	Type     string `json:"type"`      // PASSPORT / NID / TIN
	NumberH  string `json:"number_h"`  // sha256(number)[:16]
	Country  string `json:"country"`
}

// ScreenRequest 入站筛查请求 — 调用方填能拿到的所有字段, 越多越准
type ScreenRequest struct {
	RequestID   string      `json:"request_id"`             // 业务侧幂等 key (1h dedup)
	Trigger     string      `json:"trigger"`                // kyb_onboarding / payout / high_value_tx
	Subject     SubjectType `json:"subject"`
	Name        string      `json:"name"`                   // 必填
	DOB         string      `json:"dob,omitempty"`          // YYYY-MM-DD
	Nationality string      `json:"nationality,omitempty"`  // ISO-2
	Addresses   []string    `json:"addresses,omitempty"`
	IDNumber    string      `json:"id_number,omitempty"`    // 调用方应在传入前哈希, 但 server 二次保护
	IDType      string      `json:"id_type,omitempty"`
	IDCountry   string      `json:"id_country,omitempty"`
	MerchantID  string      `json:"merchant_id,omitempty"`  // 关联业务实体
	OrderID     string      `json:"order_id,omitempty"`
	Amount      int64       `json:"amount,omitempty"`       // 分; payout/tx 才填
	Currency    string      `json:"currency,omitempty"`
}

// ScreenResult screening 决策
//
// Action 三态: pass | review | block. 业务侧根据 action 决定放行 / 转人工 / 拒绝.
// Hits 是所有命中的细节, 给 ops/compliance 审单用.
type ScreenResult struct {
	RequestID   string    `json:"request_id"`
	Action      Action    `json:"action"`
	HighestHit  int       `json:"highest_hit"`  // 0-100
	Hits        []HitInfo `json:"hits"`
	ScreenedAt  time.Time `json:"screened_at"`
	Sources     []ListSource `json:"sources"`   // 本次跑了哪些库
	Latency_ms  int64     `json:"latency_ms"`
}

// Action 决策
type Action string

const (
	ActionPass   Action = "pass"
	ActionReview Action = "review"
	ActionBlock  Action = "block"
)

// HitInfo 单次命中
type HitInfo struct {
	HitID      string     `json:"hit_id"`      // server 生成稳定 ID, 给 review 工单引用
	ListEntry  ListEntry  `json:"list_entry"`  // 命中的名单条目 (冗余存便于 audit)
	Confidence int        `json:"confidence"`  // 0-100
	MatchedOn  []string   `json:"matched_on"`  // ["name","dob","nationality"] — 哪几个字段命中
	Algorithm  string     `json:"algorithm"`   // "jaro_winkler" / "phonetic" / "exact"
	State      HitState   `json:"state"`       // pending_review / cleared / frozen / escalated
}

// HitState 命中状态机
type HitState string

const (
	HitPendingReview HitState = "pending_review"
	HitCleared       HitState = "cleared"        // ops 认定误报
	HitFrozen        HitState = "frozen"         // 真命中, 业务冻结
	HitEscalated     HitState = "escalated"      // 报送监管 (SAR)
)

// HitResolution ops 复核结果 — admin api 写入
type HitResolution struct {
	HitID      string    `json:"hit_id"`
	Reviewer   string    `json:"reviewer"`   // ops 账号 (oauth2 client_id 或员工号)
	Decision   HitState  `json:"decision"`   // cleared / frozen / escalated
	Reason     string    `json:"reason"`
	Evidence   string    `json:"evidence"`   // URL / 内部工单号
	ResolvedAt time.Time `json:"resolved_at"`
}
