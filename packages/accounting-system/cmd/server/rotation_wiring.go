package main

import (
	"context"
	"errors"

	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"github.com/xiongwp/accounting-system/internal/repository"
	"github.com/xiongwp/accounting-system/internal/service"
)

// ============================================================================
// 轮换账户管理：fx 接线
//
// 把 service.NewAdminService 接进 fx 图，让 adminhttp 的 /admin/rotation/*
// 端点可用。一次性 wiring 完成：
//   - LogicalAccountRepository
//   - AccountInstanceManager（含 ListByLogical）
//   - AdminAccountReaderAdapter / SchedulerCommand
//   - service.AdminService
//
// 注意：ManualSwitch / ManualProvision 需要完整 Scheduler 才能跑。Scheduler
// 自身依赖 LogicalAccountLister.ListRotating + AccountIDGenerator.NewProvisionedAccountNo
// 这俩接口目前还没有生产实现（rotation 是新特性，尚未对接 idgen）。
// 这里给一个 stubSchedulerCommand，让两个写端点直接返回明确的错误而非 nil panic。
// 等 Scheduler 完整接线（含 background Tick loop）后，把 NewStubSchedulerCommand
// 换成 NewAdminSchedulerCommandAdapter(scheduler) 即可。
// ============================================================================

// NewRotationLogicalAccountRepository fx provider — 单实例。
func NewRotationLogicalAccountRepository(dbm *database.Manager) repository.LogicalAccountRepository {
	return repository.NewLogicalAccountRepository(dbm)
}

// NewRotationAccountInstanceManager fx provider — 跨表 instance 读写。
func NewRotationAccountInstanceManager(
	dbm *database.Manager,
	router *sharding.Router,
	laRepo repository.LogicalAccountRepository,
) repository.AccountInstanceManager {
	return repository.NewAccountInstanceManager(dbm, router, laRepo)
}

// NewRotationLogicalAccountAdminReader 适配 LogicalAccountRepository → service.LogicalAccountAdminReader
// 三个方法签名完全一致，直接返回 repo 即可。
func NewRotationLogicalAccountAdminReader(
	laRepo repository.LogicalAccountRepository,
) service.LogicalAccountAdminReader {
	return laRepo
}

// NewRotationLogicalAccountAdminRegistrar 适配 LogicalAccountRepository → service.LogicalAccountAdminRegistrar
// Register 方法签名一致，直接复用同一份 repo 实现。
func NewRotationLogicalAccountAdminRegistrar(
	laRepo repository.LogicalAccountRepository,
) service.LogicalAccountAdminRegistrar {
	return laRepo
}

// NewRotationAccountAdminReader 适配 instance manager + account repo → service.AccountAdminReader
func NewRotationAccountAdminReader(
	instances repository.AccountInstanceManager,
	accounts repository.AccountRepository,
) service.AccountAdminReader {
	return service.NewAdminAccountReaderAdapter(instances, accounts)
}

// stubSchedulerCommand 临时实现：在 Scheduler 完整接线之前，让 manual-switch/
// manual-provision 立刻返回明确错误，而不是空 nil panic。
//
// 用户在 UI 上点"立即切换" → 看到错误信息 → 知道接线未完成。
// 等 NewScheduler 完整 wiring 写好后，把这个 stub 换成 NewAdminSchedulerCommandAdapter(sch)。
type stubSchedulerCommand struct{}

// NewStubSchedulerCommand fx provider。
func NewStubSchedulerCommand() service.SchedulerCommand {
	return stubSchedulerCommand{}
}

func (stubSchedulerCommand) ForceSwitch(_ context.Context, _ int64, _, _ string) error {
	return errors.New("rotation scheduler not wired in this build; ManualSwitch unavailable (需在 cmd/server 接入完整 Scheduler + Tick loop)")
}

func (stubSchedulerCommand) ForceProvision(_ context.Context, _ int64, _, _ string) error {
	return errors.New("rotation scheduler not wired in this build; ManualProvision unavailable (需在 cmd/server 接入完整 Scheduler + Tick loop)")
}

// NewRotationAdminService fx provider — 组装 service.AdminService。
// registrar 走 LogicalAccountRepository.Register（已含前缀白名单 + unique 冲突保护）。
func NewRotationAdminService(
	logicals service.LogicalAccountAdminReader,
	registrar service.LogicalAccountAdminRegistrar,
	accounts service.AccountAdminReader,
	scheduler service.SchedulerCommand,
) *service.AdminService {
	return service.NewAdminService(logicals, registrar, accounts, scheduler, nil /* clock=time.Now */)
}
