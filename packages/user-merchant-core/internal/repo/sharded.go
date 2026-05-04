// sharded.go — repo 层共享的分片路由 helper。
//
// 5 个 repo（user / merchant / merchant_secret / audit / idempotency）原本都直接
// 用 m.GetMeta() 拿单一连接 + 默认表名（如 "users"）。分库分表后路由变成：
//
//   1. 用 router 把 user_id / merchant_id 解到 (dbIdx, globalTblIdx)
//   2. 用 m.GetShard(dbIdx) 拿对应分库连接
//   3. 用 router.TableName(ctx, base, globalTblIdx) 拿分片表名（含 _shadow 后缀）
//   4. 调 db.Table(tableName).Where(...).First(...) / Updates(...) / Delete(...)
//
// 本包封装上述 4 步成一个单点入口，避免每条 SQL 都重复写。
//
// 留 meta 表（leaf_alloc / RBAC 字典 / idempotency_key / email_codes /
// admin_audit_log / merchant_kyc_audit）继续用 m.GetMeta() —— 跨 user / merchant
// 全局，不分片。
package repo

import (
	"context"

	"gorm.io/gorm"

	"github.com/xiongwp/user-merchant-core/internal/sharding"
)

// ShardRouter 给 repo 层注入路由器；NewXxxRepo 的 ctor 接受它。
//
// dev 单库模式：传 nil → repo 自动 fallback 到 m.GetMeta() + 不带 _NN 后缀的表名
// （兼容老部署 / 单元测试）。
type ShardRouter = sharding.Router

// shardForUser 给定 user_id 返回 (db 连接, 完整表名)。
//
//	dbIdx, gtbl := router.RouteByUserID(userID)
//	db := mgr.GetShard(dbIdx)
//	tbl := router.TableName(ctx, base, gtbl)
func shardForUser(ctx context.Context, mgr *Manager, router *ShardRouter, base string, userID int64) (*gorm.DB, string) {
	if router == nil || mgr.ShardCount() == 0 {
		return mgr.GetMeta(), base
	}
	dbIdx, gtbl := router.RouteByUserID(userID)
	return mgr.GetShard(dbIdx), router.TableName(ctx, base, gtbl)
}

// shardForMerchant 同上，按 merchant_id（字符串形式的 19 位 numeric ID）路由。
func shardForMerchant(ctx context.Context, mgr *Manager, router *ShardRouter, base string, merchantID string) (*gorm.DB, string) {
	if router == nil || mgr.ShardCount() == 0 {
		return mgr.GetMeta(), base
	}
	dbIdx, gtbl := router.RouteByMerchantID(merchantID)
	return mgr.GetShard(dbIdx), router.TableName(ctx, base, gtbl)
}

// allShardsForFanout 跨分片扫描时用：返回所有 (dbIdx, fullTableName) 对，
// repo.ListAll / SearchByEmail 这种"无 user_id" 路径用。
//
// 性能提醒：fanout 全 100 张分片 + each db RT，查询慢且 connection 占用大。
// 业务上能用 user_id / merchant_id 路由的尽量不要走 fanout。
func allShardsForFanout(ctx context.Context, mgr *Manager, router *ShardRouter, base string) []shardEntry {
	if router == nil || mgr.ShardCount() == 0 {
		return []shardEntry{{DB: mgr.GetMeta(), Table: base}}
	}
	out := make([]shardEntry, 0, router.TotalTableCount())
	for _, s := range router.AllShards() {
		out = append(out, shardEntry{
			DB:    mgr.GetShard(s.DBIndex),
			Table: router.TableName(ctx, base, s.TableIndex),
		})
	}
	return out
}

type shardEntry struct {
	DB    *gorm.DB
	Table string
}
