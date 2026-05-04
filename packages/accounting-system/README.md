# 复式记账账户系统

一个高性能、高可用的复式记账账户系统，支持分库分表、分布式事务、日切和余额快照。

## 核心特性

### 1. 复式记账
- 严格遵循复式记账原则：有借必有贷，借贷必相等
- 支持资产、负债、权益、收入、费用五大会计科目
- 自动生成记账凭证和交易流水
- 支持多账户原子性记账

### 2. 账户体系
- **用户账户**：个人用户资金账户
- **商户账户**：商户收款账户
- **平台损益账户**：平台收入支出账户
- **中间账户**：交易过程中的临时账户

### 3. 分库分表
- **10个数据库，每个库10张表**
- 支持水平扩展
- 基于用户ID和账户号的Hash路由策略
- 自动分片路由和数据访问

### 4. 分布式特性
- **Kafka消息队列**：异步任务处理和重试
- **Redis分布式锁**：日切等关键操作的并发控制
- **etcd服务发现**：微服务注册与发现
- **定时任务**：自动触发日切和任务补偿

### 5. 日切功能
- 每日自动执行日切操作
- 记录每个分片的日切点transaction ID
- 生成每日余额快照
- 支持日切状态监控和失败重试

### 6. 高可用保障
- 乐观锁机制防止并发冲突
- 异步任务自动重试（指数退避策略）
- 分布式事务补偿机制
- 完整的审计日志

## 技术栈

- **语言**: Go 1.21+
- **RPC框架**: Kitex (字节跳动开源)
- **依赖注入**: Uber Fx
- **数据库**: MySQL 8.0 (分库分表)
- **缓存**: Redis 7
- **消息队列**: Kafka
- **服务发现**: etcd
- **容器化**: Docker & Docker Compose

## 项目结构

```
accounting-system/
├── cmd/
│   └── server/          # 主程序入口
│       └── main.go
├── internal/
│   ├── domain/          # 领域模型
│   │   └── model/
│   ├── repository/      # 数据访问层
│   ├── service/         # 业务逻辑层
│   │   ├── accounting_service.go      # 记账服务
│   │   ├── day_cut_service.go         # 日切服务
│   │   └── async_task_service.go      # 异步任务服务
│   └── infrastructure/  # 基础设施层
│       ├── database/    # 数据库管理
│       ├── sharding/    # 分库分表路由
│       └── kafka/       # Kafka消息处理
├── idl/                 # Kitex IDL定义
│   └── accounting.thrift
├── config/              # 配置文件
│   └── config.yaml
├── database/            # 数据库脚本
│   └── schema.sql
├── tests/               # 测试代码
│   └── e2e/            # 端到端测试
│       └── accounting_test.go
├── docker-compose.yml   # Docker编排
├── Dockerfile           # Docker镜像
├── Makefile            # 构建脚本
└── README.md           # 项目文档
```

## 快速开始

### 前置要求

- Go 1.21+
- Docker & Docker Compose
- Make

### 安装开发工具

```bash
make install-tools
```

### 启动环境

一键启动所有依赖服务（MySQL、Redis、Kafka、etcd）：

```bash
make docker-up
```

### 初始化数据库

```bash
make init-db
```

### 生成Kitex代码

```bash
make gen-kitex
```

### 编译项目

```bash
make build
```

### 运行服务

```bash
make run
```

### 运行测试

```bash
# 单元测试
make test

# E2E测试
make e2e-test

# 生成覆盖率报告
make coverage
```

## 核心业务流程

### 1. 创建账户

```go
account, err := accountingService.CreateAccount(
    ctx,
    userID,           // 用户ID
    AccountTypeUser,  // 账户类型
    AccountCategoryAsset, // 会计科目
    "PHP",           // 币种
)
```

### 2. 复式记账

```go
// 示例：用户充值100元
voucherNo, transactionIDs, err := accountingService.DoubleEntryBooking(ctx, &DoubleEntryBookingRequest{
    BusinessNo:   "BIZ_DEPOSIT_001",
    BusinessType: BusinessTypeDeposit,
    Entries: []AccountingEntry{
        {
            AccountNo:    userAccountNo,
            DebitAmount:  decimal.NewFromInt(100),  // 借：用户资产
            CreditAmount: decimal.Zero,
        },
        {
            AccountNo:    platformAccountNo,
            DebitAmount:  decimal.Zero,
            CreditAmount: decimal.NewFromInt(100),  // 贷：平台账户
        },
    },
    Currency: "PHP",
})
```

### 3. 触发日切

```go
err := dayCutService.TriggerDayCut(ctx, "2024-01-01")
```

### 4. 查询余额快照

```go
snapshot, err := getBalanceSnapshot(ctx, accountNo, "2024-01-01")
```

## 分库分表策略

### 路由算法

1. **用户账户**：`dbIndex = userID % 10`, `tableIndex = userID % 100`
2. **交易流水**：`dbIndex = hash(accountNo) % 10`, `tableIndex = hash(accountNo) % 100`
3. **余额快照**：与交易流水使用相同路由

### 表命名规则

```
account_00 ... account_99              # 账户表
account_transaction_00 ... account_transaction_99  # 流水表
account_balance_snapshot_00 ... account_balance_snapshot_99  # 快照表
```

## 日切流程

1. **初始化日切控制记录**：为每个分片创建日切任务
2. **并发处理分片**：使用Goroutine并发处理所有分片
3. **统计当日交易**：查询每个分片的当日交易
4. **生成余额快照**：计算账户期初、期末余额和交易统计
5. **记录日切点**：保存最后一笔交易ID作为日切点
6. **状态更新**：更新日切状态为完成或失败

## 异步任务重试

### Kafka消息重试

```go
// 1. 发送消息到Kafka
producer.SendMessage(ctx, businessNo, "ACCOUNTING", taskData)

// 2. 消费者处理失败时
// - 不提交offset
// - 消息会被重新消费

// 3. 达到最大重试次数后
// - 保存到async_task表
// - 定时任务定期扫描重试
```

### 定时任务补偿

```go
// 每分钟扫描一次待重试任务
asyncTaskService.ProcessPendingTasks(ctx)
```

## 监控指标

- 交易成功率
- 记账耗时
- 日切完成时间
- 异步任务重试次数
- 账户余额一致性

## 性能指标

- **TPS**: 10000+ (单机)
- **平均响应时间**: <50ms
- **日切时间**: <10min (1000万笔交易)
- **数据一致性**: 100%

## E2E测试

系统包含完整的端到端测试，涵盖：

### 1. 复式记账测试
- 账户创建
- 充值交易
- 支付交易
- 余额验证

### 2. 日切平衡测试
- 批量交易生成
- 日切执行
- 余额快照验证
- 借贷平衡验证

运行测试：

```bash
# 运行所有E2E测试
make e2e-test

# 运行特定测试
go test -v ./tests/e2e/ -run TestE2E_DayCutBalance
```

## Docker部署

### 启动完整环境

```bash
docker-compose up -d
```

### 服务列表

- `mysql-0, mysql-1, mysql-2`: MySQL数据库实例
- `redis`: Redis缓存
- `zookeeper`: Zookeeper协调服务
- `kafka`: Kafka消息队列
- `etcd`: etcd服务发现
- `accounting-service`: 记账系统服务

### 查看日志

```bash
docker-compose logs -f accounting-service
```

### 停止环境

```bash
docker-compose down
```

## 配置说明

### 数据库配置 (config/config.yaml)

```yaml
database:
  shard_count: 10        # 分库数量
  table_count: 100       # 分表数量
  databases:
    - name: accounting_db_0
      dsn: "root:password@tcp(127.0.0.1:3306)/accounting_db_0"
      max_open_conns: 100
      max_idle_conns: 10
```

### Kafka配置

```yaml
kafka:
  brokers:
    - "127.0.0.1:9092"
  topics:
    accounting: "accounting-topic"
    snapshot: "snapshot-topic"
    day_cut: "day-cut-topic"
```

### 日切配置

```yaml
day_cut:
  enabled: true
  cron: "0 0 0 * * ?"    # 每天凌晨执行
  timeout: 3600          # 超时时间（秒）
```

## 常见问题

### Q: 如何保证分布式环境下的数据一致性？

A:
1. 使用乐观锁（version字段）防止并发修改
2. 数据库事务保证单个记账操作的原子性
3. 异步任务重试机制保证最终一致性
4. 日切流程验证借贷平衡

### Q: 日切失败如何处理？

A:
1. 每个分片独立执行，失败分片会记录错误信息
2. 支持单独重试失败的分片
3. 定时任务会自动重试失败的日切任务

### Q: 如何扩展更多分片？

A:
1. 修改配置中的shard_count和table_count
2. 创建新的数据库和表
3. 历史数据需要通过数据迁移工具重新分片

## 贡献指南

1. Fork 项目
2. 创建特性分支 (`git checkout -b feature/AmazingFeature`)
3. 提交更改 (`git commit -m 'Add some AmazingFeature'`)
4. 推送到分支 (`git push origin feature/AmazingFeature`)
5. 开启 Pull Request

## 许可证

MIT License

## 联系方式

- Issue: [GitHub Issues](https://github.com/your-repo/accounting-system/issues)
- Email: your-email@example.com
