# P0-3：Payment-Core 渠道降级路径设计

## 概述

payment-core 现已支持备用渠道自动降级与异步重试，当主渠道（adapter）熔断打开或不可用时，自动尝试备用渠道；若所有备用都失败，任务入队异步重试，前端返回"处理中"而非失败。

## 架构

```
Charge Request
    ↓
[风控 Screen] → [主路由] → adapter1
                     ↓
                [熔断检查]
                     ↓
              circuit_open?
                  ↙    ↘
                 Y       N → [Charge] → Result
                 ↓
          [尝试 Fallback]
             ↙  ↙  ↘
       adapter2 / adapter3 / ... all failed?
                 ↓
              Y → [Enqueue Retry]
                     ↓
                [Return: processing]
```

## 核心组件

### 1. FallbackRouter (`routing/fallback.go`)

**职责**：管理备用渠道链配置，支持 config-center 热更新

```go
// 从 config-center 读取配置（可热更新）
config-center key: "payment-core/routing.fallback"

// 查询备用链
chain := fallback.GetFallbackChain(
    "gcash",  // 主渠道
    FallbackInput{
        Country: "PH",
        PaymentMethod: "GCASH",
        Currency: "PHP",
    },
)
// 返回：["gcash", "paymongo", "xendit"]
```

**配置格式**（JSON，存储在 config-center）：
```json
{
  "rules": [
    {
      "country": "PH",
      "payment_method": "GCASH",
      "bin": "",
      "currency": "PHP",
      "priority": [
        {"adapter": "gcash", "weight": 100},
        {"adapter": "paymongo", "weight": 50},
        {"adapter": "xendit", "weight": 25}
      ]
    }
  ]
}
```

### 2. RetryQueue & RetryWorker (`routing/retry_queue.go`)

**职责**：异步重试队列管理和后台 worker

- **MemoryRetryQueue**：内存实现（开发/测试）
- **DBRetryQueue**：数据库实现占位符（生产用）
  - 使用 outbox pattern（INSERT on charge fail, 独立 worker 轮询）
  - 表结构：`outbox_retry(id, pi_id, idempotency_key, adapter, attempt, next_retry_at, ...)`

**重试退避策略**：`[30s, 2m, 15m, 2h]`
- attempt 0 → 30s 后重试
- attempt 1 → 2m 后重试
- attempt 2 → 15m 后重试
- attempt 3+ → 2h 后重试

### 3. PaymentService 集成

在 `Charge()` 方法中的熔断检查阶段：

```go
if !isProbe && !cb.Allow() {
    // 尝试 fallback
    adapter, err = s.tryFallbackAdapter(ctx, adapter, req)
    if err != nil {
        // 全部失败 → 写 outbox，返回 processing
        s.enqueueRetry(ctx, req, adapter, "circuit_open")
        return &PaymentResponse{
            ResultType: "processing",
            FailureCode: "processing",
        }, nil
    }
    // fallback 成功 → 继续用新 adapter 路由
}
```

## 指标

### payment_routing_fallback_total
- **标签**：`from_state` (circuit_open/unavailable), `to_adapter`
- **含义**：备用渠道启用次数
- **例**：`from_state="circuit_open", to_adapter="paymongo"` 表示 gcash 熔断打开，成功切到 paymongo

### payment_routing_retry_enqueued_total
- **标签**：`reason` (circuit_open/unavailable)
- **含义**：任务入队异步重试的次数

## Config Center 配置

### 初始化（seed）
```bash
config-center-cli seed --namespace=payment-core \
  --file=packages/payment-core/config/configcenter_seed.yaml
```

文件位置：`packages/payment-core/config/configcenter_seed.yaml`

关键 key：
- **`payment-core/routing.fallback`**（JSON）：备用渠道链规则
- **`payment-core/routing.retry.enabled`**（plain）："true" 启用重试
- **`payment-core/routing.retry.backoffs`**（JSON）：退避策略（ms）

### 热更新

Admin 端点支持在线更新（无需重启）：
```bash
curl -X POST http://localhost:8080/admin/routing/reload
```

## 使用流程

### 1. 配置 Config Center

```yaml
# configcenter_seed.yaml
configs:
  - namespace: payment-core
    key: routing.fallback
    format: json
    value: |
      {
        "rules": [
          {
            "country": "PH",
            "payment_method": "GCASH",
            "priority": [
              {"adapter": "gcash", "weight": 100},
              {"adapter": "paymongo", "weight": 50}
            ]
          }
        ]
      }
```

### 2. 初始化 PaymentService

```go
import "github.com/xiongwp/payment-core/internal/routing"

// 创建 FallbackRouter（会自动绑定 config-center）
fallbackRouter := routing.NewFallbackRouter(logger)
configClient.Bind("payment-core/routing.fallback", 
    &someAtomic, 
    fallbackRouter.UpdateConfig)

// 创建 RetryQueue
retryQueue := routing.NewMemoryRetryQueue()  // 开发用
// 或：retryQueue := &routing.DBRetryQueue{...}  // 生产用

// 创建 PaymentService
svc := service.NewPaymentService(router, client, risk, logger)
svc.retryQueue = retryQueue

// 启动 RetryWorker
worker := routing.NewRetryWorker(retryQueue, router, fallbackRouter, logger)
worker.Start(ctx)
defer worker.Stop()
```

### 3. 测试

当某渠道熔断（circuit open）：
```bash
# 查看熔断状态
curl http://localhost:8080/ops/circuit/states

# 触发一笔 charge
grpcurl -d '{"pi_id":"test_001",...}' \
  payment-core.local:9700 \
  payment_channel.v1.PaymentCore/Charge

# 返回应该是：
# {
#   "result": "processing",
#   "failure_code": "processing",
#   "failure_message": "primary adapter unavailable, enqueued for async retry"
# }

# 查询 retry 队列统计（如果 MemoryRetryQueue）
curl http://localhost:8080/admin/retry/stats
```

## 幂等性保证

所有 charge 使用统一的 idempotency_key（基于 payment_intent_id + amount + currency），保证：
1. 重试时即使换了不同 adapter，payment-channel 侧通过 UNIQUE(idempotency_key) 防重复扣款
2. 多笔重试 dequeue 不会造成重复处理（DB 乐观锁/版本控制）

## 后续优化（TODO）

1. **数据库持久化**：实现 DBRetryQueue，使用 outbox pattern
2. **动态权重**：根据 adapter 成功率动态调整 fallback 链权重
3. **限流器集成**：retry worker 加限流，避免重试风暴
4. **dead letter queue**：多次重试失败后转 DLQ，人工审查
5. **通知机制**：重试成功/失败时通知 order-core
6. **监控告警**：retry 队列堆积、失败率高时告警

## 测试

```bash
cd packages/payment-core
go test ./internal/routing -v

# 测试用例覆盖：
# - FallbackRouter 配置更新和链查询
# - RetryScheduler 退避计算
# - MemoryRetryQueue 入队/出队/标记
# - 集成：熔断触发 → fallback → retry enqueue
```

## 参考

- Circuit Breaker：`internal/circuitbreaker/`
- 主路由：`internal/routing/router.go`
- 指标：`internal/metrics/`
- Config Center SDK：`packages/payment-util/configcenter/`
