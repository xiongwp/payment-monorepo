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

// Router 分库分表路由器
//
// 路由规则（10库 × 每库10表 = 100张全局表）：
//
//	n = id % 100
//	dbIndex        = n / 10      （0–9，对应物理库）
//	globalTableIdx = n           （0–99，全局表序号）
//
// 全局表序号与物理库的对应关系：
//
//	DB 0 → 表  00– 09
//	DB 1 → 表  10– 19
//	…
//	DB 9 → 表  90– 99
type Router struct {
	dbCount      int // 库数量
	tablePerDB   int // 每库表数量
}

// NewRouter 创建路由器（生产默认：10库 × 10表）
func NewRouter() *Router {
	return &Router{
		dbCount:    ShardDBCount,
		tablePerDB: ShardTablePerDB,
	}
}

// NewRouterWithConfig 创建自定义分片路由器（测试/低容量场景使用）
func NewRouterWithConfig(dbCount, tablePerDB int) *Router {
	return &Router{dbCount: dbCount, tablePerDB: tablePerDB}
}

// TableCount 返回每库分表数量
func (r *Router) TableCount() int { return r.tablePerDB }

// TotalTableCount 返回全局总表数量
func (r *Router) TotalTableCount() int { return r.dbCount * r.tablePerDB }

// RouteByID 按数字 ID 路由：
//
//	n = id % (dbCount×tablePerDB)
//	dbIndex        = n / tablePerDB
//	globalTableIdx = n
func (r *Router) RouteByID(id int64) (dbIndex, globalTableIndex int) {
	if id < 0 {
		id = -id
	}
	total := int64(r.dbCount * r.tablePerDB)
	n := int(id % total)
	return n / r.tablePerDB, n
}

// RouteByNumericStr 将字符串解析为 int64 后调用 RouteByID。
// 非数字字符串优雅降级，返回 (0, 0)。
func (r *Router) RouteByNumericStr(s string) (dbIndex, globalTableIndex int) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, 0
	}
	return r.RouteByID(id)
}

// RouteByUserID 按用户 ID 路由（alias for RouteByID）
func (r *Router) RouteByUserID(userID int64) (dbIndex, globalTableIndex int) {
	return r.RouteByID(userID)
}

// RouteByAccountNo 按账户号路由。
//
// 主格式（generateAccountNo 生成的用户账户）：
//   前3字符 = {dbIdx:1d}{globalTableIdx:02d}
//   例："115..." → dbIndex=1, globalTableIndex=15（15/10=1 ✓）
//
// 平台账户格式（数据库初始化脚本 seed）：
//   前3字符 = {0}{globalTableIdx:02d}  （首字符固定为 '0'，非 dbIdx）
//   例："010_PLATFORM_..." → globalTableIdx=10 → dbIndex=1 → account_10
//        "011_PLATFORM_..." → globalTableIdx=11 → dbIndex=1 → account_11
//
// 区分逻辑：先尝试主格式约束（tbl/tablePerDB==db）；约束失败时说明首字符是
// 固定前缀 '0' 而非 dbIdx，此时直接将 chars[1:3] 作为 globalTableIdx 并
// 推导 dbIndex。
func (r *Router) RouteByAccountNo(accountNo string) (dbIndex, globalTableIndex int) {
	if len(accountNo) >= 3 {
		if db, err := strconv.Atoi(accountNo[:1]); err == nil {
			if tbl, err := strconv.Atoi(accountNo[1:3]); err == nil {
				total := r.dbCount * r.tablePerDB
				if db < r.dbCount && tbl < total && tbl/r.tablePerDB == db {
					// Primary: {dbIdx:1d}{globalTable:02d} — normal user accounts
					return db, tbl
				}
				// Constraint failed: first char is a literal '0' prefix, not dbIdx.
				// Treat chars[1:3] as the global table index directly.
				// e.g. "010_PLATFORM_..." → tbl=10 → db=10/10=1 → account_10
				if tbl < total {
					return tbl / r.tablePerDB, tbl
				}
			}
		}
	}
	return 0, 0
}

// GetTableName 获取主表名（不感知 shadow）。
//
// Deprecated: 仅供启动期工具（schema 自愈）使用。业务 repo 一律改用
// TableName(ctx, …)，否则压测流量会落到主表。
func (r *Router) GetTableName(baseTableName string, globalTableIndex int) string {
	return fmt.Sprintf("%s_%02d", baseTableName, globalTableIndex)
}

// TableName 根据 ctx 决定返回主表名 / 影子表名（"<base>_<NN>" 或 "<base>_<NN>_shadow"）。
// 所有业务 repo 的访问点都应走此方法，让 shadow 流量自动落到 _shadow 表。
func (r *Router) TableName(ctx context.Context, baseTableName string, globalTableIndex int) string {
	return shadow.TableName(ctx, fmt.Sprintf("%s_%02d", baseTableName, globalTableIndex))
}

// GetAllShards 获取所有分片信息（10库 × 每库10表 = 100条）
//
// ShardInfo.TableIndex 为全局表序号（0–99），可直接用于 GetTableName。
func (r *Router) GetAllShards() []ShardInfo {
	shards := make([]ShardInfo, 0, r.dbCount*r.tablePerDB)
	for dbIdx := 0; dbIdx < r.dbCount; dbIdx++ {
		for localIdx := 0; localIdx < r.tablePerDB; localIdx++ {
			globalIdx := dbIdx*r.tablePerDB + localIdx
			shards = append(shards, ShardInfo{
				DBIndex:    dbIdx,
				TableIndex: globalIdx,
			})
		}
	}
	return shards
}

// ShardInfo 分片信息
type ShardInfo struct {
	DBIndex    int // 库序号（0–9）
	TableIndex int // 全局表序号（0–99）
}

// GetDBShards 获取指定库的所有分片（每库10张表）
func (r *Router) GetDBShards(dbIndex int) []ShardInfo {
	shards := make([]ShardInfo, 0, r.tablePerDB)
	for localIdx := 0; localIdx < r.tablePerDB; localIdx++ {
		globalIdx := dbIndex*r.tablePerDB + localIdx
		shards = append(shards, ShardInfo{
			DBIndex:    dbIndex,
			TableIndex: globalIdx,
		})
	}
	return shards
}
