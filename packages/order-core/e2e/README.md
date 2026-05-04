# Three-tier End-to-End Tests

本目录下的测试跨越三个仓库：

```
order-core  →  payment-core  →  payment-channel  →  fake 外部渠道
 (client)       (routing)        (adapter)           (mock in Go)
```

外部渠道（GCash Alipay+ API、Maya Checkout API、银行 API 等）完全 mock 在
`scriptedPHChannel` 里，不发真实 HTTP。

## 构建标签

所有文件都带 `//go:build e2e`，正常 `go build ./...` 不会参与构建，因此不会给
order-core 强行拉进 payment-core / payment-channel 两个模块作为依赖。

## 前置条件

把三个仓库克隆在 `/home/user/` 下同级：

```
/home/user/
├── order-core/
├── payment-core/
└── payment-channel/
```

然后在 `/home/user/go.work`（已存在于本机）里：

```go
go 1.24

use (
    ./order-core
    ./payment-core
    ./payment-channel
)
```

## 跑法

```bash
cd /home/user
go test -tags=e2e ./order-core/e2e/...
```

## 覆盖场景

| 测试 | 链路验证 |
|---|---|
| `TestThreeTier_GCash_EndToEnd` | order-core 发 GCash 请求 → payment-core 路由到 gcash → payment-channel 调 scripted adapter → `RequiredAction{app_redirect}` 从 scripted adapter 一路透传回 order-core |
| `TestThreeTier_Maya_RedirectPropagates` | 同上，验证 Maya 的 hosted checkout 重定向 URL 不丢失 |
| `TestThreeTier_InstaPay_Sync` | 银行转账同步成功路径：payment-channel 直接返回 `succeeded`，全链路零等待 |
| `TestThreeTier_Refund_WithRoutingHint` | Refund 的 `extra[adapter]` 透传验证，payment-core 必须显式拿到才路由 |
| `TestThreeTier_IdempotentReplay_NoDoubleCharge` | 同 `pi_id` 重复 Charge → payment-channel 的 `UNIQUE(adapter, idempotency_key)` 回放首次响应，不会二次调用 scripted adapter |

## 为什么需要 bufconn

`google.golang.org/grpc/test/bufconn` 让两段 gRPC 服务各跑在一个内存 listener
上，进程内点对点互连，不用占端口，测试毫秒级启停。

## 扩展

新加 PH 渠道时：

1. 在 `scriptedPHChannel.Charge` 里加一个 `case "<name>"` 分支，返回合适的
   scripted response；
2. 在 `spinUpStack` 里 `pcReg.Register(&scriptedPHChannel{name: "<name>"})`；
3. 在 `pccoreRouter` 的规则里加一条把对应 payment_method 路由到 `<name>`；
4. 写一个 `TestThreeTier_<Name>_...` 验收场景。
