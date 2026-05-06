package repo

import (
	"context"

	"gorm.io/gorm"

	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/internal/sharding"
)

const tblAuditLog = "admin_audit_log"

// AuditRepository append-only 审计日志；只提供 Insert + List（无 Update/Delete）。
//
// **分片**：admin_audit_log 已从 meta 迁到 shard（10 库 × 10 表 = 100 张），
// 按 actor 哈希路由（同 actor 同 shard）。链式签名 per-shard：LastHash 现在
// 接受 actor 参数定位该 actor 所在 shard 的最新链头。
type AuditRepository interface {
	// Insert 写入一条；prev_hash 由调用方先调 LastHash(actor) 取。
	Insert(ctx context.Context, row *domain.AdminAuditLog) error
	// LastHash 取该 actor 所在 shard 最后一行 row_hash；空表返回 ""。
	LastHash(ctx context.Context, actor string) (string, error)
	// List 按 actor / target / time 过滤，分页返回。actor 给定 → 单 shard fast path；
	// 否则 fan-out 100 shard 内存合并。
	List(ctx context.Context, actor, target string, limit, offset int) ([]*domain.AdminAuditLog, error)
}

type auditRepo struct {
	mgr    *Manager
	router *sharding.Router
}

// NewAuditRepository 构造
func NewAuditRepository(mgr *Manager, router *sharding.Router) AuditRepository {
	return &auditRepo{mgr: mgr, router: router}
}

// shardForActor 返回 (db, tableName)；ctx 决定是否加 _shadow 后缀。
// actor 空时落 shard 0 / table 0（系统兜底，但实际调用方都应有 actor）。
func (r *auditRepo) shardForActor(ctx context.Context, actor string) (*gorm.DB, string) {
	dbIdx, tblIdx := r.router.RouteByString(actor)
	db := r.mgr.GetShard(dbIdx)
	tbl := r.router.TableName(ctx, tblAuditLog, tblIdx)
	return db, tbl
}

func (r *auditRepo) Insert(ctx context.Context, row *domain.AdminAuditLog) error {
	db, tbl := r.shardForActor(ctx, row.Actor)
	if db == nil {
		return gorm.ErrInvalidDB
	}
	return db.WithContext(ctx).Table(tbl).Create(row).Error
}

func (r *auditRepo) LastHash(ctx context.Context, actor string) (string, error) {
	db, tbl := r.shardForActor(ctx, actor)
	if db == nil {
		return "", nil
	}
	var row domain.AdminAuditLog
	err := db.WithContext(ctx).Table(tbl).Order("id DESC").Limit(1).First(&row).Error
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
	// fast path: actor 给定，直接命中单 shard
	if actor != "" {
		db, tbl := r.shardForActor(ctx, actor)
		if db == nil {
			return nil, nil
		}
		q := db.WithContext(ctx).Table(tbl).Where("actor = ?", actor)
		if target != "" {
			q = q.Where("target_id = ?", target)
		}
		var out []*domain.AdminAuditLog
		err := q.Order("id DESC").Limit(limit).Offset(offset).Find(&out).Error
		return out, err
	}
	// fan-out：扫所有 100 shard。仅运维 dashboard 全平台 audit time-series 用，
	// 不在热路径。
	perShard := limit / 100
	if perShard < 5 {
		perShard = 5
	}
	merged := make([]*domain.AdminAuditLog, 0, limit)
	for di := 0; di < r.router.DBCount(); di++ {
		db := r.mgr.GetShard(di)
		if db == nil {
			continue
		}
		for ti := 0; ti < r.router.TablePerDB(); ti++ {
			tblIdx := di*r.router.TablePerDB() + ti
			tbl := r.router.TableName(ctx, tblAuditLog, tblIdx)
			q := db.WithContext(ctx).Table(tbl)
			if target != "" {
				q = q.Where("target_id = ?", target)
			}
			var rows []*domain.AdminAuditLog
			if err := q.Order("id DESC").Limit(perShard).Find(&rows).Error; err != nil {
				continue
			}
			merged = append(merged, rows...)
		}
	}
	// 内存按 created desc 排序 + 截 limit
	for i := 1; i < len(merged); i++ {
		j := i
		for j > 0 && merged[j-1].Created.Before(merged[j].Created) {
			merged[j-1], merged[j] = merged[j], merged[j-1]
			j--
		}
	}
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}
