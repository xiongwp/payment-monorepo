// Package statement — 账单聚合器。
//
// 周期工作:
//   - 每日凌晨跑前一天的 daily statement（draft）
//   - 每月 1 号跑上个月的 monthly statement（final → 触发 payout）
//
// 聚合逻辑:
//   1. 拉 (merchant_id, period) 内所有 status=pending 的 fee_events
//   2. 按 currency 分组 sum gross / fee / refund / chargeback
//   3. 计算 net_payout_minor = total_gross - total_fee - total_refund - total_chargeback
//   4. 写 statement 表
//   5. 把这批 fee_events 状态从 pending → settled，关联 statement_id
//   6. 触发 webhook statement.created 给商户
//
// 跨 currency: 一个 merchant 同一个 period 可能有多个 statements
// （一个 per currency），最终 payout 拆多笔。

package statement

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/billing-system/internal/domain"
)

// Repository 抽象。
type Repository interface {
	// ListPendingEvents 拉 [from, to) 期间内某 merchant 未归账的 fee_events。
	ListPendingEvents(ctx context.Context, merchantID string, from, to time.Time) ([]*domain.FeeEvent, error)
	// SaveStatement 写新账单。
	SaveStatement(ctx context.Context, s *domain.Statement) (int64, error)
	// MarkEventsSettled 把 fee_event_ids 状态切 settled + 关联 statement_id。
	MarkEventsSettled(ctx context.Context, eventIDs []int64, statementID int64) error
	// ListMerchantIDs 给定时间窗，返该期有 fee_event 的所有 merchant_id（驱动聚合）。
	ListMerchantIDs(ctx context.Context, from, to time.Time) ([]string, error)
}

// Aggregator 账单聚合器。
type Aggregator struct {
	repo Repository
	log  *zap.Logger
}

// New 构造。
func New(repo Repository, log *zap.Logger) *Aggregator {
	if log == nil {
		log = zap.NewNop()
	}
	return &Aggregator{repo: repo, log: log}
}

// RunPeriod 跑某时间窗的账单聚合。
//
// from inclusive, to exclusive。typical:
//   月度: from=2026-04-01T00, to=2026-05-01T00
//   日度: from=2026-05-09T00, to=2026-05-10T00
//
// final=true 时 statement 状态为 final（不可再加 fee_event）；
// false 时为 draft（中途 ad-hoc 查看用，不触发 payout）。
func (a *Aggregator) RunPeriod(ctx context.Context, from, to time.Time, final bool) (int, error) {
	if !from.Before(to) {
		return 0, fmt.Errorf("from must be before to")
	}
	merchants, err := a.repo.ListMerchantIDs(ctx, from, to)
	if err != nil {
		return 0, fmt.Errorf("list merchants: %w", err)
	}
	a.log.Info("aggregator starting",
		zap.Time("from", from), zap.Time("to", to),
		zap.Int("merchants", len(merchants)),
		zap.Bool("final", final))

	stmtCount := 0
	for _, mid := range merchants {
		events, err := a.repo.ListPendingEvents(ctx, mid, from, to)
		if err != nil {
			a.log.Warn("list pending events failed",
				zap.String("merchant_id", mid), zap.Error(err))
			continue
		}
		if len(events) == 0 {
			continue
		}
		// 按 currency 分组（每币种一份 statement）
		byCcy := groupByCurrency(events)
		for ccy, evs := range byCcy {
			stmt := buildStatement(mid, ccy, from, to, evs, final)
			id, err := a.repo.SaveStatement(ctx, stmt)
			if err != nil {
				a.log.Warn("save statement failed",
					zap.String("merchant_id", mid), zap.String("currency", ccy),
					zap.Error(err))
				continue
			}
			stmt.ID = id
			// 标记 events settled
			ids := make([]int64, len(evs))
			for i, e := range evs {
				ids[i] = e.ID
			}
			if err := a.repo.MarkEventsSettled(ctx, ids, id); err != nil {
				a.log.Warn("mark events settled failed", zap.Int64("stmt_id", id), zap.Error(err))
			}
			stmtCount++
			a.log.Info("statement created",
				zap.Int64("id", id),
				zap.String("merchant_id", mid),
				zap.String("currency", ccy),
				zap.Int("events", len(evs)),
				zap.Int64("net_payout_minor", stmt.NetPayoutMinor))
		}
	}
	return stmtCount, nil
}

// buildStatement 把一组同币种 events 聚合成 statement。
func buildStatement(merchantID, ccy string, from, to time.Time, evs []*domain.FeeEvent, final bool) *domain.Statement {
	s := &domain.Statement{
		MerchantID:  merchantID,
		PeriodStart: from,
		PeriodEnd:   to,
		Currency:    ccy,
		EventCount:  len(evs),
		IssuedAt:    time.Now().UTC(),
		Status:      domain.StmtDraft,
	}
	if final {
		s.Status = domain.StmtFinal
	}
	for _, e := range evs {
		switch e.EventType {
		case domain.EventCharge:
			s.TotalGrossMinor += e.GrossAmountMinor
			s.TotalFeeMinor += e.FeeMinor
		case domain.EventRefund:
			// refund.fee 一般是负数（退给商户）；统计时把绝对值写到 refund 总额
			absAmt := e.GrossAmountMinor
			if absAmt < 0 {
				absAmt = -absAmt
			}
			s.TotalRefundMinor += absAmt
			// refund fee 如果是负的相当于退商户，全 fee 总额减
			s.TotalFeeMinor += e.FeeMinor
		case domain.EventChargeback:
			s.TotalChargebackMinor += e.GrossAmountMinor
			s.TotalFeeMinor += e.FeeMinor // 罚款 + 维持原 fee
		case domain.EventFXSpread:
			s.TotalFeeMinor += e.FeeMinor // FX markup 单独计入 fee 总额
		}
	}
	s.NetPayoutMinor = s.TotalGrossMinor - s.TotalFeeMinor - s.TotalRefundMinor - s.TotalChargebackMinor
	if s.NetPayoutMinor < 0 {
		s.NetPayoutMinor = 0 // 一般不应负；防御性置 0（应用层 ops 看到 -1 是异常信号）
	}
	return s
}

func groupByCurrency(evs []*domain.FeeEvent) map[string][]*domain.FeeEvent {
	out := map[string][]*domain.FeeEvent{}
	for _, e := range evs {
		ccy := e.Currency
		if ccy == "" {
			ccy = "UNKNOWN"
		}
		out[ccy] = append(out[ccy], e)
	}
	return out
}
