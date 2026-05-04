# user-merchant-core 分库分表迁移指南

跟 accounting-system / order-core / payment-channel 对齐：**10 库 × 10 表 = 100 张全局分片表**。
**No data migration** — 新部署 / clean cutover。

## 已完成

- ✅ `internal/sharding/router.go` —— Router (RouteByUserID / RouteByMerchantID / TableName 等)
- ✅ `pkg/dbx/manager.go` —— Manager 扩展 NewShardedManager / GetShard / AllShards / ShardCount
- ✅ `pkg/dbx/metrics.go` —— PoolCollector 扩展 shard pool 统计 (label=shard-N)
- ✅ `database/metadb/init/init.sql` —— meta 留 leaf_alloc / RBAC / idempotency / email_codes / audit + 4 张 lookup 反查索引
- ✅ `database/metadb/init/init_shadow.sql` —— meta shadow 副本（业务分片表移到 userdb）
- ✅ `database/userdb/templates/schema.sql` —— 11 张分片表 schema 模板
- ✅ `database/userdb/scripts/generate.sh` —— 产 0_init.sql..9_init.sql + _shadow 变体
- ✅ `internal/repo/sharded.go` —— `shardForUser` / `shardForMerchant` / `allShardsForFanout`
- ✅ `internal/repo/schema_migrator.go` —— ApplyShadowTables 跨 10 shards 自愈
- ✅ `docker-compose.yml` —— 1 meta + 10 shard MySQL 容器 + DSN env 注入
- ✅ `internal/repo/merchant_secret.go` —— 改造（按 merchant_id 路由）
- ✅ `internal/repo/merchant.go` —— 改造（merchant_id 路由 + merchant_lookup 反查 + BatchGet shard 分组 + List/ListActive fanout + PurgeDeletedBefore fanout + TransitionKYC 跨 DB 妥协）
- ✅ `internal/repo/user.go` —— 改造（user_id 路由 + user_lookup/auth_lookup/session_lookup 反查 + RBAC 跨 DB 拆 join + CountRecentFailedLogins fanout）
- ✅ `cmd/server/main.go` —— newShardRouter / newDBManager 读 shard DSN + applyShadowTables OnStart 钩子
- ✅ `internal/repo/db_manager.go` —— re-export NewShardedManager

## 行为说明

### 反查索引（meta）

users / merchants 等分片后，反查路径不能直接命中。在 meta 加 lookup 表：

| 表 | 主键 | 用法 |
|---|---|---|
| `user_lookup` | (lookup_type, lookup_value) | GetUserByEmail / Username / Phone |
| `auth_lookup` | (auth_type, identifier) | FindAuth / MarkAuthVerified |
| `session_lookup` | token | GetSession / DeleteSession |
| `merchant_lookup` | (lookup_type, lookup_value) | GetByEmail / GetByKeyHash |

写入路径：
- CreateUser / UpdateUser → reserve user_lookup（INSERT，dup 返业务错） → shard insert
- CreateSession → INSERT session_lookup → shard insert
- AddAuth → INSERT auth_lookup（dup 返 ErrAuthExists）→ shard insert
- SoftDelete / DeleteSession / RemoveAuth → 同步删 lookup

读取路径：
- 反查（by email / token / identifier）→ meta lookup → user_id / merchant_id → router → shard get

### 跨 DB 操作

| 场景 | 处理 |
|---|---|
| `BatchGet(ids)` | 按 shard 分组，并发查每片，结果合并 |
| `List` / `ListActive` | fanout 100 分片 + 本地合并排序 + offset/limit |
| `PurgeDeletedBefore` | fanout 100 分片，每片单事务删 |
| `TransitionKYC` | shard tx 写 merchant + meta 写 audit（best-effort，不在单 tx） |
| `CountRecentFailedLogins` | userID > 0 路由单 shard；userID==0 fanout by IP |
| `ListRoles` / `ListPermissions` / `HasPermission` | shard 取 role_id 集合 → meta 查字典（拆 join） |
| `ReviewDocument(docID)` | fanout 100 shards 找 doc → 单 shard UPDATE |

### Cross-shard 唯一约束

users 跨 shard 后，username/email/phone 唯一约束由 user_lookup PK 保证：
1. CreateUser INSERT user_lookup（dup → ErrUsernameExists/ErrEmailExists/ErrPhoneExists）
2. INSERT user 行到 shard
3. 失败回滚 user_lookup

类似机制：auth_lookup（auth_type+identifier 跨 shard 唯一）、session_lookup（token 跨 shard 唯一）。

## Layout 兼容性

- user_id 段位 `[1e8, 9e8)` 跟 accounting-system 保留段对齐 ✓
- merchant_id 用 `shadow.EncodeID(IDTypeMerchantID, ...)` 19 位 numeric，按位编码 ✓
- shadow 流量自动路由到 `_shadow` 后缀表（router.TableName 包了 shadow.TableName）✓
- 跟 accounting-system / order-core / payment-channel 同一 shard 数 = 跨服务用相同 user_id 落同一逻辑分片号 ✓

## 验证

- `docker compose up -d` 起栈
- 11 个 MySQL 容器 healthy（meta + shard-0..9）
- exec 进每个 shard 容器：`SHOW TABLES IN user_merchant_db_N` 看到 11 × 10 = 110 张主表 + 110 张影子表
- 应用启动 log 看 `apply shadow tables ensured created_or_existing=...`
- e2e（注册用户 / 查询商户 / KYC 流程）验证路由 + 反查 + fanout 行为
