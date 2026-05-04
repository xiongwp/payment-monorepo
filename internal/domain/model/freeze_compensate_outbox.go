package model

import (
	"encoding/json"
	"time"
)

// FreezeCompensateStatus outbox 状态机
type FreezeCompensateStatus int8

const (
	FreezeCompensateStatusPending      FreezeCompensateStatus = 0
	FreezeCompensateStatusDone         FreezeCompensateStatus = 1
	FreezeCompensateStatusCompensating FreezeCompensateStatus = 2
	FreezeCompensateStatusFailed       FreezeCompensateStatus = 3
)

// FreezeCompensateOutbox 与 frozen account 同分片（按 voucher_no 路由）。
//
// Phase 1 同 tx INSERT PENDING；Phase 2 全成功 → CAS PENDING→DONE；
// inline 补偿失败 → 留 PENDING，worker 兜底重试到 DONE 或 FAILED。
type FreezeCompensateOutbox struct {
	ID            int64                  `db:"id"               json:"id"`
	VoucherNo     string                 `db:"voucher_no"       json:"voucher_no"`
	FreezeOrderNo string                 `db:"freeze_order_no"  json:"freeze_order_no"`
	Payload       string                 `db:"payload"          json:"payload"`
	Status        FreezeCompensateStatus `db:"status"           json:"status"`
	RetryCount    int                    `db:"retry_count"      json:"retry_count"`
	ErrorMsg      string                 `db:"error_msg"        json:"error_msg"`
	CreatedAt     time.Time              `db:"created_at"       json:"created_at"`
	UpdatedAt     time.Time              `db:"updated_at"       json:"updated_at"`
}

// FreezeCompensatePayload payload JSON 结构。worker 反序列化后据此精确反向。
type FreezeCompensatePayload struct {
	FreezeAccountNo    string                  `json:"freeze_account_no"`
	FreezeAmount       int64                   `json:"freeze_amount"`
	FreezeBusinessNo   string                  `json:"freeze_business_no"`
	FreezeBusinessType string                  `json:"freeze_business_type"`
	Currency           string                  `json:"currency"`
	Description        string                  `json:"description"`
	TransactionDate    string                  `json:"transaction_date"`
	CutDate            string                  `json:"cut_date"`
	NowUnix            int64                   `json:"now_unix"`
	// CreditEntries 是 Phase 2 计划要写的全部 credit entries（包括成功和失败的）。
	// SuccessfulTxIDs 标识哪些已成功（已落 transaction record）—— 这些需要反向。
	// 失败的不需要反向（本来就没成功）。
	CreditEntries   []FreezeCompensateEntry `json:"credit_entries"`
	SuccessfulTxIDs []string                `json:"successful_tx_ids"`
}

// FreezeCompensateEntry 单条 credit entry 的反向所需信息
type FreezeCompensateEntry struct {
	AccountNo    string `json:"account_no"`
	DebitAmount  int64  `json:"debit_amount"`
	CreditAmount int64  `json:"credit_amount"`
	Description  string `json:"description"`
	TxID         string `json:"tx_id"`
}

// MarshalPayload 序列化 payload
func (p *FreezeCompensatePayload) Marshal() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UnmarshalCompensatePayload 反序列化
func UnmarshalCompensatePayload(s string) (*FreezeCompensatePayload, error) {
	var p FreezeCompensatePayload
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil, err
	}
	return &p, nil
}
