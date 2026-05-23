// transaction_request.go — Translator 输出格式 (账务系统调用入参).
//
// SP-AC-7 重构:
//   一个 rule (= event_code) 可以包含多条 edge → 一次原子账务操作可涉及 ≥ 2 个账户.
//   所以 TransactionRequest 是 multi-leg 的: 携带一个 Legs 数组, 每条 leg 一对账户.
//
//   触发流程:
//     1. caller 把 (account_id, amount, currency) 按 node.account_id_attr 三件套塞进 attributes
//     2. translator 按 event_code 分组 edge → 生成 N 个 TransactionRequest
//     3. accounting.CreateTransaction 一次原子落账 (Legs 一起成功或一起失败)
package domain

import "time"

// TransactionRequest 给 accounting.CreateTransaction 的入参 (multi-leg).
type TransactionRequest struct {
	// 幂等 + 业务关联
	OrderNo      string `json:"order_no"`      // 外部幂等键 (graph_run_id + event_code)
	BusinessNo   string `json:"business_no"`   // 业务 ID (e.g. topup_xxx), 分片键
	BusinessType string `json:"business_type"` // user_topup / marketplace_split / ...

	// 规则路由 — accounting 用 (product_code, event_code) 找 TransactionRule
	ProductCode string `json:"product_code"` // = GraphSpec.Scenario
	EventCode   string `json:"event_code"`   // = Edge.EventCode (= rule 名字)

	// Legs — 一个 rule 包含的所有 edge 的资金流, 一起原子落账.
	// 顺序由 translator 决定 (remainder 永远最后, 防尾差).
	Legs []TxnLeg `json:"legs"`

	// 元信息
	Description string            `json:"description,omitempty"`
	TraceID     string            `json:"trace_id,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`

	// runtime 字段 (落 RunPlan 用)
	Status    string    `json:"status,omitempty"` // pending / posted / failed
	VoucherNo string    `json:"voucher_no,omitempty"`
	ErrorMsg  string    `json:"error_msg,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// TxnLeg 一条资金流 (from_account → to_account, 一笔金额).
//
// 同一个 TransactionRequest 里的所有 Leg 必须用同一币种 (accounting 不支持跨币种原子操作).
//
// Fleet × Rotation 路由（可选）：
//   - 留空 FromAccountID + 填 FromLogicalAccountKey + FromFlowID → server 端选 fleet sub
//   - 留空 ToAccountID   + 填 ToLogicalAccountKey   + ToFlowID   → 同上
//   - 两侧独立生效；caller 通常只对 channel-* / platform-* 等需要轮换的账户填 LA 字段，
//     用户 / 商户账户继续走 account_no 直填路径。
type TxnLeg struct {
	EdgeFromNode  string `json:"edge_from_node"`
	EdgeToNode    string `json:"edge_to_node"`
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	Amount        string `json:"amount"` // 字符串保精度 (单位 minor)
	Currency      string `json:"currency"`

	// fleet routing 可选字段
	FromLogicalAccountKey string `json:"from_logical_account_key,omitempty"`
	FromFlowID            string `json:"from_flow_id,omitempty"`
	ToLogicalAccountKey   string `json:"to_logical_account_key,omitempty"`
	ToFlowID              string `json:"to_flow_id,omitempty"`
}
