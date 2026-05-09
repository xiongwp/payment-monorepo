// router_v2.go — RouterV2 for card-center: 1000-shard 表布局。
//
// 与 order-core RouterV2 同思路（见 docs/RESHARDING.md §6），但 card-center
// 不在 ID 字面量里编码 shard（卡 token_hash 是 sha256 hex，PAN-related ID 由
// payment-channel 路由生成），所以这里只需要：
//
//	(1) 扩大 dbCount × tablePerDB 上限：100 × 1000 = 100,000 张全局表
//	(2) 新表名 layout：<base>_<dbIdx:02d>_<tbl:03d> （5 位后缀，与 V1 的 2 位区分）
//	(3) 路由方法签名与 V1 完全一致（RouteByID / RouteByUserID / RouteByPIID /
//	    RouteByTokenHash），调用方可平滑迁移
//
// 切换 SOP：
//
//	Phase 1：部署 V2 router 但 CurrentLayoutVersion=V1（写仍走 V1 表）
//	Phase 2：起 migrate worker 双写 V1 + V2，监控 diff
//	Phase 3：CurrentLayoutVersion=V2 一行翻盘，新写入全 V2
//	Phase 4：V1 表数据迁完后 deprecate
//
// 风险：card-center 维护审计哈希链 (per-shard prev_hash)，cutover 时新 V2 shard
// 需要重新初始化 prev_hash chain（新表无历史）。SOP 第 3 步前先跑一遍
// per-shard chain bootstrap script。

package sharding

import (
	"context"
	"fmt"
	"strconv"

	"github.com/xiongwp/payment-util/shadow"
)

// LayoutVersion card-center 表布局版本号。
type LayoutVersion int

const (
	LayoutV1 LayoutVersion = 1 // 现行：10×10 = 100 shard，表后缀 2 位
	LayoutV2 LayoutVersion = 2 // 1000-shard 预备：100×1000，表后缀 5 位
)

// CurrentLayoutVersion card-center 全局默认 layout。
//
// **不在生产期间动态改**——cutover 通过部署期 env / config 完成；切换前
// 必须确认所有 V2 shard 表 prev_hash chain 已初始化。
var CurrentLayoutVersion = LayoutV1

// SetCurrentLayoutVersion 切换默认布局（启动期 main 通过 env 调）。
func SetCurrentLayoutVersion(v LayoutVersion) {
	if v != LayoutV1 && v != LayoutV2 {
		return
	}
	CurrentLayoutVersion = v
}

const (
	// V2 默认配置：100 db × 1000 tbl = 100,000 shard。
	ShardDBCountV2    = 100
	ShardTablePerDBV2 = 1000
)

// RouterV2 1000-shard 路由器。签名与 V1 *Router 对齐，方便 service 层
// 用 interface 抽象后切换。
type RouterV2 struct {
	dbCount    int
	tablePerDB int
}

// NewRouterV2 默认 100×1000。
func NewRouterV2() *RouterV2 {
	return &RouterV2{dbCount: ShardDBCountV2, tablePerDB: ShardTablePerDBV2}
}

// NewRouterV2WithConfig 自定义。dbCount<=0 || >99 取默认；tablePerDB<=0 || >999 取默认。
// 上界保护是为了让 V2 表名 layout（02d / 03d）始终能编码。
func NewRouterV2WithConfig(dbCount, tablePerDB int) *RouterV2 {
	if dbCount <= 0 || dbCount > 99 {
		dbCount = ShardDBCountV2
	}
	if tablePerDB <= 0 || tablePerDB > 999 {
		tablePerDB = ShardTablePerDBV2
	}
	return &RouterV2{dbCount: dbCount, tablePerDB: tablePerDB}
}

func (r *RouterV2) DBCount() int         { return r.dbCount }
func (r *RouterV2) TablePerDB() int      { return r.tablePerDB }
func (r *RouterV2) TotalTableCount() int { return r.dbCount * r.tablePerDB }

// RouteByID 数字 ID 取模分片，与 V1 同算法但模数 1000×。
// 同一 user_id 在 V1 / V2 下落不同 shard（模数不同）—— 这就是为什么 cutover
// 必须双写 V1 + V2 + 校验数据收敛后再切。
func (r *RouterV2) RouteByID(id int64) (dbIndex, globalTableIndex int) {
	if id < 0 {
		id = -id
	}
	total := int64(r.dbCount * r.tablePerDB)
	n := int(id % total)
	return n / r.tablePerDB, n
}

// RouteByUserID alias，签名与 V1 对齐。
func (r *RouterV2) RouteByUserID(userID int64) (int, int) { return r.RouteByID(userID) }

// RouteByPIID FNV-1a 64bit hash 后取模。
func (r *RouterV2) RouteByPIID(piID string) (int, int) {
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

// RouteByTokenHash sha256 hex 前 8 char 取模。
//
// V1 同算法但模数变 1000×。同一 token_hash 在 V1 / V2 下也落不同 shard，
// 所以 cutover 期间必须双写双读（dual-read）观察 diff < 0.01% 后再切。
func (r *RouterV2) RouteByTokenHash(tokenHash string) (int, int) {
	if len(tokenHash) < 8 {
		return 0, 0
	}
	prefix := tokenHash[:8]
	if n, err := strconv.ParseInt(prefix, 16, 64); err == nil {
		return r.RouteByID(n)
	}
	return 0, 0
}

// TableName V2 表名格式：<base>_<dbIdx:02d>_<tbl:03d>。
//
// 与 V1（<base>_<tbl:02d>）通过后缀位数区分，使迁移期间双写两套表
// （stored_token_42 vs stored_token_00_037）不互相覆盖。
//
// shadow flag 通过 ctx 传递；shadow=true 时再叠加 _shadow 后缀。
func (r *RouterV2) TableName(ctx context.Context, base string, dbIndex, tableIndex int) string {
	t := fmt.Sprintf("%s_%02d_%03d", base, dbIndex, tableIndex)
	return shadow.TableName(ctx, t)
}

// AllShards 全部 (dbIdx, globalTblIdx) 元信息。
//
// 注意：V2 默认 100k 项，调用方需要显式准备处理这量级（schema migrator /
// per-shard sweep）。不是给 hot path 用的。
func (r *RouterV2) AllShards() []ShardSpec {
	out := make([]ShardSpec, 0, r.dbCount*r.tablePerDB)
	for i := 0; i < r.dbCount; i++ {
		for j := 0; j < r.tablePerDB; j++ {
			out = append(out, ShardSpec{DBIndex: i, TableIndex: i*r.tablePerDB + j})
		}
	}
	return out
}
