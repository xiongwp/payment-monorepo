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
//
// SP-3 新增字段 (向后兼容):
//   - ChargeStrategy: 三种 Stripe-like 模式 (direct / destination / separate).
//                     trigger=charge.succeeded 时生效, 决定 Transfer/AppFee 怎么生成.
//                     空值默认 separate (跟现有行为一致).
//
// SP-AC-1 新增字段:
//   - Scenario: 业务场景 = accounting product_code (e.g. "user_topup" / "marketplace_split").
//               一个 scenario 下所有 edge 的 event_code 必须落在同一 product_code 的 rule 集.
//               Engine 触发时把 (product_code, event_code) 传给 accounting.CreateTransaction.
type GraphSpec struct {
	Triggers       []Trigger      `json:"triggers"`
	Nodes          []Node         `json:"nodes"`
	Edges          []Edge         `json:"edges"`
	Guards         []Guard        `json:"guards,omitempty"`
	Hold           *Hold          `json:"hold,omitempty"`
	Reversal       *ReversalSpec  `json:"reversal,omitempty"` // 重命名 (跟新 Reversal 实体区分)
	ChargeStrategy string         `json:"charge_strategy,omitempty"` // direct / destination / separate

	// SP-AC-1: 绑定到 accounting-system 的 product_code.
	// 一个 scenario 包含多条 TransactionRule (一个 event 触发 N 笔分录).
	Scenario string `json:"scenario,omitempty"` // = product_code
}

// ChargeStrategy 常量.
const (
	ChargeStrategyDirect      = "direct"      // 顾客直付商户, 平台只抽 fee
	ChargeStrategyDestination = "destination" // 平台收, 整笔 → 商户
	ChargeStrategySeparate    = "separate"    // 平台收, 按 edge 规则分多个 Transfer (默认)
)

// Trigger 触发条件: 哪个事件 + 什么 filter 命中时执行此 graph.
type Trigger struct {
	Event  string `json:"event"`            // e.g. "charge.succeeded"
	Filter string `json:"filter,omitempty"` // Starlark 表达式, e.g. `merchant.tier=="marketplace"`
}

// Node 节点 — 通常是一个账户 (account_id 模板)。
//
// Type 分类 (translator 不区分语义,纯 UI 标记给运营看清资金路径):
//   - input:        资金来源 (顾客 / 渠道入金)
//   - intermediate: 中间过渡户 (escrow / clearing / holding / hold-period 暂留户)
//   - account:      普通收款方 (商户余额 / 推广员 / 物流方)
//   - output:       最终账户 (清结算 / 平台主账户)
//   - pool:         资金池 (历史保留, intermediate 取代了大部分场景)
//
// SP-AC-1: 真实账务模型对接
//   - AccountType:    引用 accounting-system AccountTypeInfo.account_type
//                     e.g. "PLATFORM_RECEIVABLE_CHANNEL" / "USER_WALLET" / "PLATFORM_FEE_CLEARING"
//   - PartyType:      "user" / "merchant" / "platform" — 配合 PartyIDAttr 找具体 account_no
//   - PartyIDAttr:    从 event.attributes 取 party_id 的 key
//                     party_type=platform 时为空 (平台户 owner_type=3/4 不需要 party_id)
//   - AutoClear:      intermediate 节点标 true → 进金后立即触发下游 (实时清算)
//                     false → 等独立 event 触发 (T+0 cron / 手动)
//
// 旧 AccountTemplate / FromAttr 字段保留兼容期, 优先级:
//   AccountType 非空 → 走新模型 (accounting CreateTransaction)
//   否则 → 走旧 Movement 拼分录
type Node struct {
	ID    string `json:"id"`              // 图内唯一
	Type  string `json:"type"`            // input / intermediate / account / output / pool
	Label string `json:"label,omitempty"` // UI 显示

	// SP-AC-1 新模型 (推荐)
	AccountType  string `json:"account_type,omitempty"`   // FK accounting.account_type_info
	PartyType    string `json:"party_type,omitempty"`     // user / merchant / platform
	PartyIDAttr  string `json:"party_id_attr,omitempty"`  // event.attributes 里的 key
	AutoClear    bool   `json:"auto_clear,omitempty"`     // intermediate 进金立即清算

	// 旧模型 (兼容过渡期)
	AccountTemplate string `json:"account_template,omitempty"` // 字符串模板, 支持 {placeholder}
	FromAttr        string `json:"from_attr,omitempty"`         // template 里 placeholder 的 attr 来源
	Optional        bool   `json:"optional,omitempty"`          // attribute 缺失时跳过此节点
}

// Edge 一条资金流: from → to, 按 rule 决定金额。
//
// SP-3: Kind 决定本边产生什么类型的资金对象:
//   - "transfer" (默认):   产生 Transfer (一笔正经分账)
//   - "application_fee":   产生 ApplicationFee (平台抽成)
//   - "payout":            产生 Payout (商户提现)
//
// 老 graph (没 Kind 字段) 兼容: 默认 "transfer".
type Edge struct {
	From string   `json:"from"` // node.id
	To   string   `json:"to"`
	Kind string   `json:"kind,omitempty"` // transfer / application_fee / payout, 默认 transfer
	Rule EdgeRule `json:"rule"`

	// SP-3C: 跨币种支持. 空 → 与 trigger.Currency 相同;
	// 配了 e.g. "EUR" → translator 调 FX 换算后写 Transfer.Currency.
	DestCurrency string `json:"dest_currency,omitempty"`

	// SP-AC-1: 真实账务模型对接.
	// EventCode 引用 accounting-system TransactionRule.event_code,
	// 配合 GraphSpec.Scenario (= product_code) 唯一确定一条 rule.
	// engine 触发时调 accounting.CreateTransaction(product=scenario, event=event_code, amount=...).
	//
	// 空 → 走旧路径 (Movement 直接拼分录).
	EventCode string `json:"event_code,omitempty"`
}

// EdgeKind 常量.
const (
	EdgeKindTransfer       = "transfer"
	EdgeKindApplicationFee = "application_fee"
	EdgeKindPayout         = "payout"
)

// ResolvedKind 返回有效 kind (空 → 默认 transfer).
func (e *Edge) ResolvedKind() string {
	if e.Kind == "" {
		return EdgeKindTransfer
	}
	return e.Kind
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

// ReversalSpec 退款 / 拒付时反向策略 (Graph 配置, 不是 Reversal 实体).
//
// 实际 Reversal 对象在 transfer.go 里定义.
type ReversalSpec struct {
	Strategy                string `json:"strategy"`                    // proportional / fixed_from_platform / fail_if_imbalance
	PlatformCoversShortfall bool   `json:"platform_covers_shortfall"`   // 卖家不够扣时平台垫付
	RefundApplicationFee    bool   `json:"refund_application_fee"`      // 同时退手续费 (SP-3 新)
}

// ─── 执行计划 (运行时实例化 Graph 后的产物) ────────────────────────────

// RunPlan 一次具体执行: graph + 触发事件 + 实例化的金额。
//
// SP-3 升级: 除原 Movements (兼容视图) 外,additionally 持 typed objects:
//   - Transfers       — edge.kind=transfer 的产物
//   - ApplicationFees — edge.kind=application_fee 的产物
//   - Payouts         — edge.kind=payout 的产物
//   - TransferGroup   — 同 charge 派生的所有 Transfer/Fee 关联标识
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

	// 兼容视图: 所有 movement 的 union (Transfers + Fees + Payouts 摊平).
	// 老 caller 仍可用; 新 caller 用下面的 typed 数组.
	Movements []Movement `db:"-" json:"movements"`

	// SP-3 typed 资金原语 (Stripe-style).
	TransferGroup    string           `db:"transfer_group" json:"transfer_group,omitempty"`
	Transfers        []Transfer       `db:"-" json:"transfers,omitempty"`
	ApplicationFees  []ApplicationFee `db:"-" json:"application_fees,omitempty"`
	Payouts          []Payout         `db:"-" json:"payouts,omitempty"`

	// SP-AC-1: 真实账务对接 — translator 输出的 TransactionRequest 列表.
	// 每条 → accounting.CreateTransaction → 借贷分录由 rule 自动拆.
	// 与上面的 Movements 二选一: AccountType 模式走 Transactions, 旧 template 模式走 Movements.
	Transactions []TransactionRequest `db:"-" json:"transactions,omitempty"`

	Status    string    `db:"status" json:"status"` // created/executing/completed/failed/reversed
	VoucherNo string    `db:"voucher_no" json:"voucher_no"`
	ErrorMsg  string    `db:"error_msg" json:"error_msg,omitempty"`
	TraceID   string    `db:"trace_id" json:"trace_id"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
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
