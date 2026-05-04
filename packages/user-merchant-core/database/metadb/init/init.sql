-- user_merchant_meta：非分片，留下面这些表：
--   leaf_alloc                idgen 元数据（全局共享）
--   roles / permissions /
--   role_permissions          RBAC 字典
--   idempotency_key           幂等键（by key+method, 跨用户全局）
--   email_codes               6位邮件 / 短信验证码（量级小、by identifier）
--   merchant_kyc_audit        商户 KYC 状态流转日志（append-only 合规审计）
--   admin_audit_log           管理台操作审计（append-only 合规审计）
--
-- 分片表（移到 user_merchant_db_0..9）：
--   users / user_profiles / user_auths / login_logs / user_sessions /
--   user_roles / user_accounts / user_settings        按 user_id 路由
--   merchants / merchant_kyc_document /
--   merchant_channel_secret                          按 merchant_id 路由
-- 见 database/userdb/templates/schema.sql + generate.sh。

CREATE DATABASE IF NOT EXISTS `user_merchant_meta` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `user_merchant_meta`;

-- ─── Leaf 号段 ID 分配表（全局共享）──────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `leaf_alloc` (
    `biz_tag`     VARCHAR(128) NOT NULL DEFAULT '',
    `max_id`      BIGINT       NOT NULL DEFAULT 1,
    `step`        INT          NOT NULL DEFAULT 100000,
    `description` VARCHAR(256) DEFAULT NULL,
    `update_time` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`biz_tag`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Leaf 号段 ID 分配表';
-- User id 段必须从 1e8 起：accounting-system 把 [1e8, 9e8) 保留给 user owner_id；
-- 低于 1e8 视作平台 / 系统账户（CreatePlatformAccount 才能开）。
INSERT IGNORE INTO `leaf_alloc` (`biz_tag`, `max_id`, `step`, `description`) VALUES
    ('user_merchant.user',         100000000, 100000, 'User id (≥1e8 reserved by accounting)'),
    ('user_merchant.merchant',           1000, 100000, 'Merchant id'),
    ('user_merchant.kyc_document',       1000, 100000, 'Merchant KYC document id');
UPDATE `leaf_alloc` SET `max_id` = 100000000
    WHERE `biz_tag` = 'user_merchant.user' AND `max_id` < 100000000;

-- ─── 幂等键（Stripe 风格 Idempotency-Key）────────────────────────────────────
-- by (idempotency_key, method) 唯一，跨 user / merchant 全局，留 meta。
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

-- ─── 二级索引表：把 email / username / phone / key_hash → user_id / merchant_id ─
-- users / merchants 分片后，反查路径（GetByEmail / GetByUsername / GetByPhone /
-- GetByKeyHash）不能直接命中。在 meta 加 lookup 表：
--   1) Create / Update User 时 INSERT/UPDATE 一条 (lookup_type, lookup_value, user_id)
--   2) GetByEmail：先 SELECT user_id FROM user_lookup WHERE lookup_type='email' AND lookup_value=?
--   3) 拿到 user_id 后 router.RouteByUserID 路由到 shard 拿完整 row
-- 比 fanout 100 张分片表快 100 倍。
--
-- 软删 / 改 email 时同步删除老 lookup 行；不一致风险 < 1ms 窗口。

CREATE TABLE IF NOT EXISTS `user_lookup` (
    `lookup_type`  VARCHAR(16)  NOT NULL COMMENT 'email / username / phone / auth:<authType>',
    `lookup_value` VARCHAR(254) NOT NULL,
    `user_id`      BIGINT       NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`lookup_type`, `lookup_value`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User 反查二级索引';

CREATE TABLE IF NOT EXISTS `merchant_lookup` (
    `lookup_type`  VARCHAR(16)  NOT NULL COMMENT 'email / live_key_hash / test_key_hash',
    `lookup_value` VARCHAR(254) NOT NULL,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`lookup_type`, `lookup_value`),
    KEY `idx_merchant_id` (`merchant_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Merchant 反查二级索引';

-- session_lookup: token → user_id；GetSession 反查走它（user_sessions 已分片）。
CREATE TABLE IF NOT EXISTS `session_lookup` (
    `token`      VARCHAR(255) NOT NULL,
    `user_id`    BIGINT       NOT NULL,
    `expires_at` DATETIME(3)  NOT NULL,
    `created_at` DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`token`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Session token 反查（指向 user_sessions 分片表）';

-- auth_lookup: (auth_type, identifier) → user_id；FindAuth 反查走它。
CREATE TABLE IF NOT EXISTS `auth_lookup` (
    `auth_type`  VARCHAR(20)  NOT NULL,
    `identifier` VARCHAR(100) NOT NULL,
    `user_id`    BIGINT       NOT NULL,
    `created_at` DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Auth identifier 反查（指向 user_auths 分片表）';

-- ─── 验证码（量级小，by identifier 非 user_id）────────────────────────────────
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

-- ─── KYC 状态流转日志（append-only 合规审计）─────────────────────────────────
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

-- ─── 管理员操作审计（tamper-evident，跨 merchant 全局，append-only）──────────
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

-- ─── RBAC 字典 ──────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `roles` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `name`         VARCHAR(50)  NOT NULL,
    `description`  VARCHAR(255) NOT NULL DEFAULT '',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC roles';

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
