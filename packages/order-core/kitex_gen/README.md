# kitex_gen — order-core Kitex stubs (multi-service)

order-core 暴露 **8 个 RPC service** 在同一个端口 (跟 accounting-system 同款模式):

| Service | Proto | Used by |
|---------|-------|---------|
| PaymentIntentService | order.proto | payment-admin-web |
| ChargeService | order.proto | payment-admin-web |
| RefundService | order.proto | payment-admin-web, refund-engine |
| WebhookService | order.proto | payment-channel (ForwardWebhook 路径) |
| AuditService | audit.proto | payment-admin-web |
| WebhookDeliveryService | webhook_delivery.proto | payment-admin-web |
| LedgerService | ledger.proto | payment-admin-web |
| DisputeService | dispute.proto | payment-admin-web |

Generate:

```bash
./idl/generate.sh order
```

会出 8 个 `<svc>service/` 目录, 每个含 server + client.

## 多 service 注册 (Kitex 0.10+ MultiService)

```go
import (
    paymentintentservice "reconcile-system/packages/order-core/kitex_gen/order/v1/paymentintentservice"
    chargeservice        "reconcile-system/packages/order-core/kitex_gen/order/v1/chargeservice"
    refundservice        "reconcile-system/packages/order-core/kitex_gen/order/v1/refundservice"
    webhookservice       "reconcile-system/packages/order-core/kitex_gen/order/v1/webhookservice"
    auditservice         "reconcile-system/packages/order-core/kitex_gen/order/v1/auditservice"
    webhookdeliveryservice "reconcile-system/packages/order-core/kitex_gen/order/v1/webhookdeliveryservice"
    ledgerservice        "reconcile-system/packages/order-core/kitex_gen/order/v1/ledgerservice"
    disputeservice       "reconcile-system/packages/order-core/kitex_gen/order/v1/disputeservice"
    "github.com/cloudwego/kitex/server"
)

srv := server.NewServer(server.WithServiceAddr(addr))
paymentintentservice.RegisterService(srv, s)
chargeservice.RegisterService(srv, NewChargeForwarder(s))
refundservice.RegisterService(srv, NewRefundForwarder(s))
webhookservice.RegisterService(srv, NewWebhookForwarder(s))
auditservice.RegisterService(srv, NewAuditServer(s.auditRepo))
webhookdeliveryservice.RegisterService(srv, ...)
ledgerservice.RegisterService(srv, NewLedgerServer(s.ledgerSvc))
disputeservice.RegisterService(srv, NewDisputeServer(s.disputeSvc))
srv.Run()
```

详见 Kitex 0.10 文档 "Multi-Service" 章节. 当前 `internal/server/grpc.go` 已切到这种模式
(skeleton 留, 真生成 kitex_gen 后即可编译).

## Callers

- `payment-core/internal/orderclient/` (待写) — 调 WebhookService.ForwardWebhook
- `payment-channel/...` — 调 RefundService.GetRefund (退款 outbox 兜底)
- `refund-engine/.../` — 调 RefundService
- `payment-admin-web/backend/internal/clients/clients.go` — 8 个 Client 字段全切
