# api-gateway

公网入口网关。把 HTTP/REST 请求鉴权 / 限流 / 路由到内部的 gRPC 服务（order-core / user-merchant-core 等）。

## 定位

```
商户 SDK / Web / Mobile  ─HTTPS─→  api-gateway  ─gRPC──→  order-core / user-merchant-core / ...
                                       │
                                       └─ kms-manage（API key 解密）
```

## 职责

1. **鉴权**：API key / JWT / OAuth2 校验（kms-manage 透明解密 API key）
2. **限流**：per-merchant / per-IP rate limit（令牌桶）
3. **路由**：HTTP → gRPC stub；版本管理（`/v1/*`）
4. **熔断**：下游服务故障时 fast-fail
5. **审计**：请求日志（PII 脱敏）
6. **协议适配**：REST → gRPC、响应 JSON 序列化

## 设计原则

- **无状态**：Kubernetes 水平扩展
- **不做业务决策**：规则 / 路由只配置化，不写 switch-case 业务逻辑
- **延时预算**：自身加 < 5ms；下游超时透传

## 依赖约束

- 所有 downstream gRPC 调用透传 `x-trace-id` header
- 请求/响应 body 不落本地磁盘；日志只记 metadata（size / duration / code）
- mTLS 或至少 bearer token 认证到下游
