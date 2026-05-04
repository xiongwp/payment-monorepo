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

-- ─── 管理台审计日志 (wave B) ──────────────────────────────────────────────────
-- 记录所有管理员操作（商户审核、KYC 推进、key rotate、手动退款、规则修改...）。
-- 合规要求：不可修改、保留 ≥7 年。分区留给运维做月度 partitioning。
CREATE TABLE IF NOT EXISTS `admin_audit_log` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id (from bearer token)',
    `actor_ip`       VARCHAR(64)  DEFAULT NULL,
    `action`         VARCHAR(64)  NOT NULL COMMENT 'merchant.approve / refund.create / risk.rule.update / ...',
    `target_type`    VARCHAR(32)  DEFAULT NULL COMMENT 'merchant/payment_intent/refund/risk_rule/...',
    `target_id`      VARCHAR(64)  DEFAULT NULL,
    `http_method`    VARCHAR(8)   DEFAULT NULL,
    `http_path`      VARCHAR(256) DEFAULT NULL,
    `http_status`    INT          DEFAULT NULL,
    `request_body`   MEDIUMTEXT   DEFAULT NULL COMMENT 'JSON；敏感字段应由调用方 redact',
    `response_code`  VARCHAR(32)  DEFAULT NULL COMMENT 'gRPC/HTTP error code if failure',
    `response_msg`   VARCHAR(512) DEFAULT NULL,
    `duration_ms`    INT          DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`       (`actor`),
    KEY `idx_action`      (`action`),
    KEY `idx_target`      (`target_type`, `target_id`),
    KEY `idx_created`     (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='管理台审计日志 (append-only)';

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
