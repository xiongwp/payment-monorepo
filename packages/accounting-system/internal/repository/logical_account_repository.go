package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LogicalAccountRepository manages account_meta.logical_account +
// logical_account_rotation_policy.
//
// 全局表（在 account_meta 库），无分片。
//
// 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.1, §3.2, §3.5
type LogicalAccountRepository interface {
	// Register 严格注册：禁止 lazy create（§3.1）。
	//   - logical_account_key 必须符合命名前缀白名单（见 model.ValidateLogicalAccountKey）
	//   - registered_by 必填
	//   - 若 key 已存在则返回 ErrLogicalAccountKeyExists（**不**做幂等覆盖）
	Register(ctx context.Context, la *model.LogicalAccount) (*model.LogicalAccount, error)

	// GetByKey 路由层热路径主要入口。
	//   - 未注册 → 返回 model.ErrLogicalAccountNotRegistered（**绝不**自动创建）
	//   - 已注册但 disabled → 返回 model.ErrLogicalAccountNotRegistered（业务等同未注册）
	GetByKey(ctx context.Context, key string) (*model.LogicalAccount, error)

	// GetByID 按主键 ID 查询。未找到返回 (nil, nil)。
	GetByID(ctx context.Context, id int64) (*model.LogicalAccount, error)

	// UpdateCurrentActive scheduler 切换时原子更新反范式化字段。CAS 乐观锁。
	// version 不匹配返回 ErrLogicalAccountVersionConflict。
	// activeGroup="" 时 current_active_group 不动；显式传 "A"/"B" 时同步切换。
	UpdateCurrentActive(ctx context.Context, id int64, accountNo string, activeGroup string, periodEnd time.Time, expectedVersion int64) error

	// SetRotationEnabled 切换 rotation_enabled。CAS 乐观锁。
	SetRotationEnabled(ctx context.Context, id int64, enabled bool, expectedVersion int64) error

	// ListByPrefix 按 logical_account_key 前缀检索（admin/dashboard 使用）。
	// limit ≤ 0 时默认 100；最大 1000。
	ListByPrefix(ctx context.Context, prefix string, limit int) ([]*model.LogicalAccount, error)

	// ListRotating 列所有 rotation_enabled=1 且 status=enabled 的 LA（scheduler Tick 用）。
	// 顺序：id 升序（确定性）。limit ≤ 0 时默认 1000。
	ListRotating(ctx context.Context, limit int) ([]*model.LogicalAccount, error)

	// --- Rotation policy ---

	// UpsertPolicy 创建或更新轮换策略（按 logical_account_id 主键）。
	// config_version 自动 +1。
	UpsertPolicy(ctx context.Context, p *model.LogicalAccountRotationPolicy) error

	// GetPolicy 取本 logical_account 的当前策略；未配置返回 (nil, nil)。
	GetPolicy(ctx context.Context, logicalAccountID int64) (*model.LogicalAccountRotationPolicy, error)
}

// 错误：repo 层暴露的标准错误。
var (
	ErrLogicalAccountKeyExists       = errors.New("logical_account key already exists")
	ErrLogicalAccountVersionConflict = errors.New("logical_account version conflict (CAS failed)")
)

type logicalAccountRepository struct {
	dbManager *database.Manager
}

// NewLogicalAccountRepository 创建实例。
func NewLogicalAccountRepository(dbManager *database.Manager) LogicalAccountRepository {
	return &logicalAccountRepository{dbManager: dbManager}
}

func (r *logicalAccountRepository) metaDB(ctx context.Context) (*gorm.DB, error) {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return nil, fmt.Errorf("logical_account: get meta db: %w", err)
	}
	return db.WithContext(ctx), nil
}

func (r *logicalAccountRepository) Register(ctx context.Context, la *model.LogicalAccount) (*model.LogicalAccount, error) {
	if la == nil {
		return nil, errors.New("logical_account: nil input")
	}
	if err := model.ValidateLogicalAccountKey(la.LogicalAccountKey); err != nil {
		return nil, fmt.Errorf("%w: %v", model.ErrInvalidLogicalAccountKey, err)
	}
	if la.RegisteredBy == "" {
		return nil, errors.New("logical_account: registered_by required")
	}
	if la.AccountType == 0 {
		return nil, errors.New("logical_account: account_type required")
	}
	if la.AccountBusinessType == 0 {
		return nil, errors.New("logical_account: account_business_type required")
	}
	if la.Currency == "" {
		return nil, errors.New("logical_account: currency required")
	}
	if la.Status == 0 {
		la.Status = model.LogicalAccountStatusEnabled
	}

	// 检查是否已存在（不做幂等覆盖，强一致）
	existing, err := r.GetByKey(ctx, la.LogicalAccountKey)
	if err != nil && !errors.Is(err, model.ErrLogicalAccountNotRegistered) {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("%w: key=%q", ErrLogicalAccountKeyExists, la.LogicalAccountKey)
	}

	db, err := r.metaDB(ctx)
	if err != nil {
		return nil, err
	}
	// OnConflict DoNothing 是并发兜底；GetByKey + INSERT 之间可能有并发竞争，
	// 此时 unique key 冲突走 OnConflict → INSERT IGNORE 等价 → 之后再次 GetByKey
	// 拿到对方插入的行并返回 conflict 错误。
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(la).Error; err != nil {
		return nil, fmt.Errorf("logical_account: insert: %w", err)
	}
	fresh, err := r.GetByKey(ctx, la.LogicalAccountKey)
	if err != nil {
		return nil, err
	}
	if fresh == nil {
		// 极端：INSERT 后立刻 SELECT 不到 — 配置/复制延迟问题
		return nil, errors.New("logical_account: inserted but immediately not found")
	}
	// 若发现 ID/RegisteredBy 与请求不一致，说明并发被对方插入
	if fresh.RegisteredBy != la.RegisteredBy {
		return nil, fmt.Errorf("%w: lost race; key=%q", ErrLogicalAccountKeyExists, la.LogicalAccountKey)
	}
	return fresh, nil
}

func (r *logicalAccountRepository) GetByKey(ctx context.Context, key string) (*model.LogicalAccount, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: empty key", model.ErrInvalidLogicalAccountKey)
	}
	db, err := r.metaDB(ctx)
	if err != nil {
		return nil, err
	}
	var row model.LogicalAccount
	res := db.Where("logical_account_key = ?", key).Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("%w: key=%q", model.ErrLogicalAccountNotRegistered, key)
		}
		return nil, fmt.Errorf("logical_account: get by key: %w", res.Error)
	}
	if !row.IsEnabled() {
		return nil, fmt.Errorf("%w: key=%q (disabled)", model.ErrLogicalAccountNotRegistered, key)
	}
	return &row, nil
}

func (r *logicalAccountRepository) GetByID(ctx context.Context, id int64) (*model.LogicalAccount, error) {
	if id <= 0 {
		return nil, fmt.Errorf("logical_account: invalid id %d", id)
	}
	db, err := r.metaDB(ctx)
	if err != nil {
		return nil, err
	}
	var row model.LogicalAccount
	res := db.Where("id = ?", id).Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("logical_account: get by id: %w", res.Error)
	}
	return &row, nil
}

func (r *logicalAccountRepository) UpdateCurrentActive(
	ctx context.Context,
	id int64,
	accountNo string,
	activeGroup string,
	periodEnd time.Time,
	expectedVersion int64,
) error {
	if id <= 0 {
		return errors.New("logical_account: invalid id")
	}
	if accountNo == "" {
		return errors.New("logical_account: empty account_no")
	}
	db, err := r.metaDB(ctx)
	if err != nil {
		return err
	}
	updates := map[string]any{
		"current_active_account_no": accountNo,
		"current_active_period_end": periodEnd,
		"version":                   gorm.Expr("version + 1"),
	}
	// activeGroup="" 时不动 current_active_group（兼容老 caller / legacy 切换）
	if activeGroup != "" {
		updates["current_active_group"] = activeGroup
	}
	res := db.Model(&model.LogicalAccount{}).
		Where("id = ? AND version = ?", id, expectedVersion).
		Updates(updates)
	if res.Error != nil {
		return fmt.Errorf("logical_account: update current_active: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id=%d expected_version=%d", ErrLogicalAccountVersionConflict, id, expectedVersion)
	}
	return nil
}

func (r *logicalAccountRepository) SetRotationEnabled(
	ctx context.Context, id int64, enabled bool, expectedVersion int64,
) error {
	if id <= 0 {
		return errors.New("logical_account: invalid id")
	}
	db, err := r.metaDB(ctx)
	if err != nil {
		return err
	}
	v := int8(0)
	if enabled {
		v = 1
	}
	res := db.Model(&model.LogicalAccount{}).
		Where("id = ? AND version = ?", id, expectedVersion).
		Updates(map[string]any{
			"rotation_enabled": v,
			"version":          gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("logical_account: set rotation_enabled: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id=%d", ErrLogicalAccountVersionConflict, id)
	}
	return nil
}

func (r *logicalAccountRepository) ListByPrefix(
	ctx context.Context, prefix string, limit int,
) ([]*model.LogicalAccount, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	db, err := r.metaDB(ctx)
	if err != nil {
		return nil, err
	}
	var rows []*model.LogicalAccount
	q := db
	if prefix != "" {
		q = q.Where("logical_account_key LIKE ?", prefix+"%")
	}
	if err := q.Order("logical_account_key ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("logical_account: list by prefix: %w", err)
	}
	return rows, nil
}

func (r *logicalAccountRepository) ListRotating(
	ctx context.Context, limit int,
) ([]*model.LogicalAccount, error) {
	if limit <= 0 {
		limit = 1000
	}
	if limit > 10000 {
		limit = 10000
	}
	db, err := r.metaDB(ctx)
	if err != nil {
		return nil, err
	}
	var rows []*model.LogicalAccount
	if err := db.
		Where("rotation_enabled = ? AND status = ?", 1, model.LogicalAccountStatusEnabled).
		Order("id ASC").
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("logical_account: list rotating: %w", err)
	}
	return rows, nil
}

func (r *logicalAccountRepository) UpsertPolicy(
	ctx context.Context, p *model.LogicalAccountRotationPolicy,
) error {
	if p == nil {
		return errors.New("logical_account_policy: nil input")
	}
	if err := p.Validate(); err != nil {
		return fmt.Errorf("logical_account_policy: validate: %w", err)
	}
	db, err := r.metaDB(ctx)
	if err != nil {
		return err
	}
	if p.EffectiveFrom.IsZero() {
		p.EffectiveFrom = time.Now().UTC()
	}
	if p.ConfigVersion <= 0 {
		p.ConfigVersion = 1
	}
	// Upsert by primary key (logical_account_id)：存在则更新 + config_version++
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "logical_account_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"period_unit":               p.PeriodUnit,
			"period_count":              p.PeriodCount,
			"rotation_anchor_tz":        p.RotationAnchorTZ,
			"drain_p99_seconds":         p.DrainP99Seconds,
			"drain_hard_timeout_secs":   p.DrainHardTimeoutSecs,
			"archive_grace_secs":        p.ArchiveGraceSecs,
			"provision_lead_secs":       p.ProvisionLeadSecs,
			"effective_from":            p.EffectiveFrom,
			"config_version":            gorm.Expr("config_version + 1"),
		}),
	}).Create(p).Error
}

func (r *logicalAccountRepository) GetPolicy(
	ctx context.Context, logicalAccountID int64,
) (*model.LogicalAccountRotationPolicy, error) {
	if logicalAccountID <= 0 {
		return nil, errors.New("logical_account_policy: invalid id")
	}
	db, err := r.metaDB(ctx)
	if err != nil {
		return nil, err
	}
	var row model.LogicalAccountRotationPolicy
	res := db.Where("logical_account_id = ?", logicalAccountID).Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("logical_account_policy: get: %w", res.Error)
	}
	return &row, nil
}
