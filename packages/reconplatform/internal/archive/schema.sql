-- ClickHouse schema for reconplatform diff archive (cold tier)
-- 由 Archiver.EnsureSchema() 自动 apply；这份文件只为留档 + 让 DBA 看到。
--
-- 设计要点：
-- 1. ReplacingMergeTree(updated_at)：同 diff_id 多次归档（rare 但可能）按
--    updated_at 保留最新版。SELECT 时用 FINAL 触发去重 merge。
-- 2. PARTITION BY toYYYYMM(created_at)：每月一个 partition，DROP 旧月很快。
-- 3. ORDER BY (created_at, id)：主键时间排前，跨年时间窗 SELECT 走索引。
-- 4. LowCardinality(String)：script_id / type / state / updated_by 这些枚举
--    类字段压缩比 50:1+，且过滤等值时直接走字典。
-- 5. TTL created_at + INTERVAL 5 YEAR：5 年自动删 — 等保 / SOX 一般要 3-7y。
-- 6. detail_json String：底层 diff.detail 是 dynamic dict，存 JSON 全文，
--    需要查时用 JSONExtract*。如果常查的字段确认了，可以加 MATERIALIZED。

CREATE DATABASE IF NOT EXISTS recon;

CREATE TABLE IF NOT EXISTS recon.recon_diffs_archive (
    id String,
    script_id LowCardinality(String),
    run_id String,
    type LowCardinality(String),
    key String,
    detail_json String,
    state LowCardinality(String),
    created_at DateTime64(3),
    updated_at DateTime64(3),
    updated_by LowCardinality(String),
    note String,
    archived_at DateTime DEFAULT now()
) ENGINE = ReplacingMergeTree(updated_at)
  PARTITION BY toYYYYMM(created_at)
  ORDER BY (created_at, id)
  TTL toDateTime(created_at) + INTERVAL 5 YEAR
  SETTINGS index_granularity = 8192;

-- 常用查询 examples：
--
-- 1) 跨年某脚本所有 diff：
--    SELECT * FROM recon.recon_diffs_archive FINAL
--    WHERE script_id = 'order_payment_match'
--      AND created_at >= '2025-01-01' AND created_at < '2026-01-01'
--    ORDER BY created_at LIMIT 100;
--
-- 2) 月度 diff 数量 + 状态分布：
--    SELECT toYYYYMM(created_at) AS month,
--           state,
--           count() AS c
--    FROM recon.recon_diffs_archive FINAL
--    GROUP BY month, state ORDER BY month, state;
--
-- 3) detail 里某字段筛（如 amount > 10000）：
--    SELECT id, script_id, key,
--           toFloat64OrNull(JSONExtractString(detail_json, 'amount')) AS amt
--    FROM recon.recon_diffs_archive FINAL
--    WHERE created_at >= '2025-11-01'
--      AND amt > 10000
--    ORDER BY amt DESC LIMIT 50;
