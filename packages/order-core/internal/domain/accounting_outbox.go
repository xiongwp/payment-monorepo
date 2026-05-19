package domain

import (
	"errors"
	"time"
)

// ErrAccountingOutboxDuplicate 同一 request_id 的记账事件已入队。
var ErrAccountingOutboxDuplicate = errors.New("accounting outbox duplicate")

// AccountingOutboxStatus outbox 行生命周期状态。
type AccountingOutboxStatus string

const (
	AccountingOutboxPending AccountingOutboxStatus = "pending"
	AccountingOutboxSent    AccountingOutboxStatus = "sent"
	AccountingOutboxFailed  AccountingOutboxStatus = "failed"
)

// AccountingEventType 指导 worker 按哪个方向落账.
//
// 一表概览 (借/贷方向 = order-core mapper 内 entriesDirection):
//
//	event_type            | DEBIT 侧               | CREDIT 侧              | BusinessType
//	---------------------|------------------------|-----------------------|-------------
//	charge_succeeded     | channel-buffer         | owner-pending-settle  | PAYMENT
//	refund_succeeded     | owner-pending-settle   | channel-buffer        | REFUND
//	dispute_opened       | owner-pending-settle   | channel-buffer (扣回) | REFUND
//	dispute_won          | channel-buffer         | owner-pending-settle  | PAYMENT
//	chargeback_received  | owner-pending-settle   | channel-buffer        | REFUND
//	fee_charged          | owner-pending-settle   | platform-pnl          | COMMISSION
//	reversal_succeeded   | (按上游事件反向)        | (反向)                | REFUND
type AccountingEventType string

const (
	AccountingEventChargeSucceeded     AccountingEventType = "charge_succeeded"
	AccountingEventRefundSucceeded     AccountingEventType = "refund_succeeded"
	AccountingEventDisputeOpened       AccountingEventType = "dispute_opened"        // 争议挂起: 临时把钱从商户应付划回渠道应收
	AccountingEventDisputeWon          AccountingEventType = "dispute_won"           // 我方胜诉: 撤回 dispute_opened, 重新挂账给商户
	AccountingEventChargebackReceived  AccountingEventType = "chargeback_received"   // 渠道发起拒付强扣
	AccountingEventFeeCharged          AccountingEventType = "fee_charged"           // 平台费 / 服务费向商户收
	AccountingEventReversalSucceeded   AccountingEventType = "reversal_succeeded"    // 全链路 reversal (split-payment 用)
)

// AccountingOwnerType 账户归属。
type AccountingOwnerType string

const (
	AccountingOwnerUser     AccountingOwnerType = "user"
	AccountingOwnerMerchant AccountingOwnerType = "merchant"
)

// AccountingOutbox 待投递给 accounting-system 的双分录记账事件。
//
// 写入点：webhook_service 成功分支（charge.succeeded / refund.succeeded）。
// 消费点：accounting_outbox_worker 轮询 pending，调 accounting.Client.DoubleEntryBooking。
// 幂等键：request_id 由 {pi_id}:{event_type}:{charge_or_refund_id} 构造；
//        同一 request_id 重复 Insert 被唯一约束拦截，worker 重试也安全。
// 分片：按 payment_intent_id 与 PI 同分片，便于聚合查询。
type AccountingOutbox struct {
	ID              string                 `gorm:"column:id;primaryKey;type:varchar(64)"                        json:"id"`
	RequestID       string                 `gorm:"column:request_id;type:varchar(160);uniqueIndex:uk_request"   json:"request_id"`
	EventType       AccountingEventType    `gorm:"column:event_type;type:varchar(32)"                           json:"event_type"`
	PaymentIntentID string                 `gorm:"column:payment_intent_id;type:varchar(64)"                    json:"payment_intent_id"`
	ChargeID        string                 `gorm:"column:charge_id;type:varchar(64)"                            json:"charge_id,omitempty"`
	RefundID        string                 `gorm:"column:refund_id;type:varchar(64)"                            json:"refund_id,omitempty"`
	OwnerType       AccountingOwnerType    `gorm:"column:owner_type;type:varchar(16)"                           json:"owner_type"`
	OwnerID         string                 `gorm:"column:owner_id;type:varchar(64)"                             json:"owner_id"`
	// PaymentMethod 来自 PI.PaymentMethod。worker 用它从 config counter_accounts
	// 找对端渠道应收平台账户 accountNo。
	PaymentMethod string                   `gorm:"column:payment_method;type:varchar(32)"                       json:"payment_method"`
	Amount        int64                    `gorm:"column:amount"                                                json:"amount"`
	Currency      string                   `gorm:"column:currency;type:char(3)"                                 json:"currency"`
	Metadata      Metadata                 `gorm:"column:metadata;type:json"                                    json:"metadata,omitempty"`
	Status        AccountingOutboxStatus   `gorm:"column:status;type:varchar(16)"                               json:"status"`
	Attempts      int                      `gorm:"column:attempts"                                              json:"attempts"`
	NextAttemptAt *time.Time               `gorm:"column:next_attempt_at"                                       json:"next_attempt_at,omitempty"`
	LastError     string                   `gorm:"column:last_error;type:text"                                  json:"last_error,omitempty"`
	SentAt        *time.Time               `gorm:"column:sent_at"                                               json:"sent_at,omitempty"`
	// ClaimToken worker claim 一批待投递行时写入；SELECT WHERE claim_token=? 即可拉到刚 claim 的批次。
	// 空 = 未被任何 worker claim。Mark{Sent,Retry,Failed} 也带 claim_token 条件以防 lease 过期被抢占。
	ClaimToken    string                   `gorm:"column:claim_token;type:varchar(64)"                          json:"claim_token,omitempty"`
	Created       time.Time                `gorm:"column:created;autoCreateTime:milli"                          json:"created"`
	Updated       time.Time                `gorm:"column:updated;autoUpdateTime:milli"                          json:"updated"`
}
