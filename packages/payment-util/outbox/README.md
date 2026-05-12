# outbox

Transactional outbox pattern + saga 骨架. 解决跨服务事件最终一致性的金标方案.

## 问题

```
微服务事务里:
  ┌─────────────────┐    ┌──────────────┐
  │  BEGIN tx       │    │  Kafka write │
  │  INSERT orders  │ ┄┄ │  (dual write)│
  │  COMMIT         │    │              │
  └─────────────────┘    └──────────────┘
         ▲                       ▲
         │                       │
  DB 成功了 但 Kafka 挂了        Kafka 成了 但 DB 回滚了
  下游永远丢消息                 下游收到不存在的事件
```

dual-write 没解。

## 方案

```
┌──────────────────────────────────┐    ┌─────────────────┐
│ BEGIN tx                         │    │  Publisher loop │
│  INSERT orders   (业务)          │    │   (单独 goroutine)│
│  INSERT tx_outbox (事件 staging) │ ─→ │   SELECT pending │
│ COMMIT                           │    │   send to Kafka │
└──────────────────────────────────┘    │   UPDATE published│
   原子! tx 回滚则两个都不存在        └─────────────────┘
```

publisher 挂了不影响业务 — outbox 行还在, 重启后接着发。

## 接入

### 1) 建表 (per service DB)

```sql
SOURCE outbox/schema.sql
```

### 2) 业务代码

```go
import "github.com/xiongwp/payment-util/outbox"

func (s *Svc) CreateOrder(ctx context.Context, ord Order) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil { return err }
    defer tx.Rollback()

    // 业务写
    if _, err := tx.ExecContext(ctx, "INSERT INTO orders ...", ...); err != nil {
        return err
    }

    // 同事务写 outbox
    evt, _ := outbox.NewEvent("order", ord.ID, "OrderCreated", ord)
    evt.Topic = "payment.orders"
    evt.Headers["trace_id"] = trace.ID(ctx)
    if err := outbox.Append(ctx, tx, evt); err != nil {
        return err
    }
    return tx.Commit()
}
```

### 3) Publisher loop (单实例)

```go
pub := &outbox.Publisher{
    DB:           db,
    Publish:      kafkaSend,  // func(ctx, Event) error
    Log:          log,
    PollInterval: 5*time.Second,
    BatchSize:    100,
    MaxRetries:   10,
    FailedDLQ:    true,
}
pub.MustRegister(prometheus.DefaultRegisterer, "myservice_outbox")
go pub.Loop(ctx)
```

### 4) Admin 操作

```go
// 重放失败的 (DLQ 里的)
n, _ := outbox.ReplayFailedDLQ(ctx, db, []string{"evt_abc", "evt_def"})

// 7 天前的 published 清掉
n, _ := outbox.CleanupPublished(ctx, db, 7*24*time.Hour)
```

## 指标 (prometheus)

| metric | 用 |
|---|---|
| `{ns}_events_published_total` | 速率 |
| `{ns}_events_failed_total` | DLQ 速率 (告警源) |
| `{ns}_oldest_pending_seconds` | publisher 健康 (>30s = 异常) |

## Saga (跨多服务编排)

`saga.go` 给了状态机骨架. 真生产协调器要:
1. `saga_instance` 表 + `saga_step_result` 表
2. 监听 Kafka step.completed.{name} 推 current_step
3. 失败时逆序发 compensation outbox event
4. ops 触发 retry / abandon

split-payment 的资金分账, refund cross-service 都是典型 saga 应用. 后续按 PR 加上来。

## 接入路线图

应该接 outbox 的服务 (按优先级):
1. **clearing-settlement** — payout → tax-reporting / accounting / merchant-webhook 多下游
2. **refund-engine** — refund 完成 → accounting / merchant-webhook / tax 三发
3. **payment-core** — 充值成功 → split-payment / accounting / merchant-webhook
4. **subscription** — billing cycle 出 invoice → billing-system / merchant-webhook
5. **dispute-service** — dispute won/lost → accounting reversal / merchant-webhook

每服务接的 cost: 加 schema + ~20 行接入代码 + 改原"直发 Kafka"为 Append.
