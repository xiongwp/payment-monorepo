# risk-manage ClickHouse 接入

## 两种接入路径

risk-manage 可以走两条路把 audit 写到 ClickHouse；选一种即可：

| 路径 | 谁写 CH | 优点 | 缺点 | 推荐场景 |
|---|---|---|---|---|
| **A. 直连 sink** | risk-manage `ClickHouseSink` 直接 INSERT | 写入立即可查；逻辑简单；不依赖 Kafka | 高 QPS 时 risk-manage → CH 单点压力；CH 故障会反压主路径 (虽然 AsyncBatchSink 有 buffer) | 中小流量 / 没接 Kafka / POC |
| **B. Kafka engine** | risk-manage `KafkaSink` → topic → CH Kafka engine table → MV → main table | 解耦 risk-manage 跟 CH；Kafka 天然背压；同 topic 同时给 Flink 消费 | 多一跳 (有几秒延迟才查得到)；运维复杂 (Kafka 集群 + CH Kafka engine) | 高 QPS 生产 / 需要 Flink 实时分析 |

**生产推荐 B**。Kafka 已经是 Flink / 实时聚合的事实标准，让 CH 通过 Kafka engine 消费同一 topic 比双写更省心。

### 配置切换

**A. 仅直连**：
```yaml
# config.yaml / Helm values
audit:
  clickhouse:
    dsn: "clickhouse:9000"
    database: risk
    username: risk_writer
    password: ${CH_PASSWORD}
  # 不配 audit.kafka.brokers
```

**B. 仅 Kafka → CH Kafka engine**：
```yaml
audit:
  kafka:
    brokers: "kafka-bootstrap:9092"
    topic: risk.decision.v1
  # 不配 audit.clickhouse.dsn (CH 通过 Kafka engine 自己消费)
```

CH 端建表（`deploy/clickhouse/init/01-schema.sql` 已包含）：
```sql
CREATE TABLE risk.risk_decision_kafka (...) ENGINE = Kafka SETTINGS
    kafka_broker_list = 'kafka:9092',
    kafka_topic_list = 'risk.decision.v1',
    kafka_group_name = 'ch-risk-decision', ...;

CREATE MATERIALIZED VIEW risk.risk_decision_kafka_mv
TO risk.risk_decision_audit AS SELECT ... FROM risk.risk_decision_kafka;
```

**双写 (A+B)**：仅在迁移期短期用 (从 A 切到 B)。配两组都填，然后通过 Kafka topic 双写跟直连同时落 CH，确认 Kafka 通路稳定后再删掉直连。**长期双写有数据不一致风险**（Kafka topic 滞后 / CH 直连失败时 vs Kafka 引擎失败时表现不同）。

---



长期决策审计 + OLAP 分析。risk-manage 通过 `audit.ClickHouseSink`
（`//go:build clickhouse` tag）批量 INSERT 每笔决策；ClickHouse 跑 cohort
/ vintage / dashboard 的历史查询。

## 启用步骤

1. `go.mod`：
   ```
   require github.com/ClickHouse/clickhouse-go/v2 v2.20.0
   ```
2. 删 `internal/audit/clickhouse_sink.go` 顶部 `//go:build clickhouse`。
3. 修改 `cmd/server/main.go` 的 `newAuditSink` 把 `audit.NewClickHouseSink`
   加进 MultiSink (用 `AsyncBatchSink` 包一层避免 hot path 阻塞)：
   ```go
   conn, _ := clickhouse.Open(&clickhouse.Options{
       Addr: []string{"ch:9000"},
       Auth: clickhouse.Auth{Database: "risk", Username: "risk_writer", Password: pwd},
       MaxOpenConns: 10,
   })
   chSink := audit.NewClickHouseSink(conn, logger)
   async := audit.NewAsyncBatchSink(chSink, 8192, 1000, time.Second, logger)
   base = append(base, async)
   ```
4. 配 `audit.chain_signing: true` 让 ChainSink 的 `chain_prev_hash /
   chain_row_hash` 写到 metadata；`metadata_json` 字段会一起落 CH。

## DDL — 决策审计表

```sql
CREATE TABLE risk_decision_audit (
    decision_id        String,
    occurred_at        DateTime64(9, 'UTC'),
    rule_version       Int32,
    pi_id              String,
    merchant_id        LowCardinality(String),
    customer_id        String,
    amount             Int64,
    currency           LowCardinality(String),
    payment_method     LowCardinality(String),
    country            LowCardinality(String),
    ip_address         String,
    device_id          String,
    verdict            LowCardinality(String),
    risk_score         Int32,
    risk_level         LowCardinality(String),
    hit_rule_ids       Array(String),
    shadow_rule_ids    Array(String),
    ml_score           Float64,
    ml_model_ver       LowCardinality(String),
    eval_duration_ms   Float64,
    metadata_json      String,
    INDEX idx_pi (pi_id) TYPE bloom_filter GRANULARITY 1,
    INDEX idx_did (decision_id) TYPE bloom_filter GRANULARITY 1
) ENGINE = MergeTree
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (merchant_id, occurred_at)
TTL occurred_at + INTERVAL 730 DAY;
```

## 常用查询

### 商户最近 7 天 verdict 分布
```sql
SELECT toDate(occurred_at) AS d, verdict, count() AS n
FROM risk_decision_audit
WHERE merchant_id = ? AND occurred_at >= now() - INTERVAL 7 DAY
GROUP BY d, verdict
ORDER BY d DESC, verdict;
```

### 异常商户筛查（block_rate 突变）
```sql
SELECT merchant_id,
       countIf(verdict IN ('DENY','REVIEW')) / count() AS block_rate,
       count() AS total
FROM risk_decision_audit
WHERE occurred_at >= now() - INTERVAL 1 HOUR
GROUP BY merchant_id
HAVING total >= 100 AND block_rate > 0.1
ORDER BY block_rate DESC
LIMIT 50;
```

### Cohort 拉链分析（vintage curve）
risk-manage 的 in-mem cohort 端点只能算 ring buffer 范围；CH 跑跨月 vintage
准确得多。详见 `deploy/flink/README.md` 的 `cohort_vintage_curve` 任务。

## Kafka engine 表（替代直连 INSERT）

高峰期写入压力大时，把 Kafka topic `risk.decision.v1` 直接接到 ClickHouse
Kafka engine：
```sql
CREATE TABLE risk_decision_kafka (
    -- 同上字段
) ENGINE = Kafka
SETTINGS
    kafka_broker_list = 'kafka:9092',
    kafka_topic_list = 'risk.decision.v1',
    kafka_group_name = 'ch-risk-decision',
    kafka_format = 'JSONEachRow',
    kafka_num_consumers = 4;

CREATE MATERIALIZED VIEW risk_decision_mv TO risk_decision_audit AS
SELECT * FROM risk_decision_kafka;
```
risk-manage 只发 Kafka，不直接 connect ClickHouse → 写入解耦 + Kafka
天然背压。详见 `deploy/flink/README.md`。

## Chain hash 验证

落 CH 后用 SQL 跑 chain verify（取代 in-mem `/admin/audit/chain/verify`）：
```sql
SELECT decision_id,
       JSONExtractString(metadata_json, 'chain_prev_hash') AS prev,
       JSONExtractString(metadata_json, 'chain_row_hash')  AS row
FROM risk_decision_audit
ORDER BY occurred_at;
-- caller 自己跑 sha256(prev || canonical_json) 跟 row 对比
```
未来可以提供 `cmd/audit-verify` CLI 直接吃 ClickHouse 流式扫验证。
