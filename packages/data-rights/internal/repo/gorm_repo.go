// Package repo — GORM repository 实现 store.Store 接口.
//
// 替换原 store.MemStore — 数据真正落 MySQL, 重启不丢.
//
// 用法 (main.go):
//   db, _ := scaffold.NewDB(cfg.DB, log, &domain.RequestGormModel{}, &domain.ServiceStatusGormModel{})
//   repo := repo.NewGormRepo(db.GORM(), log)

package repo

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"reconcile-system/packages/data-rights/internal/domain"
	"reconcile-system/packages/data-rights/internal/store"
)

// GormRepo 实现 store.Store.
type GormRepo struct {
	db  *gorm.DB
	log *zap.Logger
}

func NewGormRepo(db *gorm.DB, log *zap.Logger) *GormRepo {
	return &GormRepo{db: db, log: log}
}

func (r *GormRepo) SaveRequest(req domain.Request) error {
	m := domain.FromDomainRequest(req)
	return r.db.WithContext(context.Background()).Save(&m).Error
}

func (r *GormRepo) GetRequest(id string) (domain.Request, error) {
	var m domain.RequestGormModel
	err := r.db.WithContext(context.Background()).
		Where("request_id = ?", id).
		First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.Request{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Request{}, err
	}
	out := m.ToDomain()

	// 拼 service_statuses
	var statuses []domain.ServiceStatusGormModel
	if err := r.db.Where("request_id = ?", id).Find(&statuses).Error; err != nil {
		r.log.Warn("load service statuses", zap.Error(err))
	}
	for _, s := range statuses {
		out.ServiceStatuses = append(out.ServiceStatuses, domain.ServiceStatus{
			Service:       s.Service,
			Endpoint:      s.Endpoint,
			State:         domain.State(s.State),
			Attempts:      s.Attempts,
			LastAttemptAt: s.LastAttemptAt,
			Error:         s.Error,
			ExportSize:    s.ExportSize,
			ExportSHA:     s.ExportSHA,
			Held:          s.Held,
			HoldReason:    s.HoldReason,
		})
	}
	return out, nil
}

func (r *GormRepo) ListRequests(f store.ListFilter) ([]domain.Request, error) {
	q := r.db.WithContext(context.Background()).Model(&domain.RequestGormModel{})
	if f.State != "" {
		q = q.Where("state = ?", string(f.State))
	}
	if f.Type != "" {
		q = q.Where("type = ?", string(f.Type))
	}
	if f.Overdue {
		q = q.Where("state NOT IN (?, ?, ?)", "fulfilled", "rejected", "cancelled").
			Where("deadline_at < ?", time.Now().UTC())
	}
	q = q.Order("submitted_at DESC")
	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}
	if f.Offset > 0 {
		q = q.Offset(f.Offset)
	}
	var ms []domain.RequestGormModel
	if err := q.Find(&ms).Error; err != nil {
		return nil, err
	}
	out := make([]domain.Request, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ToDomain())
	}
	return out, nil
}

func (r *GormRepo) UpdateServiceStatus(reqID string, st domain.ServiceStatus) error {
	m := domain.ServiceStatusGormModel{
		RequestID:     reqID,
		Service:       st.Service,
		Endpoint:      st.Endpoint,
		State:         string(st.State),
		Attempts:      st.Attempts,
		LastAttemptAt: st.LastAttemptAt,
		Error:         st.Error,
		ExportSize:    st.ExportSize,
		ExportSHA:     st.ExportSHA,
		Held:          st.Held,
		HoldReason:    st.HoldReason,
	}
	return r.db.Save(&m).Error
}

func (r *GormRepo) UpdateState(reqID string, state domain.State, by string) error {
	updates := map[string]interface{}{"state": string(state)}
	switch state {
	case domain.StateApproved:
		updates["approved_at"] = time.Now().UTC()
		updates["approved_by"] = by
	case domain.StateRejected:
		updates["reject_reason"] = by // by 在 reject 时是 reason
	case domain.StateFulfilled:
		updates["fulfilled_at"] = time.Now().UTC()
	}
	res := r.db.Model(&domain.RequestGormModel{}).
		Where("request_id = ?", reqID).Updates(updates)
	if res.RowsAffected == 0 {
		return store.ErrNotFound
	}
	return res.Error
}

func (r *GormRepo) SetExport(reqID, url, sha string) error {
	res := r.db.Model(&domain.RequestGormModel{}).
		Where("request_id = ?", reqID).
		Updates(map[string]interface{}{
			"export_url":    url,
			"export_sha256": sha,
		})
	if res.RowsAffected == 0 {
		return store.ErrNotFound
	}
	return res.Error
}
