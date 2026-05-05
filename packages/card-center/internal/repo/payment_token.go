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
//
// MaskedPAN / Network 是**异步反写**字段：MarkUsed 时只写 token_hash + pi_id +
// caller（保证一次性 enforcement 的关键路径快），Detokenize 成功后再用 UpdateForensic
// 把 masked_pan / network 补上。失败也无所谓 —— 一次性约束已经实现，masked 仅
// 用于客服 / 风控查这个 PI 用的哪张卡。
type PaymentTokenUsedRow struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	TokenHash string    `gorm:"column:token_hash"`
	PIID      string    `gorm:"column:pi_id"`
	Caller    string    `gorm:"column:caller"`
	MaskedPAN string    `gorm:"column:masked_pan"` // 反写字段
	Network   string    `gorm:"column:network"`    // 反写字段
	UsedAt    time.Time `gorm:"column:used_at"`
	ExpiresAt time.Time `gorm:"column:expires_at"`
	CreatedAt time.Time `gorm:"column:created_at"`
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
	// UpdateForensic 反写 masked_pan + network（Detokenize 成功后调；best-effort）。
	// 不存在的行 silent no-op；不返错误中断主流程。
	UpdateForensic(ctx context.Context, piID, tokenHash, maskedPAN, network string) error
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

func (r *paymentTokenRepo) UpdateForensic(ctx context.Context, piID, tokenHash, maskedPAN, network string) error {
	if piID == "" || tokenHash == "" {
		return nil
	}
	db, tbl := r.shard(ctx, piID)
	updates := map[string]any{}
	if maskedPAN != "" {
		updates["masked_pan"] = maskedPAN
	}
	if network != "" {
		updates["network"] = network
	}
	if len(updates) == 0 {
		return nil
	}
	// 只更新空字段，避免覆盖已有取证记录
	res := db.WithContext(ctx).Table(tbl).
		Where("token_hash = ? AND pi_id = ? AND (masked_pan IS NULL OR masked_pan = '')", tokenHash, piID).
		Updates(updates)
	return res.Error
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
