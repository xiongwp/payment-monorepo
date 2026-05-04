# Stack — 全栈总编排

把基础设施 + 所有 Go 服务 + 两个 admin-web 串成一个 docker compose。

## 目录布局假设

`payment-admin-web/` 与其他 9 个 repo 平级：

```
<repos-root>/
  ├── kms-manage/
  ├── risk-manage/
  ├── payment-channel/
  ├── payment-core/
  ├── order-core/
  ├── user-merchant-core/
  ├── accounting-system/
  ├── accounting-admin-web/
  └── payment-admin-web/         ← 本目录在这里
        └── stack/
              ├── docker-compose.yml
              ├── init-db/bootstrap.sh
              └── README.md
```

## 启动

```bash
cd payment-admin-web
./deploy.sh up                  # 全部拉起
./deploy.sh up shared-meta redis  # 只起子集
./deploy.sh status              # 看 ps
./deploy.sh logs order-core     # 跟踪某个服务
./deploy.sh down                # 停掉（保留卷）
./deploy.sh down --volumes      # 停 + 清数据
```

`deploy.sh` 会先确保 `payment-stack` docker network 存在，然后 `docker compose -f stack/docker-compose.yml ...`。

## 数据库拓扑

**11 个 MySQL 实例，每实例承载多个 database** —— 对应原拓扑里的 1 个 meta + 10 个分片：

| MySQL 实例 | 容器 | host port | 装的 database |
|---|---|---|---|
| meta | `shared-meta` | 3306 | `user_merchant_meta`, `paychan_meta`, `order_meta`, `account_meta`(自动建) |
| shard 0 | `shared-shard-0` | 3307 | `paychan_db_0`, `order_db_0`, `accounting_db_0` |
| shard 1 | `shared-shard-1` | 3308 | `paychan_db_1`, `order_db_1`, `accounting_db_1` |
| … | `shared-shard-N` | 3306+1+N | `paychan_db_N`, `order_db_N`, `accounting_db_N` |
| shard 9 | `shared-shard-9` | 3316 | `paychan_db_9`, `order_db_9`, `accounting_db_9` |

合计 **30 个分库分表 db**（3 service × 10 shard）+ **4 个 meta db**，分布在 11 个 MySQL 实例上。

`db-init` 容器在 11 个 MySQL 全 healthy 后跑 `init-db/bootstrap.sh`：

1. `shared-meta` ← `user-merchant-core/database/metadb/init/init.sql` + `payment-channel/database/metadb/init/init.sql` + `order-core/database/metadb/init/init.sql`
2. 对每个 `shared-shard-N` ← 三个 `${i}_init.sql`（paychan / order / accounting 同序号）

所有 SQL 用 `CREATE * IF NOT EXISTS`，重跑无害；卷 `shared-{meta,shard-N}-data` 持久化数据，要清空时 `down --volumes`。

## 端口外露

| 容器 | host port | 说明 |
|---|---|---|
| shared-meta | 3306 | MySQL（meta 实例）|
| shared-shard-0..9 | 3307..3316 | MySQL（10 个分片实例）|
| redis | 6379 | Redis |
| kms-manage | 9290 / 9390 | gRPC / metrics |
| risk-manage | 9490 / 9590 | gRPC / metrics |
| payment-channel | 9092 / 9192 / 9093 | gRPC / metrics / mockserver |
| accounting-system | 50051 / 9091 / 8888 | gRPC / metrics / admin HTTP |
| payment-core | 9090 / 9190 | gRPC / metrics |
| order-core | 9091 | gRPC |
| user-merchant-core | 9191 / 9291 | gRPC / metrics |
| accounting-admin-web | 8080 | UI（http://localhost:8080）|
| payment-admin-frontend | 8081 | UI（http://localhost:8081）|

## 已知简化

- 没起 Kafka / ZooKeeper / etcd —— accounting-system 在 `hot_path.enabled=false` 默认配置下不需要它们。需要 hot path 时按 `accounting-system/docker-compose.yml` 补上 service 定义。
- `kms-manage` 数据卷 `kms-data`：第一次启动会写入 master key（首次部署前可 `./deploy.sh init-kms`，看 wrapper 命令）。
- `accounting.counter_accounts` 这种业务字段只能由运维通过 admin-web 注册 channel business_type 后再填回 `order-core/config/config.yaml`，再 `restart order-core` 生效。
