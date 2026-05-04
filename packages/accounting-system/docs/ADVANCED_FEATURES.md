# 高级特性文档

## 1. 热点账户处理

### 1.1 什么是热点账户

热点账户是指在系统中被频繁访问和修改的账户，例如：
- **平台账户**：平台收入支出账户
- **营销活动账户**：红包、优惠券账户
- **大商户账户**：高频交易的商户
- **中间清算账户**：资金中转账户

### 1.2 热点账户的问题

```
高并发访问
    ↓
数据库行锁竞争
    ↓
大量事务等待
    ↓
系统TPS下降
    ↓
响应时间增加
```

### 1.3 解决方案

#### 方案1：账户拆分（推荐）

将热点账户拆分为多个子账户，降低单账户并发压力：

```go
// HotAccountSplitter 热点账户拆分器
type HotAccountSplitter struct {
    baseAccountNo string
    shardCount    int  // 拆分数量
}

// GetShardAccount 获取分片账户
func (s *HotAccountSplitter) GetShardAccount(businessNo string) string {
    // 根据业务单号hash到不同的子账户
    hash := crc32.ChecksumIEEE([]byte(businessNo))
    shardIndex := hash % uint32(s.shardCount)
    return fmt.Sprintf("%s_SHARD_%d", s.baseAccountNo, shardIndex)
}

// 使用示例
splitter := &HotAccountSplitter{
    baseAccountNo: "PLATFORM_HOT_ACCOUNT",
    shardCount:    10, // 拆分为10个子账户
}

// 每笔交易路由到不同的子账户
shardAccount := splitter.GetShardAccount(businessNo)
```

#### 方案2：异步入账

对于平台账户等非核心账户，使用异步入账：

```go
// 用户支付商户场景
// 1. 用户账户、商户账户：同步扣款/入账（核心账户）
// 2. 平台手续费账户：异步记账（非核心账户）

// 同步处理用户和商户账户
hybridService.HybridDoubleEntryBooking(ctx, &HybridBookingRequest{
    FundAccountEntries: []AccountingEntry{
        {AccountNo: userAccount, CreditAmount: 100},    // 同步
        {AccountNo: merchantAccount, DebitAmount: 95},  // 同步
    },
    LedgerEntries: []AccountingEntry{
        {AccountNo: platformFeeAccount, DebitAmount: 5}, // 异步
    },
})
```

#### 方案3：缓冲账户

使用Redis作为缓冲层，批量写入数据库：

```go
// HotAccountBuffer 热点账户缓冲
type HotAccountBuffer struct {
    redis         *redis.Client
    accountNo     string
    flushInterval time.Duration
    batchSize     int
}

// BufferTransaction 缓冲交易
func (b *HotAccountBuffer) BufferTransaction(tx Transaction) error {
    // 1. 将交易写入Redis队列
    key := fmt.Sprintf("hot_account_buffer:%s", b.accountNo)
    data, _ := json.Marshal(tx)
    b.redis.RPush(ctx, key, data)

    // 2. 增加账户余额计数器（Redis原子操作）
    balanceKey := fmt.Sprintf("hot_account_balance:%s", b.accountNo)
    if tx.IsDebit {
        b.redis.IncrByFloat(ctx, balanceKey, tx.Amount.InexactFloat64())
    } else {
        b.redis.DecrByFloat(ctx, balanceKey, tx.Amount.InexactFloat64())
    }

    return nil
}

// FlushBuffer 定时刷新到数据库
func (b *HotAccountBuffer) FlushBuffer() error {
    // 1. 从Redis读取缓冲的交易
    key := fmt.Sprintf("hot_account_buffer:%s", b.accountNo)
    txs := b.redis.LRange(ctx, key, 0, b.batchSize-1)

    // 2. 批量写入数据库
    if len(txs) > 0 {
        b.batchInsertTransactions(txs)
        b.redis.LTrim(ctx, key, len(txs), -1)
    }

    return nil
}
```

#### 方案4：读写分离

```go
// 写操作：主库
masterDB.Exec("UPDATE account SET balance = ? WHERE account_no = ?")

// 读操作：从库（可能有延迟）
slaveDB.Query("SELECT balance FROM account WHERE account_no = ?")

// 强一致性读：主库
masterDB.Query("SELECT balance FROM account WHERE account_no = ? FOR UPDATE")
```

## 2. 批量入账

### 2.1 批量入账场景

- **批量充值**：活动期间批量发放红包
- **批量提现**：批量处理商户提现
- **批量清算**：每日批量清算结算
- **批量退款**：批量处理退款

### 2.2 批量入账实现

#### 实现1：串行批量
```go
// SerialBatchBooking 串行批量记账
func (s *accountingService) SerialBatchBooking(ctx context.Context, requests []BookingRequest) (*BatchResult, error) {
    results := make([]BookingResult, len(requests))

    for i, req := range requests {
        result, err := s.DoubleEntryBooking(ctx, &req)
        if err != nil {
            results[i] = BookingResult{Success: false, Error: err}
        } else {
            results[i] = BookingResult{Success: true, Data: result}
        }
    }

    return &BatchResult{Results: results}, nil
}
```

#### 实现2：并行批量（推荐）
```go
// ParallelBatchBooking 并行批量记账
func (s *accountingService) ParallelBatchBooking(ctx context.Context, requests []BookingRequest) (*BatchResult, error) {
    // 1. 按账户分组（同一账户串行，不同账户并行）
    groups := s.groupByAccount(requests)

    // 2. 并行处理每组
    var wg sync.WaitGroup
    resultsChan := make(chan []BookingResult, len(groups))

    for _, group := range groups {
        wg.Add(1)
        go func(reqs []BookingRequest) {
            defer wg.Done()

            groupResults := make([]BookingResult, len(reqs))
            for i, req := range reqs {
                result, err := s.DoubleEntryBooking(ctx, &req)
                if err != nil {
                    groupResults[i] = BookingResult{Success: false, Error: err}
                } else {
                    groupResults[i] = BookingResult{Success: true, Data: result}
                }
            }

            resultsChan <- groupResults
        }(group)
    }

    wg.Wait()
    close(resultsChan)

    // 3. 合并结果
    allResults := make([]BookingResult, 0)
    for groupResults := range resultsChan {
        allResults = append(allResults, groupResults...)
    }

    return &BatchResult{Results: allResults}, nil
}

// groupByAccount 按账户分组
func (s *accountingService) groupByAccount(requests []BookingRequest) map[string][]BookingRequest {
    groups := make(map[string][]BookingRequest)

    for _, req := range requests {
        // 提取账户号作为key
        accountKey := s.extractAccountKey(req)
        groups[accountKey] = append(groups[accountKey], req)
    }

    return groups
}
```

#### 实现3：批量SQL优化
```go
// BatchInsertTransactions 批量插入流水
func (s *repository) BatchInsertTransactions(ctx context.Context, txs []*Transaction) error {
    // 构建批量INSERT语句
    query := `
        INSERT INTO account_transaction_xx
        (transaction_id, account_no, amount, ...)
        VALUES `

    values := make([]interface{}, 0)
    placeholders := make([]string, 0)

    for _, tx := range txs {
        placeholders = append(placeholders, "(?, ?, ?, ...)")
        values = append(values, tx.TransactionID, tx.AccountNo, tx.Amount, ...)
    }

    query += strings.Join(placeholders, ", ")

    // 执行批量插入
    _, err := db.ExecContext(ctx, query, values...)
    return err
}
```

### 2.3 批量入账性能优化

#### 优化1：分批处理
```go
// 大批量拆分为小批次
batchSize := 100
for i := 0; i < len(requests); i += batchSize {
    end := i + batchSize
    if end > len(requests) {
        end = len(requests)
    }

    batch := requests[i:end]
    s.ParallelBatchBooking(ctx, batch)
}
```

#### 优化2：预编译SQL
```go
// 使用prepared statement
stmt, err := db.Prepare("INSERT INTO account_transaction_xx VALUES (?, ?, ?)")
defer stmt.Close()

for _, tx := range txs {
    stmt.ExecContext(ctx, tx.TransactionID, tx.AccountNo, tx.Amount)
}
```

## 3. 缓冲记账

### 3.1 什么是缓冲记账

缓冲记账是一种性能优化策略，将高频小额交易先缓存起来，定期批量写入数据库。

### 3.2 适用场景

- **积分系统**：高频积分变动
- **游戏金币**：游戏内货币消耗
- **流量计费**：实时流量扣费
- **打赏系统**：直播打赏

### 3.3 缓冲记账实现

#### 架构设计
```
┌─────────────┐
│   Client    │
└──────┬──────┘
       │ 记账请求
       ↓
┌──────────────────┐
│  Buffer Layer    │  ← Redis缓冲层
│  (Redis Queue)   │
└──────┬───────────┘
       │ 定时/批量
       ↓
┌──────────────────┐
│  Flush Worker    │  ← 刷新工作器
└──────┬───────────┘
       │ 批量写入
       ↓
┌──────────────────┐
│    Database      │  ← 数据库
└──────────────────┘
```

#### 核心代码
```go
// BufferedAccountingService 缓冲记账服务
type BufferedAccountingService struct {
    redis         *redis.Client
    db            *sql.DB
    flushInterval time.Duration
    batchSize     int
}

// NewBufferedAccountingService 创建缓冲记账服务
func NewBufferedAccountingService(redis *redis.Client, db *sql.DB) *BufferedAccountingService {
    svc := &BufferedAccountingService{
        redis:         redis,
        db:            db,
        flushInterval: 1 * time.Second,  // 每秒刷新一次
        batchSize:     1000,              // 每批1000条
    }

    // 启动刷新goroutine
    go svc.flushWorker()

    return svc
}

// BufferedBooking 缓冲记账
func (s *BufferedAccountingService) BufferedBooking(ctx context.Context, req *BookingRequest) error {
    // 1. 验证请求
    if err := s.validate(req); err != nil {
        return err
    }

    // 2. 生成交易ID
    txID := s.generateTransactionID()

    // 3. 序列化交易数据
    txData := &BufferedTransaction{
        TransactionID: txID,
        AccountNo:     req.AccountNo,
        Amount:        req.Amount,
        IsDebit:       req.IsDebit,
        BusinessNo:    req.BusinessNo,
        Timestamp:     time.Now(),
    }
    data, _ := json.Marshal(txData)

    // 4. 写入Redis队列
    queueKey := fmt.Sprintf("buffered_tx:queue:%s", s.getQueueShard(req.AccountNo))
    s.redis.RPush(ctx, queueKey, data)

    // 5. 更新Redis中的账户余额（用于实时查询）
    balanceKey := fmt.Sprintf("buffered_balance:%s", req.AccountNo)
    if req.IsDebit {
        s.redis.IncrByFloat(ctx, balanceKey, req.Amount.InexactFloat64())
    } else {
        s.redis.DecrByFloat(ctx, balanceKey, req.Amount.InexactFloat64())
    }

    // 6. 设置余额过期时间（防止内存泄漏）
    s.redis.Expire(ctx, balanceKey, 24*time.Hour)

    return nil
}

// flushWorker 刷新工作器
func (s *BufferedAccountingService) flushWorker() {
    ticker := time.NewTicker(s.flushInterval)
    defer ticker.Stop()

    for range ticker.C {
        s.flushAllQueues()
    }
}

// flushAllQueues 刷新所有队列
func (s *BufferedAccountingService) flushAllQueues() {
    // 获取所有队列
    queueKeys, _ := s.redis.Keys(ctx, "buffered_tx:queue:*").Result()

    for _, queueKey := range queueKeys {
        s.flushQueue(queueKey)
    }
}

// flushQueue 刷新单个队列
func (s *BufferedAccountingService) flushQueue(queueKey string) error {
    // 1. 从Redis读取一批数据
    txsJSON, err := s.redis.LRange(ctx, queueKey, 0, s.batchSize-1).Result()
    if err != nil || len(txsJSON) == 0 {
        return err
    }

    // 2. 反序列化
    txs := make([]*BufferedTransaction, 0, len(txsJSON))
    for _, txJSON := range txsJSON {
        var tx BufferedTransaction
        json.Unmarshal([]byte(txJSON), &tx)
        txs = append(txs, &tx)
    }

    // 3. 批量写入数据库
    if err := s.batchInsertDB(txs); err != nil {
        // 写入失败，不删除队列数据，下次继续重试
        return err
    }

    // 4. 删除已处理的数据
    s.redis.LTrim(ctx, queueKey, int64(len(txs)), -1)

    // 5. 更新数据库中的账户余额
    s.updateAccountBalances(txs)

    return nil
}

// batchInsertDB 批量插入数据库
func (s *BufferedAccountingService) batchInsertDB(txs []*BufferedTransaction) error {
    // 开启事务
    tx, err := s.db.Begin()
    if err != nil {
        return err
    }
    defer tx.Rollback()

    // 批量插入流水
    query := `INSERT INTO account_transaction_xx
              (transaction_id, account_no, amount, is_debit, business_no, created_at)
              VALUES `

    values := make([]interface{}, 0)
    placeholders := make([]string, 0)

    for _, t := range txs {
        placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?)")
        values = append(values, t.TransactionID, t.AccountNo, t.Amount,
                       t.IsDebit, t.BusinessNo, t.Timestamp)
    }

    query += strings.Join(placeholders, ", ")
    _, err = tx.Exec(query, values...)
    if err != nil {
        return err
    }

    // 提交事务
    return tx.Commit()
}

// GetBufferedBalance 获取缓冲余额
func (s *BufferedAccountingService) GetBufferedBalance(ctx context.Context, accountNo string) (decimal.Decimal, error) {
    // 1. 从数据库获取基准余额
    dbBalance, err := s.getDBBalance(accountNo)
    if err != nil {
        return decimal.Zero, err
    }

    // 2. 从Redis获取增量余额
    balanceKey := fmt.Sprintf("buffered_balance:%s", accountNo)
    deltaBalance, err := s.redis.Get(ctx, balanceKey).Float64()
    if err == redis.Nil {
        deltaBalance = 0
    } else if err != nil {
        return decimal.Zero, err
    }

    // 3. 计算总余额 = 数据库余额 + 缓冲增量
    totalBalance := dbBalance.Add(decimal.NewFromFloat(deltaBalance))

    return totalBalance, nil
}

// getQueueShard 获取队列分片
func (s *BufferedAccountingService) getQueueShard(accountNo string) string {
    // 根据账户号hash到不同的队列，减少Redis单key压力
    hash := crc32.ChecksumIEEE([]byte(accountNo))
    shardIndex := hash % 10
    return fmt.Sprintf("shard_%d", shardIndex)
}
```

### 3.4 缓冲记账的权衡

#### 优点
✅ **高性能**：减少数据库写入压力
✅ **高吞吐**：支持更高的TPS
✅ **批量优化**：批量写入提升效率

#### 缺点
❌ **数据延迟**：余额不是实时的
❌ **数据丢失风险**：Redis宕机可能丢失缓冲数据
❌ **复杂度增加**：需要额外的刷新机制

#### 适用判断
```
是否需要强一致性？
    ├─ 是 → 不使用缓冲记账（直接写DB）
    └─ 否 → 可以使用缓冲记账

是否可以容忍延迟？
    ├─ 是 → 使用缓冲记账
    └─ 否 → 不使用缓冲记账

是否高频小额交易？
    ├─ 是 → 适合缓冲记账
    └─ 否 → 不需要缓冲记账
```

### 3.5 缓冲记账的可靠性保证

#### 方案1：Redis持久化
```bash
# redis.conf
appendonly yes
appendfsync everysec
```

#### 方案2：双写保证
```go
// 同时写入Redis和Kafka
s.redis.RPush(ctx, queueKey, data)
s.kafka.Send(ctx, "buffered-tx-topic", data)

// Kafka消费者作为备份
kafkaConsumer.Consume(func(msg Message) {
    // 如果Redis中没有，从Kafka恢复
    if !s.redis.Exists(ctx, txID) {
        s.redis.RPush(ctx, queueKey, msg.Data)
    }
})
```

#### 方案3：定期对账
```go
// 每小时对账一次
func (s *BufferedAccountingService) ReconcileAccounts() {
    // 1. 获取所有缓冲账户
    accounts := s.getAllBufferedAccounts()

    for _, account := range accounts {
        // 2. 计算Redis中的余额增量
        redisBalance := s.redis.Get(ctx, "buffered_balance:"+account).Float64()

        // 3. 计算数据库中未刷新的流水总额
        pendingAmount := s.calculatePendingAmount(account)

        // 4. 对比差异
        if math.Abs(redisBalance - pendingAmount) > 0.01 {
            // 差异超过阈值，触发告警
            s.logger.Error("balance reconciliation failed",
                zap.String("account", account),
                zap.Float64("redis", redisBalance),
                zap.Float64("pending", pendingAmount),
            )
        }
    }
}
```

## 4. 性能对比

### 4.1 普通记账 vs 批量记账

| 指标 | 普通记账 | 批量记账（串行） | 批量记账（并行） |
|------|---------|----------------|----------------|
| TPS | 1000 | 800 | 5000 |
| 平均响应时间 | 50ms | 60ms | 150ms（总体） |
| 数据库连接数 | 高 | 高 | 中 |
| 适用场景 | 实时交易 | 对账清算 | 批量发放 |

### 4.2 直接记账 vs 缓冲记账

| 指标 | 直接记账 | 缓冲记账 |
|------|---------|---------|
| TPS | 5000 | 50000 |
| 数据延迟 | 0ms | <1s |
| 数据可靠性 | 100% | 99.99% |
| 适用场景 | 核心账户 | 非核心账户 |

## 5. 最佳实践

### 5.1 场景选择

```
用户余额账户
    └─ 直接记账（强一致性）

平台手续费账户
    └─ 缓冲记账（高性能）

积分账户
    └─ 缓冲记账 + 定期对账

红包账户
    └─ 热点账户拆分 + 异步记账

批量发放
    └─ 并行批量记账
```

### 5.2 监控告警

```go
// 关键指标监控
- 缓冲队列长度
- 刷新延迟时间
- 对账差异金额
- Redis内存使用
- 批量失败率
```

### 5.3 容灾方案

```
主备切换
    ├─ Redis主从复制
    ├─ Kafka消息备份
    └─ 定期数据快照

数据恢复
    ├─ 从Kafka恢复缓冲数据
    ├─ 从快照恢复账户余额
    └─ 重新执行未完成的批量任务
```

## 6. 总结

本文档介绍了三种高级性能优化方案：

1. **热点账户处理**：拆分、异步、缓冲、读写分离
2. **批量入账**：串行、并行、SQL优化
3. **缓冲记账**：队列缓冲、定时刷新、对账保障

根据业务场景选择合适的方案，可以大幅提升系统性能和吞吐量。
