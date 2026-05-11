-- refund-engine 数据库 schema.
--
-- 单表 refund，按 merchant_id hash 分库（生产 refund_db_0..9）。
-- 索引：
--   uk_refund_id      RefundID 唯一
--   uk_idempotent     防同一 idempotency_key 重复提交
--   idx_charge        按 charge 查累计退款（refund_excess 检测必需）
--   idx_status_submit Submit cron 用（approved → 通道）

CREATE DATABASE IF NOT EXISTS refund_db DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE refund_db;

CREATE TABLE IF NOT EXISTS refund (
    id                          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    refund_id                   VARCHAR(64) NOT NULL,
    merchant_id                 VARCHAR(64) NOT NULL,
    charge_id                   VARCHAR(128) NOT NULL,
    payment_intent_id           VARCHAR(128) NOT NULL DEFAULT '',
    amount_minor                BIGINT NOT NULL,
    original_charge_amount_minor BIGINT NOT NULL DEFAULT 0,
    already_refunded_minor      BIGINT NOT NULL DEFAULT 0,
    currency                    VARCHAR(8) NOT NULL,
    reason                      VARCHAR(32) NOT NULL,
    reason_note                 VARCHAR(500) NOT NULL DEFAULT '',
    method                      VARCHAR(32) NOT NULL DEFAULT 'original_channel',
    status                      VARCHAR(16) NOT NULL DEFAULT 'requested',
    channel_refund_id           VARCHAR(128) NOT NULL DEFAULT '',
    failure_code                VARCHAR(64) NOT NULL DEFAULT '',
    failure_message             VARCHAR(500) NOT NULL DEFAULT '',
    idempotency_key             VARCHAR(64) NOT NULL,
    requested_by                VARCHAR(64) NOT NULL,
    approved_by                 VARCHAR(64) NOT NULL DEFAULT '',
    requested_at                DATETIME(3) NOT NULL,
    approved_at                 DATETIME(3) NULL,
    submitted_at                DATETIME(3) NULL,
    completed_at                DATETIME(3) NULL,
    trace_id                    VARCHAR(64) NOT NULL DEFAULT '',
    created_at                  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at                  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_refund_id (refund_id),
    UNIQUE KEY uk_idempotent (idempotency_key),
    KEY idx_charge (charge_id),
    KEY idx_status_submit (status, requested_at) COMMENT 'Submit cron 用',
    KEY idx_merchant_created (merchant_id, created_at DESC)
) ENGINE=InnoDB;

-- recon_cdc 用户（一致）
CREATE USER IF NOT EXISTS 'recon_cdc'@'%' IDENTIFIED WITH mysql_native_password BY 'recon_cdc_pwd';
ALTER USER 'recon_cdc'@'%' IDENTIFIED WITH mysql_native_password BY 'recon_cdc_pwd';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'recon_cdc'@'%';
GRANT SELECT ON refund_db.* TO 'recon_cdc'@'%';

CREATE USER IF NOT EXISTS 'refund_app'@'%' IDENTIFIED WITH mysql_native_password BY 'refund_app_pwd';
ALTER USER 'refund_app'@'%' IDENTIFIED WITH mysql_native_password BY 'refund_app_pwd';
GRANT SELECT, INSERT, UPDATE ON refund_db.* TO 'refund_app'@'%';
FLUSH PRIVILEGES;
