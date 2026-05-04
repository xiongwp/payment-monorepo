package idgen

import (
	"context"
	"fmt"

	"github.com/xiongwp/payment-util/shadow"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// leafTable 按 ctx 解析号段表名（leaf_alloc / leaf_alloc_shadow）。
const leafAllocBase = "leaf_alloc"

func leafTable(ctx context.Context) string { return shadow.TableName(ctx, leafAllocBase) }

// segmentRepository 号段仓储（使用 *gorm.DB 直接操作，不做分库分表）
type segmentRepository struct {
	db *gorm.DB
}

func newSegmentRepository(db *gorm.DB) *segmentRepository {
	return &segmentRepository{db: db}
}

func (r *segmentRepository) getAllBizTags(ctx context.Context) ([]LeafAlloc, error) {
	var allocs []LeafAlloc
	if err := r.db.WithContext(ctx).Table(leafTable(ctx)).Find(&allocs).Error; err != nil {
		return nil, fmt.Errorf("idgen: getAllBizTags: %w", err)
	}
	return allocs, nil
}

func (r *segmentRepository) getByBizTag(ctx context.Context, bizTag string) (*LeafAlloc, error) {
	var alloc LeafAlloc
	result := r.db.WithContext(ctx).Table(leafTable(ctx)).Where("biz_tag = ?", bizTag).First(&alloc)
	if result.Error != nil {
		if result.Error == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("idgen: getByBizTag(%s): %w", bizTag, result.Error)
	}
	return &alloc, nil
}

// allocNextSegment 原子推进 max_id 并返回更新后的行。
// 主流量走 leaf_alloc，影子流量走 leaf_alloc_shadow（号段独立递增，互不影响）。
func (r *segmentRepository) allocNextSegment(ctx context.Context, bizTag string) (*LeafAlloc, error) {
	tbl := leafTable(ctx)
	var alloc LeafAlloc
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Table(tbl).
			Where("biz_tag = ?", bizTag).
			Update("max_id", gorm.Expr("max_id + step")).Error; err != nil {
			return fmt.Errorf("update max_id: %w", err)
		}
		return tx.Table(tbl).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("biz_tag = ?", bizTag).
			First(&alloc).Error
	})
	if err != nil {
		return nil, fmt.Errorf("idgen: allocNextSegment(%s): %w", bizTag, err)
	}
	return &alloc, nil
}

// register 注册 biz_tag（不存在时插入，存在时忽略）。
// 用 INSERT … ON CONFLICT DO NOTHING 由 DB 原子处理，避免 First + Create
// 两步式 race window（多 goroutine 同时 Register 同 biz_tag 撞 UNIQUE）。
func (r *segmentRepository) register(ctx context.Context, alloc *LeafAlloc) error {
	return r.db.WithContext(ctx).Table(leafTable(ctx)).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(alloc).Error
}

// updateStep 调高 / 调低指定 bizTag 的 step（自适应）。仅当 newStep != 当前值时才 UPDATE。
// 主流量 / shadow 段独立调整（leaf_alloc vs leaf_alloc_shadow，由 ctx 路由）。
func (r *segmentRepository) updateStep(ctx context.Context, bizTag string, newStep int) error {
	return r.db.WithContext(ctx).Table(leafTable(ctx)).
		Where("biz_tag = ? AND step <> ?", bizTag, newStep).
		Update("step", newStep).Error
}
