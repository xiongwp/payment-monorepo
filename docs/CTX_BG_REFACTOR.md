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

## 已完成 (17 处)

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
| accounting-system/internal/service/buffered_balance_worker.go | balance flush 主账写 | 28c81383 |
| accounting-system/internal/service/freeze_compensate_worker.go | 兜底补偿 | 28c81383 |
| accounting-system/internal/service/tcc_recovery_worker.go | TCC 三轨扫描 | 28c81383 |
| order-core/internal/service/accounting_outbox_worker.go | dispatch + archiver（2 个） | 28c81383 |
| user-merchant-core/internal/service/retention.go | 7 年合规真删 | (本批) |

## 待完成 / 已审核免修 (清单)

跨服务扫了 `time.NewTicker` 共 32 处。资金 / 调用关键路径已 17/17 完成。
剩余 15 处均不涉及主表 / 真渠道写：

### 已审核免修

- `accounting-system/internal/service/day_cut_service.go:297`
  ticker 仅做"进度日志"，真正的日切已在 line 286 自己 `shadow.WithShadow(
  context.WithoutCancel(ctx), false)` 显式覆盖，等价于 NewBackground
- `risk-manage/internal/audit/clickhouse_sink.go` — 内部 batch flush，非业务路径
- `risk-manage/internal/store/postgres_policy.go` — 缓存 TTL 重读
- `risk-manage/internal/store/predebit_commits.go` — 内存 commit 回收
- `risk-manage/internal/synthetic/synthetic.go` — synthetic 探测器，本来就 isolation
- `risk-manage/internal/metrics/token_source.go` — token 刷新
- `risk-manage/cmd/server/main.go` — fx 起点 ticker（启动期）
- `card-payment/internal/reconcile/worker.go` — ✅ 内置 NewBackground
- `payment-channel/internal/idgen/leaf.go:181` — id 生成 buffer 异步 refill，内存
- `id-generator/internal/segment/buffer.go:32` — 同上
- `id-generator/internal/worker/etcd_worker.go` — etcd 选主
- `payment-channel/cmd/server/main.go` — fx 起点 ctx
- `payment-util/serviceregistry/registrar.go` — etcd lease keepalive
- `payment-channel/internal/mockserver/util.go` — dev mockserver

### card-center / kms-manage

- 暂无 cron worker，跳过

### payment-core / clearing-settlement

- grep `time.NewTicker` 0 处。worker 集中在 service 层方法，无独立 ticker。

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
