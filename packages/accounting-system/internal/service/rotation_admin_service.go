package service

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// Rotation Admin Service — admin-web 运维接口
//
// 提供：
//   1. 列出所有正在使用的中间账户（current active）
//   2. 查询某 logical_account 的所有 instance 历史（含余额、phase、时间戳、is_zero）
//   3. 手动切换 / 手动预创建（封装 scheduler 的 force 操作）
//   4. 单 instance 详情（用于 admin-web 详情页）
//   5. 聚合视图（balance 总和、phase 计数）
//
// 设计原则：
//   - 所有写操作必须 operator + reason 必填（审计）
//   - 读操作 paginate 友好
//   - 返回数据结构对 admin-web 前端友好（JSON-ready）
// ============================================================================

// LogicalAccountAdminReader 管理员接口：读 LA。
type LogicalAccountAdminReader interface {
	GetByKey(ctx context.Context, key string) (*model.LogicalAccount, error)
	GetByID(ctx context.Context, id int64) (*model.LogicalAccount, error)
	ListByPrefix(ctx context.Context, prefix string, limit int) ([]*model.LogicalAccount, error)
}

// LogicalAccountAdminRegistrar 管理员接口：写（注册新 LA + 配 policy）。
// 实现走 repository.LogicalAccountRepository（含命名前缀白名单 + unique 冲突保护）。
type LogicalAccountAdminRegistrar interface {
	Register(ctx context.Context, la *model.LogicalAccount) (*model.LogicalAccount, error)
	// UpsertPolicy 创建或更新 rotation policy。注册 rotation_enabled=true 的 LA 后
	// AdminService 自动调一次填默认值，避免 ForceProvision 报 no policy。
	UpsertPolicy(ctx context.Context, p *model.LogicalAccountRotationPolicy) error
}

// AccountAdminReader 管理员接口：读账户实例。
type AccountAdminReader interface {
	// ListByLogical 返回 LA 下所有 instance（所有 phase），按 period_start 升序。
	ListByLogical(ctx context.Context, logicalAccountID int64, limit int) ([]*model.Account, error)

	// GetByAccountNo 单 instance 详情。
	GetByAccountNo(ctx context.Context, accountNo string) (*model.Account, error)

	// SumBalanceByLogical 聚合 LA 全部 instance 的余额（fleet 全部 sub 之和；排除 archived）。
	// 用于对账 + admin UI 显示 LA 维度总余额。
	SumBalanceByLogical(ctx context.Context, logicalAccountID int64) (LogicalAccountBalanceSummary, error)

	// GetActiveSubAccount fleet 路由：拿 fleet 中 user_id=subIdx 的当前 active sub-account。
	// 用于 ResolveFleetSubAccount 和 booking demo。
	GetActiveSubAccount(ctx context.Context, logicalAccountID int64, subIdx int) (*model.Account, error)
}

// BookingInvoker fleet booking demo 用 — 仅暴露 DoubleEntryBooking。
// 生产 caller 走 gRPC.CreateTransaction；admin HTTP 测试端点走这个接口。
//
// 签名跟 accountingService.DoubleEntryBooking 一致：(voucherNo, txIDs, err)。
type BookingInvoker interface {
	DoubleEntryBooking(ctx context.Context, req *DoubleEntryBookingRequest) (string, []string, error)
}

// LogicalAccountBalanceSummary admin / 对账接口的 LA 余额聚合视图。
// 跟 repository.LogicalAccountBalanceSummary 同 shape；这里复制一份避免 service 层依赖 repo 类型。
type LogicalAccountBalanceSummary struct {
	LogicalAccountID  int64            `json:"logical_account_id"`
	LogicalAccountKey string           `json:"logical_account_key,omitempty"`
	Currency          string           `json:"currency,omitempty"`
	Total             int64            `json:"total_balance"`
	InstanceCount     int              `json:"instance_count"`
	ByGroup           map[string]int64 `json:"by_group"`
	ByPhase           map[int]int64    `json:"by_phase"`
	GroupCounts       map[string]int   `json:"group_counts"`
	PhaseCounts       map[int]int      `json:"phase_counts"`
}

// SchedulerCommand 让 admin service 调用 scheduler 的强制操作。
// 抽象出来便于测试 mock。
type SchedulerCommand interface {
	ForceSwitch(ctx context.Context, logicalAccountID int64, operator, reason string) error
	ForceProvision(ctx context.Context, logicalAccountID int64, operator, reason string) error
}

// AdminService admin-web 后端服务。
type AdminService struct {
	logicals  LogicalAccountAdminReader
	registrar LogicalAccountAdminRegistrar // 可空 → RegisterLogicalAccount 返回明确错误
	accounts  AccountAdminReader
	scheduler SchedulerCommand
	booker    BookingInvoker // 可空 → fleet test-book 端点返回 "not wired"
	clock     func() time.Time
}

// NewAdminService 构造。registrar / booker 传 nil 时对应写端点降级（返回 "not wired"），不 panic。
func NewAdminService(
	logicals LogicalAccountAdminReader,
	registrar LogicalAccountAdminRegistrar,
	accounts AccountAdminReader,
	scheduler SchedulerCommand,
	booker BookingInvoker,
	clock func() time.Time,
) *AdminService {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &AdminService{
		logicals:  logicals,
		registrar: registrar,
		accounts:  accounts,
		scheduler: scheduler,
		booker:    booker,
		clock:     clock,
	}
}

// ============================================================================
// 1. 当前正在使用的中间账户列表
// ============================================================================

// CurrentActiveInstance 一行：某 LA 当前活跃的 instance 概要。
type CurrentActiveInstance struct {
	LogicalAccountID      int64     `json:"logical_account_id"`
	LogicalAccountKey     string    `json:"logical_account_key"`
	AccountType           int8      `json:"account_type"`
	Currency              string    `json:"currency"`
	RotationEnabled       bool      `json:"rotation_enabled"`
	ActiveAccountNo       string    `json:"active_account_no"`
	ActiveAccountBalance  int64     `json:"active_account_balance"`
	ActiveIsZero          bool      `json:"active_is_zero"`
	PeriodStart           time.Time `json:"period_start"`
	PeriodEnd             time.Time `json:"period_end"`
	TimeToEndSeconds      int64     `json:"time_to_end_seconds"`
	ProvisionedReady      bool      `json:"provisioned_ready"`
	ProvisionedAccountNo  string    `json:"provisioned_account_no,omitempty"`
}

// ListCurrentActiveInstances 列出所有正在使用的中间账户，admin-web dashboard 主页面。
//
// 参数 prefix 用于过滤 logical_account_key（如 "channel-payable:" 只看应付）。
func (s *AdminService) ListCurrentActiveInstances(
	ctx context.Context, prefix string, limit int,
) ([]*CurrentActiveInstance, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	las, err := s.logicals.ListByPrefix(ctx, prefix, limit)
	if err != nil {
		return nil, fmt.Errorf("list LAs: %w", err)
	}

	now := s.clock()
	out := make([]*CurrentActiveInstance, 0, len(las))
	for _, la := range las {
		row := &CurrentActiveInstance{
			LogicalAccountID:  la.ID,
			LogicalAccountKey: la.LogicalAccountKey,
			AccountType:       int8(la.AccountType),
			Currency:          la.Currency,
			RotationEnabled:   la.IsRotating(),
		}

		// Anchor sub-account（fleet 中 user_id=0 那个，用于 UI 显示一个代表性 account_no）
		if la.CurrentActiveAccountNo != nil && *la.CurrentActiveAccountNo != "" {
			row.ActiveAccountNo = *la.CurrentActiveAccountNo
		}

		// fleet × rotation：余额要 SUM 整个 active fleet（100 sub），而不是只看 anchor。
		// ListByLogical 拉到的是该 LA 全部 instance（含 GroupA + GroupB 所有 phase）。
		// 这里做客户端聚合：phase=active 算总 balance；同时找 provisioned 标"下期已就绪"；
		// period_start/end 取 active fleet 任意一个 sub 的值（fleet 同步建，period 一致）。
		instances, err := s.accounts.ListByLogical(ctx, la.ID, 250) // 100 active + 100 provisioned + 一些 draining
		if err == nil && len(instances) > 0 {
			var activeSum int64
			activeCount := 0
			for _, inst := range instances {
				switch inst.LifecyclePhase {
				case model.LifecyclePhaseActive:
					activeSum += inst.Balance
					activeCount++
					// 第一次遇到 active 时记录 period
					if activeCount == 1 {
						if inst.PeriodStart != nil {
							row.PeriodStart = *inst.PeriodStart
						}
						if inst.PeriodEnd != nil {
							row.PeriodEnd = *inst.PeriodEnd
							row.TimeToEndSeconds = int64(inst.PeriodEnd.Sub(now).Seconds())
						}
					}
				case model.LifecyclePhaseProvisioned:
					if !row.ProvisionedReady {
						row.ProvisionedReady = true
						row.ProvisionedAccountNo = inst.AccountNo
					}
				}
			}
			if activeCount > 0 {
				row.ActiveAccountBalance = activeSum
				row.ActiveIsZero = activeSum == 0
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// ============================================================================
// 2. 单 logical_account 的所有 instance 历史 + 余额
// ============================================================================

// InstanceHistoryRow 单行：某 LA 的某个 instance 完整信息。
type InstanceHistoryRow struct {
	AccountNo               string     `json:"account_no"`
	LifecyclePhase          int8       `json:"lifecycle_phase"`
	LifecyclePhaseName      string     `json:"lifecycle_phase_name"`
	Balance                 int64      `json:"balance"`
	IsZero                  bool       `json:"is_zero"`
	FrozenBalance           int64      `json:"frozen_balance"`
	AvailableBalance        int64      `json:"available_balance"`
	Currency                string     `json:"currency"`
	PeriodStart             *time.Time `json:"period_start,omitempty"`
	PeriodEnd               *time.Time `json:"period_end,omitempty"`
	DrainingStartedAt       *time.Time `json:"draining_started_at,omitempty"`
	FrozenAt                *time.Time `json:"frozen_at,omitempty"`
	ArchivedAt              *time.Time `json:"archived_at,omitempty"`
	PolicyVersionAtBirth    *int64     `json:"policy_version_at_birth,omitempty"`
	EffectiveHardTimeoutSec *int       `json:"effective_hard_timeout_secs,omitempty"`
	OverrideReason          *string    `json:"override_reason,omitempty"`
	OverrideBy              *string    `json:"override_by,omitempty"`
	Version                 int64      `json:"version"`
}

// GetInstanceHistory 返回 logical_account 下所有 instance（含已 archived）的完整信息。
// 用于 admin-web 详情页：运维要看每个历史 instance 的余额、状态、时间戳。
//
// 默认排序：按 period_start 升序（最老的在前）。
func (s *AdminService) GetInstanceHistory(
	ctx context.Context, logicalAccountKey string,
) (*InstanceHistoryView, error) {
	la, err := s.logicals.GetByKey(ctx, logicalAccountKey)
	if err != nil {
		return nil, err
	}
	if la == nil {
		return nil, fmt.Errorf("%w: %s", model.ErrLogicalAccountNotRegistered, logicalAccountKey)
	}
	instances, err := s.accounts.ListByLogical(ctx, la.ID, 1000)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}

	rows := make([]*InstanceHistoryRow, 0, len(instances))
	phaseCounts := map[string]int{}
	allZero := true
	var totalBalance int64

	for _, inst := range instances {
		row := &InstanceHistoryRow{
			AccountNo:               inst.AccountNo,
			LifecyclePhase:          int8(inst.LifecyclePhase),
			LifecyclePhaseName:      inst.LifecyclePhase.String(),
			Balance:                 inst.Balance,
			IsZero:                  inst.Balance == 0,
			FrozenBalance:           inst.FrozenBalance,
			AvailableBalance:        inst.AvailableBalance,
			Currency:                inst.Currency,
			PeriodStart:             inst.PeriodStart,
			PeriodEnd:               inst.PeriodEnd,
			DrainingStartedAt:       inst.DrainingStartedAt,
			FrozenAt:                inst.FrozenAt,
			ArchivedAt:              inst.ArchivedAt,
			PolicyVersionAtBirth:    inst.PolicyVersionAtBirth,
			EffectiveHardTimeoutSec: inst.EffectiveHardTimeoutSecs,
			OverrideReason:          inst.OverrideReason,
			OverrideBy:              inst.OverrideBy,
			Version:                 inst.Version,
		}
		rows = append(rows, row)
		phaseCounts[row.LifecyclePhaseName]++
		totalBalance += inst.Balance
		if inst.Balance != 0 {
			allZero = false
		}
	}

	view := &InstanceHistoryView{
		LogicalAccountID:  la.ID,
		LogicalAccountKey: la.LogicalAccountKey,
		Currency:          la.Currency,
		RotationEnabled:   la.IsRotating(),
		Instances:         rows,
		PhaseCounts:       phaseCounts,
		TotalBalance:      totalBalance,
		AllInstancesZero:  allZero,
	}
	return view, nil
}

// InstanceHistoryView 详情页聚合视图。
type InstanceHistoryView struct {
	LogicalAccountID  int64                 `json:"logical_account_id"`
	LogicalAccountKey string                `json:"logical_account_key"`
	Currency          string                `json:"currency"`
	RotationEnabled   bool                  `json:"rotation_enabled"`
	Instances         []*InstanceHistoryRow `json:"instances"`
	PhaseCounts       map[string]int        `json:"phase_counts"`
	TotalBalance      int64                 `json:"total_balance"`
	AllInstancesZero  bool                  `json:"all_instances_zero"`
}

// ============================================================================
// 3. 手动操作：force-switch / force-provision
// ============================================================================

// ManualSwitchRequest 手动切换参数。
type ManualSwitchRequest struct {
	LogicalAccountKey string
	Operator          string // 必填，审计
	Reason            string // 必填，审计
}

// ManualSwitch admin-web "立即切换" 按钮入口。
// 流程：
//   1. 校验入参（operator/reason 必填）
//   2. 找到 logical_account
//   3. 调用 scheduler.ForceSwitch（已经包含锁 + 双重校验）
func (s *AdminService) ManualSwitch(ctx context.Context, req ManualSwitchRequest) error {
	if req.Operator == "" || req.Reason == "" {
		return errors.New("ManualSwitch: operator and reason are required for audit")
	}
	la, err := s.logicals.GetByKey(ctx, req.LogicalAccountKey)
	if err != nil {
		return err
	}
	if la == nil {
		return fmt.Errorf("%w: %s", model.ErrLogicalAccountNotRegistered, req.LogicalAccountKey)
	}
	if !la.IsRotating() {
		return errors.New("ManualSwitch: rotation_enabled=0; nothing to switch")
	}
	return s.scheduler.ForceSwitch(ctx, la.ID, req.Operator, req.Reason)
}

// ManualProvision admin-web "立即预创建下一期" 按钮入口。
// 用于 scheduler 跑得晚 / 故障恢复后预先建好下一期。
func (s *AdminService) ManualProvision(ctx context.Context, req ManualSwitchRequest) error {
	if req.Operator == "" || req.Reason == "" {
		return errors.New("ManualProvision: operator and reason are required for audit")
	}
	la, err := s.logicals.GetByKey(ctx, req.LogicalAccountKey)
	if err != nil {
		return err
	}
	if la == nil {
		return fmt.Errorf("%w: %s", model.ErrLogicalAccountNotRegistered, req.LogicalAccountKey)
	}
	if !la.IsRotating() {
		return errors.New("ManualProvision: rotation_enabled=0")
	}
	return s.scheduler.ForceProvision(ctx, la.ID, req.Operator, req.Reason)
}

// ============================================================================
// 4. 单 instance 详情（admin-web 详情页）
// ============================================================================

// InstanceDetail 单 instance 完整详情 + 上下文。
type InstanceDetail struct {
	*InstanceHistoryRow
	LogicalAccountKey string `json:"logical_account_key"`
	LogicalAccountID  int64  `json:"logical_account_id"`
}

// GetInstanceDetail 单 instance 详情。
func (s *AdminService) GetInstanceDetail(ctx context.Context, accountNo string) (*InstanceDetail, error) {
	if accountNo == "" {
		return nil, errors.New("account_no required")
	}
	inst, err := s.accounts.GetByAccountNo(ctx, accountNo)
	if err != nil {
		return nil, fmt.Errorf("get account: %w", err)
	}
	if inst == nil {
		return nil, fmt.Errorf("account %s not found", accountNo)
	}

	detail := &InstanceDetail{
		InstanceHistoryRow: &InstanceHistoryRow{
			AccountNo:               inst.AccountNo,
			LifecyclePhase:          int8(inst.LifecyclePhase),
			LifecyclePhaseName:      inst.LifecyclePhase.String(),
			Balance:                 inst.Balance,
			IsZero:                  inst.Balance == 0,
			FrozenBalance:           inst.FrozenBalance,
			AvailableBalance:        inst.AvailableBalance,
			Currency:                inst.Currency,
			PeriodStart:             inst.PeriodStart,
			PeriodEnd:               inst.PeriodEnd,
			DrainingStartedAt:       inst.DrainingStartedAt,
			FrozenAt:                inst.FrozenAt,
			ArchivedAt:              inst.ArchivedAt,
			PolicyVersionAtBirth:    inst.PolicyVersionAtBirth,
			EffectiveHardTimeoutSec: inst.EffectiveHardTimeoutSecs,
			OverrideReason:          inst.OverrideReason,
			OverrideBy:              inst.OverrideBy,
			Version:                 inst.Version,
		},
	}
	if inst.LogicalAccountID != nil {
		detail.LogicalAccountID = *inst.LogicalAccountID
		la, _ := s.logicals.GetByID(ctx, *inst.LogicalAccountID)
		if la != nil {
			detail.LogicalAccountKey = la.LogicalAccountKey
		}
	}
	return detail, nil
}

// ============================================================================
// 6. 注册新 LogicalAccount — admin-web "创建 LA" 表单后端
// ============================================================================

// RegisterLogicalAccountRequest "创建 LA" 表单参数。
//
// 字段含义：
//   - LogicalAccountKey: 业务侧稳定 key，必须以 model.AllowedKeyPrefixes 之一开头
//   - AccountType / AccountBusinessType: 见 model.AccountType / AccountBusinessType
//   - Currency: ISO-4217 3 字母（USD/PHP/CNY/...）
//   - Description: 可选人类可读描述
//   - RotationEnabled: true → 启用轮换；false → 跑 legacy 单 instance 路径
//   - Operator: 必填，写入 registered_by 字段做审计
type RegisterLogicalAccountRequest struct {
	LogicalAccountKey   string `json:"logical_account_key"`
	AccountType         int8   `json:"account_type"`
	AccountBusinessType int8   `json:"account_business_type"`
	Currency            string `json:"currency"`
	Description         string `json:"description,omitempty"`
	RotationEnabled     bool   `json:"rotation_enabled"`
	Operator            string `json:"operator"`
}

// GetBalanceSummary 取 LA 维度的余额聚合（用于对账接口 + admin UI）。
// 通过 logical_account_key 或 logical_account_id 任一查询；优先 key（用户友好）。
func (s *AdminService) GetBalanceSummary(
	ctx context.Context, logicalAccountKey string, logicalAccountID int64,
) (LogicalAccountBalanceSummary, error) {
	var la *model.LogicalAccount
	var err error
	switch {
	case logicalAccountKey != "":
		la, err = s.logicals.GetByKey(ctx, logicalAccountKey)
	case logicalAccountID > 0:
		la, err = s.logicals.GetByID(ctx, logicalAccountID)
	default:
		return LogicalAccountBalanceSummary{}, errors.New("must provide logical_account_key or logical_account_id")
	}
	if err != nil {
		return LogicalAccountBalanceSummary{}, fmt.Errorf("lookup LA: %w", err)
	}
	if la == nil {
		return LogicalAccountBalanceSummary{}, errors.New("logical_account not found")
	}
	summary, err := s.accounts.SumBalanceByLogical(ctx, la.ID)
	if err != nil {
		return LogicalAccountBalanceSummary{}, fmt.Errorf("sum balance: %w", err)
	}
	summary.LogicalAccountKey = la.LogicalAccountKey
	summary.Currency = la.Currency
	return summary, nil
}

// RegisterLogicalAccount 注册一个新 LA。
// 验证由 repo.Register 完成（前缀白名单、unique key 冲突、必填字段）。
// 返回完整的新 LA（含 id / created_at / 默认值）。
func (s *AdminService) RegisterLogicalAccount(
	ctx context.Context, req RegisterLogicalAccountRequest,
) (*model.LogicalAccount, error) {
	if s.registrar == nil {
		return nil, errors.New("logical_account registrar not wired in this build")
	}
	if req.Operator == "" {
		return nil, errors.New("operator required (审计字段不能空)")
	}
	rotationEnabled := int8(0)
	if req.RotationEnabled {
		rotationEnabled = 1
	}
	var desc *string
	if req.Description != "" {
		d := req.Description
		desc = &d
	}
	la := &model.LogicalAccount{
		LogicalAccountKey:   req.LogicalAccountKey,
		AccountType:         model.AccountType(req.AccountType),
		AccountBusinessType: model.AccountBusinessType(req.AccountBusinessType),
		Currency:            req.Currency,
		Description:         desc,
		RotationEnabled:     rotationEnabled,
		Status:              model.LogicalAccountStatusEnabled,
		RegisteredBy:        req.Operator,
	}
	created, err := s.registrar.Register(ctx, la)
	if err != nil {
		return nil, err
	}

	// rotation_enabled=true 时自动建默认 policy，避免 ForceProvision 报 no policy。
	// 默认参数：MONTH 周期 / UTC 时区 / 3 天 P99 drain / 7 天 hard timeout / 1 天 provision lead
	// 运维要改 policy 现阶段直接 mysql UPDATE（未来加 UI 编辑入口）。
	if req.RotationEnabled {
		policy := &model.LogicalAccountRotationPolicy{
			LogicalAccountID:     created.ID,
			PeriodUnit:           model.PeriodUnitMonth,
			PeriodCount:          1,
			RotationAnchorTZ:     "UTC",
			DrainP99Seconds:      3 * 24 * 3600,  // 3 天
			DrainHardTimeoutSecs: 7 * 24 * 3600,  // 7 天
			ArchiveGraceSecs:     7 * 24 * 3600,  // 7 天
			ProvisionLeadSecs:    24 * 3600,      // 1 天
			ConfigVersion:        1,
			EffectiveFrom:        s.clock(),
		}
		if err := s.registrar.UpsertPolicy(ctx, policy); err != nil {
			// LA 已建，policy 失败不回滚（caller 可以重试 register 拿幂等返回 + 手动建 policy）
			return created, fmt.Errorf("LA created but default policy failed: %w", err)
		}
	}
	return created, nil
}

// ============================================================================
// 7. Fleet 路由 demo —— /admin/rotation/resolve-fleet-sub + /admin/rotation/fleet-book
//
// 给运维 / 演示用的两个端点：
//   1. ResolveFleetSubAccount：给定 LA key + flow_id，返回 fleet routing 选中的
//      sub-account（含 account_no, sub_idx, group, phase），便于排查"为啥这单写到了
//      这个 sub-account 上"。
//   2. FleetTestBook：在 admin 后端直接发起一次双分录记账，绕过 gRPC，源账户走 fleet
//      routing 选中的 sub-account；演示从 LA 抽象到具体 sub-account 的端到端链路。
// ============================================================================

// FleetSubResolution fleet 路由结果。
type FleetSubResolution struct {
	LogicalAccountID  int64  `json:"logical_account_id"`
	LogicalAccountKey string `json:"logical_account_key"`
	FlowID            string `json:"flow_id"`
	SubIdx            int    `json:"sub_idx"`            // hash(flow_id) % 100
	AccountNo         string `json:"account_no"`
	AccountGroup      string `json:"account_group"`      // "A" / "B"
	LifecyclePhase    int8   `json:"lifecycle_phase"`
	LifecyclePhaseStr string `json:"lifecycle_phase_str"`
	Balance           int64  `json:"balance"`
	Currency          string `json:"currency"`
}

// ResolveFleetSubAccount 复刻 rotation_router.go 里 fleet branch 的逻辑，返回选中的 sub-account。
// 用途：admin UI / 运维排障 / e2e demo。
//
// 算法（必须跟 rotation_router.Resolve 保持一致）：
//   1. sub_idx = fnv32a(flow_id) % 100
//   2. 在 LA 的 100 个 active sub 中找 user_id=sub_idx 那个
//   3. 找不到（rotation 中、未建好）→ 报错（生产代码会 fallback 到 anchor，这里 demo
//      返回明确错误便于运维定位）
func (s *AdminService) ResolveFleetSubAccount(
	ctx context.Context, logicalAccountKey, flowID string,
) (*FleetSubResolution, error) {
	if logicalAccountKey == "" || flowID == "" {
		return nil, errors.New("logical_account_key and flow_id required")
	}
	la, err := s.logicals.GetByKey(ctx, logicalAccountKey)
	if err != nil {
		return nil, fmt.Errorf("lookup LA: %w", err)
	}
	if la == nil {
		return nil, fmt.Errorf("%w: %s", model.ErrLogicalAccountNotRegistered, logicalAccountKey)
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(flowID))
	subIdx := int(h.Sum32() % 100)

	sub, err := s.accounts.GetActiveSubAccount(ctx, la.ID, subIdx)
	if err != nil {
		return nil, fmt.Errorf("GetActiveSubAccount(la=%d, sub=%d): %w", la.ID, subIdx, err)
	}
	if sub == nil {
		return nil, fmt.Errorf("no active sub at idx=%d for LA %s (fleet not provisioned or rotation in progress)",
			subIdx, logicalAccountKey)
	}

	return &FleetSubResolution{
		LogicalAccountID:  la.ID,
		LogicalAccountKey: la.LogicalAccountKey,
		FlowID:            flowID,
		SubIdx:            subIdx,
		AccountNo:         sub.AccountNo,
		AccountGroup:      sub.AccountGroup,
		LifecyclePhase:    int8(sub.LifecyclePhase),
		LifecyclePhaseStr: sub.LifecyclePhase.String(),
		Balance:           sub.Balance,
		Currency:          sub.Currency,
	}, nil
}

// FleetTestBookRequest admin 测试用的简化双分录入参。
//
// 业务字段：
//   - SrcLogicalAccountKey: 源 LA（走 fleet routing 选中 sub-account 作 Debit 方）
//   - SrcAccountNo: 直填源 account_no（跟 SrcLogicalAccountKey 二选一）
//   - DstAccountNo: 目标 account_no（直接给，不走 fleet routing，便于 demo 简化；Credit 方）
//   - Amount: 金额（最小单位）；Debit src，Credit dst
//   - Currency: ISO-4217 3 字母
//   - FlowID: 用作 RequestID（幂等键）+ BusinessNo + fleet routing hash
//   - BusinessType: BusinessType 字符串，对应 model.BusinessType*
//   - Operator: 必填，作为 description 一部分写入审计
type FleetTestBookRequest struct {
	SrcLogicalAccountKey string `json:"src_logical_account_key,omitempty"`
	SrcAccountNo         string `json:"src_account_no,omitempty"`
	DstAccountNo         string `json:"dst_account_no"`
	Amount               int64  `json:"amount"`
	Currency             string `json:"currency"`
	FlowID               string `json:"flow_id"`
	BusinessType         string `json:"business_type"`
	Operator             string `json:"operator"`
}

// FleetTestBookResponse 记账结果 + fleet routing 过程信息。
type FleetTestBookResponse struct {
	VoucherNo      string              `json:"voucher_no"`
	TransactionIDs []string            `json:"transaction_ids"`
	SrcResolution  *FleetSubResolution `json:"src_resolution,omitempty"` // fleet routing 选中的 sub
	BookingTime    time.Time           `json:"booking_time"`
}

// FleetTestBook 端到端 demo：fleet routing → 双分录记账。
//
// 流程：
//   1. 校验入参（operator/flow_id 必填；amount > 0）
//   2. 解析源账户：
//      a. 给了 src_logical_account_key → 走 fleet routing → 返回 sub_account_no
//      b. 给了 src_account_no → 直接用
//   3. 构造 DoubleEntryBookingRequest，调 booker.DoubleEntryBooking
//   4. 返回 voucher_no + transaction_ids + 路由过程信息
//
// 注意：这是 admin demo 端点，不做生产级幂等（依赖 booking_request_no = flow_id 兜底）。
func (s *AdminService) FleetTestBook(
	ctx context.Context, req FleetTestBookRequest,
) (*FleetTestBookResponse, error) {
	if s.booker == nil {
		return nil, errors.New("booking invoker not wired in this build")
	}
	if req.Operator == "" {
		return nil, errors.New("operator required (审计字段不能空)")
	}
	if req.FlowID == "" {
		return nil, errors.New("flow_id required (用于幂等 + fleet routing hash)")
	}
	if req.Amount <= 0 {
		return nil, errors.New("amount must be > 0")
	}
	if req.DstAccountNo == "" {
		return nil, errors.New("dst_account_no required")
	}
	if req.SrcLogicalAccountKey == "" && req.SrcAccountNo == "" {
		return nil, errors.New("must provide src_logical_account_key or src_account_no")
	}

	resp := &FleetTestBookResponse{BookingTime: s.clock()}

	srcAccountNo := req.SrcAccountNo
	if req.SrcLogicalAccountKey != "" {
		resolved, err := s.ResolveFleetSubAccount(ctx, req.SrcLogicalAccountKey, req.FlowID)
		if err != nil {
			return nil, fmt.Errorf("resolve fleet src: %w", err)
		}
		resp.SrcResolution = resolved
		srcAccountNo = resolved.AccountNo
	}

	bizType := model.BusinessType(req.BusinessType)
	if bizType == "" {
		bizType = model.BusinessTypeTransfer
	}
	bookReq := &DoubleEntryBookingRequest{
		RequestID:    req.FlowID, // 幂等键
		BusinessNo:   req.FlowID,
		BusinessType: bizType,
		Currency:     req.Currency,
		Description:  fmt.Sprintf("fleet-test-book via admin (operator=%s)", req.Operator),
		Entries: []AccountingEntry{
			{AccountNo: srcAccountNo, DebitAmount: req.Amount, Description: "fleet src"},
			{AccountNo: req.DstAccountNo, CreditAmount: req.Amount, Description: "fleet dst"},
		},
	}
	voucherNo, txIDs, err := s.booker.DoubleEntryBooking(ctx, bookReq)
	if err != nil {
		return nil, fmt.Errorf("double entry booking: %w", err)
	}
	resp.VoucherNo = voucherNo
	resp.TransactionIDs = txIDs
	return resp, nil
}
