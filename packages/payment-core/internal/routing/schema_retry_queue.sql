-- payment_retry_queue: payment-core 的 charge 失败重试队列 (DBRetryQueue 后端)
--
-- 字段说明见 packages/payment-core/internal/routing/fallback.go::RetryTask
--
-- state 状态机:
--   pending  -> leased       (Dequeue 抢占)
--   leased   -> pending      (MarkRetry / lease 超时)
--   leased   -> done         (MarkSuccess)
--   pending  -> done         (容错路径)
--
-- 物理删除由 PurgeDoneOlderThan cron 每日 03:00 跑,保留 7 天 done 记录用于审计追溯。

CREATE TABLE IF NOT EXISTS payment_retry_queue (
    id                  VARCHAR(64)  NOT NULL,
    payment_intent_id   VARCHAR(64)  NOT NULL,
    idempotency_key     VARCHAR(128) NOT NULL,
    amount              BIGINT       NOT NULL,
    currency            VARCHAR(8)   NOT NULL,
    payment_method      VARCHAR(32)  NOT NULL DEFAULT '',
    country             VARCHAR(4)   NOT NULL DEFAULT '',
    bin                 VARCHAR(16)  NOT NULL DEFAULT '',
    failed_adapter      VARCHAR(64)  NOT NULL DEFAULT '',
    reason              VARCHAR(64)  NOT NULL DEFAULT '',
    attempt             INT          NOT NULL DEFAULT 0,
    next_retry_at       DATETIME(6)  NOT NULL,
    last_error_msg      TEXT,
    metadata            JSON,
    state               ENUM('pending','leased','done') NOT NULL DEFAULT 'pending',
    lease_owner         VARCHAR(128) DEFAULT NULL,
    lease_expires_at    DATETIME(6)  DEFAULT NULL,
    created_at          DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at          DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uk_pi_idem (payment_intent_id, idempotency_key),
    KEY idx_state_next   (state, next_retry_at),
    KEY idx_lease        (lease_owner, lease_expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='payment-core retry queue (DBRetryQueue backend)';
