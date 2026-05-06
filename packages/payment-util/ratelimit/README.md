# Rate Limit Package

Per-key token bucket 限流库，用于 IP 维度和 merchant 维度的请求限流。

## Features

- **Per-Key Token Bucket**：每个 key（IP / merchant_id）独立管理限流
- **RWMutex 优化**：读路径走 RLock，无竞争；写路径（新 key）走 Lock
- **LRU 淘汰**：超过容量时随机淘汰旧 key，防止内存爆炸
- **热更新**：支持 config-center 推送配置变更，零停机更新限流参数
- **指标导出**：支持回调函数记录限流事件到 prometheus

## 使用

### IP 维度限流（API-Gateway HTTP）

```go
ipLimiter := ratelimit.NewKeyed(100, 200) // 100 rps, 200 burst

if !ipLimiter.Allow(clientIP) {
    return http.Error(429)
}
```

### Merchant 维度限流（Order-Core / Payment-Core gRPC）

```go
cfg := &ratelimit.MerchantConfig{
    Default: ratelimit.LimitSpec{RPS: 100, Burst: 200},
    Merchants: map[string]ratelimit.LimitSpec{
        "M_premium": {RPS: 1000, Burst: 2000},
    },
}

merchantLimiter := ratelimit.NewMerchantLimiter(
    cfg,
    logger,
    func(merchantID string) {
        metrics.MerchantRateLimitThrottled.WithLabelValues(merchantID).Inc()
    },
)

if !merchantLimiter.Allow(merchantID) {
    return status.Error(codes.ResourceExhausted, "limit exceeded")
}
```

### 热更新配置

```go
configCenter.Watch("rate_limit.per_merchant", func(val string) {
    var newCfg ratelimit.MerchantConfig
    json.Unmarshal([]byte(val), &newCfg)
    merchantLimiter.UpdateConfig(&newCfg)
})
```

## 性能

- **Allow 延迟**：P99 < 100µs（RWMutex 读锁路径）
- **内存**：支持 100K keys × ~100 bytes = ~10MB
- **并发**：无全局竞争，完全 non-blocking 快路径

## 文件说明

- `ratelimit.go`：基础 Keyed 限流器（底层抽象）
- `merchant.go`：Merchant 维度限流器（上层业务抽象）
- `merchant_test.go`：单元测试和性能测试
- `README.md`：本文件

## 已知约束

1. **多实例部署**：每个实例独立 token bucket，无需 Redis（trade-off）
2. **未配置 merchant_id**：不限流，兜底为 IP 限流
3. **容量保护**：超过 100K keys 时随机淘汰，避免内存泄漏
