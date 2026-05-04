-- Migration bundle for already-initialized `order_meta` databases that were
-- bootstrapped before waves F/B/C/H/D/G landed.
--
-- Everything is `CREATE TABLE IF NOT EXISTS` so running it multiple times
-- (or against a fresh DB that already has the tables) is a no-op.
--
-- Apply:
--   mysql -uroot -p order_meta < database/metadb/migrations/001_waves_F_B_C_H_D_G.sql
--
-- Or from docker-compose stack:
--   docker exec -i paychan-meta mysql -uroot -ppassword order_meta \
--     < database/metadb/migrations/001_waves_F_B_C_H_D_G.sql

USE `order_meta`;

-- wave F (商户 onboarding + KYC)
CREATE TABLE IF NOT EXISTS `merchants` (
    `id`                VARCHAR(32)  NOT NULL,
    `name`              VARCHAR(128) NOT NULL,
    `legal_name`        VARCHAR(256) DEFAULT NULL,
    `country`           CHAR(2)      NOT NULL DEFAULT 'PH',
    `business_type`     VARCHAR(16)  NOT NULL DEFAULT 'individual',
    `tax_id`            VARCHAR(64)  DEFAULT NULL,
    `contact_email`     VARCHAR(128) NOT NULL,
    `contact_phone`     VARCHAR(32)  DEFAULT NULL,
    `website`           VARCHAR(256) DEFAULT NULL,
    `mcc`               VARCHAR(8)   DEFAULT NULL,
    `live_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `test_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `webhook_url`       VARCHAR(512) NOT NULL DEFAULT '',
    `webhook_secret`    VARCHAR(64)  NOT NULL DEFAULT '',
    `kyc_status`        VARCHAR(24)  NOT NULL DEFAULT 'pending',
    `kyc_level`         TINYINT      NOT NULL DEFAULT 0,
    `kyc_reason`        VARCHAR(512) DEFAULT NULL,
    `kyc_reviewer`      VARCHAR(64)  DEFAULT NULL,
    `kyc_reviewed_at`   DATETIME(3)  DEFAULT NULL,
    `risk_tier`         VARCHAR(16)  NOT NULL DEFAULT 'standard',
    `rate_limit_rps`    INT          NOT NULL DEFAULT 100,
    `settle_currency`   CHAR(3)      NOT NULL DEFAULT 'PHP',
    `settle_method`     VARCHAR(16)  DEFAULT NULL,
    `settle_account`    VARCHAR(128) DEFAULT NULL,
    `settle_bank`       VARCHAR(128) DEFAULT NULL,
    `settle_holder`     VARCHAR(128) DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `metadata`          JSON         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document` (
    `id`              VARCHAR(32)  NOT NULL,
    `merchant_id`     VARCHAR(32)  NOT NULL,
    `doc_type`        VARCHAR(32)  NOT NULL,
    `doc_number`      VARCHAR(128) DEFAULT NULL,
    `file_url`        VARCHAR(512) NOT NULL,
    `mime_type`       VARCHAR(64)  DEFAULT NULL,
    `size_bytes`      INT          DEFAULT NULL,
    `uploaded_by`     VARCHAR(64)  DEFAULT NULL,
    `review_status`   VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `review_note`     VARCHAR(512) DEFAULT NULL,
    `expires_at`      DATETIME(3)  DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant`      (`merchant_id`),
    KEY `idx_review_status` (`review_status`),
    KEY `idx_doc_type`      (`doc_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档';

CREATE TABLE IF NOT EXISTS `merchant_kyc_audit` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`    VARCHAR(32)  NOT NULL,
    `from_status`    VARCHAR(24)  NOT NULL,
    `to_status`      VARCHAR(24)  NOT NULL,
    `reason`         VARCHAR(512) DEFAULT NULL,
    `actor`          VARCHAR(64)  DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant` (`merchant_id`),
    KEY `idx_created`  (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 状态流转日志';

-- wave C (outbound webhook deliveries)
CREATE TABLE IF NOT EXISTS `webhook_deliveries` (
    `id`               BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`      VARCHAR(32)  NOT NULL,
    `event_id`         VARCHAR(64)  NOT NULL,
    `event_type`       VARCHAR(64)  NOT NULL,
    `payload`          JSON         NOT NULL,
    `url`              VARCHAR(512) NOT NULL,
    `status`           VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `http_status`      INT          NOT NULL DEFAULT 0,
    `attempts`         INT          NOT NULL DEFAULT 0,
    `max_attempts`     INT          NOT NULL DEFAULT 5,
    `next_retry_at`    DATETIME              DEFAULT NULL,
    `last_error`       TEXT                  DEFAULT NULL,
    `created_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_event` (`event_id`),
    INDEX `idx_merchant_status` (`merchant_id`, `status`),
    INDEX `idx_retry` (`status`, `next_retry_at`, `attempts`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Webhook 投递日志';

-- wave B (admin audit log)
CREATE TABLE IF NOT EXISTS `admin_audit_log` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL,
    `actor_ip`       VARCHAR(64)  DEFAULT NULL,
    `action`         VARCHAR(64)  NOT NULL,
    `target_type`    VARCHAR(32)  DEFAULT NULL,
    `target_id`      VARCHAR(64)  DEFAULT NULL,
    `http_method`    VARCHAR(8)   DEFAULT NULL,
    `http_path`      VARCHAR(256) DEFAULT NULL,
    `http_status`    INT          DEFAULT NULL,
    `request_body`   MEDIUMTEXT   DEFAULT NULL,
    `response_code`  VARCHAR(32)  DEFAULT NULL,
    `response_msg`   VARCHAR(512) DEFAULT NULL,
    `duration_ms`    INT          DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_action`  (`action`),
    KEY `idx_target`  (`target_type`, `target_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='管理台审计日志 (append-only)';

-- wave H (double-entry ledger, single-currency PHP)
CREATE TABLE IF NOT EXISTS `gl_account` (
    `id`             VARCHAR(64)  NOT NULL,
    `name`           VARCHAR(128) NOT NULL,
    `type`           VARCHAR(16)  NOT NULL,
    `owner_type`     VARCHAR(16)  NOT NULL,
    `owner_id`       VARCHAR(64)  DEFAULT NULL,
    `currency`       CHAR(3)      NOT NULL DEFAULT 'PHP',
    `debit_balance`  BIGINT       NOT NULL DEFAULT 0,
    `credit_balance` BIGINT       NOT NULL DEFAULT 0,
    `version`        BIGINT       NOT NULL DEFAULT 0,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active',
    `metadata`       JSON         DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_owner`  (`owner_type`, `owner_id`),
    KEY `idx_type`   (`type`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='总账账户';

CREATE TABLE IF NOT EXISTS `gl_transaction` (
    `id`            VARCHAR(64)  NOT NULL,
    `event_type`    VARCHAR(64)  NOT NULL,
    `ref_type`      VARCHAR(32)  DEFAULT NULL,
    `ref_id`        VARCHAR(64)  DEFAULT NULL,
    `total_debit`   BIGINT       NOT NULL,
    `total_credit`  BIGINT       NOT NULL,
    `memo`          VARCHAR(512) DEFAULT NULL,
    `reverses`      VARCHAR(64)  DEFAULT NULL,
    `created`       DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_event_type` (`event_type`),
    KEY `idx_ref`        (`ref_type`, `ref_id`),
    KEY `idx_created`    (`created`),
    CONSTRAINT `chk_balanced` CHECK (`total_debit` = `total_credit`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='总账事务';

CREATE TABLE IF NOT EXISTS `gl_entry` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `txn_id`          VARCHAR(64)  NOT NULL,
    `account_id`      VARCHAR(64)  NOT NULL,
    `debit_amount`    BIGINT       NOT NULL DEFAULT 0,
    `credit_amount`   BIGINT       NOT NULL DEFAULT 0,
    `currency`        CHAR(3)      NOT NULL DEFAULT 'PHP',
    `memo`            VARCHAR(512) DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_txn`     (`txn_id`),
    KEY `idx_account` (`account_id`),
    KEY `idx_created` (`created`),
    CONSTRAINT `chk_entry_side` CHECK (
      (`debit_amount` > 0 AND `credit_amount` = 0)
      OR (`debit_amount` = 0 AND `credit_amount` > 0)
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='总账条目 (append-only)';

-- wave G (multi-tenant merchant channel secrets)
CREATE TABLE IF NOT EXISTS `merchant_channel_secret` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `channel`      VARCHAR(32)  NOT NULL,
    `field_name`   VARCHAR(64)  NOT NULL,
    `ciphertext`   VARBINARY(4096) NOT NULL,
    `context`      VARCHAR(256) NOT NULL,
    `masked_hint`  VARCHAR(64)  DEFAULT NULL,
    `version`      INT          NOT NULL DEFAULT 1,
    `created_by`   VARCHAR(64)  DEFAULT NULL,
    `created`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_merchant_channel_field` (`merchant_id`, `channel`, `field_name`),
    KEY `idx_merchant_channel` (`merchant_id`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据（密文）';
