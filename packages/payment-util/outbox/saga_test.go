package outbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// 示例: 一个 3 步的退款 saga (debit_merchant → credit_user → record_event)
func newRefundSagaDef(
	debitCalls, debitCompCalls *int32,
	creditCalls, creditCompCalls *int32,
	recordCalls *int32,
	failAt string,
) *SagaDef {
	mkFn := func(name string, counter *int32) StepFn {
		return func(ctx context.Context, payload map[string]any) error {
			atomic.AddInt32(counter, 1)
			if failAt == name {
				return errors.New(name + " failed")
			}
			return nil
		}
	}
	return &SagaDef{
		Name: "refund",
		Steps: []Step{
			{Name: "debit_merchant", Execute: mkFn("debit_merchant", debitCalls),
				Compensate: mkFn("debit_merchant_comp", debitCompCalls)},
			{Name: "credit_user", Execute: mkFn("credit_user", creditCalls),
				Compensate: mkFn("credit_user_comp", creditCompCalls)},
			{Name: "record_event", Execute: mkFn("record_event", recordCalls),
				Compensate: nil}, // 写日志步骤不补偿
		},
	}
}

func TestSaga_HappyPath(t *testing.T) {
	var dC, dCC, cC, cCC, rC int32
	def := newRefundSagaDef(&dC, &dCC, &cC, &cCC, &rC, "")
	co := NewSagaCoordinator(def, NewMemorySagaStore(), nil)

	inst, err := co.Start(context.Background(), "saga_1", "order_1", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if inst.State != SagaStateCompleted {
		t.Errorf("want completed, got %s", inst.State)
	}
	if dC != 1 || cC != 1 || rC != 1 {
		t.Errorf("forward counts wrong: debit=%d credit=%d record=%d", dC, cC, rC)
	}
	if dCC != 0 || cCC != 0 {
		t.Errorf("no compensations expected, got debit_comp=%d credit_comp=%d", dCC, cCC)
	}
}

func TestSaga_FailMidway_TriggersReverseCompensation(t *testing.T) {
	var dC, dCC, cC, cCC, rC int32
	def := newRefundSagaDef(&dC, &dCC, &cC, &cCC, &rC, "credit_user")
	co := NewSagaCoordinator(def, NewMemorySagaStore(), nil)

	inst, err := co.Start(context.Background(), "saga_2", "order_2", nil)
	if err == nil {
		t.Fatal("expect error")
	}
	if inst.State != SagaStateCompensating {
		t.Errorf("want compensating, got %s", inst.State)
	}
	if dC != 1 || cC != 1 {
		t.Errorf("forward: debit=%d credit=%d", dC, cC)
	}
	if dCC != 1 {
		t.Errorf("expect 1 debit compensation, got %d", dCC)
	}
	// credit_user 自身失败,本步不补偿 (status=Failed); record_event 没执行
	if rC != 0 {
		t.Errorf("record_event should not run, got %d", rC)
	}
}

func TestSaga_FailAtFirstStep_NoCompensationNeeded(t *testing.T) {
	var dC, dCC, cC, cCC, rC int32
	def := newRefundSagaDef(&dC, &dCC, &cC, &cCC, &rC, "debit_merchant")
	co := NewSagaCoordinator(def, NewMemorySagaStore(), nil)

	inst, err := co.Start(context.Background(), "saga_3", "order_3", nil)
	if err == nil {
		t.Fatal("expect error")
	}
	if inst.State != SagaStateCompensating {
		t.Errorf("want compensating, got %s", inst.State)
	}
	if dCC != 0 || cCC != 0 {
		t.Errorf("no compensation expected (failed at step 0), got %d / %d", dCC, cCC)
	}
}

func TestSaga_AdvanceStep_AsyncMode(t *testing.T) {
	var c1, c2 int32
	def := &SagaDef{
		Name: "async",
		Steps: []Step{
			{Name: "step1", Execute: func(_ context.Context, _ map[string]any) error {
				atomic.AddInt32(&c1, 1)
				return nil
			}},
			{Name: "step2", Execute: func(_ context.Context, _ map[string]any) error {
				atomic.AddInt32(&c2, 1)
				return nil
			}},
		},
	}
	store := NewMemorySagaStore()
	co := NewSagaCoordinator(def, store, nil)

	// 预先放入一个 forwarding 实例
	inst := &SagaInstance{
		SagaID: "s1", DefName: "async", State: SagaStateForwarding,
		CurrentStep: 0, StepResults: []StepResult{
			{StepName: "step1", Status: StepRunning},
			{StepName: "step2", Status: StepPending},
		},
	}
	_ = store.Save(context.Background(), inst)

	// step1 完成
	if err := co.AdvanceStep(context.Background(), "s1", 0, nil); err != nil {
		t.Fatalf("Advance step1: %v", err)
	}
	// step2 完成
	if err := co.AdvanceStep(context.Background(), "s1", 1, nil); err != nil {
		t.Fatalf("Advance step2: %v", err)
	}
	loaded, _ := store.Load(context.Background(), "s1")
	if loaded.State != SagaStateCompleted {
		t.Errorf("want completed, got %s", loaded.State)
	}
}

func TestSaga_AdvanceStep_FailsTriggerCompensation(t *testing.T) {
	var comp1 int32
	def := &SagaDef{
		Name: "async-fail",
		Steps: []Step{
			{Name: "step1", Execute: func(_ context.Context, _ map[string]any) error { return nil },
				Compensate: func(_ context.Context, _ map[string]any) error {
					atomic.AddInt32(&comp1, 1)
					return nil
				}},
			{Name: "step2", Execute: func(_ context.Context, _ map[string]any) error { return nil }},
		},
	}
	store := NewMemorySagaStore()
	co := NewSagaCoordinator(def, store, nil)

	inst := &SagaInstance{
		SagaID: "s2", State: SagaStateForwarding,
		CurrentStep: 1, // step1 已完成
		StepResults: []StepResult{
			{StepName: "step1", Status: StepCompleted},
			{StepName: "step2", Status: StepRunning},
		},
	}
	_ = store.Save(context.Background(), inst)

	// step2 失败
	err := co.AdvanceStep(context.Background(), "s2", 1, errors.New("step2 failed"))
	if err == nil {
		t.Fatal("expect step2 error")
	}
	if comp1 != 1 {
		t.Errorf("expect step1 to be compensated once, got %d", comp1)
	}
}

func TestSaga_CompensationFailure_FailedState(t *testing.T) {
	def := &SagaDef{
		Name: "comp-broken",
		Steps: []Step{
			{Name: "step1",
				Execute:    func(_ context.Context, _ map[string]any) error { return nil },
				Compensate: func(_ context.Context, _ map[string]any) error { return errors.New("comp broken") }},
			{Name: "step2",
				Execute: func(_ context.Context, _ map[string]any) error { return errors.New("step2 fail") }},
		},
	}
	co := NewSagaCoordinator(def, NewMemorySagaStore(), nil)
	inst, err := co.Start(context.Background(), "s3", "order_3", nil)
	if err == nil {
		t.Fatal("expect error")
	}
	if inst.State != SagaStateFailed {
		t.Errorf("want failed (compensation broken), got %s", inst.State)
	}
}

func TestSaga_EmptyDefRejected(t *testing.T) {
	co := NewSagaCoordinator(&SagaDef{Name: "empty"}, NewMemorySagaStore(), nil)
	_, err := co.Start(context.Background(), "x", "y", nil)
	if err == nil {
		t.Fatal("expect error for empty def")
	}
}

func TestSaga_ResumeUnfinished(t *testing.T) {
	var c1, c2 int32
	def := &SagaDef{
		Name: "resume",
		Steps: []Step{
			{Name: "s1", Execute: func(_ context.Context, _ map[string]any) error {
				atomic.AddInt32(&c1, 1)
				return nil
			}},
			{Name: "s2", Execute: func(_ context.Context, _ map[string]any) error {
				atomic.AddInt32(&c2, 1)
				return nil
			}},
		},
	}
	store := NewMemorySagaStore()
	co := NewSagaCoordinator(def, store, nil)

	// 一个 forwarding 残留实例,卡在 step 1
	inst := &SagaInstance{
		SagaID: "res1", State: SagaStateForwarding, CurrentStep: 1,
		StepResults: []StepResult{
			{StepName: "s1", Status: StepCompleted},
			{StepName: "s2", Status: StepPending},
		},
	}
	_ = store.Save(context.Background(), inst)

	n, err := co.ResumeUnfinished(context.Background(), 10)
	if err != nil || n != 1 {
		t.Errorf("Resume: n=%d err=%v", n, err)
	}
	if c1 != 0 || c2 != 1 {
		t.Errorf("expect only s2 rerun, got c1=%d c2=%d", c1, c2)
	}
	loaded, _ := store.Load(context.Background(), "res1")
	if loaded.State != SagaStateCompleted {
		t.Errorf("want completed, got %s", loaded.State)
	}
}
