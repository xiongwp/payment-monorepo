// Package workflow — Subscription 周期扣款 + dunning 重试 + lifecycle.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"reconcile-system/packages/subscription/internal/domain"

	"go.uber.org/zap"
)

// Service ...
type Service struct {
	Subs     SubsRepo
	Plans    PlanRepo
	Invoices InvoiceRepo
	Dunning  DunningRepo
	Gateway  GatewayClient
	Events   EventPublisher
	Log      *zap.Logger
	Now      func() time.Time
}

// SubsRepo ...
type SubsRepo interface {
	Get(ctx context.Context, id string) (*domain.Subscription, error)
	Save(ctx context.Context, s *domain.Subscription) error
	FindDueForCycle(ctx context.Context, before time.Time, limit int) ([]*domain.Subscription, error)
	FindDueForDunning(ctx context.Context, before time.Time, limit int) ([]*domain.Subscription, error)
}

// PlanRepo ...
type PlanRepo interface {
	Get(ctx context.Context, id int64) (*domain.Plan, error)
}

// InvoiceRepo ...
type InvoiceRepo interface {
	Save(ctx context.Context, inv *domain.Invoice) error
	Get(ctx context.Context, id string) (*domain.Invoice, error)
	GetLatestOpen(ctx context.Context, subscriptionID string) (*domain.Invoice, error)
}

// DunningRepo ...
type DunningRepo interface {
	Append(ctx context.Context, e *domain.DunningEvent) error
}

// GatewayClient 调 payment-gateway / order-core 收款。
type GatewayClient interface {
	CreateCharge(ctx context.Context, req ChargeRequest) (chargeID string, succeeded bool, code string, err error)
}

// ChargeRequest 收款入参。
type ChargeRequest struct {
	MerchantID      string
	CustomerID      string
	PaymentMethodID string
	AmountMinor     int64
	Currency        string
	Idempotency     string
	Description     string
	Metadata        map[string]string
}

// EventPublisher 把成功扣款的事件投到 Kafka, 让 moneyflow engine 消费触发分账。
type EventPublisher interface {
	Publish(ctx context.Context, topic string, payload map[string]any) error
}

// ─── 主循环: 周期扣款 ─────────────────────────────────────────────────

// CycleTick 跑一轮周期扣款 — 找 current_period_end 已到的订阅, 各扣一次。
// 一般 cron 每 5min 调一次。
func (s *Service) CycleTick(ctx context.Context, limit int) (int, error) {
	subs, err := s.Subs.FindDueForCycle(ctx, s.now(), limit)
	if err != nil {
		return 0, fmt.Errorf("find due: %w", err)
	}
	processed := 0
	for _, sub := range subs {
		if err := s.processCycle(ctx, sub); err != nil {
			s.Log.Warn("cycle failed",
				zap.String("sub", sub.SubscriptionID), zap.Error(err))
		}
		processed++
	}
	return processed, nil
}

// processCycle 单个订阅扣一次款。
func (s *Service) processCycle(ctx context.Context, sub *domain.Subscription) error {
	if sub.Status != domain.StatusActive && sub.Status != domain.StatusTrialing {
		return fmt.Errorf("status not eligible: %s", sub.Status)
	}
	// 试用期跳过, 只推进 period
	if sub.Status == domain.StatusTrialing && sub.TrialEnd != nil && s.now().Before(*sub.TrialEnd) {
		return nil
	}

	plan, err := s.Plans.Get(ctx, sub.PlanID)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	if plan.MaxCycles > 0 && sub.CycleCount >= plan.MaxCycles {
		sub.Status = domain.StatusEnded
		_ = s.Subs.Save(ctx, sub)
		return nil
	}

	// 创建 invoice
	inv := &domain.Invoice{
		InvoiceID:      genID("inv_"),
		SubscriptionID: sub.SubscriptionID,
		MerchantID:     sub.MerchantID,
		CustomerID:     sub.CustomerID,
		AmountMinor:    plan.AmountMinor,
		Currency:       plan.Currency,
		PeriodStart:    sub.CurrentPeriodEnd,
		PeriodEnd:      advance(sub.CurrentPeriodEnd, plan.IntervalUnit, plan.IntervalCount),
		Status:         domain.InvoiceOpen,
		CreatedAt:      s.now(),
	}
	if err := s.Invoices.Save(ctx, inv); err != nil {
		return err
	}

	// 调 gateway 收款 (Idempotency-Key 用 invoice_id 防重复扣)
	chargeID, succ, code, gErr := s.Gateway.CreateCharge(ctx, ChargeRequest{
		MerchantID:      sub.MerchantID,
		CustomerID:      sub.CustomerID,
		PaymentMethodID: sub.PaymentMethodID,
		AmountMinor:     inv.AmountMinor,
		Currency:        inv.Currency,
		Idempotency:     inv.InvoiceID,
		Description:     fmt.Sprintf("subscription %s cycle %d", sub.SubscriptionID, sub.CycleCount+1),
		Metadata: map[string]string{
			"subscription_id": sub.SubscriptionID,
			"plan_id":         fmt.Sprint(plan.ID),
			"cycle":           fmt.Sprint(sub.CycleCount + 1),
		},
	})

	inv.AttemptCount++
	inv.ChargeID = chargeID

	if gErr != nil || !succ {
		inv.Status = domain.InvoiceOpen
		inv.FailureCode = code
		inv.FailureMessage = errorOrEmpty(gErr)
		next := s.now().Add(dunningBackoff(inv.AttemptCount))
		inv.NextRetryAt = &next
		_ = s.Invoices.Save(ctx, inv)

		// 进入 dunning / past_due
		sub.Status = domain.StatusPastDue
		sub.DunningRetryCount++
		now := s.now()
		sub.LastDunningAt = &now
		_ = s.Subs.Save(ctx, sub)

		_ = s.Dunning.Append(ctx, &domain.DunningEvent{
			SubscriptionID: sub.SubscriptionID, InvoiceID: inv.InvoiceID,
			Attempt: inv.AttemptCount, Action: "retry_scheduled", Outcome: "failed",
			Detail: inv.FailureMessage, OccurredAt: s.now(),
		})
		return fmt.Errorf("charge failed code=%s err=%v", code, gErr)
	}

	// 成功
	now := s.now()
	inv.Status = domain.InvoicePaid
	inv.PaidAt = &now
	_ = s.Invoices.Save(ctx, inv)

	sub.Status = domain.StatusActive
	sub.CurrentPeriodStart = sub.CurrentPeriodEnd
	sub.CurrentPeriodEnd = inv.PeriodEnd
	sub.CycleCount++
	sub.DunningRetryCount = 0
	sub.UpdatedAt = now
	_ = s.Subs.Save(ctx, sub)

	// 发事件 → moneyflow engine 自动按 graph 分账
	if s.Events != nil {
		_ = s.Events.Publish(ctx, "subscription.cycle", map[string]any{
			"event":             "subscription.cycle",
			"subscription_id":   sub.SubscriptionID,
			"invoice_id":        inv.InvoiceID,
			"charge_id":         chargeID,
			"merchant_id":       sub.MerchantID,
			"customer_id":       sub.CustomerID,
			"amount_minor":      inv.AmountMinor,
			"currency":          inv.Currency,
			"cycle":             sub.CycleCount,
			"plan_id":           plan.ID,
			"moneyflow_graph":   plan.MoneyFlowGraph, // 路由到指定 graph
			"attributes":        sub.Attributes,
		})
	}
	return nil
}

// ─── Dunning 重试 ─────────────────────────────────────────────────────

// DunningTick 找 past_due 订阅, 重试扣款; 重试上限到 → canceled.
func (s *Service) DunningTick(ctx context.Context, limit int) (int, error) {
	subs, err := s.Subs.FindDueForDunning(ctx, s.now(), limit)
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, sub := range subs {
		if err := s.retryDunning(ctx, sub); err != nil {
			s.Log.Warn("dunning failed", zap.String("sub", sub.SubscriptionID), zap.Error(err))
		}
		processed++
	}
	return processed, nil
}

func (s *Service) retryDunning(ctx context.Context, sub *domain.Subscription) error {
	plan, err := s.Plans.Get(ctx, sub.PlanID)
	if err != nil {
		return err
	}
	// 上限超过 → cancel
	maxRetries := 3
	if sub.DunningRetryCount >= maxRetries {
		sub.Status = domain.StatusCanceled
		now := s.now()
		sub.CanceledAt = &now
		_ = s.Subs.Save(ctx, sub)
		_ = s.Dunning.Append(ctx, &domain.DunningEvent{
			SubscriptionID: sub.SubscriptionID,
			Attempt: sub.DunningRetryCount, Action: "cancel", Outcome: "succeeded",
			Detail: "max retries exceeded", OccurredAt: s.now(),
		})
		return nil
	}

	// 拉最新 open invoice 重扣
	inv, err := s.Invoices.GetLatestOpen(ctx, sub.SubscriptionID)
	if err != nil || inv == nil {
		return errors.New("no open invoice")
	}
	_, succ, code, gErr := s.Gateway.CreateCharge(ctx, ChargeRequest{
		MerchantID: sub.MerchantID, CustomerID: sub.CustomerID,
		PaymentMethodID: sub.PaymentMethodID, AmountMinor: inv.AmountMinor,
		Currency: inv.Currency, Idempotency: inv.InvoiceID,
	})
	inv.AttemptCount++

	if succ {
		now := s.now()
		inv.Status = domain.InvoicePaid
		inv.PaidAt = &now
		_ = s.Invoices.Save(ctx, inv)
		sub.Status = domain.StatusActive
		sub.CurrentPeriodStart = sub.CurrentPeriodEnd
		sub.CurrentPeriodEnd = advance(sub.CurrentPeriodEnd, plan.IntervalUnit, plan.IntervalCount)
		sub.CycleCount++
		sub.DunningRetryCount = 0
		_ = s.Subs.Save(ctx, sub)
		// 同样发事件给 moneyflow
		if s.Events != nil {
			_ = s.Events.Publish(ctx, "subscription.cycle", map[string]any{
				"event": "subscription.cycle", "subscription_id": sub.SubscriptionID,
				"amount_minor": inv.AmountMinor, "currency": inv.Currency,
				"moneyflow_graph": plan.MoneyFlowGraph,
			})
		}
		return nil
	}

	// 仍失败
	sub.DunningRetryCount++
	now := s.now()
	sub.LastDunningAt = &now
	_ = s.Subs.Save(ctx, sub)
	next := s.now().Add(dunningBackoff(inv.AttemptCount))
	inv.NextRetryAt = &next
	inv.FailureCode = code
	inv.FailureMessage = errorOrEmpty(gErr)
	_ = s.Invoices.Save(ctx, inv)
	_ = s.Dunning.Append(ctx, &domain.DunningEvent{
		SubscriptionID: sub.SubscriptionID, InvoiceID: inv.InvoiceID,
		Attempt: inv.AttemptCount, Action: "retry", Outcome: "failed",
		Detail: inv.FailureMessage, OccurredAt: s.now(),
	})
	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────

func dunningBackoff(attempt int) time.Duration {
	// 3 / 7 / 21 day 经验值
	days := []int{3, 7, 21}
	if attempt-1 < 0 || attempt-1 >= len(days) {
		return 21 * 24 * time.Hour
	}
	return time.Duration(days[attempt-1]) * 24 * time.Hour
}

func advance(t time.Time, unit string, n int) time.Time {
	switch unit {
	case "day":
		return t.AddDate(0, 0, n)
	case "week":
		return t.AddDate(0, 0, 7*n)
	case "month":
		return t.AddDate(0, n, 0)
	case "year":
		return t.AddDate(n, 0, 0)
	}
	return t.AddDate(0, n, 0) // default monthly
}

func errorOrEmpty(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

func genID(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}
