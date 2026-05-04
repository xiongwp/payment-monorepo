# payment-core

支付路由层。对上抽象 `PaymentChannel`（gRPC），对下按规则选择 adapter 转发到 payment-channel，封装风控 / kms / 鉴权。

## 定位

```
order-core  ─gRPC──→  payment-core  ─gRPC──→  payment-channel  ─HTTP/SDK─→ 外部渠道
                          │
                          ├─ risk-manage  风控校验
                          └─ kms-manage   鉴权 token 解密 / webhook 签名
```

本服务**无状态**（没有持久化层）。所有路由规则走 yaml config + hot reload。

## 核心概念

### 路由规则（`routing.Rule`）
```yaml
- priority: 100
  country: PH
  payment_method: GCASH
  adapter: gcash
  amount_min: 0        # 0 = 无下限
  amount_max: 0        # 0 = 无上限
  merchant: ""         # 空 = 通配
```
匹配优先级：merchant 非空优先 → priority 数值小优先 → 具体度高优先。

### 热重载
- `GET /admin/routing/rules` 查当前生效规则集
- `POST /admin/routing/reload` 重读 viper 配置 + 原子 `ReplaceRules`
- ConfigMap 热挂载 + curl reload 即可加新渠道，不用重启服务

## 接口

- gRPC `PaymentCoreService`（:9090）：`Charge / Capture / Void / Refund / Query / ParseWebhook`
- 内部管理 HTTP（:9290）：`/admin/routing/*`
- Metrics HTTP（:9190）：`/metrics`

## 流程

### Charge
1. 风控检查（risk-manage）。失败 → 直接拒（fail-close 可配）
2. 路由决策：根据 `country / payment_method / amount / merchant` 选 adapter
3. 调 payment-channel 的 `Charge(adapter=xxx, ...)`
4. 同步结果返回 order-core（`succeeded` / `failed` / `requires_action`）
5. 熔断：per-adapter circuit breaker（`internal/circuitbreaker`），故障自动跳过

### Webhook 解析
- `ParseWebhook` 透传 adapter 字段（header `X-Adapter`），委托给下游具体 adapter 解析
- 对 order-core 返回规范化 `WebhookEvent{EventType, PaymentIntentID, ChargeID, ...}`

## 运维

```bash
# 查路由规则
curl -H "X-Admin-Token: $TOKEN" http://payment-core:9290/admin/routing/rules

# 改了 ConfigMap 后热重载
curl -X POST -H "X-Admin-Token: $TOKEN" http://payment-core:9290/admin/routing/reload

# Metrics
curl http://payment-core:9190/metrics | grep circuit_breaker
```

## 依赖约束

- yaml 路由规则里 `country / payment_method` 都大写（`PH` / `GCASH`），运行时自动 ToUpper 归一化
- Charge 请求必须带 `metadata.country`，否则多数规则命不中
- `rate_limit.rps` / `rate_limit.burst` 按 yaml 配；突破上限直接 `RESOURCE_EXHAUSTED`
