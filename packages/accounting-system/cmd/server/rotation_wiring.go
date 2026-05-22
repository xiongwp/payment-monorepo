package main

import (
	"os"
	"time"

	"github.com/xiongwp/accounting-system/internal/idgen"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"github.com/xiongwp/accounting-system/internal/repository"
	"github.com/xiongwp/accounting-system/internal/service"
)

// ============================================================================
// 轮换账户管理：fx 接线
//
// 把 service.NewAdminService + 完整 Scheduler 接进 fx 图，让 adminhttp 的
// /admin/rotation/* 端点全部可用（含 ManualSwitch / ManualProvision）。
//
// Scheduler 依赖链：
//   LogicalAccountRepository (lister + policy reader + admin reader/registrar)
//   AccountInstanceManager   (instance phase 变更)
//   AccountRepository        (account by no)
//   DistributedLockRepository → RotationLockManager  (logical-level 并发互斥)
//   idgen.IDGenerator + shadow.EncodeAccountID → AccountIDGenerator (生 account_no)
//
// 注：Scheduler.Tick() 定时循环还没在这里 wire（只接了 Force* 路径以满足 admin UI）。
// 后续要按时间自动轮换的话再加个 fx.Invoke 把 Tick 挂到 context-aware ticker 上。
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

// ============================================================================
// Scheduler 接线 — 让 ManualSwitch / ManualProvision 真生效
// ============================================================================

// NewDistributedLockRepositoryFx fx provider — 跨 shard 锁仓储。
func NewDistributedLockRepositoryFx(
	dbm *database.Manager, router *sharding.Router,
) repository.DistributedLockRepository {
	return repository.NewDistributedLockRepository(dbm, router)
}

// NewRotationLockManagerFx fx provider — Scheduler 用的 LockManager。
// TTL 60s 足以覆盖一次 ManualSwitch / ManualProvision 调用。
func NewRotationLockManagerFx(
	lockRepo repository.DistributedLockRepository, router *sharding.Router,
) service.LockManager {
	return repository.NewRotationLockManager(lockRepo, router, 60*time.Second)
}

// NewSchedulerLogicalLister 把 LogicalAccountRepository 适配为 service.LogicalAccountLister。
// 跟 NewSchedulerPolicyReader 是同一个底层适配器，但 fx 类型系统要求两个 provider 分开。
func NewSchedulerLogicalLister(repo repository.LogicalAccountRepository) service.LogicalAccountLister {
	return service.NewLogicalAccountSchedulerAdapter(repo)
}

// NewSchedulerPolicyReader 把 LogicalAccountRepository 适配为 service.PolicyReaderForScheduler。
func NewSchedulerPolicyReader(repo repository.LogicalAccountRepository) service.PolicyReaderForScheduler {
	return service.NewLogicalAccountSchedulerAdapter(repo)
}

// NewSchedulerInstanceManager 适配 repo.AccountInstanceManager → service.AccountInstanceManager。
func NewSchedulerInstanceManager(
	mgr repository.AccountInstanceManager,
) service.AccountInstanceManager {
	return service.NewAccountInstanceManagerAdapter(mgr)
}

// NewSchedulerAccountIDGenerator 真实 ID 生成器（包 shadow.EncodeAccountID + idgen 号段）。
func NewSchedulerAccountIDGenerator(g idgen.IDGenerator) service.AccountIDGenerator {
	return service.NewAccountIDGenerator(g)
}

// NewRotationScheduler 组装完整 Scheduler。
// owner 用 HOSTNAME 让多副本之间区分；空字符串走 service.NewScheduler 的默认值。
func NewRotationScheduler(
	lister service.LogicalAccountLister,
	instances service.AccountInstanceManager,
	policies service.PolicyReaderForScheduler,
	locks service.LockManager,
	idgenSvc service.AccountIDGenerator,
) *service.Scheduler {
	owner := os.Getenv("HOSTNAME")
	if owner == "" {
		owner = "accounting-rotation-scheduler"
	}
	return service.NewScheduler(lister, instances, policies, locks, idgenSvc, owner, nil /* clock=Now */)
}

// NewRotationSchedulerCommand fx provider — 把 Scheduler 适配为 SchedulerCommand
// 注入 AdminService。现在 ManualSwitch / ManualProvision 真生效（替换了 stub）。
func NewRotationSchedulerCommand(sch *service.Scheduler) service.SchedulerCommand {
	return service.NewAdminSchedulerCommandAdapter(sch)
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
