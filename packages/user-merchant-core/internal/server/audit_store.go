package server

import (
	"context"

	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/internal/repo"
	"github.com/xiongwp/user-merchant-core/internal/auditstore"
)

// auditStoreAdapter 把 repo.AuditRepository 包成 pkg/auditstore.AuditStore；
// pkg 层不依赖 internal/domain，所以字段一个个搬。
type auditStoreAdapter struct{ r repo.AuditRepository }

// NewAuditStore 构造 adapter
func NewAuditStore(r repo.AuditRepository) auditstore.AuditStore {
	return &auditStoreAdapter{r: r}
}

func (a *auditStoreAdapter) LastHash(ctx context.Context, actor string) (string, error) {
	return a.r.LastHash(ctx, actor)
}

func (a *auditStoreAdapter) Insert(ctx context.Context, e *auditstore.AuditEntry) error {
	return a.r.Insert(ctx, &domain.AdminAuditLog{
		Actor:       e.Actor,
		ActorIP:     e.ActorIP,
		Method:      e.Method,
		TargetID:    e.TargetID,
		RequestBody: e.RequestBody,
		StatusCode:  e.StatusCode,
		ResponseErr: e.ResponseErr,
		DurationMs:  e.DurationMs,
		TraceID:     e.TraceID,
		PrevHash:    e.PrevHash,
		RowHash:     e.RowHash,
	})
}

// idempotencyStoreAdapter 同样把 repo 翻成拦截器需要的小接口。
type idempotencyStoreAdapter struct{ r repo.IdempotencyRepository }

// NewIdempotencyStore 构造
func NewIdempotencyStore(r repo.IdempotencyRepository) auditstore.IdempotencyStore {
	return &idempotencyStoreAdapter{r: r}
}

func (a *idempotencyStoreAdapter) Get(ctx context.Context, key, method string) (*auditstore.IdempotencyRecord, bool, error) {
	rec, ok, err := a.r.Get(ctx, key, method)
	if err != nil || !ok {
		return nil, ok, err
	}
	return &auditstore.IdempotencyRecord{
		Key:          rec.Key,
		Method:       rec.Method,
		RequestHash:  rec.RequestHash,
		StatusCode:   rec.StatusCode,
		ResponseBody: rec.ResponseBody,
		ResponseErr:  rec.ResponseErr,
		Expires:      rec.Expires,
	}, true, nil
}

func (a *idempotencyStoreAdapter) TryInsert(ctx context.Context, rec *auditstore.IdempotencyRecord) error {
	err := a.r.TryInsert(ctx, &domain.IdempotencyRecord{
		Key:          rec.Key,
		Method:       rec.Method,
		RequestHash:  rec.RequestHash,
		StatusCode:   rec.StatusCode,
		ResponseBody: rec.ResponseBody,
		ResponseErr:  rec.ResponseErr,
		Expires:      rec.Expires,
	})
	if err == repo.ErrIdempotencyExists {
		return auditstore.ErrIdempotencyExists
	}
	return err
}
