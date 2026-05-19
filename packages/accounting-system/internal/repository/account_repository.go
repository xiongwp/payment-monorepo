package repository

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AccountRepository 账户仓储接口
type AccountRepository interface {
	// CreateAccount 创建账户
	CreateAccount(ctx context.Context, account *model.Account) error

	// GetAccountByNo 根据账户号查询账户
	GetAccountByNo(ctx context.Context, accountNo string) (*model.Account, error)

	// GetAccountsByNos 批量根据账户号查询同一分片内的多个账户（IN 查询，减少 N+1 问题）
	// accountNos 中的所有账户号必须路由到相同的 (dbIndex, tableIndex)，由调用方保证。
	GetAccountsByNos(ctx context.Context, accountNos []string, dbIndex, tableIndex int) ([]*model.Account, error)

	// GetAccountByUserID 根据用户ID查询账户
	GetAccountByUserID(ctx context.Context, userID int64) (*model.Account, error)

	// GetAccountByOwnerAndType 根据 owner_id + account_type 查询账户
	// 用于 TransactionService 通过规则配置动态解析账户
	GetAccountByOwnerAndType(ctx context.Context, ownerID int64, accountType model.AccountType) (*model.Account, error)

	// GetAccountByUserAndBusinessType 根据 userId + businessType 查询账户（唯一键查询）
	// 注意：同一 (userID, businessType) 可能跨多币种存在多条记录；本方法仅返回
	// 首条（.First()）。要拿全部币种用 ListAccountsByUserAndBusinessType。
	GetAccountByUserAndBusinessType(ctx context.Context, userID int64, businessType model.AccountBusinessType) (*model.Account, error)

	// ListAccountsByUserAndBusinessType 返回该 (userID, businessType) 下全部币种的账户。
	// currency 非空时额外按 currency 精确过滤；currency 为空时返回全部币种。
	// 用于 admin-web 的「按用户+业务类型」查询，避免多币种用户只看到一个币种账户。
	ListAccountsByUserAndBusinessType(ctx context.Context, userID int64, businessType model.AccountBusinessType, currency string) ([]*model.Account, error)

	// UpdateBalance 更新账户余额（带乐观锁）
	UpdateBalance(ctx context.Context, accountNo string, amount int64, isDebit bool, version int64) error

	// GetAccountForUpdate 查询账户并加锁
	GetAccountForUpdate(ctx context.Context, tx *gorm.DB, accountNo string, dbIndex, tableIndex int) (*model.Account, error)

	// UpdateAccountStatus 修改账户状态 (Active / Frozen / Disabled)。
	// 由 admin 操作触发 (FreezeAccount / UnfreezeAccount gRPC),非业务热路径,直接 UPDATE。
	UpdateAccountStatus(ctx context.Context, accountNo string, newStatus model.AccountStatus) error
}

type accountRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewAccountRepository 创建账户仓储
func NewAccountRepository(dbManager *database.Manager, router *sharding.Router) AccountRepository {
	return &accountRepository{
		dbManager: dbManager,
		router:    router,
	}
}

// CreateAccount 创建账户
func (r *accountRepository) CreateAccount(ctx context.Context, account *model.Account) error {
	dbIndex, tableIndex := r.router.RouteByUserID(account.UserID)
	tableName := r.router.GetTableName("account", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return err
	}

	return db.WithContext(ctx).Table(tableName).Create(account).Error
}

// GetAccountByNo 根据账户号查询账户
func (r *accountRepository) GetAccountByNo(ctx context.Context, accountNo string) (*model.Account, error) {
	dbIndex, tableIndex := r.router.RouteByAccountNo(accountNo)
	tableName := r.router.GetTableName("account", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, err
	}

	var account model.Account
	result := db.WithContext(ctx).Table(tableName).Where("account_no = ?", accountNo).First(&account)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, result.Error
	}

	return &account, nil
}

// GetAccountsByNos 批量查询同一分片内的多个账户（WHERE account_no IN (...)）
func (r *accountRepository) GetAccountsByNos(ctx context.Context, accountNos []string, dbIndex, tableIndex int) ([]*model.Account, error) {
	if len(accountNos) == 0 {
		return nil, nil
	}
	tableName := r.router.GetTableName("account", tableIndex)
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, err
	}
	var accounts []*model.Account
	if err := db.WithContext(ctx).Table(tableName).
		Where("account_no IN ?", accountNos).
		Find(&accounts).Error; err != nil {
		return nil, err
	}
	return accounts, nil
}

// GetAccountByUserID 根据用户ID查询账户
func (r *accountRepository) GetAccountByUserID(ctx context.Context, userID int64) (*model.Account, error) {
	dbIndex, tableIndex := r.router.RouteByUserID(userID)
	log.Printf("Routing userID=%d to dbIndex=%d, tableIndex=%d", userID, dbIndex, tableIndex)
	tableName := r.router.GetTableName("account", tableIndex)
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, err
	}

	var account model.Account
	result := db.WithContext(ctx).Table(tableName).Where("user_id = ?", userID).First(&account)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, result.Error
	}

	return &account, nil
}

// UpdateBalance 更新账户余额（带乐观锁）
func (r *accountRepository) UpdateBalance(ctx context.Context, accountNo string, amount int64, isDebit bool, version int64) error {
	dbIndex, tableIndex := r.router.RouteByAccountNo(accountNo)
	tableName := r.router.GetTableName("account", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return err
	}

	var updates map[string]interface{}
	if isDebit {
		// 借方：增加余额（资产类科目）或减少余额（负债/权益类科目）
		updates = map[string]interface{}{
			"balance":           gorm.Expr("balance + ?", amount),
			"available_balance": gorm.Expr("available_balance + ?", amount),
			"version":           gorm.Expr("version + 1"),
		}
	} else {
		// 贷方：减少余额（资产类科目）或增加余额（负债/权益类科目）
		updates = map[string]interface{}{
			"balance":           gorm.Expr("balance - ?", amount),
			"available_balance": gorm.Expr("available_balance - ?", amount),
			"version":           gorm.Expr("version + 1"),
		}
	}

	result := db.WithContext(ctx).Table(tableName).
		Where("account_no = ? AND version = ?", accountNo, version).
		Updates(updates)

	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected == 0 {
		return fmt.Errorf("update failed: version conflict or account not found")
	}

	return nil
}

// GetAccountByOwnerAndType 根据 owner_id + account_type 查询账户
// 路由规则与 GetAccountByUserID 相同（按 owner_id 分片）
func (r *accountRepository) GetAccountByOwnerAndType(ctx context.Context, ownerID int64, accountType model.AccountType) (*model.Account, error) {
	dbIndex, tableIndex := r.router.RouteByUserID(ownerID)
	tableName := r.router.GetTableName("account", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, err
	}

	var account model.Account
	result := db.WithContext(ctx).Table(tableName).
		Where("user_id = ? AND account_type = ?", ownerID, accountType).
		First(&account)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, result.Error
	}

	return &account, nil
}

// GetAccountByUserAndBusinessType 根据 userId + businessType 查询账户（唯一键查询）
// 路由与创建时一致：按 userID 分片
func (r *accountRepository) GetAccountByUserAndBusinessType(ctx context.Context, userID int64, businessType model.AccountBusinessType) (*model.Account, error) {
	dbIndex, tableIndex := r.router.RouteByUserID(userID)
	tableName := r.router.GetTableName("account", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, err
	}

	var account model.Account
	result := db.WithContext(ctx).Table(tableName).
		Where("user_id = ? AND account_business_type = ?", userID, businessType).
		First(&account)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, result.Error
	}

	return &account, nil
}

// ListAccountsByUserAndBusinessType 返回 (userID, businessType) 下全部币种账户。
func (r *accountRepository) ListAccountsByUserAndBusinessType(ctx context.Context, userID int64, businessType model.AccountBusinessType, currency string) ([]*model.Account, error) {
	dbIndex, tableIndex := r.router.RouteByUserID(userID)
	tableName := r.router.GetTableName("account", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, err
	}

	var accounts []*model.Account
	q := db.WithContext(ctx).Table(tableName).
		Where("user_id = ? AND account_business_type = ?", userID, businessType)
	if currency != "" {
		q = q.Where("currency = ?", currency)
	}
	if err := q.Order("currency ASC").Find(&accounts).Error; err != nil {
		return nil, err
	}
	return accounts, nil
}

// GetAccountForUpdate 查询账户并加锁
func (r *accountRepository) GetAccountForUpdate(ctx context.Context, tx *gorm.DB, accountNo string, dbIndex, tableIndex int) (*model.Account, error) {
	tableName := r.router.GetTableName("account", tableIndex)

	var account model.Account
	result := tx.WithContext(ctx).Table(tableName).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("account_no = ?", accountNo).
		Take(&account)

	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, result.Error
	}

	return &account, nil
}

// UpdateAccountStatus 修改账户状态 (Active / Frozen / Disabled)。
//
// 由 FreezeAccount / UnfreezeAccount gRPC 触发,admin 路径。
// 同一账户号在 (userID 取自 account_no 派生) 单分片内只有一条记录,Where account_no UPDATE 安全。
// 加 version+1 防止与 UpdateBalance 并发覆盖。
func (r *accountRepository) UpdateAccountStatus(ctx context.Context, accountNo string, newStatus model.AccountStatus) error {
	dbIndex, tableIndex := r.router.RouteByAccountNo(accountNo)
	tableName := r.router.GetTableName("account", tableIndex)
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return err
	}
	res := db.WithContext(ctx).Table(tableName).
		Where("account_no = ?", accountNo).
		Updates(map[string]interface{}{
			"status":  newStatus,
			"version": gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("update account status: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return errors.New("account not found")
	}
	log.Printf("[account-repo] account=%s status=%d updated", accountNo, newStatus)
	return nil
}
