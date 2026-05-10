-- 共享 meta —— paychan_meta + order_meta + user_merchant_meta + account_meta

-- ==== payment-channel ====
-- paychan_meta：非分片，存放 Leaf Segment idgen 等元数据
CREATE DATABASE IF NOT EXISTS `paychan_meta` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `paychan_meta`;

-- Leaf 号段 ID 分配表
CREATE TABLE IF NOT EXISTS `leaf_alloc` (
    `biz_tag`     VARCHAR(128) NOT NULL DEFAULT '',
    `max_id`      BIGINT       NOT NULL DEFAULT 1,
    `step`        INT          NOT NULL DEFAULT 100000,
    `description` VARCHAR(256) DEFAULT NULL,
    `update_time` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`biz_tag`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Leaf 号段 ID 分配表';
-- 服务启动时 idgen.New 会幂等注册 paychan.acquirer_tx / paychan.webhook_raw / paychan.channel_token

-- ==== order-core ====
-- order_meta：非分片，存放 Leaf Segment idgen 等元数据
CREATE DATABASE IF NOT EXISTS `order_meta` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `order_meta`;

-- Leaf 号段 ID 分配表
CREATE TABLE IF NOT EXISTS `leaf_alloc` (
    `biz_tag`     VARCHAR(128) NOT NULL DEFAULT '',
    `max_id`      BIGINT       NOT NULL DEFAULT 1,
    `step`        INT          NOT NULL DEFAULT 100000,
    `description` VARCHAR(256) DEFAULT NULL,
    `update_time` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`biz_tag`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Leaf 号段 ID 分配表';
-- 服务启动时 idgen.New 会幂等注册 order.payment_intent / order.charge / order.refund

-- 注：商户主表 / KYC 文档 / KYC 审计 / 商户渠道凭据 已搬迁至独立服务
-- user-merchant-core（user_merchant_meta 库）。order-core 里的 mch_id 只是外键。

-- ─── Webhook 投递日志 (wave C 出站 webhook) ──────────────────────────────────
CREATE TABLE IF NOT EXISTS `webhook_deliveries` (
    `id`               BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`      VARCHAR(32)  NOT NULL,
    `event_id`         VARCHAR(64)  NOT NULL COMMENT '全局唯一事件 ID',
    `event_type`       VARCHAR(64)  NOT NULL COMMENT 'payment_intent.succeeded / charge.failed / ...',
    `payload`          JSON         NOT NULL COMMENT '投递的 JSON body',
    `url`              VARCHAR(512) NOT NULL COMMENT '目标 webhook URL',
    `status`           VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/succeeded/failed/exhausted',
    `http_status`      INT          NOT NULL DEFAULT 0 COMMENT '最近一次 HTTP status',
    `attempts`         INT          NOT NULL DEFAULT 0,
    `max_attempts`     INT          NOT NULL DEFAULT 5,
    `next_retry_at`    DATETIME              DEFAULT NULL,
    `last_error`       TEXT                  DEFAULT NULL,
    -- claim_token 用于多 worker 并发投递时的批次 claim：worker 拿到一批待投
    -- 递的 row → UPDATE 写入自己生成的 claim_token，后续 SELECT 只取 token
    -- 匹配的 row。原先复用 last_error 字段做 claim，混淆了"业务错误"与
    -- "投递协调"两种语义；现在拆开。
    `claim_token`      VARCHAR(64)           DEFAULT NULL,
    `created_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_event` (`event_id`),
    INDEX `idx_merchant_status` (`merchant_id`, `status`),
    -- wave L: cover all predicates in processRetries() (status + next_retry_at +
    -- attempts < max_attempts) so the hot retry scan doesn't fall back to a
    -- table scan on attempts.
    INDEX `idx_retry` (`status`, `next_retry_at`, `attempts`),
    -- worker 用 claim_token 反查刚 claim 的批次；带 prefix 区分度足够，N=batchSize 量级
    INDEX `idx_claim` (`claim_token`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Webhook 投递日志';

-- ─── admin_audit_log 已迁到 shard ───────────────────────────────────────────
-- 见 packages/order-core/database/orderdb/init/N_init.sql 里的 admin_audit_log_NN
-- 分片表（10 库 × 10 表 = 100 张），按 actor (admin user id) 哈希路由。
-- 同一 admin 的所有操作在同一 shard，取证 / 客服查全。
-- 10K TPS 写量：单 meta 库 ~30K insert/s 上限会变瓶颈，迁后总容量 10×。
-- meta 这边只留 RBAC / GL / etc.，不再承载审计写。

-- ─── Ledger (wave H) ─────────────────────────────────────────────────────────
-- 双账记账。单币种 PHP（按产品决策不做 FX）。所有金额为 minor units
-- (1 PHP = 100 centavo)，BIGINT 存储。
--
-- 账户维度：
--   platform  平台自有账户（手续费收入、现金、风控储备）
--   merchant  商户往来（应付给商户 / 商户退款应收）
--   channel   渠道往来（渠道应付给我们 / 我们应退给渠道）
--
-- 账户类型决定借贷方向：
--   asset / expense                debit 增加
--   liability / equity / revenue   credit 增加

CREATE TABLE IF NOT EXISTS `gl_account` (
    `id`             VARCHAR(64)  NOT NULL COMMENT 'acct_<owner_type>_<name>_<owner_id>',
    `name`           VARCHAR(128) NOT NULL,
    `type`           VARCHAR(16)  NOT NULL COMMENT 'asset/liability/revenue/expense/equity',
    `owner_type`     VARCHAR(16)  NOT NULL COMMENT 'platform/merchant/channel',
    `owner_id`       VARCHAR(64)  DEFAULT NULL COMMENT 'mch_xxx / gcash / maya ...',
    `currency`       CHAR(3)      NOT NULL DEFAULT 'PHP',
    `debit_balance`  BIGINT       NOT NULL DEFAULT 0 COMMENT 'denormalized; minor units',
    `credit_balance` BIGINT       NOT NULL DEFAULT 0 COMMENT 'denormalized; minor units',
    `version`        BIGINT       NOT NULL DEFAULT 0 COMMENT '乐观锁，防止并发 Post 丢更新',
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active/closed',
    `metadata`       JSON         DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_owner`   (`owner_type`, `owner_id`),
    KEY `idx_type`    (`type`),
    KEY `idx_status`  (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='总账账户';

CREATE TABLE IF NOT EXISTS `gl_transaction` (
    `id`            VARCHAR(64)  NOT NULL COMMENT 'gltxn_xxx',
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='总账事务 (双账凭证)';

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

-- 注：merchant_channel_secret 已搬迁至 user-merchant-core。

-- ==== user-merchant-core ====
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
-- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 静默截断 → SELECT 匹配不上。
-- VARCHAR(512) × 4 byte (utf8mb4) = 2048 bytes < InnoDB 单列 PK 3072-byte 上限。
CREATE TABLE IF NOT EXISTS `session_lookup` (
    `token`      VARCHAR(512) NOT NULL,
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

-- ─── admin_audit_log 已迁到 shard ───────────────────────────────────────────
-- 见 packages/user-merchant-core/database/userdb/init/N_init.sql 里的
-- admin_audit_log_NN 分片表（10 库 × 10 表 = 100 张），按 actor 哈希路由。
-- 链式签名 per-shard：每 (db,tbl) 独立 prev_hash 链。
-- 10K TPS audit 写量：meta 单库 ~30K/s 上限会瓶颈，迁后总容量 10×。

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

-- ==== accounting-system (account_meta: leaf_alloc / business_type / hot_account ...) ====
-- 创建另一个数据库account_meta 用于存储账户体系的元数据，如账户类型、交易规则等

-- 强制本次 session 的三个字符集都切到 utf8mb4。否则如果 server 默认是 latin1（MySQL 5.7
-- 很多默认 my.cnf 就是 latin1 / utf8），下面 INSERT 里的中文会被按 latin1 字节存进 utf8mb4 列，
-- 读回来时再当 utf8mb4 解码就变成  "ç"¨æˆ·ä½™é¢è´¦æˆ·"  这种 mojibake。
SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;

CREATE DATABASE IF NOT EXISTS `account_meta` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `account_meta`;


-- ============================================
-- 8. 账户类型配置表 (account_type_info)
-- 全局表，定义账户科目类型及其属性
-- ============================================
CREATE TABLE IF NOT EXISTS `account_type_info` (
    `id`               BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `account_type`     VARCHAR(32)  NOT NULL COMMENT '账户类型唯一码，如 USER_WALLET / MERCHANT_WALLET',
    `account_type_name` VARCHAR(256) NOT NULL COMMENT '账户类型名称',
    `account_type_desc` VARCHAR(256) DEFAULT NULL COMMENT '账户类型描述',
    `owner_type`       INT(4) NOT NULL COMMENT '账户所有者类型，对应 account.account_type: 1=user 2=merchant 3=merchant_pending 4=platform 5=transit_receivable 6=transit_payable 7=tx_fee 8=charge_fee 9=transit',
    -- is_platform: 1 = 平台内部类型（owner_id 落在预留段 [1,10000]，只能通过 CreatePlatformAccount / Fleet 创建）
    --              0 = 业务账户类型（owner_id 落在 user/merchant 段，通过 CreateAccount 创建）
    -- 未来新增 AccountType 时补一行并设好 is_platform；Go 代码 isPlatformAccountType 也会同步从这里派生（
    -- 当前是 hardcoded switch 4-9）。
    `is_platform`      TINYINT(1) NOT NULL DEFAULT 0 COMMENT '1=平台内部类型(owner_id∈[1,10000]) 0=业务账户类型',
    `balance_direction` VARCHAR(8) NOT NULL COMMENT '正常余额方向: C=贷方 D=借方',
    `description`      VARCHAR(1024) DEFAULT NULL,
    `byte_rds_ctx`     TEXT DEFAULT NULL COMMENT 'env ctx',
    `extra`            TEXT DEFAULT NULL,
    `create_time`      TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`      TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_account_type` (`account_type`),
    KEY `idx_is_platform` (`is_platform`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE utf8mb4_general_ci COMMENT='账户类型配置表';

-- ============================================
-- 8b. 账户业务类型配置表 (account_business_type_info)
-- 全局表，定义 (user_id, business_type) 唯一键中的 business_type 数字码（1-999）。
--
-- category 不在本表存储 —— category 由 account_type 1:1 确定(例如 TransitChannelReceivable=ASSET,
-- TransactionFee=REVENUE...), 放在表里会产生冗余并有被写错的风险。需要 category 时
-- 从 accountingService.categoryForAccountType(account_type) 派生。
--
-- 背景：AccountBusinessType 原本是硬编码的 Go 常量（1-9）。随着"同一 AccountType
-- 多渠道"（Alipay/Gcash 都是 TransitChannelReceivable=5，但需独立账务）需求出现，
-- 每个新渠道需要占用一个未使用的 business_type 码。由 DB 管理后：
--   - 新渠道注册不需要改代码、发版；
--   - 所有实例通过 ConfigSyncWorker 或 HTTP 推送同步最新映射；
--   - 运营可直接在 admin-web 查询"101 对应哪个渠道"，避免约定文档脱节。
--
-- 约定:
--   - 1-9 系统默认保留（映射 AccountType 1-9 的默认渠道）；
--   - 10-100 预留给未来官方 business_type；
--   - 101-999 给自定义渠道（Alipay/Gcash 等）使用。
-- ============================================
CREATE TABLE IF NOT EXISTS `account_business_type_info` (
    `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'id',
    `business_type`      SMALLINT UNSIGNED NOT NULL COMMENT '业务类型数字码 1-999，(user_id, business_type) 的 business_type 部分',
    `business_type_code` VARCHAR(64)  NOT NULL COMMENT '业务类型码名，如 USER_BALANCE / ALIPAY_RECEIVABLE',
    `account_type`       TINYINT      NOT NULL COMMENT '绑定的 AccountType 数字枚举（1-9，对应 account_type_info.owner_type）;category 由此 1:1 派生，不再存表',
    `description`        VARCHAR(256) DEFAULT NULL,
    `enabled`            TINYINT(1)   NOT NULL DEFAULT 1 COMMENT '0=停用，1=启用',
    `created_at`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_business_type`      (`business_type`),
    UNIQUE KEY `uk_business_type_code` (`business_type_code`),
    KEY         `idx_account_type`     (`account_type`),
    KEY         `idx_enabled`          (`enabled`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户业务类型配置表（channel registry）';

-- 预置默认 1-9（对应 Go 代码中的 AccountBusinessType 常量）
INSERT IGNORE INTO `account_business_type_info`
    (`business_type`, `business_type_code`, `account_type`, `description`, `enabled`)
VALUES
    (1, 'USER_BALANCE',                 1, '用户余额账户', 1),
    (2, 'MERCHANT_BALANCE',             2, '商户结算账户', 1),
    (3, 'MERCHANT_PENDING_SETTLE',      3, '商户待结算余额账户', 1),
    (4, 'PLATFORM_PNL',                 4, '平台损益账户', 1),
    (5, 'TRANSIT_CHANNEL_RECEIVABLE',   5, '中间账户-渠道应收款（默认渠道）', 1),
    (6, 'TRANSIT_CHANNEL_PAYABLE',      6, '中间账户-渠道应付款（默认渠道）', 1),
    (7, 'TRANSACTION_FEE',              7, '平台手续费账户', 1),
    (8, 'CHARGE_FEE',                   8, '平台服务费账户', 1),
    (9, 'TRANSIT',                      9, '平台中间账户', 1);

-- ============================================
-- 9. 交易规则表 (transaction_rule)
-- 全局表，product_code + event_code 决定记账科目和方向
-- 同一 product+event 可有多条规则（主流水 + 手续费等）
-- ============================================
CREATE TABLE IF NOT EXISTS `transaction_rule` (
    `id`               BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `product_code`     VARCHAR(256) NOT NULL COMMENT '产品编码',
    `event_code`       VARCHAR(256) NOT NULL COMMENT '事件编码',
    `hash_key`         VARCHAR(256) NOT NULL COMMENT 'product_code,event_code,credit_subject_id,debit_subject_id 拼接',
    `credit_subject_id` VARCHAR(256) DEFAULT NULL COMMENT '贷方科目，对应 account_type_info.account_type',
    `debit_subject_id`  VARCHAR(256) DEFAULT NULL COMMENT '借方科目，对应 account_type_info.account_type',
    `from_direction`   VARCHAR(16) NOT NULL COMMENT 'from方账户在本规则中的方向: debit/credit',
    `to_direction`     VARCHAR(16) NOT NULL COMMENT 'to方账户在本规则中的方向: debit/credit',
    `transaction_type` INT(4)  NOT NULL COMMENT '交易类型',
    `bookkeeping_mode` VARCHAR(32) NOT NULL COMMENT '记账模式',
    `extra`            TEXT DEFAULT NULL,
    `byte_rds_ctx`     TEXT DEFAULT NULL,
    `create_time`      TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`      TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_hash_key` (`hash_key`),
    KEY `idx_product_code_event_code` (`product_code`, `event_code`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE utf8mb4_general_ci COMMENT='交易规则配置表';
-- ============================================
-- 初始化示例：account_type_info 数据
-- ============================================
-- 1-3 业务账户 (is_platform=0)，4-9 平台内部账户 (is_platform=1)。
-- 未来新增类型(例如 10 = OFFLINE_SETTLEMENT 等)按同样规则写 is_platform,
-- admin-web 的"平台账户"查询页/账户管理下拉会按此 flag 自动过滤。
INSERT IGNORE INTO `account_type_info` (`id`, `account_type`, `account_type_name`, `owner_type`, `is_platform`, `balance_direction`)
VALUES
    (1, 'USER_WALLET',                       '用户钱包',             1, 0, 'C'),
    (2, 'MERCHANT_WALLET',                   '商户钱包',             2, 0, 'C'),
    (3, 'MERCHANT_PENDING_SETTLE',           '商户待结算账户',       3, 0, 'C'),
    (4, 'PLATFORM_PNL',                      '平台损益账户',         4, 1, 'C'),
    (5, 'PLATFORM_TRANSIT_CHANNEL_RECEIVABLE','中间账户渠道应收款',  5, 1, 'D'),
    (6, 'PLATFORM_TRANSIT_CHANNEL_PAYABLE',  '中间账户渠道应付款',   6, 1, 'C'),
    (7, 'PLATFORM_TRANSACTION_FEE',          '平台手续费账户',       7, 1, 'C'),
    (8, 'PLATFORM_CHARGE_FEE',               '平台服务费账户',       8, 1, 'C'),
    (9, 'PLATFORM_TRANSIT',                  '平台中间账户',         9, 1, 'C');

-- ============================================

-- ============================================
-- 10. 号段 ID 分配表 (leaf_alloc)
-- 全局单表，存于 account_meta，不随分库分表复制。
-- 号段模式（Leaf Segment）：每次分配一段连续 ID，双 Buffer 保证高吞吐低延迟。
-- ID 严格单调递增，不依赖时钟，重启后时间漂移不影响单调性。
-- ============================================
CREATE TABLE IF NOT EXISTS `leaf_alloc` (
    `biz_tag`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '业务标签（全局唯一，如 accounting.voucher / accounting.transaction）',
    `max_id`      BIGINT       NOT NULL DEFAULT 1   COMMENT '当前已分配到的最大号段 ID',
    `step`        INT          NOT NULL              COMMENT '号段步长（每次从 DB 获取的 ID 数量）',
    `description` VARCHAR(256)          DEFAULT NULL COMMENT '业务描述',
    `update_time` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最后更新时间',
    PRIMARY KEY (`biz_tag`),
    KEY `idx_update_time` (`update_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='号段 ID 分配表（Leaf Segment 模式）';

-- ============================================
-- 11. TCC 全局协调者表 (tcc_coordinator)
-- 已迁移至各分片库（accounting_db_0 ~ accounting_db_9），每库一张，不分表。
-- 路由规则：hash(tcc_id 数字) % 10 → accounting_db_{dbIndex}.tcc_coordinator
-- 迁移原因：meta DB 单实例无法承受高并发 TCC 的协调者写入（30K TPS × 3 次/TCC = 90K writes/s）。
-- ============================================

-- ============================================
-- 12. 热点账户配置表 (hot_account_config)
-- 记录走 Redis 热路径的账户列表，服务启动时加载到本地缓存。
-- admin 更新后通过 gRPC ReloadHotAccountAllowlist 触发重新拉取。
-- ============================================
CREATE TABLE IF NOT EXISTS `hot_account_config` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `account_no`  VARCHAR(128) NOT NULL COMMENT '账户号（唯一）',
    `enabled`     TINYINT(1)   NOT NULL DEFAULT 1 COMMENT '是否启用热路径：1=是 0=否',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '备注',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_account_no` (`account_no`),
    KEY `idx_enabled` (`enabled`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='热点账户配置（Redis 热路径白名单）';

-- ============================================
-- 13. 缓冲记账账户配置表 (buffer_account_config)
-- 记录走缓冲记账模式的账户及其余额刷新间隔等级。
-- flush_interval_level: 1=1分钟 5=5分钟 10=10分钟 60=60分钟 1440=24小时
-- 服务启动时加载到本地缓存；admin 更新后通过 gRPC ReloadBufferAccountConfig 重新拉取。
-- ============================================
CREATE TABLE IF NOT EXISTS `buffer_account_config` (
    `id`                   BIGINT       NOT NULL AUTO_INCREMENT,
    `account_no`           VARCHAR(128) NOT NULL COMMENT '账户号（唯一）',
    `flush_interval_level` SMALLINT     NOT NULL DEFAULT 1 COMMENT '刷新间隔（分钟）：1/5/10/60/1440',
    `enabled`              TINYINT(1)   NOT NULL DEFAULT 1 COMMENT '是否启用缓冲记账：1=是 0=否',
    `description`          VARCHAR(256) DEFAULT NULL COMMENT '备注',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_account_no` (`account_no`),
    KEY `idx_enabled_level` (`enabled`, `flush_interval_level`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='缓冲记账账户配置（按刷新间隔等级分组）';

-- ============================================
-- 初始化示例：transaction_rule 数据（CHECKOUT_PAY 支付场景）
-- from=买家(user) → credit(贷出), to=卖家(merchant) → debit(借入)
-- ============================================
INSERT IGNORE INTO `transaction_rule`
    (`id`, `product_code`, `event_code`, `hash_key`,
     `credit_subject_id`, `debit_subject_id`,
     `from_direction`, `to_direction`,
     `transaction_type`, `bookkeeping_mode`)
VALUES
    (1, 'PAYMENT', 'CHECKOUT_PAY',
     'PAYMENT,CHECKOUT_PAY,USER_WALLET,MERCHANT_WALLET',
     'USER_WALLET', 'MERCHANT_WALLET',
     'credit', 'debit',
     1, 'DOUBLE_ENTRY');

-- ============================================
-- 服务实例注册表 (service_instance)
-- 每个 accounting-system 实例启动时注册，定期心跳，停止时注销。
-- admin-web 通过此表发现所有活跃实例并推送配置变更。
-- ============================================
CREATE TABLE IF NOT EXISTS `service_instance` (
    `instance_id`     VARCHAR(128) NOT NULL COMMENT '实例唯一ID（hostname:http_admin_port）',
    `host`            VARCHAR(256) NOT NULL COMMENT '外部可达主机地址（IP 或域名）',
    `http_admin_port` INT          NOT NULL COMMENT 'HTTP admin 端口',
    `grpc_port`       INT          NOT NULL COMMENT 'gRPC 端口',
    `status`          TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1=运行中，0=已停止',
    `last_heartbeat`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '最近心跳时间',
    `started_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '实例启动时间',
    PRIMARY KEY (`instance_id`),
    KEY `idx_status_heartbeat` (`status`, `last_heartbeat`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='accounting-system 服务实例注册表';

-- ============================================
-- 系统配置（system_config）已删除（v2 迁到 config-center 服务）。
--
-- 历史：本表是 accounting-system 内置的山寨 key-value 配置中心，被
--       SystemConfigService.GetXxx 当作 hot-path 读源，admin-web 改后
--       走 /admin/reload/config 扇出。
--
-- 现状（v2）：
--   - 全平台统一 config-center 服务（packages/config-center）已上线
--   - 业务侧 SystemConfigService 现在 wrap configcenter SDK，namespace =
--     "accounting-system"，key 名 1:1 保留
--   - admin 写操作改走 config-center admin web /admin/ns/accounting-system
--   - 历史 7 个种子 key 在首次部署 config-center 时由运维人工 seed：
--       tcc_recovery.stuck_timeout_minutes  = 5
--       outbox.poll_interval_ms             = 100
--       outbox.batch_size                   = 500
--       day_cut.chunk_size                  = 100000
--       outbox_backpressure.high_threshold  = 5000
--       outbox_backpressure.low_threshold   = 1000
--       outbox_backpressure.shrink_ratio    = 0.5
-- ============================================

-- ==== card-center meta (card_center_meta: leaf_alloc + audit_log) ====
-- card_center_meta：留 leaf_alloc + 全局审计（量小不分片）。
CREATE DATABASE IF NOT EXISTS `card_center_meta` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_center_meta`;

CREATE TABLE IF NOT EXISTS `leaf_alloc` (
    `biz_tag`     VARCHAR(128) NOT NULL DEFAULT '',
    `max_id`      BIGINT       NOT NULL DEFAULT 1,
    `step`        INT          NOT NULL DEFAULT 100000,
    `description` VARCHAR(256) DEFAULT NULL,
    `update_time` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`biz_tag`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Leaf 号段';

INSERT IGNORE INTO `leaf_alloc` (`biz_tag`, `max_id`, `step`, `description`) VALUES
    ('card_center.stored_token', 1, 100000, 'card_stored_token id'),
    ('card_center.payment_token_used', 1, 100000, 'card_payment_token_used id');

-- user_card_session：HTTPS 入口认证的短期会话 token。
--
-- 流程：
--   1. 浏览器登录 api-gateway → api-gateway 持 user_id 调 card-center.IssueUserCardSession
--      （走 mTLS gRPC，clientCN 白名单：api-gateway）
--   2. card-center 颁发 ucs_<random_32bytes>，绑 user_id + scope + ttl，存本表
--   3. api-gateway 把 ucs_xxx 通过 HTTPS 返浏览器（HttpOnly cookie 或一次性表单字段）
--   4. 浏览器 / 前端 SDK HTTPS 调 card-center 时带 Authorization: Bearer ucs_xxx
--   5. card-center HTTPS handler 校验 session：未过期 + 未撤销 + scope 匹配 → 注入 user_id 到 ctx
--   6. 任何 handler 都从 ctx 取 user_id，**绝不**信任 request body 里的 user_id
--      → 用户 A 永远拿不到用户 B 的卡信息
--
-- TTL 策略（按 scope）：
--   tokenize  → 5 min（绑卡场景，给前端校验用户输入留时间）
--   list      → 30 sec（拉列表，毫秒级，给短暂窗口）
--   delete    → 30 sec
-- 一次性约束：绑卡（tokenize scope）成功后立即标 used=1，禁止复用。
CREATE TABLE IF NOT EXISTS `user_card_session` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `session_hash`  CHAR(64)     NOT NULL,             -- sha256(ucs_xxx)；不存 token 本身，DB 泄露也拿不到 session
    `user_id`       BIGINT       NOT NULL,
    `scope`         VARCHAR(16)  NOT NULL,             -- tokenize / list / delete
    `issued_to`     VARCHAR(64)  NOT NULL,             -- 申请方 mTLS CN，e.g. "api-gateway"
    `client_ip`     VARCHAR(64)  DEFAULT NULL,         -- 浏览器侧 IP（绑卡时用，可做风控）
    `expires_at`    DATETIME(3)  NOT NULL,
    `used_count`    INT          NOT NULL DEFAULT 0,   -- list/delete 可多次复用；tokenize 单次后置 1
    `revoked_at`    DATETIME(3)  DEFAULT NULL,
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_session_hash` (`session_hash`),
    KEY `idx_user_scope`         (`user_id`, `scope`),
    KEY `idx_expires_at`         (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='HTTPS 入口的用户会话 token (Bearer auth)';

-- ─── audit_log 已从 meta 移到 shard ─────────────────────────────────────────
-- 详见 packages/card-center/database/userdb/init/N_init.sql 里的
-- audit_log_NN 分片表（10 库 × 10 表 = 100 表，按 user_id 路由）。
--
-- 移走原因（10K TPS 时 meta 单库 audit insert 是写瓶颈）：
--   - 单 MySQL ~30K simple-insert/s 上限
--   - 10K charge × 1-2 audit/charge = 15K-20K writes/s ≈ 67% 容量
--   - fsync 频率 + AUTO_INCREMENT 锁 + 链式 sha256 串行 → 实际更慢
--
-- 分片设计（per-shard chain）：
--   - 路由 key: user_id；user_id 为空（系统操作）走 trace_id hash 兜底
--   - 链式签名变 per-shard：每 (db_idx, table_idx) 维护独立 prev_hash 链
--     verify 工具按 (db_idx, table_idx) 走 100 条独立链各自验证完整性
--   - 跨 shard 全局序由 Kafka append-only 保证（PCI Req 10 canonical store）
--
-- meta 这边只留 leaf_alloc + user_card_session（量小、不分片）。

-- ==== card-payment meta (card_payment_meta) ====
CREATE DATABASE IF NOT EXISTS `card_payment_meta` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_payment_meta`;

CREATE TABLE IF NOT EXISTS `leaf_alloc` (
    `biz_tag`     VARCHAR(128) NOT NULL DEFAULT '',
    `max_id`      BIGINT       NOT NULL DEFAULT 1,
    `step`        INT          NOT NULL DEFAULT 100000,
    `description` VARCHAR(256) DEFAULT NULL,
    `update_time` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`biz_tag`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT IGNORE INTO `leaf_alloc` (`biz_tag`, `max_id`, `step`, `description`) VALUES
    ('card_payment.transaction', 1, 100000, 'card_transaction id');

-- network 调用审计（poll/查询都打一行，方便排查 + 对账）
CREATE TABLE IF NOT EXISTS `network_call_log` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`          VARCHAR(64),
    `network`        VARCHAR(16)  NOT NULL,
    `op`             VARCHAR(16)  NOT NULL,           -- authorize / capture / refund / void / query
    `network_ref_no` VARCHAR(64),
    `status`         VARCHAR(16),
    `latency_ms`     INT,
    `decline_code`   VARCHAR(32),
    `trace_id`       VARCHAR(64),
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`           (`pi_id`),
    KEY `idx_network_ref`  (`network_ref_no`),
    KEY `idx_created`      (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Network call audit (no PAN)';

-- ==== database meta _shadow ====
-- paychan_meta 的影子表（压测 / shadow 流量）。
-- 依赖：init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）。
--
-- leaf_alloc 也建一份独立影子号段：压测 shadow 流量从 leaf_alloc_shadow 取号，
-- 主流量号段不被压测消耗。两张表的 max_id 各自递增、互不影响。
USE `paychan_meta`;

CREATE TABLE IF NOT EXISTS `leaf_alloc_shadow` LIKE `leaf_alloc`;

-- ==== database meta _shadow ====
-- order_meta 的影子表（压测 / shadow 流量）。
-- 依赖：init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）。
--
-- leaf_alloc 也建一份独立影子号段：压测 shadow 流量从 leaf_alloc_shadow 取号，
-- 主流量号段不被压测消耗。两张表的 max_id 各自递增、互不影响。
USE `order_meta`;

CREATE TABLE IF NOT EXISTS `leaf_alloc_shadow`         LIKE `leaf_alloc`;
CREATE TABLE IF NOT EXISTS `webhook_deliveries_shadow` LIKE `webhook_deliveries`;
-- admin_audit_log_shadow 已迁到 shard（见 orderdb/init/N_init_shadow.sql 里
-- 的 admin_audit_log_shadow_NN）；主表 admin_audit_log 在 order_meta 已删，
-- 这里不再 CREATE LIKE。
CREATE TABLE IF NOT EXISTS `gl_account_shadow`         LIKE `gl_account`;
CREATE TABLE IF NOT EXISTS `gl_transaction_shadow`     LIKE `gl_transaction`;
CREATE TABLE IF NOT EXISTS `gl_entry_shadow`           LIKE `gl_entry`;

-- ==== database meta _shadow ====
-- user_merchant_meta 影子表（压测 / shadow 流量）。
-- 依赖：init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）。
--
-- 分库分表后 meta 留下的表（leaf_alloc / RBAC 字典 / idempotency / email_codes /
-- 审计 / lookup 反查索引）每张都有 _shadow 副本；users / merchants 等 11 张
-- 业务分片表的 _shadow 在 user_merchant_db_0..9 里，不在 meta（见
-- database/userdb/init/N_init_shadow.sql）。
USE `user_merchant_meta`;

-- 号段独立（影子流量取 ID 不消耗主用户号段）
CREATE TABLE IF NOT EXISTS `leaf_alloc_shadow`            LIKE `leaf_alloc`;

-- 幂等 / 验证码
CREATE TABLE IF NOT EXISTS `idempotency_key_shadow`       LIKE `idempotency_key`;
CREATE TABLE IF NOT EXISTS `email_codes_shadow`           LIKE `email_codes`;

-- 反查二级索引（meta；指向 user_merchant_db_*.users_NN(_shadow) 等分片表）
CREATE TABLE IF NOT EXISTS `user_lookup_shadow`           LIKE `user_lookup`;
CREATE TABLE IF NOT EXISTS `merchant_lookup_shadow`       LIKE `merchant_lookup`;
CREATE TABLE IF NOT EXISTS `session_lookup_shadow`        LIKE `session_lookup`;
CREATE TABLE IF NOT EXISTS `auth_lookup_shadow`           LIKE `auth_lookup`;

-- 合规审计（append-only）
CREATE TABLE IF NOT EXISTS `merchant_kyc_audit_shadow`    LIKE `merchant_kyc_audit`;
-- admin_audit_log_shadow 已迁到 shard（见 user-merchant-core/database/userdb/
-- init/N_init_shadow.sql 里的 admin_audit_log_shadow_NN）；主表 admin_audit_log
-- 在 user_merchant_meta 已删，这里不再 CREATE LIKE。

-- RBAC 字典（虽然角色 / 权限通常静态，shadow 隔离避免压测期 admin 写覆盖主表）
CREATE TABLE IF NOT EXISTS `roles_shadow`                 LIKE `roles`;
CREATE TABLE IF NOT EXISTS `permissions_shadow`           LIKE `permissions`;
CREATE TABLE IF NOT EXISTS `role_permissions_shadow`      LIKE `role_permissions`;

-- ─── leaf_alloc_shadow seed：起点对齐 payment-util/shadow 的数字 layout ────
--
-- 与 payment-util/shadow/identity.go 的段定义保持一致：
--   ShadowUserIDMin       = 9_000_000_000        (1e9 × 9)
--   MerchantIDShadowMul   = 1_000_000_000_000_000_000  (1e18)
--   EntityIDShadowMul     = 1_000_000_000_000_000_000  (1e18)
--
-- 这样 shadow 流量从 idgen 拿到的 ID 直接落在影子段，不需要后续 caller 加偏移。
INSERT IGNORE INTO `leaf_alloc_shadow` (`biz_tag`, `max_id`, `step`, `description`) VALUES
    ('user_merchant.user',           9000000000,          100000, 'Shadow user id (>= ShadowUserIDMin = 9e9)'),
    ('user_merchant.merchant',       1000000000000000001, 100000, 'Shadow merchant id (high bit 1 = shadow per layout)'),
    ('user_merchant.kyc_document',   1000000000000000001, 100000, 'Shadow KYC document id (entity layout high bit = shadow)');

-- ==== database meta _shadow ====
-- account_meta 影子表（压测 / shadow 流量）。
-- 依赖：init.sql 必须已经导入（CREATE TABLE LIKE 需要主表存在）。
--
-- 影子表 = 主表的 LIKE 副本 + 字典 seed 同步 + 号段起点对齐 shadow 段。
USE `account_meta`;

-- 号段独立
CREATE TABLE IF NOT EXISTS `leaf_alloc_shadow`              LIKE `leaf_alloc`;

-- 字典 / 配置类表
CREATE TABLE IF NOT EXISTS `account_type_info_shadow`        LIKE `account_type_info`;
CREATE TABLE IF NOT EXISTS `account_business_type_info_shadow` LIKE `account_business_type_info`;
CREATE TABLE IF NOT EXISTS `transaction_rule_shadow`         LIKE `transaction_rule`;
CREATE TABLE IF NOT EXISTS `hot_account_config_shadow`       LIKE `hot_account_config`;
CREATE TABLE IF NOT EXISTS `buffer_account_config_shadow`    LIKE `buffer_account_config`;
CREATE TABLE IF NOT EXISTS `service_instance_shadow`         LIKE `service_instance`;
-- system_config / system_config_shadow 已删除（v2 迁到全平台 config-center
-- 服务，namespace=accounting-system；shadow 流量同样消费 config-center key，
-- 无需独立 _shadow 表）。

-- ─── 字典 seed 复制：shadow 流量也要能查这些固定枚举 ──────────────────────
-- 业务字典（业务类型 / 账户类型 / 交易规则）在主和影流量下语义一致，
-- 直接拷贝主表的 seed 即可；shadow 流量绝不修改这些字典。
INSERT IGNORE INTO `account_business_type_info_shadow` SELECT * FROM `account_business_type_info`;
INSERT IGNORE INTO `account_type_info_shadow`           SELECT * FROM `account_type_info`;
INSERT IGNORE INTO `transaction_rule_shadow`            SELECT * FROM `transaction_rule`;
INSERT IGNORE INTO `hot_account_config_shadow`          SELECT * FROM `hot_account_config`;
INSERT IGNORE INTO `buffer_account_config_shadow`       SELECT * FROM `buffer_account_config`;

-- ─── leaf_alloc_shadow seed：起点对齐 payment-util/shadow 数字 layout ────
--
-- 与 payment-util/shadow/identity.go 段定义对齐：
--   ShadowUserIDMin       = 9_000_000_000              (1e9 × 9)，shadow 真实用户段
--   EntityIDShadowMul     = 1_000_000_000_000_000_000  (1e18)，shadow entity ID 段
--
-- shadow 号段从这个起点开始递增，业务侧 caller 拿到的 ID 直接带 shadow 高位标识。
-- 注：accounting-system 内部生成的 user_id（fleet 平台账户）走 [9e9, 9e9+99]
-- 段，业务用户 user_id 走 [9e9+100, 9.9e9]；fleet seed 见各 shard 的
-- *_init_tmp_shadow.sql。
INSERT IGNORE INTO `leaf_alloc_shadow` (`biz_tag`, `max_id`, `step`, `description`) VALUES
    ('accounting.user',           9000000100,          100000, 'Shadow user id (≥ 9e9 + 100，预留 fleet)'),
    ('accounting.voucher',        1000000000000000001, 100000, 'Shadow voucher id (entity layout high bit = shadow)'),
    ('accounting.tcc',            1000000000000000001, 100000, 'Shadow tcc id (entity layout high bit = shadow)'),
    ('accounting.batch_order',    1000000000000000001, 100000, 'Shadow batch order id'),
    ('accounting.account',        90000000000,         100000, 'Shadow account_id (high bit 1 in 19-digit account layout)');
