package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// Forced Migration Job — 长尾 anchor 强制迁移
//
// 触发场景：anchor 在 draining instance 上停留过久（超过 drain_hard_timeout），
// 业务上无法自然 settle（争议持续、对手方失联、等待数据）。
// 解决方案：把 anchor 强制搬到当前 active instance，让 flow 在新 instance 上继续。
//
// 流程（每个 anchor 独立处理）：
//   1. 找到 logical 当前 active instance（迁移目标）
//   2. 在源 instance 上 mark anchor.status=migrated, migrated_to=新 instance, chain_depth++
//   3. 同步更新 flow_anchor_route.account_no = 新 instance, chain_depth++
//   4. 生成迁移凭证（借: 内部过渡科目, 贷: 源 instance；新 instance 接收）
//      —— 原 MigrationSuspense business_type 已删除，过渡科目改用代码内部专用编码
//
// 【方向 B 关键】anchor 和 routing 在不同分片：
//   - anchor 在 source account_no 分片
//   - routing 在 flow_id 分片
//   - 必须用 TCC 或顺序两步事务协调
//
// 多次迁移（chain_depth > 5）→ 进入 quarantined，停止自动迁移。
//
// 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §8
// ============================================================================

// StuckAnchorLister 找到候选迁移 anchor。
type StuckAnchorLister interface {
	// ListMigrationCandidates 返回所有 status=active 且 last_posting_at < cutoff 的 anchor，
	// 跨所有分片扫描（migration job 频率低，可承受全表扫描）。
	ListMigrationCandidates(ctx context.Context, cutoff time.Time, limit int) ([]*model.TxAccountAnchor, error)
}

// MigrationExecutor 执行单 anchor 的强制迁移。
// 实现层负责协调跨分片的 anchor + routing + voucher 三方写入。
type MigrationExecutor interface {
	// Migrate 执行强制迁移：
	//   - source: 源 anchor（status=active 即将转为 migrated）
	//   - targetAccountNo: 新 instance 的 account_no
	//   - voucherNo: 调用方生成的迁移凭证号
	// 返回错误时业务方 retry：next call 应该是幂等的（DB 唯一键 + status 校验）。
	Migrate(ctx context.Context, params MigrationParams) error
}

// MigrationParams 迁移参数。
type MigrationParams struct {
	SourceAnchor          *model.TxAccountAnchor
	SourceRouteID         int64
	SourceRouteVersion    int64
	TargetAccountNo       string
	VoucherNo             string
	NewChainDepth         int8
	Now                   time.Time
}

// ActiveResolver 找到 logical 当前 active instance 的 account_no。
type ActiveResolver interface {
	GetActiveAccountNo(ctx context.Context, logicalAccountID int64) (string, error)
}

// RouteByFlowReader 根据 flow_id + LA 查 routing（迁移时需要更新 chain_depth）。
type RouteByFlowReader interface {
	GetByFlowAndLogical(ctx context.Context, flowID string, logicalAccountID int64) (*model.FlowAnchorRoute, error)
}

// VoucherNoGenerator 为迁移凭证生成唯一编号。
type VoucherNoGenerator interface {
	NewMigrationVoucherNo(ctx context.Context, anchorID int64) (string, error)
}

// MigrationJob 强制迁移调度。
type MigrationJob struct {
	lister        StuckAnchorLister
	executor      MigrationExecutor
	resolver      ActiveResolver
	routes        RouteByFlowReader
	voucherGen    VoucherNoGenerator
	locks         LockManager
	policies      PolicyReaderForScheduler
	maxChainDepth int8
	owner         string
	clock         func() time.Time
}

// NewMigrationJob 构造。
func NewMigrationJob(
	lister StuckAnchorLister,
	executor MigrationExecutor,
	resolver ActiveResolver,
	routes RouteByFlowReader,
	voucherGen VoucherNoGenerator,
	locks LockManager,
	policies PolicyReaderForScheduler,
	owner string,
	clock func() time.Time,
) *MigrationJob {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if owner == "" {
		owner = "migration-job-anonymous"
	}
	return &MigrationJob{
		lister:        lister,
		executor:      executor,
		resolver:      resolver,
		routes:        routes,
		voucherGen:    voucherGen,
		locks:         locks,
		policies:      policies,
		maxChainDepth: 5, // E-50: 超过 5 跳进 quarantined
		owner:         owner,
		clock:         clock,
	}
}

// MigrationResult 一次 Tick 汇总。
type MigrationResult struct {
	Scanned         int
	Migrated        int
	SkippedTooDeep  int // chain_depth >= 5 → quarantined（本 job 不处理）
	SkippedNoActive int // 找不到迁移目标
	Errors          []MigrationError
}

// MigrationError 单 anchor 处理失败诊断。
type MigrationError struct {
	AnchorID int64
	FlowID   string
	Err      error
}

// Tick 触发一次迁移扫描 + 执行。
//
// 处理顺序：
//   1. 扫描所有 active 且 last_posting_at < cutoff 的 anchor
//   2. 对每个 anchor：
//      a. 检查 chain_depth：>= max → 跳过（应该已经 quarantined）
//      b. 拿 logical 锁（与 scheduler 同锁）
//      c. 查 policy 拿 hard_timeout
//      d. 确认本 anchor 确实超过 hard_timeout（双重校验）
//      e. 找到 active 目标 instance
//      f. 生成 voucher_no
//      g. 调用 MigrationExecutor.Migrate
func (j *MigrationJob) Tick(ctx context.Context) (*MigrationResult, error) {
	result := &MigrationResult{}
	// 用一个保守的 cutoff（最大 P99）— 真实判定在 processOne 内
	cutoff := j.clock().Add(-24 * time.Hour) // 至少 1 天前最后活动
	anchors, err := j.lister.ListMigrationCandidates(ctx, cutoff, 1000)
	if err != nil {
		return nil, fmt.Errorf("migration: list candidates: %w", err)
	}
	for _, anchor := range anchors {
		result.Scanned++
		if err := j.processOne(ctx, anchor, result); err != nil {
			result.Errors = append(result.Errors, MigrationError{
				AnchorID: anchor.ID,
				FlowID:   anchor.FlowID,
				Err:      err,
			})
		}
	}
	return result, nil
}

func (j *MigrationJob) processOne(
	ctx context.Context, anchor *model.TxAccountAnchor, result *MigrationResult,
) error {
	// 1. chain_depth 上限检查
	if anchor.MigrationChainDepth >= j.maxChainDepth {
		// 应该已经被 invariant_audit_job 检测到并 quarantined
		result.SkippedTooDeep++
		return nil
	}

	// 2. 锁
	release, err := j.locks.AcquireForLogical(ctx, anchor.LogicalAccountID, j.owner)
	if err != nil {
		return nil //nolint:nilerr
	}
	defer release()

	// 3. policy 校验 hard_timeout
	policy, err := j.policies.GetPolicy(ctx, anchor.LogicalAccountID)
	if err != nil {
		return fmt.Errorf("get policy: %w", err)
	}
	if policy == nil {
		return errors.New("no policy")
	}

	// 双重校验：anchor 的"年龄"必须确实超过 hard_timeout
	age := j.clock().Sub(anchor.LastPostingAt)
	hardTimeout := time.Duration(policy.DrainHardTimeoutSecs) * time.Second
	if age < hardTimeout {
		// 还没到强制迁移的时候 — 收敛 job 会处理（等业务自然 settle）
		return nil
	}

	// 4. 目标 active instance
	targetAccountNo, err := j.resolver.GetActiveAccountNo(ctx, anchor.LogicalAccountID)
	if err != nil {
		return fmt.Errorf("get active: %w", err)
	}
	if targetAccountNo == "" {
		result.SkippedNoActive++
		return errors.New("no active instance to migrate to")
	}
	if targetAccountNo == anchor.AccountNo {
		// 已经在 active 上（异常态：可能数据不一致）
		return fmt.Errorf("anchor already on active instance %s — likely data inconsistency", targetAccountNo)
	}

	// 5. routing 信息
	route, err := j.routes.GetByFlowAndLogical(ctx, anchor.FlowID, anchor.LogicalAccountID)
	if err != nil {
		return fmt.Errorf("get route: %w", err)
	}
	if route == nil {
		return fmt.Errorf("routing missing for flow=%s la=%d", anchor.FlowID, anchor.LogicalAccountID)
	}

	// 6. voucher
	voucherNo, err := j.voucherGen.NewMigrationVoucherNo(ctx, anchor.ID)
	if err != nil {
		return fmt.Errorf("alloc voucher_no: %w", err)
	}

	// 7. 执行迁移（实现层负责跨片协调）
	params := MigrationParams{
		SourceAnchor:       anchor,
		SourceRouteID:      route.ID,
		SourceRouteVersion: route.Version,
		TargetAccountNo:    targetAccountNo,
		VoucherNo:          voucherNo,
		NewChainDepth:      anchor.MigrationChainDepth + 1,
		Now:                j.clock(),
	}
	if err := j.executor.Migrate(ctx, params); err != nil {
		return fmt.Errorf("execute migration: %w", err)
	}
	result.Migrated++
	return nil
}
