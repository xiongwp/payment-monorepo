# split-payment 元数据库 init 脚本

11 张表的 DDL, 由 MySQL 容器启动期通过 `/docker-entrypoint-initdb.d/` 自动灌入.

## 文件 → 表清单

| 文件 | 表 | 用途 |
|---|---|---|
| `1_moneyflow_core.sql` | `moneyflow_graphs`, `moneyflow_runs` | Graph DSL + 执行实例 |
| `2_stripe_entities.sql` | `connected_accounts`, `transfers`, `application_fees`, `payouts`, `reversals` | SP-3/SP-4 Stripe-style 资金原语 |
| `3_versioning.sql` | `moneyflow_graph_versions` | SP-7 Graph 版本快照 |
| `4_saga.sql` | `moneyflow_sagas` | SP-3A 持久化 saga 状态 |
| `5_outbox.sql` | `event_outbox`, `reversal_retry_outbox` | L5/R5 事件 + 反转重试 outbox |
| `6_cron_lease.sql` | `cron_lease` | X3 多副本 cron 互斥 lease |

## 用法 (docker compose)

把本目录挂到 MySQL 容器的 init 目录:

```yaml
services:
  shared-meta:
    image: mysql:8.0
    environment:
      MYSQL_DATABASE: split_payment
      MYSQL_ROOT_PASSWORD: password
    volumes:
      - ./packages/split-payment/database/metadb/init:/docker-entrypoint-initdb.d:ro
```

MySQL 首次启动 (数据卷空) 时自动按文件名字典序灌入. 已有数据卷时**不会重跑** —
要重新灌, 先 `docker compose down -v` 清卷再 `up`.

## 老库迁移 (已有数据卷)

老库已经有 split-payment 的部分表 (历史 EnsureSchema 建的), 不需要清卷重灌. 但
**老 `moneyflow_runs` 表可能缺 `hold_until` / `hold_released` 两列** (PH3-7 后才加).
手动 ALTER 一次:

```sql
ALTER TABLE moneyflow_runs ADD COLUMN hold_until DATETIME DEFAULT NULL;
ALTER TABLE moneyflow_runs ADD COLUMN hold_released TINYINT(1) NOT NULL DEFAULT 0;
ALTER TABLE moneyflow_runs ADD KEY idx_hold_expired (hold_released, hold_until);
```
报 `Error 1060 Duplicate column name` 表示列已存在, 忽略.

同理 `moneyflow_graphs` 可能缺 `active_version_id` (SP-7 后加):
```sql
ALTER TABLE moneyflow_graphs ADD COLUMN active_version_id BIGINT DEFAULT NULL;
```

## 跟代码侧的约定

应用启动**不再做 DDL** — `internal/repo/*.Ensure*Schema` 函数全部移除. 应用只
`db.PingContext` 检查可达性. 跟 card-center / order-core / accounting-system 一致.
