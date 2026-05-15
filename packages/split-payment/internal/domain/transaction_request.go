// transaction_request.go — SP-AC-1 Translator 输出新格式.
//
// 替代旧 Movement (from_acc, to_acc, amount), 升级为引用 accounting TransactionRule:
//
//   旧: Movement{from_account: "user_wallet/u_xxx", to_account: "platform_fee/", amount: 100}
//       engine → accounting.PostMovements (自己拼借贷)
//
//   新: TransactionRequest{product_code:"user_topup", event_code:"channel_settled_to_user",
//                          from_party_id:42, from_party_type:"user",
//                          to_party_id:0,    to_party_type:"platform",
//                          amount:10000, business_no:"topup_xxx"}
//       engine → accounting.CreateTransaction (rule 自动拆借贷)
//
// 一个 Graph execution → N 个 TransactionRequest (按 edge 顺序, saga step 各跑一条).
package domain

import "time"

// TransactionRequest 给 accounting.CreateTransaction 的入参.
type TransactionRequest struct {
	// 幂等 + 业务关联
	OrderNo      string `json:"order_no"`      // 外部幂等键 (e.g. graph_run_id + edge_id)
	BusinessNo   string `json:"business_no"`   // 业务 ID, 分片键 (e.g. topup_xxx / charge_xxx)
	BusinessType string `json:"business_type"` // user_topup / marketplace_split / ...

	// 规则路由
	ProductCode string `json:"product_code"` // = GraphSpec.Scenario
	EventCode   string `json:"event_code"`   // = Edge.EventCode

	// 参与方
	FromPartyID   int64  `json:"from_party_id"`
	FromPartyType string `json:"from_party_type"` // user / merchant / platform
	ToPartyID     int64  `json:"to_party_id"`
	ToPartyType   string `json:"to_party_type"`

	// 金额
	Amount   string `json:"amount"`   // 字符串保精度 (accounting 内部用 string)
	Currency string `json:"currency"`

	// 元信息
	Description string            `json:"description,omitempty"`
	TraceID     string            `json:"trace_id,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`

	// runtime 字段 (落 RunPlan 用)
	EdgeFromNode string    `json:"edge_from_node,omitempty"`
	EdgeToNode   string    `json:"edge_to_node,omitempty"`
	Status       string    `json:"status,omitempty"` // pending / posted / failed
	VoucherNo    string    `json:"voucher_no,omitempty"`
	ErrorMsg     string    `json:"error_msg,omitempty"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
}
