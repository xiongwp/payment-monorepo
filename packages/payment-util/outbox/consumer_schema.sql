-- outbox_processed — 消费端 dedup 表.
-- 每个 consumer 服务 DB 都要一份, 跟业务行同事务写防重.

CREATE TABLE IF NOT EXISTS `outbox_processed` (
  `event_id`     VARCHAR(48)  NOT NULL,
  `source`       VARCHAR(64)  NOT NULL COMMENT 'consumer 标识, eg "accounting" / "merchant-webhook"',
  `event_type`   VARCHAR(96)  NOT NULL,
  `processed_at` DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`event_id`, `source`),
  KEY `idx_processed_at` (`processed_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci
  COMMENT='outbox consumer dedup — 防重处理';
