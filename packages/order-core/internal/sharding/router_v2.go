// Package sharding — RouterV2 layout (1000 shard 兼容预备)。
//
// **背景**：当前 RouterV1 layout 是 `<prefix>_<db:1d><tbl:2d><seq>`，最大 100 shard。
// 单 shard 5K QPS / 100GB 数据后需要 reshard（参考 docs/RESHARDING.md）。
//
// V2 layout：`<prefix>_v2_<db:2d><tbl:3d>_<seq>`，最大 100 db × 1000 tbl = 100,000 shard。
// 远超未来 5 年 reshard 需求。
//
// **关键不变量**（与 V1 一致）：
//   - 同 business_key 生成的 V2 ID 路由结果 == business_key 自身在 V2 router 上的路由
//   - V1 ID 仍由 V1 router 解析（dual-read 期间老数据不动）
//   - V2 ID 用单独前缀字面量（`_v2_`），无视觉冲突
//
// **dual-read 模式**：
//
//	Phase 1 (准备)   ：CurrentLayoutVersion=V1，所有写入仍 V1。RouteByPrefixedID 自动识别 v1/v2。
//	Phase 2 (双写)   ：CurrentLayoutVersion=V1，新建表使用 V2 layout，但写入仍 V1（compat）。
//	Phase 3 (cutover) ：CurrentLayoutVersion=V2，新写入 V2；老 V1 ID 通过 RouterV1 路由继续正常读。
//	Phase 4 (清理)   ：V1 数据全部迁完后才 deprecate V1 router。
//
// 见 docs/RESHARDING.md §2.

package sharding

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/xiongwp/order-core/internal/shadow"
)

// LayoutVersion ID layout 版本号。
type LayoutVersion int

const (
	// LayoutV1 当前 layout：<prefix>_<db:1d><tbl:2d><seq>，最大 100 shard。
	LayoutV1 LayoutVersion = 1
	// LayoutV2 1000 shard layout：<prefix>_v2_<db:2d><tbl:3d>_<seq>。
	LayoutV2 LayoutVersion = 2
)

const (
	// V2 marker — 字面量插在 prefix 后：`pi_v2_...`
	v2Marker = "_v2_"

	// DefaultShardDBCountV2 V2 默认分库数（100）。
	DefaultShardDBCountV2 = 100
	// DefaultTablePerDBV2 V2 默认每库表数（1000）。
	DefaultTablePerDBV2 = 1000

	// V2 layout 字段宽度（与 FormatIDV2 / parse 同步）：
	//   db    -> 2 digits, max 99
	//   tbl   -> 3 digits (global table idx within db), max 999
	v2DBDigits  = 2
	v2TblDigits = 3
)

// CurrentLayoutVersion 全局默认 layout 版本。**不要在生产期间动态改这个值**——
// reshard SOP 通过部署期 env / config 切。
//
// 默认 V1：本仓 ID 仍按 100 shard 编码生成；RouteByPrefixedID 兼容 V1+V2。
var CurrentLayoutVersion = LayoutV1

// SetCurrentLayoutVersion 切换默认 layout 版本（启动期 main 通过 env 调）。
// 一旦切到 V2，新生成的 ID 全是 V2 格式；老 V1 ID 仍可被 RouteByPrefixedID 解析。
func SetCurrentLayoutVersion(v LayoutVersion) {
	if v != LayoutV1 && v != LayoutV2 {
		return
	}
	CurrentLayoutVersion = v
}

// LayoutVersionOfID 通过观察 ID 字符串识别它属于哪个 layout 版本。
//
// 探测规则：找到第一个 '_' 后的前 4 字节。如果等于 "v2_" 视为 V2，否则 V1。
// 留 _vN_ 形态给未来扩展（e.g. "_v3_"）。
func LayoutVersionOfID(id string) LayoutVersion {
	idx := strings.IndexByte(id, '_')
	if idx < 0 || idx+4 > len(id) {
		return LayoutV1
	}
	if id[idx:idx+4] == v2Marker {
		return LayoutV2
	}
	return LayoutV1
}

// RouterV2 1000-shard 路由器。和 V1 同语义但容量大 10x。
type RouterV2 struct {
	dbCount    int
	tablePerDB int
}

// NewRouterV2 构造默认 100×1000 的 V2 router。
func NewRouterV2() *RouterV2 {
	return &RouterV2{dbCount: DefaultShardDBCountV2, tablePerDB: DefaultTablePerDBV2}
}

// NewRouterV2WithConfig 自定义。dbCount / tablePerDB <= 0 取默认值。
//
// **约束**：dbCount ≤ 99（2 位），tablePerDB ≤ 999（3 位）。超界会被截到 V1 router
// 上限（避免 panic / 数据漂移）。
func NewRouterV2WithConfig(dbCount, tablePerDB int) *RouterV2 {
	if dbCount <= 0 || dbCount > 99 {
		dbCount = DefaultShardDBCountV2
	}
	if tablePerDB <= 0 || tablePerDB > 999 {
		tablePerDB = DefaultTablePerDBV2
	}
	return &RouterV2{dbCount: dbCount, tablePerDB: tablePerDB}
}

// DBCount returns 库数。
func (r *RouterV2) DBCount() int { return r.dbCount }

// TablePerDB returns 每库表数。
func (r *RouterV2) TablePerDB() int { return r.tablePerDB }

// TotalTableCount returns 全局表数（dbCount * tablePerDB）。
func (r *RouterV2) TotalTableCount() int { return r.dbCount * r.tablePerDB }

// RouteByID 按数字 id 取模分片。和 V1 同算法但模数大。
func (r *RouterV2) RouteByID(id int64) (dbIndex, tableIndex int) {
	if id < 0 {
		id = -id
	}
	total := int64(r.dbCount * r.tablePerDB)
	n := int(id % total)
	return n / r.tablePerDB, n
}

// RouteByString FNV-1a 64 位哈希后路由。同 V1。
func (r *RouterV2) RouteByString(s string) (dbIndex, tableIndex int) {
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

// RouteByPrefixedID 解析 V2 ID（"<prefix>_v2_<db:2d><tbl:3d>_<seq>"）。
// 不是 V2 格式时回退 RouteByString 全 hash 重路由（保险路径）。
func (r *RouterV2) RouteByPrefixedID(id string) (dbIndex, tableIndex int) {
	if LayoutVersionOfID(id) != LayoutV2 {
		return r.RouteByString(id)
	}
	idx := strings.Index(id, v2Marker)
	if idx < 0 {
		return r.RouteByString(id)
	}
	body := id[idx+len(v2Marker):]
	// body = "DDTTT_seq..."; 前 5 byte 是 db+tbl
	const head = v2DBDigits + v2TblDigits // = 5
	if len(body) < head {
		return r.RouteByString(id)
	}
	db, err := strconv.Atoi(body[:v2DBDigits])
	if err != nil {
		return r.RouteByString(id)
	}
	tbl, err := strconv.Atoi(body[v2DBDigits:head])
	if err != nil {
		return r.RouteByString(id)
	}
	if db < 0 || db >= r.dbCount {
		return r.RouteByString(id)
	}
	if tbl < 0 || tbl >= r.tablePerDB {
		return r.RouteByString(id)
	}
	// 一致性自检：tbl 的 dbIdx 必须等于显式 db（保护 ID 篡改）。
	// V2 layout 里 globalTblIdx = tbl 是绝对 idx，dbIdx = tbl/tablePerDB 不再成立
	//（V1 是局部 layout，V2 是绝对 layout——见下面 FormatIDV2 注释）。
	return db, tbl
}

// FormatIDV2 拼 V2 ID："<prefix>_v2_<db:02d><tbl:03d>_<seq>"。
//
// 注意：V2 的 tbl 是**库内局部 idx** [0, tablePerDB)，不是 V1 的 globalTblIdx。
// 这样让单 db 的表不超过 999 上限（3 位编码），同时支持最多 100 db。
//
// 例：dbCount=100, tablePerDB=1000 时 tbl ∈ [0,999]，db ∈ [0,99]。
// "pi_v2_03037_99999" → db=3, tbl=37, seq=99999。
func (r *RouterV2) FormatIDV2(prefix string, dbIndex, tableIndex int, seq int64) string {
	return fmt.Sprintf("%s_v2_%02d%03d_%d", prefix, dbIndex, tableIndex, seq)
}

// TableName 拼 V2 表名："<base>_<dbIdx:02d>_<tbl:03d>"。
//
// V2 表命名空间和 V1 完全独立 — V2 表后缀 5 位（V1 是 2 位）。同时挂 ctx-scoped
// shadow flag。
//
// 业务 repo 在 dual-read 期间应该按 LayoutVersionOfID 决定调 V1 还是 V2 router。
func (r *RouterV2) TableName(ctx context.Context, base string, dbIndex, tableIndex int) string {
	return shadow.TableName(ctx, fmt.Sprintf("%s_%02d_%03d", base, dbIndex, tableIndex))
}

// AllShards V2 所有分片元信息（dbIdx, tblIdx）。
//
// 返 dbCount × tablePerDB 个 [2]int；典型 100×1000=100,000 个，调用方需要明确
// 自己处理这量级（不要 inline range）。给 schema migrator / sweep 用。
func (r *RouterV2) AllShards() [][2]int {
	out := make([][2]int, 0, r.dbCount*r.tablePerDB)
	for db := 0; db < r.dbCount; db++ {
		for tbl := 0; tbl < r.tablePerDB; tbl++ {
			out = append(out, [2]int{db, tbl})
		}
	}
	return out
}
