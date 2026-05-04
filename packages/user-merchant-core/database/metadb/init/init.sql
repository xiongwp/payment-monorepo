-- user_merchant_meta：非分片，存放商户主数据 / KYC / 渠道凭据 / Leaf Segment idgen 元数据
CREATE DATABASE IF NOT EXISTS `user_merchant_meta` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `user_merchant_meta`;

-- Leaf 号段 ID 分配表
CREATE TABLE IF NOT EXISTS `leaf_alloc` (
    `biz_tag`     VARCHAR(128) NOT NULL DEFAULT '',
    `max_id`      BIGINT       NOT NULL DEFAULT 1,
    `step`        INT          NOT NULL DEFAULT 100000,
    `description` VARCHAR(256) DEFAULT NULL,
    `update_time` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`biz_tag`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Leaf 号段 ID 分配表';
-- 服务启动时 idgen.New 会幂等注册 user_merchant.merchant / user_merchant.kyc_document /
-- user_merchant.user。User id 段必须从 1e8 起：accounting-system 把 [1e8, 9e8)
-- 保留给 user owner_id，低于 1e8 视作平台 / 系统账户（CreatePlatformAccount 才能开）。
-- 旧库 idgen.New 用 FirstOrCreate 已经写过 max_id=1_000_000 的 stale 行 → 升级时这条
-- INSERT IGNORE 不生效，所以再加一条 UPDATE 兜底，把过低的 max_id 抬到 1e8。
INSERT IGNORE INTO `leaf_alloc` (`biz_tag`, `max_id`, `step`, `description`) VALUES
    ('user_merchant.user',         100000000, 100000, 'User id (≥1e8 reserved by accounting)'),
    ('user_merchant.merchant',           1000, 100000, 'Merchant id'),
    ('user_merchant.kyc_document',       1000, 100000, 'Merchant KYC document id');
UPDATE `leaf_alloc` SET `max_id` = 100000000
    WHERE `biz_tag` = 'user_merchant.user' AND `max_id` < 100000000;

-- ─── 商户 (PSP merchant onboarding + KYC + API auth + outbound webhook) ──────
-- 单库非分片：商户量级（万级）远小于交易。每笔 PI/Charge/Refund 都按 mch_id
-- 先查配置 → 放 meta 库便于全局缓存 + 主从读写分离。
CREATE TABLE IF NOT EXISTS `merchants` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx',

    -- 基础信息
    `name`              VARCHAR(128) NOT NULL COMMENT '商户展示名',
    `legal_name`        VARCHAR(256) DEFAULT NULL COMMENT '注册主体全称',
    `country`           CHAR(2)      NOT NULL DEFAULT 'PH',
    `business_type`     VARCHAR(16)  NOT NULL DEFAULT 'individual' COMMENT 'individual/corporate/non_profit/government',
    `tax_id`            VARCHAR(64)  DEFAULT NULL COMMENT 'PH TIN / SEC reg # / DTI #',
    `contact_email`     VARCHAR(128) NOT NULL,
    `contact_phone`     VARCHAR(32)  DEFAULT NULL,
    `website`           VARCHAR(256) DEFAULT NULL,
    `mcc`               VARCHAR(8)   DEFAULT NULL COMMENT 'merchant category code (ISO 18245)',

    -- API 鉴权（Stripe 风格 live/test 双钥）。只存 hash，明文只在签发时返回一次。
    `live_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'SHA-256(sk_live_xxx)',
    `test_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'SHA-256(sk_test_xxx)',

    -- 出站 webhook 配置
    `webhook_url`       VARCHAR(512) NOT NULL DEFAULT '' COMMENT '商户接收我方 webhook 的 URL',
    `webhook_secret`    VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'HMAC-SHA256 签名 key',

    -- KYC 状态机
    `kyc_status`        VARCHAR(24)  NOT NULL DEFAULT 'pending'
                         COMMENT 'pending/submitted/reviewing/needs_more_info/approved/rejected/suspended/terminated',
    `kyc_level`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0=none / 1=basic / 2=full / 3=enhanced (CDD/EDD)',
    `kyc_reason`        VARCHAR(512) DEFAULT NULL COMMENT '当前状态原因（拒绝原因/需补项）',
    `kyc_reviewer`      VARCHAR(64)  DEFAULT NULL COMMENT '审核人 ID',
    `kyc_reviewed_at`   DATETIME(3)  DEFAULT NULL,

    -- 风控分层
    `risk_tier`         VARCHAR(16)  NOT NULL DEFAULT 'standard' COMMENT 'standard/elevated/high/restricted',
    `rate_limit_rps`    INT          NOT NULL DEFAULT 100,

    -- 结算配置
    `settle_currency`   CHAR(3)      NOT NULL DEFAULT 'PHP',
    `settle_method`     VARCHAR(16)  DEFAULT NULL COMMENT 'bank_transfer / wallet_topup / manual',
    `settle_account`    VARCHAR(128) DEFAULT NULL COMMENT '收款账户（bank# / GCash mobile）',
    `settle_bank`       VARCHAR(128) DEFAULT NULL,
    `settle_holder`     VARCHAR(128) DEFAULT NULL,

    -- 业务可用状态（与 KYC 解耦）
    `status`            VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/active/suspended/terminated',

    `metadata`          JSON         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    -- 软删：Terminate → 置 deleted_at；retention sweeper 在 deleted_at + N 天后真删。
    -- GDPR 导出 / 合规审计期间仍可 SELECT WITH deleted。
    `deleted_at`        DATETIME(3)  DEFAULT NULL,

    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表';

-- KYC 文档（身份证 / SEC / DTI / 法人代表 ID / Bank statement 等）
-- 文档正文走对象存储，本表只存元信息 + 引用。
CREATE TABLE IF NOT EXISTS `merchant_kyc_document` (
    `id`              VARCHAR(32)  NOT NULL,
    `merchant_id`     VARCHAR(32)  NOT NULL,
    `doc_type`        VARCHAR(32)  NOT NULL COMMENT 'gov_id / sec_registration / dti / bir_2303 / bank_statement / utility_bill / authorization / proof_of_address / other',
    `doc_number`      VARCHAR(128) DEFAULT NULL COMMENT '证件编号',
    `file_url`        VARCHAR(512) NOT NULL COMMENT '对象存储 URL',
    `mime_type`       VARCHAR(64)  DEFAULT NULL,
    `size_bytes`      INT          DEFAULT NULL,
    `uploaded_by`     VARCHAR(64)  DEFAULT NULL COMMENT '商户 user / admin reviewer',
    `review_status`   VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/accepted/rejected/expired',
    `review_note`     VARCHAR(512) DEFAULT NULL,
    `expires_at`      DATETIME(3)  DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant`      (`merchant_id`),
    KEY `idx_review_status` (`review_status`),
    KEY `idx_doc_type`      (`doc_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档';

-- KYC 状态流转日志（every status change）
CREATE TABLE IF NOT EXISTS `merchant_kyc_audit` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`    VARCHAR(32)  NOT NULL,
    `from_status`    VARCHAR(24)  NOT NULL,
    `to_status`      VARCHAR(24)  NOT NULL,
    `reason`         VARCHAR(512) DEFAULT NULL,
    `actor`          VARCHAR(64)  DEFAULT NULL COMMENT 'admin user id / system',
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant` (`merchant_id`),
    KEY `idx_created`  (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 状态流转日志';

-- ─── 幂等键（Stripe 风格 Idempotency-Key）────────────────────────────────────
-- 所有 mutation RPC（Create / RotateApiKey / SubmitKyc / ... / PutSecret）在
-- 网关侧用 header 附一个 key；第一次请求把 (key, request_hash) 保存并记录响应，
-- 后续 24h 内带同样 key 的请求直接返回之前的响应，不再下游执行。
-- request_hash 防止"同 key 换 body"的重放攻击（Stripe 也这么做）。
CREATE TABLE IF NOT EXISTS `idempotency_key` (
    `idempotency_key`  VARCHAR(128) NOT NULL COMMENT 'client-supplied',
    `method`           VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `request_hash`     CHAR(64)     NOT NULL COMMENT 'sha256 of serialized request',
    `status_code`      VARCHAR(32)  NOT NULL COMMENT 'gRPC code string',
    `response_body`    MEDIUMBLOB            DEFAULT NULL COMMENT 'protobuf-serialized response',
    `response_err`     VARCHAR(512)          DEFAULT NULL COMMENT 'populated when status_code != OK',
    `created`          DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires`          DATETIME(3)  NOT NULL COMMENT 'TTL eviction, typically 24h out',
    PRIMARY KEY (`idempotency_key`, `method`),
    KEY `idx_expires` (`expires`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='幂等键 (req/resp 缓存)';

-- ─── 管理员操作审计（tamper-evident）─────────────────────────────────────────
-- 每次 mutation RPC 由 interceptor 写一条；append-only；合规要求保留 ≥7 年。
-- chain_hash 把上一行的 hash 带进来，篡改任意一行会让后续链断开，运维侧
-- nightly 扫一遍验证。
CREATE TABLE IF NOT EXISTS `admin_audit_log` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL COMMENT 'merchant_id / doc_id / secret_id',
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields must be redacted before write',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'sha256 of previous row (tamper-evident chain)',
    `row_hash`       CHAR(64)     NOT NULL COMMENT 'sha256 of this row inputs',
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='管理台审计日志 (append-only)';

-- ─── 商户渠道凭据（多租户密钥隔离） ──────────────────────────────────────────
-- 每个 (merchant_id, channel, field_name) 一条：保存加密后的渠道凭据。
-- 明文只有 payment-channel 在 adapter init 时才向本服务请求解密；admin UI
-- 永远不展示原文。Put 前会经 kms-manage 加密
-- （context="merchant:<id>:channel:<ch>:<field>"）。
CREATE TABLE IF NOT EXISTS `merchant_channel_secret` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `channel`      VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/grabpay/...',
    `field_name`   VARCHAR(64)  NOT NULL COMMENT 'client_id/client_secret/signing_key/webhook_secret/partner_id/...',
    `ciphertext`   VARBINARY(4096) NOT NULL COMMENT 'kms:v1:<key_id>:<payload>',
    `context`      VARCHAR(256) NOT NULL COMMENT 'KMS AAD used on Put; needed for decrypt',
    `masked_hint`  VARCHAR(64)  DEFAULT NULL COMMENT '展示用摘要: 如 "sk_live_***abc" (第一次写入时算出)',
    `version`      INT          NOT NULL DEFAULT 1 COMMENT '每次 rotate 自增',
    `created_by`   VARCHAR(64)  DEFAULT NULL COMMENT 'admin user id',
    `created`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_merchant_channel_field` (`merchant_id`, `channel`, `field_name`),
    KEY `idx_merchant_channel` (`merchant_id`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据（密文）';

-- ─── C 端用户身份（11 张表 + email_codes）────────────────────────────────────
-- 跟 merchants 表完全独立。
--   users               主表（username/email/phone 都唯一可空，bcrypt 主密码 + status FSM）
--   user_profiles       1:1 资料
--   user_auths          多渠道认证（password / email / phone / google / wechat / github / apple）
--   login_logs          登录日志（成功 + 失败 + IP + UA）
--   user_sessions       会话 / SSO 撤销点（删 row → 下一次 IntrospectToken 失败）
--   roles / user_roles / permissions / role_permissions  RBAC
--   user_accounts       (user_id, currency) → accounting-system account_id 引用
--   user_settings       UI 偏好 / 通知开关 (JSON)
--   email_codes         6 位邮件 / 短信验证码（sha256，不存明文）+ purpose
-- user_id 由 idgen.BizTagUser 分配，[100000000, 899999999) 与 accounting-system 对齐。

CREATE TABLE IF NOT EXISTS `users` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户主表';

CREATE TABLE IF NOT EXISTS `user_profiles` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0 COMMENT '0=未知 1=男 2=女',
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户资料';

CREATE TABLE IF NOT EXISTS `user_auths` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL COMMENT 'password/email/phone/google/wechat/github/apple',
    `identifier`  VARCHAR(100) NOT NULL COMMENT 'username / email / phone / OAuth openid',
    `credential`  VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'OAuth token（建议 KMS 加密）',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证';

CREATE TABLE IF NOT EXISTS `login_logs` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志';

CREATE TABLE IF NOT EXISTS `user_sessions` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `token`       VARCHAR(255) NOT NULL COMMENT 'JWT 字符串（也可换 opaque session id）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 / SSO 撤销点';

CREATE TABLE IF NOT EXISTS `roles` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `name`         VARCHAR(50)  NOT NULL,
    `description`  VARCHAR(255) NOT NULL DEFAULT '',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC roles';

CREATE TABLE IF NOT EXISTS `user_roles` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role';

CREATE TABLE IF NOT EXISTS `permissions` (
    `id`    BIGINT       NOT NULL AUTO_INCREMENT,
    `name`  VARCHAR(100) NOT NULL,
    `code`  VARCHAR(100) NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_code` (`code`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC permissions';

CREATE TABLE IF NOT EXISTS `role_permissions` (
    `role_id`        BIGINT NOT NULL,
    `permission_id`  BIGINT NOT NULL,
    PRIMARY KEY (`role_id`, `permission_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC role-permission';

CREATE TABLE IF NOT EXISTS `user_accounts` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL COMMENT 'USD / PHP / CNY',
    `account_id`  VARCHAR(64)  NOT NULL COMMENT 'accounting-system 主键',
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用';

CREATE TABLE IF NOT EXISTS `user_settings` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 / 通知开关';

CREATE TABLE IF NOT EXISTS `email_codes` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `identifier`  VARCHAR(254) NOT NULL COMMENT 'email 或 E.164 phone',
    `code_hash`   VARCHAR(64)  NOT NULL COMMENT 'sha256(code)；不存明文',
    `purpose`     VARCHAR(32)  NOT NULL COMMENT 'verify_email/reset_password/login_otp/bind_phone',
    `expires_at`  DATETIME(3)  NOT NULL,
    `used_at`     DATETIME(3)  NULL,
    `attempts`    INT          NOT NULL DEFAULT 0 COMMENT '失败尝试 >5 → 锁掉',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_identifier` (`identifier`),
    KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='6 位邮件 / 短信验证码';

-- ─── RBAC 默认数据 ──────────────────────────────────────────────────────────
INSERT IGNORE INTO `roles` (`name`, `description`) VALUES
    ('user',  'C 端默认用户角色'),
    ('admin', '运营后台管理员');

INSERT IGNORE INTO `permissions` (`name`, `code`) VALUES
    ('查看自己的资料',     'user.profile.read'),
    ('修改自己的资料',     'user.profile.write'),
    ('查看自己的账户',     'user.account.read'),
    ('管理用户',           'admin.user.manage'),
    ('管理商户',           'admin.merchant.manage'),
    ('管理 KYC 审核',     'admin.kyc.review'),
    ('查看风控决策',       'admin.risk.read');

INSERT IGNORE INTO `role_permissions` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `roles` r
JOIN `permissions` p ON (
    (r.name = 'user'  AND p.code IN ('user.profile.read','user.profile.write','user.account.read'))
 OR (r.name = 'admin' AND p.code IN ('admin.user.manage','admin.merchant.manage','admin.kyc.review','admin.risk.read',
                                     'user.profile.read','user.profile.write','user.account.read'))
);
