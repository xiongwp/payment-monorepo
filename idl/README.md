# Kitex IDL — Monorepo 统一 IDL 目录

## 决策

- **IDL**: Protobuf (复用现有 `packages/*/api/proto/**` 内容, 整合到 `idl/`)
- **RPC 框架**: CloudWeGo Kitex (完全替换 `google.golang.org/grpc`)
- **生成代码**: `kitex_gen/` 出在每个服务 package 下 (跟服务代码共仓库)
- **跨服务调用**: 同一份 `idl/<svc>/v1/<svc>.proto` 作为 contract, server + client 都从这里生成

## 目录结构

```
idl/
├── README.md                          # 本文件
├── generate.sh                        # 一键生成脚本
├── accounting/v1/accounting.proto     # 从 packages/accounting-system/api/proto 整合
├── cardcenter/v1/cardcenter.proto
├── cardpayment/v1/cardpayment.proto
├── channel/v1/channel.proto
├── configcenter/v1/configcenter.proto
├── idgen/v1/idgen.proto
├── kms/v1/kms.proto
├── order/v1/order.proto
├── order/v1/audit.proto
├── order/v1/dispute.proto
├── order/v1/ledger.proto
├── order/v1/webhook_delivery.proto
├── paymentcore/v1/paymentcore.proto
├── risk/v1/risk.proto
├── splitpayment/v1/splitpayment.proto # NEW: 需要从 split-payment 现有 gRPC 抽出
└── usermerchant/v1/{user,merchant,user_card,merchant_secret}.proto
```

## 生成命令

```bash
# 安装一次性 (CI / 本地都需要):
go install github.com/cloudwego/kitex/tool/cmd/kitex@latest

# 生成 (在 idl/ 目录跑):
./generate.sh
```

或者按 service 增量生成:

```bash
cd packages/order-core
kitex -module reconcile-system/packages/order-core \
      -gen-path kitex_gen \
      -service order-service \
      ../../idl/order/v1/order.proto
```

## 迁移 checklist (每个服务)

1. **复制 .proto** — 把 `packages/<svc>/api/proto/<svc>/v1/<svc>.proto` 复制到 `idl/<svc>/v1/<svc>.proto`,
   `go_package` option 改成 `reconcile-system/packages/<svc>/kitex_gen/<svc>/v1`
2. **生成 Kitex stub** — `cd packages/<svc> && kitex -service <svc>-service ../../idl/<svc>/v1/<svc>.proto`
3. **改 cmd/server/main.go** — 把 `grpc.NewServer()` 换 `<svc>service.NewServer(impl)`;
   把 `grpc.Dial()` 换 `<svc>service.NewClient("dest", opts...)`
4. **删除老 .pb.go** — `packages/<svc>/api/proto/<svc>/v1/<svc>.pb.go` 全删
5. **删除老 grpc 导入** — `google.golang.org/grpc` 从 go.mod 移除
6. **跨服务调用方同步切** — 调 `<svc>` 的服务也得换 `<svc>service.NewClient`,
   `<svc>` 自己切完就跑不通了 (因为协议层不兼容 — Kitex 用 thrift / TTHeader 协议,
   即使 IDL 是 proto, wire format 跟 gRPC 不一定 100% 互通; 详见 Kitex 文档关于 PROTOBUF + TTHeader 模式).

## 推进顺序

按 leaf service 先切 (没被其它服务调用的), 减少 cascade 风险:

1. **id-generator** — 被 order-core/payment-core dial, leaf 端干净
2. **kms-manage** — 被 card-center / card-payment dial
3. **accounting-system** — 被 split-payment / order-core dial, **最大爆破面**
4. **risk-manage** — 被 split-payment / payment-core dial
5. **配套 client 同步切** — 上游服务把 grpc.NewClient → kitex client

## 风险点

- **wire 协议不兼容**: Kitex 默认 thrift+ttheader, 而 gRPC 是 protobuf+http2; 配置 Kitex
  用 grpc transport 才能互通 — 这需要 server / client 同时声明 `WithTransportProtocol(transport.GRPC)`
- **interceptor 不通用**: grpc.UnaryInterceptor 跟 Kitex 的 endpoint.Middleware 不一样, 现有
  payment-util/serviceregistry / mtls / shadow 等 helper 都要重写一份 Kitex 版
- **服务发现**: 当前 etcd-based serviceregistry 直接走 gRPC resolver API; Kitex 自带
  client.WithResolver, 需要写个 EtcdResolver 适配器
- **mTLS**: 当前 google.golang.org/grpc/credentials → Kitex 用 client.WithTransportProtocol + 自带 tls.Config

## 待办

- [ ] 把 12 份散落的 .proto 收到 idl/ (统一 go_package)
- [ ] 写 generate.sh (循环调 kitex)
- [ ] 写 payment-util/kitexserviceregistry (EtcdResolver + LoadBalancer)
- [ ] 写 payment-util/kitexmiddleware (替换 grpc.Interceptor: auth/log/metrics/circuitbreaker)
- [ ] 按 leaf 顺序逐个切换 + 验证
