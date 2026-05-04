# risk-manage

风控决策服务。对每笔 payment 请求做规则 / 黑白名单 / 频控 / 账单卡校验，返回 allow / reject / review。

## 定位

```
payment-core  ─gRPC──→  risk-manage
                           │
                           ├─ shared-meta  （黑名单 / 规则 / 历史统计）
                           └─ 外部征信 / 反欺诈 API（可选）
```

## 核心决策

- **黑名单**：卡号 / 设备 / IP / 手机号。命中直接 reject
- **频控**：同 user_id / 卡 / IP 的高频支付尝试。Redis sliding window
- **金额规则**：大额 / 超过 user 日累计 → review
- **外部反欺诈**（可选）：Sift / MaxMind 查分数
- **Fail-close vs fail-open**：payment-core 配置，默认 **fail-open**（风控服务挂了不阻断交易），可改 `risk.fail_close=true` 切回

## 接口

gRPC（:9490）：
- `Screen(req) → Decision{Verdict: allow|reject|review, Reasons, Score}`
- `Report(outcome)` 反馈（给机器学习闭环）

payment-core 调用：
```go
decision, err := risk.Screen(ctx, riskclient.Request{
    UserID:     userID,
    Amount:     amount,
    Currency:   currency,
    CardBIN:    bin,
    DeviceID:   deviceID,
})
```

## 依赖约束

- `Screen` 超时 3s，超时视为 allow（fail-open）
- 决策延时要可观测（Prometheus histogram `risk_screen_duration_seconds`）
- 反向频控数据一定落 Redis，不要读到 MySQL（否则高并发爆库）
