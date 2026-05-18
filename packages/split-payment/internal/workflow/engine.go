// engine.go — workflow 事件驱动引擎。
//
// 1. 订阅 Kafka 业务事件 (charge.succeeded / refund.completed / hold.expired ...)
// 2. 查 Graph 仓库找匹配 trigger 的 graphs
// 3. 用 Translator 把 graph + event → RunPlan
// 4. 调 AccountingClient.PostBatch 原子下账
// 5. 落库 RunPlan + voucher_no, 推 audit

package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"

	"go.uber.org/zap"
)

// AuditClient is the minimal audit sink interface used by Engine. Implementations
// (e.g. logAudit in cmd/server) only need to fulfil Write.
type AuditClient interface {
	Write(ctx context.Context, ev map[string]any) error
}

// Engine 主对象。
//
// SP-6 升级:
//   - 持典 Stripe-style 资源对象 (Transfer / ApplicationFee / Payout) 的 repo,
//     执行成功后落它们各自的表 (graph_run_id 关联回 RunPlan).
//   - 执行前校验 ConnectedAccount.CanTransfer() / CanPayout(), capability 不通过
//     的 movement 标 skipped (不发 accounting, 不阻塞其它).
//
// 所有 Stripe repo 字段可选 (nil = 老行为, 不持 typed 对象). 启动期若用 MySQL
// 即应注入, 见 cmd/server/main.go.
type Engine struct {
	GraphRepo GraphRepo
	RunRepo   RunRepo
	// SP-AC-7: 删除老 *clients.AccountingClient stub 字段, 业务路径走 AccountingMeta (gRPC).
	Audit AuditClient
	Log   *zap.Logger

	// SP-6 typed repo (可选). nil → 不落 Transfer/Fee/Payout 表, 跑老路径.
	AccountRepo  AccountRepo
	TransferRepo TransferRepo
	AppFeeRepo   AppFeeRepo
	PayoutRepo   PayoutRepo

	// SP-8 webhook (可选). nil → 不 publish 事件.
	Events EventPublisher

	// SP-3A 持久化 saga (可选). 非 nil → executeOne 走 saga 模式 (持久 + 失败自动 compensate);
	// nil → 老路径 (直接 accounting.PostMovements + 写 typed repo).
	Saga *SagaCoordinator
	// SP-3A 给 saga 用的 step deps (Transfer/Fee/Payout repo refs).
	// engine 启动期组装一次,后续 BuildSteps 重用.
	SagaDeps *StepDeps

	// SP-9 refund 用 (放 engine 上方便 worker 引用)
	TransferReverseRepo TransferReverseRepo
	AppFeeRefundRepo    AppFeeRefundRepo
	ReversalInsertRepo  ReversalExtRepo

	// SP-AC-7 R1: refund 写两表的原子 helper. nil → 退化到非事务的两步写 (有不一致窗口).
	ReversalApply ReversalApplier

	// SP-AC-7 R5: Reversal 失败时把任务排入 outbox 让后台 worker 重试. nil → 退化 (失败只 log).
	ReversalRetry ReversalRetryEnqueuer

	// SP-3B Risk + AML gate. 可选, nil → 跳过风控直接执行.
	RiskGate *RiskGate

	// SP-3C FX client + snapshot repo. 可选, nil → 不支持跨币种 Transfer (edge.dest_currency 报错).
	FX     FXClient
	FXRepo FXSnapshotRepo

	// SP-AC-3 accounting rule 模式. 非 nil + plan.Transactions 非空 → 走 CreateTransaction 路径.
	AccountingMeta AccountingMetaCaller
}

// AccountingMetaCaller — engine 视角的 accounting client 接口 (避免循环 import).
//
// 真实实现走 clients.AccountingGRPCClient (gRPC TransactionService).
// HTTP 路径已废弃, 只剩 ops/admin UI 用 HTTP.
type AccountingMetaCaller interface {
	CreateTransaction(ctx context.Context, req *domain.TransactionRequest) (*AccountingTxResp, error)
}

// AccountingTxResp 返回值.
//
// Status 沿用 accounting-system 编码: 0=pending / 1=processing / 2=success / 3=failed.
type AccountingTxResp struct {
	VoucherNo string
	Status    int8
	Error     string
}

// AccountRepo workflow 视角 (SP-6 capability gate 用).
type AccountRepo interface {
	Get(ctx context.Context, id string) (*domain.ConnectedAccount, error)
}

// TransferRepo workflow 视角.
type TransferRepo interface {
	Insert(ctx context.Context, t *domain.Transfer) error
	UpdateStatus(ctx context.Context, id, status string, postedAt time.Time) error
}

// AppFeeRepo workflow 视角.
type AppFeeRepo interface {
	Insert(ctx context.Context, f *domain.ApplicationFee) error
}

// PayoutRepo workflow 视角.
type PayoutRepo interface {
	Insert(ctx context.Context, p *domain.Payout) error
}

// EventPublisher SP-8 webhook events.
type EventPublisher interface {
	Publish(ctx context.Context, eventType string, payload any) error
}

// GraphRepo Graph 仓储。
type GraphRepo interface {
	// FindByTrigger 找所有 status=active 且 trigger 含此 event 的 graph。
	FindByTrigger(ctx context.Context, event string) ([]*domain.Graph, error)
	GetByKey(ctx context.Context, key string) (*domain.Graph, error)
	Save(ctx context.Context, g *domain.Graph) (int64, error)
	List(ctx context.Context, status string) ([]*domain.Graph, error)
}

// RunRepo RunPlan 仓储。
type RunRepo interface {
	Save(ctx context.Context, p *domain.RunPlan) (int64, error)
	Update(ctx context.Context, p *domain.RunPlan) error
	GetByCharge(ctx context.Context, chargeID string) ([]*domain.RunPlan, error)
	// SP-AC-7 PH3-7: HoldUnstickWorker 用 — 拉到期的 hold 行 (hold_released=0 AND hold_until<now).
	ListExpiredHolds(ctx context.Context, now time.Time, limit int) ([]*domain.RunPlan, error)
	// SP-AC-7 PH3-7: 释放 hold 后标 hold_released=1.
	MarkHoldReleased(ctx context.Context, runID int64) error
}

// BusinessEvent Kafka / 内部事件总线收到的事件。
type BusinessEvent struct {
	Event      string            `json:"event"`       // e.g. "charge.succeeded"
	ChargeID   string            `json:"charge_id"`
	MerchantID string            `json:"merchant_id"`
	AmountMinor int64            `json:"amount_minor"`
	Currency   string            `json:"currency"`
	Attributes map[string]string `json:"attributes"` // event payload 摊平的属性 (seller_id 等)
	TraceID    string            `json:"trace_id"`
	OccurredAt time.Time         `json:"occurred_at"`
}

// Handle 入口 — 收到事件后调一次。
//
// 行为:
//   - 单事件可能命中多个 graph (e.g. marketplace 分账 + referral 返利两个 graph 都订阅 charge.succeeded)
//   - 每个 graph 独立翻译 + 独立提交 atomic batch (失败互不影响)
//   - 任一 graph 失败 → 落 failed plan + 通知, 但其他 graph 继续
func (e *Engine) Handle(ctx context.Context, ev BusinessEvent) error {
	graphs, err := e.GraphRepo.FindByTrigger(ctx, ev.Event)
	if err != nil {
		return fmt.Errorf("find graphs: %w", err)
	}
	if len(graphs) == 0 {
		e.Log.Debug("no graph matched", zap.String("event", ev.Event))
		return nil
	}

	for _, g := range graphs {
		// 过 trigger filter (Starlark 表达式 — 当前 stub, 见 starlark_filter.go 占位)
		if !matchesTrigger(g, ev) {
			continue
		}
		// SP-AC-7 PH3-8: 验证 ChargeStrategy. 未知值拒绝执行; 占位值 (direct/destination)
		// log warn 但仍按 separate 路径走 (向后兼容).
		if _, warn, csErr := domain.ValidateChargeStrategy(g.Spec.ChargeStrategy); csErr != nil {
			e.Log.Error("graph charge_strategy invalid; skipping",
				zap.String("graph_key", g.Key), zap.Error(csErr))
			continue
		} else if warn != "" {
			e.Log.Warn(warn, zap.String("graph_key", g.Key))
		}
		if err := e.executeOne(ctx, g, ev); err != nil {
			e.Log.Error("graph execution failed",
				zap.String("graph_key", g.Key),
				zap.String("charge", ev.ChargeID),
				zap.Error(err))
			// 继续下一个 graph, 不阻塞
		}
	}
	return nil
}

func (e *Engine) executeOne(ctx context.Context, g *domain.Graph, ev BusinessEvent) error {
	tc := TriggerContext{
		Event:       ev.Event,
		ChargeID:    ev.ChargeID,
		MerchantID:  ev.MerchantID,
		AmountMinor: ev.AmountMinor,
		Currency:    ev.Currency,
		Attributes:  ev.Attributes,
		TraceID:     ev.TraceID,
	}
	// 1) Translate
	plan, err := Translate(g, tc)
	if err != nil {
		// 落 failed plan 便于审计
		failed := &domain.RunPlan{
			GraphID: g.ID, GraphVersion: g.Version,
			TriggerEvent: ev.Event, ChargeID: ev.ChargeID, MerchantID: ev.MerchantID,
			AmountMinor: ev.AmountMinor, Currency: ev.Currency,
			Status: "failed", ErrorMsg: err.Error(), TraceID: ev.TraceID,
			CreatedAt: time.Now().UTC(),
		}
		_, _ = e.RunRepo.Save(ctx, failed)
		return err
	}
	plan.Status = PlanStatusCreated
	plan.CreatedAt = time.Now().UTC()
	id, err := e.RunRepo.Save(ctx, plan)
	if err != nil {
		return fmt.Errorf("save plan: %w", err)
	}
	plan.ID = id

	// SP-3B: Risk + AML gate (Translate 之后, 任何资金动作之前).
	if e.RiskGate != nil {
		decision, reason, rerr := e.RiskGate.Evaluate(ctx, tc, g.Key)
		switch decision {
		case RiskDeny:
			plan.Status = PlanStatusRejected
			plan.ErrorMsg = "risk gate denied: " + reason
			_ = e.RunRepo.Update(ctx, plan)
			e.publishEvent(ctx, EventFlowRejected, plan)
			e.Log.Warn("plan rejected by risk gate",
				zap.Int64("plan_id", plan.ID),
				zap.String("reason", reason))
			return nil // 不返 err — 业务侧风控拒绝是预期行为
		case RiskReview:
			plan.Status = PlanStatusAwaitingReview
			plan.ErrorMsg = "awaiting manual review: " + reason
			_ = e.RunRepo.Update(ctx, plan)
			e.publishEvent(ctx, EventFlowAwaitingReview, plan)
			e.Log.Info("plan awaiting review",
				zap.Int64("plan_id", plan.ID),
				zap.String("reason", reason))
			return nil // 不执行, 等人工 admin UI 放行 (Phase 3D 加)
		case RiskAllow:
			// 继续
		}
		_ = rerr // err 已在 Evaluate 内按 FailSafe 策略处理
	}

	// SP-3C: 跨币种 Transfer 换汇 (translator 标了 fx_pending 的, 走 FX client 拿 rate).
	if e.FX != nil {
		e.applyFX(ctx, plan)
	}

	// SP-6: capability gate — Transfer 前校验 destination account 能否收钱;
	// 不能 → 把对应 movement 标 skipped, 不发 accounting.
	if e.AccountRepo != nil {
		e.applyCapabilityGate(ctx, plan)
	}

	// SP-AC-3: accounting rule 模式 — 优先级最高.
	// translator 输出了 plan.Transactions → 顺序调 CreateTransaction (accounting 内部按 rule 拆借贷).
	// 失败 → 调过的事务不可逆 (accounting 自己负责跨 entry 原子), 标 plan failed 即可;
	// 严格回滚靠 accounting 反向 event (e.g. user_topup.reversed) 由 caller 触发.
	if e.AccountingMeta != nil && len(plan.Transactions) > 0 {
		return e.runAccountingTransactions(ctx, plan)
	}

	// SP-3A: 若挂了 SagaCoordinator → 走持久化 saga 路径 (推荐生产模式).
	//
	// Saga 每 step 落表, 失败自动 compensate, 进程重启可 resume.
	// 走 saga 时跳过下面的 accounting.PostMovements + persistTypedObjects,
	// 这些逻辑被 BuildSteps 拆到每个 step 的 Execute / Compensate.
	if e.Saga != nil && e.SagaDeps != nil {
		return e.runSaga(ctx, plan)
	}

	// SP-AC-7: 已删除老 fallback (clients.AccountingClient.PostMovements + typed-objects 链).
	// 走到这意味着 AccountingMeta 和 Saga 都没 wire — 配置错, 不允许跑下去 (资金安全).
	plan.Status = PlanStatusFailed
	plan.ErrorMsg = "no execution backend wired (AccountingMeta and Saga both nil)"
	_ = e.RunRepo.Update(ctx, plan)
	return errors.New("engine: no execution backend wired (need AccountingMeta or Saga)")
}

// runAccountingTransactions (SP-AC-3) 顺序调 accounting.CreateTransaction 提交每条 rule.
//
// 行为:
//   - 顺序执行 (不并发, 保证审计顺序可读)
//   - 任一失败 → plan.Status=failed, 不自动 compensate (accounting 反向用专门的 reversal 事件)
//   - 成功 → 累加 voucher_no 到 plan.VoucherNo, 标各 tx posted
//
// 跟 saga 路径互斥 (engine 优先走 AccountingMeta != nil 这条).
func (e *Engine) runAccountingTransactions(ctx context.Context, plan *domain.RunPlan) error {
	plan.Status = PlanStatusExecuting
	_ = e.RunRepo.Update(ctx, plan)

	vouchers := []string{}
	for i := range plan.Transactions {
		tx := &plan.Transactions[i]
		resp, err := e.AccountingMeta.CreateTransaction(ctx, tx)
		if err != nil {
			tx.Status = "failed"
			tx.ErrorMsg = err.Error()
			plan.Status = PlanStatusFailed
			plan.ErrorMsg = fmt.Sprintf("tx %s (%s/%s) failed: %s",
				tx.OrderNo, tx.ProductCode, tx.EventCode, err.Error())
			_ = e.RunRepo.Update(ctx, plan)
			e.Log.Error("accounting CreateTransaction failed",
				zap.String("order_no", tx.OrderNo),
				zap.String("event", tx.EventCode),
				zap.Error(err))
			return err
		}
		tx.Status = "posted"
		tx.VoucherNo = resp.VoucherNo
		if resp.VoucherNo != "" {
			vouchers = append(vouchers, resp.VoucherNo)
		}
		e.Log.Info("accounting transaction posted",
			zap.String("order_no", tx.OrderNo),
			zap.String("event", tx.EventCode),
			zap.String("voucher", resp.VoucherNo))
	}
	plan.Status = PlanStatusCompleted
	if len(vouchers) > 0 {
		plan.VoucherNo = vouchers[0] // 首张凭证号
		if len(vouchers) > 1 {
			plan.VoucherNo += "+" + fmt.Sprintf("%d", len(vouchers)-1) // 标记还有 N 张
		}
	}
	_ = e.RunRepo.Update(ctx, plan)
	return nil
}

// applyFX (SP-3C) 遍历 plan.Transfers, 把 metadata["fx_pending"] 标记的换算成目标币种.
//
// translator 是纯函数不调网络, 这里 engine 调 FXClient 拿 rate 落 snapshot,
// 然后改 Transfer.AmountMinor + Metadata["fx_snapshot_id"]/["fx_rate"].
func (e *Engine) applyFX(ctx context.Context, plan *domain.RunPlan) {
	for i := range plan.Transfers {
		t := &plan.Transfers[i]
		pending, ok := t.Metadata["fx_pending"]
		if !ok || pending == "" {
			continue
		}
		// pending = "USD->EUR"
		var from, to string
		_, _ = fmt.Sscanf(pending, "%[^-]->%s", &from, &to)
		if from == "" || to == "" {
			continue
		}
		srcAmtStr := t.Metadata["fx_source_amount_minor"]
		var srcAmt int64
		_, _ = fmt.Sscanf(srcAmtStr, "%d", &srcAmt)
		if srcAmt <= 0 {
			srcAmt = t.AmountMinor // fallback
		}
		converted, snap, err := ConvertAmount(ctx, e.FX, e.FXRepo, srcAmt, from, to, plan.ID)
		if err != nil {
			e.Log.Warn("fx convert failed",
				zap.String("transfer", t.ID),
				zap.String("from", from), zap.String("to", to),
				zap.Error(err))
			// 失败 → 标 transfer failed, capability gate 也会跳
			t.Status = domain.TransferStatusFailed
			continue
		}
		t.AmountMinor = converted
		t.Currency = to
		if snap != nil {
			if t.Metadata == nil {
				t.Metadata = map[string]string{}
			}
			t.Metadata["fx_snapshot_id"] = snap.ID
			t.Metadata["fx_rate"] = fmt.Sprintf("%.6f", snap.Rate)
			delete(t.Metadata, "fx_pending")
			delete(t.Metadata, "fx_source_amount_minor")
		}
	}
}

// ExecuteApproved (SP-FIN-4) 4-eyes 审批通过后重新触发, 跳过 risk gate.
//
// 复用 executeOne 的 capability / persist / accounting 路径, 仅把 RiskGate 临时移除.
// 完成后状态 → completed (或 failed if 中途出错).
func (e *Engine) ExecuteApproved(ctx context.Context, plan *domain.RunPlan) error {
	// 临时禁用 risk gate (审批通过 = 显式 override)
	originalGate := e.RiskGate
	e.RiskGate = nil
	defer func() { e.RiskGate = originalGate }()

	// 重构 TriggerContext 跑一遍下游 (capability gate / accounting / saga).
	// 注: 不重新 Translate, 用 plan 现有的 Movements / Transfers.
	// SP-6 capability gate 仍然跑 (账户 capabilities 可能变了)
	if e.AccountRepo != nil {
		e.applyCapabilityGate(ctx, plan)
	}
	if e.FX != nil {
		e.applyFX(ctx, plan)
	}
	e.persistTypedObjects(ctx, plan)

	if e.Saga != nil && e.SagaDeps != nil {
		return e.runSaga(ctx, plan)
	}
	if e.AccountingMeta != nil && len(plan.Transactions) > 0 {
		return e.runAccountingTransactions(ctx, plan)
	}
	// SP-AC-7: 已删除老 *clients.AccountingClient 直调路径.
	plan.Status = PlanStatusFailed
	plan.ErrorMsg = "ExecuteApproved: no execution backend wired"
	_ = e.RunRepo.Update(ctx, plan)
	return errors.New("ExecuteApproved: no execution backend wired (need AccountingMeta or Saga)")
}

// runSaga (SP-3A) 走持久化 saga 路径.
//
// BuildSteps 把 plan.Transfers/Fees/Payouts 编排成 SagaStep 序列, SagaCoordinator
// 同步跑每一步, 落 SagaStore. 失败逆序 compensate 已成功 step (Transfer→Reversal).
//
// 进程崩了重启后, main.go 调 Saga.ResumeUnfinished() 会把 state=forwarding
// 的 saga 接着跑 — 要求 Step 幂等 (idempotency_key 已保证).
func (e *Engine) runSaga(ctx context.Context, plan *domain.RunPlan) error {
	steps := BuildSteps(plan, *e.SagaDeps)
	if len(steps) == 0 {
		// 没事干, 直接完成
		plan.Status = "completed"
		_ = e.RunRepo.Update(ctx, plan)
		return nil
	}
	inst := &SagaInstance{
		SagaID:        fmt.Sprintf("saga_%d_%d", plan.ID, time.Now().UnixNano()),
		GraphRunID:    plan.ID,
		CorrelationID: plan.ChargeID,
		Steps:         steps,
	}
	plan.Status = "executing"
	_ = e.RunRepo.Update(ctx, plan)
	err := e.Saga.Start(ctx, inst)
	if err != nil {
		plan.Status = "failed"
		plan.ErrorMsg = err.Error()
		_ = e.RunRepo.Update(ctx, plan)
		return err
	}
	plan.Status = "completed"
	_ = e.RunRepo.Update(ctx, plan)
	return nil
}

// applyCapabilityGate (SP-6) 把不满足 capability 的 Transfer 标 failed/skipped, 不发 accounting.
//
// 规则:
//   - Transfer 看 destination account: !CanTransfer() → status=failed
//   - Payout 看 source account: !CanPayout() → status=failed
//   - 未知 account (Get 返 ErrNotFound) → 当作 failed, 资金安全优先
//
// 失败的 Transfer 在 plan.Transfers 中标记, 同时把 plan.Movements 对应行也标 skipped.
func (e *Engine) applyCapabilityGate(ctx context.Context, plan *domain.RunPlan) {
	for i := range plan.Transfers {
		t := &plan.Transfers[i]
		a, err := e.AccountRepo.Get(ctx, t.DestinationAccount)
		if err != nil || a == nil || !a.CanTransfer() {
			t.Status = domain.TransferStatusFailed
			e.markMovementSkipped(plan, t.SourceAccount, t.DestinationAccount,
				"capability gate: destination cannot receive transfer")
		}
	}
	for i := range plan.Payouts {
		p := &plan.Payouts[i]
		a, err := e.AccountRepo.Get(ctx, p.Account)
		if err != nil || a == nil || !a.CanPayout() {
			p.Status = domain.PayoutStatusFailed
			p.FailureCode = "capability_inactive"
			p.FailureMessage = "account cannot payout (capability inactive or status not enabled)"
			e.markMovementSkipped(plan, p.Account, "", "capability gate: cannot payout")
		}
	}
}

// markMovementSkipped 把 plan.Movements 里对应行 (按 fromAcc/toAcc) 状态置 skipped.
func (e *Engine) markMovementSkipped(plan *domain.RunPlan, fromAcc, toAcc, reason string) {
	for i := range plan.Movements {
		m := &plan.Movements[i]
		if m.Status != "pending" {
			continue
		}
		if m.FromAccount == fromAcc && (toAcc == "" || m.ToAccount == toAcc) {
			m.Status = "skipped"
			m.Reason = reason
			break
		}
	}
}

// persistTypedObjects 把 typed Transfer/AppFee/Payout 插表, graph_run_id 回填.
//
// 写失败仅 warn, 不阻塞 accounting (审计追溯属于次要路径).
func (e *Engine) persistTypedObjects(ctx context.Context, plan *domain.RunPlan) {
	if e.TransferRepo != nil {
		for i := range plan.Transfers {
			t := &plan.Transfers[i]
			t.GraphRunID = plan.ID
			if err := e.TransferRepo.Insert(ctx, t); err != nil {
				e.Log.Warn("transfer insert failed",
					zap.String("id", t.ID), zap.Error(err))
			}
		}
	}
	if e.AppFeeRepo != nil {
		for i := range plan.ApplicationFees {
			f := &plan.ApplicationFees[i]
			f.GraphRunID = plan.ID
			if err := e.AppFeeRepo.Insert(ctx, f); err != nil {
				e.Log.Warn("application_fee insert failed",
					zap.String("id", f.ID), zap.Error(err))
			}
		}
	}
	if e.PayoutRepo != nil {
		for i := range plan.Payouts {
			p := &plan.Payouts[i]
			p.GraphRunID = plan.ID
			if err := e.PayoutRepo.Insert(ctx, p); err != nil {
				e.Log.Warn("payout insert failed",
					zap.String("id", p.ID), zap.Error(err))
			}
		}
	}
}

// publishEvent SP-8 把状态机迁移 fan-out 到 webhook dispatcher (kafka).
//
// EventPublisher nil → 静默跳过 (Phase 1 没接 Kafka 时不报错).
func (e *Engine) publishEvent(ctx context.Context, evtType string, payload any) {
	if e.Events == nil {
		return
	}
	if err := e.Events.Publish(ctx, evtType, payload); err != nil {
		e.Log.Warn("event publish failed",
			zap.String("event", evtType), zap.Error(err))
	}
}

// matchesTrigger 检查触发条件是否命中。
//
// 设计 (MF-3 已升级为真表达式求值, 见 filter_expr.go):
//   - g.Spec.Triggers 列表里每条 (Event + Filter) 都要匹配 ev.Event
//   - Filter 空 → 命中
//   - Filter 非空 → 走 EvalFilter, 支持:
//       字段:  amount_minor / currency / event / charge_id / merchant_id /
//             attr.<key> (Attributes 取) / "<key>" 含点也行 (e.g. merchant.tier)
//       比较:  == != < <= > >=
//       逻辑:  and / or / not / in (列表成员)
//       例: merchant.tier == 'marketplace' and amount_minor > 10000
//
// 老 "k=v" 风格自动 upgrade 到 "k==v" — 向后兼容已上线规则.
func matchesTrigger(g *domain.Graph, ev BusinessEvent) bool {
	for _, t := range g.Spec.Triggers {
		if t.Event != ev.Event {
			continue
		}
		if t.Filter == "" {
			return true
		}
		if EvalFilter(t.Filter, ev) {
			return true
		}
	}
	return false
}
