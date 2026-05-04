// Package sharding 实现 10 库 × 10 表的路由器。
//
// 路由规则（dbCount=10, tablePerDB=10，总 100 张全局表）：
//
//	n              = id % (dbCount × tablePerDB)
//	dbIndex        = n / tablePerDB        （0–9）
//	globalTableIdx = n                     （0–99）
//
// PaymentIntent.id 编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
// Charge / Refund 的 id 同样以 ch_ / re_ 加分片前缀，保证与父 PI 同分片。
package sharding

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// DefaultShardDBCount 分库数量
	DefaultShardDBCount = 10
	// DefaultTablePerDB 每库表数
	DefaultTablePerDB = 10
)

// Router 分库分表路由器
type Router struct {
	dbCount    int
	tablePerDB int
}

// NewRouter 默认 10 × 10
func NewRouter() *Router {
	return &Router{dbCount: DefaultShardDBCount, tablePerDB: DefaultTablePerDB}
}

// NewRouterWithConfig 自定义
func NewRouterWithConfig(dbCount, tablePerDB int) *Router {
	if dbCount <= 0 {
		dbCount = DefaultShardDBCount
	}
	if tablePerDB <= 0 {
		tablePerDB = DefaultTablePerDB
	}
	return &Router{dbCount: dbCount, tablePerDB: tablePerDB}
}

// DBCount 库数量
func (r *Router) DBCount() int { return r.dbCount }

// TablePerDB 每库表数
func (r *Router) TablePerDB() int { return r.tablePerDB }

// TotalTableCount 全局表数量
func (r *Router) TotalTableCount() int { return r.dbCount * r.tablePerDB }

// RouteByID 按数字 id 路由
func (r *Router) RouteByID(id int64) (dbIndex, tableIndex int) {
	if id < 0 {
		id = -id
	}
	total := int64(r.dbCount * r.tablePerDB)
	n := int(id % total)
	return n / r.tablePerDB, n
}

// RouteByString FNV-1a 64 位哈希后路由
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

// RouteByPrefixedID 解析 "<prefix>_{dbIdx:1d}{tblIdx:02d}<seq>" 形式 id 反推分片。
//
// 例如 "pi_115xxxxx" → dbIndex=1, tableIndex=15。
// 解析失败时回退到 RouteByString。
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

// FormatID 拼出带分片前缀的 id："<prefix>_<db><tbl>{seq}"
func (r *Router) FormatID(prefix string, dbIndex, tableIndex int, seq int64) string {
	return fmt.Sprintf("%s_%d%02d%d", prefix, dbIndex, tableIndex, seq)
}

// GetTableName 拼接 {base}_{tableIdx:02d}
func (r *Router) GetTableName(base string, tableIndex int) string {
	return fmt.Sprintf("%s_%02d", base, tableIndex)
}

// AllShards 返回所有分片元信息（dbIdx, tblIdx）
func (r *Router) AllShards() [][2]int {
	out := make([][2]int, 0, r.dbCount*r.tablePerDB)
	for db := 0; db < r.dbCount; db++ {
		for off := 0; off < r.tablePerDB; off++ {
			out = append(out, [2]int{db, db*r.tablePerDB + off})
		}
	}
	return out
}
