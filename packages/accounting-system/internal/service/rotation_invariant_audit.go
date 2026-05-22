package service

import (
	"context"
	"fmt"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// Invariant Audit Job — 资金不变量自检
//
// 定期巡检所有不变量是否成立，违反则告警并尝试自愈：
//   I1: 同 logical 下至多 1 个 active instance
//   I3: archived instance balance ≠ 0（异常）
//   I-LA: LA.current_active_account_no 与实际 phase=active 的 instance 一致
//   I-Chain: anchor.migration_chain_depth > 5 → 应当 quarantined
//
// 历史 I-MS（MigrationSuspense 净额=0）已删除：MigrationSuspense business_type 不再
// 占用 registry 名额，相关 migration 流程若需过渡科目应在代码内部用专用编码处理。
//
// 周期：每小时跑一次（频率低但全面）
//
// 处理策略：
//   - 不变量违反 → 写入 violations，记录到监控（生产实现接 metrics）
//   - 部分违反可自愈（如 LA 不一致），部分必须人工（如双 active）
//
// 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §10
// ============================================================================

// InvariantViolationType 违反类型。
type InvariantViolationType string

const (
	ViolationI1MultipleActive         InvariantViolationType = "I1_MULTIPLE_ACTIVE"
	ViolationI4ArchivedNonZeroBalance InvariantViolationType = "I4_ARCHIVED_NONZERO"
	ViolationLAActiveMismatch         InvariantViolationType = "LA_ACTIVE_MISMATCH"
	ViolationChainDepthExceeded       InvariantViolationType = "CHAIN_DEPTH_EXCEEDED"
	// ViolationMigrationSuspenseNonZero 已删除（MigrationSuspense business_type 移除）
)

// InvariantViolation 单次违反记录。
type InvariantViolation struct {
	Type              InvariantViolationType `json:"type"`
	LogicalAccountID  int64                  `json:"logical_account_id,omitempty"`
	LogicalAccountKey string                 `json:"logical_account_key,omitempty"`
	AccountNo         string                 `json:"account_no,omitempty"`
	AnchorID          int64                  `json:"anchor_id,omitempty"`
	Detail            string                 `json:"detail"`
	Severity          string                 `json:"severity"` // P0 / P1 / P2
	DetectedAt        time.Time              `json:"detected_at"`
	AutoHealed        bool                   `json:"auto_healed"`
}

// InvariantAuditReader 巡检需要的查询能力。
type InvariantAuditReader interface {
	// ListAllRotatingLAs 列出所有 rotation_enabled=1 的 LA。
	ListAllRotatingLAs(ctx context.Context, limit int) ([]*model.LogicalAccount, error)

	// CountInstancesByPhase 计数 logical 下指定 phase 的 instance 数。
	CountInstancesByPhase(ctx context.Context, logicalAccountID int64, phase model.LifecyclePhase) (int, error)

	// GetActiveAccountNoForLogical 返回实际 phase=active 的 instance.account_no（可能多个 → 取第一个 + 数量）。
	GetActiveAccountNoForLogical(ctx context.Context, logicalAccountID int64) (accountNo string, count int, err error)

	// ListArchivedNonZeroBalance 找所有 phase=archived 且 balance≠0 的 instance。
	ListArchivedNonZeroBalance(ctx context.Context, limit int) ([]*model.Account, error)

	// SumMigrationSuspenseBalance 已删除（MigrationSuspense business_type 移除）

	// ListAnchorsWithChainDepthExceeded 找 migration_chain_depth > maxDepth 的 anchor。
	ListAnchorsWithChainDepthExceeded(ctx context.Context, maxDepth int8, limit int) ([]*model.TxAccountAnchor, error)
}

// AuditHealer 自愈接口：处理可自动修复的违反。
type AuditHealer interface {
	// RealignLogicalActive 修正 LA.current_active_account_no 为实际 phase=active 的 instance。
	RealignLogicalActive(ctx context.Context, logicalAccountID int64, correctAccountNo string) error

	// QuarantineInstance 把 instance 移入 quarantined（用于 I1 双 active 场景）。
	QuarantineInstance(ctx context.Context, accountNo string, reason string) error
}

// InvariantAuditJob 不变量自检 job。
type InvariantAuditJob struct {
	reader InvariantAuditReader
	healer AuditHealer
	owner  string
	clock  func() time.Time
}

// NewInvariantAuditJob 构造。
func NewInvariantAuditJob(
	reader InvariantAuditReader,
	healer AuditHealer,
	owner string,
	clock func() time.Time,
) *InvariantAuditJob {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if owner == "" {
		owner = "invariant-audit-anonymous"
	}
	return &InvariantAuditJob{reader: reader, healer: healer, owner: owner, clock: clock}
}

// AuditResult 一次扫描的全部违反 + 自愈结果。
type AuditResult struct {
	Violations    []InvariantViolation
	AutoHealedNum int
	UnhealedNum   int
}

// Run 执行一次全量巡检。
func (j *InvariantAuditJob) Run(ctx context.Context) (*AuditResult, error) {
	result := &AuditResult{}
	now := j.clock()

	// 1. 逐 LA 检查 I1 (active 数) + LA 一致性
	las, err := j.reader.ListAllRotatingLAs(ctx, 1000)
	if err != nil {
		return nil, fmt.Errorf("audit: list LAs: %w", err)
	}
	for _, la := range las {
		// I1: 同 LA 下至多 1 个 active
		activeCount, err := j.reader.CountInstancesByPhase(ctx, la.ID, model.LifecyclePhaseActive)
		if err != nil {
			continue // 容错：单 LA 失败不阻断整次审计
		}
		if activeCount > 1 {
			result.Violations = append(result.Violations, InvariantViolation{
				Type:              ViolationI1MultipleActive,
				LogicalAccountID:  la.ID,
				LogicalAccountKey: la.LogicalAccountKey,
				Detail:            fmt.Sprintf("%d active instances under same LA (I1 violation)", activeCount),
				Severity:          "P0",
				DetectedAt:        now,
				AutoHealed:        false, // P0：不自动处理，必须 ops 决定保留哪个
			})
			result.UnhealedNum++
		}

		// I-LA: LA.current_active 与实际 phase=active 一致
		realActive, _, err := j.reader.GetActiveAccountNoForLogical(ctx, la.ID)
		if err != nil {
			continue
		}
		declaredActive := ""
		if la.CurrentActiveAccountNo != nil {
			declaredActive = *la.CurrentActiveAccountNo
		}
		if realActive != declaredActive {
			v := InvariantViolation{
				Type:              ViolationLAActiveMismatch,
				LogicalAccountID:  la.ID,
				LogicalAccountKey: la.LogicalAccountKey,
				Detail: fmt.Sprintf("LA.current_active=%s but actual phase=active instance=%s",
					declaredActive, realActive),
				Severity:   "P1",
				DetectedAt: now,
			}
			// 自愈：把 LA.current_active 改为真实 active（仅当真实 active 唯一存在时）
			if realActive != "" && activeCount == 1 {
				if err := j.healer.RealignLogicalActive(ctx, la.ID, realActive); err == nil {
					v.AutoHealed = true
					result.AutoHealedNum++
				} else {
					result.UnhealedNum++
				}
			} else {
				result.UnhealedNum++
			}
			result.Violations = append(result.Violations, v)
		}
	}

	// 2. I4: archived instance balance ≠ 0
	archivedBad, err := j.reader.ListArchivedNonZeroBalance(ctx, 1000)
	if err == nil {
		for _, inst := range archivedBad {
			result.Violations = append(result.Violations, InvariantViolation{
				Type:       ViolationI4ArchivedNonZeroBalance,
				AccountNo:  inst.AccountNo,
				Detail:     fmt.Sprintf("archived instance balance=%d (should be 0)", inst.Balance),
				Severity:   "P0",
				DetectedAt: now,
			})
			result.UnhealedNum++
		}
	}

	// I-MS check 已删除（MigrationSuspense business_type 移除）

	// 4. Migration chain depth > 5 → 应当 quarantined
	deepAnchors, err := j.reader.ListAnchorsWithChainDepthExceeded(ctx, 5, 1000)
	if err == nil {
		for _, anchor := range deepAnchors {
			v := InvariantViolation{
				Type:       ViolationChainDepthExceeded,
				AnchorID:   anchor.ID,
				AccountNo:  anchor.AccountNo,
				Detail:     fmt.Sprintf("anchor migration_chain_depth=%d > 5 (E-50)", anchor.MigrationChainDepth),
				Severity:   "P1",
				DetectedAt: now,
			}
			// 自愈：把该 instance quarantined
			if err := j.healer.QuarantineInstance(ctx, anchor.AccountNo, "migration chain depth exceeded"); err == nil {
				v.AutoHealed = true
				result.AutoHealedNum++
			} else {
				result.UnhealedNum++
			}
			result.Violations = append(result.Violations, v)
		}
	}

	return result, nil
}
