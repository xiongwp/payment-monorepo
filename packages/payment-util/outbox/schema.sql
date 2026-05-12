-- outbox.sql — 各服务接入 outbox 前先建这张表 (一份 / DB).
--
-- 表名约定:  tx_outbox  (跟其他业务表用 tx_ 前缀区分; 命名空间清晰)
-- 分库分表:  不分; outbox 只是 staging, publisher 一发即删
--           (或保留 published 7d 给审计 / 重放; 然后 cron 清)
-- size:     稳态下 row < 10w (publish 速度 >> 业务速度); 不需要分片

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

  -- publisher 扫表的核心索引: WHERE status='pending' ORDER BY created_at
  KEY `idx_pending_age` (`status`, `created_at`),

  -- 给 DLQ 操作员排查 (按 aggregate 找事件)
  KEY `idx_aggregate`   (`aggregate`, `aggregate_id`, `created_at`),

  -- 给清理任务 (7d 之前已发送的)
  KEY `idx_cleanup`     (`status`, `published_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci
  COMMENT='transactional outbox — 业务事件待发 Kafka';
