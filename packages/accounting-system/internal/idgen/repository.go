package idgen

import (
	"context"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// segmentRepository 号段仓储（使用 *gorm.DB 直接操作，不做分库分表）
type segmentRepository struct {
	db *gorm.DB
}

func newSegmentRepository(db *gorm.DB) *segmentRepository {
	return &segmentRepository{db: db}
}

func (r *segmentRepository) getAllBizTags(ctx context.Context) ([]LeafAlloc, error) {
	var allocs []LeafAlloc
	if err := r.db.WithContext(ctx).Find(&allocs).Error; err != nil {
		return nil, fmt.Errorf("idgen: getAllBizTags: %w", err)
	}
	return allocs, nil
}

func (r *segmentRepository) getByBizTag(ctx context.Context, bizTag string) (*LeafAlloc, error) {
	var alloc LeafAlloc
	result := r.db.WithContext(ctx).Where("biz_tag = ?", bizTag).First(&alloc)
	if result.Error != nil {
		if result.Error == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("idgen: getByBizTag(%s): %w", bizTag, result.Error)
	}
	return &alloc, nil
}

// allocNextSegment 原子推进 max_id 并返回更新后的行。
// UPDATE leaf_alloc SET max_id = max_id + step WHERE biz_tag = ? → SELECT FOR UPDATE
func (r *segmentRepository) allocNextSegment(ctx context.Context, bizTag string) (*LeafAlloc, error) {
	var alloc LeafAlloc
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&LeafAlloc{}).
			Where("biz_tag = ?", bizTag).
			Update("max_id", gorm.Expr("max_id + step")).Error; err != nil {
			return fmt.Errorf("update max_id: %w", err)
		}
		return tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("biz_tag = ?", bizTag).
			First(&alloc).Error
	})
	if err != nil {
		return nil, fmt.Errorf("idgen: allocNextSegment(%s): %w", bizTag, err)
	}
	return &alloc, nil
}

// register 注册 biz_tag（不存在时插入，存在时忽略）
func (r *segmentRepository) register(ctx context.Context, alloc *LeafAlloc) error {
	return r.db.WithContext(ctx).
		Where("biz_tag = ?", alloc.BizTag).
		FirstOrCreate(alloc).Error
}
