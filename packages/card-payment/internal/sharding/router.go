// Package sharding 10×10=100 路由（按 pi_id 分片）。
package sharding

import (
	"context"
	"fmt"

	"github.com/xiongwp/payment-util/shadow"
)

const (
	ShardDBCount    = 10
	ShardTablePerDB = 10
)

type Router struct{ dbCount, tablePerDB int }

func NewRouter() *Router { return &Router{dbCount: ShardDBCount, tablePerDB: ShardTablePerDB} }

func (r *Router) DBCount() int    { return r.dbCount }
func (r *Router) TablePerDB() int { return r.tablePerDB }

func (r *Router) RouteByID(id int64) (int, int) {
	if id < 0 {
		id = -id
	}
	total := int64(r.dbCount * r.tablePerDB)
	n := int(id % total)
	return n / r.tablePerDB, n
}

// RouteByPIID 按字符串哈希
func (r *Router) RouteByPIID(piID string) (int, int) {
	const offset64, prime64 uint64 = 14695981039346656037, 1099511628211
	h := offset64
	for i := 0; i < len(piID); i++ {
		h ^= uint64(piID[i])
		h *= prime64
	}
	return r.RouteByID(int64(h & 0x7fffffffffffffff))
}

func (r *Router) TableName(ctx context.Context, base string, gtbl int) string {
	return shadow.TableName(ctx, fmt.Sprintf("%s_%02d", base, gtbl))
}

type ShardSpec struct{ DBIndex, TableIndex int }

func (r *Router) AllShards() []ShardSpec {
	out := make([]ShardSpec, 0, r.dbCount*r.tablePerDB)
	for i := 0; i < r.dbCount; i++ {
		for j := 0; j < r.tablePerDB; j++ {
			out = append(out, ShardSpec{DBIndex: i, TableIndex: i*r.tablePerDB + j})
		}
	}
	return out
}
