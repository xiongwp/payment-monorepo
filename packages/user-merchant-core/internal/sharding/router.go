// Package sharding 实现 user-merchant-core 的 10 库 × 10 表 = 100 张全局分片表
// 路由器。形状跟 accounting-system / order-core / payment-channel 完全对齐，
// 这样跨服务用同一 user_id / merchant_id 落同一逻辑分片号，跨服务 join / 排查
// 时拼 SQL 省心。
package sharding

import (
	"context"
	"fmt"
	"strconv"

	"github.com/xiongwp/payment-util/shadow"
)

const (
	// ShardDBCount 分库数量
	ShardDBCount = 10
	// ShardTablePerDB 每库分表数量
	ShardTablePerDB = 10
	// ShardTableTotal 全局总表数量（= ShardDBCount × ShardTablePerDB）
	ShardTableTotal = ShardDBCount * ShardTablePerDB
)

// Router 分库分表路由器。
//
// 路由规则（10 库 × 每库 10 表）：
//
//	n = id % 100
//	dbIndex        = n / 10      （0-9，对应物理库 user_merchant_db_N）
//	globalTableIdx = n           （0-99，全局表序号，落 users_NN / merchants_NN 等）
type Router struct {
	dbCount    int
	tablePerDB int
}

// NewRouter 创建路由器（生产默认：10 库 × 10 表）
func NewRouter() *Router {
	return &Router{dbCount: ShardDBCount, tablePerDB: ShardTablePerDB}
}

// NewRouterWithConfig 创建自定义分片路由器（测试 / 低容量场景）
func NewRouterWithConfig(dbCount, tablePerDB int) *Router {
	return &Router{dbCount: dbCount, tablePerDB: tablePerDB}
}

// DBCount / TablePerDB / TotalTableCount accessors
func (r *Router) DBCount() int         { return r.dbCount }
func (r *Router) TablePerDB() int      { return r.tablePerDB }
func (r *Router) TotalTableCount() int { return r.dbCount * r.tablePerDB }

// RouteByID 按数字 ID 路由（user_id / merchant_id 都用这个）。
//
//	n = id % (dbCount × tablePerDB)
func (r *Router) RouteByID(id int64) (dbIndex, globalTableIndex int) {
	if id < 0 {
		id = -id
	}
	total := int64(r.dbCount * r.tablePerDB)
	n := int(id % total)
	return n / r.tablePerDB, n
}

// RouteByUserID alias，让调用方意图更清晰。
func (r *Router) RouteByUserID(userID int64) (dbIndex, globalTableIndex int) {
	return r.RouteByID(userID)
}

// RouteByMerchantID alias。merchant_id 是 shadow.EncodeID(IDTypeMerchantID, ...)
// 编码出的字符串，需要先 ParseInt 再路由。
func (r *Router) RouteByMerchantID(merchantID string) (dbIndex, globalTableIndex int) {
	id, err := strconv.ParseInt(merchantID, 10, 64)
	if err != nil {
		return 0, 0
	}
	return r.RouteByID(id)
}

// RouteByString FNV-1a 哈希字符串后路由。
//
// 给 actor / trace_id 等非数字 key 用（admin audit_log 按 actor 路由时
// actor 可能是 "admin_42" / "svc-bot" 这种非纯数字串，需要哈希）。
// 跟 order-core/internal/sharding/router.go 的实现保持一致。
func (r *Router) RouteByString(s string) (dbIndex, globalTableIndex int) {
	if s == "" {
		return 0, 0
	}
	const (
		offset64 uint64 = 14695981039346656037
		prime64  uint64 = 1099511628211
	)
	h := offset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return r.RouteByID(int64(h & 0x7fffffffffffffff))
}

// TableName 按 ctx + (base, globalTblIdx) 解出最终表名（含 shadow 后缀）。
//
// 主流量：base + "_" + idx → "users_42"
// shadow：base + "_" + idx + "_shadow" → "users_42_shadow"
func (r *Router) TableName(ctx context.Context, base string, globalTableIdx int) string {
	t := fmt.Sprintf("%s_%02d", base, globalTableIdx)
	return shadow.TableName(ctx, t)
}

// TableNameByUserID 一步到位：从 user_id 解到最终表名。
func (r *Router) TableNameByUserID(ctx context.Context, base string, userID int64) string {
	_, idx := r.RouteByUserID(userID)
	return r.TableName(ctx, base, idx)
}

// TableNameByMerchantID 一步到位：从 merchant_id 解到最终表名。
func (r *Router) TableNameByMerchantID(ctx context.Context, base string, merchantID string) string {
	_, idx := r.RouteByMerchantID(merchantID)
	return r.TableName(ctx, base, idx)
}

// ShardSpec 单分片元信息（schema migrator / batchtask 等扫全分片场景用）。
type ShardSpec struct {
	DBIndex     int
	TableIndex  int
}

// AllShards 返回所有 (dbIdx, globalTblIdx) 组合（迭代 100 张分片表用）。
func (r *Router) AllShards() []ShardSpec {
	out := make([]ShardSpec, 0, r.dbCount*r.tablePerDB)
	for i := 0; i < r.dbCount; i++ {
		for j := 0; j < r.tablePerDB; j++ {
			out = append(out, ShardSpec{DBIndex: i, TableIndex: i*r.tablePerDB + j})
		}
	}
	return out
}
