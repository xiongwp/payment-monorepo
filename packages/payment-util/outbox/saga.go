// saga.go — 跨服务 saga 编排 (类型 + Coordinator 实现).
//
// Saga 模式: 多步事务里每步如果失败, 跑前面已成功步骤的 compensating action 回滚.
//
// 一个 saga 流的例子 (split-payment 资金分账):
//   Step 1: 主商户账户扣 \$100      → 失败: 退回 \$100
//   Step 2: 分账商户A 加 \$60       → 失败: 主商户回 \$100 + 直接放弃
//   Step 3: 分账商户B 加 \$30       → 失败: 主商户回 \$100, 商户A 回 \$60
//   Step 4: 平台收入加 \$10         → 失败: ... (前三步全反)
//
// 实现策略:
//   - SagaCoordinator 是单一可调用对象, 不再要求"各服务自己实现"
//   - Step.Execute / Step.Compensate 通过函数指针注入业务逻辑 (单元可测)
//   - 状态持久化通过 SagaStore 抽象 (本包提供 MemorySagaStore 用于测试,
//     生产由调用方注入 SQL/Redis 实现)
//   - 监听通过 Coordinator.Run(ctx) 同步推进; 也可由调用方放进自己的
//     event consumer 循环里, 收到 step.completed Kafka 消息后调 AdvanceStep().

package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SagaState 单个 saga instance 的状态机
type SagaState string

const (
	SagaStateStarted      SagaState = "started"
	SagaStateForwarding   SagaState = "forwarding"
	SagaStateCompleted    SagaState = "completed"
	SagaStateCompensating SagaState = "compensating"
	SagaStateFailed       SagaState = "failed" // compensation 也失败 → 报告 ops
)

// StepStatus 单步状态
type StepStatus string

const (
	StepPending     StepStatus = "pending"
	StepRunning     StepStatus = "running"
	StepCompleted   StepStatus = "completed"
	StepCompensated StepStatus = "compensated"
	StepFailed      StepStatus = "failed"
)

// StepFn / CompensateFn 业务逻辑注入点.
// 返回 error → 该步失败 (触发逆序补偿).
type StepFn func(ctx context.Context, payload map[string]any) error

// Step 单步定义.
//
// Forward/Backward 仍保留 (向后兼容 Kafka 异步模式), 但同步模式只用 Execute/Compensate.
type Step struct {
	Name       string
	Execute    StepFn
	Compensate StepFn // nil 表示该步不可补偿 (副作用安全步骤,如读)
	TimeoutSec int    // 0 = 不超时
	// 兼容旧 fields (Kafka publisher 风格)
	Forward  Event
	Backward Event
}

// SagaDef saga 定义 (静态)
type SagaDef struct {
	Name  string
	Steps []Step
}

// SagaInstance 运行时实例
type SagaInstance struct {
	SagaID        string
	DefName       string
	State         SagaState
	CurrentStep   int
	StartedAt     time.Time
	CompletedAt   time.Time
	StepResults   []StepResult
	CorrelationID string         // 业务关联 ID, eg. order_id
	Payload       map[string]any // 跨 step 共享的载荷
}

// StepResult 每步结果
type StepResult struct {
	StepName   string
	Status     StepStatus
	StartedAt  time.Time
	FinishedAt time.Time
	ErrorMsg   string
}

// SagaStore 状态持久化抽象 (生产由调用方注入 SQL / Redis 实现)
type SagaStore interface {
	Save(ctx context.Context, inst *SagaInstance) error
	Load(ctx context.Context, sagaID string) (*SagaInstance, error)
	ListUnfinished(ctx context.Context, limit int) ([]*SagaInstance, error)
}

// MemorySagaStore 内存实现 (单测用)
type MemorySagaStore struct {
	mu sync.RWMutex
	m  map[string]*SagaInstance
}

// NewMemorySagaStore returns a new store.
func NewMemorySagaStore() *MemorySagaStore {
	return &MemorySagaStore{m: map[string]*SagaInstance{}}
}

func (s *MemorySagaStore) Save(_ context.Context, inst *SagaInstance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// deep-copy step results to avoid aliasing in tests
	c := *inst
	c.StepResults = append([]StepResult(nil), inst.StepResults...)
	s.m[inst.SagaID] = &c
	return nil
}

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

// SagaCoordinator 真实编排器.
//
// 使用方式 (同步):
//
//	def := &SagaDef{Name: "refund", Steps: []Step{ {"debit_merchant", debitFn, debitCompFn, 5, ...}, ... }}
//	co := NewSagaCoordinator(def, store)
//	inst, err := co.Start(ctx, "saga_"+orderID, orderID, payload)
//	// 失败时 inst.State == SagaStateFailed; ops 看 inst.StepResults
//
// 使用方式 (异步, 由 Kafka consumer 调):
//
//	co.AdvanceStep(ctx, sagaID, stepIndex, nil)         // 报告完成
//	co.AdvanceStep(ctx, sagaID, stepIndex, errors.New(.."tx_failed")) // 报告失败 → 触发补偿
type SagaCoordinator struct {
	def    *SagaDef
	store  SagaStore
	logger Logger
}

// Logger 最小日志接口 (避免引入 zap 让 payment-util 减少依赖深度)
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
}

type nopLogger struct{}

func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

// NewSagaCoordinator initializes a coordinator.
func NewSagaCoordinator(def *SagaDef, store SagaStore, logger Logger) *SagaCoordinator {
	if logger == nil {
		logger = nopLogger{}
	}
	return &SagaCoordinator{def: def, store: store, logger: logger}
}

// Start 启动一个 saga, 同步跑完 (适合 RPC handler 内调用).
//
// 出口语义:
//
//	inst.State == SagaStateCompleted    全部 step 成功
//	inst.State == SagaStateCompensating 中间失败,补偿正在跑
//	inst.State == SagaStateFailed       补偿也失败,需人工介入
func (c *SagaCoordinator) Start(
	ctx context.Context,
	sagaID, correlationID string,
	payload map[string]any,
) (*SagaInstance, error) {
	if c.def == nil || len(c.def.Steps) == 0 {
		return nil, errors.New("saga: empty definition")
	}
	if sagaID == "" {
		return nil, errors.New("saga: id required")
	}
	inst := &SagaInstance{
		SagaID:        sagaID,
		DefName:       c.def.Name,
		State:         SagaStateStarted,
		CurrentStep:   0,
		StartedAt:     time.Now().UTC(),
		StepResults:   make([]StepResult, len(c.def.Steps)),
		CorrelationID: correlationID,
		Payload:       payload,
	}
	if payload == nil {
		inst.Payload = map[string]any{}
	}
	for i, s := range c.def.Steps {
		inst.StepResults[i] = StepResult{StepName: s.Name, Status: StepPending}
	}
	if err := c.store.Save(ctx, inst); err != nil {
		return nil, fmt.Errorf("saga save start: %w", err)
	}

	inst.State = SagaStateForwarding
	_ = c.store.Save(ctx, inst)

	for i, step := range c.def.Steps {
		inst.CurrentStep = i
		inst.StepResults[i].Status = StepRunning
		inst.StepResults[i].StartedAt = time.Now().UTC()
		_ = c.store.Save(ctx, inst)

		err := c.runWithTimeout(ctx, step, inst.Payload)
		inst.StepResults[i].FinishedAt = time.Now().UTC()
		if err != nil {
			inst.StepResults[i].Status = StepFailed
			inst.StepResults[i].ErrorMsg = err.Error()
			c.logger.Warn("saga step failed",
				"saga", sagaID, "step", step.Name, "err", err)

			// 触发逆序补偿
			if compErr := c.compensate(ctx, inst, i-1); compErr != nil {
				inst.State = SagaStateFailed
				_ = c.store.Save(ctx, inst)
				return inst, fmt.Errorf("saga %s failed, compensation also failed: %w", sagaID, compErr)
			}
			inst.State = SagaStateCompensating
			inst.CompletedAt = time.Now().UTC()
			_ = c.store.Save(ctx, inst)
			return inst, err
		}
		inst.StepResults[i].Status = StepCompleted
		_ = c.store.Save(ctx, inst)
	}

	inst.State = SagaStateCompleted
	inst.CompletedAt = time.Now().UTC()
	_ = c.store.Save(ctx, inst)
	c.logger.Info("saga completed", "saga", sagaID, "steps", len(c.def.Steps))
	return inst, nil
}

// runWithTimeout 跑一步 (含 timeout)
func (c *SagaCoordinator) runWithTimeout(
	ctx context.Context, step Step, payload map[string]any,
) error {
	if step.Execute == nil {
		return errors.New("step has no Execute fn: " + step.Name)
	}
	if step.TimeoutSec <= 0 {
		return step.Execute(ctx, payload)
	}
	stepCtx, cancel := context.WithTimeout(ctx, time.Duration(step.TimeoutSec)*time.Second)
	defer cancel()
	return step.Execute(stepCtx, payload)
}

// compensate 逆序跑 step[from..0] 的 Compensate.
//
// 任一 compensate 失败 → 抛错, saga 进 SagaStateFailed.
// nil Compensate 视为 "no-op safe", 跳过.
func (c *SagaCoordinator) compensate(
	ctx context.Context, inst *SagaInstance, from int,
) error {
	for i := from; i >= 0; i-- {
		step := c.def.Steps[i]
		if step.Compensate == nil {
			c.logger.Info("saga step has no compensator; skip",
				"saga", inst.SagaID, "step", step.Name)
			continue
		}
		inst.StepResults[i].Status = StepCompensated
		started := time.Now().UTC()
		err := c.runWithTimeout(ctx, Step{Execute: step.Compensate, TimeoutSec: step.TimeoutSec}, inst.Payload)
		inst.StepResults[i].FinishedAt = time.Now().UTC()
		if err != nil {
			inst.StepResults[i].ErrorMsg = "compensate failed: " + err.Error()
			c.logger.Error("saga compensation failed; needs manual intervention",
				"saga", inst.SagaID, "step", step.Name,
				"compensate_started", started.Format(time.RFC3339),
				"err", err)
			return fmt.Errorf("compensate step %d (%s): %w", i, step.Name, err)
		}
		_ = c.store.Save(ctx, inst)
	}
	return nil
}

// AdvanceStep 异步推进 (Kafka consumer 调用,报告某 step 完成 / 失败).
//
// 这是给 "Forward/Backward = Event" 的事件驱动模式准备的: Coordinator 持有
// SagaInstance, 各服务跑完自己的 step 后发个 step.completed.{name} 消息,
// 上层 consumer 解析后调本方法把状态机往前推一格,或触发补偿.
func (c *SagaCoordinator) AdvanceStep(
	ctx context.Context, sagaID string, stepIdx int, stepErr error,
) error {
	inst, err := c.store.Load(ctx, sagaID)
	if err != nil {
		return err
	}
	if stepIdx != inst.CurrentStep {
		return fmt.Errorf("saga %s: step index mismatch want=%d got=%d",
			sagaID, inst.CurrentStep, stepIdx)
	}
	if stepErr != nil {
		inst.StepResults[stepIdx].Status = StepFailed
		inst.StepResults[stepIdx].ErrorMsg = stepErr.Error()
		inst.StepResults[stepIdx].FinishedAt = time.Now().UTC()
		if compErr := c.compensate(ctx, inst, stepIdx-1); compErr != nil {
			inst.State = SagaStateFailed
			_ = c.store.Save(ctx, inst)
			return compErr
		}
		inst.State = SagaStateCompensating
		_ = c.store.Save(ctx, inst)
		return stepErr
	}
	inst.StepResults[stepIdx].Status = StepCompleted
	inst.StepResults[stepIdx].FinishedAt = time.Now().UTC()
	inst.CurrentStep++
	if inst.CurrentStep >= len(c.def.Steps) {
		inst.State = SagaStateCompleted
		inst.CompletedAt = time.Now().UTC()
	}
	return c.store.Save(ctx, inst)
}

// ResumeUnfinished 进程重启后扫一遍未完成的 saga,
// 对 SagaStateForwarding 的实例重新跑(从 CurrentStep 起).
//
// 注意: 这要求 Step.Execute 必须幂等 (业务方负责)。
func (c *SagaCoordinator) ResumeUnfinished(ctx context.Context, limit int) (int, error) {
	pending, err := c.store.ListUnfinished(ctx, limit)
	if err != nil {
		return 0, err
	}
	resumed := 0
	for _, inst := range pending {
		if inst.State != SagaStateForwarding {
			continue
		}
		c.logger.Info("resuming saga", "saga", inst.SagaID, "from_step", inst.CurrentStep)
		for i := inst.CurrentStep; i < len(c.def.Steps); i++ {
			step := c.def.Steps[i]
			if err := c.runWithTimeout(ctx, step, inst.Payload); err != nil {
				if compErr := c.compensate(ctx, inst, i-1); compErr != nil {
					inst.State = SagaStateFailed
				} else {
					inst.State = SagaStateCompensating
				}
				_ = c.store.Save(ctx, inst)
				break
			}
			inst.StepResults[i].Status = StepCompleted
			inst.CurrentStep = i + 1
		}
		if inst.State == SagaStateForwarding {
			inst.State = SagaStateCompleted
			inst.CompletedAt = time.Now().UTC()
			_ = c.store.Save(ctx, inst)
		}
		resumed++
	}
	return resumed, nil
}
