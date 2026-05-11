-- billing-system 数据库 schema.
--
-- 4 张核心表 + 索引。dev 单库；生产按 merchant_id hash 分 10 库
-- (billing_db_0..9)。
--
-- ⚠️ 钱相关字段（amount / fee）全用 BIGINT minor unit (cents/centavos)，
--    不用 FLOAT/DECIMAL 防精度丢失。

CREATE DATABASE IF NOT EXISTS billing_db DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE billing_db;

-- ─── fee_rule ────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS fee_rule (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    name            VARCHAR(128) NOT NULL,
    priority        INT NOT NULL DEFAULT 0,
    merchant_id     VARCHAR(64) NOT NULL DEFAULT '',
    merchant_tier   VARCHAR(32) NOT NULL DEFAULT '',
    product         VARCHAR(64) NOT NULL DEFAULT '',
    channel_adapter VARCHAR(64) NOT NULL DEFAULT '',
    region          VARCHAR(8) NOT NULL DEFAULT '',
    currency_allow  VARCHAR(128) NOT NULL DEFAULT '',  -- CSV "USD,PHP,SGD"
    card_bin_range  VARCHAR(255) NOT NULL DEFAULT '',  -- "400000-499999;510000-559999"
    amount_min_minor BIGINT NOT NULL DEFAULT 0,
    amount_max_minor BIGINT NOT NULL DEFAULT 0,
    percent_bps     INT NOT NULL DEFAULT 0,            -- basis points
    fixed_minor     BIGINT NOT NULL DEFAULT 0,
    fee_min_minor   BIGINT NOT NULL DEFAULT 0,
    fee_max_minor   BIGINT NOT NULL DEFAULT 0,
    fx_markup_bps   INT NOT NULL DEFAULT 0,
    refund_fee_behavior VARCHAR(16) NOT NULL DEFAULT 'keep',  -- 'refund'|'keep'|'prorate'
    effective_from  DATETIME(3) NOT NULL,
    effective_to    DATETIME(3) NULL,
    active          TINYINT(1) NOT NULL DEFAULT 1,
    created_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_priority_active (priority DESC, active),
    KEY idx_merchant (merchant_id),
    KEY idx_effective (effective_from, effective_to)
) ENGINE=InnoDB;

-- ─── fee_event ───────────────────────────────────────────────────────
-- 钱命中的核心表。按 merchant_id 分库；表内按 occurred_at 月度分区（年终归档 ClickHouse）。
CREATE TABLE IF NOT EXISTS fee_event (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    merchant_id     VARCHAR(64) NOT NULL,
    event_type      VARCHAR(32) NOT NULL,             -- 'charge'|'refund'|'chargeback'|'adjustment'|'fx_spread'
    ref_id          VARCHAR(128) NOT NULL,            -- pi_id / charge_id / refund_id
    ref_service     VARCHAR(32) NOT NULL,             -- 'order-core'|'payment-channel'|...
    gross_amount_minor BIGINT NOT NULL,
    currency        VARCHAR(8) NOT NULL,
    fee_minor       BIGINT NOT NULL,
    fee_minor_base  BIGINT NOT NULL DEFAULT 0,        -- 折算到 base currency
    fx_rate         DECIMAL(20,8) NOT NULL DEFAULT 1.0,
    rule_id         BIGINT UNSIGNED NOT NULL DEFAULT 0,
    rule_name       VARCHAR(128) NOT NULL DEFAULT '',
    product         VARCHAR(64) NOT NULL DEFAULT '',
    channel_adapter VARCHAR(64) NOT NULL DEFAULT '',
    region          VARCHAR(8) NOT NULL DEFAULT '',
    status          VARCHAR(16) NOT NULL DEFAULT 'pending',  -- 'pending'|'settled'|'void'
    statement_id    BIGINT UNSIGNED NULL,             -- 归属哪期账单
    trace_id        VARCHAR(64) NOT NULL DEFAULT '',
    occurred_at     DATETIME(3) NOT NULL,
    created_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_idempotent (merchant_id, ref_id, event_type),
    KEY idx_merchant_occurred (merchant_id, occurred_at DESC),
    KEY idx_statement (statement_id),
    KEY idx_status_occurred (status, occurred_at)
) ENGINE=InnoDB;

-- 按月分区（生产建议加 — 加速归档 / DROP PARTITION 删旧数据）：
-- ALTER TABLE fee_event PARTITION BY RANGE (TO_DAYS(occurred_at)) (
--   PARTITION p2026_01 VALUES LESS THAN (TO_DAYS('2026-02-01')),
--   PARTITION p2026_02 VALUES LESS THAN (TO_DAYS('2026-03-01')),
--   ...
-- );

-- ─── statement ───────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS statement (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    merchant_id     VARCHAR(64) NOT NULL,
    period_start    DATETIME(3) NOT NULL,
    period_end      DATETIME(3) NOT NULL,
    currency        VARCHAR(8) NOT NULL,
    total_gross_minor BIGINT NOT NULL DEFAULT 0,
    total_fee_minor BIGINT NOT NULL DEFAULT 0,
    total_refund_minor BIGINT NOT NULL DEFAULT 0,
    total_chargeback_minor BIGINT NOT NULL DEFAULT 0,
    net_payout_minor BIGINT NOT NULL DEFAULT 0,
    event_count     INT NOT NULL DEFAULT 0,
    status          VARCHAR(16) NOT NULL DEFAULT 'draft',   -- 'draft'|'final'|'paid'|'void'
    pdf_url         VARCHAR(512) NOT NULL DEFAULT '',
    issued_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    paid_at         DATETIME(3) NULL,
    payout_id       BIGINT UNSIGNED NOT NULL DEFAULT 0,
    PRIMARY KEY (id),
    UNIQUE KEY uk_period (merchant_id, period_start, period_end, currency),
    KEY idx_merchant_status (merchant_id, status, issued_at DESC),
    KEY idx_status_issued (status, issued_at)
) ENGINE=InnoDB;

-- ─── adjustment ──────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS adjustment (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    merchant_id     VARCHAR(64) NOT NULL,
    amount_minor    BIGINT NOT NULL,                  -- 正=补给商户，负=扣商户
    currency        VARCHAR(8) NOT NULL,
    reason          VARCHAR(255) NOT NULL,
    created_by      VARCHAR(64) NOT NULL,
    approved_by     VARCHAR(64) NOT NULL DEFAULT '',
    statement_id    BIGINT UNSIGNED NULL,
    created_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_merchant_created (merchant_id, created_at DESC),
    KEY idx_statement (statement_id)
) ENGINE=InnoDB;

-- ─── recon_cdc 用户（reconplatform 订 binlog 用，与其它 shard 一致） ──
CREATE USER IF NOT EXISTS 'recon_cdc'@'%' IDENTIFIED WITH mysql_native_password BY 'recon_cdc_pwd';
ALTER USER 'recon_cdc'@'%' IDENTIFIED WITH mysql_native_password BY 'recon_cdc_pwd';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'recon_cdc'@'%';
GRANT SELECT ON billing_db.* TO 'recon_cdc'@'%';
FLUSH PRIVILEGES;

-- ─── billing app 用户 (业务读写) ──────────────────────────────────────
CREATE USER IF NOT EXISTS 'billing_app'@'%' IDENTIFIED WITH mysql_native_password BY 'billing_app_pwd';
ALTER USER 'billing_app'@'%' IDENTIFIED WITH mysql_native_password BY 'billing_app_pwd';
GRANT SELECT, INSERT, UPDATE, DELETE ON billing_db.* TO 'billing_app'@'%';
FLUSH PRIVILEGES;
