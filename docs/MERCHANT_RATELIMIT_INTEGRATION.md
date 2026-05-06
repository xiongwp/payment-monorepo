# Merchant 维度限流集成指南

## 概述

本文档说明如何在 api-gateway / order-core / payment-core 中集成 merchant 维度限流，防止单个行为异常的 merchant（卡测试、promo 抓、fraud burst）打爆全平台。

## 架构

```
Request → Auth（提取 merchant_id）
         ↓
         IP 限流（兜底）
         ↓
         Merchant 限流（新增，P0-4）
         ↓
         业务逻辑
```

## 限流配置

### 配置结构（JSON）

```json
{
  "default": {
    "rps": 100,
    "burst": 200
  },
  "merchants": {
    "M_premium_001": { "rps": 1000, "burst": 2000 },
    "M_test_999":    { "rps": 5,    "burst": 10 }
  }
}
```

- **default**：所有未在 merchants 中特别配置的 merchant 使用此限流
- **merchants**：特定 merchant 的自定义限流。可为 premium 设更高额度、为测试号设更低额度

### Config-Center 部署

配置 key：`api-gateway/rate_limit.per_merchant`（或各服务的对应 key）

在 config-center 中创建此 key，value 为上述 JSON。启动时从 config-center 读取，支持热更新。

**Seed 文件**：`packages/config-center/seed/merchant_ratelimit.json`

## 代码集成

### Order-Core 示例

```go
import (
    "github.com/xiongwp/payment-util/ratelimit"
    "github.com/xiongwp/order-core/internal/metrics"
)

// 启动时
func setupServer(cfg *Config) (*grpc.Server, error) {
    // 1. 初始化 merchant 限流器
    merchantLimiter := ratelimit.NewMerchantLimiter(
        &cfg.MerchantRateLimit,
        logger,
        func(merchantID string) {
            metrics.MerchantRateLimitThrottled.WithLabelValues(merchantID).Inc()
        },
    )
    
    // 2. 监听 config-center 变更，热更新
    configCenter.Watch("payment/rate_limit.per_merchant", func(val string) {
        var newCfg ratelimit.MerchantConfig
        if err := json.Unmarshal([]byte(val), &newCfg); err != nil {
            logger.Error("failed to parse merchant ratelimit config", zap.Error(err))
            return
        }
        merchantLimiter.UpdateConfig(&newCfg)
    })
    
    // 3. 注册 interceptor（在 auth 之后、业务 handler 之前）
    srv := grpc.NewServer(
        grpc.UnaryInterceptor(
            grpc.ChainUnaryInterceptor(
                LoggingInterceptor(logger),
                AuthInterceptor(validTokens, false, logger),
                MerchantRateLimitInterceptor(merchantLimiter, logger),  // ← 新增
                MetricsInterceptor(),
                RecoverInterceptor(logger),
            ),
        ),
    )
    
    return srv, nil
}
```

### API-Gateway HTTP Middleware 集成（可选，HTTP 端已有 IP 维度限流）

```go
// 如需在 HTTP 层也加 merchant 限流，类似 gRPC：
func setupHTTPServer(cfg *Config) (*http.Server, error) {
    merchantLimiter := ratelimit.NewMerchantLimiter(
        &cfg.MerchantRateLimit,
        logger,
        func(merchantID string) {
            metrics.MerchantRateLimitThrottled.WithLabelValues(merchantID).Inc()
        },
    )
    
    // ... 中间件链中添加
}
```

## 指标和告警

### Prometheus 指标

- `order_merchant_ratelimit_throttled_total{merchant_id="M_xxx"}`
- `paycore_merchant_ratelimit_throttled_total{merchant_id="M_xxx"}`

### 告警规则

部署文件：`deploy/monitoring/merchant_ratelimit_alerts.yaml`

- **WARNING**：单 merchant 5min 内限流 > 1000 次
- **CRITICAL**：单 merchant 5min 内限流 > 5000 次

告警触发后，ops 可：
1. 查看 Prometheus 时序数据，确认是否是 promo / 压测
2. 在 config-center 临时调高该 merchant 的限流额度，或
3. 主动联系该 merchant 排查异常流量

## 性能约束

- **延迟**：P99 < 100µs（使用 sync.RWMutex 读锁 + 无锁快路径）
- **内存**：支持 100K merchants × ~100 bytes = ~10MB（可配）
- **并发**：无全局锁竞争，每个 merchant 独立 token bucket

## 约束和注意

1. **取不到 merchant_id 时**：不限流，兜底为 IP 限流
2. **多实例部署**：每个实例独立的 token bucket（已知 trade-off）
   - v1 先单实例内限流，无需 Redis
   - 若来日需全局共享，可用 Redis Lua 脚本实现，但复杂度上升
3. **配置变更**：支持热更新（config-center watch），无需重启

## 故障排除

### merchant 限流生效了但数值好像不对

检查：
1. Metadata 是否正确设置 `x-merchant-id`
2. Config-center key 的 path 是否与代码一致
3. 限流器初始化时是否正确传入回调

### 指标看不到

1. Prometheus 是否已加载 alert rules
2. Metrics 是否已 Register（见各服务 main.go）
3. Prometheus scrape 任务是否正确配置

### 性能下降

- 监控 `order_db_pool_in_use_count` 和 `order_merchant_ratelimit_throttled_total` 的增长关联
- 若限流触发过多但 merchant RPS 配置过低，调整配置
