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
