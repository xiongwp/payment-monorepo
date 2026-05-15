// saga.go — SP-3A 持久化 saga 编排, fork 自 payment-util/outbox/saga.go,
// 跟 typed Transfer/ApplicationFee/Payout 强耦合.
//
// 核心思想:
//   - RunPlan 被翻译成一组顺序 Step (Translator 产出)
//   - 每 Step 有 Execute (做事) + Compensate (反向)
//   - SagaCoordinator 跑完每一步落 SagaStore (MySQL), 失败逆序补偿前面成功的步骤
//   - 进程重启 ResumeUnfinished() 把 forwarding 状态的 saga 接着跑
//
// 与 payment-util 的区别:
//   - 减依赖: 不用 etcd / otel / prometheus
//   - 强类型: Step 知道是 Transfer / AppFee / Payout 哪种 (而不是泛型 Step)
//   - 不实现 AdvanceStep (异步事件驱动 Phase 4 加)
//
// 状态机:
//
//	started → forwarding → completed
//	  forwarding → compensating → (compensation ok) ended
//	  forwarding → compensating → (compensation fail) failed (人工)
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ─── 状态机常量 ────────────────────────────────────────────────────────

// SagaState saga 总状态.
type SagaState string

const (
	SagaStateStarted      SagaState = "started"
	SagaStateForwarding   SagaState = "forwarding"
	SagaStateCompleted    SagaState = "completed"
	SagaStateCompensating SagaState = "compensating"
	SagaStateFailed       SagaState = "failed"
)

// StepStatus 单步状态.
type StepStatus string

const (
	StepPending     StepStatus = "pending"
	StepRunning     StepStatus = "running"
	StepCompleted   StepStatus = "completed"
	StepCompensated StepStatus = "compensated"
	StepFailed      StepStatus = "failed"
)

// StepKind 步骤类型 — 决定 Execute / Compensate 默认行为.
type StepKind string

const (
	StepKindTransfer       StepKind = "transfer"
	StepKindApplicationFee StepKind = "application_fee"
	StepKindPayout         StepKind = "payout"
	StepKindAccounting     StepKind = "accounting" // 调 accounting-system PostMovements
	StepKindWait           StepKind = "wait"       // 等到指定 time (hold 期)
	StepKindCustom         StepKind = "custom"     // 自定义业务步骤
)

// ─── 数据结构 ──────────────────────────────────────────────────────────

// SagaStep 单步定义.
//
// Execute / Compensate 是闭包函数, 由 Translator 构造时注入业务逻辑;
// SagaStore 只持序列化字段 (Name / Kind / Payload / Status / ErrorMsg),
// 进程重启后用 Kind + Payload 重新组装 Execute (engine 提供 step factory).
type SagaStep struct {
	Name       string          `json:"name"`
	Kind       StepKind        `json:"kind"`
	Payload    json.RawMessage `json:"payload,omitempty"` // 序列化的业务对象 (Transfer / Fee / Payout)
	Status     StepStatus      `json:"status"`
	StartedAt  time.Time       `json:"started_at,omitempty"`
	FinishedAt time.Time       `json:"finished_at,omitempty"`
	ErrorMsg   string          `json:"error_msg,omitempty"`
	TimeoutSec int             `json:"timeout_sec,omitempty"`

	// runtime-only (不序列化, 重启后由 stepFactory 重建)
	Execute    StepFn `json:"-"`
	Compensate StepFn `json:"-"`
}

// StepFn 业务逻辑注入点.
type StepFn func(ctx context.Context) error

// SagaInstance 运行时实例 (持久化到 SagaStore).
type SagaInstance struct {
	SagaID        string     `json:"saga_id"`
	GraphRunID    int64      `json:"graph_run_id"`
	CorrelationID string     `json:"correlation_id"` // charge_id / refund_id
	State         SagaState  `json:"state"`
	CurrentStep   int        `json:"current_step"`
	Steps         []SagaStep `json:"steps"`
	StartedAt     time.Time  `json:"started_at"`
	CompletedAt   time.Time  `json:"completed_at,omitempty"`
}

// SagaStore 状态持久化.
type SagaStore interface {
	Save(ctx context.Context, inst *SagaInstance) error
	Load(ctx context.Context, sagaID string) (*SagaInstance, error)
	ListUnfinished(ctx context.Context, limit int) ([]*SagaInstance, error)
}

// MemorySagaStore 内存实现 (单测 / fallback).
type MemorySagaStore struct {
	mu sync.RWMutex
	m  map[string]*SagaInstance
}

// NewMemorySagaStore.
func NewMemorySagaStore() *MemorySagaStore {
	return &MemorySagaStore{m: map[string]*SagaInstance{}}
}

// Save.
func (s *MemorySagaStore) Save(_ context.Context, inst *SagaInstance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *inst
	c.Steps = append([]SagaStep(nil), inst.Steps...)
	s.m[inst.SagaID] = &c
	return nil
}

// Load.
func (s *MemorySagaStore) Load(_ context.Context, sagaID string) (*SagaInstance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[sagaID]
	if !ok {
		return nil, errors.New("saga not found: " + sagaID)
	}
	c := *v
	return &c, nil
}

// ListUnfinished.
func (s *MemorySagaStore) ListUnfinished(_ context.Context, limit int) ([]*SagaInstance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*SagaInstance, 0, limit)
	for _, v := range s.m {
		if v.State == SagaStateCompleted || v.State == SagaStateFailed {
			continue
		}
		c := *v
		out = append(out, &c)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// StepFactory 进程重启后用 (Kind, Payload) 重新组装 Execute / Compensate.
//
// 实现见 saga_step.go 的 NewDefaultStepFactory().
type StepFactory interface {
	BuildExecute(ctx context.Context, kind StepKind, payload json.RawMessage) (StepFn, error)
	BuildCompensate(ctx context.Context, kind StepKind, payload json.RawMessage) (StepFn, error)
}

// SagaCoordinator 持久化编排器.
type SagaCoordinator struct {
	Store   SagaStore
	Factory StepFactory
	Logger  Logger
}

// Logger 最小日志接口.
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// NopLogger 占位.
type NopLogger struct{}

// Info.
func (NopLogger) Info(string, ...any) {}

// Warn.
func (NopLogger) Warn(string, ...any) {}

// Error.
func (NopLogger) Error(string, ...any) {}

// ─── Coordinator 方法 ──────────────────────────────────────────────────

// Start 启动 saga, 同步跑完.
//
// 出口语义:
//   inst.State == SagaStateCompleted    全部 step 成功
//   inst.State == SagaStateCompensating 中间失败,补偿正在跑
//   inst.State == SagaStateFailed       补偿也失败,需人工介入
func (c *SagaCoordinator) Start(ctx context.Context, inst *SagaInstance) error {
	if c.Logger == nil {
		c.Logger = NopLogger{}
	}
	if inst.SagaID == "" {
		return errors.New("saga: id required")
	}
	if len(inst.Steps) == 0 {
		return errors.New("saga: empty steps")
	}
	inst.State = SagaStateForwarding
	inst.CurrentStep = 0
	inst.StartedAt = time.Now().UTC()
	if err := c.Store.Save(ctx, inst); err != nil {
		return fmt.Errorf("saga save start: %w", err)
	}

	for i := range inst.Steps {
		inst.CurrentStep = i
		step := &inst.Steps[i]
		step.Status = StepRunning
		step.StartedAt = time.Now().UTC()
		_ = c.Store.Save(ctx, inst)

		err := c.runStep(ctx, *step)
		step.FinishedAt = time.Now().UTC()
		if err != nil {
			step.Status = StepFailed
			step.ErrorMsg = err.Error()
			c.Logger.Warn("saga step failed",
				"saga", inst.SagaID, "step", step.Name, "err", err)

			// 触发补偿
			inst.State = SagaStateCompensating
			_ = c.Store.Save(ctx, inst)
			if cerr := c.compensate(ctx, inst, i-1); cerr != nil {
				inst.State = SagaStateFailed
				_ = c.Store.Save(ctx, inst)
				return fmt.Errorf("saga %s failed, compensation also failed: %w",
					inst.SagaID, cerr)
			}
			inst.CompletedAt = time.Now().UTC()
			_ = c.Store.Save(ctx, inst)
			return err
		}
		step.Status = StepCompleted
		_ = c.Store.Save(ctx, inst)
	}

	inst.State = SagaStateCompleted
	inst.CompletedAt = time.Now().UTC()
	_ = c.Store.Save(ctx, inst)
	c.Logger.Info("saga completed", "saga", inst.SagaID, "steps", len(inst.Steps))
	return nil
}

// runStep 跑一步 (含 timeout).
func (c *SagaCoordinator) runStep(ctx context.Context, step SagaStep) error {
	if step.Execute == nil {
		return errors.New("step has no Execute fn: " + step.Name)
	}
	if step.TimeoutSec <= 0 {
		return step.Execute(ctx)
	}
	sCtx, cancel := context.WithTimeout(ctx, time.Duration(step.TimeoutSec)*time.Second)
	defer cancel()
	return step.Execute(sCtx)
}

// compensate 逆序跑 step[from..0] 的 Compensate.
//
// 任一 compensate 失败 → 抛错, saga 进 SagaStateFailed.
// nil Compensate (e.g. AppFee 步骤, hold 期已过) 视为 no-op safe, 跳过.
func (c *SagaCoordinator) compensate(ctx context.Context, inst *SagaInstance, from int) error {
	for i := from; i >= 0; i-- {
		step := &inst.Steps[i]
		if step.Status != StepCompleted {
			continue
		}
		if step.Compensate == nil {
			c.Logger.Info("saga step has no compensator; skip",
				"saga", inst.SagaID, "step", step.Name)
			step.Status = StepCompensated
			continue
		}
		err := c.runStep(ctx, SagaStep{
			Name: step.Name + "_compensate", Execute: step.Compensate,
			TimeoutSec: step.TimeoutSec,
		})
		step.FinishedAt = time.Now().UTC()
		if err != nil {
			step.ErrorMsg = "compensate failed: " + err.Error()
			c.Logger.Error("saga compensation failed; needs manual intervention",
				"saga", inst.SagaID, "step", step.Name, "err", err)
			return fmt.Errorf("compensate step %d (%s): %w", i, step.Name, err)
		}
		step.Status = StepCompensated
		_ = c.Store.Save(ctx, inst)
	}
	return nil
}

// ResumeUnfinished 进程重启后扫一遍未完成的 saga, 从 CurrentStep 起重跑.
//
// 重跑要求 Step.Execute 必须幂等 (Transfer/AppFee/Payout 都有 idempotency_key,
// 重复 Insert ON DUPLICATE KEY UPDATE 自动幂等).
func (c *SagaCoordinator) ResumeUnfinished(ctx context.Context, limit int) (int, error) {
	pending, err := c.Store.ListUnfinished(ctx, limit)
	if err != nil {
		return 0, err
	}
	resumed := 0
	for _, inst := range pending {
		if inst.State != SagaStateForwarding {
			continue
		}
		// 重建 Execute/Compensate fns
		for i := range inst.Steps {
			step := &inst.Steps[i]
			if step.Execute == nil && c.Factory != nil {
				step.Execute, _ = c.Factory.BuildExecute(ctx, step.Kind, step.Payload)
			}
			if step.Compensate == nil && c.Factory != nil {
				step.Compensate, _ = c.Factory.BuildCompensate(ctx, step.Kind, step.Payload)
			}
		}
		c.Logger.Info("resuming saga", "saga", inst.SagaID, "from_step", inst.CurrentStep)
		if err := c.Start(ctx, inst); err != nil {
			c.Logger.Warn("resume failed", "saga", inst.SagaID, "err", err)
		}
		resumed++
	}
	return resumed, nil
}
