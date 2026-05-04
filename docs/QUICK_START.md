# 快速开始指南

## 前置准备

### 安装依赖
```bash
# macOS
brew install go mysql redis kafka

# Ubuntu
sudo apt-get install golang mysql-server redis-server
```

### 安装Docker
```bash
# 安装Docker和Docker Compose
curl -fsSL https://get.docker.com -o get-docker.sh
sh get-docker.sh
```

## 一、快速启动（推荐）

### 1. 克隆项目
```bash
cd /path/to/your/workspace
cp -r /Users/bytedance/Documents/accounting-system .
cd accounting-system
```

### 2. 安装开发工具
```bash
make install-tools
```

### 3. 启动Docker环境
```bash
# 启动所有依赖服务（MySQL、Redis、Kafka、etcd）
make docker-up

# 等待服务启动完成（约30秒）
```

### 4. 初始化数据库
```bash
# 创建数据库表结构
make init-db
```

### 5. 生成Kitex代码
```bash
make gen-kitex
```

### 6. 编译项目
```bash
make build
```

### 7. 运行服务
```bash
make run
```

✅ 服务启动成功！访问 `http://localhost:8888`

## 二、手动启动（开发环境）

### 1. 启动MySQL（3个实例）
```bash
# 启动MySQL容器
docker run -d --name mysql-0 -p 3306:3306 \
  -e MYSQL_ROOT_PASSWORD=password \
  -e MYSQL_DATABASE=accounting_db_0 \
  mysql:8.0

docker run -d --name mysql-1 -p 3307:3306 \
  -e MYSQL_ROOT_PASSWORD=password \
  -e MYSQL_DATABASE=accounting_db_1 \
  mysql:8.0

docker run -d --name mysql-2 -p 3308:3306 \
  -e MYSQL_ROOT_PASSWORD=password \
  -e MYSQL_DATABASE=accounting_db_2 \
  mysql:8.0
```

### 2. 启动Redis
```bash
docker run -d --name redis -p 6379:6379 redis:7-alpine
```

### 3. 启动Kafka
```bash
# 启动Zookeeper
docker run -d --name zookeeper -p 2181:2181 \
  -e ZOOKEEPER_CLIENT_PORT=2181 \
  confluentinc/cp-zookeeper:7.5.0

# 启动Kafka
docker run -d --name kafka -p 9092:9092 \
  -e KAFKA_BROKER_ID=1 \
  -e KAFKA_ZOOKEEPER_CONNECT=localhost:2181 \
  -e KAFKA_ADVERTISED_LISTENERS=PLAINTEXT://localhost:9092 \
  confluentinc/cp-kafka:7.5.0
```

### 4. 启动etcd
```bash
docker run -d --name etcd -p 2379:2379 -p 2380:2380 \
  -e ETCD_NAME=etcd0 \
  -e ETCD_ADVERTISE_CLIENT_URLS=http://localhost:2379 \
  quay.io/coreos/etcd:v3.5.10
```

### 5. 初始化数据库
```bash
# 导入表结构
for i in 0 1 2; do
  mysql -h127.0.0.1 -P330$i -uroot -ppassword accounting_db_$i < database/schema.sql
done
```

### 6. 修改配置文件
```bash
# 编辑 config/config.yaml
# 确保数据库连接信息正确
vim config/config.yaml
```

### 7. 运行服务
```bash
go run cmd/server/main.go
```

## 三、运行测试

### 单元测试
```bash
make test
```

### E2E测试
```bash
make e2e-test
```

### 查看覆盖率
```bash
make coverage
```

## 四、使用示例

### 示例1：创建账户
```go
package main

import (
    "context"
    "fmt"

    "github.com/accounting-system/internal/domain/model"
    "github.com/accounting-system/internal/service"
)

func main() {
    ctx := context.Background()

    // 创建用户账户
    account, err := accountingService.CreateAccount(
        ctx,
        100001,                      // 用户ID
        model.AccountTypeUser,       // 账户类型
        model.AccountCategoryAsset,  // 会计科目
        "PHP",                       // 币种
    )

    if err != nil {
        panic(err)
    }

    fmt.Printf("账户创建成功: %s\n", account.AccountNo)
}
```

### 示例2：用户充值
```go
// 用户充值100元
voucherNo, txIDs, err := accountingService.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
    BusinessNo:   "DEPOSIT_001",
    BusinessType: model.BusinessTypeDeposit,
    Entries: []service.AccountingEntry{
        {
            AccountNo:    "A1100001xxxx", // 用户账户
            DebitAmount:  decimal.NewFromInt(100),
            CreditAmount: decimal.Zero,
            Description:  "用户充值",
        },
        {
            AccountNo:    "PLATFORM_PROFIT_LOSS", // 平台账户
            DebitAmount:  decimal.Zero,
            CreditAmount: decimal.NewFromInt(100),
            Description:  "平台收款",
        },
    },
    Currency:    "PHP",
    Description: "用户充值100元",
})

fmt.Printf("充值成功! 凭证号: %s\n", voucherNo)
```

### 示例3：使用资金流引擎
```go
// 用户支付商户
response, err := flowEngine.ExecuteFlow(ctx, &service.FlowExecutionRequest{
    ProductCode:  "PAYMENT",
    SceneCode:    "PAY_MERCHANT",
    BusinessNo:   "ORDER_001",
    Amount:       decimal.NewFromInt(100),
    Participants: map[string]string{
        "user":     "A1100001xxxx",
        "merchant": "A2200001xxxx",
    },
    Currency: "PHP",
})

fmt.Printf("支付成功! 凭证号: %s\n", response.VoucherNo)
```

### 示例4：触发日切
```go
// 触发指定日期的日切
err := dayCutService.TriggerDayCut(ctx, "2024-01-01")
if err != nil {
    panic(err)
}

// 检查日切状态
status, _ := dayCutService.CheckDayCutStatus(ctx, "2024-01-01")
fmt.Printf("日切状态: %+v\n", status)
```

### 示例5：批量记账
```go
// 批量处理多笔记账
requests := []*service.DoubleEntryBookingRequest{
    {BusinessNo: "BIZ_001", ...},
    {BusinessNo: "BIZ_002", ...},
    {BusinessNo: "BIZ_003", ...},
}

// 并行处理
response, err := facadeService.BatchBooking(ctx, requests, true)
fmt.Printf("批量记账完成: 成功=%d, 失败=%d\n", response.Success, response.Failed)
```

## 五、常用命令

```bash
# 查看服务日志
docker-compose logs -f accounting-service

# 停止所有服务
make docker-down

# 重新构建
make build

# 代码格式化
make fmt

# 代码检查
make lint

# 清理构建产物
make clean

# 完整构建流程
make all
```

## 六、验证服务

### 1. 检查服务状态
```bash
# 检查Docker容器
docker ps

# 应该看到以下容器:
# - accounting-mysql-0
# - accounting-mysql-1
# - accounting-mysql-2
# - accounting-redis
# - accounting-kafka
# - accounting-zookeeper
# - accounting-etcd
# - accounting-service
```

### 2. 检查数据库
```bash
# 连接数据库
mysql -h127.0.0.1 -P3306 -uroot -ppassword accounting_db_0

# 查看表
SHOW TABLES;

# 应该看到:
# - account_00 ~ account_99
# - account_transaction_00 ~ account_transaction_99
# - account_balance_snapshot_00 ~ account_balance_snapshot_99
# - day_cut_control
# - async_task
```

### 3. 检查Redis
```bash
redis-cli ping
# 应该返回: PONG
```

### 4. 检查Kafka
```bash
# 查看topic
docker exec kafka kafka-topics --list --bootstrap-server localhost:9092

# 应该看到:
# - accounting-topic
# - snapshot-topic
# - day-cut-topic
```

## 七、故障排查

### 问题1: 端口被占用
```bash
# 查看端口占用
lsof -i :3306
lsof -i :6379
lsof -i :9092

# 停止占用端口的进程
kill -9 <PID>
```

### 问题2: Docker容器启动失败
```bash
# 查看容器日志
docker logs <container_name>

# 重启容器
docker restart <container_name>

# 删除并重新创建
docker rm -f <container_name>
make docker-up
```

### 问题3: 数据库连接失败
```bash
# 检查MySQL是否启动
docker ps | grep mysql

# 测试连接
mysql -h127.0.0.1 -P3306 -uroot -ppassword -e "SELECT 1"

# 查看配置文件
cat config/config.yaml
```

### 问题4: Kafka消息消费异常
```bash
# 查看消费者组
docker exec kafka kafka-consumer-groups --bootstrap-server localhost:9092 --list

# 查看消费者组详情
docker exec kafka kafka-consumer-groups --bootstrap-server localhost:9092 \
  --describe --group accounting-consumer-group
```

## 八、性能测试

### 基准测试
```bash
make benchmark
```

### 压力测试
```bash
# 使用hey工具
go install github.com/rakyll/hey@latest

# 测试记账接口
hey -n 10000 -c 100 -m POST \
  -H "Content-Type: application/json" \
  -d '{"business_no":"TEST001","amount":"100"}' \
  http://localhost:8888/api/v1/accounting/book
```

### 结果分析
```
Summary:
  Total:        10.5432 secs
  Slowest:      0.5234 secs
  Fastest:      0.0123 secs
  Average:      0.0987 secs
  Requests/sec: 948.5123

Status code distribution:
  [200] 10000 responses
```

## 九、生产部署

### 环境要求
- Go 1.21+
- MySQL 8.0+
- Redis 7+
- Kafka 3.5+
- 至少4核8G内存

### 部署步骤
```bash
# 1. 编译生产版本
CGO_ENABLED=0 GOOS=linux go build -o accounting-system ./cmd/server

# 2. 构建Docker镜像
docker build -t accounting-system:v1.0.0 .

# 3. 推送到镜像仓库
docker tag accounting-system:v1.0.0 registry.example.com/accounting-system:v1.0.0
docker push registry.example.com/accounting-system:v1.0.0

# 4. 使用Kubernetes部署
kubectl apply -f k8s/deployment.yaml
```

## 十、相关项目

本系统采用微服务架构，相关组件拆分为独立仓库：

### 1. 核心服务（当前仓库）
- **accounting-system**: 核心记账服务
- 提供：账户管理、复式记账、日切、调账等核心功能
- 技术栈：Go + Kitex + MySQL + Kafka

### 2. gRPC API服务（独立仓库）
- **accounting-grpc-api**: gRPC对外接口
- 提供：标准化的gRPC接口定义
- 包含：Proto文件、Client SDK、接口文档

### 3. Admin Web服务（独立仓库）
- **accounting-admin-web**: 管理后台
- 提供：账户查询、交易明细、调账审批、监控看板
- 技术栈：React + TypeScript + Ant Design

### 项目关系
```
accounting-system (核心服务)
    ↓ 被调用
accounting-grpc-api (接口层)
    ↓ 被调用
accounting-admin-web (管理后台)
```

## 十一、学习资源

### 文档
- [架构设计文档](./ARCHITECTURE.md)
- [API接口文档](./API.md) _(待创建)_
- [数据库设计文档](./DATABASE.md) _(待创建)_

### 示例代码
- [tests/e2e/](../tests/e2e/) - 端到端测试示例
- [internal/service/](../internal/service/) - 服务层实现

### 相关技术
- [Kitex文档](https://www.cloudwego.io/zh/docs/kitex/)
- [Uber Fx文档](https://uber-go.github.io/fx/)
- [Go最佳实践](https://github.com/uber-go/guide)

## 十二、获取帮助

### 问题反馈
- GitHub Issues: [提交Issue](https://github.com/your-repo/accounting-system/issues)
- 邮件: your-email@example.com

### 社区
- 技术讨论群: xxxxxx
- 文档贡献: 欢迎PR

---

**祝你使用愉快！** 🎉
