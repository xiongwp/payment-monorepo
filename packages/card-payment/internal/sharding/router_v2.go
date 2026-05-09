// router_v2.go — RouterV2 for card-payment: 1000-shard 表布局。
//
// card-payment 跟 card-center 一样，不在 ID 字面量编码 shard（pi_id / token_hash
// 由 order-core / card-center 路由生成），所以这里只需要扩大 dbCount × tablePerDB
// + 改 TableName 后缀格式。
//
// 见 docs/RESHARDING.md §6 + packages/card-center/internal/sharding/router_v2.go。

package sharding

import (
	"context"
	"fmt"

	"github.com/xiongwp/payment-util/shadow"
)

// LayoutVersion 表布局版本号。
type LayoutVersion int

const (
	LayoutV1 LayoutVersion = 1
	LayoutV2 LayoutVersion = 2
)

const (
	ShardDBCountV2    = 100
	ShardTablePerDBV2 = 1000
)

// CurrentLayoutVersion 全局默认 layout（默认 V1，cutover 时切 V2）。
var CurrentLayoutVersion = LayoutV1

// SetCurrentLayoutVersion 切换默认 layout 版本。
func SetCurrentLayoutVersion(v LayoutVersion) {
	if v != LayoutV1 && v != LayoutV2 {
		return
	}
	CurrentLayoutVersion = v
}

// RouterV2 1000-shard 路由器。签名与 V1 *Router 对齐。
type RouterV2 struct {
	dbCount    int
	tablePerDB int
}

// NewRouterV2 默认 100×1000。
func NewRouterV2() *RouterV2 {
	return &RouterV2{dbCount: ShardDBCountV2, tablePerDB: ShardTablePerDBV2}
}

// NewRouterV2WithConfig 自定义；越界回默认。
func NewRouterV2WithConfig(dbCount, tablePerDB int) *RouterV2 {
	if dbCount <= 0 || dbCount > 99 {
		dbCount = ShardDBCountV2
	}
	if tablePerDB <= 0 || tablePerDB > 999 {
		tablePerDB = ShardTablePerDBV2
	}
	return &RouterV2{dbCount: dbCount, tablePerDB: tablePerDB}
}

func (r *RouterV2) DBCount() int    { return r.dbCount }
func (r *RouterV2) TablePerDB() int { return r.tablePerDB }
func (r *RouterV2) TotalTableCount() int {
	return r.dbCount * r.tablePerDB
}

// RouteByID 数字 ID 取模分片。
func (r *RouterV2) RouteByID(id int64) (int, int) {
	if id < 0 {
		id = -id
	}
	total := int64(r.dbCount * r.tablePerDB)
	n := int(id % total)
	return n / r.tablePerDB, n
}

// RouteByPIID FNV-1a 64bit 哈希后取模。
func (r *RouterV2) RouteByPIID(piID string) (int, int) {
	const offset64, prime64 uint64 = 14695981039346656037, 1099511628211
	h := offset64
	for i := 0; i < len(piID); i++ {
		h ^= uint64(piID[i])
		h *= prime64
	}
	return r.RouteByID(int64(h & 0x7fffffffffffffff))
}

// TableName V2 layout: "<base>_<db:02d>_<tbl:03d>"，叠加 ctx-scoped shadow。
func (r *RouterV2) TableName(ctx context.Context, base string, dbIndex, tableIndex int) string {
	return shadow.TableName(ctx, fmt.Sprintf("%s_%02d_%03d", base, dbIndex, tableIndex))
}

// AllShards 全部 (db, tbl) 元信息。注意 V2 默认 100k 项。
func (r *RouterV2) AllShards() []ShardSpec {
	out := make([]ShardSpec, 0, r.dbCount*r.tablePerDB)
	for i := 0; i < r.dbCount; i++ {
		for j := 0; j < r.tablePerDB; j++ {
			out = append(out, ShardSpec{DBIndex: i, TableIndex: i*r.tablePerDB + j})
		}
	}
	return out
}
