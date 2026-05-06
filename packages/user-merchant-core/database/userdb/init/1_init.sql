SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `user_merchant_db_1` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `user_merchant_db_1`;

-- 分片表 schema 模板。1 和 10 由 generate.sh 替换：
--   1     = 0..9         分库 idx
--   10  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_10` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 10)';

CREATE TABLE IF NOT EXISTS `user_profiles_10` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 10)';

CREATE TABLE IF NOT EXISTS `user_auths_10` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 10)';

CREATE TABLE IF NOT EXISTS `login_logs_10` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 10)';

CREATE TABLE IF NOT EXISTS `user_sessions_10` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 10)';

CREATE TABLE IF NOT EXISTS `user_roles_10` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 10)';

CREATE TABLE IF NOT EXISTS `user_accounts_10` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 10)';

CREATE TABLE IF NOT EXISTS `user_settings_10` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 10)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_10` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 10)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_10` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
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
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 10)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_10` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 10)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_10` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 10)';

-- ─── admin_audit_log_10：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_10` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 10';

-- 分片表 schema 模板。1 和 11 由 generate.sh 替换：
--   1     = 0..9         分库 idx
--   11  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_11` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 11)';

CREATE TABLE IF NOT EXISTS `user_profiles_11` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 11)';

CREATE TABLE IF NOT EXISTS `user_auths_11` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 11)';

CREATE TABLE IF NOT EXISTS `login_logs_11` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 11)';

CREATE TABLE IF NOT EXISTS `user_sessions_11` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 11)';

CREATE TABLE IF NOT EXISTS `user_roles_11` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 11)';

CREATE TABLE IF NOT EXISTS `user_accounts_11` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 11)';

CREATE TABLE IF NOT EXISTS `user_settings_11` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 11)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_11` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 11)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_11` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
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
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 11)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_11` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 11)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_11` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 11)';

-- ─── admin_audit_log_11：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_11` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 11';

-- 分片表 schema 模板。1 和 12 由 generate.sh 替换：
--   1     = 0..9         分库 idx
--   12  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_12` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 12)';

CREATE TABLE IF NOT EXISTS `user_profiles_12` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 12)';

CREATE TABLE IF NOT EXISTS `user_auths_12` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 12)';

CREATE TABLE IF NOT EXISTS `login_logs_12` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 12)';

CREATE TABLE IF NOT EXISTS `user_sessions_12` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 12)';

CREATE TABLE IF NOT EXISTS `user_roles_12` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 12)';

CREATE TABLE IF NOT EXISTS `user_accounts_12` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 12)';

CREATE TABLE IF NOT EXISTS `user_settings_12` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 12)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_12` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 12)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_12` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
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
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 12)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_12` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 12)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_12` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 12)';

-- ─── admin_audit_log_12：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_12` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 12';

-- 分片表 schema 模板。1 和 13 由 generate.sh 替换：
--   1     = 0..9         分库 idx
--   13  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_13` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 13)';

CREATE TABLE IF NOT EXISTS `user_profiles_13` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 13)';

CREATE TABLE IF NOT EXISTS `user_auths_13` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 13)';

CREATE TABLE IF NOT EXISTS `login_logs_13` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 13)';

CREATE TABLE IF NOT EXISTS `user_sessions_13` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 13)';

CREATE TABLE IF NOT EXISTS `user_roles_13` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 13)';

CREATE TABLE IF NOT EXISTS `user_accounts_13` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 13)';

CREATE TABLE IF NOT EXISTS `user_settings_13` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 13)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_13` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 13)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_13` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
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
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 13)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_13` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 13)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_13` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 13)';

-- ─── admin_audit_log_13：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_13` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 13';

-- 分片表 schema 模板。1 和 14 由 generate.sh 替换：
--   1     = 0..9         分库 idx
--   14  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_14` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 14)';

CREATE TABLE IF NOT EXISTS `user_profiles_14` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 14)';

CREATE TABLE IF NOT EXISTS `user_auths_14` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 14)';

CREATE TABLE IF NOT EXISTS `login_logs_14` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 14)';

CREATE TABLE IF NOT EXISTS `user_sessions_14` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 14)';

CREATE TABLE IF NOT EXISTS `user_roles_14` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 14)';

CREATE TABLE IF NOT EXISTS `user_accounts_14` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 14)';

CREATE TABLE IF NOT EXISTS `user_settings_14` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 14)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_14` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 14)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_14` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
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
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 14)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_14` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 14)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_14` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 14)';

-- ─── admin_audit_log_14：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_14` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 14';

-- 分片表 schema 模板。1 和 15 由 generate.sh 替换：
--   1     = 0..9         分库 idx
--   15  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_15` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 15)';

CREATE TABLE IF NOT EXISTS `user_profiles_15` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 15)';

CREATE TABLE IF NOT EXISTS `user_auths_15` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 15)';

CREATE TABLE IF NOT EXISTS `login_logs_15` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 15)';

CREATE TABLE IF NOT EXISTS `user_sessions_15` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 15)';

CREATE TABLE IF NOT EXISTS `user_roles_15` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 15)';

CREATE TABLE IF NOT EXISTS `user_accounts_15` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 15)';

CREATE TABLE IF NOT EXISTS `user_settings_15` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 15)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_15` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 15)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_15` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
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
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 15)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_15` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 15)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_15` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 15)';

-- ─── admin_audit_log_15：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_15` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 15';

-- 分片表 schema 模板。1 和 16 由 generate.sh 替换：
--   1     = 0..9         分库 idx
--   16  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_16` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 16)';

CREATE TABLE IF NOT EXISTS `user_profiles_16` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 16)';

CREATE TABLE IF NOT EXISTS `user_auths_16` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 16)';

CREATE TABLE IF NOT EXISTS `login_logs_16` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 16)';

CREATE TABLE IF NOT EXISTS `user_sessions_16` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 16)';

CREATE TABLE IF NOT EXISTS `user_roles_16` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 16)';

CREATE TABLE IF NOT EXISTS `user_accounts_16` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 16)';

CREATE TABLE IF NOT EXISTS `user_settings_16` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 16)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_16` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 16)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_16` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
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
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 16)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_16` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 16)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_16` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 16)';

-- ─── admin_audit_log_16：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_16` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 16';

-- 分片表 schema 模板。1 和 17 由 generate.sh 替换：
--   1     = 0..9         分库 idx
--   17  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_17` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 17)';

CREATE TABLE IF NOT EXISTS `user_profiles_17` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 17)';

CREATE TABLE IF NOT EXISTS `user_auths_17` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 17)';

CREATE TABLE IF NOT EXISTS `login_logs_17` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 17)';

CREATE TABLE IF NOT EXISTS `user_sessions_17` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 17)';

CREATE TABLE IF NOT EXISTS `user_roles_17` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 17)';

CREATE TABLE IF NOT EXISTS `user_accounts_17` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 17)';

CREATE TABLE IF NOT EXISTS `user_settings_17` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 17)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_17` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 17)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_17` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
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
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 17)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_17` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 17)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_17` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 17)';

-- ─── admin_audit_log_17：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_17` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 17';

-- 分片表 schema 模板。1 和 18 由 generate.sh 替换：
--   1     = 0..9         分库 idx
--   18  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_18` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 18)';

CREATE TABLE IF NOT EXISTS `user_profiles_18` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 18)';

CREATE TABLE IF NOT EXISTS `user_auths_18` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 18)';

CREATE TABLE IF NOT EXISTS `login_logs_18` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 18)';

CREATE TABLE IF NOT EXISTS `user_sessions_18` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 18)';

CREATE TABLE IF NOT EXISTS `user_roles_18` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 18)';

CREATE TABLE IF NOT EXISTS `user_accounts_18` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 18)';

CREATE TABLE IF NOT EXISTS `user_settings_18` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 18)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_18` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 18)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_18` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
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
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 18)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_18` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 18)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_18` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 18)';

-- ─── admin_audit_log_18：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_18` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 18';

-- 分片表 schema 模板。1 和 19 由 generate.sh 替换：
--   1     = 0..9         分库 idx
--   19  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_19` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 19)';

CREATE TABLE IF NOT EXISTS `user_profiles_19` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 19)';

CREATE TABLE IF NOT EXISTS `user_auths_19` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 19)';

CREATE TABLE IF NOT EXISTS `login_logs_19` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 19)';

CREATE TABLE IF NOT EXISTS `user_sessions_19` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 19)';

CREATE TABLE IF NOT EXISTS `user_roles_19` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 19)';

CREATE TABLE IF NOT EXISTS `user_accounts_19` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 19)';

CREATE TABLE IF NOT EXISTS `user_settings_19` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 19)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_19` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 19)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_19` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
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
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 19)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_19` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 19)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_19` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 19)';

-- ─── admin_audit_log_19：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_19` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 19';

