# Kitex 迁移 — 推进记录 + 剩余 checklist

## 决策

- **mTLS**: 内部 service mesh **不走 TLS** (用户 2026-05 决策).
  原 `payment-util/mtls.LoadFromEnv` 调用 / `MTLS_*` env / `client.WithTLSConfig`
  全部从迁移清单去除. 边缘 ingress (k8s gateway) 仍是 TLS 终端,
  内部 RPC 都明文跑 + service mesh 网络隔离保证.
- **wire 协议**: Kitex 默认 TTHeader + Protobuf
- **IDL**: Protobuf (复用现有 `packages/<svc>/api/proto/**`)

## 已完成 ✓

| Service | Server | Callers |
|---------|:------:|---------|
| **id-generator** | ✓ | (无显式 client wrapper) |
| **kms-manage** | ✓ | card-center ✓ / payment-core ✓ / user-merchant-core ✓ / payment-admin-web ✓ |
| **risk-manage** | ✓ | payment-core ✓ / payment-admin-web ✓ |
| **accounting-system** (proto only) | ⚠ 部分 | 0/5 — 见下 |

共享基础设施 ✓:
- `payment-util/kitexutil/` — EtcdResolver + Auth/Log/Metrics/Recover/CB middleware
- `idl/<svc>/v1/<svc>.proto` — 跟原 `packages/<svc>/api/proto/<svc>/v1/<svc>.proto` 内容一致, `go_package` 改成 `reconcile-system/packages/<svc>/kitex_gen/...`
- `idl/generate.sh` — 一键 `kitex` 生成

## kms-manage 剩余调用方 (3 个)

每个 caller 改造步骤跟 card-center 同款 (参考 `packages/card-center/internal/kmsclient/client.go`):

```diff
 // 老 grpc imports
-import (
-    "google.golang.org/grpc"
-    "google.golang.org/grpc/credentials"
-    "google.golang.org/grpc/metadata"
-    kmsv1 "github.com/xiongwp/kms-manage/api/proto/kms/v1"
-    "github.com/xiongwp/payment-util/serviceregistry"
-)
+import (
+    "github.com/cloudwego/kitex/client"
+    "github.com/xiongwp/payment-util/kitexutil"
+    kmsv1 "reconcile-system/packages/kms-manage/kitex_gen/kms/v1"
+    kmsservice "reconcile-system/packages/kms-manage/kitex_gen/kms/v1/kmsservice"
+)
```

Client 构造:

```diff
-conn, err := serviceregistry.DialWithFallback(endpoints, "kms-manage", fallback, opts...)
-api := kmsv1.NewKMSServiceClient(conn)
+api, err := kmsservice.NewClient("kms-manage", client.WithHostPorts(fallback))
```

Bearer token 透传:

```diff
-cctx = metadata.AppendToOutgoingContext(cctx, "authorization", "Bearer "+c.bearer)
+cctx = kitexutil.WithAdminToken(cctx, c.bearer)
```

具体文件:
- [ ] `payment-core/internal/kmsclient/client.go`
- [ ] `user-merchant-core/internal/kmsclient/kmsclient.go`
- [ ] `payment-admin-web/backend/internal/clients/clients.go` + `internal/handler/{dashboard,kms,kms_test}.go`

每个 caller 改完, go.mod 加 `github.com/cloudwego/kitex v0.10.0` (脚本同 KX-UTIL/KX-1/KX-2 用过的 python3 inject 一行即可).

## 剩 11 个 leaf 推进顺序 (按 idl/README.md)

1. **risk-manage** — split-payment + payment-core 调; 同步切 2 个 client
2. **accounting-system** — split-payment / order-core / payment-core / payment-channel 调; **fanout 最大**, 4 个 client 同切
3. **config-center** — 几乎所有服务调; 上线时全停滚动
4. **user-merchant-core** — order-core / payment-core / card-center 调
5. **order-core** — payment-core / payment-channel / refund-engine 调
6. **payment-core** — payment-channel / api-gateway / order-core 调
7. **payment-channel** — order-core / payment-core 调
8. **split-payment** — accounting-system / payment-admin-web 调
9. **card-center** — card-payment 调
10. **card-payment** — api-gateway 调
11. **accounting-grpc-api** — REST gateway, 内部走 accounting-system Kitex client (跟 #2 同步切)

## 通用步骤 (per service)

1. `cp packages/<svc>/api/proto/<svc>/v1/<svc>.proto idl/<svc>/v1/<svc>.proto`, 改 `go_package` 到 `reconcile-system/packages/<svc>/kitex_gen/<svc>/v1`
2. `./idl/generate.sh <svc>` — 生成 kitex_gen
3. 写 `packages/<svc>/kitex_gen/README.md` 占位
4. 改 `packages/<svc>/cmd/server/main.go`:
   - 删 `google.golang.org/grpc` import
   - 加 `github.com/cloudwego/kitex/server` + kitex_gen import
   - server 构造: `<svc>service.NewServer(impl, server.WithServiceAddr(addr))`
   - lifecycle Run/Stop 换 Kitex API
5. `go.mod` 加 `github.com/cloudwego/kitex v0.10.0`, 删 `google.golang.org/grpc`
6. 同步改所有 caller 的 `internal/<svc>client/client.go` (跟 card-center kmsclient 同款 diff)
7. 验证 `go build ./...` (跨服务跑 unit test)

## accounting-system 卡点 (KX-4)

accounting-system 比预期复杂:
- proto 里只声明 1 个 service (`AccountingService`)
- 但 `internal/grpc/server.go` 注册 4 个 service:
  `AccountingService` / `AccountingAdminService` / `FreezeService` / `TransactionService`
- 后 3 个 service 是手写 grpc service (admin_extensions.go / freeze_extensions.go / transaction_service.go), 没有 .proto
- Kitex 是 IDL-driven, 必须先给这 3 个 service 写 .proto, 然后 MultiService 模式注册

**KX-4 当前状态**: idl/accounting/v1/accounting.proto 已就绪 (只含 AccountingService),
kitex_gen/README.md 已落档 plan. 后续需要把 3 个手写 service 反向写成 proto 再继续.

## payment-util/serviceregistry 退役 plan

**当前状态**: 10 处生产代码仍调 `serviceregistry.DialWithFallback / DialFromEndpoints / DialDirect`,
返回 `*grpc.ClientConn` 给 downstream gRPC client wrapper (gRPC `XxxServiceClient`) 用. 这些
consumer 没切 Kitex, 所以 serviceregistry 暂时不能删.

### 已切 Kitex 的 dial site (1/10)

- ✅ `split-payment/cmd/server/providers.go` — `newAccountingGRPCClientFx` 现走 `transactionservice.NewClient`,
  `*grpc.ClientConn` Provider 删了. `clients.NewAccountingGRPCClient(endpoint)` 内部 Kitex.

### 未切 (9 处, 需要 consumer 同步切才能删 serviceregistry)

| 文件 | 调谁 | Consumer 阻塞 |
|------|------|---------|
| `api-gateway/cmd/server/main.go:375` | user-merchant-core | `userweb.Handler` 内部 `usermerchantv1.NewUserServiceClient(conn)` 用 gRPC |
| `api-gateway/cmd/server/main.go:421` | order-core | `cardweb.CardHandler` 内 `orderv1.NewPaymentIntentServiceClient(conn)` 用 gRPC |
| `card-center/cmd/server/main.go:437` | user-merchant-core | downstream merchant cache 用 gRPC client |
| `accounting-admin-web/backend/cmd/server/main.go:37` | accounting-system | admin BFF 全 gRPC client |
| `user-merchant-core/cmd/server/main.go:714` | risk-manage | user 注册路径 risk screen, gRPC client |
| `user-merchant-core/cmd/server/main.go:839` | accounting-service | user 注册路径建账, gRPC client |
| `payment-admin-web/backend/cmd/server/main.go:402` | 通用 service | admin BFF 17 gRPC clients (已在 clients.go 切 Kitex; main.go dial 不再用) |
| `order-core/internal/accounting/client.go:121` | accounting-service | order-core 自己的 accounting wrapper |
| `accounting-system/cmd/batchtask/main.go:176` | accounting-service | batch task CLI |
| `config-center/internal/server/admin_html.go:127` | config endpoint | admin UI |
| `order-core/cmd/e2e-accounting/main.go:102` | order-core | e2e test entry |

每条都需要 consumer-side 改造 (大约 5-30 行/处) 才能切; 干完 10 处 serviceregistry 包就可删整个 package.

## 风险点 (mTLS 已 ✗ 不需要)

- Kitex 默认 TTHeader+Protobuf, **跟 gRPC wire 不互通**: server + client 必须同时切
- ~~mTLS~~ 用户决策不要, 内部 mesh 明文跑
- 老 `payment-util/shadow.UnaryServerInterceptor` (gRPC) 需要 port `kitexutil.ShadowMW` (从 metainfo 取 shadow header)
- 老 `payment-util/trace.UnaryServerInterceptor` (gRPC) 需要 port `kitexutil.TraceMW`
- rate_limit / SAN whitelist (kms-manage): TODO 留 kitexutil 接好后展开
- 服务发现: `kitexutil.EtcdResolver` stub Instance impl 待跟 Kitex `discovery.Instance` 对齐
