// Package workflow — split-payment 编排逻辑。
//
// 设计原则:
//   * split-payment 是 *编排服务*, 不持账
//   * 资金真相在 accounting-system, 这里只存 Rule (规则定义) + Plan (拆分计划 + 状态)
//   * Plan 提交给 accounting 用 AtomicBatchBooking 原子下账 — 失败整体回滚
//
// 状态机:
//   Plan: created → calculated → executing → ┬→ completed
//                                              └→ failed (accounting 拒绝, 已自动回滚)

package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"reconcile-system/packages/split-payment/internal/clients"
	"reconcile-system/packages/split-payment/internal/domain"
)

// ExecuteRequest 执行拆分入参。
type ExecuteRequest struct {
	ChargeID    string
	AmountMinor int64
	Currency    string
	RuleID      int64
	MerchantID  string
	Attributes  map[string]string
	TraceID     string
}

// Service 拆分服务。
type Service struct {
	Repo       Repository
	Accounting *clients.AccountingClient // 真实账本 (accounting-system 客户端)
	Audit      AuditClient
	Now        func() time.Time
}

// Repository 持久化接口 — 只存 rule 配置 + plan 编排状态, 不存资金。
type Repository interface {
	GetRule(ctx context.Context, id int64) (*domain.Rule, error)
	SavePlan(ctx context.Context, p *domain.Plan) (int64, error)
	UpdatePlan(ctx context.Context, p *domain.Plan) error
	GetPlanByCharge(ctx context.Context, chargeID string) (*domain.Plan, error)
	SaveReversal(ctx context.Context, r *domain.Reversal) (int64, error)
}

// AuditClient audit-log 调用。
type AuditClient interface {
	Write(ctx context.Context, ev map[string]any) error
}

// ─── 主流程 ────────────────────────────────────────────────────────────

// Execute 计算 + 提交 accounting 原子下账。
//
// 失败语义:
//   计算失败       → plan 状态 failed, accounting 没动 (没调)
//   accounting 失败 → plan 状态 failed, accounting 内部已经回滚所有 entries
//   全成功         → plan 状态 completed, voucher_no 落库供审计
func (s *Service) Execute(ctx context.Context, req ExecuteRequest) (*domain.Plan, error) {
	if req.AmountMinor <= 0 {
		return nil, errors.New("amount must be positive")
	}
	rule, err := s.Repo.GetRule(ctx, req.RuleID)
	if err != nil {
		return nil, fmt.Errorf("rule: %w", err)
	}
	if rule.Status != "active" {
		return nil, fmt.Errorf("rule %d not active", rule.ID)
	}
	if rule.Currency != "" && rule.Currency != req.Currency {
		return nil, fmt.Errorf("currency mismatch: rule=%s req=%s", rule.Currency, req.Currency)
	}

	plan := &domain.Plan{
		RuleID:      rule.ID,
		ChargeID:    req.ChargeID,
		MerchantID:  req.MerchantID,
		AmountMinor: req.AmountMinor,
		Currency:    req.Currency,
		Status:      "created",
		TraceID:     req.TraceID,
		CreatedAt:   s.now(),
		UpdatedAt:   s.now(),
	}
	if rule.HoldPeriodDays > 0 {
		until := s.now().Add(time.Duration(rule.HoldPeriodDays) * 24 * time.Hour)
		plan.HoldUntil = &until
	}

	// 1) 计算 (纯本地, 不动 accounting)
	items, err := calculate(rule, req)
	if err != nil {
		plan.Status = "failed"
		plan.ErrorMsg = err.Error()
		_, _ = s.Repo.SavePlan(ctx, plan)
		return plan, err
	}
	plan.Items = items
	plan.Status = "calculated"

	id, err := s.Repo.SavePlan(ctx, plan)
	if err != nil {
		return nil, err
	}
	plan.ID = id

	// 2) 提交 accounting 原子下账 (AtomicBatchBooking - all-or-nothing)
	plan.Status = "executing"
	_ = s.Repo.UpdatePlan(ctx, plan)

	voucherNo, txIDs, err := s.Accounting.PostSplitAtomic(ctx, plan)
	if err != nil {
		plan.Status = "failed"
		plan.ErrorMsg = fmt.Sprintf("accounting: %v", err)
		_ = s.Repo.UpdatePlan(ctx, plan)
		s.audit(ctx, map[string]any{
			"action": "split.execute.failed",
			"plan_id": plan.ID, "charge": plan.ChargeID, "err": err.Error(),
		})
		return plan, err
	}

	// 3) 落 voucher_no / tx_id 到 plan items (审计对账时能跟 accounting 关联)
	for i := range plan.Items {
		plan.Items[i].Status = "ledger_posted"
		if i < len(txIDs) {
			plan.Items[i].Reason = "tx=" + txIDs[i]
		}
	}
	plan.Status = "completed"
	plan.UpdatedAt = s.now()
	_ = s.Repo.UpdatePlan(ctx, plan)
	// 注: 数据库写失败但 accounting 已经下账 — 不致命, accounting 是事实,
	// 这边 plan 状态是 cache, 后续 reconciliation 会一致 (reconplatform 帮忙发现)

	s.audit(ctx, map[string]any{
		"action":     "split.execute",
		"plan_id":    plan.ID,
		"charge":     plan.ChargeID,
		"merchant":   plan.MerchantID,
		"amount":     plan.AmountMinor,
		"items":      plan.Items,
		"voucher_no": voucherNo,
	})
	return plan, nil
}

// Reverse 退款时反向 — 按原比例从各方账户扣回; 不足由平台垫付。
//
// 关键: 余额查询和提交不是原子的 (TOCTOU window), 但 accounting 内部对每个账户
// 的扣减是强一致 — 如果在 GetBalance 到 AtomicBatch 之间被别的 tx 抽走, accounting
// 会拒绝 → 这边 retry (用 backoff) 或人工介入.
func (s *Service) Reverse(ctx context.Context, refundID, originalChargeID string, refundAmount int64) (*domain.Reversal, error) {
	orig, err := s.Repo.GetPlanByCharge(ctx, originalChargeID)
	if err != nil || orig == nil {
		return nil, fmt.Errorf("original plan not found: %v", err)
	}
	if orig.Status != "completed" {
		return nil, fmt.Errorf("can only reverse completed plan, status=%s", orig.Status)
	}
	if refundAmount <= 0 || refundAmount > orig.AmountMinor {
		return nil, fmt.Errorf("invalid refund amount %d (orig=%d)", refundAmount, orig.AmountMinor)
	}

	rev := &domain.Reversal{
		OriginalPlanID: orig.ID,
		RefundID:       refundID,
		AmountMinor:    refundAmount,
		Status:         "computing",
		CreatedAt:      s.now(),
	}

	// 1) 按原比例算每个收款方该退多少
	for _, it := range orig.Items {
		share := int64(0)
		if orig.AmountMinor > 0 {
			share = it.AmountMinor * refundAmount / orig.AmountMinor
		}
		if share == 0 {
			continue
		}
		ri := domain.ReversalItem{Beneficiary: it.Beneficiary, AmountMinor: share}
		// 2) 查 accounting 当前余额是否够扣
		bal, err := s.Accounting.GetBalance(ctx, it.Beneficiary, orig.Currency)
		if err != nil {
			return nil, fmt.Errorf("get balance %s: %w", it.Beneficiary, err)
		}
		if bal < share {
			ri.Shortfall = share - bal
			ri.PlatformCovered = true
		}
		rev.Items = append(rev.Items, ri)
	}

	id, err := s.Repo.SaveReversal(ctx, rev)
	if err != nil {
		return nil, err
	}
	rev.ID = id

	// 3) accounting 原子下账: 卖家扣回 + 平台垫付缺口, 全成功 or 全回滚
	voucher, err := s.Accounting.ReverseSplit(ctx, rev, orig)
	if err != nil {
		rev.Status = "failed"
		return rev, err
	}
	rev.Status = "completed"

	s.audit(ctx, map[string]any{
		"action":     "split.reverse",
		"refund_id":  refundID,
		"plan_id":    orig.ID,
		"amount":     refundAmount,
		"items":      rev.Items,
		"voucher_no": voucher,
	})
	return rev, nil
}

// ─── helpers ──────────────────────────────────────────────────────────

func (s *Service) audit(ctx context.Context, ev map[string]any) {
	if s.Audit == nil {
		return
	}
	_ = s.Audit.Write(ctx, ev)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// ─── 计算 (纯函数, 无副作用) ──────────────────────────────────────────
// 算法:
//   1. 按 position 排序
//   2. percent  : floor(amount * bp / 10000), bp = basis points (10000=100%)
//   3. fixed_minor: 直接固定金额
//   4. remainder: 兜底, 取所有剩余 — 确保 sum == 原始金额 (防尾差丢钱)
//   5. placeholder ({xxx}) 从 Attributes 解析; 不存在且非 Optional → 报错
//   6. 任何一项超出剩余余额 → 错 (规则配错)
//   7. 尾差归最后一项 (向下取整产生的 < 100 minor 残差)

func calculate(rule *domain.Rule, req ExecuteRequest) ([]domain.PlanItem, error) {
	items := make([]domain.RuleItem, len(rule.Items))
	copy(items, rule.Items)
	sort.SliceStable(items, func(i, j int) bool { return items[i].Position < items[j].Position })

	out := []domain.PlanItem{}
	remaining := req.AmountMinor

	for _, item := range items {
		ben := item.Beneficiary
		if item.FromAttribute != "" {
			v, ok := req.Attributes[item.FromAttribute]
			if !ok || v == "" {
				if item.Optional {
					continue
				}
				return nil, fmt.Errorf("missing required attribute %q", item.FromAttribute)
			}
			ben = v
		}

		var amount int64
		switch item.Type {
		case "percent":
			amount = req.AmountMinor * item.Value / 10000
		case "fixed_minor":
			amount = item.Value
		case "remainder":
			amount = remaining
		default:
			return nil, fmt.Errorf("unknown rule type %q", item.Type)
		}

		if item.MaxAmountMinor > 0 && amount > item.MaxAmountMinor {
			amount = item.MaxAmountMinor
		}
		if amount < item.MinAmountMinor {
			if item.Optional {
				continue
			}
			amount = item.MinAmountMinor
		}
		if amount > remaining {
			return nil, fmt.Errorf("split overflows source: pos %d wants %d remain %d",
				item.Position, amount, remaining)
		}
		out = append(out, domain.PlanItem{
			Position:    item.Position,
			Beneficiary: ben,
			AmountMinor: amount,
			Status:      "pending",
		})
		remaining -= amount
	}

	// 尾差归最后一项 — 确保 sum 严格等于原始
	if remaining > 0 && len(out) > 0 {
		out[len(out)-1].AmountMinor += remaining
	}
	return out, nil
}
