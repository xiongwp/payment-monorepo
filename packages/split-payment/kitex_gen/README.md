# kitex_gen — split-payment Kitex stubs

split-payment 暴露 1 个 service: **AdminService** (6 RPCs).

历史: 老 `internal/grpcsvc/admin_service.go` 是**手写 grpc service code**, 没 .proto.
反向 .proto 已落 `idl/splitpayment/v1/splitpayment.proto` (跟手写 Go types 对齐).

Generate:
```bash
./idl/generate.sh splitpayment
```

Server (`cmd/server/main.go` 里的 `runAdminGRPCServer`):
```go
import (
    adminservice "reconcile-system/packages/split-payment/kitex_gen/split_payment/v1/adminservice"
    "github.com/cloudwego/kitex/server"
)
srv := adminservice.NewServer(impl, server.WithServiceAddr(addr))
```

Caller (`payment-admin-web/backend/internal/clients/clients.go`):
```go
import adminservice "reconcile-system/packages/split-payment/kitex_gen/split_payment/v1/adminservice"
cli, _ := adminservice.NewClient("split-payment", client.WithHostPorts(endpoint))
```

## 老手写 stubs 怎么处理

`internal/grpcsvc/admin_service.go` 里有:
- `AdminServiceServer` / `AdminServiceClient` interface
- `RegisterAdminServiceServer(s grpc.ServiceRegistrar, srv ...)`
- `NewAdminServiceClient(cc grpc.ClientConnInterface)`
- 6 个 message 类型 (ListGraphsRequest/Response/...)

Kitex `./idl/generate.sh splitpayment` 后会自动生成同名 message types 在 `kitex_gen/`.
两份并存会冲突, 切完 Kitex 后:

1. 让 `internal/grpcsvc/admin_handlers.go` 的 handler types implement Kitex `adminservice.Server` interface (签名一样, 不用改 handler body)
2. 把 `internal/grpcsvc/admin_service.go` 整个删掉 (生成的 kitex_gen 包含 message types + service interface)
3. `import grpcsvc → kitex_gen/.../adminservice`
