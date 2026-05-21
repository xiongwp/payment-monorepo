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
	accounts  AccountAdminReader
	scheduler SchedulerCommand
	clock     func() time.Time
}

// NewAdminService 构造。
func NewAdminService(
	logicals LogicalAccountAdminReader,
	accounts AccountAdminReader,
	scheduler SchedulerCommand,
	clock func() time.Time,
) *AdminService {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &AdminService{
		logicals:  logicals,
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

		// 反范式化字段直接读
		if la.CurrentActiveAccountNo != nil && *la.CurrentActiveAccountNo != "" {
			row.ActiveAccountNo = *la.CurrentActiveAccountNo

			// 加载 instance 拿余额
			acc, err := s.accounts.GetByAccountNo(ctx, *la.CurrentActiveAccountNo)
			if err == nil && acc != nil {
				row.ActiveAccountBalance = acc.Balance
				row.ActiveIsZero = acc.Balance == 0
				if acc.PeriodStart != nil {
					row.PeriodStart = *acc.PeriodStart
				}
				if acc.PeriodEnd != nil {
					row.PeriodEnd = *acc.PeriodEnd
					row.TimeToEndSeconds = int64(acc.PeriodEnd.Sub(now).Seconds())
				}
			}
		}

		// 检查是否有 provisioned ready
		if la.IsRotating() {
			instances, err := s.accounts.ListByLogical(ctx, la.ID, 50)
			if err == nil {
				for _, inst := range instances {
					if inst.LifecyclePhase == model.LifecyclePhaseProvisioned {
						row.ProvisionedReady = true
						row.ProvisionedAccountNo = inst.AccountNo
						break
					}
				}
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
