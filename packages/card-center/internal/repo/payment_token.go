package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// PaymentTokenUsedRow card_payment_token_used 表行（按 pi_id 分片）
type PaymentTokenUsedRow struct {
	ID         int64     `gorm:"column:id;primaryKey;autoIncrement"`
	TokenHash  string    `gorm:"column:token_hash"`
	PIID       string    `gorm:"column:pi_id"`
	Caller     string    `gorm:"column:caller"`
	UsedAt     time.Time `gorm:"column:used_at"`
	ExpiresAt  time.Time `gorm:"column:expires_at"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

// ErrPaymentTokenAlreadyUsed 一次性 token 已被使用过 → 拒
var ErrPaymentTokenAlreadyUsed = errors.New("repo: payment token already used")

// PaymentTokenRepo 一次性支付 token 使用记录仓储。
//
// 关键纪律：MarkUsed **必须**先于 Detokenize 返回 PAN 调用。INSERT 失败（dup key）
// = token 之前用过 → 拒，绝不能 detok 后再 MarkUsed（race 窗口让攻击者多次套）。
type PaymentTokenRepo interface {
	// MarkUsed 标记 token 已使用。dup key (uk_token_hash) 返 ErrPaymentTokenAlreadyUsed。
	MarkUsed(ctx context.Context, row *PaymentTokenUsedRow) error
	// PurgeExpired 清理 expires_at < cutoff 的行（cron 调，每片最多 limit）
	PurgeExpired(ctx context.Context, cutoff time.Time, limitPerShard int) (int64, error)
}

const tblPaymentTokenUsed = "card_payment_token_used"

type paymentTokenRepo struct {
	mgr *Manager
}

// NewPaymentTokenRepo 构造
func NewPaymentTokenRepo(mgr *Manager) PaymentTokenRepo {
	return &paymentTokenRepo{mgr: mgr}
}

func (r *paymentTokenRepo) shard(ctx context.Context, piID string) (*gorm.DB, string) {
	dbIdx, gtbl := r.mgr.router.RouteByPIID(piID)
	return r.mgr.Shard(dbIdx), r.mgr.router.TableName(ctx, tblPaymentTokenUsed, gtbl)
}

func (r *paymentTokenRepo) MarkUsed(ctx context.Context, row *PaymentTokenUsedRow) error {
	if row.TokenHash == "" || row.PIID == "" {
		return fmt.Errorf("MarkUsed: token_hash / pi_id required")
	}
	if row.UsedAt.IsZero() {
		row.UsedAt = time.Now()
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = row.UsedAt
	}
	db, tbl := r.shard(ctx, row.PIID)
	err := db.WithContext(ctx).Table(tbl).Create(row).Error
	if err != nil && isDupKey(err) {
		return ErrPaymentTokenAlreadyUsed
	}
	return err
}

func (r *paymentTokenRepo) PurgeExpired(ctx context.Context, cutoff time.Time, limitPerShard int) (int64, error) {
	if limitPerShard <= 0 {
		limitPerShard = 1000
	}
	var total int64
	for _, s := range r.mgr.router.AllShards() {
		db := r.mgr.Shard(s.DBIndex)
		tbl := r.mgr.router.TableName(ctx, tblPaymentTokenUsed, s.TableIndex)
		res := db.WithContext(ctx).Table(tbl).
			Where("expires_at < ?", cutoff).
			Limit(limitPerShard).
			Delete(nil)
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
	}
	return total, nil
}

func isDupKey(err error) bool {
	if err == nil {
		return false
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1062 {
		return true
	}
	return strings.Contains(err.Error(), "Duplicate entry")
}
