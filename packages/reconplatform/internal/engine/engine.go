package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"reconcile-system/internal/metrics"
	"reconcile-system/internal/model"
	"reconcile-system/internal/rule"
	"reconcile-system/internal/store"
)

type State struct {
	Order   *model.Event `json:"order,omitempty"`
	Payment *model.Event `json:"payment,omitempty"`
	Updated int64        `json:"updated"`
}

type Engine struct {
	store *store.Store
	rule  *rule.Engine
	out   chan model.Result
}

func New(store *store.Store, rule *rule.Engine) *Engine {
	return &Engine{
		store: store,
		rule:  rule,
		out:   make(chan model.Result, 100000),
	}
}

// Handle 资损修复：
//   1. 用 store.UpsertEvent 原子 merge 事件到 state（Lua 单线程读改写）。
//      之前 Get → mutate → Set 两步式 read-modify-write race 在两个 goroutine
//      处理同 OrderID 的 ORDER 和 PAYMENT 时会丢一个事件 → reconcile 永远
//      不触发 → 资金缺漏没人发现。
//   2. UpsertEvent 用 stateTTL=1h（store 定义），之前传 int(60) 给 redis-go
//      被当 60 纳秒 → key 立即过期 → state 永远不持久 → 同样 reconcile 不触发。
func (e *Engine) Handle(ctx context.Context, ev model.Event) {
	if ev.OrderID == "" {
		return
	}
	evBytes, err := json.Marshal(ev)
	if err != nil {
		log.Printf("recon: marshal event failed order_id=%s err=%v", ev.OrderID, err)
		return
	}
	stateJSON, err := e.store.UpsertEvent(ctx, ev.OrderID, evBytes)
	if err != nil {
		log.Printf("recon: upsert state failed order_id=%s err=%v", ev.OrderID, err)
		return
	}
	if stateJSON == "" {
		return
	}
	var st State
	if err := json.Unmarshal([]byte(stateJSON), &st); err != nil {
		log.Printf("recon: unmarshal state failed order_id=%s err=%v", ev.OrderID, err)
		return
	}
	if st.Order == nil || st.Payment == nil {
		return
	}

	env := map[string]interface{}{
		"order":   map[string]interface{}{"amount": st.Order.Amount},
		"payment": map[string]interface{}{"amount": st.Payment.Amount},
	}

	// 记录 reconciliation run 和检测时间
	startTime := time.Now()
	metrics.RunTotal.Inc()

	results := e.rule.EvalAll(env)

	// 检查是否存在差异异常（amount mismatch）
	if st.Order.Amount != st.Payment.Amount {
		diffAmount := st.Order.Amount - st.Payment.Amount
		if diffAmount < 0 {
			diffAmount = -diffAmount
		}

		// 确定严重程度：单笔 > 100,000 是 critical，否则 warning
		severity := "warning"
		if diffAmount > 100000 {
			severity = "critical"
		}

		// 记录 exception
		metrics.ExceptionTotal.WithLabelValues("amount_mismatch", severity).Inc()
		metrics.ExceptionPending.Inc()
		metrics.DiffAmountMinorTotal.WithLabelValues("amount_mismatch").Add(float64(diffAmount))

		log.Printf("recon: amount exception detected order_id=%s order=%d payment=%d diff=%d severity=%s",
			ev.OrderID, st.Order.Amount, st.Payment.Amount, diffAmount, severity)
	}

	for ruleName, status := range results {
		select {
		case e.out <- model.Result{OrderID: ev.OrderID, Rule: ruleName, Status: status}:
		default:
			// out chan 满了（10w buffer 还堵 → 下游 producer 卡住）。丢弃保护
			// 上游 Kafka consumer 不被反压打死；落 log 让 SRE 看到真实问题。
			log.Printf("recon: result channel full, dropped order_id=%s rule=%s status=%s",
				ev.OrderID, ruleName, status)
		}
	}

	// 记录 reconciliation 耗时
	duration := time.Since(startTime).Seconds()
	metrics.RunDurationSeconds.Observe(duration)
	// reconcile 完成（match/mismatch 都算完成）→ Del 释放 state。删除失败只记日志，
	// TTL 1h 兜底让 stale state 自然过期。
	e.store.Del(ctx, ev.OrderID)
}

// HandleLegacy 老接口：吞 Background context，给暂未升级 ctx 的 caller 兼容。
func (e *Engine) HandleLegacy(ev model.Event) {
	e.Handle(context.Background(), ev)
}

func (e *Engine) Output() <-chan model.Result {
	return e.out
}

// IsNotFound 暴露给上层：state 不存在不是 error
func IsNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
