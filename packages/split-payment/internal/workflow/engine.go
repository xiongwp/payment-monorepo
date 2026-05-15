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
	"encoding/json"
	"fmt"
	"time"

	"reconcile-system/packages/split-payment/internal/clients"
	"reconcile-system/packages/split-payment/internal/domain"

	"go.uber.org/zap"
)

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
	GraphRepo  GraphRepo
	RunRepo    RunRepo
	Accounting *clients.AccountingClient
	Audit      AuditClient
	Log        *zap.Logger

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

	// SP-3B Risk + AML gate. 可选, nil → 跳过风控直接执行.
	RiskGate *RiskGate

	// SP-3C FX client + snapshot repo. 可选, nil → 不支持跨币种 Transfer (edge.dest_currency 报错).
	FX     FXClient
	FXRepo FXSnapshotRepo
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

	// SP-3A: 若挂了 SagaCoordinator → 走持久化 saga 路径 (推荐生产模式).
	//
	// Saga 每 step 落表, 失败自动 compensate, 进程重启可 resume.
	// 走 saga 时跳过下面的 accounting.PostMovements + persistTypedObjects,
	// 这些逻辑被 BuildSteps 拆到每个 step 的 Execute / Compensate.
	if e.Saga != nil && e.SagaDeps != nil {
		return e.runSaga(ctx, plan)
	}

	// SP-6: 先把 typed 对象插表 (status=created/pending), 这样即使 accounting 失败也有审计痕迹.
	// graph_run_id 回填到每条 typed 对象, 让运营反查 "这个 Transfer 来自哪次 run".
	e.persistTypedObjects(ctx, plan)

	// 2) PostBatch (accounting AtomicBatchBooking)
	plan.Status = "executing"
	_ = e.RunRepo.Update(ctx, plan)

	voucher, txIDs, err := e.Accounting.PostMovements(ctx, plan)
	if err != nil {
		plan.Status = "failed"
		plan.ErrorMsg = err.Error()
		_ = e.RunRepo.Update(ctx, plan)
		return err
	}
	plan.VoucherNo = voucher
	// 把 tx_id 回填到对应 movement (按顺序对应)
	txIdx := 0
	for i := range plan.Movements {
		if plan.Movements[i].Status != "pending" {
			continue
		}
		if txIdx < len(txIDs) {
			plan.Movements[i].TxID = txIDs[txIdx]
		}
		plan.Movements[i].Status = "posted"
		txIdx++
	}
	plan.Status = "completed"
	_ = e.RunRepo.Update(ctx, plan)

	// SP-6: accounting 成功 → Transfer 状态 created → posted (附 PostedAt).
	now := time.Now().UTC()
	if e.TransferRepo != nil {
		for _, t := range plan.Transfers {
			if t.Status == domain.TransferStatusFailed {
				continue // capability gate 已挡掉的
			}
			_ = e.TransferRepo.UpdateStatus(ctx, t.ID, domain.TransferStatusPosted, now)
			e.publishEvent(ctx, "transfer.posted", t)
		}
	}
	// AppFee posted → collected
	for _, f := range plan.ApplicationFees {
		e.publishEvent(ctx, "application_fee.collected", f)
	}
	// Payout 不在这里完成 (走 cron / 银行通道 worker), 这里只发 created 事件
	for _, p := range plan.Payouts {
		e.publishEvent(ctx, "payout.created", p)
	}

	// 3) Audit
	if e.Audit != nil {
		body, _ := json.Marshal(plan)
		_ = e.Audit.Write(ctx, map[string]any{
			"action":  "moneyflow.execute",
			"event":   ev.Event,
			"graph":   g.Key,
			"plan_id": plan.ID,
			"charge":  ev.ChargeID,
			"voucher": voucher,
			"detail":  string(body),
		})
	}
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

	// 老路径: 直接 accounting batch
	plan.Status = PlanStatusExecuting
	_ = e.RunRepo.Update(ctx, plan)
	voucher, txIDs, err := e.Accounting.PostMovements(ctx, plan)
	if err != nil {
		plan.Status = PlanStatusFailed
		plan.ErrorMsg = "approved exec failed: " + err.Error()
		_ = e.RunRepo.Update(ctx, plan)
		return err
	}
	plan.VoucherNo = voucher
	txIdx := 0
	for i := range plan.Movements {
		if plan.Movements[i].Status != "pending" {
			continue
		}
		if txIdx < len(txIDs) {
			plan.Movements[i].TxID = txIDs[txIdx]
		}
		plan.Movements[i].Status = "posted"
		txIdx++
	}
	plan.Status = PlanStatusCompleted
	_ = e.RunRepo.Update(ctx, plan)
	return nil
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
