// Package sharding — split-payment 流水表分片路由
//
// 设计：跟 accounting-system 的 sharding.Router 完全对齐
//
//	total = dbCount(10) * tablePerDB(10) = 100 全局表/族
//	n     = hash(key) % total   in [0, 99]
//	dbIdx = n / tablePerDB      in [0, 9]
//	tblIdx = n                  in [0, 99]
//
// 流水表族（每族 100 张全局子表）:
//
//	moneyflow_runs_NN         一次 TriggerEvent 的 RunPlan
//	transfers_NN              Stripe-style transfer
//	application_fees_NN       Stripe-style fee
//	payouts_NN                Stripe-style payout
//	reversals_NN              Stripe-style reversal
//	moneyflow_sagas_NN        持久化 saga 状态
//	event_outbox_NN           事件 outbox
//	reversal_retry_outbox_NN  反转重试 outbox
//
// 路由 key 约定:
//   - moneyflow_runs / transfers / fees / payouts / reversals: idempotency_key 或
//     charge_id（同一笔交易子表对齐到同 shard，子表查 graph_run_id 不跨库）
//   - moneyflow_sagas: saga_id
//   - event_outbox / reversal_retry_outbox: 业务关联键（payload 里的 reversal_id 等）
package sharding

import (
	"context"
	"fmt"
	"hash/fnv"
)

const (
	// 跟 accounting-system 一致：10 库 × 10 表/库 = 100 全局表/族。
	// MaxSupportedShards 是 router 编码层的上限，跨这个数字要走 router version 升级
	// （详见 accounting docs/RESHARDING.md）。loadtest / dev 用 10×10。
	DefaultDBCount      = 10
	DefaultTablePerDB   = 10
	DefaultTotalTables  = DefaultDBCount * DefaultTablePerDB // 100
)

// Router 分片路由器。
type Router struct {
	dbCount    int
	tablePerDB int
	totalTbls  int
}

// NewRouter 用默认 10×10 配置。
func NewRouter() *Router {
	return NewRouterWith(DefaultDBCount, DefaultTablePerDB)
}

// NewRouterWith 自定义 db 数 × 表/库。loadtest 默认 10×10，单元测试可用 2×3 之类。
func NewRouterWith(dbCount, tablePerDB int) *Router {
	if dbCount <= 0 {
		dbCount = DefaultDBCount
	}
	if tablePerDB <= 0 {
		tablePerDB = DefaultTablePerDB
	}
	return &Router{
		dbCount:    dbCount,
		tablePerDB: tablePerDB,
		totalTbls:  dbCount * tablePerDB,
	}
}

// DBCount 返回 db 数量。
func (r *Router) DBCount() int { return r.dbCount }

// TablePerDB 返回每库表数。
func (r *Router) TablePerDB() int { return r.tablePerDB }

// TotalTables 返回全局总表数（dbCount * tablePerDB）。
func (r *Router) TotalTables() int { return r.totalTbls }

// RouteByString 用字符串 key 路由到 (dbIdx, tblIdx).
//
// 流程: fnv32(key) % total → n; dbIdx = n / tablePerDB; tblIdx = n.
// FNV 是 Go 标准库里最便宜的 string hash，分布性对随机字符串 (idempotency_key /
// charge_id / saga_id) 足够均匀。空字符串退化到 (0, 0).
//
// 注: tblIdx 是全局 0..99 的"表索引"（不是 db 内 0..9 的局部索引）。子表名按这个
// 全局 idx 编：transfers_07 在 db_0 (因为 7/10=0)。
func (r *Router) RouteByString(key string) (dbIdx, tblIdx int) {
	if key == "" {
		return 0, 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	n := int(h.Sum32() % uint32(r.totalTbls))
	return n / r.tablePerDB, n
}

// RouteByInt 数字 key 路由（moneyflow_runs.id BIGINT 之类）。
// 跟 accounting Router.RouteByID 完全相同的取模规则。
func (r *Router) RouteByInt(id int64) (dbIdx, tblIdx int) {
	if id < 0 {
		id = -id
	}
	n := int(id % int64(r.totalTbls))
	return n / r.tablePerDB, n
}

// TableName 给定 family 前缀 + 全局 tblIdx 拼表名（带 2 位补零）.
//
// 例: TableName("transfers", 7) → "transfers_07"
//     TableName("moneyflow_runs", 99) → "moneyflow_runs_99"
//
// 注意必须用 2 位补零，跟 DDL gen.sh 里 printf "%02d" 对齐。
//
// shadow=true → 拼 _shadow 后缀（全链路压测影子表）。
func (r *Router) TableName(family string, tblIdx int, shadow bool) string {
	suffix := ""
	if shadow {
		suffix = "_shadow"
	}
	return fmt.Sprintf("%s_%02d%s", family, tblIdx, suffix)
}

// TableNameCtx 从 ctx 读 shadow flag 后拼表名。等价于
//   TableName(family, tblIdx, IsShadow(ctx))
// repo 层推荐用这个，调用方只需要传 ctx，不用每个地方手动判 shadow。
func (r *Router) TableNameCtx(ctx context.Context, family string, tblIdx int) string {
	return r.TableName(family, tblIdx, IsShadow(ctx))
}

// DBName 给定 dbIdx 拼库名.
//
// 例: DBName(0) → "split_payment_db_0"
func (r *Router) DBName(dbIdx int) string {
	return fmt.Sprintf("split_payment_db_%d", dbIdx)
}

// Resolve 一步到位：ctx + key → (dbName, tableName)。便于 repo 层一行拿全。
// 自动判 shadow（从 ctx 读）.
//
// 例: Resolve(ctx, "transfers", "idem-abc-123") → ("split_payment_db_3", "transfers_37")
//     带 shadow ctx                              → ("split_payment_db_3", "transfers_37_shadow")
func (r *Router) Resolve(ctx context.Context, family, key string) (dbName, tableName string, dbIdx int) {
	d, t := r.RouteByString(key)
	return r.DBName(d), r.TableNameCtx(ctx, family, t), d
}

// ResolveByInt 数字 key 版本，自动判 shadow。
func (r *Router) ResolveByInt(ctx context.Context, family string, id int64) (dbName, tableName string, dbIdx int) {
	d, t := r.RouteByInt(id)
	return r.DBName(d), r.TableNameCtx(ctx, family, t), d
}

// AllTables 列出某 family 在所有 shard 上的全部子表名（用于 cross-shard scan，
// e.g. outbox worker 扫所有 pending）。返回 ((dbName, tableName), ...) 100 条。
// 自动判 shadow（按 ctx）。
func (r *Router) AllTables(ctx context.Context, family string) []ShardTable {
	shadow := IsShadow(ctx)
	out := make([]ShardTable, 0, r.totalTbls)
	for tblIdx := 0; tblIdx < r.totalTbls; tblIdx++ {
		out = append(out, ShardTable{
			DBIdx:     tblIdx / r.tablePerDB,
			DBName:    r.DBName(tblIdx / r.tablePerDB),
			TableIdx:  tblIdx,
			TableName: r.TableName(family, tblIdx, shadow),
		})
	}
	return out
}

// ShardTable 一张物理子表的全部坐标信息。
type ShardTable struct {
	DBIdx     int
	DBName    string
	TableIdx  int
	TableName string
}
