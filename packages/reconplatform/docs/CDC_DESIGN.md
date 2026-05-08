# reconplatform CDC + 动态脚本对账系统设计

## 目标

1. **数据归集**：从全部 13 个业务服务的 MySQL binlog 实时同步到 reconplatform Redis
2. **多维索引**：以 `order_id` / `transaction_id` / `pi_id` 等业务主键索引，跨服务关联查询 O(1)
3. **动态脚本**：对账脚本用 Golang，热加载（Yaegi 解释器），不停机更新
4. **schema 自描述**：业务库表结构自动同步到 recon meta，脚本编辑器右侧可实时搜索 / 自动补全
5. **触发机制**：手动 / cron / 事件驱动 三种触发方式

## 包布局

```
packages/reconplatform/
├── cmd/server/main.go                  # fx 装配入口
├── internal/
│   ├── ingester/                       # 新增：CDC binlog 摄入
│   │   ├── canal.go                    # go-mysql-org/go-mysql/canal 封装
│   │   ├── parser.go                   # row event → recon.Event
│   │   ├── publisher.go                # → Redis (events + indexes)
│   │   └── source.go                   # 服务源配置（binlog endpoint / pos）
│   ├── meta/                           # 新增：schema 同步
│   │   ├── syncer.go                   # information_schema 拉取，定时刷新
│   │   ├── store.go                    # Redis 存 schema JSON
│   │   └── api.go                      # GET /api/v1/meta/* 给编辑器用
│   ├── store/                          # 已有：Redis 客户端
│   │   ├── redis.go                    # 已存在
│   │   ├── event.go                    # 新增：Event Get/Set/Index
│   │   └── search.go                   # 新增：按业务 key 跨服务查询
│   ├── engine/                         # 已有，扩展为脚本驱动
│   │   ├── runner.go                   # 已存在
│   │   ├── yaegi_loader.go             # 新增：Yaegi 加载 .go 脚本
│   │   ├── script.go                   # 脚本接口定义 + Context
│   │   └── builtin.go                  # 暴露给脚本的 helper（搜索 / 比对 / diff）
│   ├── scheduler/                      # 新增：cron + 事件触发
│   │   ├── cron.go                     # robfig/cron
│   │   └── trigger.go                  # Redis Streams 订阅触发
│   ├── api/                            # 新增：admin web HTTP
│   │   ├── server.go
│   │   ├── scripts.go                  # CRUD 脚本
│   │   ├── search.go                   # 实时搜索（业务 key）
│   │   ├── meta.go                     # schema 浏览
│   │   └── result.go                   # 对账结果查询
│   └── webui/                          # 新增：embed 编辑器 HTML
│       └── editor.html                 # CodeMirror + autocomplete from /api/v1/meta
├── scripts/                            # 内置示例脚本
│   ├── pi_amount_consistency.go        # PI/Charge/Txn 金额一致性
│   └── outbox_lag.go                   # outbox pending > 1h 告警
└── docs/
    └── CDC_DESIGN.md                   # 本文档
```

## Redis Key 设计

```
# 事件存储（最新值，可配 TTL，默认永久）
recon:evt:<svc>:<table>:<pk>          → JSON {row data + _meta {ts, op, binlog_pos}}

# 索引（业务 key → 关联事件集合）
recon:idx:<index_name>:<value>        → SET of "<svc>:<table>:<pk>"
  例：recon:idx:order_id:ord_xxx
       recon:idx:transaction_id:tx_yyy
       recon:idx:pi_id:pi_zzz
       recon:idx:merchant_id:m_ddd:date:2026-05-08
                                       (复合索引：商户 + 日期)

# Schema meta（自动从 information_schema 同步）
recon:meta:tables                     → SET of "<svc>:<table>"
recon:meta:schema:<svc>:<table>       → JSON [{name, type, nullable, key, comment}]
recon:meta:idx_keys                   → SET of available index names

# 脚本
recon:script:<id>                     → JSON {name, code, schedule, triggers, version}
recon:script:list                     → SORTED SET (id by updated_at)

# 对账结果
recon:result:<script_id>:<run_id>     → JSON {started_at, status, diffs[], stats}
recon:result:list:<script_id>         → SORTED SET (run_id by ts，最近 100)

# Binlog 位点（每个服务一份，crash recovery）
recon:cdc:pos:<svc>                   → JSON {file, position, gtid}

# Streams（事件驱动触发用）
recon:stream:events                   → XADD <ts> svc=<x> table=<y> pk=<z>
                                        consumer group: recon-engine
```

## 流程

### 1. CDC 摄入

每个业务服务 MySQL 配置 `log-bin = mysql-bin` `binlog_format = ROW`。

reconplatform/ingester 启动时：

```go
type Source struct {
    Service string   // "order-core"
    Addr    string   // mysql 地址
    User    string   // recon 专用账号（REPLICATION SLAVE 权限）
    Tables  []string // 关心的表（白名单）
    Indexes map[string][]string // 表 → 用作索引的列
                                 // 例：{"payment_intents": ["id", "merchant_id", "customer_id"]}
}
```

go-mysql 的 canal.Canal 订阅 binlog → onRow callback → publisher 写 Redis：

```go
func (p *Publisher) Publish(evt *Event) error {
    // 1. 写主存
    pipe := p.redis.Pipeline()
    key := fmt.Sprintf("recon:evt:%s:%s:%s", evt.Service, evt.Table, evt.PK)
    pipe.Set(ctx, key, evt.JSON(), ttl)

    // 2. 维护所有索引
    for idxName, val := range evt.IndexValues() {
        idxKey := fmt.Sprintf("recon:idx:%s:%v", idxName, val)
        pipe.SAdd(ctx, idxKey, fmt.Sprintf("%s:%s:%s", evt.Service, evt.Table, evt.PK))
    }

    // 3. 写 Stream（事件驱动触发）
    pipe.XAdd(ctx, &redis.XAddArgs{
        Stream: "recon:stream:events",
        Values: map[string]any{"svc": evt.Service, "table": evt.Table, "pk": evt.PK, "op": evt.Op},
    })

    // 4. 持久化 binlog 位点（每秒批 flush，避免 fsync 风暴）
    p.posTracker.Update(evt.Service, evt.BinlogFile, evt.BinlogPos)

    _, err := pipe.Exec(ctx)
    return err
}
```

### 2. Schema meta 自动同步

`meta/syncer.go` 每 5min 拉一次 `information_schema.columns`：

```sql
SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, DATA_TYPE, IS_NULLABLE,
       COLUMN_KEY, EXTRA, COLUMN_COMMENT, ORDINAL_POSITION
FROM information_schema.columns
WHERE TABLE_SCHEMA = '<service-db>'
ORDER BY TABLE_NAME, ORDINAL_POSITION;
```

写入 `recon:meta:schema:<svc>:<table>`。Schema 变化（DDL）自动反映。

### 3. 动态脚本

用 Yaegi（github.com/traefik/yaegi）—— Go 的解释器，支持 90% 标准库 + 我们暴露的 recon 包。

脚本接口：

```go
package recon

// 脚本必须实现 Check 函数
type Script interface {
    Check(ctx *Context) (*Result, error)
}

type Context struct {
    // 跨服务搜索：返回 idx_name=value 关联的所有 event
    GetByIndex(ctx context.Context, idxName, value string) Events
    // SCAN 一段时间内的 events
    Scan(ctx context.Context, indexName string, since, until time.Time) []string
    // schema 查询（脚本可动态决定列）
    Schema(svc, table string) []Column

    Logger *zap.Logger
}

type Events []Event
func (es Events) Find(svc, table string) Event { ... }

type Event struct {
    Service, Table, PK string
    Row map[string]any
    Op string  // INSERT / UPDATE / DELETE
    Timestamp time.Time
}

func (e Event) Int(col string) int64
func (e Event) Str(col string) string
func (e Event) Time(col string) time.Time
```

脚本示例：

```go
package main

import "recon"

func Check(ctx *recon.Context) (*recon.Result, error) {
    r := &recon.Result{Name: "PI 金额三表一致性"}
    // 找最近 24h 所有 pi_id
    pis := ctx.Scan("pi_id", recon.Last24Hours())
    for _, pi := range pis {
        events := ctx.GetByIndex("pi_id", pi)
        order := events.Find("order-core", "payment_intents")
        txn   := events.Find("accounting-system", "account_transaction")
        chg   := events.Find("payment-channel", "card_charges")
        if order.IsEmpty() || txn.IsEmpty() || chg.IsEmpty() {
            r.AddDiff("missing", pi, fmt.Sprintf("missing leg: order=%v txn=%v chg=%v",
                !order.IsEmpty(), !txn.IsEmpty(), !chg.IsEmpty()))
            continue
        }
        amt := order.Int("amount")
        if amt != txn.Int("amount") || amt != chg.Int("amount") {
            r.AddDiff("amount_mismatch", pi, map[string]any{
                "order": amt,
                "txn":   txn.Int("amount"),
                "chg":   chg.Int("amount"),
            })
        }
    }
    return r, nil
}
```

加载方式：admin web 上点「保存脚本」→ Redis 存源码 → `engine.yaegi_loader` 重新解释加载 → 立即生效。

### 4. 实时搜索（编辑器右侧）

编辑器面板：
- 左侧：CodeMirror Go syntax + 自动补全（pull from `/api/v1/meta/tables` + `/api/v1/meta/schema/{svc}/{table}`）
- 右侧：搜索框 → 输入 `order_id=ord_xxx` 或 `transaction_id=tx_yyy` → 立即返回该 key 关联的所有 event（JSON tree 展示）
- 底部：试运行（Yaegi exec on Redis 数据子集）+ 实际运行（持久化结果）

### 5. 触发机制

- **手动**：admin web 「Run Now」按钮
- **Cron**：脚本配 `schedule: "*/5 * * * *"`，robfig/cron 调度
- **事件驱动**：脚本声明 `triggers: ["order-core:payment_intents", "accounting-system:account_transaction"]`，Redis Streams XREAD 消费 group，对应 event 到达即触发

## 实施分期

**阶段 1（本周）**：CDC 单服务（order-core）跑通 → meta sync → Yaegi 脚本加载 → 1 个示例脚本（PI 金额一致性）  
**阶段 2（下周）**：13 个服务全接 CDC → 脚本编辑 web UI（搜索 + 自动补全）→ cron 调度  
**阶段 3（再下周）**：事件驱动触发 → 告警接入 alertmanager → 历史 result 查询

## 依赖

```
github.com/go-mysql-org/go-mysql/canal  // binlog 订阅
github.com/redis/go-redis/v9             // 已有
github.com/traefik/yaegi                 // Go 解释器（动态脚本）
github.com/robfig/cron/v3                // cron 调度
```

## 安全

- recon 用专用 mysql 账号（REPLICATION SLAVE / REPLICATION CLIENT 只读）
- 脚本沙箱：Yaegi 默认禁 unsafe / os.Exec 类危险操作（白名单导入）
- admin web 走 user-merchant-core IntrospectToken（同 config-center 模式）
- Redis 走 mTLS（payment-stack 内部）

## 监控指标

```
recon_cdc_lag_seconds{service}              # binlog 滞后
recon_cdc_event_total{service,table,op}     # 事件计数
recon_meta_sync_total{service,success}      # schema 同步状态
recon_script_run_total{script,status}       # 脚本运行
recon_script_run_duration_seconds{script}   # 运行时长
recon_diff_total{script,type}               # 差异条数（按类型）
```
