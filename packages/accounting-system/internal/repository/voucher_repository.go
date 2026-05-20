package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
)

// VoucherRepository 凭证仓储接口
type VoucherRepository interface {
	// Create inserts a voucher within an existing transaction.
	// dbIndex and tableIndex are routing info (same shard as the business order).
	Create(ctx context.Context, tx *gorm.DB, voucher *model.AccountingVoucher, dbIndex, tableIndex int) error
	// GetByVoucherNo retrieves a voucher; returns nil if not found.
	GetByVoucherNo(ctx context.Context, voucherNo string) (*model.AccountingVoucher, error)
}

type voucherRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewVoucherRepository 创建凭证仓储
func NewVoucherRepository(dbManager *database.Manager, router *sharding.Router) VoucherRepository {
	return &voucherRepository{
		dbManager: dbManager,
		router:    router,
	}
}

// Create inserts a voucher within an existing transaction.
func (r *voucherRepository) Create(ctx context.Context, tx *gorm.DB, voucher *model.AccountingVoucher, dbIndex, tableIndex int) error {
	tableName := fmt.Sprintf("accounting_voucher_%02d", tableIndex)
	return tx.WithContext(ctx).Table(tableName).Create(voucher).Error
}

// GetByVoucherNo retrieves a voucher by its voucher number; returns nil if not found.
// Routing: voucherNo has same prefix format as accountNo: {1d-dbIdx}{2d-tableIdx}...
func (r *voucherRepository) GetByVoucherNo(ctx context.Context, voucherNo string) (*model.AccountingVoucher, error) {
	dbIndex, tableIndex := r.router.RouteByAccountNo(voucherNo)
	tableName := fmt.Sprintf("accounting_voucher_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("voucher GetByVoucherNo: get db[%d]: %w", dbIndex, err)
	}

	var voucher model.AccountingVoucher
	result := db.WithContext(ctx).Table(tableName).
		Where("voucher_no = ?", voucherNo).
		First(&voucher)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("voucher GetByVoucherNo: %w", result.Error)
	}

	return &voucher, nil
}

