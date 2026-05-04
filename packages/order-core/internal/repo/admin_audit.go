package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xiongwp/order-core/internal/domain"
)

// AdminAuditRepository append-only log of admin actions.
type AdminAuditRepository interface {
	Insert(ctx context.Context, a *domain.AdminAuditLog) error
	List(ctx context.Context, filter AuditFilter) ([]*domain.AdminAuditLog, int64, error)
}

// AuditFilter scoping options. Empty strings = no filter.
type AuditFilter struct {
	Actor      string
	Action     string
	TargetType string
	TargetID   string
	Since      *time.Time
	Until      *time.Time
	Limit      int
	Offset     int
}

type adminAuditRepo struct{ mgr *Manager }

// NewAdminAuditRepository 构造
func NewAdminAuditRepository(mgr *Manager) AdminAuditRepository {
	return &adminAuditRepo{mgr: mgr}
}

func (r *adminAuditRepo) Insert(ctx context.Context, a *domain.AdminAuditLog) error {
	if a.Actor == "" || a.Action == "" {
		return fmt.Errorf("%w: actor and action required", domain.ErrValidation)
	}
	// Keep request_body bounded — one actor flooding the DB would be a DOS.
	const maxBody = 16 * 1024
	if len(a.RequestBody) > maxBody {
		a.RequestBody = a.RequestBody[:maxBody] + "...[truncated]"
	}
	return r.mgr.GetMeta().WithContext(ctx).Create(a).Error
}

func (r *adminAuditRepo) List(ctx context.Context, f AuditFilter) ([]*domain.AdminAuditLog, int64, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := r.mgr.GetMeta().WithContext(ctx).Model(&domain.AdminAuditLog{})
	if f.Actor != "" {
		q = q.Where("actor = ?", f.Actor)
	}
	if f.Action != "" {
		// Prefix match for action taxonomies like merchant.* / risk.*
		if strings.HasSuffix(f.Action, "*") {
			q = q.Where("action LIKE ?", strings.TrimSuffix(f.Action, "*")+"%")
		} else {
			q = q.Where("action = ?", f.Action)
		}
	}
	if f.TargetType != "" {
		q = q.Where("target_type = ?", f.TargetType)
	}
	if f.TargetID != "" {
		q = q.Where("target_id = ?", f.TargetID)
	}
	if f.Since != nil {
		q = q.Where("created >= ?", *f.Since)
	}
	if f.Until != nil {
		q = q.Where("created < ?", *f.Until)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []*domain.AdminAuditLog
	if err := q.Order("id DESC").Limit(limit).Offset(f.Offset).Find(&out).Error; err != nil {
		return nil, 0, err
	}
	return out, total, nil
}
