// Package sharding — oauth2-server 10 库 × 10 表 = 100 分片路由器.
//
// 分片对象:
//   - clients          按 client_id 一致性 hash → 100 shard
//   - revoked_tokens   按 jti hash → 100 shard
//   - admin_audit      按 actor hash → 100 shard (跟 user-merchant-core 同 layout)
//
// 不分:
//   - rsa_keys (key 数量极少, 全表几行;
//     甚至放 etcd / config-center 也行, 暂时 keep MySQL 单表方便冷备)
//
// 跟 user-merchant-core / order-core / accounting-system 同 layout, 跨服务
// 同 actor 落同一逻辑分片号, join / 排查时 SQL 拼接省心。

package sharding

import (
	"fmt"
)

const (
	// ShardDBCount 分库数 (横向: 物理 DB 实例数)
	ShardDBCount = 10
	// ShardTablePerDB 每库分表数 (纵向: 单库内逻辑分表)
	ShardTablePerDB = 10
	// ShardTableTotal 全局分片数 (100)
	ShardTableTotal = ShardDBCount * ShardTablePerDB
)

// Router 路由器.
//
// 算法:
//   n = fnv1a(key) % 100
//   dbIndex   = n / 10      (0-9, 对应 DB: oauth2_db_0..9)
//   tableIdx  = n           (0-99, 全局表 idx, 表名 e.g. clients_42)
type Router struct {
	dbCount    int
	tablePerDB int
}

// NewRouter 默认生产配置.
func NewRouter() *Router {
	return &Router{dbCount: ShardDBCount, tablePerDB: ShardTablePerDB}
}

// NewRouterWithConfig 自定义 (测试 / 低容量).
func NewRouterWithConfig(dbCount, tablePerDB int) *Router {
	return &Router{dbCount: dbCount, tablePerDB: tablePerDB}
}

func (r *Router) DBCount() int    { return r.dbCount }
func (r *Router) TablePerDB() int { return r.tablePerDB }
func (r *Router) Total() int      { return r.dbCount * r.tablePerDB }

// RouteByKey 一致性 hash 字符串 key 到分片.
//
// 用 FNV-1a (跟 user-merchant-core / order-core 同算法), 跨服务一致。
func (r *Router) RouteByKey(key string) (dbIndex, tableIdx int) {
	if key == "" {
		return 0, 0
	}
	const (
		offset64 uint64 = 14695981039346656037
		prime64  uint64 = 1099511628211
	)
	h := offset64
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= prime64
	}
	n := int((h & 0x7fffffffffffffff) % uint64(r.Total()))
	return n / r.tablePerDB, n
}

// ─── 各表的便利函数 ──────────────────────────────────────────────────

// ClientsTable 按 client_id 路由到的 clients_NN 分表 + db_N.
//   "mer_merchant_001_abc" → ("oauth2_db_7", "clients_72")
func (r *Router) ClientsTable(clientID string) (dbName, tableName string) {
	db, t := r.RouteByKey(clientID)
	return fmt.Sprintf("oauth2_db_%d", db), fmt.Sprintf("clients_%02d", t)
}

// RevokedTokensTable 按 jti 路由到 revoked_tokens_NN.
//   "abc123..." → ("oauth2_db_3", "revoked_tokens_31")
func (r *Router) RevokedTokensTable(jti string) (dbName, tableName string) {
	db, t := r.RouteByKey(jti)
	return fmt.Sprintf("oauth2_db_%d", db), fmt.Sprintf("revoked_tokens_%02d", t)
}

// AdminAuditTable 按 actor 路由到 admin_audit_NN.
//   "ops@example.com" → ("oauth2_db_5", "admin_audit_56")
func (r *Router) AdminAuditTable(actor string) (dbName, tableName string) {
	db, t := r.RouteByKey(actor)
	return fmt.Sprintf("oauth2_db_%d", db), fmt.Sprintf("admin_audit_%02d", t)
}

// AllShards 列举全部 100 个 (db, table) 组合 — 给 ops 工具 / 批量 GC / 全表扫
// 时遍历用。
func (r *Router) AllShards(baseTable string) []struct {
	DB, Table string
} {
	out := make([]struct{ DB, Table string }, 0, r.Total())
	for i := 0; i < r.Total(); i++ {
		out = append(out, struct{ DB, Table string }{
			DB:    fmt.Sprintf("oauth2_db_%d", i/r.tablePerDB),
			Table: fmt.Sprintf("%s_%02d", baseTable, i),
		})
	}
	return out
}
