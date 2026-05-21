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
