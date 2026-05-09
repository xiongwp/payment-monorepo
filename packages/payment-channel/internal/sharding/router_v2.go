// router_v2.go — RouterV2 for payment-channel: 1000-shard 表布局。
//
// payment-channel 的 ID 字面量与 order-core 共享（pi_xxx / ch_xxx），所以
// V2 marker `_v2_` 必须跟 order-core 一致。RouteByPrefixedID 解析逻辑同形复用。
//
// 见 docs/RESHARDING.md §6 + packages/order-core/internal/sharding/router_v2.go。

package sharding

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/xiongwp/payment-util/shadow"
)

// LayoutVersion ID layout 版本号；与 order-core 保持枚举值一致。
type LayoutVersion int

const (
	LayoutV1 LayoutVersion = 1
	LayoutV2 LayoutVersion = 2
)

const (
	// V2 marker — 与 order-core 一致，跨服务 ID 路由结果稳定。
	v2Marker = "_v2_"

	DefaultShardDBCountV2 = 100
	DefaultTablePerDBV2   = 1000

	v2DBDigits  = 2
	v2TblDigits = 3
)

// CurrentLayoutVersion 全局默认 layout 版本（默认 V1，cutover 切 V2）。
var CurrentLayoutVersion = LayoutV1

// SetCurrentLayoutVersion 切换默认 layout 版本（启动期 main 通过 env 调）。
func SetCurrentLayoutVersion(v LayoutVersion) {
	if v != LayoutV1 && v != LayoutV2 {
		return
	}
	CurrentLayoutVersion = v
}

// LayoutVersionOfID 通过 ID 字符串识别 layout 版本。
// 规则同 order-core：第一个 '_' 后接 "v2_" → V2，否则 V1。
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

// RouterV2 1000-shard 路由器。
type RouterV2 struct {
	dbCount    int
	tablePerDB int
}

// NewRouterV2 构造默认 100×1000 的 V2 router。
func NewRouterV2() *RouterV2 {
	return &RouterV2{dbCount: DefaultShardDBCountV2, tablePerDB: DefaultTablePerDBV2}
}

// NewRouterV2WithConfig 自定义；越界时取默认值（保护 02d/03d 编码）。
func NewRouterV2WithConfig(dbCount, tablePerDB int) *RouterV2 {
	if dbCount <= 0 || dbCount > 99 {
		dbCount = DefaultShardDBCountV2
	}
	if tablePerDB <= 0 || tablePerDB > 999 {
		tablePerDB = DefaultTablePerDBV2
	}
	return &RouterV2{dbCount: dbCount, tablePerDB: tablePerDB}
}

func (r *RouterV2) DBCount() int         { return r.dbCount }
func (r *RouterV2) TablePerDB() int      { return r.tablePerDB }
func (r *RouterV2) TotalTableCount() int { return r.dbCount * r.tablePerDB }

// RouteByID 数字 id 取模。
func (r *RouterV2) RouteByID(id int64) (dbIndex, tableIndex int) {
	if id < 0 {
		id = -id
	}
	total := int64(r.dbCount * r.tablePerDB)
	n := int(id % total)
	return n / r.tablePerDB, n
}

// RouteByIDHashed 用 FNV-1a mix 后再取模 — 抗 bit-encoded ID 聚集。
func (r *RouterV2) RouteByIDHashed(id int64) (dbIndex, tableIndex int) {
	if id < 0 {
		id = -id
	}
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

// RouteByString FNV-1a 哈希后取模。
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
// 不是 V2 → 回落 RouteByString。
func (r *RouterV2) RouteByPrefixedID(id string) (dbIndex, tableIndex int) {
	if LayoutVersionOfID(id) != LayoutV2 {
		return r.RouteByString(id)
	}
	idx := strings.Index(id, v2Marker)
	if idx < 0 {
		return r.RouteByString(id)
	}
	body := id[idx+len(v2Marker):]
	const head = v2DBDigits + v2TblDigits
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
	return db, tbl
}

// FormatIDV2 拼 V2 ID："<prefix>_v2_<db:02d><tbl:03d>_<seq>"。
func (r *RouterV2) FormatIDV2(prefix string, dbIndex, tableIndex int, seq int64) string {
	return fmt.Sprintf("%s_v2_%02d%03d_%d", prefix, dbIndex, tableIndex, seq)
}

// TableName V2 表名："<base>_<dbIdx:02d>_<tbl:03d>"，叠加 ctx-scoped shadow。
func (r *RouterV2) TableName(ctx context.Context, base string, dbIndex, tableIndex int) string {
	return shadow.TableName(ctx, fmt.Sprintf("%s_%02d_%03d", base, dbIndex, tableIndex))
}

// AllShards 全部 (dbIdx, tblIdx)。
func (r *RouterV2) AllShards() [][2]int {
	out := make([][2]int, 0, r.dbCount*r.tablePerDB)
	for db := 0; db < r.dbCount; db++ {
		for tbl := 0; tbl < r.tablePerDB; tbl++ {
			out = append(out, [2]int{db, tbl})
		}
	}
	return out
}

// ─── dual-read dispatcher ──────────────────────────────────────────────

// RouteByPrefixedIDDual 自动按 ID 字符串识别 layout 版本，dispatch 到对应 router。
//
// 与 order-core 同形：v2 == nil 时 V2 ID 兜底走 V1.RouteByString，
// caller 应监控并打 metric (v2_seen_without_v2_router_total)。
func RouteByPrefixedIDDual(v1 *Router, v2 *RouterV2, id string) (dbIndex, tableIndex int, version LayoutVersion) {
	if v1 == nil {
		v1 = NewRouter()
	}
	v := LayoutVersionOfID(id)
	if v == LayoutV2 {
		if v2 != nil {
			db, tbl := v2.RouteByPrefixedID(id)
			return db, tbl, LayoutV2
		}
		db, tbl := v1.RouteByString(id)
		return db, tbl, LayoutV2
	}
	db, tbl := v1.RouteByPrefixedID(id)
	return db, tbl, LayoutV1
}

// FormatIDForCurrentLayout 按 CurrentLayoutVersion 选 V1 / V2 编码生成新 ID。
func FormatIDForCurrentLayout(v1 *Router, v2 *RouterV2, prefix string, dbIndex, tableIndex int, seq int64) string {
	switch CurrentLayoutVersion {
	case LayoutV2:
		if v2 != nil {
			return v2.FormatIDV2(prefix, dbIndex, tableIndex, seq)
		}
		fallthrough
	default:
		if v1 == nil {
			v1 = NewRouter()
		}
		return v1.FormatID(prefix, dbIndex, tableIndex, seq)
	}
}
