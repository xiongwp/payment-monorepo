package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/xiongwp/user-merchant-core/internal/domain"
)

// MerchantRepository merchants 表（meta 库，非分片）+ 配套 KYC 文档/审计表。
type MerchantRepository interface {
	Create(ctx context.Context, m *domain.Merchant) error
	Get(ctx context.Context, id string) (*domain.Merchant, error)
	BatchGet(ctx context.Context, ids []string) ([]*domain.Merchant, error)
	GetByEmail(ctx context.Context, email string) (*domain.Merchant, error)
	GetByKeyHash(ctx context.Context, hash string) (*domain.Merchant, error)
	List(ctx context.Context, status domain.MerchantStatus, kyc domain.KYCStatus, limit, offset int) ([]*domain.Merchant, int64, error)
	Update(ctx context.Context, m *domain.Merchant) error
	UpdateFields(ctx context.Context, id string, fields map[string]any) (*domain.Merchant, error)

	// KYC transitions — 组合更新 status + 写一条 audit 记录在同一事务里
	TransitionKYC(ctx context.Context, id string, from, to domain.KYCStatus, reason, actor string) (*domain.Merchant, error)

	// Documents
	AddDocument(ctx context.Context, d *domain.MerchantKYCDocument) error
	ListDocuments(ctx context.Context, merchantID string) ([]*domain.MerchantKYCDocument, error)
	ReviewDocument(ctx context.Context, id, status, note string) error

	// Audit trail
	ListKYCAudits(ctx context.Context, merchantID string, limit int) ([]*domain.MerchantKYCAudit, error)

	// ListActive 最多 N 条 active 商户（供缓存 warmup 用；不带分页/状态过滤的简化版）
	ListActive(ctx context.Context, limit int) ([]*domain.Merchant, error)

	// SoftDelete 把 deleted_at 置为 now；不会真删。调用方通常是 TransitionKYC 到
	// Terminated 之后触发，或合规 hard-request 从 admin 端显式调用。
	SoftDelete(ctx context.Context, id string) error
	// PurgeDeletedBefore 真删 deleted_at < cutoff 的商户（及其 KYC 文档/审计）。
	// 返回删除条数。retention sweeper 定时调；cutoff 通常是 now-7y 合规要求。
	PurgeDeletedBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

type merchantRepo struct {
	mgr *Manager
}

// NewMerchantRepository 构造（用 meta DB，非分片）
func NewMerchantRepository(mgr *Manager) MerchantRepository {
	return &merchantRepo{mgr: mgr}
}

func (r *merchantRepo) db() *gorm.DB   { return r.mgr.GetMeta() }
func (r *merchantRepo) dbRO() *gorm.DB { return r.mgr.GetMetaRO() }

// ─── basic CRUD ──────────────────────────────────────────────────────────────

func (r *merchantRepo) Create(ctx context.Context, m *domain.Merchant) error {
	if m.ID == "" || m.ContactEmail == "" {
		return fmt.Errorf("%w: id and contact_email required", domain.ErrValidation)
	}
	if m.Country == "" {
		m.Country = "PH"
	}
	if m.SettleCurrency == "" {
		m.SettleCurrency = "PHP"
	}
	if m.BusinessType == "" {
		m.BusinessType = "individual"
	}
	if m.KYCStatus == "" {
		m.KYCStatus = domain.KYCStatusPending
	}
	if m.Status == "" {
		m.Status = domain.MerchantStatusPending
	}
	if m.RiskTier == "" {
		m.RiskTier = "standard"
	}
	if m.RateLimitRPS == 0 {
		m.RateLimitRPS = 100
	}
	err := r.db().WithContext(ctx).Create(m).Error
	if isDupKey(err) {
		return fmt.Errorf("%w: duplicate email or id", domain.ErrValidation)
	}
	return err
}

func (r *merchantRepo) Get(ctx context.Context, id string) (*domain.Merchant, error) {
	var m domain.Merchant
	// 默认过滤软删行；Terminated 的商户真实身份应走一条合规导出 API（不经这里）。
	err := r.db().WithContext(ctx).Where("id = ? AND deleted_at IS NULL", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrMerchantNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// BatchGet 一次拉回多个 merchant；读走 RO，重复 id 在结果里合并。上限 500 防
// admin-web 传 10k ids 把 DB 打跪；调用方应按需分页。软删的行不返回。
func (r *merchantRepo) BatchGet(ctx context.Context, ids []string) ([]*domain.Merchant, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 500 {
		ids = ids[:500]
	}
	var out []*domain.Merchant
	err := r.dbRO().WithContext(ctx).
		Where("id IN ? AND deleted_at IS NULL", ids).Find(&out).Error
	return out, err
}

// SoftDelete 设置 deleted_at=now；行仍在表内供审计/合规 SELECT，但不再对外可见。
func (r *merchantRepo) SoftDelete(ctx context.Context, id string) error {
	res := r.db().WithContext(ctx).Model(&domain.Merchant{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Update("deleted_at", time.Now())
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrMerchantNotFound
	}
	return nil
}

// PurgeDeletedBefore 真删 deleted_at<cutoff 的商户 + 级联清其 KYC 文档 / audit。
// 在一个事务里；批量上限 5000 防长事务锁表。调用方应按需循环调用。
func (r *merchantRepo) PurgeDeletedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	err := r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先锁定要删的 id
		var ids []string
		if err := tx.Model(&domain.Merchant{}).
			Where("deleted_at IS NOT NULL AND deleted_at < ?", cutoff).
			Limit(5000).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if err := tx.Where("merchant_id IN ?", ids).
			Delete(&domain.MerchantKYCDocument{}).Error; err != nil {
			return err
		}
		if err := tx.Where("merchant_id IN ?", ids).
			Delete(&domain.MerchantKYCAudit{}).Error; err != nil {
			return err
		}
		res := tx.Where("id IN ?", ids).Delete(&domain.Merchant{})
		if res.Error != nil {
			return res.Error
		}
		total = res.RowsAffected
		return nil
	})
	return total, err
}

func (r *merchantRepo) GetByEmail(ctx context.Context, email string) (*domain.Merchant, error) {
	var m domain.Merchant
	err := r.db().WithContext(ctx).Where("contact_email = ?", email).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrMerchantNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *merchantRepo) GetByKeyHash(ctx context.Context, hash string) (*domain.Merchant, error) {
	var m domain.Merchant
	// Auth 热路径：可容忍复制延迟（rotate/suspend 有 cache.Invalidate 兜底），走 RO。
	// 软删商户的 key 立刻失效。
	err := r.dbRO().WithContext(ctx).
		Where("(live_key_hash = ? OR test_key_hash = ?) AND deleted_at IS NULL",
			hash, hash).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrMerchantNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *merchantRepo) List(ctx context.Context, status domain.MerchantStatus, kyc domain.KYCStatus, limit, offset int) ([]*domain.Merchant, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	// admin 翻页的 List 容忍滞后，走 RO。默认隐藏软删行 —— 合规导出要看全部的
	// 走另一条 export 路径。
	q := r.dbRO().WithContext(ctx).Model(&domain.Merchant{}).Where("deleted_at IS NULL")
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if kyc != "" {
		q = q.Where("kyc_status = ?", kyc)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []*domain.Merchant
	if err := q.Order("created DESC").Limit(limit).Offset(offset).Find(&out).Error; err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (r *merchantRepo) Update(ctx context.Context, m *domain.Merchant) error {
	return r.db().WithContext(ctx).Save(m).Error
}

func (r *merchantRepo) UpdateFields(ctx context.Context, id string, fields map[string]any) (*domain.Merchant, error) {
	if len(fields) == 0 {
		return r.Get(ctx, id)
	}
	if err := r.db().WithContext(ctx).Model(&domain.Merchant{}).
		Where("id = ?", id).Updates(fields).Error; err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

// ─── KYC FSM ────────────────────────────────────────────────────────────────

func (r *merchantRepo) TransitionKYC(ctx context.Context, id string, from, to domain.KYCStatus, reason, actor string) (*domain.Merchant, error) {
	if !from.CanTransition(to) {
		return nil, fmt.Errorf("%w: %s → %s", domain.ErrMerchantKYCInvalidTransition, from, to)
	}
	var updated *domain.Merchant
	err := r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Compare-and-swap on current KYC status: reject if concurrent change moved it.
		res := tx.Model(&domain.Merchant{}).
			Where("id = ? AND kyc_status = ?", id, from).
			Updates(map[string]any{
				"kyc_status":      to,
				"kyc_reason":      reason,
				"kyc_reviewer":    actor,
				"kyc_reviewed_at": gorm.Expr("CURRENT_TIMESTAMP(3)"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// Either not found or kyc_status changed under us.
			var cur domain.Merchant
			if err := tx.Where("id = ?", id).First(&cur).Error; errors.Is(err, gorm.ErrRecordNotFound) {
				return domain.ErrMerchantNotFound
			} else if err != nil {
				return err
			}
			return fmt.Errorf("%w: current=%s, tried from=%s", domain.ErrMerchantKYCInvalidTransition, cur.KYCStatus, from)
		}
		// Sync business status: approved→active, rejected/terminated→terminated, suspended→suspended.
		switch to {
		case domain.KYCStatusApproved:
			_ = tx.Model(&domain.Merchant{}).Where("id = ?", id).Update("status", domain.MerchantStatusActive).Error
		case domain.KYCStatusSuspended:
			_ = tx.Model(&domain.Merchant{}).Where("id = ?", id).Update("status", domain.MerchantStatusSuspended).Error
		case domain.KYCStatusRejected, domain.KYCStatusTerminated:
			_ = tx.Model(&domain.Merchant{}).Where("id = ?", id).Update("status", domain.MerchantStatusTerminated).Error
		}
		audit := &domain.MerchantKYCAudit{
			MerchantID: id, FromStatus: from, ToStatus: to, Reason: reason, Actor: actor,
		}
		if err := tx.Create(audit).Error; err != nil {
			return err
		}
		var m domain.Merchant
		if err := tx.Where("id = ?", id).First(&m).Error; err != nil {
			return err
		}
		updated = &m
		return nil
	})
	return updated, err
}

// ─── documents ───────────────────────────────────────────────────────────────

func (r *merchantRepo) AddDocument(ctx context.Context, d *domain.MerchantKYCDocument) error {
	if d.MerchantID == "" || d.FileURL == "" {
		return fmt.Errorf("%w: merchant_id and file_url required", domain.ErrValidation)
	}
	if d.ReviewStatus == "" {
		d.ReviewStatus = "pending"
	}
	return r.db().WithContext(ctx).Create(d).Error
}

// ListActive 用于启动 warmup；按 updated DESC 取热商户（被动端 Update 倾向热）。
// 走主库：warmup 发生在启动期，replica 可能还没开连接；一致性也应当优先。
func (r *merchantRepo) ListActive(ctx context.Context, limit int) ([]*domain.Merchant, error) {
	if limit <= 0 || limit > 50_000 {
		limit = 1000
	}
	var out []*domain.Merchant
	err := r.db().WithContext(ctx).
		Where("status = ?", domain.MerchantStatusActive).
		Order("updated DESC").
		Limit(limit).Find(&out).Error
	return out, err
}

func (r *merchantRepo) ListDocuments(ctx context.Context, merchantID string) ([]*domain.MerchantKYCDocument, error) {
	var out []*domain.MerchantKYCDocument
	err := r.dbRO().WithContext(ctx).Where("merchant_id = ?", merchantID).
		Order("created DESC").Find(&out).Error
	return out, err
}

func (r *merchantRepo) ReviewDocument(ctx context.Context, id, status, note string) error {
	return r.db().WithContext(ctx).Model(&domain.MerchantKYCDocument{}).
		Where("id = ?", id).
		Updates(map[string]any{"review_status": status, "review_note": note}).Error
}

// ─── audit trail ─────────────────────────────────────────────────────────────

func (r *merchantRepo) ListKYCAudits(ctx context.Context, merchantID string, limit int) ([]*domain.MerchantKYCAudit, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []*domain.MerchantKYCAudit
	err := r.dbRO().WithContext(ctx).Where("merchant_id = ?", merchantID).
		Order("created DESC").Limit(limit).Find(&out).Error
	return out, err
}

