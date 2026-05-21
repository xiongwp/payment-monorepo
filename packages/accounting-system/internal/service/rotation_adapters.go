package service

import (
	"context"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/repository"
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
