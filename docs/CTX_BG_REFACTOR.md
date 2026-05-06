# Background ctx 重构进度 — trace_id + shadow=false 上下文连贯性

## 背景

cron / kafka consumer / 渠道异步回调 / outbox 等 background goroutine 启动时
若直接复用 `parent ctx` 或 `context.Background()`，会出现两类问题：

1. **trace_id 缺失**：单 cycle 在日志里散乱，无法按 trace 聚合，故障排查难。
2. **shadow flag 漂移**：parent ctx 若意外携带 `shadow=true`（罕见但可能），
   background 会把压测流量灌到主表 / 真渠道 → **数据污染**，PCI / 合规事故。

## 标准修法

`payment-util/trace.NewBackground(parent, label, logger, timeout)` 返回：

- 不继承 parent.Done()（context.WithoutCancel）
- **新 trace_id**（父 ctx 没有就 Generate）
- **强制 shadow=false**（覆盖任何漂移）
- logger 自动带 trace_id + bg_task=label
- 自带 timeout

调用模式：每 tick / 每个 detached goroutine 起一次 NewBackground。

```go
case <-ticker.C:
    tickCtx, cancel := trace.NewBackground(ctx, "outbox-recovery", w.logger, 30*time.Second)
    if err := w.tick(tickCtx); err != nil {
        trace.Logger(tickCtx, w.logger).Error("...", zap.Error(err))
    }
    cancel()
```

如果是 shadow 重放 worker（极少数情况），用 `trace.NewBackgroundShadow` 显式
开启 shadow，便于审查时 grep 定位。

## 已完成 (12 处)

| 文件 | 类型 | commit |
| --- | --- | --- |
| payment-channel/internal/server/http_webhook.go | 渠道 webhook 入口 | 5af475b9 |
| payment-channel/internal/service/call_retry_worker.go | acquirer 重试 | 5af475b9 |
| payment-channel/internal/service/pending_query_worker.go | pending 查询 | 5af475b9 |
| order-core/internal/webhook/delivery.go | 出站 webhook 重试 | 5af475b9 |
| order-core/internal/service/expire_worker.go | PI 过期扫描 | (本批) |
| order-core/internal/service/refund_reconcile.go | refund 重试 | (本批) |
| order-core/internal/service/charge_workers.go | charge expire + reconcile | (本批) |
| order-core/internal/service/notify_service.go | 商户通知重试 | (本批) |
| accounting-system/internal/service/outbox_worker.go | outbox recovery + order recovery + cleanup（3 个 ticker） | (本批) |

## 待完成 (清单)

跨服务扫了 `time.NewTicker` 共 32 处，资金 / 调用关键路径优先：

### order-core 剩余

- internal/service/accounting_outbox_worker.go (`AccountingOutboxWorker.Run` + `AccountingOutboxArchiver.Run`)

### accounting-system 剩余

- internal/service/buffered_balance_worker.go
- internal/service/freeze_compensate_worker.go
- internal/service/tcc_recovery_worker.go
- internal/service/day_cut_service.go (cron-style，多 ticker)

### payment-core

- 看 `cmd/server/main.go` 的 worker 起点（claim_token / outbox / risk-flush）

### clearing-settlement

- 多个 cron 任务，按 file 一次过

### card-payment

- internal/reconcile/worker.go ✅ 已经按新模式跑 (NewBackground 内置)

### card-center / kms-manage

- 暂无 cron worker，跳过

## 验证

`grep -rn 'context\.Background\(\)' --include='*.go' | grep -v vendor | grep -v _test.go`
逐条审查：

- main.go 的 OTel init / fx OnStart 用 `context.Background()` 是 OK 的（启动期）
- bash 命令 ctx / shutdown ctx 用 `context.Background()` 是 OK 的
- worker / consumer / 异步 handler 应该 0 处 `context.Background()`，全部走 NewBackground

## P1-7 (sarama 接通) 进度

- payment-util/audit/kafkago：✅ 实现
- card-center wire kafka producer：✅ commit a212df89
- order-core wire：⏳ 待补
- accounting-system wire：⏳ 待补
- payment-core wire：⏳ 待补
