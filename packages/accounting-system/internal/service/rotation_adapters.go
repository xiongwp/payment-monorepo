package service

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/idgen"
	"github.com/xiongwp/accounting-system/internal/repository"
	"github.com/xiongwp/payment-util/money"
	"github.com/xiongwp/payment-util/shadow"
)

// ============================================================================
// Repository → Service 接口适配器
//
// service 层定义了自己的接口（便于 mock 单测）；repo 层有不同的实现签名。
// 这些适配器把 repo 实现包装成 service 接口。
// ============================================================================

// accountInstanceManagerAdapter 把 repo.AccountInstanceManager 适配为 service 用的两个接口：
// AccountInstanceManager (scheduler) + InstancePhasePromoter (convergence)。
type accountInstanceManagerAdapter struct {
	repo repository.AccountInstanceManager
}

// NewAccountInstanceManagerAdapter 工厂。
func NewAccountInstanceManagerAdapter(repo repository.AccountInstanceManager) *accountInstanceManagerAdapter {
	return &accountInstanceManagerAdapter{repo: repo}
}

func (a *accountInstanceManagerAdapter) GetActiveInstance(ctx context.Context, laID int64) (*model.Account, error) {
	return a.repo.GetActiveInstance(ctx, laID)
}

func (a *accountInstanceManagerAdapter) GetProvisionedInstance(ctx context.Context, laID int64) (*model.Account, error) {
	return a.repo.GetProvisionedInstance(ctx, laID)
}

func (a *accountInstanceManagerAdapter) CreateProvisioned(ctx context.Context, acc *model.Account) error {
	return a.repo.CreateProvisioned(ctx, acc)
}

func (a *accountInstanceManagerAdapter) PromoteAndDrain(ctx context.Context, p PromoteAndDrainParams) error {
	return a.repo.PromoteAndDrain(ctx, repository.AIMPromoteParams{
		LogicalAccountID:      p.LogicalAccountID,
		OldActiveAccountNo:    p.OldActiveAccountNo,
		OldActiveVersion:      p.OldActiveVersion,
		NewActiveAccountNo:    p.NewActiveAccountNo,
		NewActiveVersion:      p.NewActiveVersion,
		NewActiveGroup:        p.NewActiveGroup,
		NewActivePeriodEnd:    p.NewActivePeriodEnd,
		LogicalAccountVersion: p.LogicalAccountVersion,
		Now:                   p.Now,
	})
}

func (a *accountInstanceManagerAdapter) PromoteAndDrainFleet(ctx context.Context, p PromoteAndDrainFleetParams) error {
	return a.repo.PromoteAndDrainFleet(ctx, repository.AIMPromoteFleetParams{
		LogicalAccountID:      p.LogicalAccountID,
		OldGroup:              p.OldGroup,
		NewGroup:              p.NewGroup,
		NewActiveAccountNo:    p.NewActiveAccountNo,
		NewActivePeriodEnd:    p.NewActivePeriodEnd,
		LogicalAccountVersion: p.LogicalAccountVersion,
		Now:                   p.Now,
	})
}

// PromoteToFrozen 适配 InstancePhasePromoter 接口。
func (a *accountInstanceManagerAdapter) PromoteToFrozen(
	ctx context.Context, accountNo string, expectedVersion int64, frozenAt time.Time,
) error {
	return a.repo.PromoteToFrozen(ctx, accountNo, expectedVersion, frozenAt)
}

// ============================================================================
// AdminService 适配器 — 给 admin-web HTTP 端点用
//
// service.AdminService 要求 3 个上游接口（LogicalAccountAdminReader / AccountAdminReader
// / SchedulerCommand）。本文件下半给 repo 实现到这 3 个接口的适配。
// ============================================================================

// adminAccountReaderAdapter 用 AccountInstanceManager (sharded ListByLogical) +
// AccountRepository (GetAccountByNo) 实现 AccountAdminReader。
type adminAccountReaderAdapter struct {
	instances repository.AccountInstanceManager
	accounts  repository.AccountRepository
}

// NewAdminAccountReaderAdapter 工厂。
func NewAdminAccountReaderAdapter(
	instances repository.AccountInstanceManager,
	accounts repository.AccountRepository,
) AccountAdminReader {
	return &adminAccountReaderAdapter{instances: instances, accounts: accounts}
}

func (a *adminAccountReaderAdapter) ListByLogical(
	ctx context.Context, logicalAccountID int64, limit int,
) ([]*model.Account, error) {
	return a.instances.ListByLogical(ctx, logicalAccountID, limit)
}

func (a *adminAccountReaderAdapter) GetByAccountNo(ctx context.Context, accountNo string) (*model.Account, error) {
	return a.accounts.GetAccountByNo(ctx, accountNo)
}

// GetActiveSubAccount 实现 AccountAdminReader.GetActiveSubAccount — 给 fleet 测试端点用。
func (a *adminAccountReaderAdapter) GetActiveSubAccount(
	ctx context.Context, logicalAccountID int64, subIdx int,
) (*model.Account, error) {
	return a.instances.GetActiveSubAccount(ctx, logicalAccountID, subIdx)
}

// SumBalanceByLogical 适配 repo.SumBalanceByLogical → service 层视图。
func (a *adminAccountReaderAdapter) SumBalanceByLogical(ctx context.Context, logicalAccountID int64) (LogicalAccountBalanceSummary, error) {
	repoOut, err := a.instances.SumBalanceByLogical(ctx, logicalAccountID)
	if err != nil {
		return LogicalAccountBalanceSummary{}, err
	}
	return LogicalAccountBalanceSummary{
		LogicalAccountID: repoOut.LogicalAccountID,
		Total:            repoOut.Total,
		InstanceCount:    repoOut.InstanceCount,
		ByGroup:          repoOut.ByGroup,
		ByPhase:          repoOut.ByPhase,
		GroupCounts:      repoOut.GroupCounts,
		PhaseCounts:      repoOut.PhaseCounts,
	}, nil
}

// adminSchedulerCommandAdapter 让 *Scheduler 满足 SchedulerCommand 接口（接口签名
// 已经完全一致，但拿到的是 struct 不是 interface，包一层）。
type adminSchedulerCommandAdapter struct {
	sch *Scheduler
}

// NewAdminSchedulerCommandAdapter 工厂。
func NewAdminSchedulerCommandAdapter(sch *Scheduler) SchedulerCommand {
	return &adminSchedulerCommandAdapter{sch: sch}
}

func (a *adminSchedulerCommandAdapter) ForceSwitch(ctx context.Context, laID int64, operator, reason string) error {
	return a.sch.ForceSwitch(ctx, laID, operator, reason)
}

func (a *adminSchedulerCommandAdapter) ForceProvision(ctx context.Context, laID int64, operator, reason string) error {
	return a.sch.ForceProvision(ctx, laID, operator, reason)
}

// LogicalAccountRepository 已经满足 LogicalAccountAdminReader 接口（GetByKey/GetByID/
// ListByPrefix 签名都一致），所以 fx provider 可以直接传 repo 实例。

// ============================================================================
// AccountIDGenerator 真实实现 —— scheduler 创建 provisioned instance 时用
// ============================================================================

// accountIDGeneratorImpl 实现 service.AccountIDGenerator。
//
// account_no 19 位 layout（见 payment-util/shadow.EncodeAccountID）：
//   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
//
// 跟 accounting_service.go:generateAccountNo 一致；区别是这里 globalTblIdx 不走
// user_id 而是用 LA.ID 散到 0..99（LA 不属于任何用户，独立分片）。
type accountIDGeneratorImpl struct {
	idGen idgen.IDGenerator
}

// NewAccountIDGenerator 工厂。
func NewAccountIDGenerator(g idgen.IDGenerator) AccountIDGenerator {
	return &accountIDGeneratorImpl{idGen: g}
}

func (g *accountIDGeneratorImpl) NewProvisionedAccountNo(
	ctx context.Context, la *model.LogicalAccount, _ time.Time,
) (string, error) {
	if la == nil {
		return "", fmt.Errorf("NewProvisionedAccountNo: nil LogicalAccount")
	}
	// 单 instance 路径（Phase 1 用，Phase 2 fleet 走下面 Fleet 方法）：
	// 落到 la.ID hash % 100 的固定 shard，方便老的单 instance 操作。
	h := fnv.New32a()
	_, _ = h.Write([]byte(strconv.FormatInt(la.ID, 10)))
	globalTblIdx := int(h.Sum32() % 100)
	return g.encodeAccountNo(ctx, la, globalTblIdx)
}

func (g *accountIDGeneratorImpl) NewProvisionedFleetAccountNos(
	ctx context.Context, la *model.LogicalAccount, _ time.Time,
) ([]string, error) {
	if la == nil {
		return nil, fmt.Errorf("NewProvisionedFleetAccountNos: nil LogicalAccount")
	}
	out := make([]string, 100)
	for i := 0; i < 100; i++ {
		// 第 i 个 sub-account 的 globalTblIdx 直接用 i（散到所有 100 个分片）
		no, err := g.encodeAccountNo(ctx, la, i)
		if err != nil {
			return nil, fmt.Errorf("fleet sub %d: %w", i, err)
		}
		out[i] = no
	}
	return out, nil
}

// encodeAccountNo 封装 EncodeAccountID 调用，给定 globalTblIdx 生成对应 shard 的 account_no。
func (g *accountIDGeneratorImpl) encodeAccountNo(
	ctx context.Context, la *model.LogicalAccount, globalTblIdx int,
) (string, error) {
	currencyNum, err := money.NumericCode(la.Currency)
	if err != nil {
		return "", fmt.Errorf("currency %q: %w", la.Currency, err)
	}
	seq, err := g.idGen.NextID(ctx, idgen.BizTagAccount)
	if err != nil {
		return "", fmt.Errorf("idgen: %w", err)
	}
	id, err := shadow.EncodeAccountID(ctx,
		currencyNum,
		int(la.AccountType),
		globalTblIdx,
		int(la.AccountBusinessType),
		seq,
	)
	if err != nil {
		return "", fmt.Errorf("encode: %w", err)
	}
	return strconv.FormatInt(id, 10), nil
}

// ============================================================================
// LogicalAccount 适配器：repo → service.LogicalAccountLister + PolicyReaderForScheduler
// ============================================================================

// logicalAccountSchedulerAdapter 包 LogicalAccountRepository → service 接口三件套。
// 同一个 repo 实现 ListRotating（LogicalAccountLister）+ GetPolicy（PolicyReaderForScheduler）。
type logicalAccountSchedulerAdapter struct {
	repo repository.LogicalAccountRepository
}

// NewLogicalAccountSchedulerAdapter 工厂。
func NewLogicalAccountSchedulerAdapter(repo repository.LogicalAccountRepository) *logicalAccountSchedulerAdapter {
	return &logicalAccountSchedulerAdapter{repo: repo}
}

// ListRotating 实现 LogicalAccountLister。
func (a *logicalAccountSchedulerAdapter) ListRotating(ctx context.Context, limit int) ([]*model.LogicalAccount, error) {
	return a.repo.ListRotating(ctx, limit)
}

// GetPolicy 实现 PolicyReaderForScheduler。
func (a *logicalAccountSchedulerAdapter) GetPolicy(ctx context.Context, logicalAccountID int64) (*model.LogicalAccountRotationPolicy, error) {
	return a.repo.GetPolicy(ctx, logicalAccountID)
}
