# 复式记账账户系统 - 架构设计文档

## 1. 系统概述

本系统是一个企业级复式记账账户系统，基于Go语言和微服务架构设计，支持高并发、高可用、强一致性的账务处理。

### 1.1 核心特性

- **复式记账**：严格遵循会计准则，有借必有贷，借贷必相等
- **分库分表**：10库100表，支持海量数据和高并发
- **多种记账模式**：同步、异步、批量、混合模式
- **资金流引擎**：可配置的资金流规则，支持多种业务场景
- **日切与快照**：自动日切，生成每日余额快照
- **调账与补偿**：支持调账、单边账、冲正等特殊操作
- **分布式架构**：基于Kafka、etcd、Redis的分布式系统

## 2. 系统架构

### 2.1 整体架构图

```
┌─────────────────────────────────────────────────────────────┐
│                        Client Layer                          │
│                    (gRPC / HTTP API)                         │
└────────────────────┬────────────────────────────────────────┘
                     │
┌────────────────────┴────────────────────────────────────────┐
│                     Service Layer                            │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │ Accounting   │  │ Money Flow   │  │ Day Cut      │      │
│  │ Service      │  │ Engine       │  │ Service      │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │ Adjustment   │  │ Async Task   │  │ Hybrid       │      │
│  │ Service      │  │ Service      │  │ Accounting   │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
└────────────────────┬────────────────────────────────────────┘
                     │
┌────────────────────┴────────────────────────────────────────┐
│                   Repository Layer                           │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │ Account      │  │ Transaction  │  │ Snapshot     │      │
│  │ Repository   │  │ Repository   │  │ Repository   │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
└────────────────────┬────────────────────────────────────────┘
                     │
┌────────────────────┴────────────────────────────────────────┐
│                Infrastructure Layer                          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │ DB Manager   │  │ Sharding     │  │ Kafka        │      │
│  │ (10 DBs)     │  │ Router       │  │ Producer     │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │ Redis        │  │ etcd         │  │ Kafka        │      │
│  │ (Lock/Cache) │  │ (Discovery)  │  │ Consumer     │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
└─────────────────────────────────────────────────────────────┘
```

### 2.2 技术选型

| 组件 | 技术选型 | 说明 |
|------|---------|------|
| 编程语言 | Go 1.21+ | 高性能、并发友好 |
| RPC框架 | Kitex | 字节跳动开源，高性能 |
| 依赖注入 | Uber Fx | 优雅的依赖管理 |
| 数据库 | MySQL 8.0 | 分库分表，事务支持 |
| ORM | GORM | 推荐使用（当前用sqlx） |
| 缓存 | Redis 7 | 分布式锁、缓存 |
| 消息队列 | Kafka | 异步任务、削峰填谷 |
| 服务发现 | etcd | 服务注册与发现 |
| 容器化 | Docker | 一键部署 |

## 3. 数据库设计

### 3.1 分库分表策略

#### 分片规则
- **10个数据库**：`accounting_db_0` ~ `accounting_db_9`
- **每库10张表**：`table_{$dbindex}0` ~ `table_{$dbindex}9`

#### 路由策略
```go
// 用户账户：按 user_id 路由
dbIndex = user_id / 10
tableIndex = user_id % 10

// 交易流水：按 account_no 路由
dbIndex = hash(account_no) / 10
tableIndex = hash(account_no) % 10
```

### 3.2 核心表结构

#### 账户表 (account_xx)
```sql
- account_no (账户号, UK)
- user_id (用户ID)
- account_type (账户类型: 1-用户 2-商户 3-平台 4-中间)
- account_category (会计科目: ASSET/LIABILITY/EQUITY/REVENUE/EXPENSE)
- balance (余额)
- frozen_balance (冻结余额)
- available_balance (可用余额)
- version (乐观锁版本号)
```

#### 交易流水表 (account_transaction_xx)
```sql
- transaction_id (交易ID, UK)
- parent_transaction_id (父交易ID)
- account_no (账户号)
- business_no (业务订单号)
- debit_amount (借方金额)
- credit_amount (贷方金额)
- balance_before (交易前余额)
- balance_after (交易后余额)
- transaction_date (交易日期)
- status (状态)
```

#### 余额快照表 (account_balance_snapshot_xx)
```sql
- account_no (账户号)
- snapshot_date (快照日期)
- beginning_balance (期初余额)
- ending_balance (期末余额)
- total_debit (当日借方总额)
- total_credit (当日贷方总额)
- transaction_count (交易笔数)
```

#### 日切控制表 (day_cut_control)
```sql
- database_index (库索引)
- table_index (表索引)
- cut_date (日切日期)
- cut_transaction_id (日切点交易ID)
- status (状态)
```

## 4. 核心服务设计

### 4.1 复式记账服务 (AccountingService)

#### 核心方法
```go
// 复式记账
DoubleEntryBooking(ctx, req) -> (voucherNo, transactionIDs, error)

// 创建账户
CreateAccount(ctx, userID, accountType, category, currency) -> (account, error)

// 查询账户
GetAccount(ctx, accountNo) -> (account, error)
```

#### 记账原则
1. **有借必有贷，借贷必相等**
2. **原子性**：单个记账操作要么全部成功，要么全部失败
3. **乐观锁**：使用version字段防止并发冲突
4. **幂等性**：业务订单号唯一，防止重复记账

### 4.2 资金流引擎 (MoneyFlowEngine)

#### 设计思想
通过配置化的资金流规则，实现不同产品和场景的自动记账。

#### 资金流配置
```go
FlowConfig {
    ProductCode: "PAYMENT"    // 产品编码
    SceneCode: "PAY_MERCHANT" // 场景编码
    FlowRules: [              // 资金流规则
        {
            DebitAccount: "merchant",   // 借方
            CreditAccount: "user",      // 贷方
            AmountFormula: "amount"     // 金额公式
        }
    ]
    FeeRules: [               // 费用规则
        {
            FeeType: "PERCENT",
            Percent: 0.006,   // 0.6%手续费
            PayerAccount: "merchant",
            ReceiverAccount: "platform"
        }
    ]
}
```

#### 内置场景
- **用户充值** (DEPOSIT)
- **用户提现** (WITHDRAW)
- **支付商户** (PAY_MERCHANT)
- **用户转账** (USER_TO_USER)
- **退款** (REFUND)

### 4.3 混合记账服务 (HybridAccountingService)

#### 设计原则
- **资金账户（用户/商户）**：同步操作，保证实时性
- **会计分录（平台/中间账户）**：异步记录，提升性能

#### 工作流程
```
1. 同步更新用户/商户账户余额
   ├─ 加锁查询账户
   ├─ 验证余额
   ├─ 更新余额
   └─ 提交事务

2. 异步记录会计分录
   ├─ 创建异步任务
   ├─ 发送Kafka消息
   └─ 定时任务重试
```

### 4.4 调账服务 (AdjustmentService)

#### 功能
- **余额调整**：修正账务错误
- **单边账**：特殊场景的单方记账
- **冲正**：反向冲销交易

#### 调账类型
```go
CORRECTION  // 余额修正
COMPENSATE  // 补偿调账
MANUAL      // 手工调账
```

#### 安全控制
- 必须提供审批单号
- 记录操作员信息
- 完整的审计日志
- 权限验证

### 4.5 日切服务 (DayCutService)

#### 日切流程
```
1. 初始化日切控制记录
   └─ 为每个分片创建日切任务

2. 并发处理所有分片
   ├─ 查询当日交易
   ├─ 按账户统计
   ├─ 生成余额快照
   └─ 记录日切点transaction_id

3. 更新日切状态
   └─ 记录完成时间和结果
```

#### 日切点设计
每个分片记录最后一笔交易ID，用于：
- 日终对账
- 增量同步
- 数据恢复

### 4.6 异步任务服务 (AsyncTaskService)

#### 重试策略
- **指数退避**：1分钟 -> 2分钟 -> 4分钟 -> 8分钟
- **最大重试次数**：3次
- **失败处理**：记录错误信息，人工介入

#### 任务类型
```go
ACCOUNTING  // 记账任务
SNAPSHOT    // 快照任务
DAY_CUT     // 日切任务
LEDGER_ENTRY // 会计分录任务
```

## 5. 分布式设计

### 5.1 Kafka消息队列

#### Topic设计
```
accounting-topic  // 记账消息
snapshot-topic    // 快照消息
day-cut-topic     // 日切消息
```

#### 消息格式
```json
{
  "key": "business_no",
  "type": "ACCOUNTING",
  "data": {...},
  "timestamp": 1234567890
}
```

#### 消费者组
- `accounting-consumer-group`：记账消费者组
- 支持横向扩展
- 自动负载均衡

### 5.2 Redis分布式锁

#### 使用场景
- 日切并发控制
- 账户操作串行化
- 定时任务互斥

#### 锁设计
```go
lockKey: "day_cut:2024-01-01"
lockValue: UUID
expireTime: 1小时
```

### 5.3 etcd服务发现

#### 服务注册
```go
key: /services/accounting/{instance_id}
value: {
  "host": "192.168.1.100",
  "port": 8888,
  "weight": 100
}
TTL: 30秒（心跳续约）
```

#### 服务发现
- Kitex集成etcd
- 自动健康检查
- 负载均衡

## 6. 一致性保证

### 6.1 数据一致性

#### 单账户一致性
- 数据库事务保证
- 乐观锁防止并发冲突

#### 多账户一致性
- 复式记账保证借贷平衡
- 日切验证总账平衡

#### 最终一致性
- 异步任务重试机制
- 定时任务补偿
- 人工对账

### 6.2 幂等性设计

#### 业务层幂等
```go
// 使用business_no作为唯一键
UNIQUE KEY uk_business_no (business_no)
```

#### 消息层幂等
```go
// Kafka消费者使用offset管理
// 处理完成才提交offset
```

## 7. 性能优化

### 7.1 数据库优化

#### 索引设计
```sql
-- 账户表
PRIMARY KEY (id)
UNIQUE KEY uk_account_no (account_no)
KEY idx_user_id (user_id)

-- 流水表
PRIMARY KEY (id)
UNIQUE KEY uk_transaction_id (transaction_id)
KEY idx_account_no (account_no)
KEY idx_business_no (business_no)
KEY idx_transaction_date (transaction_date)
```

#### 分库分表
- 10库100表 = 100个分片
- 单表数据量控制在1000万以内
- 支持水平扩展

### 7.2 缓存策略

#### 热点账户缓存
```go
key: "account:{account_no}"
value: account_json
TTL: 5分钟
```

#### 缓存更新策略
- Cache Aside Pattern
- 更新数据库后删除缓存
- 惰性加载

### 7.3 批量操作

#### 批量记账
```go
// 并行处理
BatchBooking(requests, parallel=true)

// 串行处理
BatchBooking(requests, parallel=false)
```

#### 性能指标
- 并行批量：TPS提升5-10倍
- 适用于账户不冲突的场景

## 8. 监控与运维

### 8.1 监控指标

#### 业务指标
- 记账成功率
- 记账平均耗时
- 日切完成时间
- 借贷平衡检查

#### 系统指标
- QPS/TPS
- 响应时间P99
- 数据库连接池
- Kafka消费延迟

### 8.2 日志设计

#### 日志级别
```go
INFO  // 正常业务流程
WARN  // 单边账、调账等特殊操作
ERROR // 错误和异常
DEBUG // 调试信息
```

#### 关键日志
```go
记账开始/结束
账户余额变更
日切开始/结束
异步任务重试
```

### 8.3 告警规则

- 记账失败率 > 1%
- 日切超时 > 1小时
- 异步任务积压 > 1000
- 数据库慢查询 > 1秒

## 9. 安全设计

### 9.1 权限控制

- 操作员身份认证
- 调账需要审批
- 操作日志审计

### 9.2 数据安全

- 敏感信息加密
- 数据库访问控制
- 定期备份

### 9.3 防范措施

- SQL注入防护
- XSS防护
- CSRF防护
- 限流和熔断

## 10. 扩展性设计

### 10.1 水平扩展

#### 应用层
- 无状态设计
- 通过etcd服务发现
- 任意数量实例

#### 数据库层
- 继续分库分表
- 数据迁移工具
- 路由规则调整

### 10.2 功能扩展

#### 新增账户类型
```go
// 在AccountType中添加
AccountTypeVirtual = 5  // 虚拟账户
```

#### 新增资金流场景
```go
engine.RegisterFlowConfig("LOAN", "BORROW", &FlowConfig{...})
```

## 11. 部署架构

### 11.1 开发环境
```
docker-compose up
```

### 11.2 生产环境
```
┌─────────────────────────────────────┐
│          Load Balancer              │
└───────────┬─────────────────────────┘
            │
    ┌───────┴────────┬─────────┐
    │                │         │
┌───┴───┐      ┌────┴───┐  ┌──┴────┐
│ App-1 │      │ App-2  │  │ App-3 │
└───┬───┘      └────┬───┘  └──┬────┘
    │               │         │
    └───────┬───────┴─────────┘
            │
    ┌───────┴────────────────────┐
    │      etcd Cluster          │
    │      (Service Discovery)    │
    └────────────────────────────┘

┌────────────────────────────────────┐
│         Kafka Cluster              │
│       (Message Queue)              │
└────────────────────────────────────┘

┌────────────────────────────────────┐
│       MySQL Cluster (10 DBs)       │
│       + Read Replicas              │
└────────────────────────────────────┘

┌────────────────────────────────────┐
│       Redis Cluster                │
│       (Cache & Lock)               │
└────────────────────────────────────┘
```

## 12. 使用示例

### 12.1 基础记账
```go
// 用户充值100元
voucherNo, txIDs, err := accountingService.DoubleEntryBooking(ctx, &DoubleEntryBookingRequest{
    BusinessNo: "BIZ001",
    BusinessType: model.BusinessTypeDeposit,
    Entries: []AccountingEntry{
        {AccountNo: "user_account", DebitAmount: 100, CreditAmount: 0},
        {AccountNo: "platform_account", DebitAmount: 0, CreditAmount: 100},
    },
    Currency: "PHP",
})
```

### 12.2 资金流引擎
```go
// 用户支付商户
response, err := flowEngine.ExecuteFlow(ctx, &FlowExecutionRequest{
    ProductCode: "PAYMENT",
    SceneCode: "PAY_MERCHANT",
    BusinessNo: "ORDER001",
    Amount: decimal.NewFromInt(100),
    Participants: map[string]string{
        "user": "user_account_123",
        "merchant": "merchant_account_456",
    },
})
```

### 12.3 混合记账
```go
// 资金账户同步，会计分录异步
response, err := hybridService.HybridDoubleEntryBooking(ctx, &HybridBookingRequest{
    FundAccountEntries: [...],  // 同步处理
    LedgerEntries: [...],       // 异步处理
})
```

## 13. 最佳实践

### 13.1 开发规范
- 使用依赖注入（Fx）
- 错误处理要完整
- 日志记录要详细
- 测试覆盖率 > 80%

### 13.2 运维规范
- 定期备份数据库
- 监控告警及时响应
- 定期日切平衡检查
- 审计日志定期归档

### 13.3 安全规范
- 敏感操作需要审批
- 定期权限审计
- 数据访问审计
- 定期安全扫描

## 14. FAQ

### Q1: 如何保证高并发下的性能？
A:
- 分库分表减少单库压力
- 缓存热点数据
- 异步处理非核心流程
- 批量操作提升吞吐量

### Q2: 如何保证数据一致性？
A:
- 数据库事务保证单账户一致性
- 复式记账保证借贷平衡
- 异步任务重试保证最终一致性
- 日切验证总账平衡

### Q3: 如何处理分布式事务？
A:
- 资金账户使用本地事务
- 跨账户使用异步消息+补偿
- 最终一致性模型

### Q4: 如何扩展到更多分片？
A:
- 修改分片数量配置
- 历史数据迁移
- 更新路由规则
- 灰度切换

## 15. 总结

本系统是一个完整的企业级复式记账解决方案，具备以下优势：

### 技术优势
- **高性能**：分库分表 + 缓存 + 异步处理
- **高可用**：分布式架构 + 服务发现 + 自动重试
- **强一致**：事务保证 + 复式记账 + 日切验证
- **易扩展**：配置化 + 插件化 + 水平扩展

### 业务优势
- **灵活配置**：资金流引擎支持多种场景
- **多种模式**：同步/异步/批量/混合
- **完整功能**：开户/记账/调账/日切/快照
- **审计追溯**：完整的操作日志和审计跟踪

### 生产就绪
- Docker一键部署
- 完整的E2E测试
- 详细的监控指标
- 运维工具完善
