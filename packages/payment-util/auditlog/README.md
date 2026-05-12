# auditlog

各 service 接 audit-log 服务的 HTTP 客户端. async batch + retry + DLQ.

## Why

之前 4 个 P0 service (aml-screening / tokenization-vault / tax-reporting / merchant-webhook) 都用 `LogSink` 兜底 — 审计事件只写 zap 日志, 没真接 audit-log hash 链, 合规审计不可追溯.

此 client 让接入只要 ~5 行代码:

```go
import "github.com/xiongwp/payment-util/auditlog"

ac := auditlog.New(auditlog.Config{
    BaseURL: getenv("AUDITLOG_URL", "http://audit-log:8087"),
    Service: "aml-screening",
    Token:   os.Getenv("AUDITLOG_TOKEN"),
}, logger)
defer ac.Stop()

// 替代 LogSink:
ac.Emit(ctx, auditlog.Event{
    Action: "screen", ResourceType: "merchant", ResourceID: req.MerchantID,
    Actor: "service:aml-screening",
    Details: map[string]any{"action":"block", "top_score":92},
})
```

## 行为

| 场景 | 表现 |
|---|---|
| audit-log 正常 | events 1s 内 batch 发 (最多 50/批) |
| 网络抖动 | 单条重试 5 次, 指数退避 100ms→1.6s |
| audit-log 长时间挂 | retry 用尽 → log error; **不阻塞业务** |
| buffer 满 (>1000 events 积压) | drop + `Dropped()` 计数 + zap warn |
| service shutdown | `Stop()` 阻塞 drain buffer 后返 |

## Emit 永不阻塞 > 1ms

```
Emit(e) → buffered chan ──→ background flusher ──→ HTTP POST
                                                       │
                                                       ↓ retry
                                                       ↓ fail
                                                       └→ zap.Error (audit-log 自己监控)
```

业务调用方不需要 try/catch — `Emit` 总返 nil 或 "buffer full".

## 缺省值

- BatchSize: 50
- FlushInterval: 1s
- BufferSize: 1000
- MaxRetries: 5
- HTTPTimeout: 5s

dev 环境压根没接 audit-log, BaseURL 留空时 client 仍 work — 所有 events 都会 retry 5 次失败 → zap 兜底. 这是预期行为.
