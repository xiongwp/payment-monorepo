# 现有服务接 OAuth2 — Migration Guide

把一个**已经在跑的服务**接入 OAuth2，分 3 步。以 refund-engine 为例。

## 概览

| Before | After |
|---|---|
| 自己手写 http.Server | `mw.Bootstrap()` |
| `X-API-Key` 校验 | `Authorization: Bearer <JWT>` |
| 各 handler 自己写鉴权 | `mw.RequireScope()` 守卫 |
| 没有 actor 概念 | `mw.ActorFromCtx(ctx)` |
| 服务间走内网 token | OAuth2 + mTLS 双重 |

## Step 1 — 改 cmd/server/main.go

### Before

```go
// packages/refund-engine/cmd/server/main.go (现状)
package main

import (
    "context"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"
    "go.uber.org/zap"
)

func main() {
    logger, _ := zap.NewProduction()
    port := envOr("REFUND_HTTP_PORT", "8080")

    repo := newMemoryRepo()
    svc := workflow.New(repo, stubChannel{}, notif, logger)

    mux := http.NewServeMux()
    mux.HandleFunc("/api/v1/refunds", refundsHandler(svc))
    mux.HandleFunc("/api/v1/refunds/", refundByIDHandler(svc))
    mux.HandleFunc("/healthz", healthHandler)

    srv := &http.Server{Addr: ":" + port, Handler: mux, ...}
    // 手写 graceful shutdown + signal handling ...
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()
    // ... 30 行 boilerplate
}
```

### After

```go
// packages/refund-engine/cmd/server/main.go (改造后)
package main

import (
    "context"
    "net/http"
    "go.uber.org/zap"
    mw "reconcile-system/packages/payment-mw"
)

func main() {
    mw.Bootstrap(mw.BootstrapConfig{
        ServiceName: "refund-engine",
        DefaultPort: "8080",
        AuthCfg: mw.AuthConfig{
            // /healthz, /metrics 默认就是 public，不用再列
        },
        SetupRoutes: func(mux *http.ServeMux, log *zap.Logger) error {
            repo := newMemoryRepo()
            svc := workflow.New(repo, stubChannel{}, notif, log)

            // 各 endpoint 配 scope 守卫
            mux.Handle("POST /api/v1/refunds",
                mw.RequireScope("refund:write")(refundsHandler(svc, "POST")))
            mux.Handle("GET /api/v1/refunds",
                mw.RequireScope("refund:read")(refundsHandler(svc, "GET")))
            mux.Handle("POST /api/v1/refunds/{id}/approve",
                mw.RequireScope("ops:write")(approveHandler(svc)))
            return nil
        },
        OnStart: func(ctx context.Context, log *zap.Logger) error {
            // 后台 cron 跑 submit 推 approved refund 到通道
            go runSubmitCron(ctx, svc, log)
            return nil
        },
    })
}
```

**代码量从 ~270 行 → ~50 行**，去掉了所有 boilerplate (signal handling, shutdown, healthz, metrics, recover, trace, request log, mTLS auto-detect)。

## Step 2 — 改 handler 用 Actor

### Before

```go
func createRefund(w http.ResponseWriter, r *http.Request) {
    // 自己从 header 解 merchant id (脆弱)
    merchantID := r.Header.Get("X-Merchant-ID")
    if merchantID == "" {
        http.Error(w, "unauthorized", 401)
        return
    }
    // 业务逻辑 ...
}
```

### After

```go
func createRefund(w http.ResponseWriter, r *http.Request) {
    actor := mw.ActorFromCtx(r.Context())

    // mw 已经验过签 + 检查 scope，这里直接用
    switch actor.Type {
    case "merchant":
        // 商户调用，限制只能退自己的 charge
        validateOwnership(actor.MerchantID, body.ChargeID)
    case "service":
        // 服务间调用，比如 payment-gateway 自动触发退款
        log.Info("internal refund", zap.String("by", actor.ServiceID))
    case "ops":
        // ops 后台手动退款
        log.Info("ops refund", zap.String("by", actor.OpsEmail))
    }

    // 业务逻辑 ...
}
```

## Step 3 — 加 env，重启

```bash
# Deployment env (k8s) 或 docker-compose
OAUTH2_JWKS_URL=http://oauth2-server:8087/.well-known/jwks.json
OAUTH2_ISSUER=http://oauth2-server:8087
OAUTH2_AUDIENCE=payment-api
```

完事。重启服务后:
- 老调用方带 `X-API-Key` 还能用（向后兼容）
- 新调用方带 `Authorization: Bearer <JWT>` 走 OAuth2 路径
- 灰度切换完成后删 X-API-Key 兼容代码

## 灰度滚动计划

```
Day 0:  oauth2-server 部署，给 5 个内部服务发 service client
Day 1:  payment-gateway 升级 (加 Bearer 验签，X-API-Key 仍兼容)
Day 2-3: 逐个升级 refund / dispute / kyc / billing / webhook / audit
Day 7:  商户 SDK 发布 v2 (内置 OAuth2)
Day 14: 通知商户 30 天内迁
Day 45: 关掉 X-API-Key 兼容路径
Day 46: 老 API-Key 全部失效
```

## Rollback

如果出问题，临时关掉 OAuth2 验签只需取消 env:
```bash
unset OAUTH2_JWKS_URL  # mw.Bootstrap 不会启 BearerJWT，自动 fallback 到 X-API-Key
```

## checklist

- [ ] go.mod 加 `reconcile-system/packages/payment-mw` (workspace 自动 resolve)
- [ ] cmd/server/main.go 改用 `mw.Bootstrap()`
- [ ] 各 handler 用 `mw.RequireScope()` + `mw.ActorFromCtx()`
- [ ] Deployment 加 3 个 env
- [ ] 在 oauth2-server 创建 service client (拿 client_id + secret)
- [ ] secret 注入 sealed-secret (k8s) 或 vault
- [ ] 监控加: `payment_mw_auth_total{outcome="oauth2_ok"}`, `oauth2_jwks_refresh_total`
- [ ] 烟雾测试: `examples/oauth2-integration/05_e2e_test.sh`
- [ ] OpenAPI spec 更新: `security: [{ oauth2BearerJWT: [<scope>] }]`
