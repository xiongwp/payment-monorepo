// router_v2.go — RouterV2 for accounting-system: 1000-shard 表布局。
//
// 与 order-core / card-center RouterV2 同思路（见 docs/RESHARDING.md §6）。
//
// **关键限制**：accounting-system 的 RouteByAccountNo 依赖 payment-util/shadow
// 的 AccountIDDBIndex / AccountIDTableIndex，而那两个函数当前只能解 V1 layout
// （globalTblIdx 占 2 位，max=99）。要让 V2 router 支持 account_no 路径，必须
// 先升级 payment-util/shadow 的 EncodeAccountID layout-version 高位 — 这是
// reshard SOP Phase 1 的子项，**不在本文件 scope**。
//
// 当前 V2 router 提供：
//
//	(1) RouteByID / RouteByUserID — 简单 modulo，能直接 1000-shard
//	(2) RouteByNumericStr — 透传 RouteByID
//	(3) TableName V2 layout (<base>_<db:02d>_<tbl:03d>) — 物理表与 V1 隔离
//	(4) GetAllShards / GetDBShards — 给 schema migrator / sweep
//
// **不提供**：RouteByAccountNo —— 在 EncodeAccountIDV2 落地之前，
// 调用 V2 router 的 RouteByAccountNo 会返 (0,0) + log warn，
// 提醒迁移负责人补齐 layout 升级。

package sharding

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"

	"github.com/xiongwp/payment-util/shadow"
)

const (
	// V2 默认配置：10 db × 100 tbl = 1000 shard。
	// **必须与 EncodeAccountIDV2 的 globalTblIdx 3 位 (max=999) 严格对齐** —
	// 否则 RouteByAccountNo 拿到的 globalTblIdx 会 OOR。
	// 1000 shard 是当前 V1 (100) 的 10× 容量，按下次 reshard 触发线 (单 shard
	// > 5K QPS 持续 7 天) 估算可撑 ~5 年。
	ShardDBCountV2    = 10
	ShardTablePerDBV2 = 100
	// MaxSupportedShardsV2 layout 上限。
	// 由 EncodeAccountIDV2 的 globalTblIdx 字段 3 位 (max=999) 决定。
	MaxSupportedShardsV2 = 1000
)

const (
	// RouterV2 启用 1000-shard layout（需配套 EncodeAccountID layout-version 高位升级）
	RouterV2Const RouterVersion = 2
)

// RouterV2 1000-shard 路由器。签名与 V1 *Router 对齐，service 层可用 interface
// 抽象后切换。
type RouterV2 struct {
	dbCount    int
	tablePerDB int
	version    RouterVersion
}

// NewRouterV2 默认 100×1000 的 V2 router。
func NewRouterV2() *RouterV2 {
	return &RouterV2{
		dbCount:    ShardDBCountV2,
		tablePerDB: ShardTablePerDBV2,
		version:    RouterV2Const,
	}
}

// NewRouterV2WithConfig 自定义。dbCount<=0 || >99 取默认；tablePerDB<=0 || >999 取默认。
// 上界保护让 V2 表名 layout（02d / 03d）始终能编码。
func NewRouterV2WithConfig(dbCount, tablePerDB int) *RouterV2 {
	if dbCount <= 0 || dbCount > 99 {
		dbCount = ShardDBCountV2
	}
	if tablePerDB <= 0 || tablePerDB > 999 {
		tablePerDB = ShardTablePerDBV2
	}
	if dbCount*tablePerDB > MaxSupportedShardsV2 {
		// 严格来说上面的范围限制后已不会触发，但显式 guard 留作后续放宽时的提醒。
		panic(fmt.Sprintf(
			"NewRouterV2WithConfig: %d×%d=%d exceeds MaxSupportedShardsV2=%d",
			dbCount, tablePerDB, dbCount*tablePerDB, MaxSupportedShardsV2))
	}
	return &RouterV2{dbCount: dbCount, tablePerDB: tablePerDB, version: RouterV2Const}
}

// Version 返回 router 当前版本。
func (r *RouterV2) Version() RouterVersion { return r.version }

// TableCount 每库分表数。
func (r *RouterV2) TableCount() int { return r.tablePerDB }

// TotalTableCount 全局总表数。
func (r *RouterV2) TotalTableCount() int { return r.dbCount * r.tablePerDB }

// RouteByID 数字 ID 取模分片，与 V1 同算法但模数 1000×。
func (r *RouterV2) RouteByID(id int64) (dbIndex, globalTableIndex int) {
	if id < 0 {
		id = -id
	}
	total := int64(r.dbCount * r.tablePerDB)
	n := int(id % total)
	return n / r.tablePerDB, n
}

// RouteByNumericStr 字符串 → 路由。
//
//   - 数字 → ParseInt + RouteByID（向后兼容）
//   - 非数字 → FNV-1a hash 散布到全部 shard（避免 (0,0) footgun，跟 Router 行为一致）
func (r *RouterV2) RouteByNumericStr(s string) (dbIndex, globalTableIndex int) {
	if id, err := strconv.ParseInt(s, 10, 64); err == nil {
		return r.RouteByID(id)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	total := int64(r.dbCount * r.tablePerDB)
	n := int(int64(h.Sum32()) % total)
	return n / r.tablePerDB, n
}

// RouteByUserID alias。
func (r *RouterV2) RouteByUserID(userID int64) (int, int) {
	return r.RouteByID(userID)
}

// RouteByAccountNo 按 V2 layout account_no 路由。
//
// **只接受 V2 ID**（accountID >= 2e18）。V1 ID 经过 V1 router 的 RouteByAccountNo
// 处理；同 user 的 V1/V2 ID 落不同分片（不同 modulo 总数），dual-read 期 caller
// 必须按 ID 版本 dispatch：见 RouteByAccountNoDual。
//
// 出错条件：
//
//	解析失败 / 非正数            → (0, 0)
//	V1 ID（layout=1）           → (-1, -1) 显式拒绝（避免静默错路由）
//	V2 ID 但 globalTbl 越界 V2 router → (-1, -1)
func (r *RouterV2) RouteByAccountNo(accountNo string) (dbIndex, globalTableIndex int) {
	id, err := strconv.ParseInt(accountNo, 10, 64)
	if err != nil || id <= 0 {
		return 0, 0
	}
	if shadow.AccountIDLayoutVersion(id) != 2 {
		// V1 ID 不该路由进 V2 router；caller 用 RouteByAccountNoDual。
		return -1, -1
	}
	tbl := shadow.AccountIDTableIndex(id)
	if tbl < 0 || tbl >= r.dbCount*r.tablePerDB {
		return -1, -1
	}
	return shadow.AccountIDDBIndex(id), tbl
}

// RouteByAccountNoDual 按 ID layout 版本 dispatch 到对应 router。
// dual-read 期间所有 account_no 路由都应该走这个入口。
//
//	v1 / v2: 同名相反语义；v1==nil 且 v1id 走 v2 router 兜底失败 → (-1,-1,1)
//	返回 layoutVersion 让 caller 选 V1 或 V2 表名读取
func RouteByAccountNoDual(v1 *Router, v2 *RouterV2, accountNo string) (dbIndex, globalTableIndex, layoutVersion int) {
	id, err := strconv.ParseInt(accountNo, 10, 64)
	if err != nil || id <= 0 {
		return 0, 0, 1
	}
	ver := shadow.AccountIDLayoutVersion(id)
	if ver == 2 && v2 != nil {
		db, tbl := v2.RouteByAccountNo(accountNo)
		return db, tbl, 2
	}
	if v1 == nil {
		v1 = NewRouter()
	}
	db, tbl := v1.RouteByAccountNo(accountNo)
	return db, tbl, 1
}

// GetTableName 主表名（不感知 shadow）。
//
// V2 layout: <base>_<db:02d>_<tbl:03d>，5 位后缀与 V1 的 2 位区分。
//
// Deprecated: 同 V1，仅供 schema 自愈工具用，业务 repo 走 TableName(ctx, ...)。
func (r *RouterV2) GetTableName(baseTableName string, dbIndex, globalTableIndex int) string {
	return fmt.Sprintf("%s_%02d_%03d", baseTableName, dbIndex, globalTableIndex)
}

// TableName 按 ctx 决定主表 / shadow 表，签名与 V1 不同 — V2 需要 dbIndex 显式
// 传入（V1 只用 globalTblIdx 因为 dbIdx = globalTbl/10，V2 表名同时编码两段）。
//
// 所有业务 repo 都应走此方法。
func (r *RouterV2) TableName(ctx context.Context, baseTableName string, dbIndex, globalTableIndex int) string {
	return shadow.TableName(ctx, fmt.Sprintf("%s_%02d_%03d", baseTableName, dbIndex, globalTableIndex))
}

// GetAllShards 全部分片元信息。
//
// 注意：V2 默认 100k 项，调用方需要明确处理这量级（schema migrator / 跨分片
// sweep）。不是给 hot path 用的。
func (r *RouterV2) GetAllShards() []ShardInfo {
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

// GetDBShards 单库的所有分片。
func (r *RouterV2) GetDBShards(dbIndex int) []ShardInfo {
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
