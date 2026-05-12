// saga.go — 跨服务 saga 编排骨架.
//
// Saga 模式: 多步事务里每步如果失败, 跑前面已成功步骤的 compensating action 回滚.
//
// 一个 saga 流的例子 (split-payment 资金分账):
//   Step 1: 主商户账户扣 \$100      → 失败: 退回 \$100
//   Step 2: 分账商户A 加 \$60       → 失败: 主商户回 \$100 + 直接放弃
//   Step 3: 分账商户B 加 \$30       → 失败: 主商户回 \$100, 商户A 回 \$60
//   Step 4: 平台收入加 \$10         → 失败: ... (前三步全反)
//
// 实现:
//   - 每步是一个 Step{Forward, Backward}; Forward 是 outbox.Event (异步发);
//     Backward 是反向命令事件 (也走 outbox).
//   - SagaInstance 记录每步状态; 监听 saga.{name}.completed / failed.
//   - 协调器 SagaCoordinator 单实例跑, 维护 instance 状态机.
//
// 这里只给类型 + 状态机骨架; 真实 coordinator 由各服务自己实现 (耦合自己的业务事件).

package outbox

import (
	"time"
)

// SagaState 单个 saga instance 的状态机
type SagaState string

const (
	SagaStateStarted     SagaState = "started"
	SagaStateForwarding  SagaState = "forwarding"
	SagaStateCompleted   SagaState = "completed"
	SagaStateCompensating SagaState = "compensating"
	SagaStateFailed      SagaState = "failed"        // compensation 也失败 → 报告 ops
)

// Step 单步定义 (declarative)
type Step struct {
	Name        string
	Forward     Event // 正向命令事件 — publisher 发出, 下游消费
	Backward    Event // 反向命令 (补偿) — 上一步成功后我们这步失败时用
	TimeoutSec  int   // 等下游回应的超时 (秒), 0 = 不超时
}

// SagaDef saga 定义 (静态)
type SagaDef struct {
	Name  string
	Steps []Step
}

// SagaInstance 运行时实例
type SagaInstance struct {
	SagaID       string
	DefName      string
	State        SagaState
	CurrentStep  int
	StartedAt    time.Time
	CompletedAt  time.Time
	StepResults  []StepResult
	CorrelationID string  // 业务关联 ID, eg. order_id
}

// StepResult 每步结果
type StepResult struct {
	StepName    string
	Status      SagaState // forwarding / completed / compensating / failed
	StartedAt   time.Time
	FinishedAt  time.Time
	ErrorMsg    string
}

// 注: 真生产 coordinator 实现思路:
//   1. saga_instance 表 (saga_id PK, def_name, state, current_step, correlation_id, ...)
//   2. saga_step_result 表
//   3. 监听 Kafka step.completed.{step_name} → 推进 current_step + 发下一步
//   4. 监听 Kafka step.failed.{step_name} → state=compensating, 逆序发 compensation
//   5. ops 触发 retry / abandon (admin API)
