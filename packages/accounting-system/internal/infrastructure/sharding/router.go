package sharding

import (
	"context"
	"fmt"
	"hash/fnv"
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

	// MaxSupportedShards 当前 layout 支持的全局表数量上限（硬上限）。
	//
	// 限制来源：account_id layout 里 globalTblIdx 占 2 位（accountIDGlobalTblMax=99），
	// 所以 ID 编码层最多只能表达 100 个 globalTblIdx。要扩到 200 必须先：
	//   1. 升级 layout（globalTblIdx 占 3 位 → seq 减少 1 位 → 最多 1000 shard）
	//   2. 全平台双写灰度 + 校验
	//   3. 详见 docs/RESHARDING.md
	//
	// **本常量不要随便改**——任何想超过它的代码都应该走 router version 升级，而不是绕过校验。
	MaxSupportedShards = 100
)

// RouterVersion 路由策略版本号。
//
// 不同 version 对应不同的"id → (db, globalTbl)"映射函数。允许同时存在多版本，
// resharding 灰度期 reads 可以并发尝试 v1+v2 直到 cutover；写一律落 latest。
//
// 现役版本：
//   - V1: simple modulo (id % 100)；当前生产实现，dbCount=10 tablePerDB=10
//
// 预留扩展：
//   - V2: jump consistent hash + virtual buckets，支持平滑加 shard
//   - V3: 引入 group / region 概念
type RouterVersion int

const (
	RouterV1 RouterVersion = 1
)

// CurrentRouterVersion 当前默认版本。resharding 时改这个 + 加新版本逻辑。
const CurrentRouterVersion = RouterV1

// Router 分库分表路由器
//
// 路由规则（v1：10库 × 每库10表 = 100张全局表）：
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
//
// 容量边界与 resharding：
//   - account_id layout 的 globalTblIdx 字段 2 位 → MaxSupportedShards = 100
//   - 加 shard 必须先升级 router version 并 layout 调整，参见 docs/RESHARDING.md
type Router struct {
	dbCount    int // 库数量
	tablePerDB int // 每库表数量
	version    RouterVersion
}

// NewRouter 创建路由器（生产默认：10库 × 10表，version=v1）
func NewRouter() *Router {
	return &Router{
		dbCount:    ShardDBCount,
		tablePerDB: ShardTablePerDB,
		version:    CurrentRouterVersion,
	}
}

// NewRouterWithConfig 创建自定义分片路由器（测试/低容量场景使用）。
// 拒绝 dbCount × tablePerDB > MaxSupportedShards 的配置，防止误用绕过 layout 边界。
func NewRouterWithConfig(dbCount, tablePerDB int) *Router {
	if dbCount*tablePerDB > MaxSupportedShards {
		// 测试 / dev 不应触发；生产代码不会调本函数，所以 panic 让问题尽早暴露
		panic(fmt.Sprintf(
			"NewRouterWithConfig: %d×%d=%d exceeds MaxSupportedShards=%d; "+
				"increase RouterVersion + layout in payment-util/shadow/identity.go first",
			dbCount, tablePerDB, dbCount*tablePerDB, MaxSupportedShards))
	}
	return &Router{dbCount: dbCount, tablePerDB: tablePerDB, version: CurrentRouterVersion}
}

// Version 返回 router 当前版本（监控 / 排查 / dual-read 用）。
func (r *Router) Version() RouterVersion { return r.version }

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

// RouteByNumericStr 把字符串当 routing key 解析。
//
//   - 数字字符串 → ParseInt 后走 RouteByID（保持向后兼容，跟历史 numeric-only business_no 一致）
//   - 非数字字符串 → FNV-1a 32-bit hash 散布到全 100 个 shard
//
// 历史坑：原实现非数字直接返回 (0,0)，外部 caller 一旦传了带字母/下划线/横杠的 ID
// （e.g. UUID、snowflake-as-string、split-payment 的 chargeID）就全部堆 shard-0 = 单点。
// 看 docs/RESHARDING.md 关于 routing key 约束的讨论。
func (r *Router) RouteByNumericStr(s string) (dbIndex, globalTableIndex int) {
	if id, err := strconv.ParseInt(s, 10, 64); err == nil {
		return r.RouteByID(id)
	}
	// 非数字 fallback：FNV-1a 32-bit，% 100 后跟 RouteByID 的 layout 一致
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	total := int64(r.dbCount * r.tablePerDB)
	n := int(int64(h.Sum32()) % total)
	return n / r.tablePerDB, n
}

// RouteByUserID 按用户 ID 路由（alias for RouteByID）
func (r *Router) RouteByUserID(userID int64) (dbIndex, globalTableIndex int) {
	return r.RouteByID(userID)
}

// RouteByAccountNo 按账户号路由（按位编码 layout）。
//
// account_no 是 EncodeAccountID 生成的 19 位 int64 的十进制字符串：
//   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
//
// 直接 ParseInt + AccountIDDBIndex/TableIndex 取出 dbIdx/globalTblIdx，
// 不再字符串切片。所有账户（包括平台 fleet）一律走同一 layout，
// 无需特殊路径判别。
func (r *Router) RouteByAccountNo(accountNo string) (dbIndex, globalTableIndex int) {
	id, err := strconv.ParseInt(accountNo, 10, 64)
	if err != nil || id <= 0 {
		return 0, 0
	}
	tbl := shadow.AccountIDTableIndex(id)
	if tbl < 0 || tbl >= r.dbCount*r.tablePerDB {
		return 0, 0
	}
	return shadow.AccountIDDBIndex(id), tbl
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
