// accounting_meta.go — SP-AC-7 元数据类型 (HTTP 客户端已删除, 走 gRPC).
//
// 历史 (SP-AC-2): 这里曾经是 HTTP-JSON 客户端, 跟 accounting-system /admin/* 端点对接.
// SP-AC-7 把所有业务 RPC 迁到 gRPC (见 accounting_grpc.go), HTTP 只留作 ops/admin UI 用.
// 本文件保留公共类型定义 (AccountTypeInfo / TransactionRule / CreateTransactionResponse),
// 它们仍是 AccountingGRPCClient 的返回类型, 维持 caller 兼容.
package clients

// AccountTypeInfo 跟 accounting domain model 同形态, 这里只取 UI / 校验需要的字段.
type AccountTypeInfo struct {
	AccountType      string `json:"account_type"`       // e.g. "USER_WALLET"
	AccountTypeName  string `json:"account_type_name"`  // 中文名
	OwnerType        int    `json:"owner_type"`         // user/merchant/platform 数值码
	IsPlatform       int    `json:"is_platform"`        // 1 = 平台内部
	BalanceDirection string `json:"balance_direction"`  // C=贷方常态, D=借方常态
	Description      string `json:"description,omitempty"`
}

// TransactionRule.
type TransactionRule struct {
	ID              int64  `json:"id"`
	ProductCode     string `json:"product_code"`
	EventCode       string `json:"event_code"`
	DebitSubjectID  string `json:"debit_subject_id"`
	CreditSubjectID string `json:"credit_subject_id"`
	FromDirection   string `json:"from_direction"`
	ToDirection     string `json:"to_direction"`
	Description     string `json:"description,omitempty"`
}

// CreateTransactionResponse 跟 accounting-system 的响应同形态.
//
// upstream Status: 0=pending / 1=processing / 2=success / 3=failed.
type CreateTransactionResponse struct {
	OrderNo      string
	Status       int8
	VoucherNo    string
	ErrorMessage string
}

// Success 是否落账成功.
func (r *CreateTransactionResponse) Success() bool { return r != nil && r.Status == 2 }

// IsFailed 是否最终失败.
func (r *CreateTransactionResponse) IsFailed() bool { return r != nil && r.Status == 3 }
