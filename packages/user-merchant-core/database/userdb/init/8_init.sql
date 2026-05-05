SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `user_merchant_db_8` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `user_merchant_db_8`;

-- 分片表 schema 模板。8 和 80 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   80  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 80)';

CREATE TABLE IF NOT EXISTS `user_profiles_80` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 80)';

CREATE TABLE IF NOT EXISTS `user_auths_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 80)';

CREATE TABLE IF NOT EXISTS `login_logs_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 80)';

CREATE TABLE IF NOT EXISTS `user_sessions_80` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 80)';

CREATE TABLE IF NOT EXISTS `user_roles_80` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 80)';

CREATE TABLE IF NOT EXISTS `user_accounts_80` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 80)';

CREATE TABLE IF NOT EXISTS `user_settings_80` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 80)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 80)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 80)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 80)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 80)';

-- 分片表 schema 模板。8 和 81 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   81  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 81)';

CREATE TABLE IF NOT EXISTS `user_profiles_81` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 81)';

CREATE TABLE IF NOT EXISTS `user_auths_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 81)';

CREATE TABLE IF NOT EXISTS `login_logs_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 81)';

CREATE TABLE IF NOT EXISTS `user_sessions_81` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 81)';

CREATE TABLE IF NOT EXISTS `user_roles_81` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 81)';

CREATE TABLE IF NOT EXISTS `user_accounts_81` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 81)';

CREATE TABLE IF NOT EXISTS `user_settings_81` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 81)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 81)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 81)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 81)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 81)';

-- 分片表 schema 模板。8 和 82 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   82  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 82)';

CREATE TABLE IF NOT EXISTS `user_profiles_82` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 82)';

CREATE TABLE IF NOT EXISTS `user_auths_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 82)';

CREATE TABLE IF NOT EXISTS `login_logs_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 82)';

CREATE TABLE IF NOT EXISTS `user_sessions_82` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 82)';

CREATE TABLE IF NOT EXISTS `user_roles_82` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 82)';

CREATE TABLE IF NOT EXISTS `user_accounts_82` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 82)';

CREATE TABLE IF NOT EXISTS `user_settings_82` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 82)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 82)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 82)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 82)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 82)';

-- 分片表 schema 模板。8 和 83 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   83  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 83)';

CREATE TABLE IF NOT EXISTS `user_profiles_83` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 83)';

CREATE TABLE IF NOT EXISTS `user_auths_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 83)';

CREATE TABLE IF NOT EXISTS `login_logs_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 83)';

CREATE TABLE IF NOT EXISTS `user_sessions_83` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 83)';

CREATE TABLE IF NOT EXISTS `user_roles_83` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 83)';

CREATE TABLE IF NOT EXISTS `user_accounts_83` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 83)';

CREATE TABLE IF NOT EXISTS `user_settings_83` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 83)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 83)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 83)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 83)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 83)';

-- 分片表 schema 模板。8 和 84 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   84  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 84)';

CREATE TABLE IF NOT EXISTS `user_profiles_84` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 84)';

CREATE TABLE IF NOT EXISTS `user_auths_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 84)';

CREATE TABLE IF NOT EXISTS `login_logs_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 84)';

CREATE TABLE IF NOT EXISTS `user_sessions_84` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 84)';

CREATE TABLE IF NOT EXISTS `user_roles_84` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 84)';

CREATE TABLE IF NOT EXISTS `user_accounts_84` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 84)';

CREATE TABLE IF NOT EXISTS `user_settings_84` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 84)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 84)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 84)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 84)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 84)';

-- 分片表 schema 模板。8 和 85 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   85  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 85)';

CREATE TABLE IF NOT EXISTS `user_profiles_85` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 85)';

CREATE TABLE IF NOT EXISTS `user_auths_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 85)';

CREATE TABLE IF NOT EXISTS `login_logs_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 85)';

CREATE TABLE IF NOT EXISTS `user_sessions_85` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 85)';

CREATE TABLE IF NOT EXISTS `user_roles_85` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 85)';

CREATE TABLE IF NOT EXISTS `user_accounts_85` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 85)';

CREATE TABLE IF NOT EXISTS `user_settings_85` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 85)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 85)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 85)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 85)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 85)';

-- 分片表 schema 模板。8 和 86 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   86  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 86)';

CREATE TABLE IF NOT EXISTS `user_profiles_86` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 86)';

CREATE TABLE IF NOT EXISTS `user_auths_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 86)';

CREATE TABLE IF NOT EXISTS `login_logs_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 86)';

CREATE TABLE IF NOT EXISTS `user_sessions_86` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 86)';

CREATE TABLE IF NOT EXISTS `user_roles_86` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 86)';

CREATE TABLE IF NOT EXISTS `user_accounts_86` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 86)';

CREATE TABLE IF NOT EXISTS `user_settings_86` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 86)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 86)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 86)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 86)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 86)';

-- 分片表 schema 模板。8 和 87 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   87  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 87)';

CREATE TABLE IF NOT EXISTS `user_profiles_87` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 87)';

CREATE TABLE IF NOT EXISTS `user_auths_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 87)';

CREATE TABLE IF NOT EXISTS `login_logs_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 87)';

CREATE TABLE IF NOT EXISTS `user_sessions_87` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 87)';

CREATE TABLE IF NOT EXISTS `user_roles_87` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 87)';

CREATE TABLE IF NOT EXISTS `user_accounts_87` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 87)';

CREATE TABLE IF NOT EXISTS `user_settings_87` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 87)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 87)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 87)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 87)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 87)';

-- 分片表 schema 模板。8 和 88 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   88  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 88)';

CREATE TABLE IF NOT EXISTS `user_profiles_88` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 88)';

CREATE TABLE IF NOT EXISTS `user_auths_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 88)';

CREATE TABLE IF NOT EXISTS `login_logs_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 88)';

CREATE TABLE IF NOT EXISTS `user_sessions_88` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 88)';

CREATE TABLE IF NOT EXISTS `user_roles_88` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 88)';

CREATE TABLE IF NOT EXISTS `user_accounts_88` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 88)';

CREATE TABLE IF NOT EXISTS `user_settings_88` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 88)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 88)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 88)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 88)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 88)';

-- 分片表 schema 模板。8 和 89 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   89  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 89)';

CREATE TABLE IF NOT EXISTS `user_profiles_89` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 89)';

CREATE TABLE IF NOT EXISTS `user_auths_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 89)';

CREATE TABLE IF NOT EXISTS `login_logs_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 89)';

CREATE TABLE IF NOT EXISTS `user_sessions_89` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 89)';

CREATE TABLE IF NOT EXISTS `user_roles_89` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 89)';

CREATE TABLE IF NOT EXISTS `user_accounts_89` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 89)';

CREATE TABLE IF NOT EXISTS `user_settings_89` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 89)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 89)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 89)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 89)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 89)';

