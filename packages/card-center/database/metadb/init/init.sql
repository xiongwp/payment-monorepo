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

-- audit_log：每次 Tokenize / Detokenize 写一行（同步落 DB + 异步发 Kafka 双重保险）。
-- 7 年留存（PCI-DSS 10.7）+ tamper-evident chain（prev_hash + row_hash）。
CREATE TABLE IF NOT EXISTS `audit_log` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `op`           VARCHAR(32)  NOT NULL,             -- tokenize / create_payment / detokenize / delete / revoke
    `caller`       VARCHAR(64)  NOT NULL,             -- 客户端 CN（mTLS）
    `caller_ip`    VARCHAR(64)  DEFAULT NULL,
    `user_id`      BIGINT       DEFAULT NULL,
    `pi_id`        VARCHAR(64)  DEFAULT NULL,
    `token_hash`   CHAR(64)     DEFAULT NULL,        -- token 的 sha256，避免存 token 本身
    `kms_kid`      VARCHAR(32)  DEFAULT NULL,
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,        -- BIN+last4，展示 / 取证用
    `network`      VARCHAR(16)  DEFAULT NULL,        -- visa / mastercard / ...
    `result`       VARCHAR(16)  NOT NULL,             -- ok / denied / error
    `reason`       VARCHAR(256) DEFAULT NULL,
    `trace_id`     VARCHAR(64)  DEFAULT NULL,
    `prev_hash`    CHAR(64)     DEFAULT NULL,         -- 前一行 row_hash（链式签名 tamper-evident）
    `row_hash`     CHAR(64)     NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_op_created`  (`op`, `created_at`),
    KEY `idx_caller`      (`caller`),
    KEY `idx_user`        (`user_id`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_token_hash`  (`token_hash`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log (7y retention)';
