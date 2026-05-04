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
