// Package workflow — repo 接口定义（极简版）.
//
// DB-split Batch 7 之后，workflow 包只保留：
//   - translator.go     graph + event → plan（核心翻译逻辑）
//   - repo.go (本文件)  Graph / Event 仓储接口
//   - execute.go        历史注释占位（SP-1 时代 Service 已废弃）
//
// 删除的（原 engine.go 集合）：
//   - Engine.Handle / Kafka subscriber 路径（split-payment 不再做事件订阅）
//   - SagaCoordinator / SagaStep / SagaStore（saga 模式不再用）
//   - TransferRepo / AppFeeRepo / PayoutRepo / ReversalRepo（业务账本归 accounting）
//   - RiskGate / FXClient / EventPublisher（极简版不集成）
//   - PendingHoldsRepo / HoldReleaser / payout cron（hold-period 场景剥离）
package workflow

import (
	"context"

	"github.com/xiongwp/split-payment/internal/domain"
)

// GraphRepo Graph DSL 仓储（meta DB，单表 moneyflow_graphs）。
type GraphRepo interface {
	GetByKey(ctx context.Context, key string) (*domain.Graph, error)
	Save(ctx context.Context, g *domain.Graph) (int64, error)
	List(ctx context.Context, status string) ([]*domain.Graph, error)
	// FindByTrigger 查所有 status=active 且 trigger.event 含此 event 的 graph。
	FindByTrigger(ctx context.Context, event string) ([]*domain.Graph, error)
	// Delete 软删 (admin-web 删 graph 按钮触发，archive 而非物理删).
	Delete(ctx context.Context, key string) error
}

// EventRepo moneyflow_event_NN 事件流水仓储（shard DB，分片 by event_id hash）。
//
// DB-split Batch 7 之后，原 RunRepo 改名为 EventRepo，对应单一 shard 表族
// moneyflow_event_NN（统一 trigger / saga / outbox / 等所有事件类型为同一张表）。
type EventRepo interface {
	// Save 落一条 event 行（每次 TriggerEvent 一条）。
	Save(ctx context.Context, p *domain.RunPlan) (int64, error)
	// Update 改 status / accounting_voucher_no / plan_json 等。
	Update(ctx context.Context, p *domain.RunPlan) error
	// GetByCharge 同一 charge_id 关联的所有 event（多次 trigger / 重试）。
	GetByCharge(ctx context.Context, chargeID string) ([]*domain.RunPlan, error)
	// GetByID 仅在排查时用，全 shard 扫描，性能差。
	GetByID(ctx context.Context, id int64) (*domain.RunPlan, error)
	// Search 跨 shard 按 event_type / time 查询，admin UI 用。
	Search(ctx context.Context, eventLike string, limit int) ([]*domain.RunPlan, error)
	// ListByStatus 跨 shard 按 status 列。
	ListByStatus(ctx context.Context, status string, limit int) ([]*domain.RunPlan, error)
}

// RunRepo 老命名 alias（保持过渡期源码兼容性）。新代码用 EventRepo。
type RunRepo = EventRepo

// （AccountingMetaCaller / AccountingTxResp / AuditClient / AuditEvent 不在 workflow
// 包定义 —— grpcsvc 包自己有，避免重复 + 跨包依赖）
