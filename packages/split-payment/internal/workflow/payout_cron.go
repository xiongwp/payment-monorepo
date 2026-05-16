// payout_cron.go — SP-10: 周期性扫描 ConnectedAccount, 按 payout_schedule 自动发 Payout.
//
// 触发条件 (cron):
//   - schedule.interval = "daily":   每天凌晨 (UTC) 对所有 enabled + CanPayout 的 account
//   - schedule.interval = "weekly":  WeeklyAnchor 当周第几天 (monday-sunday)
//   - schedule.interval = "monthly": MonthlyAnchor 月份第几号 (1-31)
//   - "manual":                       不自动发, 商户主动 POST /api/payouts
//
// 行为:
//   1. 列所有 enabled 且 CanPayout 的 account
//   2. 对每个: 查可提余额 (调 accounting-system query balance — 占位, 实际接口待加)
//   3. 余额 ≥ 1 USD → 创建 Payout (method=standard, status=pending)
//   4. publish payout.created 事件
//   5. 真实银行通道由 clearing-settlement 服务异步 pickup pending payout → 状态机 in_transit → paid
//
// Hold-period worker (HoldUnstickWorker):
//   1. 扫 RunPlan 里所有带 Hold 的 plan, 检查 Hold.Days 是否到期
//   2. 到期 → 把"暂留账户" 的 unsettled 余额搬到正式账户
//      (实际 accounting batch: unsettled → settled, 走 PostMovements 反向)
//   3. publish "hold.released" 事件 (商户 webhook 可订阅)
//
// 简化: 当前先把 Payout 自动创建做完, 实际银行通道 + 余额查询留 Phase 3.
// Hold worker 也只做"事件 + 状态切换", 实际 accounting 反向记账走现有 PostMovements.

package workflow

import (
	"context"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"

	"go.uber.org/zap"
)

// PayoutCronConfig.
type PayoutCronConfig struct {
	Interval    time.Duration // 扫描间隔, 默认 1h
	MinBalance  int64         // 最小可提现金额 (cents), 默认 100 = $1
	LiveMode    bool          // false → 只 log 不真创建 Payout
}

// DefaultPayoutCronConfig.
func DefaultPayoutCronConfig() PayoutCronConfig {
	return PayoutCronConfig{
		Interval:   1 * time.Hour,
		MinBalance: 100,
		LiveMode:   false,
	}
}

// AccountListerRepo cron 用 (workflow 包不能直接依赖具体 repo).
type AccountListerRepo interface {
	List(ctx context.Context, status string, limit int) ([]*domain.ConnectedAccount, error)
}

// PayoutInserterRepo cron 用.
type PayoutInserterRepo interface {
	Insert(ctx context.Context, p *domain.Payout) error
}

// BalanceQuerier 查 account 可提现余额. 真实实现走 accounting-system gRPC.
//
// Stub: 返一个伪随机值 / 0; 接 accounting-system 后替换.
type BalanceQuerier interface {
	GetAvailableBalance(ctx context.Context, accountID, currency string) (int64, error)
}

// StubBalanceQuerier dev 用 — 所有账户余额 = 0.
type StubBalanceQuerier struct{}

// GetAvailableBalance.
func (s StubBalanceQuerier) GetAvailableBalance(_ context.Context, _, _ string) (int64, error) {
	return 0, nil
}

// PayoutCron 周期性扫账户发 Payout.
type PayoutCron struct {
	Cfg        PayoutCronConfig
	Accounts   AccountListerRepo
	Payouts    PayoutInserterRepo
	Balances   BalanceQuerier
	Events     EventPublisher
	Log        *zap.Logger
}

// Run 阻塞 ticker. ctx 取消即停.
func (c *PayoutCron) Run(ctx context.Context) {
	if c.Cfg.Interval <= 0 {
		c.Cfg.Interval = 1 * time.Hour
	}
	t := time.NewTicker(c.Cfg.Interval)
	defer t.Stop()
	c.Log.Info("payout cron started", zap.Duration("interval", c.Cfg.Interval))
	// 启动期跑一次
	c.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			c.Log.Info("payout cron stopped")
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

// tick 单次扫描.
func (c *PayoutCron) tick(ctx context.Context) {
	accounts, err := c.Accounts.List(ctx, domain.AccountStatusEnabled, 1000)
	if err != nil {
		c.Log.Warn("payout cron: list accounts failed", zap.Error(err))
		return
	}
	now := time.Now().UTC()
	for _, a := range accounts {
		if !a.CanPayout() {
			continue
		}
		// 检查 schedule 当前时间窗
		if !c.shouldPayoutNow(a, now) {
			continue
		}
		// 查余额
		bal, err := c.Balances.GetAvailableBalance(ctx, a.ID, a.DefaultCurrency)
		if err != nil {
			c.Log.Warn("payout cron: balance query failed",
				zap.String("account", a.ID), zap.Error(err))
			continue
		}
		if bal < c.Cfg.MinBalance {
			continue
		}
		// 算 arrival_date: standard = +2 天
		arrival := now.AddDate(0, 0, 2+a.PayoutSchedule.DelayDays)
		po := &domain.Payout{
			ID:             "po_" + randHex8(),
			Account:        a.ID,
			AmountMinor:    bal,
			Currency:       a.DefaultCurrency,
			Destination:    a.PayoutDestination,
			Method:         domain.PayoutMethodStandard,
			Status:         domain.PayoutStatusPending,
			ArrivalDate:    arrival,
			IdempotencyKey: a.ID + "::" + now.Format("2006-01-02"), // 同账户同天一次
			CreatedAt:      now,
		}
		if !c.Cfg.LiveMode {
			c.Log.Info("payout cron: would create (dry-run, set LiveMode)",
				zap.String("account", a.ID), zap.Int64("amount_minor", bal))
			continue
		}
		if err := c.Payouts.Insert(ctx, po); err != nil {
			c.Log.Warn("payout cron: insert failed",
				zap.String("account", a.ID), zap.Error(err))
			continue
		}
		c.Log.Info("payout cron: created",
			zap.String("payout_id", po.ID),
			zap.String("account", a.ID),
			zap.Int64("amount_minor", bal))
		if c.Events != nil {
			_ = c.Events.Publish(ctx, EventPayoutCreated, po)
		}
	}
}

// shouldPayoutNow 当前时间是否符合 account.PayoutSchedule 触发窗.
//
// 简化策略:
//   - daily:   每天 00:00-01:00 UTC 内触发 (假设 cron interval ≤ 1h)
//   - weekly:  WeeklyAnchor 当天的 00:00-01:00 (anchor=monday → 每周一)
//   - monthly: MonthlyAnchor 当月日期的 00:00-01:00 (anchor=15 → 每月 15 日)
//   - manual:  永远 false
//
// 真实生产应该用 idempotency key + 历史记录精确控制, 这里 demo 用.
func (c *PayoutCron) shouldPayoutNow(a *domain.ConnectedAccount, now time.Time) bool {
	switch a.PayoutSchedule.Interval {
	case domain.PayoutDaily:
		return now.Hour() == 0
	case domain.PayoutWeekly:
		if dayMatches(a.PayoutSchedule.WeeklyAnchor, now.Weekday()) && now.Hour() == 0 {
			return true
		}
		return false
	case domain.PayoutMonthly:
		if a.PayoutSchedule.MonthlyAnchor > 0 && a.PayoutSchedule.MonthlyAnchor == now.Day() && now.Hour() == 0 {
			return true
		}
		// 31 = 月末
		if a.PayoutSchedule.MonthlyAnchor == 31 && isLastDayOfMonth(now) && now.Hour() == 0 {
			return true
		}
		return false
	case domain.PayoutManual, "":
		return false
	}
	return false
}

func dayMatches(anchor string, weekday time.Weekday) bool {
	switch anchor {
	case "monday":
		return weekday == time.Monday
	case "tuesday":
		return weekday == time.Tuesday
	case "wednesday":
		return weekday == time.Wednesday
	case "thursday":
		return weekday == time.Thursday
	case "friday":
		return weekday == time.Friday
	case "saturday":
		return weekday == time.Saturday
	case "sunday":
		return weekday == time.Sunday
	}
	return false
}

func isLastDayOfMonth(t time.Time) bool {
	return t.AddDate(0, 0, 1).Day() == 1
}

// ─── Hold-period unstick worker ───────────────────────────────────────

// HoldUnstickConfig.
type HoldUnstickConfig struct {
	Interval time.Duration // 扫描间隔, 默认 1h
}

// DefaultHoldUnstickConfig.
func DefaultHoldUnstickConfig() HoldUnstickConfig {
	return HoldUnstickConfig{Interval: 1 * time.Hour}
}

// PendingHoldsRepo workflow 包用接口, 拉所有"有 hold 但已过期"的 RunPlan.
//
// 现状: RunRepo 没暴露这个查询; 留接口给 Phase 3 接实现.
type PendingHoldsRepo interface {
	ListExpiredHolds(ctx context.Context, now time.Time, limit int) ([]*domain.RunPlan, error)
}

// HoldReleaser SP-FIN-3 把 hold 期资金搬到正式账户的执行器.
//
// Hold 期间钱压在 "unsettled" 账户 (translator 落账时已写),
// 到期后:
//   1. 找该 plan 的 unsettled movements
//   2. 调 accounting-system PostMovements 反向 batch: unsettled → settled
//   3. 标 plan.hold_released=true (用 metadata 字段)
//   4. publish hold.released 事件
//
// 接口抽象 (不直接依赖 clients.AccountingClient, 方便单测).
type HoldReleaser interface {
	ReleaseHold(ctx context.Context, plan *domain.RunPlan) error
}

// HoldUnstickWorker 把到期的 hold 资金从 unsettled 搬到正式账户.
type HoldUnstickWorker struct {
	Cfg      HoldUnstickConfig
	Plans    PendingHoldsRepo
	Releaser HoldReleaser // SP-FIN-3 真接 accounting; nil 退化为仅发事件
	Events   EventPublisher
	Log      *zap.Logger
}

// Run 阻塞 ticker.
func (w *HoldUnstickWorker) Run(ctx context.Context) {
	if w.Cfg.Interval <= 0 {
		w.Cfg.Interval = 1 * time.Hour
	}
	t := time.NewTicker(w.Cfg.Interval)
	defer t.Stop()
	w.Log.Info("hold unstick worker started", zap.Duration("interval", w.Cfg.Interval))
	for {
		select {
		case <-ctx.Done():
			w.Log.Info("hold unstick worker stopped")
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *HoldUnstickWorker) tick(ctx context.Context) {
	if w.Plans == nil {
		return
	}
	now := time.Now().UTC()
	plans, err := w.Plans.ListExpiredHolds(ctx, now, 100)
	if err != nil {
		w.Log.Warn("hold unstick: list failed", zap.Error(err))
		return
	}
	for _, p := range plans {
		// SP-FIN-3: 真接 accounting 把 unsettled → settled.
		if w.Releaser != nil {
			if err := w.Releaser.ReleaseHold(ctx, p); err != nil {
				w.Log.Error("hold release failed",
					zap.Int64("plan_id", p.ID), zap.Error(err))
				continue
			}
		}
		if w.Events != nil {
			_ = w.Events.Publish(ctx, "hold.released", p)
		}
		w.Log.Info("hold unstick: released",
			zap.Int64("plan_id", p.ID))
	}
}

// ─── 默认 HoldReleaser 实现 (基于 accounting batch 反向) ───────────────

// AccountingHoldReleaser 通过调 accounting-system 把 unsettled → settled 搬钱.
//
// 工作流:
//   1. 遍历 plan.Movements, 找 to_account 含 "unsettled" 前缀的 (translator
//      在 hold 期把 to 改成 unsettled/<原 acc>)
//   2. 反向构造两笔: unsettled→0 + 0→正式账户 (借贷平衡)
//   3. 一次 atomic batch 提交给 accounting
//
// 简化版: 当前 RunPlan 没显式存 "hold movement" 标记, 这里按命名约定 (account
// 含 "unsettled_") 识别. 实际生产用 hold_movements 子表精确管理.
type AccountingHoldReleaser struct {
	Accounting accountingPoster // duck-typed interface
	Log        *zap.Logger
}

// accountingPoster 跟 clients.AccountingClient.PostMovements 同签名 (避免循环 import).
type accountingPoster interface {
	PostMovements(ctx context.Context, plan *domain.RunPlan) (voucher string, txIDs []string, err error)
}

// NewAccountingHoldReleaser.
func NewAccountingHoldReleaser(acc accountingPoster, log *zap.Logger) *AccountingHoldReleaser {
	return &AccountingHoldReleaser{Accounting: acc, Log: log}
}

// ReleaseHold 真实搬钱.
func (r *AccountingHoldReleaser) ReleaseHold(ctx context.Context, plan *domain.RunPlan) error {
	// 找 hold 中的 movement (account 名含 "unsettled_" 前缀)
	releases := []domain.Movement{}
	for _, m := range plan.Movements {
		if m.Status != "posted" {
			continue
		}
		// 约定: hold 期暂留账户用 "unsettled_<原 account>" 命名
		if !contains(m.ToAccount, "unsettled_") {
			continue
		}
		realAcc := stripUnsettledPrefix(m.ToAccount)
		releases = append(releases, domain.Movement{
			EdgeFromNode: "hold_release",
			EdgeToNode:   m.EdgeToNode,
			FromAccount:  m.ToAccount, // unsettled → real
			ToAccount:    realAcc,
			AmountMinor:  m.AmountMinor,
			Status:       "pending",
			Reason:       "hold expired",
		})
	}
	if len(releases) == 0 {
		return nil
	}
	// 构造一个反向 plan 调 accounting
	releasePlan := &domain.RunPlan{
		GraphID:       plan.GraphID,
		GraphVersion:  plan.GraphVersion,
		TriggerEvent:  "hold.expired",
		ChargeID:      plan.ChargeID,
		MerchantID:    plan.MerchantID,
		AmountMinor:   plan.AmountMinor,
		Currency:      plan.Currency,
		TransferGroup: plan.TransferGroup,
		Movements:     releases,
	}
	voucher, _, err := r.Accounting.PostMovements(ctx, releasePlan)
	if err != nil {
		return err
	}
	if r.Log != nil {
		r.Log.Info("hold released via accounting",
			zap.Int64("plan_id", plan.ID),
			zap.String("voucher", voucher),
			zap.Int("movements", len(releases)))
	}
	return nil
}

// contains 简单 substring 检查.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// stripUnsettledPrefix "unsettled_seller_balance/123" → "seller_balance/123".
func stripUnsettledPrefix(s string) string {
	const p = "unsettled_"
	if len(s) > len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}
