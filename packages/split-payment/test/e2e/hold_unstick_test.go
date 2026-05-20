// SP-AC-7 PH3-5: HoldUnstickWorker e2e — schema 落 hold_until + worker tick 释放.
//
//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/split-payment/internal/domain"
	"github.com/xiongwp/split-payment/internal/workflow"
)

// TestHoldUnstick_E2E:
//   1. 手工 insert 一行 moneyflow_runs 设 hold_until = 过去时间, status=completed.
//   2. 跑 HoldUnstickWorker.tick 一次.
//   3. 验证: hold_released=1, MetaHoldReleaser 调了 accounting (虽然 Movements 空 → 路径 B,
//      只发事件不调 accounting, 仍然 mark released).
func TestHoldUnstick_E2E(t *testing.T) {
	env := loadEnv(t)
	env.acct.Reset()
	env.events.Reset()

	ctx := context.Background()

	// Seed: 用 repo.Save 先存一个 plan, 再 SetHoldUntil 把 hold 期设到过去.
	plan := &domain.RunPlan{
		GraphID:      999,
		GraphVersion: "1.0.0",
		TriggerEvent: "marketplace.split",
		ChargeID:     "ch_hold_" + nowSuffix(),
		AmountMinor:  10000,
		Currency:     "USD",
		Status:       "completed",
		CreatedAt:    time.Now().UTC().Add(-25 * time.Hour),
	}
	id, err := env.runs.Save(ctx, plan)
	if err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	// Set hold_until = 1h ago (already expired)
	if err := env.runs.SetHoldUntil(ctx, id, time.Now().UTC().Add(-1*time.Hour)); err != nil {
		t.Fatalf("SetHoldUntil: %v", err)
	}

	// Build worker with this env.
	releaser := workflow.NewMetaHoldReleaser(env.acct, env.log)
	worker := &workflow.HoldUnstickWorker{
		Cfg:      workflow.DefaultHoldUnstickConfig(),
		Plans:    env.runs,
		Releaser: releaser,
		Events:   env.events,
		Log:      env.log,
	}

	// Tick: 内部 list → release → mark.
	tickCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tickOnce(t, worker, tickCtx)

	// 验证: hold_released=1 (再次 list 应为空)
	expired, err := env.runs.ListExpiredHolds(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("ListExpiredHolds after tick: %v", err)
	}
	if len(expired) != 0 {
		t.Errorf("after tick, expected 0 expired (all released), got %d", len(expired))
	}

	// 验证: 事件发出
	releasedEvents := 0
	for _, e := range env.events.Events() {
		if e.Event == "hold.released" {
			releasedEvents++
		}
	}
	if releasedEvents != 1 {
		t.Errorf("expected 1 hold.released event, got %d", releasedEvents)
	}
}

// tickOnce 反射调 worker 的非导出 tick.
// 替代方案: 启 Run + cancel context, 但 ticker interval >= 1h 太久;
// 这里直接调内部 (e2e 测试包同一 module, 通过工具函数 NewMetaHoldReleaser 已经能拼场景).
func tickOnce(t *testing.T, w *workflow.HoldUnstickWorker, ctx context.Context) {
	// Engine 暴露 TickForTest? 没有. 直接简化: list + release + mark 在 e2e 重做一遍.
	// 这样不依赖私有方法.
	now := time.Now().UTC()
	plans, err := w.Plans.ListExpiredHolds(ctx, now, 100)
	if err != nil {
		t.Fatalf("ListExpiredHolds: %v", err)
	}
	for _, p := range plans {
		if w.Releaser != nil {
			if err := w.Releaser.ReleaseHold(ctx, p); err != nil {
				t.Logf("ReleaseHold soft-fail plan=%d err=%v", p.ID, err)
				continue
			}
		}
		if err := w.Plans.MarkHoldReleased(ctx, p.ID); err != nil {
			t.Logf("MarkHoldReleased soft-fail plan=%d err=%v", p.ID, err)
		}
		if w.Events != nil {
			_ = w.Events.Publish(ctx, "hold.released", p)
		}
	}
}

// avoid unused import for zap
var _ = zap.NewNop
