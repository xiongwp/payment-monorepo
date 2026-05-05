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
