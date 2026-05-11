# oauth2-server 分库分表

跟 user-merchant-core / order-core / accounting-system 完全同 layout: **10 库 × 10 表 = 100 全局分片**, FNV-1a hash 跨服务一致。

## 分片对象

| 表 | 分片 key | 数量 | 理由 |
|---|---|---|---|
| `clients_NN` | `client_id` FNV-1a | 100 | 跨平台时 client 数量增长 |
| `revoked_tokens_NN` | `jti` FNV-1a | 100 | 每次 revoke 写一行, 高频累积 |
| `admin_audit_NN` | `actor` FNV-1a | 100 | 跟 u-m-c admin_audit_log 同模式 |
| `rsa_keys` | — | 1 (oauth2_db_0) | key 极少 (历史 10+ 个), 全局单表 |
| `client_index` | — | 1 (oauth2_db_0) | client_id → (db,table) 反查映射 |

## 路由公式

```
n = fnv1a(key) % 100
dbIndex   = n / 10     (0-9, 对应 DB oauth2_db_0..9)
tableIdx  = n          (0-99, 全局表 idx, 表名 e.g. clients_42)
```

## 物理布局示例

```
oauth2_db_0:   clients_00..09    revoked_tokens_00..09    admin_audit_00..09    rsa_keys    client_index
oauth2_db_1:   clients_10..19    revoked_tokens_10..19    admin_audit_10..19
...
oauth2_db_9:   clients_90..99    revoked_tokens_90..99    admin_audit_90..99
```

## 部署

```bash
# 1. 生成完整 ~5000 行 schema
cd database/init
bash generate.sh > 02_schema_full.sql

# 2. 在生产 MySQL 上跑
mysql -h <host> -u root -p < 02_schema_full.sql

# 3. 验证
mysql -h <host> -u root -p -e "SHOW DATABASES LIKE 'oauth2_db_%'"
mysql -h <host> -u root -p -e "USE oauth2_db_0; SHOW TABLES" | head
```

## 切换到 ShardedMySQLStore

`config/config.yaml`:

```yaml
database:
  dsn_prefix: "user:pwd@tcp(mysql-cluster:3306)/oauth2_db_"
  sharded: true
```

`cmd/server/main.go` 选择 store:

```go
if cfg.Database.Sharded {
    s, err = store.NewShardedMySQLStore(func(dbIdx int) string {
        return cfg.Database.DSNPrefix + strconv.Itoa(dbIdx) +
            "?charset=utf8mb4&parseTime=true&loc=UTC"
    })
} else if cfg.Database.DSN != "" {
    db, _ := sql.Open("mysql", cfg.Database.DSN)
    s = store.NewMySQLStore(db)
} else {
    s = store.NewMemoryStore()
}
```

## 跨 shard 查询

- **List clients** (ops 后台): fan-out 全 100 shard, merge + 排序后返
- **GC revoked**: 每 shard 独立 `DELETE FROM revoked_tokens_NN WHERE expires_at <= NOW() LIMIT 10000`
- **Search by 非分片 key** (e.g. owner_id): 必须全扫;
  ⚠️ 高频查询应该建二级索引: `oauth2_db_0.client_by_owner (owner_id, client_id, shard_db, shard_table)`

## Reshard SOP (扩容 100 → 1000)

详见 user-merchant-core/SHARDING_MIGRATION.md。oauth2-server 跟它共享 RouterV2 layout-version 编码,扩容路径一致。

简要:
1. Phase 1: 上 EncodeAccountIDV2 layout 高位 (新 client 落新 layout)
2. Phase 2: 配 dual-read (新老 layout 都查, 写双写)
3. Phase 3: 跑 reshard worker 把老数据搬到新 layout
4. Phase 4: 灰度切流量到新 layout
5. Phase 5: 清理老表
