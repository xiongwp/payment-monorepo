// Package catalog — 内置对账规则目录。
//
// 行业 80% 对账场景就那几条经典规则，每家公司重复实现一遍很傻。本包内置 10 条
// 即装即用脚本，admin web 显示 "Built-in Rules" 标签 + 每条 [Install] 按钮，
// 运营选模板填参数 → 写成 user 脚本 → 进 user catalog 跑。
//
// 内置规则清单：
//
//	1.  amount_mismatch        order-core.payment_intent.amount vs accounting account_transaction sum
//	2.  missing_charge_leg     payment_intent SUCCEEDED 但找不到 channel charge
//	3.  orphan_refund          refund 找不到对应 charge
//	4.  duplicate_charge       同一 idempotency_key 在 channel 多次落地
//	5.  settlement_short_long  渠道结算单 vs 内部 charge 金额差
//	6.  webhook_lag            webhook 落库 → channel ack 间隔 > 30s
//	7.  fee_mismatch           计算 fee 跟 channel 上报 fee 差 > 1 cent
//	8.  refund_excess          某 charge 累计 refund > 原 charge.amount
//	9.  state_consistency      payment_intent.status vs charge.status 状态机一致性
//	10. dispute_unhandled      dispute 状态 received > 4h 未处理
//
// 每条规则:
//   - id: 唯一名（snake_case）
//   - title / description: 业务文字
//   - category: settlement / state / amount / sla
//   - severity: P0/P1/P2
//   - parameters: [{name, type, required, default, hint}]
//   - script_template: Go text/template 拼出 Starlark 代码
//   - default_schedule: 推荐 cron 频率
//
// admin web 流程:
//   GET /api/v1/catalog/rules           列表
//   GET /api/v1/catalog/rules/:id       详情 + 模板源码 preview
//   POST /api/v1/catalog/rules/:id/install  body={params: {...}, schedule: ""}
//                                       → 创建 user script，立即可跑

package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"text/template"
)

// Rule 一条内置规则。
type Rule struct {
	ID              string            `json:"id"`
	Title           string            `json:"title"`
	Description     string            `json:"description"`
	Category        string            `json:"category"` // settlement / state / amount / sla / fraud
	Severity        string            `json:"severity"` // P0 / P1 / P2
	Parameters      []RuleParam       `json:"parameters"`
	DefaultSchedule string            `json:"default_schedule"`
	Tags            []string          `json:"tags,omitempty"`
	scriptTpl       *template.Template
}

// RuleParam 规则参数定义。
type RuleParam struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Type     string `json:"type"` // string / int / float / select
	Options  []string `json:"options,omitempty"` // for select
	Default  string `json:"default,omitempty"`
	Required bool   `json:"required,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

// 各 SKILL.md 模板源代码 embed —— 改文件不重 build。
// 注意每条 .star 文件只是 Starlark 模板，admin 安装时 Render 后再 save。

//go:embed scripts/amount_mismatch.star
var tplAmountMismatch string

//go:embed scripts/missing_charge_leg.star
var tplMissingChargeLeg string

//go:embed scripts/orphan_refund.star
var tplOrphanRefund string

//go:embed scripts/duplicate_charge.star
var tplDuplicateCharge string

//go:embed scripts/settlement_short_long.star
var tplSettlementShortLong string

//go:embed scripts/webhook_lag.star
var tplWebhookLag string

//go:embed scripts/fee_mismatch.star
var tplFeeMismatch string

//go:embed scripts/refund_excess.star
var tplRefundExcess string

//go:embed scripts/state_consistency.star
var tplStateConsistency string

//go:embed scripts/dispute_unhandled.star
var tplDisputeUnhandled string

// All 启动时构造的全部规则列表。
var All []*Rule

func init() {
	All = []*Rule{
		mustRule("amount_mismatch", "金额不匹配", "amount", "P0",
			"order-core.payment_intent.amount 与 accounting_system.account_transaction sum 差异",
			"0 */15 * * * *",
			tplAmountMismatch, []RuleParam{
				{Name: "tolerance_minor", Label: "容忍差值（分）", Type: "int", Default: "0", Hint: "默认 0；浮点币种调 1"},
			}),
		mustRule("missing_charge_leg", "缺失渠道支付腿", "state", "P0",
			"PaymentIntent SUCCEEDED 但找不到对应 channel charge",
			"*/30 * * * *",
			tplMissingChargeLeg, []RuleParam{
				{Name: "min_age_min", Label: "最小延迟（分）", Type: "int", Default: "5", Hint: "PI succeed 后多久还无 charge 才报"},
			}),
		mustRule("orphan_refund", "孤儿 refund", "state", "P1",
			"Refund 找不到对应 charge_id",
			"*/30 * * * *",
			tplOrphanRefund, nil),
		mustRule("duplicate_charge", "重复扣款", "amount", "P0",
			"同一 idempotency_key 在 channel 落多条 charge",
			"*/15 * * * *",
			tplDuplicateCharge, nil),
		mustRule("settlement_short_long", "结算长短款", "settlement", "P1",
			"渠道结算单总额 vs 内部 charge 累加差异",
			"0 5 * * *",
			tplSettlementShortLong, []RuleParam{
				{Name: "external_source", Label: "外部源 name", Type: "string", Required: true, Hint: "external.sources 配置里的 name，如 visa_settlement"},
				{Name: "tolerance_minor", Label: "容忍差值", Type: "int", Default: "100", Hint: "默认 100 分"},
			}),
		mustRule("webhook_lag", "Webhook 滞后", "sla", "P2",
			"channel 上报 ack 与 webhook_raw 落库时间差 > 阈值",
			"0 */10 * * * *",
			tplWebhookLag, []RuleParam{
				{Name: "threshold_seconds", Label: "阈值（秒）", Type: "int", Default: "30"},
			}),
		mustRule("fee_mismatch", "手续费差异", "amount", "P1",
			"内部计算 fee vs channel 上报 fee 差 > 阈值",
			"0 */30 * * * *",
			tplFeeMismatch, []RuleParam{
				{Name: "tolerance_minor", Label: "容忍差值（分）", Type: "int", Default: "1"},
			}),
		mustRule("refund_excess", "超额退款", "amount", "P0",
			"某 charge 累计 refund 金额 > 原 charge.amount",
			"*/15 * * * *",
			tplRefundExcess, nil),
		mustRule("state_consistency", "状态机一致性", "state", "P1",
			"payment_intent.status vs charge.status 不应共存的组合",
			"*/30 * * * *",
			tplStateConsistency, nil),
		mustRule("dispute_unhandled", "Dispute 未处理", "sla", "P0",
			"dispute received > N 小时未流转",
			"0 */15 * * * *",
			tplDisputeUnhandled, []RuleParam{
				{Name: "threshold_hours", Label: "阈值（小时）", Type: "int", Default: "4"},
			}),
	}
}

func mustRule(id, title, cat, sev, desc, sched, tplBody string, params []RuleParam) *Rule {
	tpl, err := template.New(id).Parse(tplBody)
	if err != nil {
		panic(fmt.Sprintf("rule %s template parse: %v", id, err))
	}
	return &Rule{
		ID:              id,
		Title:           title,
		Description:     desc,
		Category:        cat,
		Severity:        sev,
		DefaultSchedule: sched,
		Parameters:      params,
		scriptTpl:       tpl,
	}
}

// Get 按 id 取规则。
func Get(id string) *Rule {
	for _, r := range All {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// Render 用参数渲染脚本源码。返 Starlark 代码字符串。
func (r *Rule) Render(params map[string]string) (string, error) {
	// 默认值 fallback
	merged := map[string]string{}
	for _, p := range r.Parameters {
		if p.Default != "" {
			merged[p.Name] = p.Default
		}
	}
	for k, v := range params {
		merged[k] = v
	}
	// required 校验
	for _, p := range r.Parameters {
		if p.Required && merged[p.Name] == "" {
			return "", fmt.Errorf("missing required param %q", p.Name)
		}
	}
	var buf bytesBuffer
	if err := r.scriptTpl.Execute(&buf, merged); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// MarshalJSON 自定义序列化（隐藏 scriptTpl 这个内部字段）。
func (r *Rule) MarshalJSON() ([]byte, error) {
	type alias Rule
	return json.Marshal((*alias)(r))
}

// 简单本地 buffer 避免 import bytes（保持文件依赖最小）。
type bytesBuffer struct{ b []byte }

func (b *bytesBuffer) Write(p []byte) (int, error) { b.b = append(b.b, p...); return len(p), nil }
func (b *bytesBuffer) String() string              { return string(b.b) }
