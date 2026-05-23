package service

import "time"

// ============================================================================
// Rotation Metrics — 监控指标统一接口
//
// 所有 rotation 组件（Router / Scheduler / Convergence / Migration / Audit）
// 通过同一个 RotationMetrics 接口上报。生产实现接 Prometheus / OpenTelemetry。
//
// 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §10 + RISK_AUDIT.md §4
//
// 告警阈值（已在 RISK_AUDIT.md 列明）：
//   P0: invariant_violation_total{type=I1|I4|MS} > 0
//   P0: archived_with_nonzero_balance_count > 0
//   P1: scheduler_lock_failure_rate > 50%
//   P1: anchor_status_stuck_count > 0 持续 1h
//   P2: routing_orphan_count > 100
// ============================================================================

// RotationMetrics 上报接口。
type RotationMetrics interface {
	// ===== Router 热路径 =====

	// RouterResolve 记录一次 Router.Resolve 的耗时 + 结果（成功/不同错误类型）。
	RouterResolve(duration time.Duration, result string, isLegacy bool)

	// RouterCacheHit / Miss — logical_account 缓存命中率
	RouterCacheHit()
	RouterCacheMiss()

	// RouterAnchorLookup 路由 anchor 时的 fallback path 计数
	//   path = "self" / "refund-of" / "reverse-of" / "new" / "recovery"
	RouterAnchorLookup(path string)

	// ===== Scheduler =====

	SchedulerTickCompleted(processed, provisioned, activated, errors int)
	SchedulerLockContention() // 锁未拿到时累加
	SchedulerForceSwitch(operator string)
	SchedulerForceProvision(operator string)

	// ===== Convergence Job =====

	ConvergenceTickCompleted(scanned, converged, notReady, errors int)
	ConvergenceInstanceFrozen(logicalAccountID int64)

	// ===== Migration Job =====

	MigrationTickCompleted(scanned, migrated, tooDeep, errors int)
	MigrationAnchorMigrated(anchorID int64, chainDepth int8)

	// ===== Invariant Audit =====

	InvariantViolation(violationType string, severity string, autoHealed bool)
	AuditTickCompleted(violationsTotal, autoHealedTotal int)

	// ===== 业务流量统计 =====

	// FlowAnchored 新 flow 首次锚定（route + anchor 都建立）
	FlowAnchored(logicalAccountID int64)
	// FlowMigrated flow 经过强制迁移
	FlowMigrated(logicalAccountID int64)
}

// NoopMetrics 不上报的实现，用于测试或本地开发。
type NoopMetrics struct{}

func (NoopMetrics) RouterResolve(_ time.Duration, _ string, _ bool)        {}
func (NoopMetrics) RouterCacheHit()                                         {}
func (NoopMetrics) RouterCacheMiss()                                        {}
func (NoopMetrics) RouterAnchorLookup(_ string)                             {}
func (NoopMetrics) SchedulerTickCompleted(_, _, _, _ int)                  {}
func (NoopMetrics) SchedulerLockContention()                               {}
func (NoopMetrics) SchedulerForceSwitch(_ string)                          {}
func (NoopMetrics) SchedulerForceProvision(_ string)                       {}
func (NoopMetrics) ConvergenceTickCompleted(_, _, _, _ int)                {}
func (NoopMetrics) ConvergenceInstanceFrozen(_ int64)                      {}
func (NoopMetrics) MigrationTickCompleted(_, _, _, _ int)                  {}
func (NoopMetrics) MigrationAnchorMigrated(_ int64, _ int8)                {}
func (NoopMetrics) InvariantViolation(_ string, _ string, _ bool)          {}
func (NoopMetrics) AuditTickCompleted(_, _ int)                            {}
func (NoopMetrics) FlowAnchored(_ int64)                                   {}
func (NoopMetrics) FlowMigrated(_ int64)                                   {}

// 推荐 Prometheus 指标名（生产实现可用）：
//
// 计数器（counter）:
//   rotation_router_resolve_total{result, is_legacy}
//   rotation_router_cache_hit_total / rotation_router_cache_miss_total
//   rotation_router_anchor_lookup_total{path}
//   rotation_scheduler_tick_completed_total
//   rotation_scheduler_lock_contention_total
//   rotation_scheduler_force_switch_total{operator}
//   rotation_convergence_instance_frozen_total
//   rotation_migration_anchor_migrated_total{chain_depth}
//   rotation_invariant_violation_total{type, severity, auto_healed}
//   rotation_flow_anchored_total / rotation_flow_migrated_total
//
// 直方图（histogram）:
//   rotation_router_resolve_duration_seconds{result}
//     buckets: [0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5]
//
// Gauge:
//   rotation_archived_with_nonzero_balance_count
//   rotation_anchor_status_stuck_count{logical_account_key}
//   rotation_open_anchor_age_p99_seconds{logical_account_key}
//   rotation_migration_suspense_net_amount
//   rotation_phase_la_current_active_mismatch
