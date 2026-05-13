# API Versioning Policy

## 公网 API: URL versioning

```
https://api.payment.example.com/v1/charges
https://api.payment.example.com/v2/charges   # 重大改动后
```

- 每个主版本独立路由 + handler;不混用
- `v1` 一直可用,至少在 `v2` 上线后再支持 **12 个月**
- 弃用 `v1` 时:
  1. 发邮件给所有 `v1` 调用商户 (前置 6 个月)
  2. HTTP `Deprecation: true` + `Sunset: <date>` header
  3. 文档 `v1` 标 `[Deprecated]`
  4. Sunset 前 1 周开始返 410 Gone (warmup,可回滚)
  5. Sunset 当日,`v1` route 关闭

## 内部 gRPC: proto versioning

- proto 包名带版本: `cardpayment.v1`, `accounting.v1`
- 加字段 (backward-compatible) → 不改版本,但 generated stub 重发
- 删字段 / 改语义 → `v2` 新 package,server 同时挂 `v1` 和 `v2` 一段时间
- 内部 client 升级到 `v2` 后,grep `.v1` 不到剩余调用,删 `v1` server

## 不允许的破坏 (any version)

- 改 enum 已存在值的语义
- 把 `optional` 字段改成 `required` (proto2) / 改默认值 (proto3)
- 删除 RPC method,即便没人调 (打 deprecated tag 后再删)
- 改 idempotency-key / signature 含义

## 版本测试

- contract tests (Pact / Hoverfly) 锁住 v1/v2 表现
- e2e 路由表保证 `/v1/*` 永远命中老 handler

## 发布

- 新版本 → ADR (e.g. `docs/adr/00NN-introduce-v2-charges.md`)
- changelog: `CHANGES.md` 标 `### v2 (YYYY-MM-DD)` 起一段
