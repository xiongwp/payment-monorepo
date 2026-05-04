// Package sharding 实现 10 库 × 10 表的路由器。形状与 order-core 完全相同，
// 这样同一 pi_id 在两个仓库落同一逻辑分片号，跨仓 join 时拼 SQL 省心。
package sharding

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	DefaultShardDBCount = 10
	DefaultTablePerDB   = 10
)

// Router 分库分表路由器
type Router struct {
	dbCount    int
	tablePerDB int
}

func NewRouter() *Router {
	return &Router{dbCount: DefaultShardDBCount, tablePerDB: DefaultTablePerDB}
}

func NewRouterWithConfig(dbCount, tablePerDB int) *Router {
	if dbCount <= 0 {
		dbCount = DefaultShardDBCount
	}
	if tablePerDB <= 0 {
		tablePerDB = DefaultTablePerDB
	}
	return &Router{dbCount: dbCount, tablePerDB: tablePerDB}
}

func (r *Router) DBCount() int         { return r.dbCount }
func (r *Router) TablePerDB() int      { return r.tablePerDB }
func (r *Router) TotalTableCount() int { return r.dbCount * r.tablePerDB }

func (r *Router) RouteByID(id int64) (dbIndex, tableIndex int) {
	if id < 0 {
		id = -id
	}
	total := int64(r.dbCount * r.tablePerDB)
	n := int(id % total)
	return n / r.tablePerDB, n
}

// RouteByIDHashed 用 FNV-1a mix 后再取模，对**有结构性聚集**的 numeric ID 更均匀。
// 典型场景：bit-encoded ID（account_id / transaction_id）的低位 seq 可能集中（leaf
// 段连续发号），直接 id%100 在初始化阶段会让前几个 shard 被打爆。
//
// 用法：
//   - user_id（leaf 发号、稀疏分布）→ 用 RouteByID 即可
//   - bit-encoded account_id / 类似有 positional 字段的 ID → 优先用专用解码（如
//     shadow.AccountIDDBIndex），fallback 用 RouteByIDHashed
func (r *Router) RouteByIDHashed(id int64) (dbIndex, tableIndex int) {
	if id < 0 {
		id = -id
	}
	// FNV-1a 64-bit
	const (
		offset64 uint64 = 14695981039346656037
		prime64  uint64 = 1099511628211
	)
	h := offset64
	u := uint64(id)
	for i := 0; i < 8; i++ {
		h ^= u & 0xff
		h *= prime64
		u >>= 8
	}
	total := uint64(r.dbCount * r.tablePerDB)
	n := int(h % total)
	return n / r.tablePerDB, n
}

func (r *Router) RouteByString(s string) (dbIndex, tableIndex int) {
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

// RouteByPrefixedID 解析 "<prefix>_{dbIdx:1d}{tblIdx:02d}<seq>"
func (r *Router) RouteByPrefixedID(id string) (dbIndex, tableIndex int) {
	idx := strings.IndexByte(id, '_')
	body := id
	if idx >= 0 && idx < len(id)-1 {
		body = id[idx+1:]
	}
	if len(body) >= 3 {
		if db, err := strconv.Atoi(body[:1]); err == nil {
			if tbl, err := strconv.Atoi(body[1:3]); err == nil {
				total := r.dbCount * r.tablePerDB
				if db < r.dbCount && tbl < total && tbl/r.tablePerDB == db {
					return db, tbl
				}
				if tbl < total {
					return tbl / r.tablePerDB, tbl
				}
			}
		}
	}
	return r.RouteByString(id)
}

func (r *Router) FormatID(prefix string, dbIndex, tableIndex int, seq int64) string {
	return fmt.Sprintf("%s_%d%02d%d", prefix, dbIndex, tableIndex, seq)
}

func (r *Router) GetTableName(base string, tableIndex int) string {
	return fmt.Sprintf("%s_%02d", base, tableIndex)
}

func (r *Router) AllShards() [][2]int {
	out := make([][2]int, 0, r.dbCount*r.tablePerDB)
	for db := 0; db < r.dbCount; db++ {
		for off := 0; off < r.tablePerDB; off++ {
			out = append(out, [2]int{db, db*r.tablePerDB + off})
		}
	}
	return out
}
