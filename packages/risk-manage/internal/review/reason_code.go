// reason_code.go: 结构化 review decision reason code（enum）。
//
// 现状（旧）：Decide(reason string) 只是 free-text，DBA 看到一堆"看着像 fraud"
// "card velocity"无法聚合。要算"误杀率 / 真欺诈率 / chargeback 关联率"全
// 靠人工字符串归类。
//
// 改造：Decide 必须带一个枚举 reason_code（拉自下面 12 个），外加可选 free
// text 详述（DecideReason）。ML offline 训练 / BI 报表都靠 reason_code 聚合。
//
// 选 12 个的依据是历史 dispute / chargeback 工单的人工 tag 分布（top-N
// 覆盖 95%+），剩下走 "other"+ 强制 free text。
//
// i18n 双语支持：admin-web 通过 GET /admin/review/reason-codes 拉到 label
// 字典；新加语种只改这里 + 重启服务。
package review

// ReasonCode 风控人工决议的结构化原因。新增前请走变更评审：影响 ML 训练
// 标签 + BI 看板拆分 + 跟 chargeback 工单的 join key。
type ReasonCode string

const (
	ReasonFraudConfirmed    ReasonCode = "fraud_confirmed"     // 真欺诈，确认 fraud（reject）
	ReasonFraudSuspected    ReasonCode = "fraud_suspected"     // 可疑 fraud（一般触发升 L2 进一步）
	ReasonTestCard          ReasonCode = "test_card"           // 测试卡 / sandbox 流量
	ReasonFalsePositive     ReasonCode = "false_positive"      // 误杀，应放行（approve）
	ReasonChargebackDispute ReasonCode = "chargeback_dispute"  // 拒付争议关联
	ReasonAccountTakeover   ReasonCode = "account_takeover"    // 账号被盗（reject）
	ReasonPolicyViolation   ReasonCode = "policy_violation"    // 违反风控政策（合规 reject）
	ReasonDuplicate         ReasonCode = "duplicate"           // 重复交易
	ReasonInsufficientData  ReasonCode = "insufficient_data"   // 信息不足，让用户补
	ReasonUserAuthorized    ReasonCode = "user_authorized"     // 用户确认是本人操作（approve）
	ReasonCustomerRequested ReasonCode = "customer_requested"  // 客户主动要求
	ReasonOther             ReasonCode = "other"               // 其它（必须配合 DecideReason free text）
)

// reasonCodeSet 用于 O(1) 校验。
var reasonCodeSet = map[ReasonCode]struct{}{
	ReasonFraudConfirmed: {}, ReasonFraudSuspected: {}, ReasonTestCard: {},
	ReasonFalsePositive: {}, ReasonChargebackDispute: {}, ReasonAccountTakeover: {},
	ReasonPolicyViolation: {}, ReasonDuplicate: {}, ReasonInsufficientData: {},
	ReasonUserAuthorized: {}, ReasonCustomerRequested: {}, ReasonOther: {},
}

// ReasonCodeLabel 一个 code 的多语种 label。前端下拉项展示用。
type ReasonCodeLabel struct {
	Code  ReasonCode `json:"code"`
	ZhCN  string     `json:"zh_CN"`
	EnUS  string     `json:"en_US"`
	// HintFreeText: true 表示运营选这个 code 后必须再补 DecideReason 详述（如 "other"）。
	HintFreeText bool `json:"hint_free_text,omitempty"`
}

// reasonCodeLabels 双语 label 字典，按 UI 想要的展示顺序排（fraud 先，"other"
// 殿后）。改顺序不破坏 ABI，前端就按这数组渲染。
var reasonCodeLabels = []ReasonCodeLabel{
	{Code: ReasonFraudConfirmed, ZhCN: "确认欺诈", EnUS: "Fraud confirmed"},
	{Code: ReasonFraudSuspected, ZhCN: "可疑欺诈", EnUS: "Suspected fraud"},
	{Code: ReasonAccountTakeover, ZhCN: "账号被盗", EnUS: "Account takeover"},
	{Code: ReasonChargebackDispute, ZhCN: "拒付争议", EnUS: "Chargeback dispute"},
	{Code: ReasonTestCard, ZhCN: "测试卡 / 沙箱", EnUS: "Test card / sandbox"},
	{Code: ReasonDuplicate, ZhCN: "重复交易", EnUS: "Duplicate transaction"},
	{Code: ReasonPolicyViolation, ZhCN: "违反风控政策", EnUS: "Policy violation"},
	{Code: ReasonFalsePositive, ZhCN: "误杀（应放行）", EnUS: "False positive"},
	{Code: ReasonUserAuthorized, ZhCN: "用户已确认本人操作", EnUS: "User authorized"},
	{Code: ReasonCustomerRequested, ZhCN: "客户主动要求", EnUS: "Customer requested"},
	{Code: ReasonInsufficientData, ZhCN: "信息不足，待补充", EnUS: "Insufficient data"},
	{Code: ReasonOther, ZhCN: "其它（请详述）", EnUS: "Other (please specify)", HintFreeText: true},
}

// ReasonCodes 给前端 / API 拉下拉项用。返回 copy（防止调用方改原数组顺序）。
func ReasonCodes() []ReasonCodeLabel {
	out := make([]ReasonCodeLabel, len(reasonCodeLabels))
	copy(out, reasonCodeLabels)
	return out
}

// ValidReasonCode 是否一个合法 enum。空串视为非法（Decide 必传）。
func ValidReasonCode(s string) bool {
	if s == "" {
		return false
	}
	_, ok := reasonCodeSet[ReasonCode(s)]
	return ok
}
