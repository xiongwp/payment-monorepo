# risk-manage Flink jobs

风控系统的实时聚合任务。risk-manage 通过 `audit.KafkaSink` 把每笔 decision
推到 Kafka topic `risk.decision.v1`；Flink 任务消费这个 topic 计算需要长
窗口 / 跨 record 关联的指标，结果写回 risk-manage feature store 形成闭环。

## 接入路径

```
risk-manage (audit.KafkaSink → kafka.Writer)
       │
       ▼
Kafka  risk.decision.v1   (key=merchant_id 分区，retention 14d)
       │
       ├─→ ClickHouse Kafka engine    (历史 OLAP 查询)
       │
       └─→ Flink jobs                 (实时聚合)
                  │
                  ▼
        feature.update topic + 直接写 risk-manage feature_store
```

## 推荐的 Flink 任务

### 1. fraud_rate_per_merchant_5min

滑动 5min 窗口，按 `merchant_id` 聚合 `verdict` 分布 + outcome label join。
输出 `merchant_fraud_rate` 给 cohort 监控告警。

**SQL（Flink Table API）：**

```sql
CREATE TABLE risk_decision (
  decision_id      STRING,
  occurred_at      TIMESTAMP_LTZ(3),
  merchant_id      STRING,
  verdict          STRING,
  ml_score         DOUBLE,
  WATERMARK FOR occurred_at AS occurred_at - INTERVAL '5' SECOND
) WITH (
  'connector' = 'kafka',
  'topic' = 'risk.decision.v1',
  'properties.bootstrap.servers' = 'kafka:9092',
  'properties.group.id' = 'flink-fraud-rate',
  'format' = 'json',
  'json.timestamp-format.standard' = 'ISO-8601'
);

SELECT
    merchant_id,
    TUMBLE_START(occurred_at, INTERVAL '5' MINUTE) AS bucket_start,
    COUNT(*) AS total,
    SUM(CASE WHEN verdict IN ('DENY', 'REVIEW') THEN 1 ELSE 0 END) AS blocked,
    AVG(ml_score) AS avg_ml_score
FROM risk_decision
GROUP BY merchant_id, TUMBLE(occurred_at, INTERVAL '5' MINUTE);
```

输出可写回 `risk.merchant_metrics.v1` topic 让 risk-manage 后续的
`/admin/dashboard/cohort` 端点消费。

### 2. device_velocity（cross-merchant）

同 device 5min 内多 merchant → 卡测试 (card testing) 攻击信号。

```sql
SELECT device_id,
       TUMBLE_START(occurred_at, INTERVAL '5' MINUTE) AS bucket,
       COUNT(DISTINCT merchant_id) AS merchant_count,
       COUNT(*) AS total
FROM risk_decision
WHERE device_id <> ''
GROUP BY device_id, TUMBLE(occurred_at, INTERVAL '5' MINUTE)
HAVING COUNT(DISTINCT merchant_id) >= 3;
```

输出回写到 `risk.device_velocity.v1` → risk-manage 加规则
"device 5min 跨 3+ merchant → REVIEW"。

### 3. ip_geo_anomaly

同 IP 多个 country → VPN 切换 / 跳板机。

```sql
SELECT ip,
       TUMBLE_START(occurred_at, INTERVAL '1' HOUR) AS bucket,
       COUNT(DISTINCT country) AS country_count
FROM risk_decision
WHERE ip <> ''
GROUP BY ip, TUMBLE(occurred_at, INTERVAL '1' HOUR)
HAVING COUNT(DISTINCT country) >= 3;
```

### 4. cohort_vintage_curve（按月 cohort 画图）

每个 customer 的首次交易月 = vintage；M+1, M+2, ... 月的 fraud_rate 拉链。
用于 30/60/90 天 chargeback rate 趋势。

```sql
WITH first_seen AS (
  SELECT customer_id, MIN(occurred_at) AS first_at
  FROM risk_decision
  WHERE customer_id <> ''
  GROUP BY customer_id
),
labeled AS (
  -- join risk_outcome topic (dispute / merchant_confirm / review_human)
  ...
)
SELECT DATE_TRUNC('month', first_at) AS vintage,
       DATE_TRUNC('month', occurred_at) AS observe_month,
       COUNT(*) AS total,
       SUM(CASE WHEN is_fraud THEN 1 ELSE 0 END) AS fraud
FROM ...
GROUP BY 1, 2;
```

## 部署

本目录预留 `jobs/` 子目录给具体 Flink job 实现（Java / Scala / PyFlink）。
risk-manage 这个 repo 只维护 Kafka schema 跟消费端的契约 (`internal/audit/
kafka_sink.go` 里的 `kafkaPayload` struct)；具体 Flink 作业代码生产应放
独立 ML / data 仓库管理。

## Schema 演进

`kafka_sink.go` 里的 `kafkaPayload` 字段目前是稳定 v1。新增字段必须：
1. JSON `omitempty` 让旧 Flink job 兼容
2. topic 仍叫 `risk.decision.v1`
3. breaking change → 升 v2 topic + 双写一段时间

## 监控

- `risk_audit_async_dropped_total{}` > 0 → Kafka producer 跟不上 → 调大
  AsyncBatchSink queueSize / batchSize
- Kafka consumer lag > 5min → Flink job 跟不上 → 扩并发 or 拆窗口
- ClickHouse `risk_decision_audit` 表行数 vs Kafka topic offset 总和差异
  > 0.1% → 双写不一致 → 调查
