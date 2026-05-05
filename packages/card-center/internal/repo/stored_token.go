package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/xiongwp/payment-util/shadow"
)

// StoredCardRow card_stored_token 表行（按 user_id 分片）
type StoredCardRow struct {
	ID          int64      `gorm:"column:id;primaryKey;autoIncrement"`
	UserID      int64      `gorm:"column:user_id"`
	StoredToken string     `gorm:"column:stored_token"`
	TokenHash   string     `gorm:"column:token_hash"`
	MaskedPAN   string     `gorm:"column:masked_pan"`
	Network     string     `gorm:"column:network"`
	ExpMonth    int        `gorm:"column:exp_month"`
	ExpYear     int        `gorm:"column:exp_year"`
	HolderName  string     `gorm:"column:holder_name"`
	Status      string     `gorm:"column:status"` // active / deleted
	KMSKid      string     `gorm:"column:kms_kid"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
	DeletedAt   *time.Time `gorm:"column:deleted_at"`
}

// ErrStoredCardNotFound 记录不存在
var ErrStoredCardNotFound = errors.New("repo: stored card not found")

// StoredCardRepo 用户存卡索引仓储
type StoredCardRepo interface {
	// Insert 入库新存卡。token_hash 唯一约束保证同 token 不重复。
	Insert(ctx context.Context, row *StoredCardRow) error
	// GetByTokenHash 按 token_hash 查（user_id 校验由 service 层做）
	GetByTokenHash(ctx context.Context, userID int64, tokenHash string) (*StoredCardRow, error)
	// ListActiveByUser 列出该用户所有 active 卡（前端 UI 展示用）
	ListActiveByUser(ctx context.Context, userID int64) ([]*StoredCardRow, error)
	// SoftDelete 软删（status=deleted, deleted_at=now）。物理失效靠 KMS rotation。
	SoftDelete(ctx context.Context, userID int64, tokenHash string, reason string) error
}

const tblStoredCard = "card_stored_token"

type storedCardRepo struct {
	mgr *Manager
}

// NewStoredCardRepo 构造
func NewStoredCardRepo(mgr *Manager) StoredCardRepo {
	return &storedCardRepo{mgr: mgr}
}

func (r *storedCardRepo) shard(ctx context.Context, userID int64) (*gorm.DB, string) {
	dbIdx, gtbl := r.mgr.router.RouteByUserID(userID)
	return r.mgr.Shard(dbIdx), r.mgr.router.TableName(ctx, tblStoredCard, gtbl)
}

func (r *storedCardRepo) Insert(ctx context.Context, row *StoredCardRow) error {
	if row.UserID == 0 || row.TokenHash == "" || row.StoredToken == "" {
		return fmt.Errorf("Insert: user_id / token_hash / stored_token required")
	}
	if row.Status == "" {
		row.Status = "active"
	}
	db, tbl := r.shard(ctx, row.UserID)
	return db.WithContext(ctx).Table(tbl).Create(row).Error
}

func (r *storedCardRepo) GetByTokenHash(ctx context.Context, userID int64, tokenHash string) (*StoredCardRow, error) {
	db, tbl := r.shard(ctx, userID)
	var row StoredCardRow
	err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND token_hash = ?", userID, tokenHash).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrStoredCardNotFound
	}
	return &row, err
}

func (r *storedCardRepo) ListActiveByUser(ctx context.Context, userID int64) ([]*StoredCardRow, error) {
	db, tbl := r.shard(ctx, userID)
	var rows []*StoredCardRow
	err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND status = ?", userID, "active").
		Order("created_at DESC").Find(&rows).Error
	// 不返完整 stored_token（避免 list API 把长 token 都吐出来给前端，无需）
	for _, r := range rows {
		r.StoredToken = "" // 客户端要 token 走 GetByTokenHash 单查
	}
	return rows, err
}

func (r *storedCardRepo) SoftDelete(ctx context.Context, userID int64, tokenHash, reason string) error {
	db, tbl := r.shard(ctx, userID)
	now := time.Now()
	res := db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND token_hash = ? AND status = ?", userID, tokenHash, "active").
		Updates(map[string]any{
			"status":     "deleted",
			"deleted_at": &now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrStoredCardNotFound
	}
	return nil
}

// shadowTbl helper（其它包用 shadow.TableName 兜底）
var _ = shadow.TableName
