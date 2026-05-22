package service

import (
	"context"
	"errors"
	"fmt"
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
	clock     func() time.Time
}

// NewAdminService 构造。registrar 传 nil 时写端点降级（返回 "not wired"），不 panic。
func NewAdminService(
	logicals LogicalAccountAdminReader,
	registrar LogicalAccountAdminRegistrar,
	accounts AccountAdminReader,
	scheduler SchedulerCommand,
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
