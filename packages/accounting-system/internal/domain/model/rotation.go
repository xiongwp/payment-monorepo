package model

import (
	"errors"
	"fmt"
	"time"
)

// ============================================================================
// Rotating Suspense / Receivable / Payable Accounts — Domain Model
//
// 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md
//
// 这一层只包含纯数据模型与状态转换合法性表，不依赖任何 infra / repo / 时钟。
// 所有 IO 行为放在 repository 与 service 层。
// ============================================================================

// LifecyclePhase Account Instance 在轮换链中的位置。
//
// 与 AccountStatus 正交：status 控制 "能否被业务使用（启停冻）"，phase 控制
// "在轮换链中处于哪一段（active / draining / frozen / archived / quarantined）"。
type LifecyclePhase int8

const (
	// LifecyclePhaseLegacy 旧账户，未参与轮换（rotation_enabled=0 的 logical 下的
	// 唯一 instance）。等价于 "永久 active"。写入约束与历史行为一致。
	LifecyclePhaseLegacy LifecyclePhase = 0

	// LifecyclePhaseProvisioned 预创建：由 scheduler 在 period_end - lead_time
	// 提前建好；尚未进入 active 接收流量。
	LifecyclePhaseProvisioned LifecyclePhase = 5

	// LifecyclePhaseActive 当期：接收所有新交易锚定 + 既有交易后续分录。
	LifecyclePhaseActive LifecyclePhase = 1

	// LifecyclePhaseDraining 排空中：拒绝新锚定，只接受已锚定交易的后续分录。
	LifecyclePhaseDraining LifecyclePhase = 2

	// LifecyclePhaseFrozen 冻结：任何写入禁止；可读；等待 archive_grace 后归档。
	// 例外：TCC Cancel 在 anchor.status=trying 时可写入（§5.6 / E-16 / E-49）。
	LifecyclePhaseFrozen LifecyclePhase = 3

	// LifecyclePhaseArchived 归档：只读；可能转冷存储。路由层绝不命中。
	LifecyclePhaseArchived LifecyclePhase = 4

	// LifecyclePhaseQuarantined 隔离：因不变量违反 / 异常进入，需人工介入。
	// 可从任意 phase 转入；解除时根据收敛 job 结果回到 draining 或 frozen。
	LifecyclePhaseQuarantined LifecyclePhase = 9
)

// String returns the human-readable phase name. Used for error messages and logs.
func (p LifecyclePhase) String() string {
	switch p {
	case LifecyclePhaseLegacy:
		return "legacy"
	case LifecyclePhaseProvisioned:
		return "provisioned"
	case LifecyclePhaseActive:
		return "active"
	case LifecyclePhaseDraining:
		return "draining"
	case LifecyclePhaseFrozen:
		return "frozen"
	case LifecyclePhaseArchived:
		return "archived"
	case LifecyclePhaseQuarantined:
		return "quarantined"
	default:
		return fmt.Sprintf("unknown(%d)", int8(p))
	}
}

// IsTerminal returns true if no automatic phase transition is allowed from this
// phase (only manual ops can reverse archived; archived itself is final-final).
func (p LifecyclePhase) IsTerminal() bool {
	return p == LifecyclePhaseArchived
}

// AcceptsNewAnchoring 写路径关键判定：本 phase 是否允许接受新交易首次锚定。
// 见 §5.2 步骤 2c。
func (p LifecyclePhase) AcceptsNewAnchoring() bool {
	return p == LifecyclePhaseActive
}

// AcceptsFollowupPosting 写路径关键判定：对既有锚点 anchor 的后续分录是否允许写入。
// 注意：frozen 上的 TCC Cancel 例外不在本函数判定，由 router 单独走 §5.6 路径。
func (p LifecyclePhase) AcceptsFollowupPosting() bool {
	return p == LifecyclePhaseActive || p == LifecyclePhaseDraining
}

// AllowedPhaseTransitions instance phase 合法转换表。
// 任何不在表中的 (from, to) 都是非法。
//
// 转换守卫细化由 service 层在事务内附加（CAS 乐观锁 + 业务规则）。
var AllowedPhaseTransitions = map[LifecyclePhase][]LifecyclePhase{
	LifecyclePhaseProvisioned:  {LifecyclePhaseActive, LifecyclePhaseQuarantined},
	LifecyclePhaseActive:       {LifecyclePhaseDraining, LifecyclePhaseQuarantined},
	LifecyclePhaseDraining:     {LifecyclePhaseFrozen, LifecyclePhaseQuarantined},
	LifecyclePhaseFrozen:       {LifecyclePhaseArchived, LifecyclePhaseQuarantined},
	LifecyclePhaseArchived:     {LifecyclePhaseQuarantined}, // 极端：归档后发现不一致，需人工拉回
	LifecyclePhaseQuarantined:  {LifecyclePhaseDraining, LifecyclePhaseFrozen}, // 人工解除后由收敛 job 决定回归态
	LifecyclePhaseLegacy:       {LifecyclePhaseQuarantined}, // legacy 一般不参与转换，但保留隔离逃逸口
}

// CanTransitionPhase 检查 phase 转换是否合法（纯函数，不涉及业务守卫）。
//
// 业务守卫示例（不在本函数内）：
//   - active→draining：必须先有新 active 就位
//   - draining→frozen：必须满足 §7.1 全部收敛指标
//   - frozen→archived：archive_grace 已过 + balance=0
//
// 这些守卫由 service 层在事务内附加。本函数只检查"图上是否有边"。
func CanTransitionPhase(from, to LifecyclePhase) bool {
	if from == to {
		return false // 不允许自环（无意义 + 易掩盖 bug）
	}
	allowed, ok := AllowedPhaseTransitions[from]
	if !ok {
		return false
	}
	for _, p := range allowed {
		if p == to {
			return true
		}
	}
	return false
}

// ErrIllegalPhaseTransition phase 转换图上不存在该边。
var ErrIllegalPhaseTransition = errors.New("illegal lifecycle phase transition")

// ValidatePhaseTransition 返回包含上下文的错误，便于上层包装。
func ValidatePhaseTransition(from, to LifecyclePhase) error {
	if !CanTransitionPhase(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrIllegalPhaseTransition, from, to)
	}
	return nil
}

// ============================================================================
// Anchor — 交易在 logical_account 上的锚点
// ============================================================================

// AnchorStatus tx_account_anchor.status 字段取值。详见 §3.4.1。
type AnchorStatus int8

const (
	// AnchorStatusTrying 已建锚但首笔分录还是 TCC Try 中（未确认/未取消）。
	AnchorStatusTrying AnchorStatus = 0

	// AnchorStatusActive 已有终态分录但业务未声明结算完成。仍可有后续分录（红冲、调整、清算）。
	AnchorStatusActive AnchorStatus = 1

	// AnchorStatusSettled 业务声明结算完成；不再期望任何后续分录。终态。
	AnchorStatusSettled AnchorStatus = 2

	// AnchorStatusMigrated 已发生强制迁移，请按 migrated_to_account_no 路由。
	AnchorStatusMigrated AnchorStatus = 3

	// AnchorStatusStuck 自动重试耗尽，等待人工处理。block 收敛 job 推进。
	AnchorStatusStuck AnchorStatus = 4
)

// String returns human-readable anchor status name.
func (s AnchorStatus) String() string {
	switch s {
	case AnchorStatusTrying:
		return "trying"
	case AnchorStatusActive:
		return "active"
	case AnchorStatusSettled:
		return "settled"
	case AnchorStatusMigrated:
		return "migrated"
	case AnchorStatusStuck:
		return "stuck"
	default:
		return fmt.Sprintf("unknown(%d)", int8(s))
	}
}

// IsOpen "open" 的统一定义（§3.4.1）。被收敛 job 使用判断 instance 是否可推进。
//
// 注意：migrated 不算 open（已迁移至新 instance）；stuck 也不算 open，但 instance
// 有 stuck > 0 时禁止推进到 frozen，必须先 quarantined（见 §7.1）。
func (s AnchorStatus) IsOpen() bool {
	return s == AnchorStatusTrying || s == AnchorStatusActive
}

// IsTerminal 终态：settled / migrated。stuck 不算终态（等待人工）。
func (s AnchorStatus) IsTerminal() bool {
	return s == AnchorStatusSettled || s == AnchorStatusMigrated
}

// AllowedAnchorTransitions anchor status 合法转换表。详见 §3.4.1。
//
//   trying  -> active   (任一 transaction 翻为 success)
//   trying  -> settled  (全部 transaction 翻为 failed/cancelled — TCC Cancel 完成)
//   trying  -> stuck    (Try 阶段卡死，超过 hard timeout)
//   active  -> settled  (业务显式 SettleAnchor 或全部 TCC 分支终态)
//   active  -> migrated (§8 强制迁移完成)
//   active  -> stuck    (自动重试耗尽)
//
// settled / migrated 是终态，不再转换。
// stuck 只能通过运维手动转出（一般转 quarantined 路径处理后置回 active）。
var AllowedAnchorTransitions = map[AnchorStatus][]AnchorStatus{
	AnchorStatusTrying: {AnchorStatusActive, AnchorStatusSettled, AnchorStatusStuck},
	AnchorStatusActive: {AnchorStatusSettled, AnchorStatusMigrated, AnchorStatusStuck},
}

// ErrIllegalAnchorTransition anchor 转换图上不存在该边。
var ErrIllegalAnchorTransition = errors.New("illegal anchor status transition")

// CanTransitionAnchor 检查 anchor.status 转换是否合法。
func CanTransitionAnchor(from, to AnchorStatus) bool {
	if from == to {
		return false
	}
	allowed, ok := AllowedAnchorTransitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// ValidateAnchorTransition 同上但返回带上下文的错误。
func ValidateAnchorTransition(from, to AnchorStatus) error {
	if !CanTransitionAnchor(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrIllegalAnchorTransition, from, to)
	}
	return nil
}

// AnchorReuseSource anchor 复用来源（§5.5）。
//
// 复用规则：refund / reverse posting 不共享 anchor 行，而是用本请求的
// related_request_id 新建 anchor 行，account_no 拷自被复用 anchor，
// reuse_source / reuse_source_anchor_id 填入用于审计。
type AnchorReuseSource int8

const (
	// AnchorReuseSourcePrimary 本请求自身首次锚定（默认）。
	AnchorReuseSourcePrimary AnchorReuseSource = 0

	// AnchorReuseSourceRefundOf 退款引用原支付的 related_request_id_root。
	AnchorReuseSourceRefundOf AnchorReuseSource = 1

	// AnchorReuseSourceReverseOf 红冲引用原 transaction_id（错账冲销）。
	AnchorReuseSourceReverseOf AnchorReuseSource = 2
)

// String returns human-readable reuse source name.
func (s AnchorReuseSource) String() string {
	switch s {
	case AnchorReuseSourcePrimary:
		return "primary"
	case AnchorReuseSourceRefundOf:
		return "refund-of"
	case AnchorReuseSourceReverseOf:
		return "reverse-of"
	default:
		return fmt.Sprintf("unknown(%d)", int8(s))
	}
}

// AnchorDirectionMask bit0=曾借记 bit1=曾贷记。便于审计 anchor 在 instance 上
// 的资金流方向。
type AnchorDirectionMask int8

const (
	AnchorDirectionDebit  AnchorDirectionMask = 1 // bit 0
	AnchorDirectionCredit AnchorDirectionMask = 2 // bit 1
)

// HasDebit 是否已有借方分录写入。
func (m AnchorDirectionMask) HasDebit() bool { return m&AnchorDirectionDebit != 0 }

// HasCredit 是否已有贷方分录写入。
func (m AnchorDirectionMask) HasCredit() bool { return m&AnchorDirectionCredit != 0 }

// WithDebit 返回追加借方标记后的 mask（不修改原值）。
func (m AnchorDirectionMask) WithDebit() AnchorDirectionMask { return m | AnchorDirectionDebit }

// WithCredit 返回追加贷方标记后的 mask（不修改原值）。
func (m AnchorDirectionMask) WithCredit() AnchorDirectionMask { return m | AnchorDirectionCredit }

// ============================================================================
// LogicalAccount — 跨周期稳定的逻辑账户
// ============================================================================

// LogicalAccount 一个业务侧稳定的"账户"，跨周期不变。多个 Account instance
// 在不同周期承接其流量。
//
// 表：logical_account（全局表，存在 account_meta 库；不分片）
// 路由层缓存 5s；scheduler 切换时原子更新 current_active_account_no。
type LogicalAccount struct {
	ID                     int64               `db:"id"                       gorm:"column:id;primaryKey"                              json:"id"`
	LogicalAccountKey      string              `db:"logical_account_key"      gorm:"column:logical_account_key;uniqueIndex:uk_lak"     json:"logical_account_key"`
	AccountType            AccountType         `db:"account_type"             gorm:"column:account_type"                               json:"account_type"`
	AccountBusinessType    AccountBusinessType `db:"account_business_type"    gorm:"column:account_business_type"                      json:"account_business_type"`
	Currency               string              `db:"currency"                 gorm:"column:currency;type:char(3)"                      json:"currency"`
	Description            *string             `db:"description"              gorm:"column:description"                                json:"description,omitempty"`
	RotationEnabled        int8                `db:"rotation_enabled"         gorm:"column:rotation_enabled;default:0"                 json:"rotation_enabled"`
	// CurrentActiveAccountNo 反范式化字段：当期 active 的 account_no。
	// 写入唯一入口：rotation scheduler 的切换事务（见 §5.2.1）。
	// 路由层热路径只查本表即可，避免跨片 account 表 scan。
	CurrentActiveAccountNo *string             `db:"current_active_account_no" gorm:"column:current_active_account_no"                 json:"current_active_account_no,omitempty"`
	// CurrentActivePeriodEnd 当期 active 的 period_end。路由层用它判断"是否在边界附近"
	// 触发预读下一期 active。
	CurrentActivePeriodEnd *time.Time          `db:"current_active_period_end" gorm:"column:current_active_period_end"                 json:"current_active_period_end,omitempty"`
	Status                 int8                `db:"status"                   gorm:"column:status;default:1"                           json:"status"`
	// RegisteredBy 注册者（人/系统），必填。审计用，禁止 lazy create（§3.1）。
	RegisteredBy           string              `db:"registered_by"            gorm:"column:registered_by"                              json:"registered_by"`
	CreatedAt              time.Time           `db:"created_at"               gorm:"column:created_at"                                 json:"created_at"`
	UpdatedAt              time.Time           `db:"updated_at"               gorm:"column:updated_at"                                 json:"updated_at"`
	Version                int64               `db:"version"                  gorm:"column:version"                                    json:"version"`
}

// TableName GORM 表名约定。
func (LogicalAccount) TableName() string { return "logical_account" }

// IsRotating 是否启用轮换。rotation_enabled=0 走 legacy 路径，与既有逻辑等价。
func (la *LogicalAccount) IsRotating() bool { return la.RotationEnabled == 1 }

// IsEnabled 业务启停。
func (la *LogicalAccount) IsEnabled() bool { return la.Status == 1 }

// LogicalAccountStatus 取值。
const (
	LogicalAccountStatusDisabled int8 = 0
	LogicalAccountStatusEnabled  int8 = 1
)

// AllowedKeyPrefixes logical_account_key 命名空间白名单（§3.1 防 typo 创建幽灵账户）。
// 注册接口 RegisterLogicalAccount 必须校验前缀。
var AllowedKeyPrefixes = []string{
	"transit:",            // 通用中间账户 (account_type=9)
	"channel-payable:",    // 渠道应付款  (account_type=6)
	"channel-receivable:", // 渠道应收款  (account_type=5)
}

// ValidateLogicalAccountKey 检查 key 是否符合命名规范。
// 规则：必须以 AllowedKeyPrefixes 之一开头；长度 ∈ [8, 64]；ASCII 可打印字符。
func ValidateLogicalAccountKey(key string) error {
	if l := len(key); l < 8 || l > 64 {
		return fmt.Errorf("logical_account_key length out of range [8,64]: got %d", l)
	}
	for _, c := range key {
		if c < 0x21 || c > 0x7E {
			return fmt.Errorf("logical_account_key contains non-ASCII or whitespace: %q", key)
		}
	}
	for _, prefix := range AllowedKeyPrefixes {
		if hasPrefix(key, prefix) {
			return nil
		}
	}
	return fmt.Errorf("logical_account_key does not match any allowed prefix %v: %q", AllowedKeyPrefixes, key)
}

// hasPrefix 不引入 strings 包以降低 domain model 的依赖面（保持纯）。
func hasPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if s[i] != prefix[i] {
			return false
		}
	}
	return true
}

// ============================================================================
// LogicalAccountRotationPolicy — 周期与超时配置
// ============================================================================

// PeriodUnit 周期粒度。
type PeriodUnit string

const (
	PeriodUnitDay     PeriodUnit = "DAY"     // 仅用于测试 / 短周期场景
	PeriodUnitMonth   PeriodUnit = "MONTH"   // 默认（应付/应收）
	PeriodUnitQuarter PeriodUnit = "QUARTER" // 通用中间账户
)

// IsValid 校验 PeriodUnit 取值合法。
func (u PeriodUnit) IsValid() bool {
	switch u {
	case PeriodUnitDay, PeriodUnitMonth, PeriodUnitQuarter:
		return true
	default:
		return false
	}
}

// LogicalAccountRotationPolicy 单个 logical_account 的轮换策略。
//
// 表：logical_account_rotation_policy（全局表）。
type LogicalAccountRotationPolicy struct {
	LogicalAccountID     int64      `db:"logical_account_id"      gorm:"column:logical_account_id;primaryKey"`
	PeriodUnit           PeriodUnit `db:"period_unit"             gorm:"column:period_unit;type:varchar(8)"`
	PeriodCount          int        `db:"period_count"            gorm:"column:period_count;default:1"`
	RotationAnchorTZ     string     `db:"rotation_anchor_tz"      gorm:"column:rotation_anchor_tz;type:varchar(32)"`
	DrainP99Seconds      int        `db:"drain_p99_seconds"       gorm:"column:drain_p99_seconds"`
	DrainHardTimeoutSecs int        `db:"drain_hard_timeout_secs" gorm:"column:drain_hard_timeout_secs"`
	ArchiveGraceSecs     int        `db:"archive_grace_secs"      gorm:"column:archive_grace_secs;default:604800"`
	ProvisionLeadSecs    int        `db:"provision_lead_secs"     gorm:"column:provision_lead_secs;default:86400"`
	ConfigVersion        int64      `db:"config_version"          gorm:"column:config_version;default:1"`
	EffectiveFrom        time.Time  `db:"effective_from"          gorm:"column:effective_from"`
	UpdatedAt            time.Time  `db:"updated_at"              gorm:"column:updated_at"`
}

// TableName GORM 表名约定。
func (LogicalAccountRotationPolicy) TableName() string { return "logical_account_rotation_policy" }

// Validate 策略合法性校验。
func (p *LogicalAccountRotationPolicy) Validate() error {
	if !p.PeriodUnit.IsValid() {
		return fmt.Errorf("invalid period_unit: %q", p.PeriodUnit)
	}
	if p.PeriodCount <= 0 {
		return fmt.Errorf("period_count must be > 0: got %d", p.PeriodCount)
	}
	if p.RotationAnchorTZ == "" {
		return errors.New("rotation_anchor_tz is required")
	}
	if _, err := time.LoadLocation(p.RotationAnchorTZ); err != nil {
		return fmt.Errorf("invalid rotation_anchor_tz %q: %w", p.RotationAnchorTZ, err)
	}
	if p.DrainP99Seconds <= 0 {
		return errors.New("drain_p99_seconds must be > 0")
	}
	if p.DrainHardTimeoutSecs < p.DrainP99Seconds {
		return fmt.Errorf("drain_hard_timeout_secs (%d) must be >= drain_p99_seconds (%d)",
			p.DrainHardTimeoutSecs, p.DrainP99Seconds)
	}
	if p.ArchiveGraceSecs < 0 {
		return errors.New("archive_grace_secs must be >= 0")
	}
	if p.ProvisionLeadSecs < 0 {
		return errors.New("provision_lead_secs must be >= 0")
	}
	return nil
}

// ============================================================================
// TxAccountAnchor — 交易锚点注册表
// ============================================================================

// TxAccountAnchor 交易在 logical_account 上的锚点。
//
// 表：tx_account_anchor，按 related_request_id 哈希分 100 片（与 account_transaction 对齐）。
//
// 关键不变量（强制）：
//   I-A1 (related_request_id, logical_account_id) 唯一 → uk_req_logical
//   I-A2 同一 anchor 的所有 transaction 必须落在 anchor.account_no 上（或迁移后的 chain 末端）
//   I-A3 status 转换必须经过 CanTransitionAnchor 校验
//
// 见设计文档 §3.4 与 §3.4.1。
type TxAccountAnchor struct {
	ID                   int64               `db:"id"                       gorm:"column:id;primaryKey"`
	RelatedRequestID     string              `db:"related_request_id"       gorm:"column:related_request_id;type:varchar(64);uniqueIndex:uk_req_logical,priority:1"`
	LogicalAccountID     int64               `db:"logical_account_id"       gorm:"column:logical_account_id;uniqueIndex:uk_req_logical,priority:2"`
	AccountNo            string              `db:"account_no"               gorm:"column:account_no;type:varchar(32);index:idx_account_status,priority:1"`
	DirectionMask        AnchorDirectionMask `db:"direction_mask"           gorm:"column:direction_mask"`
	AnchoredAt           time.Time           `db:"anchored_at"              gorm:"column:anchored_at"`
	LastPostingAt        time.Time           `db:"last_posting_at"          gorm:"column:last_posting_at"`
	PostingCount         int                 `db:"posting_count"            gorm:"column:posting_count;default:1"`
	MigratedToAccountNo  *string             `db:"migrated_to_account_no"   gorm:"column:migrated_to_account_no;type:varchar(32)"`
	MigrationVoucherNo   *string             `db:"migration_voucher_no"     gorm:"column:migration_voucher_no;type:varchar(64)"`
	Status               AnchorStatus        `db:"status"                   gorm:"column:status;default:1;index:idx_account_status,priority:2"`
	ReuseSource          AnchorReuseSource   `db:"reuse_source"             gorm:"column:reuse_source;default:0"`
	ReuseSourceAnchorID  *int64              `db:"reuse_source_anchor_id"   gorm:"column:reuse_source_anchor_id"`
	MigrationChainDepth  int8                `db:"migration_chain_depth"    gorm:"column:migration_chain_depth;default:0"`
	CreatedAt            time.Time           `db:"created_at"               gorm:"column:created_at"`
	UpdatedAt            time.Time           `db:"updated_at"               gorm:"column:updated_at"`
	Version              int64               `db:"version"                  gorm:"column:version;default:0"`
}

// TableName GORM 表名约定。
func (TxAccountAnchor) TableName() string { return "tx_account_anchor" }

// IsOpen anchor 仍可能有后续分录写入。被收敛 job 用于 open_anchor_count。
func (a *TxAccountAnchor) IsOpen() bool { return a.Status.IsOpen() }

// IsMigrated anchor 已被强制迁移到新 instance。
func (a *TxAccountAnchor) IsMigrated() bool { return a.Status == AnchorStatusMigrated }

// IsStuck 自动重试耗尽，等待人工处理。
func (a *TxAccountAnchor) IsStuck() bool { return a.Status == AnchorStatusStuck }

// EffectiveAccountNo 路由层使用的"当前应写入的 account_no"：
// 若已迁移则取 migrated_to_account_no，否则取 account_no。
//
// 注意：多代迁移（chain depth > 1）时本函数只返回单跳目标。完整链式追踪由
// router 在 §8.6 中循环跟随完成。
func (a *TxAccountAnchor) EffectiveAccountNo() string {
	if a.IsMigrated() && a.MigratedToAccountNo != nil {
		return *a.MigratedToAccountNo
	}
	return a.AccountNo
}

// ============================================================================
// Account 字段扩展（与 ALTER TABLE 对应）
//
// 这些字段加在现有 Account struct 上需要 account.go 同步修改。本文件在此声明
// 常量与辅助函数，供 service / repo 使用。Account struct 本身的字段在
// Batch 2 的 DDL 提交后由 account.go 增量扩展（保持单一职责，避免本文件膨胀）。
// ============================================================================

// AccountRotationFields 描述 account 表新增的轮换相关字段名（与 DDL 对应）。
// 测试使用，避免硬编码字符串散落。
var AccountRotationFields = struct {
	LogicalAccountID        string
	PeriodStart             string
	PeriodEnd               string
	LifecyclePhase          string
	DrainingStartedAt       string
	FrozenAt                string
	ArchivedAt              string
	PolicyVersionAtBirth    string
	EffectiveHardTimeoutSec string
	OverrideReason          string
	OverrideBy              string
	OverrideAt              string
}{
	LogicalAccountID:        "logical_account_id",
	PeriodStart:             "period_start",
	PeriodEnd:               "period_end",
	LifecyclePhase:          "lifecycle_phase",
	DrainingStartedAt:       "draining_started_at",
	FrozenAt:                "frozen_at",
	ArchivedAt:              "archived_at",
	PolicyVersionAtBirth:    "policy_version_at_birth",
	EffectiveHardTimeoutSec: "effective_hard_timeout_secs",
	OverrideReason:          "override_reason",
	OverrideBy:              "override_by",
	OverrideAt:              "override_at",
}

// ============================================================================
// 错误：路由层可能返回的标准错误
// ============================================================================

var (
	// ErrLogicalAccountNotRegistered 路由层未找到 logical_account（不存在或被禁用）。
	// 见 §3.1：绝不 lazy create。
	ErrLogicalAccountNotRegistered = errors.New("logical_account not registered")

	// ErrNoActiveInstance 当前 logical_account 没有 active phase instance。
	// 见 E-04：scheduler 故障导致 active 没就位；路由层应触发紧急告警 + 业务方重试。
	ErrNoActiveInstance = errors.New("no active instance for logical_account")

	// ErrAnchorOnClosedInstance anchor 指向的 instance 已 frozen/archived/quarantined。
	// 见 E-08：触发紧急迁移路径；router 不应继续写入。
	ErrAnchorOnClosedInstance = errors.New("anchor points to closed instance")

	// ErrLegacyApiOnRotatingAccount 旧记账接口（直接传 account_no）被用于 rotation_enabled=1
	// 的 logical_account。见 E-39。
	ErrLegacyApiOnRotatingAccount = errors.New("legacy booking API used on rotating logical_account")

	// ErrMigrationChainTooDeep anchor 迁移链过深（> 5），可能存在循环或系统性问题。
	// 见 E-50：进入 quarantined 而非继续迁移。
	ErrMigrationChainTooDeep = errors.New("anchor migration chain too deep")

	// ErrPhaseGuardRejected 写入被 phase 守卫拒绝（不是 active/draining 也不是允许的
	// frozen 例外）。见 §5.6。
	ErrPhaseGuardRejected = errors.New("write rejected by lifecycle phase guard")

	// ErrInvalidLogicalAccountKey key 不符合命名规范。
	ErrInvalidLogicalAccountKey = errors.New("invalid logical_account_key")
)

// ============================================================================
// 新增预置 AccountBusinessType
// ============================================================================

const (
	// AccountBusinessTypeMigrationSuspense 跨期强制迁移的过渡科目。
	// 余额恒为 0（旧/新 instance 上对冲）。account_type=9 (Transit)。
	AccountBusinessTypeMigrationSuspense AccountBusinessType = 10

	// AccountBusinessTypeResidualWriteOff 归档时核销小额尾差的损益账户。
	// account_type=4 (Platform P&L)。
	AccountBusinessTypeResidualWriteOff AccountBusinessType = 11

	// AccountBusinessTypeRotationOpsAdjust 人工运维调整入口（独立科目，便于审计）。
	// account_type=4 (Platform P&L)。
	AccountBusinessTypeRotationOpsAdjust AccountBusinessType = 12

	// AccountBusinessTypeRotationCarryforward 跨期结转科目（余额恒为 0）。
	// account_type=2 (Liability)。仅在显式启用 carryforward 时使用（§11 Phase 2）。
	AccountBusinessTypeRotationCarryforward AccountBusinessType = 13
)
