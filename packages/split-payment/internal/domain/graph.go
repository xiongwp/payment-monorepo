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

import (
	"fmt"
	"time"
)

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
//
// SP-AC-7 PH3-8 决议 (2026-05):
//   - Separate 是当前唯一真实实现 — Engine.Handle 走 translator 按 edges 拆 Transfer/Fee.
//   - Direct / Destination 是 Stripe 概念占位, 当前 engine 不分支; 设置 = 无效果 (会被 ValidateChargeStrategy
//     log warn 提醒). 保留字段 + 常量是为了 future routing (Phase 4 真接 Stripe Connect 时).
//   - 空值默认 Separate (向后兼容).
const (
	ChargeStrategyDirect      = "direct"      // ⚠ 占位 — 顾客直付商户, 平台只抽 fee. 尚未实现.
	ChargeStrategyDestination = "destination" // ⚠ 占位 — 平台收, 整笔 → 商户. 尚未实现.
	ChargeStrategySeparate    = "separate"    // 平台收, 按 edge 规则分多个 Transfer (默认 + 唯一真实实现).
)

// ValidateChargeStrategy 检查 ChargeStrategy 合法性.
//
// P2-STRAT-1: direct / destination 之前静默降级 separate, 现在改成**拒绝**
// (返 err), 避免商户/管理员配了未实现策略时引擎悄悄改路径. 真要支持等
// Phase 4 Stripe Connect 接通, 把这两条 case 改成 return s, "", nil.
//
// 返回:
//   - normalized: 规整后的 strategy (空→Separate; 已知值原样返).
//   - warn: 非空 → 调用方应 log warn.
//   - err:  非空 → 拒绝该 graph (未知或未实现 strategy).
func ValidateChargeStrategy(s string) (normalized string, warn string, err error) {
	switch s {
	case "":
		return ChargeStrategySeparate, "", nil
	case ChargeStrategySeparate:
		return s, "", nil
	case ChargeStrategyDirect:
		return "", "", fmt.Errorf("charge_strategy=%q 未实现 (Phase 4 Stripe Connect 才接), "+
			"当前不允许用 — 防引擎静默降级 separate 改资金路径", s)
	case ChargeStrategyDestination:
		return "", "", fmt.Errorf("charge_strategy=%q 未实现 (Phase 4 Stripe Connect 才接), "+
			"当前不允许用 — 防引擎静默降级 separate 改资金路径", s)
	default:
		return "", "", fmt.Errorf("unknown charge_strategy %q (allowed: separate; direct/destination 尚未实现)", s)
	}
}

// Trigger 触发条件: 哪个事件 + 什么 filter 命中时执行此 graph.
type Trigger struct {
	Event  string `json:"event"`            // e.g. "charge.succeeded"
	Filter string `json:"filter,omitempty"` // Starlark 表达式, e.g. `merchant.tier=="marketplace"`
}

// Node 节点 — 一个资金端点 (本质上就是 "谁的某类账户").
//
// SP-AC-7 瘦身:
//   - 不再带 AccountType — 由 Edge.event_code → TransactionRule.debit/credit_subject 推导.
//   - 不再带 AccountTemplate / FromAttr / Optional — 字符串拼 account_no 的 DIY 时代终结.
//   - 不再带 PartyType — 由 AccountTypeInfo.owner_type 推导.
//
// Type 分类 (translator 不区分语义,纯 UI 标记给运营看清资金路径):
//   - input:        资金来源 (顾客 / 渠道入金)
//   - intermediate: 中间过渡户 (escrow / clearing / holding / suspense)
//   - account:      普通收款方 (商户余额 / 推广员 / 物流方)
//   - output:       最终账户 (清结算 / 平台主账户)
//
// Node 上的三件套 attr key —— 告诉运行时去 event/request payload 里取:
//
//   AccountIDAttr  → attributes[key] = account_id   ("user_id_account"   → "42")
//   AmountAttr     → attributes[key] = amount_minor ("user_id_account_amount"   → "9900")
//   CurrencyAttr   → attributes[key] = currency     ("user_id_account_currency" → "CNY")
//
// 每个 key 在 caller (channel webhook / api 入参) 里都对应一组 (account_id, amount, currency).
// translator 直接透传给 accounting.CreateTransaction, 不做任何模板拼接.
//
// 默认 convention (省 designer 配置):
//   AmountAttr   空 → 用 AccountIDAttr + "_amount"
//   CurrencyAttr 空 → 用 AccountIDAttr + "_currency"
//   AccountIDAttr 空 → 用 node.ID (平台固定户 e.g. PLATFORM_FEE_REVENUE 全局唯一,不需要 caller 传)
type Node struct {
	ID    string `json:"id"`              // 图内唯一
	Type  string `json:"type"`            // input / intermediate / account / output (UI 分类)
	Label string `json:"label,omitempty"` // UI 显示

	AccountIDAttr string `json:"account_id_attr,omitempty"` // attributes 里取 account_id 的 key
	AmountAttr    string `json:"amount_attr,omitempty"`     // attributes 里取 amount (minor) 的 key
	CurrencyAttr  string `json:"currency_attr,omitempty"`   // attributes 里取 currency 的 key

	// AutoClear: intermediate 节点的实时清算开关.
	// true  → 进金事件落账后立刻派生下游 event (实时清算)
	// false → 等独立 event 触发 (cron / 人工)
	AutoClear bool `json:"auto_clear,omitempty"`
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

	// EventCode: 引用 accounting TransactionRule.event_code, 一条 rule 的名字.
	// 配合 GraphSpec.Scenario (= product_code) 唯一确定一条 rule.
	//
	// 关键语义 (SP-AC-7):
	//   多条 edge 可以共享同一个 EventCode → 这些 edge 一起组成 **一次原子账户操作** (一条 rule).
	//   例: 三方分账规则 "split_with_fee" 涉及 3 个账户、3 条 edge, 全挂同一个 event_code,
	//   translator 把它们打包成一个 TransactionRequest, accounting 一次落账 (要么全成要么全失败).
	//
	// 调用时 caller 传 (scenario, event_code) + 三件套 attribute (account_id/amount/currency),
	// accounting-system 拿到后查 rule → 拆借贷 → 一笔原子落账.
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
