// graph.go — Money Flow Graph: 通用资金流图。
//
// 一个 Graph 描述了:
//   - 触发条件 (什么事件 + 什么 filter 进来时启动)
//   - 节点 (账户, 可带 placeholder, e.g. seller_balance/{seller_id})
//   - 边 (资金流, 带规则: percent/fixed/remainder + 守护)
//   - guards (执行前的全局校验)
//   - hold (保留期, 钱压平台 N 天)
//   - reversal (退款时反向策略)
//
// 序列化形式 = JSON (存 DB / config-center, 可在 admin 设计器拖出来)。

package domain

import "time"

// Graph 一个资金流图定义。
type Graph struct {
	ID        int64     `db:"id" json:"id"`
	Key       string    `db:"key" json:"key"`             // 业务键, 如 "marketplace-default"
	Name      string    `db:"name" json:"name"`
	Version   string    `db:"version" json:"version"`     // semver, 改图升版本不影响历史 plan
	Status    string    `db:"status" json:"status"`       // draft / active / archived
	OwnerType string    `db:"owner_type" json:"owner_type"` // platform / merchant
	OwnerID   string    `db:"owner_id" json:"owner_id"`
	Spec      GraphSpec `db:"-" json:"spec"`              // 序列化进 spec_json 列
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// GraphSpec 真正的图定义 (JSON-serializable)。
type GraphSpec struct {
	Triggers []Trigger `json:"triggers"`
	Nodes    []Node    `json:"nodes"`
	Edges    []Edge    `json:"edges"`
	Guards   []Guard   `json:"guards,omitempty"`
	Hold     *Hold     `json:"hold,omitempty"`
	Reversal *Reversal `json:"reversal,omitempty"`
}

// Trigger 触发条件: 哪个事件 + 什么 filter 命中时执行此 graph.
type Trigger struct {
	Event  string `json:"event"`            // e.g. "charge.succeeded"
	Filter string `json:"filter,omitempty"` // Starlark 表达式, e.g. `merchant.tier=="marketplace"`
}

// Node 节点 — 通常是一个账户 (account_id 模板)。
type Node struct {
	ID              string `json:"id"`              // 图内唯一
	Type            string `json:"type"`            // input / account / output / pool
	Label           string `json:"label,omitempty"` // UI 显示
	AccountTemplate string `json:"account_template"` // 渲染后是真实 account_id; 支持 {placeholder}
	FromAttr        string `json:"from_attr,omitempty"` // 若 template 含 placeholder, 从 event payload attribute 取
	Optional        bool   `json:"optional,omitempty"`  // attribute 缺失时整个节点跳过
}

// Edge 一条资金流: from → to, 按 rule 决定金额。
type Edge struct {
	From string    `json:"from"` // node.id
	To   string    `json:"to"`
	Rule EdgeRule  `json:"rule"`
}

// EdgeRule 边的金额规则。
type EdgeRule struct {
	Type            string `json:"type"`             // percent / fixed_minor / remainder
	Value           int64  `json:"value"`            // percent: basis points (10000=100%); fixed: cents
	MinAmount       int64  `json:"min_amount,omitempty"`
	MaxAmount       int64  `json:"max_amount,omitempty"` // cap
	IfMissing       string `json:"if_missing,omitempty"` // skip / fail / default (针对 placeholder 缺失)
	DefaultBeneficiary string `json:"default_beneficiary,omitempty"` // if_missing=default 时用这个
}

// Guard 执行前校验, 任何 guard 失败 → graph 拒绝执行。
type Guard struct {
	Kind  string `json:"kind"`  // amount_min / amount_max / merchant_active / currency_in / custom_starlark
	Value any    `json:"value"` // kind 决定语义
	Msg   string `json:"msg"`
}

// Hold 保留期 — 部分节点上的钱压 N 天后再可用。
type Hold struct {
	Days      int      `json:"days"`
	AppliesTo []string `json:"applies_to"` // node.id 列表, e.g. ["seller"]
	// 实现: 落账时 to= account_template_unsettled/{id}, cron 到期搬到正式账户
}

// Reversal 退款 / 拒付时反向策略。
type Reversal struct {
	Strategy                string `json:"strategy"`                  // proportional / fixed_from_platform / fail_if_imbalance
	PlatformCoversShortfall bool   `json:"platform_covers_shortfall"` // 卖家不够扣时平台垫付
}

// ─── 执行计划 (运行时实例化 Graph 后的产物) ────────────────────────────

// RunPlan 一次具体执行: graph + 触发事件 + 实例化的金额。
type RunPlan struct {
	ID           int64     `db:"id" json:"id"`
	GraphID      int64     `db:"graph_id" json:"graph_id"`
	GraphVersion string    `db:"graph_version" json:"graph_version"`
	TriggerEvent string    `db:"trigger_event" json:"trigger_event"`
	ChargeID     string    `db:"charge_id" json:"charge_id"`
	MerchantID   string    `db:"merchant_id" json:"merchant_id"`
	AmountMinor  int64     `db:"amount_minor" json:"amount_minor"`
	Currency     string    `db:"currency" json:"currency"`
	Attributes   map[string]string `db:"-" json:"attributes"` // event payload merge
	Movements    []Movement `db:"-" json:"movements"`
	Status       string    `db:"status" json:"status"` // created/executing/completed/failed/reversed
	VoucherNo    string    `db:"voucher_no" json:"voucher_no"`
	ErrorMsg     string    `db:"error_msg" json:"error_msg,omitempty"`
	TraceID      string    `db:"trace_id" json:"trace_id"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

// Movement 一条资金移动 (Graph.Edge 实例化的结果)。
type Movement struct {
	EdgeFromNode string `json:"edge_from"`
	EdgeToNode   string `json:"edge_to"`
	FromAccount  string `json:"from_account"`
	ToAccount    string `json:"to_account"`
	AmountMinor  int64  `json:"amount_minor"`
	TxID         string `json:"tx_id,omitempty"`     // accounting 回填
	Status       string `json:"status"`              // pending / posted / skipped / failed
	Reason       string `json:"reason,omitempty"`
}
