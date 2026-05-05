// Package sharding 实现 card-center 的 10 库 × 10 表 = 100 分片路由。
// 跟 user-merchant-core / order-core / payment-channel layout 完全对齐：
// 同一 user_id 在不同服务里落同一逻辑分片号，跨服务 join / 排查时拼 SQL 省心。
package sharding

import (
	"context"
	"fmt"
	"strconv"

	"github.com/xiongwp/payment-util/shadow"
)

const (
	ShardDBCount    = 10
	ShardTablePerDB = 10
	ShardTableTotal = ShardDBCount * ShardTablePerDB
)

type Router struct {
	dbCount    int
	tablePerDB int
}

func NewRouter() *Router {
	return &Router{dbCount: ShardDBCount, tablePerDB: ShardTablePerDB}
}

func (r *Router) DBCount() int         { return r.dbCount }
func (r *Router) TablePerDB() int      { return r.tablePerDB }
func (r *Router) TotalTableCount() int { return r.dbCount * r.tablePerDB }

// RouteByID 数字 ID（user_id）路由
func (r *Router) RouteByID(id int64) (dbIndex, globalTableIndex int) {
	if id < 0 {
		id = -id
	}
	total := int64(r.dbCount * r.tablePerDB)
	n := int(id % total)
	return n / r.tablePerDB, n
}

// RouteByUserID alias
func (r *Router) RouteByUserID(userID int64) (int, int) { return r.RouteByID(userID) }

// RouteByPIID pi_id 是字符串前缀 ID，需要解出尾部数字段。pi_xxx 之类。
// 规则：pi_id 的下划线后 hex/数字段做 hash → mod 100。
func (r *Router) RouteByPIID(piID string) (int, int) {
	// 简化版：直接按整串 fnv 哈希。生产可换成 RouteByPrefixedID 跟 payment-channel 对齐。
	const (
		offset64 uint64 = 14695981039346656037
		prime64  uint64 = 1099511628211
	)
	h := offset64
	for i := 0; i < len(piID); i++ {
		h ^= uint64(piID[i])
		h *= prime64
	}
	id := int64(h & 0x7fffffffffffffff)
	return r.RouteByID(id)
}

// RouteByTokenHash token_hash 的 sha256 hex 字符串，用前 8 字符做路由
func (r *Router) RouteByTokenHash(tokenHash string) (int, int) {
	if len(tokenHash) < 8 {
		return 0, 0
	}
	prefix := tokenHash[:8]
	if n, err := strconv.ParseInt(prefix, 16, 64); err == nil {
		return r.RouteByID(n)
	}
	return 0, 0
}

// TableName 按 ctx 决定主表 / shadow 表
func (r *Router) TableName(ctx context.Context, base string, globalTblIdx int) string {
	t := fmt.Sprintf("%s_%02d", base, globalTblIdx)
	return shadow.TableName(ctx, t)
}

// AllShards 给 schema migrator / 跨分片扫表用
type ShardSpec struct {
	DBIndex    int
	TableIndex int
}

func (r *Router) AllShards() []ShardSpec {
	out := make([]ShardSpec, 0, r.dbCount*r.tablePerDB)
	for i := 0; i < r.dbCount; i++ {
		for j := 0; j < r.tablePerDB; j++ {
			out = append(out, ShardSpec{DBIndex: i, TableIndex: i*r.tablePerDB + j})
		}
	}
	return out
}
