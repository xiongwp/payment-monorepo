// admin.go：service 层暴露给 admin UI handler 的方法 + DTO。
//
// 设计纪律：
//   - admin handler 永远不直接 import repo（避免循环 + 类型耦合）；
//     所有 admin 查询走 service.Service，service 内部 delegate 到 Repo
//   - DTO 类型在本包定义；repo 实现转换并填值。
//
// admin handler 用到的全部方法都在这里集中：SearchItems / ListNamespace /
// ListVersions / GetActiveAdmin / GetSubscribers / SetSubscribers /
// GetVersion / RecentAudit。
package service

import (
	"context"
	"time"
)

// ─── DTO ──────────────────────────────────────────────────────────────────

// ConfigItemView admin 列表 / 单 namespace 视图行。
type ConfigItemView struct {
	ID            int64
	Namespace     string
	KeyName       string
	ActiveVersion int64
	LatestVersion int64
	UpdatedAt     time.Time
}

// ConfigAuditEntry admin /audit 页一行。
type ConfigAuditEntry struct {
	ID            int64
	Namespace     string
	KeyName       string
	Op            string
	VersionBefore *int64
	VersionAfter  *int64
	Actor         string
	ActorIP       string
	ChangeReason  string
	TraceID       string
	CreatedAt     time.Time
}

// ─── service.Repo extension ───────────────────────────────────────────────
//
// 把 admin 用到的方法补到 Repo 接口；repo 实现已就位（见 internal/repo/repo.go）。

// AdminRepo 把 admin 用 / 业务非 hot-path 用的查询接口集中。
//
// repo.*Repo 实现这个接口；service.Service 持有 AdminRepo 并提供 wrapper。
type AdminRepo interface {
	Repo
	SearchItems(ctx context.Context, q, subscriber string, limit int) ([]*ConfigItemView, error)
	ListItemsByNamespace(ctx context.Context, namespace string) ([]*ConfigItemView, error)
	GetVersion(ctx context.Context, id int64) (*ConfigRow, error)
	FindItemID(ctx context.Context, namespace, key string) (int64, error)
	GetSubscribers(ctx context.Context, itemID int64) ([]string, error)
	SetSubscribers(ctx context.Context, itemID int64, subscribers []string, actor string) error
	RecentAudit(ctx context.Context, limit int) ([]*ConfigAuditEntry, error)
	EnsureNamespace(ctx context.Context, name, owner string) error
}

// ─── service.Service admin wrappers ──────────────────────────────────────

// adminRepo lazy 类型断言；非 AdminRepo 的实现（mock 测试）调时直接报错。
func (s *Service) adminRepo() AdminRepo {
	if a, ok := s.repo.(AdminRepo); ok {
		return a
	}
	return nil
}

// SearchItems 全平台 item 检索。
func (s *Service) SearchItems(ctx context.Context, q, sub string, limit int) ([]*ConfigItemView, error) {
	a := s.adminRepo()
	if a == nil {
		return nil, nil
	}
	return a.SearchItems(ctx, q, sub, limit)
}

// ListNamespace 单 namespace 全部 item。
func (s *Service) ListNamespace(ctx context.Context, namespace string) ([]*ConfigItemView, error) {
	a := s.adminRepo()
	if a == nil {
		return nil, nil
	}
	return a.ListItemsByNamespace(ctx, namespace)
}

// ListVersions 单 (ns,key) 历史。
func (s *Service) ListVersions(ctx context.Context, namespace, key string, limit int) ([]*ConfigRow, error) {
	return s.repo.ListVersions(ctx, namespace, key, limit)
}

// GetActiveAdmin 跟 GetConfig 不同：不做 strategy / 生效窗口过滤，
// admin 永远看得到最新 active 行（哪怕 instance 不命中）。
func (s *Service) GetActiveAdmin(ctx context.Context, namespace, key string) (*ConfigRow, error) {
	return s.repo.GetActive(ctx, namespace, key)
}

// GetVersion 单 version 行。
func (s *Service) GetVersion(ctx context.Context, namespace, key string, version int64) (*ConfigRow, error) {
	a := s.adminRepo()
	if a == nil {
		return nil, nil
	}
	// 先查 (ns,key,version) → ConfigVersion.id；再 GetVersion(id)
	rows, err := s.repo.ListVersions(ctx, namespace, key, 200)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Version == version {
			return r, nil
		}
	}
	return nil, nil
}

// GetSubscribers 当前订阅服务名列表。
func (s *Service) GetSubscribers(ctx context.Context, namespace, key string) ([]string, error) {
	a := s.adminRepo()
	if a == nil {
		return nil, nil
	}
	itemID, err := a.FindItemID(ctx, namespace, key)
	if err != nil || itemID == 0 {
		return nil, err
	}
	return a.GetSubscribers(ctx, itemID)
}

// SetSubscribers 重置订阅清单。
func (s *Service) SetSubscribers(ctx context.Context, namespace, key string, subscribers []string) error {
	a := s.adminRepo()
	if a == nil {
		return nil
	}
	itemID, err := a.FindItemID(ctx, namespace, key)
	if err != nil || itemID == 0 {
		return err
	}
	actor := "" // ctx 注入；admin handler 已从 actor 上下文里拿
	if v := ctx.Value(actorContextKey); v != nil {
		if str, ok := v.(string); ok {
			actor = str
		}
	}
	return a.SetSubscribers(ctx, itemID, subscribers, actor)
}

// RecentAudit 时间序最近 N 条 audit。
func (s *Service) RecentAudit(ctx context.Context, limit int) ([]*ConfigAuditEntry, error) {
	a := s.adminRepo()
	if a == nil {
		return nil, nil
	}
	return a.RecentAudit(ctx, limit)
}

// actorContextKey ctx 里 admin actor 的 key（admin handler middleware 注）；
// 跟 server.actorCtxKey 等价但本包不能反向 import server。
type ctxKey struct{ name string }

var actorContextKey = ctxKey{"actor"}

// WithActor caller (admin middleware) 用；service 内 SetSubscribers 之类查 actor。
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorContextKey, actor)
}
