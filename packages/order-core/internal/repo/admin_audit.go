package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/sharding"
)

const adminAuditTable = "admin_audit_log"

// AdminAuditRepository append-only log of admin actions.
//
// **分片**：admin_audit_log 已迁到 shard（10 库 × 10 表 = 100 张），
// 按 actor (admin user id) 哈希路由（RouteByString）。同一 admin 的所有
// 操作落同一 shard，取证 / 客服查全。
//
// **跨 shard List**：filter 没指定 actor 时退化为 fan-out（10 shard 各
// SELECT 然后内存合并）。生产 admin UI 一般会带 actor / target，
// fan-out 是少数用户的少数请求，不是热路径。
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

type adminAuditRepo struct {
	mgr    *Manager
	router *sharding.Router
}

// NewAdminAuditRepository 构造
func NewAdminAuditRepository(mgr *Manager, router *sharding.Router) AdminAuditRepository {
	return &adminAuditRepo{mgr: mgr, router: router}
}

// Insert 按 actor 路由到 shard 表。
func (r *adminAuditRepo) Insert(ctx context.Context, a *domain.AdminAuditLog) error {
	if a.Actor == "" || a.Action == "" {
		return fmt.Errorf("%w: actor and action required", domain.ErrValidation)
	}
	// 限 body 大小，防恶意 actor flood DOS。
	const maxBody = 16 * 1024
	if len(a.RequestBody) > maxBody {
		a.RequestBody = a.RequestBody[:maxBody] + "...[truncated]"
	}
	dbIdx, tblIdx := r.router.RouteByString(a.Actor)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return fmt.Errorf("admin_audit_log shard %d: %w", dbIdx, err)
	}
	tbl := r.router.TableName(ctx, adminAuditTable, tblIdx)
	return db.WithContext(ctx).Table(tbl).Create(a).Error
}

// List 优先：actor 给定 → 单 shard query。否则 fan-out 全部 100 shard 合并。
func (r *adminAuditRepo) List(ctx context.Context, f AuditFilter) ([]*domain.AdminAuditLog, int64, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	// Fast path：actor 给定，单 shard 直查
	if f.Actor != "" {
		dbIdx, tblIdx := r.router.RouteByString(f.Actor)
		db, err := r.mgr.GetShard(dbIdx)
		if err != nil {
			return nil, 0, fmt.Errorf("admin_audit_log shard %d: %w", dbIdx, err)
		}
		tbl := r.router.TableName(ctx, adminAuditTable, tblIdx)
		return r.queryOne(ctx, db, tbl, f, limit)
	}

	// Fan-out：扫所有 100 shard。生产 admin UI 几乎总会带 actor，这里仅
	// 兜底（"全平台 audit 时间序"看板）。limit 等比缩小到 limit/100 防止
	// 内存爆。
	perShard := limit / 100
	if perShard < 5 {
		perShard = 5
	}
	type shardRes struct {
		rows  []*domain.AdminAuditLog
		total int64
	}
	results := make(chan shardRes, 100)
	for di := 0; di < r.router.DBCount(); di++ {
		db, err := r.mgr.GetShard(di)
		if err != nil {
			continue
		}
		for ti := 0; ti < r.router.TablePerDB(); ti++ {
			tblIdx := di*r.router.TablePerDB() + ti
			tbl := r.router.TableName(ctx, adminAuditTable, tblIdx)
			go func(db *gorm.DB, tbl string) {
				rows, total, _ := r.queryOne(ctx, db, tbl, f, perShard)
				results <- shardRes{rows: rows, total: total}
			}(db, tbl)
		}
	}
	merged := make([]*domain.AdminAuditLog, 0, limit)
	var totalSum int64
	expected := r.router.DBCount() * r.router.TablePerDB()
	for i := 0; i < expected; i++ {
		res := <-results
		merged = append(merged, res.rows...)
		totalSum += res.total
	}
	// 内存按 created desc 排序 + 取 limit
	sortByCreatedDesc(merged)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, totalSum, nil
}

func (r *adminAuditRepo) queryOne(ctx context.Context, db *gorm.DB, tbl string, f AuditFilter, limit int) ([]*domain.AdminAuditLog, int64, error) {
	q := db.WithContext(ctx).Table(tbl)
	if f.Actor != "" {
		q = q.Where("actor = ?", f.Actor)
	}
	if f.Action != "" {
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

// sortByCreatedDesc 内存排序：fan-out 合并后按 created desc。
func sortByCreatedDesc(rows []*domain.AdminAuditLog) {
	// 简单插入排序（n ~ limit；fan-out 一般 100*5=500，OK）。
	for i := 1; i < len(rows); i++ {
		j := i
		for j > 0 && rows[j-1].Created.Before(rows[j].Created) {
			rows[j-1], rows[j] = rows[j], rows[j-1]
			j--
		}
	}
}
