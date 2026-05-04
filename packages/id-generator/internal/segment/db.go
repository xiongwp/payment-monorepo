// Package segment 持久化号段：主流量从 id_segment 取，shadow 流量从 id_segment_shadow 取。
package segment

import (
	"context"

	"gorm.io/gorm"

	"github.com/xiongwp/payment-util/shadow"
)

const (
	tableMain   = "id_segment"
	tableShadow = "id_segment_shadow" // 与 init_shadow.sql 一致
)

// segmentTable 按 ctx 决定取号段表名。
func segmentTable(ctx context.Context) string {
	if shadow.IsShadow(ctx) {
		return tableShadow
	}
	return tableMain
}

// Fetch 取号段当前 max_id；按 ctx 走主表或影子表。
//
// 注意：原实现只 SELECT max_id，并没有把 max_id 推进。这是个简化（号段耗尽前
// Buffer 里的 ID 一直递增到 max；耗尽时 Buffer.Load 重新 Fetch 拿同一个
// max_id 又起一次）—— 真实生产应该是 UPDATE max_id = max_id + step + 拿回新值
// （Leaf 经典模式）。这里只补 ctx 路由，不改这个 bug，留作单独工单。
func Fetch(ctx context.Context, db *gorm.DB) (int64, error) {
	tbl := segmentTable(ctx)
	var maxID int64

	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return tx.Raw("SELECT max_id FROM `" + tbl + "` LIMIT 1").Scan(&maxID).Error
	})
	if err != nil {
		return 0, err
	}
	return maxID, nil
}
