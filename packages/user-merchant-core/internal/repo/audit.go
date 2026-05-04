package repo

import (
	"context"

	"gorm.io/gorm"

	"github.com/xiongwp/user-merchant-core/internal/domain"
)

// AuditRepository append-only 审计日志；只提供 Insert + List（无 Update/Delete）。
type AuditRepository interface {
	// Insert 写入一条；prev_hash 由调用方在事务里用 SELECT row_hash ORDER BY id DESC
	// 取上一行得到，保证链式签名连续。
	Insert(ctx context.Context, row *domain.AdminAuditLog) error
	// LastHash 取表里最后一行的 row_hash；空表返回 ""。
	LastHash(ctx context.Context) (string, error)
	// List 按 actor / target / time 过滤，分页返回。
	List(ctx context.Context, actor, target string, limit, offset int) ([]*domain.AdminAuditLog, error)
}

type auditRepo struct{ mgr *Manager }

// NewAuditRepository 构造
func NewAuditRepository(mgr *Manager) AuditRepository {
	return &auditRepo{mgr: mgr}
}

func (r *auditRepo) db() *gorm.DB   { return r.mgr.GetMeta() }
func (r *auditRepo) dbRO() *gorm.DB { return r.mgr.GetMetaRO() }

func (r *auditRepo) Insert(ctx context.Context, row *domain.AdminAuditLog) error {
	return r.db().WithContext(ctx).Create(row).Error
}

func (r *auditRepo) LastHash(ctx context.Context) (string, error) {
	var row domain.AdminAuditLog
	err := r.db().WithContext(ctx).Order("id DESC").Limit(1).First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return row.RowHash, nil
}

func (r *auditRepo) List(ctx context.Context, actor, target string, limit, offset int) ([]*domain.AdminAuditLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := r.dbRO().WithContext(ctx).Model(&domain.AdminAuditLog{})
	if actor != "" {
		q = q.Where("actor = ?", actor)
	}
	if target != "" {
		q = q.Where("target_id = ?", target)
	}
	var out []*domain.AdminAuditLog
	err := q.Order("id DESC").Limit(limit).Offset(offset).Find(&out).Error
	return out, err
}
