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
