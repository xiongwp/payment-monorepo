# user-merchant-core

PSP 侧 **商户 (merchant) + 用户** 核心域服务。承接 merchant onboarding、
KYC 状态机、API key 签发/轮转、出站 webhook 配置，以及 **商户渠道凭据
(merchant channel secret)** 的密文存储 —— 密文经 `kms-manage` 信封加密后
落库，明文只在 admin 首次 Put 时与 payment-channel 在 adapter init 时由内部
RPC 取回一次。

## 目录结构

```
├── api/proto/usermerchant/v1/   # gRPC proto + 生成的 pb.go
├── cmd/server/main.go           # fx DI 入口
├── config/config.yaml           # 数据库 / KMS / server 配置
├── database/metadb/init/        # meta 库建表 SQL
├── internal/
│   ├── domain/                  # Merchant / KYC / ChannelSecret 等实体
│   ├── repo/                    # GORM 仓储 + DB Manager
│   ├── service/                 # 业务编排 (merchant / merchant_secret)
│   ├── server/                  # gRPC 适配层 + interceptor
│   ├── idgen/                   # Leaf Segment ID 生成器
│   ├── kmsclient/               # kms-manage gRPC 客户端
│   ├── metrics/                 # Prometheus 指标
│   └── trace/                   # trace_id 传播
├── Dockerfile / docker-compose.yml
├── Makefile
├── go.mod / go.sum
```

## 本地开发

```shell
make install-tools   # 一次性：装 protoc-gen-go / protoc-gen-go-grpc
make proto           # 重新生成 pb.go（修改 .proto 之后）
make build           # 编译 bin/user-merchant-core
make run             # 本地运行（默认读 ./config/config.yaml）
```

## 联栈

单服务：

```shell
cd ..
docker compose -f user-merchant-core/docker-compose.yml up --build
```

7 个工程一键起（kms-manage / risk-manage / order-core / user-merchant-core /
payment-channel / payment-core / payment-admin-web，加上 payment-channel 需要的
shared-meta + shared-shard-0..9 MySQL 集群，全部挂 `payment-stack` 网络）：

```shell
# 把脚本放到 sibling 根目录
cp scripts/start-all.sh ../
cp scripts/stop-all.sh  ../
cd ..
./start-all.sh         # 起全部
./start-all.sh status  # 看状态
./start-all.sh logs    # tail 全部日志
./stop-all.sh          # 停全部（保留数据卷）
./stop-all.sh --wipe   # 停 + 清数据卷
```

脚本也可直接从 `user-merchant-core/scripts/` 运行（它会自动识别 sibling 根目录）。
