// Package sharding: 按 merchant_id 路由到 10 dbs × 10 tables = 100 分片。
//
// 策略与 accounting-system 一致：
//   - hash(merchant_id) → uint32
//   - dbIndex   = h % dbCount         (0..9)
//   - tableIdx  = (h / dbCount) % tablePerDB  (0..9)
//
// 同 merchant 永远落同一物理表 → 单库事务 + 顺序扫描友好。
//
// table 命名：
//   settlement_run_00 .. settlement_run_99
//   settlement_record_00 .. settlement_record_99
package sharding

import (
	"fmt"
	"hash/fnv"
)

// 默认分片规模。NewRouter 不传参 → 10×10。
const (
	defaultDBCount    = 10
	defaultTablePerDB = 10
)

// Router merchant_id → (dbIndex, tableIndex)。
type Router struct {
	dbCount    int
	tablePerDB int
}

// NewRouter 默认 10×10 路由。
func NewRouter() *Router {
	return &Router{dbCount: defaultDBCount, tablePerDB: defaultTablePerDB}
}

// NewRouterWithConfig 指定 dbCount × tablePerDB。dbCount × tablePerDB 必须等于
// 总分片数（典型 100），不强制校验，由配置层确认。
func NewRouterWithConfig(dbCount, tablePerDB int) *Router {
	if dbCount <= 0 {
		dbCount = defaultDBCount
	}
	if tablePerDB <= 0 {
		tablePerDB = defaultTablePerDB
	}
	return &Router{dbCount: dbCount, tablePerDB: tablePerDB}
}

// RouteByMerchantID 返回 (dbIndex, tableIndex)。
func (r *Router) RouteByMerchantID(merchantID string) (dbIndex, tableIndex int) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(merchantID))
	v := h.Sum32()
	dbIndex = int(v) % r.dbCount
	tableIndex = (int(v) / r.dbCount) % r.tablePerDB
	return
}

// AllShards 返回 100 个 (dbIndex, tableIndex)，admin 状态聚合 / 调度全分片用。
func (r *Router) AllShards() []Shard {
	out := make([]Shard, 0, r.dbCount*r.tablePerDB)
	for d := 0; d < r.dbCount; d++ {
		for t := 0; t < r.tablePerDB; t++ {
			out = append(out, Shard{DBIndex: d, TableIndex: t})
		}
	}
	return out
}

// DBCount 暴露 dbCount 给上层（database manager 初始化连接池数量）。
func (r *Router) DBCount() int { return r.dbCount }

// TablePerDB 单库表数。
func (r *Router) TablePerDB() int { return r.tablePerDB }

// Shard 表标识。
type Shard struct {
	DBIndex    int
	TableIndex int
}

// RunTableName 返回 settlement_run_NN。
func (r *Router) RunTableName(tableIndex int) string {
	return fmt.Sprintf("settlement_run_%02d", tableIndex)
}

// RecordTableName 返回 settlement_record_NN。
func (r *Router) RecordTableName(tableIndex int) string {
	return fmt.Sprintf("settlement_record_%02d", tableIndex)
}
