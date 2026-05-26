-- risk-manage ClickHouse 初始 schema。docker-compose CH 容器启动时
-- 自动执行 (clickhouse-server entrypoint 跑 /docker-entrypoint-initdb.d/*).

CREATE DATABASE IF NOT EXISTS risk;

-- ── 决策审计主表 (risk-manage 直连 INSERT，跟 Kafka engine 表双写) ─
CREATE TABLE IF NOT EXISTS risk.risk_decision_audit (
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
-- TTL 表达式必须返回 Date / DateTime；DateTime64 在 CH 24.3+ 严格校验下不再隐式收敛。
-- toDate() 把纳秒精度降到天级；TTL 本来就是按天淘汰，精度足够。
TTL toDate(occurred_at) + INTERVAL 730 DAY;

-- ── Kafka engine 表 (从 risk.decision.v1 topic 拉 + materialize 进主表) ─
-- 高峰期写入解耦；生产可只走这条路径不让 risk-manage 直连 CH。
CREATE TABLE IF NOT EXISTS risk.risk_decision_kafka (
    decision_id        String,
    occurred_at        String,  -- Kafka 消息里是 RFC3339；MV 转 DateTime
    merchant_id        String,
    customer_id        String,
    payment_intent_id  String,
    amount             Int64,
    currency           String,
    payment_method     String,
    country            String,
    ip                 String,
    device_id          String,
    verdict            String,
    risk_score         Int32,
    risk_level         String,
    ml_score           Float64,
    ml_model_ver       String,
    hit_rule_ids       Array(String),
    shadow_rule_ids    Array(String),
    eval_duration_ms   Float64
) ENGINE = Kafka
SETTINGS
    kafka_broker_list = 'kafka:9092',
    kafka_topic_list = 'risk.decision.v1',
    kafka_group_name = 'ch-risk-decision',
    kafka_format = 'JSONEachRow',
    kafka_num_consumers = 4,
    kafka_skip_broken_messages = 100;

CREATE MATERIALIZED VIEW IF NOT EXISTS risk.risk_decision_kafka_mv
TO risk.risk_decision_audit AS
SELECT
    decision_id,
    parseDateTime64BestEffort(occurred_at) AS occurred_at,
    0 AS rule_version,
    payment_intent_id AS pi_id,
    merchant_id,
    customer_id,
    amount,
    currency,
    payment_method,
    country,
    ip AS ip_address,
    device_id,
    verdict,
    risk_score,
    risk_level,
    hit_rule_ids,
    shadow_rule_ids,
    ml_score,
    ml_model_ver,
    eval_duration_ms,
    '' AS metadata_json
FROM risk.risk_decision_kafka;

-- ── 常用 view: per-merchant 5min fraud_rate ──────────────────────
CREATE VIEW IF NOT EXISTS risk.merchant_fraud_5min AS
SELECT
    merchant_id,
    toStartOfFiveMinute(occurred_at) AS bucket,
    count() AS total,
    countIf(verdict IN ('DENY', 'REVIEW')) AS blocked,
    blocked / total AS block_rate,
    avg(ml_score) AS avg_ml_score
FROM risk.risk_decision_audit
WHERE occurred_at >= now() - INTERVAL 1 DAY
GROUP BY merchant_id, bucket
ORDER BY merchant_id, bucket DESC;
