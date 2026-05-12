// Package outboxconsumer — accounting 消费 payment-core / refund-engine / clearing-settlement 的 outbox 事件.
//
// 事件 → 会计分录映射:
//
//   ChargeCaptured        → DR cust_wallet/{m} | CR merchant_ar/{m}  (商户应收+) + DR merchant_ar/{m} | CR platform_revenue (手续费)
//   ChargeRefunded        → 反向
//   RefundCompleted       → DR merchant_ar/{m} | CR cust_wallet/{m}
//   PayoutSettled         → DR merchant_ar/{m} | CR our_bank/{ccy}
//   PayoutReversed        → 反向
//
// 所有事件用 AtomicBatchBooking 出账 (已有 accounting-system 内方法).

package outboxconsumer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"
)

// 本地 Event 副本 (避免跨包依赖 — go.work 接通后可直接 import payment-util/outbox.Event)
type Event struct {
	EventID     string          `json:"event_id"`
	Aggregate   string          `json:"aggregate"`
	AggregateID string          `json:"aggregate_id"`
	EventType   string          `json:"event_type"`
	Payload     json.RawMessage `json:"payload"`
}

// BookingClient 跟 accounting-system 内 AtomicBatchBooking 的契约.
type BookingClient interface {
	PostMovements(ctx context.Context, movements []Movement, idempotencyKey string) error
}

type Movement struct {
	DebitAccount  string
	CreditAccount string
	Amount        int64
	Currency      string
	Memo          string
}

// Handler 实现 payment-util/outbox.EventHandler 接口.
type Handler struct {
	Booking BookingClient
	Log     *zap.Logger
}

// Handle 路由到对应 event 处理.
func (h *Handler) Handle(ctx context.Context, tx *sql.Tx, evt Event) error {
	h.Log.Debug("accounting consumer handle",
		zap.String("event_id", evt.EventID),
		zap.String("event_type", evt.EventType))

	switch evt.EventType {
	case "ChargeCaptured":
		return h.handleChargeCaptured(ctx, evt)
	case "ChargeRefunded":
		return h.handleChargeRefunded(ctx, evt)
	case "RefundCompleted":
		return h.handleRefundCompleted(ctx, evt)
	case "PayoutSettled":
		return h.handlePayoutSettled(ctx, evt)
	case "PayoutReversed":
		return h.handlePayoutReversed(ctx, evt)
	default:
		// 不关心的事件 — 不处理但不算失败
		h.Log.Debug("accounting consumer skip",
			zap.String("event_type", evt.EventType))
		return nil
	}
}

// ─── 单事件处理 ───

type chargePayload struct {
	ID            string `json:"id"`
	MerchantID    string `json:"merchant_id"`
	Currency      string `json:"currency"`
	AmountCents   int64  `json:"amount_cents"`
	FeesCents     int64  `json:"fees_cents"`
}

func (h *Handler) handleChargeCaptured(ctx context.Context, evt Event) error {
	var p chargePayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("unmarshal charge: %w", err)
	}
	movements := []Movement{
		{
			// 商户应收 +
			DebitAccount:  "cust_wallet/" + p.MerchantID,
			CreditAccount: "merchant_ar/" + p.MerchantID,
			Amount:        p.AmountCents,
			Currency:      p.Currency,
			Memo:          "charge " + p.ID,
		},
	}
	if p.FeesCents > 0 {
		movements = append(movements, Movement{
			// 平台手续费收入
			DebitAccount:  "merchant_ar/" + p.MerchantID,
			CreditAccount: "platform_revenue/" + p.Currency,
			Amount:        p.FeesCents,
			Currency:      p.Currency,
			Memo:          "fee " + p.ID,
		})
	}
	return h.Booking.PostMovements(ctx, movements, "evt:"+evt.EventID)
}

func (h *Handler) handleChargeRefunded(ctx context.Context, evt Event) error {
	// 反向 charge
	var p chargePayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return err
	}
	movements := []Movement{
		{
			DebitAccount:  "merchant_ar/" + p.MerchantID,
			CreditAccount: "cust_wallet/" + p.MerchantID,
			Amount:        p.AmountCents,
			Currency:      p.Currency,
			Memo:          "charge_refund " + p.ID,
		},
	}
	return h.Booking.PostMovements(ctx, movements, "evt:"+evt.EventID)
}

type refundPayload struct {
	ID            string `json:"id"`
	MerchantID    string `json:"merchant_id"`
	Currency      string `json:"currency"`
	AmountCents   int64  `json:"amount_cents"`
}

func (h *Handler) handleRefundCompleted(ctx context.Context, evt Event) error {
	var p refundPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return err
	}
	return h.Booking.PostMovements(ctx, []Movement{
		{
			DebitAccount:  "merchant_ar/" + p.MerchantID,
			CreditAccount: "cust_wallet/" + p.MerchantID,
			Amount:        p.AmountCents,
			Currency:      p.Currency,
			Memo:          "refund " + p.ID,
		},
	}, "evt:"+evt.EventID)
}

type payoutPayload struct {
	ID           string `json:"id"`
	MerchantID   string `json:"merchant_id"`
	Currency     string `json:"currency"`
	NetCents     int64  `json:"net_cents"`
}

func (h *Handler) handlePayoutSettled(ctx context.Context, evt Event) error {
	var p payoutPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return err
	}
	return h.Booking.PostMovements(ctx, []Movement{
		{
			DebitAccount:  "merchant_ar/" + p.MerchantID,
			CreditAccount: "our_bank/" + p.Currency,
			Amount:        p.NetCents,
			Currency:      p.Currency,
			Memo:          "payout " + p.ID,
		},
	}, "evt:"+evt.EventID)
}

func (h *Handler) handlePayoutReversed(ctx context.Context, evt Event) error {
	var p payoutPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return err
	}
	return h.Booking.PostMovements(ctx, []Movement{
		{
			DebitAccount:  "our_bank/" + p.Currency,
			CreditAccount: "merchant_ar/" + p.MerchantID,
			Amount:        p.NetCents,
			Currency:      p.Currency,
			Memo:          "payout_reverse " + p.ID,
		},
	}, "evt:"+evt.EventID)
}
