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
type Engine struct {
	GraphRepo  GraphRepo
	RunRepo    RunRepo
	Accounting *clients.AccountingClient
	Audit      AuditClient
	Log        *zap.Logger
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
	plan.Status = "created"
	plan.CreatedAt = time.Now().UTC()
	id, err := e.RunRepo.Save(ctx, plan)
	if err != nil {
		return fmt.Errorf("save plan: %w", err)
	}
	plan.ID = id

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
