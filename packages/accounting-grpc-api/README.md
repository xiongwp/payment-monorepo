# ⚠️ DEPRECATED — accounting-grpc-api

**This repository is deprecated as of April 2026.**

The platform is migrating all RPC interfaces from Protobuf/gRPC to **Thrift + Kitex**.
The canonical accounting IDL now lives in [`accounting-system/idl/accounting.thrift`](https://github.com/xiongwp/accounting-system/blob/main/idl/accounting.thrift).

## What to do

- **Consumers**: migrate off `github.com/xiongwp/accounting-grpc-api/gen/accounting/v1`
  imports. Generate a Kitex Thrift client from `accounting-system/idl/accounting.thrift`
  instead:

  ```bash
  kitex -module <your-module> -I ../accounting-system ../accounting-system/idl/accounting.thrift
  ```

  Then import from the generated `kitex_gen/accounting` package.

- **accounting-system**: the server is being migrated from `grpc-go/protobuf` to
  Kitex/Thrift in a separate PR. Until that merges, this repo's `gen/` stubs are
  still functional for existing callers.

- **This repo**: will be archived once all callers have migrated. Do not add
  new RPCs or fields — they will not carry over.

## Rationale

The Thrift IDL in `accounting-system` is significantly richer (9 account types,
dynamic `accountBusinessType` registry, TCC maintenance, trial balance, hybrid
booking) and Kitex offers better extensibility for our platform's growth.

See the tracking PR in `accounting-system` for the full migration plan.

---

## Historical content (for existing consumers still using this repo)

The below documentation applies to the deprecated gRPC interface and will remain
available for reference until this repo is archived.

---

# Accounting gRPC API (deprecated)

Accounting System的gRPC接口层，提供标准化的gRPC API和客户端SDK。

## 功能特性

- ✅ **完整的gRPC接口**：账户管理、记账、查询、日切、调账等
- ✅ **多种记账模式**：同步、异步、批量
- ✅ **Protocol Buffers**：标准化的接口定义
- ✅ **gRPC反射**：支持grpcurl等工具调试
- ✅ **客户端SDK**：自动生成的Go客户端
- ✅ **依赖注入**：使用Uber Fx管理依赖

## 项目结构

```
accounting-grpc-api/
├── proto/                  # Proto文件定义
│   └── accounting.proto
├── gen/                    # 生成的代码
│   └── accounting/v1/
├── cmd/server/             # 服务器入口
│   └── main.go
├── internal/handler/       # gRPC Handler
│   └── accounting_handler.go
├── config/                 # 配置文件
├── Makefile               # 构建脚本
└── README.md              # 文档
```

## 相关链接

- 核心服务 + 新 Thrift IDL: [accounting-system](https://github.com/xiongwp/accounting-system)
- 管理后台: [accounting-admin-web](https://github.com/xiongwp/accounting-admin-web)

## License

MIT
