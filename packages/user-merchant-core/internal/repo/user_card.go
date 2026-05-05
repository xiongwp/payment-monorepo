package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/xiongwp/user-merchant-core/internal/domain"
)

// 跟其它 user_id 分片表对齐；TableName helper 留下 base name
const tblUserCard = "user_card"

// UserCardRepository 用户存卡仓储（按 user_id 分片到 user_merchant_db_0..9）。
//
// 注意：本仓储**不存** PAN / CVV，只存 card-center stored_token + masked_pan + 元数据。
type UserCardRepository interface {
	// Insert 入库；TokenHash uniq → 同 token 不会重复加。
	Insert(ctx context.Context, c *domain.UserCard) error
	// GetByID 按 (user_id, id) 查（id 给前端用，对外暴露）。
	GetByID(ctx context.Context, userID, id int64) (*domain.UserCard, error)
	// GetByTokenHash 按 (user_id, token_hash) 查。
	GetByTokenHash(ctx context.Context, userID int64, tokenHash string) (*domain.UserCard, error)
	// ListActiveByUser 列出 active + 未过期的卡（前端展示）。
	ListActiveByUser(ctx context.Context, userID int64, limit int) ([]*domain.UserCard, error)
	// SetDefault 把 (user_id, id) 设为默认卡，同用户其它卡 is_default=false。
	SetDefault(ctx context.Context, userID, id int64) error
	// SoftDelete 软删（status=deleted + deleted_at=now）。
	SoftDelete(ctx context.Context, userID, id int64) error
}

type userCardRepo struct {
	mgr    *Manager
	router *ShardRouter
}

// NewUserCardRepository 构造。router 为 nil（dev/单测）退化到单 meta 库。
func NewUserCardRepository(mgr *Manager, router *ShardRouter) UserCardRepository {
	return &userCardRepo{mgr: mgr, router: router}
}

func (r *userCardRepo) shard(ctx context.Context, userID int64) (*gorm.DB, string) {
	return shardForUser(ctx, r.mgr, r.router, tblUserCard, userID)
}

func (r *userCardRepo) Insert(ctx context.Context, c *domain.UserCard) error {
	if c.UserID == 0 || c.StoredToken == "" || c.TokenHash == "" {
		return fmt.Errorf("%w: user_id/stored_token/token_hash required", domain.ErrValidation)
	}
	if c.Status == "" {
		c.Status = domain.UserCardActive
	}
	db, tbl := r.shard(ctx, c.UserID)
	if err := db.WithContext(ctx).Table(tbl).Create(c).Error; err != nil {
		if isDupKey(err) {
			// token_hash uniq 撞了：调用方应直接复用现有 row
			return fmt.Errorf("user card already exists: %w", err)
		}
		return err
	}
	return nil
}

func (r *userCardRepo) GetByID(ctx context.Context, userID, id int64) (*domain.UserCard, error) {
	db, tbl := r.shard(ctx, userID)
	var c domain.UserCard
	err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND id = ?", userID, id).
		Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrUserCardNotFound
	}
	return &c, err
}

func (r *userCardRepo) GetByTokenHash(ctx context.Context, userID int64, tokenHash string) (*domain.UserCard, error) {
	db, tbl := r.shard(ctx, userID)
	var c domain.UserCard
	err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND token_hash = ?", userID, tokenHash).
		Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrUserCardNotFound
	}
	return &c, err
}

func (r *userCardRepo) ListActiveByUser(ctx context.Context, userID int64, limit int) ([]*domain.UserCard, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	db, tbl := r.shard(ctx, userID)
	var rows []*domain.UserCard
	err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND status = ?", userID, domain.UserCardActive).
		Order("is_default DESC, created_at DESC").
		Limit(limit).Find(&rows).Error
	// 安全：list 不返完整 stored_token（避免下行下游误存）。前端走 ID 拿，
	// 单查时再返完整 token；调用方在 service 层置空。
	return rows, err
}

func (r *userCardRepo) SetDefault(ctx context.Context, userID, id int64) error {
	db, tbl := r.shard(ctx, userID)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) 把同用户其它卡 is_default=false
		if err := tx.Table(tbl).
			Where("user_id = ? AND is_default = ?", userID, true).
			Update("is_default", false).Error; err != nil {
			return err
		}
		// 2) 把目标设为 default
		res := tx.Table(tbl).
			Where("user_id = ? AND id = ? AND status = ?", userID, id, domain.UserCardActive).
			Update("is_default", true)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return domain.ErrUserCardNotFound
		}
		return nil
	})
}

func (r *userCardRepo) SoftDelete(ctx context.Context, userID, id int64) error {
	db, tbl := r.shard(ctx, userID)
	now := time.Now()
	res := db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND id = ? AND status = ?", userID, id, domain.UserCardActive).
		Updates(map[string]any{
			"status":     domain.UserCardDeleted,
			"deleted_at": &now,
			"is_default": false,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrUserCardNotFound
	}
	return nil
}
