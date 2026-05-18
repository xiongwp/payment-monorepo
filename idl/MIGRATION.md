# Kitex 迁移 — 推进记录 + 剩余 checklist

## 已完成 ✓

| Service | 角色 | Server | Client 切换方 |
|---------|------|:------:|---------------|
| **id-generator** | leaf | ✓ | (订单 / 支付内部调, 暂无显式 client wrapper 文件) |
| **kms-manage** | leaf | ✓ | card-center ✓ / payment-core / user-merchant-core / payment-admin-web ⏳ |

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

## 风险点 (per idl/README.md "风险点")

- Kitex 默认 TTHeader+Protobuf, **跟 gRPC wire 不互通**: server + client 必须同时切; 否则一边 OK 一边 connection refused
- 老 `payment-util/shadow.UnaryServerInterceptor` 是 gRPC interceptor: 需要重写一份 `shadow.KitexMW` (从 metainfo 取 shadow header 注 ctx)
- mTLS: 老 `mtls.LoadFromEnv()` 给的是 `grpc.DialOption`, Kitex 用 `tls.Config` 直接装 `client.WithTLSConfig`; payment-util 需要加 `mtls.KitexClientConfig() *tls.Config`
- rate_limit / SAN whitelist (kms-manage): 当前 main.go 留 TODO; 真接 Kitex middleware 时把老 `RateLimitInterceptor` / `ClientIdentityInterceptor` port 一份到 kitexutil
- 服务发现: 当前 `kitexutil.EtcdResolver` 是 stub Instance impl, 真接 Kitex 时换成 `discovery.Instance` 接口 (kitex/pkg/discovery)
