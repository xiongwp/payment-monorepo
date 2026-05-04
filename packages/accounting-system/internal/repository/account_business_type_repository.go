package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AccountBusinessTypeRepository 管理 account_meta.account_business_type_info。
//
// 存在 meta 库（account_meta），全局单表，无分片。Register 在新渠道首次出现时
// Upsert 一行；List / Get 供 admin-web 和 ConfigSyncWorker 查询。
type AccountBusinessTypeRepository interface {
	// Register 幂等注册一个 business_type。
	//   - 如果 business_type 不存在：INSERT 一行。
	//   - 如果 business_type 已存在且 (account_type, category) 一致：不做任何修改，返回已有记录。
	//   - 如果 business_type 已存在但 (account_type, category) 冲突：返回 ErrBusinessTypeConflict，拒绝覆盖。
	//     因为一个 business_type 的 category 决定它的借贷方向，绝不能热改。
	Register(ctx context.Context, info *model.AccountBusinessTypeInfo) (*model.AccountBusinessTypeInfo, error)

	// List 返回所有已注册业务类型，按 business_type 升序。
	List(ctx context.Context) ([]*model.AccountBusinessTypeInfo, error)

	// GetByBusinessType 按数字码查询；不存在返回 nil。
	GetByBusinessType(ctx context.Context, businessType model.AccountBusinessType) (*model.AccountBusinessTypeInfo, error)

	// GetByCode 按字符码查询；不存在返回 nil。
	GetByCode(ctx context.Context, code string) (*model.AccountBusinessTypeInfo, error)

	// UpdateEnabled 启用 / 停用某条记录（不改其他字段）。
	UpdateEnabled(ctx context.Context, id int64, enabled bool) error
}

// ErrBusinessTypeConflict Register 时 business_type 已存在但 account_type 冲突。
var ErrBusinessTypeConflict = errors.New("business_type already registered with different account_type")

type accountBusinessTypeRepository struct {
	dbManager *database.Manager
}

func NewAccountBusinessTypeRepository(dbManager *database.Manager) AccountBusinessTypeRepository {
	return &accountBusinessTypeRepository{dbManager: dbManager}
}

func (r *accountBusinessTypeRepository) metaDB(ctx context.Context) (*gorm.DB, error) {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return nil, fmt.Errorf("account_business_type: get meta db: %w", err)
	}
	return db.WithContext(ctx), nil
}

func (r *accountBusinessTypeRepository) Register(ctx context.Context, info *model.AccountBusinessTypeInfo) (*model.AccountBusinessTypeInfo, error) {
	if info == nil {
		return nil, fmt.Errorf("nil info")
	}
	if info.BusinessType <= 0 || int32(info.BusinessType) > 999 {
		return nil, fmt.Errorf("business_type %d out of range [1, 999]", info.BusinessType)
	}
	if info.BusinessTypeCode == "" {
		return nil, fmt.Errorf("business_type_code required")
	}
	if info.AccountType <= 0 {
		return nil, fmt.Errorf("account_type required")
	}
	db, err := r.metaDB(ctx)
	if err != nil {
		return nil, err
	}

	// 冲突检查：若已存在，校验一致性而不是盲目覆盖。
	// 只比较 account_type —— category 不在本表存储（由 account_type 派生）。
	existing, err := r.GetByBusinessType(ctx, info.BusinessType)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.AccountType != info.AccountType {
			return nil, fmt.Errorf("%w: business_type=%d existing_account_type=%d new_account_type=%d",
				ErrBusinessTypeConflict, info.BusinessType,
				existing.AccountType, info.AccountType)
		}
		// 一致性 OK，返回现有
		return existing, nil
	}

	// 保证 enabled 默认 1
	if info.Enabled == 0 {
		info.Enabled = 1
	}
	// OnConflict 兜底（business_type_code 也是 unique key，防并发冲突）
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(info).Error; err != nil {
		return nil, fmt.Errorf("register business_type: %w", err)
	}
	// 重新读回（ID 可能由 DB 生成）
	fresh, err := r.GetByBusinessType(ctx, info.BusinessType)
	if err != nil {
		return nil, err
	}
	if fresh == nil {
		return info, nil
	}
	return fresh, nil
}

func (r *accountBusinessTypeRepository) List(ctx context.Context) ([]*model.AccountBusinessTypeInfo, error) {
	db, err := r.metaDB(ctx)
	if err != nil {
		return nil, err
	}
	var rows []*model.AccountBusinessTypeInfo
	if err := db.Order("business_type ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list business_type: %w", err)
	}
	return rows, nil
}

func (r *accountBusinessTypeRepository) GetByBusinessType(ctx context.Context, businessType model.AccountBusinessType) (*model.AccountBusinessTypeInfo, error) {
	db, err := r.metaDB(ctx)
	if err != nil {
		return nil, err
	}
	var row model.AccountBusinessTypeInfo
	res := db.Where("business_type = ?", businessType).Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get business_type: %w", res.Error)
	}
	return &row, nil
}

func (r *accountBusinessTypeRepository) GetByCode(ctx context.Context, code string) (*model.AccountBusinessTypeInfo, error) {
	db, err := r.metaDB(ctx)
	if err != nil {
		return nil, err
	}
	var row model.AccountBusinessTypeInfo
	res := db.Where("business_type_code = ?", code).Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get business_type by code: %w", res.Error)
	}
	return &row, nil
}

func (r *accountBusinessTypeRepository) UpdateEnabled(ctx context.Context, id int64, enabled bool) error {
	db, err := r.metaDB(ctx)
	if err != nil {
		return err
	}
	en := int8(0)
	if enabled {
		en = 1
	}
	return db.Model(&model.AccountBusinessTypeInfo{}).
		Where("id = ?", id).
		Update("enabled", en).Error
}
