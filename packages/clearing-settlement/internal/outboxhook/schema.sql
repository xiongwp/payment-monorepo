-- 跟 payment-util/outbox/schema.sql 同结构.
-- 各服务 DB 都需要一份 (跟业务行同事务才能写).

CREATE TABLE IF NOT EXISTS `tx_outbox` (
  `event_id`      VARCHAR(48)     NOT NULL,
  `aggregate`     VARCHAR(64)     NOT NULL,
  `aggregate_id`  VARCHAR(128)    NOT NULL,
  `event_type`    VARCHAR(96)     NOT NULL,
  `payload`       MEDIUMBLOB      NOT NULL,
  `topic`         VARCHAR(128)    NOT NULL DEFAULT '',
  `headers_json`  JSON            NULL,
  `created_at`    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `status`        ENUM('pending','published','failed') NOT NULL DEFAULT 'pending',
  `published_at`  DATETIME(3)     NULL,
  `retry_count`   INT             NOT NULL DEFAULT 0,
  `last_error`    TEXT            NULL,
  PRIMARY KEY (`event_id`),
  KEY `idx_pending_age` (`status`, `created_at`),
  KEY `idx_aggregate`   (`aggregate`, `aggregate_id`, `created_at`),
  KEY `idx_cleanup`     (`status`, `published_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci
  COMMENT='clearing-settlement outbox';
