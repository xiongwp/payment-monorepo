-- ┌──────────────────────────────────────────────────────────────────────┐
-- │ 共享 MySQL shard 2 —— paychan_db_2 + order_db_2 +
-- │ accounting_db_2 + user_merchant_db_2                            │
-- └──────────────────────────────────────────────────────────────────────┘

-- ==== payment-channel ====
CREATE DATABASE IF NOT EXISTS `paychan_db_2` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `paychan_db_2`;

-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 20 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_20` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `aq_id`              VARCHAR(32)  NOT NULL COMMENT '本仓生成 aq_<db><tbl><seq>',
  `pi_id`              VARCHAR(32)  NOT NULL COMMENT '上游 PI ID，分片键',
  `adapter`            VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/grabpay/...',
  `action`             VARCHAR(32)  NOT NULL COMMENT 'charge/refund/capture/void/query',
  `idempotency_key`    VARCHAR(64)  NOT NULL COMMENT 'sha256(pi_id:action)',
  `state`              VARCHAR(16)  NOT NULL COMMENT 'pending/unknown/succeeded/failed',
  `external_ref_no`    VARCHAR(64)  DEFAULT NULL COMMENT '渠道返回的流水号',
  `amount`             BIGINT       DEFAULT NULL COMMENT '单位：分',
  `currency`           VARCHAR(8)   DEFAULT NULL,
  `failure_code`       VARCHAR(32)  DEFAULT NULL COMMENT '归一后的失败码',
  `raw_failure_code`   VARCHAR(64)  DEFAULT NULL COMMENT '渠道原始错误码',
  `request_method`     VARCHAR(8)   NOT NULL,
  `request_url`        VARCHAR(512) NOT NULL,
  `request_headers`    JSON         DEFAULT NULL,
  `request_body`       MEDIUMTEXT   DEFAULT NULL COMMENT '明文或密文 JSON',
  `response_status`    INT          DEFAULT NULL,
  `response_headers`   JSON         DEFAULT NULL,
  `response_body`      MEDIUMTEXT   DEFAULT NULL,
  `latency_ms`         INT          DEFAULT NULL,
  `attempt`            INT          NOT NULL DEFAULT 1,
  `next_retry_at`      DATETIME     DEFAULT NULL,
  `last_query_at`      DATETIME     DEFAULT NULL COMMENT 'PendingQueryWorker 上次 Query 推进时间',
  `query_count`        INT          NOT NULL DEFAULT 0 COMMENT 'Query 推进次数；超过上限走 stuck-cancel',
  `created_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_aq_id` (`aq_id`),
  UNIQUE KEY `uk_idem` (`adapter`, `idempotency_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_retry` (`state`, `next_retry_at`),
  KEY `idx_unknown` (`state`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2. 原始 webhook 记录：入表做幂等 + 审计，异步消费。
--    dedupe_key = sha256(adapter:event_id:event_type)（修复 P2-5）—— 同一 event_id
--    不同 event_type 不再误判 dedup。
CREATE TABLE IF NOT EXISTS `webhook_raw_20` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`          VARCHAR(32) NOT NULL COMMENT '从 body 解出的 PI，分片键',
  `adapter`        VARCHAR(32) NOT NULL,
  `event_id`       VARCHAR(64) DEFAULT NULL COMMENT '渠道事件 ID（若渠道不提供则自算 hash）',
  `event_type`     VARCHAR(64) DEFAULT NULL,
  `dedupe_key`     VARCHAR(96) NOT NULL COMMENT 'sha256(adapter:event_id:event_type)',
  `signature_ok`   TINYINT(1)  NOT NULL DEFAULT 0,
  `forwarded`      TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '是否已转发给 order-core',
  `forward_err`    VARCHAR(255) DEFAULT NULL,
  `headers`        JSON        DEFAULT NULL,
  `body`           MEDIUMTEXT  NOT NULL,
  `received_at`    DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `forwarded_at`   DATETIME    DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_dedupe` (`dedupe_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_pending` (`forwarded`, `received_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2b. 拒收的 webhook 单独审计表（修复 P1-3）：签名 fail / 金额 mismatch 等
--     事件不写主 webhook_raw 的 UNIQUE 槽位，只在这里做 audit。无 dedupe UNIQUE。
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_20` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`        VARCHAR(32)  DEFAULT NULL,
  `adapter`      VARCHAR(32)  NOT NULL,
  `event_id`     VARCHAR(64)  DEFAULT NULL,
  `event_type`   VARCHAR(64)  DEFAULT NULL,
  `reason`       VARCHAR(32)  NOT NULL COMMENT 'signature_fail/amount_mismatch/replay_window/...',
  `headers`      MEDIUMTEXT   DEFAULT NULL,
  `body`         MEDIUMTEXT   NOT NULL,
  `received_at`  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_adapter_reason` (`adapter`, `reason`, `received_at`),
  KEY `idx_event` (`adapter`, `event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='webhook 拒收审计，独立于 webhook_raw 防止占 UNIQUE';

-- 3. 渠道 token / mandate 映射（用户在本渠道的长期绑定）。
CREATE TABLE IF NOT EXISTS `channel_token_20` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`            VARCHAR(32) NOT NULL COMMENT '首次创建时的 pi，分片键',
  `customer_ref`     VARCHAR(64) NOT NULL COMMENT 'payment-core 透传的 customer id',
  `adapter`          VARCHAR(32) NOT NULL,
  `token`            VARCHAR(512) NOT NULL COMMENT '密文存储（AES-GCM）',
  `brand`            VARCHAR(32) DEFAULT NULL,
  `last4`            VARCHAR(8)  DEFAULT NULL,
  `status`           VARCHAR(16) NOT NULL DEFAULT 'active',
  `expires_at`       DATETIME    DEFAULT NULL,
  `created_at`       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_customer` (`customer_ref`, `adapter`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 21 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_21` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `aq_id`              VARCHAR(32)  NOT NULL COMMENT '本仓生成 aq_<db><tbl><seq>',
  `pi_id`              VARCHAR(32)  NOT NULL COMMENT '上游 PI ID，分片键',
  `adapter`            VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/grabpay/...',
  `action`             VARCHAR(32)  NOT NULL COMMENT 'charge/refund/capture/void/query',
  `idempotency_key`    VARCHAR(64)  NOT NULL COMMENT 'sha256(pi_id:action)',
  `state`              VARCHAR(16)  NOT NULL COMMENT 'pending/unknown/succeeded/failed',
  `external_ref_no`    VARCHAR(64)  DEFAULT NULL COMMENT '渠道返回的流水号',
  `amount`             BIGINT       DEFAULT NULL COMMENT '单位：分',
  `currency`           VARCHAR(8)   DEFAULT NULL,
  `failure_code`       VARCHAR(32)  DEFAULT NULL COMMENT '归一后的失败码',
  `raw_failure_code`   VARCHAR(64)  DEFAULT NULL COMMENT '渠道原始错误码',
  `request_method`     VARCHAR(8)   NOT NULL,
  `request_url`        VARCHAR(512) NOT NULL,
  `request_headers`    JSON         DEFAULT NULL,
  `request_body`       MEDIUMTEXT   DEFAULT NULL COMMENT '明文或密文 JSON',
  `response_status`    INT          DEFAULT NULL,
  `response_headers`   JSON         DEFAULT NULL,
  `response_body`      MEDIUMTEXT   DEFAULT NULL,
  `latency_ms`         INT          DEFAULT NULL,
  `attempt`            INT          NOT NULL DEFAULT 1,
  `next_retry_at`      DATETIME     DEFAULT NULL,
  `last_query_at`      DATETIME     DEFAULT NULL COMMENT 'PendingQueryWorker 上次 Query 推进时间',
  `query_count`        INT          NOT NULL DEFAULT 0 COMMENT 'Query 推进次数；超过上限走 stuck-cancel',
  `created_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_aq_id` (`aq_id`),
  UNIQUE KEY `uk_idem` (`adapter`, `idempotency_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_retry` (`state`, `next_retry_at`),
  KEY `idx_unknown` (`state`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2. 原始 webhook 记录：入表做幂等 + 审计，异步消费。
--    dedupe_key = sha256(adapter:event_id:event_type)（修复 P2-5）—— 同一 event_id
--    不同 event_type 不再误判 dedup。
CREATE TABLE IF NOT EXISTS `webhook_raw_21` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`          VARCHAR(32) NOT NULL COMMENT '从 body 解出的 PI，分片键',
  `adapter`        VARCHAR(32) NOT NULL,
  `event_id`       VARCHAR(64) DEFAULT NULL COMMENT '渠道事件 ID（若渠道不提供则自算 hash）',
  `event_type`     VARCHAR(64) DEFAULT NULL,
  `dedupe_key`     VARCHAR(96) NOT NULL COMMENT 'sha256(adapter:event_id:event_type)',
  `signature_ok`   TINYINT(1)  NOT NULL DEFAULT 0,
  `forwarded`      TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '是否已转发给 order-core',
  `forward_err`    VARCHAR(255) DEFAULT NULL,
  `headers`        JSON        DEFAULT NULL,
  `body`           MEDIUMTEXT  NOT NULL,
  `received_at`    DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `forwarded_at`   DATETIME    DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_dedupe` (`dedupe_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_pending` (`forwarded`, `received_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2b. 拒收的 webhook 单独审计表（修复 P1-3）：签名 fail / 金额 mismatch 等
--     事件不写主 webhook_raw 的 UNIQUE 槽位，只在这里做 audit。无 dedupe UNIQUE。
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_21` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`        VARCHAR(32)  DEFAULT NULL,
  `adapter`      VARCHAR(32)  NOT NULL,
  `event_id`     VARCHAR(64)  DEFAULT NULL,
  `event_type`   VARCHAR(64)  DEFAULT NULL,
  `reason`       VARCHAR(32)  NOT NULL COMMENT 'signature_fail/amount_mismatch/replay_window/...',
  `headers`      MEDIUMTEXT   DEFAULT NULL,
  `body`         MEDIUMTEXT   NOT NULL,
  `received_at`  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_adapter_reason` (`adapter`, `reason`, `received_at`),
  KEY `idx_event` (`adapter`, `event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='webhook 拒收审计，独立于 webhook_raw 防止占 UNIQUE';

-- 3. 渠道 token / mandate 映射（用户在本渠道的长期绑定）。
CREATE TABLE IF NOT EXISTS `channel_token_21` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`            VARCHAR(32) NOT NULL COMMENT '首次创建时的 pi，分片键',
  `customer_ref`     VARCHAR(64) NOT NULL COMMENT 'payment-core 透传的 customer id',
  `adapter`          VARCHAR(32) NOT NULL,
  `token`            VARCHAR(512) NOT NULL COMMENT '密文存储（AES-GCM）',
  `brand`            VARCHAR(32) DEFAULT NULL,
  `last4`            VARCHAR(8)  DEFAULT NULL,
  `status`           VARCHAR(16) NOT NULL DEFAULT 'active',
  `expires_at`       DATETIME    DEFAULT NULL,
  `created_at`       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_customer` (`customer_ref`, `adapter`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 22 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_22` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `aq_id`              VARCHAR(32)  NOT NULL COMMENT '本仓生成 aq_<db><tbl><seq>',
  `pi_id`              VARCHAR(32)  NOT NULL COMMENT '上游 PI ID，分片键',
  `adapter`            VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/grabpay/...',
  `action`             VARCHAR(32)  NOT NULL COMMENT 'charge/refund/capture/void/query',
  `idempotency_key`    VARCHAR(64)  NOT NULL COMMENT 'sha256(pi_id:action)',
  `state`              VARCHAR(16)  NOT NULL COMMENT 'pending/unknown/succeeded/failed',
  `external_ref_no`    VARCHAR(64)  DEFAULT NULL COMMENT '渠道返回的流水号',
  `amount`             BIGINT       DEFAULT NULL COMMENT '单位：分',
  `currency`           VARCHAR(8)   DEFAULT NULL,
  `failure_code`       VARCHAR(32)  DEFAULT NULL COMMENT '归一后的失败码',
  `raw_failure_code`   VARCHAR(64)  DEFAULT NULL COMMENT '渠道原始错误码',
  `request_method`     VARCHAR(8)   NOT NULL,
  `request_url`        VARCHAR(512) NOT NULL,
  `request_headers`    JSON         DEFAULT NULL,
  `request_body`       MEDIUMTEXT   DEFAULT NULL COMMENT '明文或密文 JSON',
  `response_status`    INT          DEFAULT NULL,
  `response_headers`   JSON         DEFAULT NULL,
  `response_body`      MEDIUMTEXT   DEFAULT NULL,
  `latency_ms`         INT          DEFAULT NULL,
  `attempt`            INT          NOT NULL DEFAULT 1,
  `next_retry_at`      DATETIME     DEFAULT NULL,
  `last_query_at`      DATETIME     DEFAULT NULL COMMENT 'PendingQueryWorker 上次 Query 推进时间',
  `query_count`        INT          NOT NULL DEFAULT 0 COMMENT 'Query 推进次数；超过上限走 stuck-cancel',
  `created_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_aq_id` (`aq_id`),
  UNIQUE KEY `uk_idem` (`adapter`, `idempotency_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_retry` (`state`, `next_retry_at`),
  KEY `idx_unknown` (`state`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2. 原始 webhook 记录：入表做幂等 + 审计，异步消费。
--    dedupe_key = sha256(adapter:event_id:event_type)（修复 P2-5）—— 同一 event_id
--    不同 event_type 不再误判 dedup。
CREATE TABLE IF NOT EXISTS `webhook_raw_22` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`          VARCHAR(32) NOT NULL COMMENT '从 body 解出的 PI，分片键',
  `adapter`        VARCHAR(32) NOT NULL,
  `event_id`       VARCHAR(64) DEFAULT NULL COMMENT '渠道事件 ID（若渠道不提供则自算 hash）',
  `event_type`     VARCHAR(64) DEFAULT NULL,
  `dedupe_key`     VARCHAR(96) NOT NULL COMMENT 'sha256(adapter:event_id:event_type)',
  `signature_ok`   TINYINT(1)  NOT NULL DEFAULT 0,
  `forwarded`      TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '是否已转发给 order-core',
  `forward_err`    VARCHAR(255) DEFAULT NULL,
  `headers`        JSON        DEFAULT NULL,
  `body`           MEDIUMTEXT  NOT NULL,
  `received_at`    DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `forwarded_at`   DATETIME    DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_dedupe` (`dedupe_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_pending` (`forwarded`, `received_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2b. 拒收的 webhook 单独审计表（修复 P1-3）：签名 fail / 金额 mismatch 等
--     事件不写主 webhook_raw 的 UNIQUE 槽位，只在这里做 audit。无 dedupe UNIQUE。
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_22` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`        VARCHAR(32)  DEFAULT NULL,
  `adapter`      VARCHAR(32)  NOT NULL,
  `event_id`     VARCHAR(64)  DEFAULT NULL,
  `event_type`   VARCHAR(64)  DEFAULT NULL,
  `reason`       VARCHAR(32)  NOT NULL COMMENT 'signature_fail/amount_mismatch/replay_window/...',
  `headers`      MEDIUMTEXT   DEFAULT NULL,
  `body`         MEDIUMTEXT   NOT NULL,
  `received_at`  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_adapter_reason` (`adapter`, `reason`, `received_at`),
  KEY `idx_event` (`adapter`, `event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='webhook 拒收审计，独立于 webhook_raw 防止占 UNIQUE';

-- 3. 渠道 token / mandate 映射（用户在本渠道的长期绑定）。
CREATE TABLE IF NOT EXISTS `channel_token_22` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`            VARCHAR(32) NOT NULL COMMENT '首次创建时的 pi，分片键',
  `customer_ref`     VARCHAR(64) NOT NULL COMMENT 'payment-core 透传的 customer id',
  `adapter`          VARCHAR(32) NOT NULL,
  `token`            VARCHAR(512) NOT NULL COMMENT '密文存储（AES-GCM）',
  `brand`            VARCHAR(32) DEFAULT NULL,
  `last4`            VARCHAR(8)  DEFAULT NULL,
  `status`           VARCHAR(16) NOT NULL DEFAULT 'active',
  `expires_at`       DATETIME    DEFAULT NULL,
  `created_at`       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_customer` (`customer_ref`, `adapter`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 23 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_23` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `aq_id`              VARCHAR(32)  NOT NULL COMMENT '本仓生成 aq_<db><tbl><seq>',
  `pi_id`              VARCHAR(32)  NOT NULL COMMENT '上游 PI ID，分片键',
  `adapter`            VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/grabpay/...',
  `action`             VARCHAR(32)  NOT NULL COMMENT 'charge/refund/capture/void/query',
  `idempotency_key`    VARCHAR(64)  NOT NULL COMMENT 'sha256(pi_id:action)',
  `state`              VARCHAR(16)  NOT NULL COMMENT 'pending/unknown/succeeded/failed',
  `external_ref_no`    VARCHAR(64)  DEFAULT NULL COMMENT '渠道返回的流水号',
  `amount`             BIGINT       DEFAULT NULL COMMENT '单位：分',
  `currency`           VARCHAR(8)   DEFAULT NULL,
  `failure_code`       VARCHAR(32)  DEFAULT NULL COMMENT '归一后的失败码',
  `raw_failure_code`   VARCHAR(64)  DEFAULT NULL COMMENT '渠道原始错误码',
  `request_method`     VARCHAR(8)   NOT NULL,
  `request_url`        VARCHAR(512) NOT NULL,
  `request_headers`    JSON         DEFAULT NULL,
  `request_body`       MEDIUMTEXT   DEFAULT NULL COMMENT '明文或密文 JSON',
  `response_status`    INT          DEFAULT NULL,
  `response_headers`   JSON         DEFAULT NULL,
  `response_body`      MEDIUMTEXT   DEFAULT NULL,
  `latency_ms`         INT          DEFAULT NULL,
  `attempt`            INT          NOT NULL DEFAULT 1,
  `next_retry_at`      DATETIME     DEFAULT NULL,
  `last_query_at`      DATETIME     DEFAULT NULL COMMENT 'PendingQueryWorker 上次 Query 推进时间',
  `query_count`        INT          NOT NULL DEFAULT 0 COMMENT 'Query 推进次数；超过上限走 stuck-cancel',
  `created_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_aq_id` (`aq_id`),
  UNIQUE KEY `uk_idem` (`adapter`, `idempotency_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_retry` (`state`, `next_retry_at`),
  KEY `idx_unknown` (`state`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2. 原始 webhook 记录：入表做幂等 + 审计，异步消费。
--    dedupe_key = sha256(adapter:event_id:event_type)（修复 P2-5）—— 同一 event_id
--    不同 event_type 不再误判 dedup。
CREATE TABLE IF NOT EXISTS `webhook_raw_23` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`          VARCHAR(32) NOT NULL COMMENT '从 body 解出的 PI，分片键',
  `adapter`        VARCHAR(32) NOT NULL,
  `event_id`       VARCHAR(64) DEFAULT NULL COMMENT '渠道事件 ID（若渠道不提供则自算 hash）',
  `event_type`     VARCHAR(64) DEFAULT NULL,
  `dedupe_key`     VARCHAR(96) NOT NULL COMMENT 'sha256(adapter:event_id:event_type)',
  `signature_ok`   TINYINT(1)  NOT NULL DEFAULT 0,
  `forwarded`      TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '是否已转发给 order-core',
  `forward_err`    VARCHAR(255) DEFAULT NULL,
  `headers`        JSON        DEFAULT NULL,
  `body`           MEDIUMTEXT  NOT NULL,
  `received_at`    DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `forwarded_at`   DATETIME    DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_dedupe` (`dedupe_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_pending` (`forwarded`, `received_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2b. 拒收的 webhook 单独审计表（修复 P1-3）：签名 fail / 金额 mismatch 等
--     事件不写主 webhook_raw 的 UNIQUE 槽位，只在这里做 audit。无 dedupe UNIQUE。
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_23` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`        VARCHAR(32)  DEFAULT NULL,
  `adapter`      VARCHAR(32)  NOT NULL,
  `event_id`     VARCHAR(64)  DEFAULT NULL,
  `event_type`   VARCHAR(64)  DEFAULT NULL,
  `reason`       VARCHAR(32)  NOT NULL COMMENT 'signature_fail/amount_mismatch/replay_window/...',
  `headers`      MEDIUMTEXT   DEFAULT NULL,
  `body`         MEDIUMTEXT   NOT NULL,
  `received_at`  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_adapter_reason` (`adapter`, `reason`, `received_at`),
  KEY `idx_event` (`adapter`, `event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='webhook 拒收审计，独立于 webhook_raw 防止占 UNIQUE';

-- 3. 渠道 token / mandate 映射（用户在本渠道的长期绑定）。
CREATE TABLE IF NOT EXISTS `channel_token_23` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`            VARCHAR(32) NOT NULL COMMENT '首次创建时的 pi，分片键',
  `customer_ref`     VARCHAR(64) NOT NULL COMMENT 'payment-core 透传的 customer id',
  `adapter`          VARCHAR(32) NOT NULL,
  `token`            VARCHAR(512) NOT NULL COMMENT '密文存储（AES-GCM）',
  `brand`            VARCHAR(32) DEFAULT NULL,
  `last4`            VARCHAR(8)  DEFAULT NULL,
  `status`           VARCHAR(16) NOT NULL DEFAULT 'active',
  `expires_at`       DATETIME    DEFAULT NULL,
  `created_at`       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_customer` (`customer_ref`, `adapter`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 24 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_24` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `aq_id`              VARCHAR(32)  NOT NULL COMMENT '本仓生成 aq_<db><tbl><seq>',
  `pi_id`              VARCHAR(32)  NOT NULL COMMENT '上游 PI ID，分片键',
  `adapter`            VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/grabpay/...',
  `action`             VARCHAR(32)  NOT NULL COMMENT 'charge/refund/capture/void/query',
  `idempotency_key`    VARCHAR(64)  NOT NULL COMMENT 'sha256(pi_id:action)',
  `state`              VARCHAR(16)  NOT NULL COMMENT 'pending/unknown/succeeded/failed',
  `external_ref_no`    VARCHAR(64)  DEFAULT NULL COMMENT '渠道返回的流水号',
  `amount`             BIGINT       DEFAULT NULL COMMENT '单位：分',
  `currency`           VARCHAR(8)   DEFAULT NULL,
  `failure_code`       VARCHAR(32)  DEFAULT NULL COMMENT '归一后的失败码',
  `raw_failure_code`   VARCHAR(64)  DEFAULT NULL COMMENT '渠道原始错误码',
  `request_method`     VARCHAR(8)   NOT NULL,
  `request_url`        VARCHAR(512) NOT NULL,
  `request_headers`    JSON         DEFAULT NULL,
  `request_body`       MEDIUMTEXT   DEFAULT NULL COMMENT '明文或密文 JSON',
  `response_status`    INT          DEFAULT NULL,
  `response_headers`   JSON         DEFAULT NULL,
  `response_body`      MEDIUMTEXT   DEFAULT NULL,
  `latency_ms`         INT          DEFAULT NULL,
  `attempt`            INT          NOT NULL DEFAULT 1,
  `next_retry_at`      DATETIME     DEFAULT NULL,
  `last_query_at`      DATETIME     DEFAULT NULL COMMENT 'PendingQueryWorker 上次 Query 推进时间',
  `query_count`        INT          NOT NULL DEFAULT 0 COMMENT 'Query 推进次数；超过上限走 stuck-cancel',
  `created_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_aq_id` (`aq_id`),
  UNIQUE KEY `uk_idem` (`adapter`, `idempotency_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_retry` (`state`, `next_retry_at`),
  KEY `idx_unknown` (`state`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2. 原始 webhook 记录：入表做幂等 + 审计，异步消费。
--    dedupe_key = sha256(adapter:event_id:event_type)（修复 P2-5）—— 同一 event_id
--    不同 event_type 不再误判 dedup。
CREATE TABLE IF NOT EXISTS `webhook_raw_24` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`          VARCHAR(32) NOT NULL COMMENT '从 body 解出的 PI，分片键',
  `adapter`        VARCHAR(32) NOT NULL,
  `event_id`       VARCHAR(64) DEFAULT NULL COMMENT '渠道事件 ID（若渠道不提供则自算 hash）',
  `event_type`     VARCHAR(64) DEFAULT NULL,
  `dedupe_key`     VARCHAR(96) NOT NULL COMMENT 'sha256(adapter:event_id:event_type)',
  `signature_ok`   TINYINT(1)  NOT NULL DEFAULT 0,
  `forwarded`      TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '是否已转发给 order-core',
  `forward_err`    VARCHAR(255) DEFAULT NULL,
  `headers`        JSON        DEFAULT NULL,
  `body`           MEDIUMTEXT  NOT NULL,
  `received_at`    DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `forwarded_at`   DATETIME    DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_dedupe` (`dedupe_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_pending` (`forwarded`, `received_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2b. 拒收的 webhook 单独审计表（修复 P1-3）：签名 fail / 金额 mismatch 等
--     事件不写主 webhook_raw 的 UNIQUE 槽位，只在这里做 audit。无 dedupe UNIQUE。
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_24` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`        VARCHAR(32)  DEFAULT NULL,
  `adapter`      VARCHAR(32)  NOT NULL,
  `event_id`     VARCHAR(64)  DEFAULT NULL,
  `event_type`   VARCHAR(64)  DEFAULT NULL,
  `reason`       VARCHAR(32)  NOT NULL COMMENT 'signature_fail/amount_mismatch/replay_window/...',
  `headers`      MEDIUMTEXT   DEFAULT NULL,
  `body`         MEDIUMTEXT   NOT NULL,
  `received_at`  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_adapter_reason` (`adapter`, `reason`, `received_at`),
  KEY `idx_event` (`adapter`, `event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='webhook 拒收审计，独立于 webhook_raw 防止占 UNIQUE';

-- 3. 渠道 token / mandate 映射（用户在本渠道的长期绑定）。
CREATE TABLE IF NOT EXISTS `channel_token_24` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`            VARCHAR(32) NOT NULL COMMENT '首次创建时的 pi，分片键',
  `customer_ref`     VARCHAR(64) NOT NULL COMMENT 'payment-core 透传的 customer id',
  `adapter`          VARCHAR(32) NOT NULL,
  `token`            VARCHAR(512) NOT NULL COMMENT '密文存储（AES-GCM）',
  `brand`            VARCHAR(32) DEFAULT NULL,
  `last4`            VARCHAR(8)  DEFAULT NULL,
  `status`           VARCHAR(16) NOT NULL DEFAULT 'active',
  `expires_at`       DATETIME    DEFAULT NULL,
  `created_at`       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_customer` (`customer_ref`, `adapter`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 25 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_25` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `aq_id`              VARCHAR(32)  NOT NULL COMMENT '本仓生成 aq_<db><tbl><seq>',
  `pi_id`              VARCHAR(32)  NOT NULL COMMENT '上游 PI ID，分片键',
  `adapter`            VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/grabpay/...',
  `action`             VARCHAR(32)  NOT NULL COMMENT 'charge/refund/capture/void/query',
  `idempotency_key`    VARCHAR(64)  NOT NULL COMMENT 'sha256(pi_id:action)',
  `state`              VARCHAR(16)  NOT NULL COMMENT 'pending/unknown/succeeded/failed',
  `external_ref_no`    VARCHAR(64)  DEFAULT NULL COMMENT '渠道返回的流水号',
  `amount`             BIGINT       DEFAULT NULL COMMENT '单位：分',
  `currency`           VARCHAR(8)   DEFAULT NULL,
  `failure_code`       VARCHAR(32)  DEFAULT NULL COMMENT '归一后的失败码',
  `raw_failure_code`   VARCHAR(64)  DEFAULT NULL COMMENT '渠道原始错误码',
  `request_method`     VARCHAR(8)   NOT NULL,
  `request_url`        VARCHAR(512) NOT NULL,
  `request_headers`    JSON         DEFAULT NULL,
  `request_body`       MEDIUMTEXT   DEFAULT NULL COMMENT '明文或密文 JSON',
  `response_status`    INT          DEFAULT NULL,
  `response_headers`   JSON         DEFAULT NULL,
  `response_body`      MEDIUMTEXT   DEFAULT NULL,
  `latency_ms`         INT          DEFAULT NULL,
  `attempt`            INT          NOT NULL DEFAULT 1,
  `next_retry_at`      DATETIME     DEFAULT NULL,
  `last_query_at`      DATETIME     DEFAULT NULL COMMENT 'PendingQueryWorker 上次 Query 推进时间',
  `query_count`        INT          NOT NULL DEFAULT 0 COMMENT 'Query 推进次数；超过上限走 stuck-cancel',
  `created_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_aq_id` (`aq_id`),
  UNIQUE KEY `uk_idem` (`adapter`, `idempotency_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_retry` (`state`, `next_retry_at`),
  KEY `idx_unknown` (`state`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2. 原始 webhook 记录：入表做幂等 + 审计，异步消费。
--    dedupe_key = sha256(adapter:event_id:event_type)（修复 P2-5）—— 同一 event_id
--    不同 event_type 不再误判 dedup。
CREATE TABLE IF NOT EXISTS `webhook_raw_25` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`          VARCHAR(32) NOT NULL COMMENT '从 body 解出的 PI，分片键',
  `adapter`        VARCHAR(32) NOT NULL,
  `event_id`       VARCHAR(64) DEFAULT NULL COMMENT '渠道事件 ID（若渠道不提供则自算 hash）',
  `event_type`     VARCHAR(64) DEFAULT NULL,
  `dedupe_key`     VARCHAR(96) NOT NULL COMMENT 'sha256(adapter:event_id:event_type)',
  `signature_ok`   TINYINT(1)  NOT NULL DEFAULT 0,
  `forwarded`      TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '是否已转发给 order-core',
  `forward_err`    VARCHAR(255) DEFAULT NULL,
  `headers`        JSON        DEFAULT NULL,
  `body`           MEDIUMTEXT  NOT NULL,
  `received_at`    DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `forwarded_at`   DATETIME    DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_dedupe` (`dedupe_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_pending` (`forwarded`, `received_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2b. 拒收的 webhook 单独审计表（修复 P1-3）：签名 fail / 金额 mismatch 等
--     事件不写主 webhook_raw 的 UNIQUE 槽位，只在这里做 audit。无 dedupe UNIQUE。
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_25` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`        VARCHAR(32)  DEFAULT NULL,
  `adapter`      VARCHAR(32)  NOT NULL,
  `event_id`     VARCHAR(64)  DEFAULT NULL,
  `event_type`   VARCHAR(64)  DEFAULT NULL,
  `reason`       VARCHAR(32)  NOT NULL COMMENT 'signature_fail/amount_mismatch/replay_window/...',
  `headers`      MEDIUMTEXT   DEFAULT NULL,
  `body`         MEDIUMTEXT   NOT NULL,
  `received_at`  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_adapter_reason` (`adapter`, `reason`, `received_at`),
  KEY `idx_event` (`adapter`, `event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='webhook 拒收审计，独立于 webhook_raw 防止占 UNIQUE';

-- 3. 渠道 token / mandate 映射（用户在本渠道的长期绑定）。
CREATE TABLE IF NOT EXISTS `channel_token_25` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`            VARCHAR(32) NOT NULL COMMENT '首次创建时的 pi，分片键',
  `customer_ref`     VARCHAR(64) NOT NULL COMMENT 'payment-core 透传的 customer id',
  `adapter`          VARCHAR(32) NOT NULL,
  `token`            VARCHAR(512) NOT NULL COMMENT '密文存储（AES-GCM）',
  `brand`            VARCHAR(32) DEFAULT NULL,
  `last4`            VARCHAR(8)  DEFAULT NULL,
  `status`           VARCHAR(16) NOT NULL DEFAULT 'active',
  `expires_at`       DATETIME    DEFAULT NULL,
  `created_at`       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_customer` (`customer_ref`, `adapter`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 26 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_26` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `aq_id`              VARCHAR(32)  NOT NULL COMMENT '本仓生成 aq_<db><tbl><seq>',
  `pi_id`              VARCHAR(32)  NOT NULL COMMENT '上游 PI ID，分片键',
  `adapter`            VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/grabpay/...',
  `action`             VARCHAR(32)  NOT NULL COMMENT 'charge/refund/capture/void/query',
  `idempotency_key`    VARCHAR(64)  NOT NULL COMMENT 'sha256(pi_id:action)',
  `state`              VARCHAR(16)  NOT NULL COMMENT 'pending/unknown/succeeded/failed',
  `external_ref_no`    VARCHAR(64)  DEFAULT NULL COMMENT '渠道返回的流水号',
  `amount`             BIGINT       DEFAULT NULL COMMENT '单位：分',
  `currency`           VARCHAR(8)   DEFAULT NULL,
  `failure_code`       VARCHAR(32)  DEFAULT NULL COMMENT '归一后的失败码',
  `raw_failure_code`   VARCHAR(64)  DEFAULT NULL COMMENT '渠道原始错误码',
  `request_method`     VARCHAR(8)   NOT NULL,
  `request_url`        VARCHAR(512) NOT NULL,
  `request_headers`    JSON         DEFAULT NULL,
  `request_body`       MEDIUMTEXT   DEFAULT NULL COMMENT '明文或密文 JSON',
  `response_status`    INT          DEFAULT NULL,
  `response_headers`   JSON         DEFAULT NULL,
  `response_body`      MEDIUMTEXT   DEFAULT NULL,
  `latency_ms`         INT          DEFAULT NULL,
  `attempt`            INT          NOT NULL DEFAULT 1,
  `next_retry_at`      DATETIME     DEFAULT NULL,
  `last_query_at`      DATETIME     DEFAULT NULL COMMENT 'PendingQueryWorker 上次 Query 推进时间',
  `query_count`        INT          NOT NULL DEFAULT 0 COMMENT 'Query 推进次数；超过上限走 stuck-cancel',
  `created_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_aq_id` (`aq_id`),
  UNIQUE KEY `uk_idem` (`adapter`, `idempotency_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_retry` (`state`, `next_retry_at`),
  KEY `idx_unknown` (`state`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2. 原始 webhook 记录：入表做幂等 + 审计，异步消费。
--    dedupe_key = sha256(adapter:event_id:event_type)（修复 P2-5）—— 同一 event_id
--    不同 event_type 不再误判 dedup。
CREATE TABLE IF NOT EXISTS `webhook_raw_26` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`          VARCHAR(32) NOT NULL COMMENT '从 body 解出的 PI，分片键',
  `adapter`        VARCHAR(32) NOT NULL,
  `event_id`       VARCHAR(64) DEFAULT NULL COMMENT '渠道事件 ID（若渠道不提供则自算 hash）',
  `event_type`     VARCHAR(64) DEFAULT NULL,
  `dedupe_key`     VARCHAR(96) NOT NULL COMMENT 'sha256(adapter:event_id:event_type)',
  `signature_ok`   TINYINT(1)  NOT NULL DEFAULT 0,
  `forwarded`      TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '是否已转发给 order-core',
  `forward_err`    VARCHAR(255) DEFAULT NULL,
  `headers`        JSON        DEFAULT NULL,
  `body`           MEDIUMTEXT  NOT NULL,
  `received_at`    DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `forwarded_at`   DATETIME    DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_dedupe` (`dedupe_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_pending` (`forwarded`, `received_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2b. 拒收的 webhook 单独审计表（修复 P1-3）：签名 fail / 金额 mismatch 等
--     事件不写主 webhook_raw 的 UNIQUE 槽位，只在这里做 audit。无 dedupe UNIQUE。
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_26` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`        VARCHAR(32)  DEFAULT NULL,
  `adapter`      VARCHAR(32)  NOT NULL,
  `event_id`     VARCHAR(64)  DEFAULT NULL,
  `event_type`   VARCHAR(64)  DEFAULT NULL,
  `reason`       VARCHAR(32)  NOT NULL COMMENT 'signature_fail/amount_mismatch/replay_window/...',
  `headers`      MEDIUMTEXT   DEFAULT NULL,
  `body`         MEDIUMTEXT   NOT NULL,
  `received_at`  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_adapter_reason` (`adapter`, `reason`, `received_at`),
  KEY `idx_event` (`adapter`, `event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='webhook 拒收审计，独立于 webhook_raw 防止占 UNIQUE';

-- 3. 渠道 token / mandate 映射（用户在本渠道的长期绑定）。
CREATE TABLE IF NOT EXISTS `channel_token_26` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`            VARCHAR(32) NOT NULL COMMENT '首次创建时的 pi，分片键',
  `customer_ref`     VARCHAR(64) NOT NULL COMMENT 'payment-core 透传的 customer id',
  `adapter`          VARCHAR(32) NOT NULL,
  `token`            VARCHAR(512) NOT NULL COMMENT '密文存储（AES-GCM）',
  `brand`            VARCHAR(32) DEFAULT NULL,
  `last4`            VARCHAR(8)  DEFAULT NULL,
  `status`           VARCHAR(16) NOT NULL DEFAULT 'active',
  `expires_at`       DATETIME    DEFAULT NULL,
  `created_at`       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_customer` (`customer_ref`, `adapter`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 27 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_27` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `aq_id`              VARCHAR(32)  NOT NULL COMMENT '本仓生成 aq_<db><tbl><seq>',
  `pi_id`              VARCHAR(32)  NOT NULL COMMENT '上游 PI ID，分片键',
  `adapter`            VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/grabpay/...',
  `action`             VARCHAR(32)  NOT NULL COMMENT 'charge/refund/capture/void/query',
  `idempotency_key`    VARCHAR(64)  NOT NULL COMMENT 'sha256(pi_id:action)',
  `state`              VARCHAR(16)  NOT NULL COMMENT 'pending/unknown/succeeded/failed',
  `external_ref_no`    VARCHAR(64)  DEFAULT NULL COMMENT '渠道返回的流水号',
  `amount`             BIGINT       DEFAULT NULL COMMENT '单位：分',
  `currency`           VARCHAR(8)   DEFAULT NULL,
  `failure_code`       VARCHAR(32)  DEFAULT NULL COMMENT '归一后的失败码',
  `raw_failure_code`   VARCHAR(64)  DEFAULT NULL COMMENT '渠道原始错误码',
  `request_method`     VARCHAR(8)   NOT NULL,
  `request_url`        VARCHAR(512) NOT NULL,
  `request_headers`    JSON         DEFAULT NULL,
  `request_body`       MEDIUMTEXT   DEFAULT NULL COMMENT '明文或密文 JSON',
  `response_status`    INT          DEFAULT NULL,
  `response_headers`   JSON         DEFAULT NULL,
  `response_body`      MEDIUMTEXT   DEFAULT NULL,
  `latency_ms`         INT          DEFAULT NULL,
  `attempt`            INT          NOT NULL DEFAULT 1,
  `next_retry_at`      DATETIME     DEFAULT NULL,
  `last_query_at`      DATETIME     DEFAULT NULL COMMENT 'PendingQueryWorker 上次 Query 推进时间',
  `query_count`        INT          NOT NULL DEFAULT 0 COMMENT 'Query 推进次数；超过上限走 stuck-cancel',
  `created_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_aq_id` (`aq_id`),
  UNIQUE KEY `uk_idem` (`adapter`, `idempotency_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_retry` (`state`, `next_retry_at`),
  KEY `idx_unknown` (`state`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2. 原始 webhook 记录：入表做幂等 + 审计，异步消费。
--    dedupe_key = sha256(adapter:event_id:event_type)（修复 P2-5）—— 同一 event_id
--    不同 event_type 不再误判 dedup。
CREATE TABLE IF NOT EXISTS `webhook_raw_27` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`          VARCHAR(32) NOT NULL COMMENT '从 body 解出的 PI，分片键',
  `adapter`        VARCHAR(32) NOT NULL,
  `event_id`       VARCHAR(64) DEFAULT NULL COMMENT '渠道事件 ID（若渠道不提供则自算 hash）',
  `event_type`     VARCHAR(64) DEFAULT NULL,
  `dedupe_key`     VARCHAR(96) NOT NULL COMMENT 'sha256(adapter:event_id:event_type)',
  `signature_ok`   TINYINT(1)  NOT NULL DEFAULT 0,
  `forwarded`      TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '是否已转发给 order-core',
  `forward_err`    VARCHAR(255) DEFAULT NULL,
  `headers`        JSON        DEFAULT NULL,
  `body`           MEDIUMTEXT  NOT NULL,
  `received_at`    DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `forwarded_at`   DATETIME    DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_dedupe` (`dedupe_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_pending` (`forwarded`, `received_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2b. 拒收的 webhook 单独审计表（修复 P1-3）：签名 fail / 金额 mismatch 等
--     事件不写主 webhook_raw 的 UNIQUE 槽位，只在这里做 audit。无 dedupe UNIQUE。
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_27` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`        VARCHAR(32)  DEFAULT NULL,
  `adapter`      VARCHAR(32)  NOT NULL,
  `event_id`     VARCHAR(64)  DEFAULT NULL,
  `event_type`   VARCHAR(64)  DEFAULT NULL,
  `reason`       VARCHAR(32)  NOT NULL COMMENT 'signature_fail/amount_mismatch/replay_window/...',
  `headers`      MEDIUMTEXT   DEFAULT NULL,
  `body`         MEDIUMTEXT   NOT NULL,
  `received_at`  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_adapter_reason` (`adapter`, `reason`, `received_at`),
  KEY `idx_event` (`adapter`, `event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='webhook 拒收审计，独立于 webhook_raw 防止占 UNIQUE';

-- 3. 渠道 token / mandate 映射（用户在本渠道的长期绑定）。
CREATE TABLE IF NOT EXISTS `channel_token_27` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`            VARCHAR(32) NOT NULL COMMENT '首次创建时的 pi，分片键',
  `customer_ref`     VARCHAR(64) NOT NULL COMMENT 'payment-core 透传的 customer id',
  `adapter`          VARCHAR(32) NOT NULL,
  `token`            VARCHAR(512) NOT NULL COMMENT '密文存储（AES-GCM）',
  `brand`            VARCHAR(32) DEFAULT NULL,
  `last4`            VARCHAR(8)  DEFAULT NULL,
  `status`           VARCHAR(16) NOT NULL DEFAULT 'active',
  `expires_at`       DATETIME    DEFAULT NULL,
  `created_at`       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_customer` (`customer_ref`, `adapter`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 28 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_28` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `aq_id`              VARCHAR(32)  NOT NULL COMMENT '本仓生成 aq_<db><tbl><seq>',
  `pi_id`              VARCHAR(32)  NOT NULL COMMENT '上游 PI ID，分片键',
  `adapter`            VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/grabpay/...',
  `action`             VARCHAR(32)  NOT NULL COMMENT 'charge/refund/capture/void/query',
  `idempotency_key`    VARCHAR(64)  NOT NULL COMMENT 'sha256(pi_id:action)',
  `state`              VARCHAR(16)  NOT NULL COMMENT 'pending/unknown/succeeded/failed',
  `external_ref_no`    VARCHAR(64)  DEFAULT NULL COMMENT '渠道返回的流水号',
  `amount`             BIGINT       DEFAULT NULL COMMENT '单位：分',
  `currency`           VARCHAR(8)   DEFAULT NULL,
  `failure_code`       VARCHAR(32)  DEFAULT NULL COMMENT '归一后的失败码',
  `raw_failure_code`   VARCHAR(64)  DEFAULT NULL COMMENT '渠道原始错误码',
  `request_method`     VARCHAR(8)   NOT NULL,
  `request_url`        VARCHAR(512) NOT NULL,
  `request_headers`    JSON         DEFAULT NULL,
  `request_body`       MEDIUMTEXT   DEFAULT NULL COMMENT '明文或密文 JSON',
  `response_status`    INT          DEFAULT NULL,
  `response_headers`   JSON         DEFAULT NULL,
  `response_body`      MEDIUMTEXT   DEFAULT NULL,
  `latency_ms`         INT          DEFAULT NULL,
  `attempt`            INT          NOT NULL DEFAULT 1,
  `next_retry_at`      DATETIME     DEFAULT NULL,
  `last_query_at`      DATETIME     DEFAULT NULL COMMENT 'PendingQueryWorker 上次 Query 推进时间',
  `query_count`        INT          NOT NULL DEFAULT 0 COMMENT 'Query 推进次数；超过上限走 stuck-cancel',
  `created_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_aq_id` (`aq_id`),
  UNIQUE KEY `uk_idem` (`adapter`, `idempotency_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_retry` (`state`, `next_retry_at`),
  KEY `idx_unknown` (`state`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2. 原始 webhook 记录：入表做幂等 + 审计，异步消费。
--    dedupe_key = sha256(adapter:event_id:event_type)（修复 P2-5）—— 同一 event_id
--    不同 event_type 不再误判 dedup。
CREATE TABLE IF NOT EXISTS `webhook_raw_28` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`          VARCHAR(32) NOT NULL COMMENT '从 body 解出的 PI，分片键',
  `adapter`        VARCHAR(32) NOT NULL,
  `event_id`       VARCHAR(64) DEFAULT NULL COMMENT '渠道事件 ID（若渠道不提供则自算 hash）',
  `event_type`     VARCHAR(64) DEFAULT NULL,
  `dedupe_key`     VARCHAR(96) NOT NULL COMMENT 'sha256(adapter:event_id:event_type)',
  `signature_ok`   TINYINT(1)  NOT NULL DEFAULT 0,
  `forwarded`      TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '是否已转发给 order-core',
  `forward_err`    VARCHAR(255) DEFAULT NULL,
  `headers`        JSON        DEFAULT NULL,
  `body`           MEDIUMTEXT  NOT NULL,
  `received_at`    DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `forwarded_at`   DATETIME    DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_dedupe` (`dedupe_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_pending` (`forwarded`, `received_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2b. 拒收的 webhook 单独审计表（修复 P1-3）：签名 fail / 金额 mismatch 等
--     事件不写主 webhook_raw 的 UNIQUE 槽位，只在这里做 audit。无 dedupe UNIQUE。
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_28` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`        VARCHAR(32)  DEFAULT NULL,
  `adapter`      VARCHAR(32)  NOT NULL,
  `event_id`     VARCHAR(64)  DEFAULT NULL,
  `event_type`   VARCHAR(64)  DEFAULT NULL,
  `reason`       VARCHAR(32)  NOT NULL COMMENT 'signature_fail/amount_mismatch/replay_window/...',
  `headers`      MEDIUMTEXT   DEFAULT NULL,
  `body`         MEDIUMTEXT   NOT NULL,
  `received_at`  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_adapter_reason` (`adapter`, `reason`, `received_at`),
  KEY `idx_event` (`adapter`, `event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='webhook 拒收审计，独立于 webhook_raw 防止占 UNIQUE';

-- 3. 渠道 token / mandate 映射（用户在本渠道的长期绑定）。
CREATE TABLE IF NOT EXISTS `channel_token_28` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`            VARCHAR(32) NOT NULL COMMENT '首次创建时的 pi，分片键',
  `customer_ref`     VARCHAR(64) NOT NULL COMMENT 'payment-core 透传的 customer id',
  `adapter`          VARCHAR(32) NOT NULL,
  `token`            VARCHAR(512) NOT NULL COMMENT '密文存储（AES-GCM）',
  `brand`            VARCHAR(32) DEFAULT NULL,
  `last4`            VARCHAR(8)  DEFAULT NULL,
  `status`           VARCHAR(16) NOT NULL DEFAULT 'active',
  `expires_at`       DATETIME    DEFAULT NULL,
  `created_at`       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_customer` (`customer_ref`, `adapter`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 29 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_29` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `aq_id`              VARCHAR(32)  NOT NULL COMMENT '本仓生成 aq_<db><tbl><seq>',
  `pi_id`              VARCHAR(32)  NOT NULL COMMENT '上游 PI ID，分片键',
  `adapter`            VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/grabpay/...',
  `action`             VARCHAR(32)  NOT NULL COMMENT 'charge/refund/capture/void/query',
  `idempotency_key`    VARCHAR(64)  NOT NULL COMMENT 'sha256(pi_id:action)',
  `state`              VARCHAR(16)  NOT NULL COMMENT 'pending/unknown/succeeded/failed',
  `external_ref_no`    VARCHAR(64)  DEFAULT NULL COMMENT '渠道返回的流水号',
  `amount`             BIGINT       DEFAULT NULL COMMENT '单位：分',
  `currency`           VARCHAR(8)   DEFAULT NULL,
  `failure_code`       VARCHAR(32)  DEFAULT NULL COMMENT '归一后的失败码',
  `raw_failure_code`   VARCHAR(64)  DEFAULT NULL COMMENT '渠道原始错误码',
  `request_method`     VARCHAR(8)   NOT NULL,
  `request_url`        VARCHAR(512) NOT NULL,
  `request_headers`    JSON         DEFAULT NULL,
  `request_body`       MEDIUMTEXT   DEFAULT NULL COMMENT '明文或密文 JSON',
  `response_status`    INT          DEFAULT NULL,
  `response_headers`   JSON         DEFAULT NULL,
  `response_body`      MEDIUMTEXT   DEFAULT NULL,
  `latency_ms`         INT          DEFAULT NULL,
  `attempt`            INT          NOT NULL DEFAULT 1,
  `next_retry_at`      DATETIME     DEFAULT NULL,
  `last_query_at`      DATETIME     DEFAULT NULL COMMENT 'PendingQueryWorker 上次 Query 推进时间',
  `query_count`        INT          NOT NULL DEFAULT 0 COMMENT 'Query 推进次数；超过上限走 stuck-cancel',
  `created_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_aq_id` (`aq_id`),
  UNIQUE KEY `uk_idem` (`adapter`, `idempotency_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_retry` (`state`, `next_retry_at`),
  KEY `idx_unknown` (`state`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2. 原始 webhook 记录：入表做幂等 + 审计，异步消费。
--    dedupe_key = sha256(adapter:event_id:event_type)（修复 P2-5）—— 同一 event_id
--    不同 event_type 不再误判 dedup。
CREATE TABLE IF NOT EXISTS `webhook_raw_29` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`          VARCHAR(32) NOT NULL COMMENT '从 body 解出的 PI，分片键',
  `adapter`        VARCHAR(32) NOT NULL,
  `event_id`       VARCHAR(64) DEFAULT NULL COMMENT '渠道事件 ID（若渠道不提供则自算 hash）',
  `event_type`     VARCHAR(64) DEFAULT NULL,
  `dedupe_key`     VARCHAR(96) NOT NULL COMMENT 'sha256(adapter:event_id:event_type)',
  `signature_ok`   TINYINT(1)  NOT NULL DEFAULT 0,
  `forwarded`      TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '是否已转发给 order-core',
  `forward_err`    VARCHAR(255) DEFAULT NULL,
  `headers`        JSON        DEFAULT NULL,
  `body`           MEDIUMTEXT  NOT NULL,
  `received_at`    DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `forwarded_at`   DATETIME    DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_dedupe` (`dedupe_key`),
  KEY `idx_pi` (`pi_id`),
  KEY `idx_pending` (`forwarded`, `received_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2b. 拒收的 webhook 单独审计表（修复 P1-3）：签名 fail / 金额 mismatch 等
--     事件不写主 webhook_raw 的 UNIQUE 槽位，只在这里做 audit。无 dedupe UNIQUE。
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_29` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`        VARCHAR(32)  DEFAULT NULL,
  `adapter`      VARCHAR(32)  NOT NULL,
  `event_id`     VARCHAR(64)  DEFAULT NULL,
  `event_type`   VARCHAR(64)  DEFAULT NULL,
  `reason`       VARCHAR(32)  NOT NULL COMMENT 'signature_fail/amount_mismatch/replay_window/...',
  `headers`      MEDIUMTEXT   DEFAULT NULL,
  `body`         MEDIUMTEXT   NOT NULL,
  `received_at`  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_adapter_reason` (`adapter`, `reason`, `received_at`),
  KEY `idx_event` (`adapter`, `event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='webhook 拒收审计，独立于 webhook_raw 防止占 UNIQUE';

-- 3. 渠道 token / mandate 映射（用户在本渠道的长期绑定）。
CREATE TABLE IF NOT EXISTS `channel_token_29` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `pi_id`            VARCHAR(32) NOT NULL COMMENT '首次创建时的 pi，分片键',
  `customer_ref`     VARCHAR(64) NOT NULL COMMENT 'payment-core 透传的 customer id',
  `adapter`          VARCHAR(32) NOT NULL,
  `token`            VARCHAR(512) NOT NULL COMMENT '密文存储（AES-GCM）',
  `brand`            VARCHAR(32) DEFAULT NULL,
  `last4`            VARCHAR(8)  DEFAULT NULL,
  `status`           VARCHAR(16) NOT NULL DEFAULT 'active',
  `expires_at`       DATETIME    DEFAULT NULL,
  `created_at`       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_customer` (`customer_ref`, `adapter`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ==== order-core ====
CREATE DATABASE IF NOT EXISTS `order_db_2` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `order_db_2`;

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：2 = 0-9 物理库；20 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_20
CREATE TABLE IF NOT EXISTS `payment_intent_20` (
    `id`                         VARCHAR(64)   NOT NULL,
    `amount`                     BIGINT        NOT NULL COMMENT '应付金额（= subtotal - coupon - points）',
    `amount_subtotal`            BIGINT        NOT NULL DEFAULT 0 COMMENT '原价',
    `amount_coupon`              BIGINT        NOT NULL DEFAULT 0 COMMENT '优惠券抵扣',
    `amount_points`              BIGINT        NOT NULL DEFAULT 0 COMMENT '积分抵扣',
    `currency`                   CHAR(3)       NOT NULL,
    `status`                     VARCHAR(32)   NOT NULL COMMENT '支付生命周期: created/requires_action/processing/succeeded/failed/canceled',
    `refund_phase`               VARCHAR(32)   DEFAULT NULL COMMENT '退款维度状态（独立字段）: refunding/partially_refunded/fully_refunded/refund_failed/refund_canceled',
    `customer_id`                VARCHAR(64)   DEFAULT NULL,
    `description`                VARCHAR(512)  DEFAULT NULL,
    `mch_id`                     VARCHAR(32)   NOT NULL,
    `mch_order_no`               VARCHAR(64)   DEFAULT NULL,
    `business_id`                VARCHAR(64)   DEFAULT NULL COMMENT '分片路由键；pi_id 前缀由此派生（fallback mch_id）',
    `idempotency_key`            VARCHAR(128)  DEFAULT NULL COMMENT '(mch_id,idempotency_key) 唯一；防重',
    `previous_payment_intent_id` VARCHAR(64)   DEFAULT NULL COMMENT '换单重下时指向上一次失败的 PI',
    `capture_method`             VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `confirmation_method`        VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `client_secret`              VARCHAR(128)  DEFAULT NULL,
    `payment_method_types`       JSON          DEFAULT NULL,
    `payment_method`             VARCHAR(32)   DEFAULT NULL,
    `user_id`                    BIGINT        DEFAULT NULL COMMENT '付款用户（可空，匿名 PI 不绑用户）',
    `user_card_id`               BIGINT        DEFAULT NULL COMMENT '卡支付时关联 user_merchant.user_card.id；其它支付方式为空',
    `active_charge_ids`          JSON          DEFAULT NULL COMMENT '进行中的 charge_id 列表（终态后移除）',
    `active_refund_ids`          JSON          DEFAULT NULL COMMENT '进行中的 refund_id 列表（终态后移除）',
    `amount_capturable`          BIGINT        NOT NULL DEFAULT 0,
    `amount_received`            BIGINT        NOT NULL DEFAULT 0,
    `return_url`                 VARCHAR(512)  DEFAULT NULL,
    `notify_url`                 VARCHAR(512)  DEFAULT NULL,
    `statement_descriptor`       VARCHAR(32)   DEFAULT NULL,
    `metadata`                   JSON          DEFAULT NULL,
    `livemode`                   TINYINT(1)    NOT NULL DEFAULT 0,
    `created`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `expired_at`                 DATETIME(3)   DEFAULT NULL,
    `canceled_at`                DATETIME(3)   DEFAULT NULL,
    `cancellation_reason`        VARCHAR(32)   DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_status`     (`status`),
    KEY `idx_mch`        (`mch_id`),
    KEY `idx_customer`   (`customer_id`),
    KEY `idx_business`   (`business_id`),
    UNIQUE KEY `uk_idem` (`mch_id`, `idempotency_key`),
    KEY `idx_prev`       (`previous_payment_intent_id`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Payment Intent 主表';

-- 2. Charge 表 charge_20（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_20` (
    `id`                  VARCHAR(64)   NOT NULL,
    `payment_intent_id`   VARCHAR(64)   NOT NULL,
    `amount`              BIGINT        NOT NULL,
    `amount_captured`     BIGINT        NOT NULL DEFAULT 0,
    `amount_refunded`     BIGINT        NOT NULL DEFAULT 0,
    `currency`            CHAR(3)       NOT NULL,
    `status`              VARCHAR(16)   NOT NULL COMMENT 'pending/succeeded/failed',
    `payment_method`      VARCHAR(32)   NOT NULL,
    `captured`            TINYINT(1)    NOT NULL DEFAULT 0,
    `paid`                TINYINT(1)    NOT NULL DEFAULT 0,
    `refunded`            TINYINT(1)    NOT NULL DEFAULT 0,
    `outcome_risk_level`  VARCHAR(16)   DEFAULT NULL,
    `outcome_risk_score`  INT           DEFAULT NULL,
    `outcome_seller_msg`  VARCHAR(256)  DEFAULT NULL,
    `outcome_type`        VARCHAR(32)   DEFAULT NULL,
    `outcome_reason`      VARCHAR(64)   DEFAULT NULL,
    `outcome_network`     VARCHAR(32)   DEFAULT NULL,
    `failure_code`        VARCHAR(64)   DEFAULT NULL,
    `failure_message`     VARCHAR(512)  DEFAULT NULL,
    `receipt_url`         VARCHAR(512)  DEFAULT NULL,
    `balance_transaction` VARCHAR(64)   DEFAULT NULL,
    `livemode`            TINYINT(1)    NOT NULL DEFAULT 0,
    `metadata`            JSON          DEFAULT NULL,
    `expired_at`          DATETIME(3)   DEFAULT NULL COMMENT '支付单过期时间：超时未完成 → 失败',
    `completed_at`        DATETIME(3)   DEFAULT NULL,
    `created`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_status`     (`status`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Charge 表（一次扣款尝试）';

-- 3. PayAction 表 pay_action_20（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_20` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `action_type`       VARCHAR(32)  NOT NULL COMMENT 'three_d_secure / otp / pay_password',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/expired',
    `payload`           JSON         DEFAULT NULL COMMENT '返回给前端的挑战材料（redirect_url / recipient_masked 等）',
    `expected_secret`   VARCHAR(256) DEFAULT NULL COMMENT 'OTP 明文或密码哈希，不返回给前端',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_attempts`      INT          NOT NULL DEFAULT 0,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `expires_at`        DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_charge`     (`charge_id`),
    KEY `idx_type`       (`action_type`),
    KEY `idx_status`     (`status`),
    KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='PayAction 表（用户挑战：3DS / OTP / PayPassword）';

-- 4. ExceptionCase 表 exception_case_20（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_20` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `case_type`         VARCHAR(32)  NOT NULL COMMENT 'late_refund_success/late_refund_failed/late_charge_success/amount_mismatch/duplicate_webhook',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'open/processing/resolved/ignored',
    `summary`           VARCHAR(512) DEFAULT NULL,
    `payload`           JSON         DEFAULT NULL,
    `resolution`        VARCHAR(512) DEFAULT NULL,
    `assigned_to`       VARCHAR(64)  DEFAULT NULL,
    `resolved_at`       DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`     (`payment_intent_id`),
    KEY `idx_refund` (`refund_id`),
    KEY `idx_type`   (`case_type`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='差错处理单';

-- 5. NotifyLog 表 notify_log_20（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_20` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `event_id`          VARCHAR(64)  NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL COMMENT 'payment_intent.succeeded / charge.refunded / ...',
    `client_type`       VARCHAR(16)  DEFAULT NULL COMMENT 'server/web/ios/android/miniapp/internal',
    `notify_channel`    VARCHAR(16)  NOT NULL COMMENT 'http/apns/fcm/websocket/mq/sms/email',
    `target`            VARCHAR(512) NOT NULL,
    `payload`           BLOB         DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/retrying/succeeded/failed/canceled',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_retries`       INT          NOT NULL DEFAULT 5,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `http_status`       INT          DEFAULT NULL,
    `error_code`        VARCHAR(64)  DEFAULT NULL,
    `error_msg`         TEXT         DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`            (`payment_intent_id`),
    KEY `idx_event`         (`event_id`),
    KEY `idx_event_type`    (`event_type`),
    KEY `idx_status`        (`status`),
    -- wave L: cover the ListDue() predicate (status IN (...) AND next_retry_at)
    -- so the cron scan hits one composite instead of two single-column indexes.
    KEY `idx_status_retry`  (`status`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通知日志';

-- 5. Refund 表 refund_20（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_20` (
    `id`                VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    -- P0 #3 幂等键：(payment_intent_id, idempotency_key) 唯一。
    -- 客户端 retry Create 同一退款请求 → DB 兜底防止双行；service 层先查 GetByIdempotencyKey 返已有。
    `idempotency_key`   VARCHAR(128) DEFAULT NULL,
    `amount`            BIGINT       NOT NULL,
    `currency`          CHAR(3)      NOT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/canceled',
    `reason`            VARCHAR(32)  DEFAULT NULL,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `receipt_number`    VARCHAR(64)  DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `auto_compensate`   TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '系统自动补偿退款：retry worker 无限重试直到成功；商户发起=0',
    `retry_count`       INT          NOT NULL DEFAULT 0,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem`       (`payment_intent_id`, `idempotency_key`),
    KEY `idx_charge`          (`charge_id`),
    KEY `idx_pi`              (`payment_intent_id`),
    KEY `idx_status`          (`status`),
    KEY `idx_auto_compensate` (`auto_compensate`),
    KEY `idx_next_retry`      (`next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Refund 表';

-- 7. InboundWebhook 表 inbound_webhook_20
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_20` (
    `id`                VARCHAR(64)  NOT NULL,
    `channel_name`      VARCHAR(32)  NOT NULL,
    `event_id`          VARCHAR(128) NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  DEFAULT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `dispute_id`        VARCHAR(64)  DEFAULT NULL,
    `signature_ok`      TINYINT(1)   NOT NULL DEFAULT 0,
    `headers`           JSON         DEFAULT NULL,
    `body`              MEDIUMBLOB   DEFAULT NULL,
    `processed_at`      DATETIME(3)  DEFAULT NULL,
    `process_status`    VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/succeeded/failed/duplicate',
    `error_msg`         TEXT         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_event`  (`channel_name`, `event_id`),
    KEY         `idx_pi`            (`payment_intent_id`),
    KEY         `idx_event_type`    (`event_type`),
    KEY         `idx_process_status` (`process_status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='入站 webhook 去重日志';

-- 8. Dispute 表 dispute_20
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_20` (
    `id`                   VARCHAR(64)  NOT NULL COMMENT 'dp_xxx',
    `payment_intent_id`    VARCHAR(64)  NOT NULL,
    `charge_id`            VARCHAR(64)  NOT NULL,
    `merchant_id`          VARCHAR(32)  NOT NULL,
    `channel`              VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/paymongo/...',
    `channel_dispute_id`   VARCHAR(128) DEFAULT NULL COMMENT '渠道侧 dispute id (用于回写)',
    `status`               VARCHAR(24)  NOT NULL COMMENT 'needs_response/under_review/won/lost/warning_closed/charge_refunded/canceled',
    `amount`               BIGINT       NOT NULL COMMENT 'disputed amount in minor units',
    `currency`             CHAR(3)      NOT NULL DEFAULT 'PHP',
    `reason`               VARCHAR(64)  NOT NULL COMMENT 'fraud/product_not_received/unrecognized/duplicate/credit_not_processed/other',
    `reason_detail`        VARCHAR(512) DEFAULT NULL,
    `evidence_due_at`      DATETIME(3)  DEFAULT NULL COMMENT '商户响应截止时间',
    `evidence`             JSON         DEFAULT NULL COMMENT '证据包：{text, photos:[url], shipping:{}, emails:[url], receipt:url, ...}',
    `decided_at`           DATETIME(3)  DEFAULT NULL,
    `outcome_amount`       BIGINT       DEFAULT NULL COMMENT '实际被扣回金额（lost 情形），<= amount',
    `auto_refund_charge`   TINYINT(1)   NOT NULL DEFAULT 1 COMMENT 'lost/charge_refunded 时是否联动退款',
    `metadata`             JSON         DEFAULT NULL,
    `created`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_dispute` (`channel`, `channel_dispute_id`),
    KEY `idx_pi`        (`payment_intent_id`),
    KEY `idx_charge`    (`charge_id`),
    KEY `idx_merchant`  (`merchant_id`),
    KEY `idx_status`    (`status`),
    KEY `idx_due`       (`status`, `evidence_due_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute / Chargeback';

-- 9. DisputeEvent 表 dispute_event_20（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_20` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `dispute_id`      VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64) NOT NULL,
    `from_status`     VARCHAR(24)  NOT NULL,
    `to_status`       VARCHAR(24)  NOT NULL,
    `source`          VARCHAR(32)  NOT NULL COMMENT 'channel_webhook / admin / system',
    `actor`           VARCHAR(64)  DEFAULT NULL,
    `note`            VARCHAR(512) DEFAULT NULL,
    `payload`         JSON         DEFAULT NULL COMMENT '触发事件 (webhook body / admin input)',
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_dispute`  (`dispute_id`),
    KEY `idx_pi`       (`payment_intent_id`),
    KEY `idx_created`  (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute 状态流转日志';

-- 10. AccountingOutbox 表 accounting_outbox_20
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_20` (
    `id`                 VARCHAR(64)  NOT NULL,
    `request_id`         VARCHAR(160) NOT NULL,
    `event_type`         VARCHAR(32)  NOT NULL COMMENT 'charge_succeeded/refund_succeeded',
    `payment_intent_id`  VARCHAR(64)  NOT NULL,
    `charge_id`          VARCHAR(64)  DEFAULT NULL,
    `refund_id`          VARCHAR(64)  DEFAULT NULL,
    `owner_type`         VARCHAR(16)  NOT NULL COMMENT 'user/merchant',
    `owner_id`           VARCHAR(64)  NOT NULL,
    `payment_method`     VARCHAR(32)  NOT NULL COMMENT 'gcash/shopeepay/maya/grabpay/...',
    `amount`             BIGINT       NOT NULL,
    `currency`           CHAR(3)      NOT NULL,
    `metadata`           JSON         DEFAULT NULL,
    `status`             VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/sent/failed',
    `attempts`           INT          NOT NULL DEFAULT 0,
    `next_attempt_at`    DATETIME(3)  DEFAULT NULL,
    `last_error`         TEXT         DEFAULT NULL,
    `sent_at`            DATETIME(3)  DEFAULT NULL,
    -- claim_token 用于多 worker / 多副本并发投递时的批次 claim：
    -- worker 用一条 UPDATE ... LIMIT N 把候选行的 next_attempt_at 推到远未来 +
    -- 写入自己生成的 token，其他 worker 的 SELECT 因 next_attempt_at 在未来天然
    -- 跳过；本 worker 再用 SELECT WHERE claim_token = ? 拉回该批次处理。
    -- 模式同 webhook_deliveries.claim_token（见 internal/webhook/delivery.go）。
    `claim_token`        VARCHAR(64)  DEFAULT NULL,
    `created`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_request`       (`request_id`),
    KEY         `idx_pi`           (`payment_intent_id`),
    KEY         `idx_status_next`  (`status`, `next_attempt_at`),
    -- claim_token 反查刚 claim 的批次；带前缀 + 纳秒时间戳 + 随机数，区分度足够
    KEY         `idx_claim`        (`claim_token`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='记账事件 outbox（投递给 accounting-system）';

-- ─── admin_audit_log_20：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_20` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id (from bearer token)',
    `actor_ip`       VARCHAR(64)  DEFAULT NULL,
    `action`         VARCHAR(64)  NOT NULL COMMENT 'merchant.approve / refund.create / risk.rule.update / ...',
    `target_type`    VARCHAR(32)  DEFAULT NULL,
    `target_id`      VARCHAR(64)  DEFAULT NULL,
    `http_method`    VARCHAR(8)   DEFAULT NULL,
    `http_path`      VARCHAR(256) DEFAULT NULL,
    `http_status`    INT          DEFAULT NULL,
    `request_body`   MEDIUMTEXT   DEFAULT NULL COMMENT 'JSON, sensitive fields redacted by caller',
    `response_code`  VARCHAR(32)  DEFAULT NULL,
    `response_msg`   VARCHAR(512) DEFAULT NULL,
    `duration_ms`    INT          DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_action`  (`action`),
    KEY `idx_target`  (`target_type`, `target_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 20';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：2 = 0-9 物理库；21 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_21
CREATE TABLE IF NOT EXISTS `payment_intent_21` (
    `id`                         VARCHAR(64)   NOT NULL,
    `amount`                     BIGINT        NOT NULL COMMENT '应付金额（= subtotal - coupon - points）',
    `amount_subtotal`            BIGINT        NOT NULL DEFAULT 0 COMMENT '原价',
    `amount_coupon`              BIGINT        NOT NULL DEFAULT 0 COMMENT '优惠券抵扣',
    `amount_points`              BIGINT        NOT NULL DEFAULT 0 COMMENT '积分抵扣',
    `currency`                   CHAR(3)       NOT NULL,
    `status`                     VARCHAR(32)   NOT NULL COMMENT '支付生命周期: created/requires_action/processing/succeeded/failed/canceled',
    `refund_phase`               VARCHAR(32)   DEFAULT NULL COMMENT '退款维度状态（独立字段）: refunding/partially_refunded/fully_refunded/refund_failed/refund_canceled',
    `customer_id`                VARCHAR(64)   DEFAULT NULL,
    `description`                VARCHAR(512)  DEFAULT NULL,
    `mch_id`                     VARCHAR(32)   NOT NULL,
    `mch_order_no`               VARCHAR(64)   DEFAULT NULL,
    `business_id`                VARCHAR(64)   DEFAULT NULL COMMENT '分片路由键；pi_id 前缀由此派生（fallback mch_id）',
    `idempotency_key`            VARCHAR(128)  DEFAULT NULL COMMENT '(mch_id,idempotency_key) 唯一；防重',
    `previous_payment_intent_id` VARCHAR(64)   DEFAULT NULL COMMENT '换单重下时指向上一次失败的 PI',
    `capture_method`             VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `confirmation_method`        VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `client_secret`              VARCHAR(128)  DEFAULT NULL,
    `payment_method_types`       JSON          DEFAULT NULL,
    `payment_method`             VARCHAR(32)   DEFAULT NULL,
    `user_id`                    BIGINT        DEFAULT NULL COMMENT '付款用户（可空，匿名 PI 不绑用户）',
    `user_card_id`               BIGINT        DEFAULT NULL COMMENT '卡支付时关联 user_merchant.user_card.id；其它支付方式为空',
    `active_charge_ids`          JSON          DEFAULT NULL COMMENT '进行中的 charge_id 列表（终态后移除）',
    `active_refund_ids`          JSON          DEFAULT NULL COMMENT '进行中的 refund_id 列表（终态后移除）',
    `amount_capturable`          BIGINT        NOT NULL DEFAULT 0,
    `amount_received`            BIGINT        NOT NULL DEFAULT 0,
    `return_url`                 VARCHAR(512)  DEFAULT NULL,
    `notify_url`                 VARCHAR(512)  DEFAULT NULL,
    `statement_descriptor`       VARCHAR(32)   DEFAULT NULL,
    `metadata`                   JSON          DEFAULT NULL,
    `livemode`                   TINYINT(1)    NOT NULL DEFAULT 0,
    `created`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `expired_at`                 DATETIME(3)   DEFAULT NULL,
    `canceled_at`                DATETIME(3)   DEFAULT NULL,
    `cancellation_reason`        VARCHAR(32)   DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_status`     (`status`),
    KEY `idx_mch`        (`mch_id`),
    KEY `idx_customer`   (`customer_id`),
    KEY `idx_business`   (`business_id`),
    UNIQUE KEY `uk_idem` (`mch_id`, `idempotency_key`),
    KEY `idx_prev`       (`previous_payment_intent_id`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Payment Intent 主表';

-- 2. Charge 表 charge_21（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_21` (
    `id`                  VARCHAR(64)   NOT NULL,
    `payment_intent_id`   VARCHAR(64)   NOT NULL,
    `amount`              BIGINT        NOT NULL,
    `amount_captured`     BIGINT        NOT NULL DEFAULT 0,
    `amount_refunded`     BIGINT        NOT NULL DEFAULT 0,
    `currency`            CHAR(3)       NOT NULL,
    `status`              VARCHAR(16)   NOT NULL COMMENT 'pending/succeeded/failed',
    `payment_method`      VARCHAR(32)   NOT NULL,
    `captured`            TINYINT(1)    NOT NULL DEFAULT 0,
    `paid`                TINYINT(1)    NOT NULL DEFAULT 0,
    `refunded`            TINYINT(1)    NOT NULL DEFAULT 0,
    `outcome_risk_level`  VARCHAR(16)   DEFAULT NULL,
    `outcome_risk_score`  INT           DEFAULT NULL,
    `outcome_seller_msg`  VARCHAR(256)  DEFAULT NULL,
    `outcome_type`        VARCHAR(32)   DEFAULT NULL,
    `outcome_reason`      VARCHAR(64)   DEFAULT NULL,
    `outcome_network`     VARCHAR(32)   DEFAULT NULL,
    `failure_code`        VARCHAR(64)   DEFAULT NULL,
    `failure_message`     VARCHAR(512)  DEFAULT NULL,
    `receipt_url`         VARCHAR(512)  DEFAULT NULL,
    `balance_transaction` VARCHAR(64)   DEFAULT NULL,
    `livemode`            TINYINT(1)    NOT NULL DEFAULT 0,
    `metadata`            JSON          DEFAULT NULL,
    `expired_at`          DATETIME(3)   DEFAULT NULL COMMENT '支付单过期时间：超时未完成 → 失败',
    `completed_at`        DATETIME(3)   DEFAULT NULL,
    `created`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_status`     (`status`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Charge 表（一次扣款尝试）';

-- 3. PayAction 表 pay_action_21（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_21` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `action_type`       VARCHAR(32)  NOT NULL COMMENT 'three_d_secure / otp / pay_password',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/expired',
    `payload`           JSON         DEFAULT NULL COMMENT '返回给前端的挑战材料（redirect_url / recipient_masked 等）',
    `expected_secret`   VARCHAR(256) DEFAULT NULL COMMENT 'OTP 明文或密码哈希，不返回给前端',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_attempts`      INT          NOT NULL DEFAULT 0,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `expires_at`        DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_charge`     (`charge_id`),
    KEY `idx_type`       (`action_type`),
    KEY `idx_status`     (`status`),
    KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='PayAction 表（用户挑战：3DS / OTP / PayPassword）';

-- 4. ExceptionCase 表 exception_case_21（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_21` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `case_type`         VARCHAR(32)  NOT NULL COMMENT 'late_refund_success/late_refund_failed/late_charge_success/amount_mismatch/duplicate_webhook',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'open/processing/resolved/ignored',
    `summary`           VARCHAR(512) DEFAULT NULL,
    `payload`           JSON         DEFAULT NULL,
    `resolution`        VARCHAR(512) DEFAULT NULL,
    `assigned_to`       VARCHAR(64)  DEFAULT NULL,
    `resolved_at`       DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`     (`payment_intent_id`),
    KEY `idx_refund` (`refund_id`),
    KEY `idx_type`   (`case_type`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='差错处理单';

-- 5. NotifyLog 表 notify_log_21（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_21` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `event_id`          VARCHAR(64)  NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL COMMENT 'payment_intent.succeeded / charge.refunded / ...',
    `client_type`       VARCHAR(16)  DEFAULT NULL COMMENT 'server/web/ios/android/miniapp/internal',
    `notify_channel`    VARCHAR(16)  NOT NULL COMMENT 'http/apns/fcm/websocket/mq/sms/email',
    `target`            VARCHAR(512) NOT NULL,
    `payload`           BLOB         DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/retrying/succeeded/failed/canceled',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_retries`       INT          NOT NULL DEFAULT 5,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `http_status`       INT          DEFAULT NULL,
    `error_code`        VARCHAR(64)  DEFAULT NULL,
    `error_msg`         TEXT         DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`            (`payment_intent_id`),
    KEY `idx_event`         (`event_id`),
    KEY `idx_event_type`    (`event_type`),
    KEY `idx_status`        (`status`),
    -- wave L: cover the ListDue() predicate (status IN (...) AND next_retry_at)
    -- so the cron scan hits one composite instead of two single-column indexes.
    KEY `idx_status_retry`  (`status`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通知日志';

-- 5. Refund 表 refund_21（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_21` (
    `id`                VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    -- P0 #3 幂等键：(payment_intent_id, idempotency_key) 唯一。
    -- 客户端 retry Create 同一退款请求 → DB 兜底防止双行；service 层先查 GetByIdempotencyKey 返已有。
    `idempotency_key`   VARCHAR(128) DEFAULT NULL,
    `amount`            BIGINT       NOT NULL,
    `currency`          CHAR(3)      NOT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/canceled',
    `reason`            VARCHAR(32)  DEFAULT NULL,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `receipt_number`    VARCHAR(64)  DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `auto_compensate`   TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '系统自动补偿退款：retry worker 无限重试直到成功；商户发起=0',
    `retry_count`       INT          NOT NULL DEFAULT 0,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem`       (`payment_intent_id`, `idempotency_key`),
    KEY `idx_charge`          (`charge_id`),
    KEY `idx_pi`              (`payment_intent_id`),
    KEY `idx_status`          (`status`),
    KEY `idx_auto_compensate` (`auto_compensate`),
    KEY `idx_next_retry`      (`next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Refund 表';

-- 7. InboundWebhook 表 inbound_webhook_21
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_21` (
    `id`                VARCHAR(64)  NOT NULL,
    `channel_name`      VARCHAR(32)  NOT NULL,
    `event_id`          VARCHAR(128) NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  DEFAULT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `dispute_id`        VARCHAR(64)  DEFAULT NULL,
    `signature_ok`      TINYINT(1)   NOT NULL DEFAULT 0,
    `headers`           JSON         DEFAULT NULL,
    `body`              MEDIUMBLOB   DEFAULT NULL,
    `processed_at`      DATETIME(3)  DEFAULT NULL,
    `process_status`    VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/succeeded/failed/duplicate',
    `error_msg`         TEXT         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_event`  (`channel_name`, `event_id`),
    KEY         `idx_pi`            (`payment_intent_id`),
    KEY         `idx_event_type`    (`event_type`),
    KEY         `idx_process_status` (`process_status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='入站 webhook 去重日志';

-- 8. Dispute 表 dispute_21
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_21` (
    `id`                   VARCHAR(64)  NOT NULL COMMENT 'dp_xxx',
    `payment_intent_id`    VARCHAR(64)  NOT NULL,
    `charge_id`            VARCHAR(64)  NOT NULL,
    `merchant_id`          VARCHAR(32)  NOT NULL,
    `channel`              VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/paymongo/...',
    `channel_dispute_id`   VARCHAR(128) DEFAULT NULL COMMENT '渠道侧 dispute id (用于回写)',
    `status`               VARCHAR(24)  NOT NULL COMMENT 'needs_response/under_review/won/lost/warning_closed/charge_refunded/canceled',
    `amount`               BIGINT       NOT NULL COMMENT 'disputed amount in minor units',
    `currency`             CHAR(3)      NOT NULL DEFAULT 'PHP',
    `reason`               VARCHAR(64)  NOT NULL COMMENT 'fraud/product_not_received/unrecognized/duplicate/credit_not_processed/other',
    `reason_detail`        VARCHAR(512) DEFAULT NULL,
    `evidence_due_at`      DATETIME(3)  DEFAULT NULL COMMENT '商户响应截止时间',
    `evidence`             JSON         DEFAULT NULL COMMENT '证据包：{text, photos:[url], shipping:{}, emails:[url], receipt:url, ...}',
    `decided_at`           DATETIME(3)  DEFAULT NULL,
    `outcome_amount`       BIGINT       DEFAULT NULL COMMENT '实际被扣回金额（lost 情形），<= amount',
    `auto_refund_charge`   TINYINT(1)   NOT NULL DEFAULT 1 COMMENT 'lost/charge_refunded 时是否联动退款',
    `metadata`             JSON         DEFAULT NULL,
    `created`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_dispute` (`channel`, `channel_dispute_id`),
    KEY `idx_pi`        (`payment_intent_id`),
    KEY `idx_charge`    (`charge_id`),
    KEY `idx_merchant`  (`merchant_id`),
    KEY `idx_status`    (`status`),
    KEY `idx_due`       (`status`, `evidence_due_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute / Chargeback';

-- 9. DisputeEvent 表 dispute_event_21（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_21` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `dispute_id`      VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64) NOT NULL,
    `from_status`     VARCHAR(24)  NOT NULL,
    `to_status`       VARCHAR(24)  NOT NULL,
    `source`          VARCHAR(32)  NOT NULL COMMENT 'channel_webhook / admin / system',
    `actor`           VARCHAR(64)  DEFAULT NULL,
    `note`            VARCHAR(512) DEFAULT NULL,
    `payload`         JSON         DEFAULT NULL COMMENT '触发事件 (webhook body / admin input)',
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_dispute`  (`dispute_id`),
    KEY `idx_pi`       (`payment_intent_id`),
    KEY `idx_created`  (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute 状态流转日志';

-- 10. AccountingOutbox 表 accounting_outbox_21
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_21` (
    `id`                 VARCHAR(64)  NOT NULL,
    `request_id`         VARCHAR(160) NOT NULL,
    `event_type`         VARCHAR(32)  NOT NULL COMMENT 'charge_succeeded/refund_succeeded',
    `payment_intent_id`  VARCHAR(64)  NOT NULL,
    `charge_id`          VARCHAR(64)  DEFAULT NULL,
    `refund_id`          VARCHAR(64)  DEFAULT NULL,
    `owner_type`         VARCHAR(16)  NOT NULL COMMENT 'user/merchant',
    `owner_id`           VARCHAR(64)  NOT NULL,
    `payment_method`     VARCHAR(32)  NOT NULL COMMENT 'gcash/shopeepay/maya/grabpay/...',
    `amount`             BIGINT       NOT NULL,
    `currency`           CHAR(3)      NOT NULL,
    `metadata`           JSON         DEFAULT NULL,
    `status`             VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/sent/failed',
    `attempts`           INT          NOT NULL DEFAULT 0,
    `next_attempt_at`    DATETIME(3)  DEFAULT NULL,
    `last_error`         TEXT         DEFAULT NULL,
    `sent_at`            DATETIME(3)  DEFAULT NULL,
    -- claim_token 用于多 worker / 多副本并发投递时的批次 claim：
    -- worker 用一条 UPDATE ... LIMIT N 把候选行的 next_attempt_at 推到远未来 +
    -- 写入自己生成的 token，其他 worker 的 SELECT 因 next_attempt_at 在未来天然
    -- 跳过；本 worker 再用 SELECT WHERE claim_token = ? 拉回该批次处理。
    -- 模式同 webhook_deliveries.claim_token（见 internal/webhook/delivery.go）。
    `claim_token`        VARCHAR(64)  DEFAULT NULL,
    `created`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_request`       (`request_id`),
    KEY         `idx_pi`           (`payment_intent_id`),
    KEY         `idx_status_next`  (`status`, `next_attempt_at`),
    -- claim_token 反查刚 claim 的批次；带前缀 + 纳秒时间戳 + 随机数，区分度足够
    KEY         `idx_claim`        (`claim_token`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='记账事件 outbox（投递给 accounting-system）';

-- ─── admin_audit_log_21：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_21` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id (from bearer token)',
    `actor_ip`       VARCHAR(64)  DEFAULT NULL,
    `action`         VARCHAR(64)  NOT NULL COMMENT 'merchant.approve / refund.create / risk.rule.update / ...',
    `target_type`    VARCHAR(32)  DEFAULT NULL,
    `target_id`      VARCHAR(64)  DEFAULT NULL,
    `http_method`    VARCHAR(8)   DEFAULT NULL,
    `http_path`      VARCHAR(256) DEFAULT NULL,
    `http_status`    INT          DEFAULT NULL,
    `request_body`   MEDIUMTEXT   DEFAULT NULL COMMENT 'JSON, sensitive fields redacted by caller',
    `response_code`  VARCHAR(32)  DEFAULT NULL,
    `response_msg`   VARCHAR(512) DEFAULT NULL,
    `duration_ms`    INT          DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_action`  (`action`),
    KEY `idx_target`  (`target_type`, `target_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 21';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：2 = 0-9 物理库；22 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_22
CREATE TABLE IF NOT EXISTS `payment_intent_22` (
    `id`                         VARCHAR(64)   NOT NULL,
    `amount`                     BIGINT        NOT NULL COMMENT '应付金额（= subtotal - coupon - points）',
    `amount_subtotal`            BIGINT        NOT NULL DEFAULT 0 COMMENT '原价',
    `amount_coupon`              BIGINT        NOT NULL DEFAULT 0 COMMENT '优惠券抵扣',
    `amount_points`              BIGINT        NOT NULL DEFAULT 0 COMMENT '积分抵扣',
    `currency`                   CHAR(3)       NOT NULL,
    `status`                     VARCHAR(32)   NOT NULL COMMENT '支付生命周期: created/requires_action/processing/succeeded/failed/canceled',
    `refund_phase`               VARCHAR(32)   DEFAULT NULL COMMENT '退款维度状态（独立字段）: refunding/partially_refunded/fully_refunded/refund_failed/refund_canceled',
    `customer_id`                VARCHAR(64)   DEFAULT NULL,
    `description`                VARCHAR(512)  DEFAULT NULL,
    `mch_id`                     VARCHAR(32)   NOT NULL,
    `mch_order_no`               VARCHAR(64)   DEFAULT NULL,
    `business_id`                VARCHAR(64)   DEFAULT NULL COMMENT '分片路由键；pi_id 前缀由此派生（fallback mch_id）',
    `idempotency_key`            VARCHAR(128)  DEFAULT NULL COMMENT '(mch_id,idempotency_key) 唯一；防重',
    `previous_payment_intent_id` VARCHAR(64)   DEFAULT NULL COMMENT '换单重下时指向上一次失败的 PI',
    `capture_method`             VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `confirmation_method`        VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `client_secret`              VARCHAR(128)  DEFAULT NULL,
    `payment_method_types`       JSON          DEFAULT NULL,
    `payment_method`             VARCHAR(32)   DEFAULT NULL,
    `user_id`                    BIGINT        DEFAULT NULL COMMENT '付款用户（可空，匿名 PI 不绑用户）',
    `user_card_id`               BIGINT        DEFAULT NULL COMMENT '卡支付时关联 user_merchant.user_card.id；其它支付方式为空',
    `active_charge_ids`          JSON          DEFAULT NULL COMMENT '进行中的 charge_id 列表（终态后移除）',
    `active_refund_ids`          JSON          DEFAULT NULL COMMENT '进行中的 refund_id 列表（终态后移除）',
    `amount_capturable`          BIGINT        NOT NULL DEFAULT 0,
    `amount_received`            BIGINT        NOT NULL DEFAULT 0,
    `return_url`                 VARCHAR(512)  DEFAULT NULL,
    `notify_url`                 VARCHAR(512)  DEFAULT NULL,
    `statement_descriptor`       VARCHAR(32)   DEFAULT NULL,
    `metadata`                   JSON          DEFAULT NULL,
    `livemode`                   TINYINT(1)    NOT NULL DEFAULT 0,
    `created`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `expired_at`                 DATETIME(3)   DEFAULT NULL,
    `canceled_at`                DATETIME(3)   DEFAULT NULL,
    `cancellation_reason`        VARCHAR(32)   DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_status`     (`status`),
    KEY `idx_mch`        (`mch_id`),
    KEY `idx_customer`   (`customer_id`),
    KEY `idx_business`   (`business_id`),
    UNIQUE KEY `uk_idem` (`mch_id`, `idempotency_key`),
    KEY `idx_prev`       (`previous_payment_intent_id`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Payment Intent 主表';

-- 2. Charge 表 charge_22（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_22` (
    `id`                  VARCHAR(64)   NOT NULL,
    `payment_intent_id`   VARCHAR(64)   NOT NULL,
    `amount`              BIGINT        NOT NULL,
    `amount_captured`     BIGINT        NOT NULL DEFAULT 0,
    `amount_refunded`     BIGINT        NOT NULL DEFAULT 0,
    `currency`            CHAR(3)       NOT NULL,
    `status`              VARCHAR(16)   NOT NULL COMMENT 'pending/succeeded/failed',
    `payment_method`      VARCHAR(32)   NOT NULL,
    `captured`            TINYINT(1)    NOT NULL DEFAULT 0,
    `paid`                TINYINT(1)    NOT NULL DEFAULT 0,
    `refunded`            TINYINT(1)    NOT NULL DEFAULT 0,
    `outcome_risk_level`  VARCHAR(16)   DEFAULT NULL,
    `outcome_risk_score`  INT           DEFAULT NULL,
    `outcome_seller_msg`  VARCHAR(256)  DEFAULT NULL,
    `outcome_type`        VARCHAR(32)   DEFAULT NULL,
    `outcome_reason`      VARCHAR(64)   DEFAULT NULL,
    `outcome_network`     VARCHAR(32)   DEFAULT NULL,
    `failure_code`        VARCHAR(64)   DEFAULT NULL,
    `failure_message`     VARCHAR(512)  DEFAULT NULL,
    `receipt_url`         VARCHAR(512)  DEFAULT NULL,
    `balance_transaction` VARCHAR(64)   DEFAULT NULL,
    `livemode`            TINYINT(1)    NOT NULL DEFAULT 0,
    `metadata`            JSON          DEFAULT NULL,
    `expired_at`          DATETIME(3)   DEFAULT NULL COMMENT '支付单过期时间：超时未完成 → 失败',
    `completed_at`        DATETIME(3)   DEFAULT NULL,
    `created`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_status`     (`status`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Charge 表（一次扣款尝试）';

-- 3. PayAction 表 pay_action_22（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_22` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `action_type`       VARCHAR(32)  NOT NULL COMMENT 'three_d_secure / otp / pay_password',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/expired',
    `payload`           JSON         DEFAULT NULL COMMENT '返回给前端的挑战材料（redirect_url / recipient_masked 等）',
    `expected_secret`   VARCHAR(256) DEFAULT NULL COMMENT 'OTP 明文或密码哈希，不返回给前端',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_attempts`      INT          NOT NULL DEFAULT 0,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `expires_at`        DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_charge`     (`charge_id`),
    KEY `idx_type`       (`action_type`),
    KEY `idx_status`     (`status`),
    KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='PayAction 表（用户挑战：3DS / OTP / PayPassword）';

-- 4. ExceptionCase 表 exception_case_22（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_22` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `case_type`         VARCHAR(32)  NOT NULL COMMENT 'late_refund_success/late_refund_failed/late_charge_success/amount_mismatch/duplicate_webhook',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'open/processing/resolved/ignored',
    `summary`           VARCHAR(512) DEFAULT NULL,
    `payload`           JSON         DEFAULT NULL,
    `resolution`        VARCHAR(512) DEFAULT NULL,
    `assigned_to`       VARCHAR(64)  DEFAULT NULL,
    `resolved_at`       DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`     (`payment_intent_id`),
    KEY `idx_refund` (`refund_id`),
    KEY `idx_type`   (`case_type`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='差错处理单';

-- 5. NotifyLog 表 notify_log_22（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_22` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `event_id`          VARCHAR(64)  NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL COMMENT 'payment_intent.succeeded / charge.refunded / ...',
    `client_type`       VARCHAR(16)  DEFAULT NULL COMMENT 'server/web/ios/android/miniapp/internal',
    `notify_channel`    VARCHAR(16)  NOT NULL COMMENT 'http/apns/fcm/websocket/mq/sms/email',
    `target`            VARCHAR(512) NOT NULL,
    `payload`           BLOB         DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/retrying/succeeded/failed/canceled',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_retries`       INT          NOT NULL DEFAULT 5,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `http_status`       INT          DEFAULT NULL,
    `error_code`        VARCHAR(64)  DEFAULT NULL,
    `error_msg`         TEXT         DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`            (`payment_intent_id`),
    KEY `idx_event`         (`event_id`),
    KEY `idx_event_type`    (`event_type`),
    KEY `idx_status`        (`status`),
    -- wave L: cover the ListDue() predicate (status IN (...) AND next_retry_at)
    -- so the cron scan hits one composite instead of two single-column indexes.
    KEY `idx_status_retry`  (`status`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通知日志';

-- 5. Refund 表 refund_22（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_22` (
    `id`                VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    -- P0 #3 幂等键：(payment_intent_id, idempotency_key) 唯一。
    -- 客户端 retry Create 同一退款请求 → DB 兜底防止双行；service 层先查 GetByIdempotencyKey 返已有。
    `idempotency_key`   VARCHAR(128) DEFAULT NULL,
    `amount`            BIGINT       NOT NULL,
    `currency`          CHAR(3)      NOT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/canceled',
    `reason`            VARCHAR(32)  DEFAULT NULL,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `receipt_number`    VARCHAR(64)  DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `auto_compensate`   TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '系统自动补偿退款：retry worker 无限重试直到成功；商户发起=0',
    `retry_count`       INT          NOT NULL DEFAULT 0,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem`       (`payment_intent_id`, `idempotency_key`),
    KEY `idx_charge`          (`charge_id`),
    KEY `idx_pi`              (`payment_intent_id`),
    KEY `idx_status`          (`status`),
    KEY `idx_auto_compensate` (`auto_compensate`),
    KEY `idx_next_retry`      (`next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Refund 表';

-- 7. InboundWebhook 表 inbound_webhook_22
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_22` (
    `id`                VARCHAR(64)  NOT NULL,
    `channel_name`      VARCHAR(32)  NOT NULL,
    `event_id`          VARCHAR(128) NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  DEFAULT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `dispute_id`        VARCHAR(64)  DEFAULT NULL,
    `signature_ok`      TINYINT(1)   NOT NULL DEFAULT 0,
    `headers`           JSON         DEFAULT NULL,
    `body`              MEDIUMBLOB   DEFAULT NULL,
    `processed_at`      DATETIME(3)  DEFAULT NULL,
    `process_status`    VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/succeeded/failed/duplicate',
    `error_msg`         TEXT         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_event`  (`channel_name`, `event_id`),
    KEY         `idx_pi`            (`payment_intent_id`),
    KEY         `idx_event_type`    (`event_type`),
    KEY         `idx_process_status` (`process_status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='入站 webhook 去重日志';

-- 8. Dispute 表 dispute_22
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_22` (
    `id`                   VARCHAR(64)  NOT NULL COMMENT 'dp_xxx',
    `payment_intent_id`    VARCHAR(64)  NOT NULL,
    `charge_id`            VARCHAR(64)  NOT NULL,
    `merchant_id`          VARCHAR(32)  NOT NULL,
    `channel`              VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/paymongo/...',
    `channel_dispute_id`   VARCHAR(128) DEFAULT NULL COMMENT '渠道侧 dispute id (用于回写)',
    `status`               VARCHAR(24)  NOT NULL COMMENT 'needs_response/under_review/won/lost/warning_closed/charge_refunded/canceled',
    `amount`               BIGINT       NOT NULL COMMENT 'disputed amount in minor units',
    `currency`             CHAR(3)      NOT NULL DEFAULT 'PHP',
    `reason`               VARCHAR(64)  NOT NULL COMMENT 'fraud/product_not_received/unrecognized/duplicate/credit_not_processed/other',
    `reason_detail`        VARCHAR(512) DEFAULT NULL,
    `evidence_due_at`      DATETIME(3)  DEFAULT NULL COMMENT '商户响应截止时间',
    `evidence`             JSON         DEFAULT NULL COMMENT '证据包：{text, photos:[url], shipping:{}, emails:[url], receipt:url, ...}',
    `decided_at`           DATETIME(3)  DEFAULT NULL,
    `outcome_amount`       BIGINT       DEFAULT NULL COMMENT '实际被扣回金额（lost 情形），<= amount',
    `auto_refund_charge`   TINYINT(1)   NOT NULL DEFAULT 1 COMMENT 'lost/charge_refunded 时是否联动退款',
    `metadata`             JSON         DEFAULT NULL,
    `created`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_dispute` (`channel`, `channel_dispute_id`),
    KEY `idx_pi`        (`payment_intent_id`),
    KEY `idx_charge`    (`charge_id`),
    KEY `idx_merchant`  (`merchant_id`),
    KEY `idx_status`    (`status`),
    KEY `idx_due`       (`status`, `evidence_due_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute / Chargeback';

-- 9. DisputeEvent 表 dispute_event_22（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_22` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `dispute_id`      VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64) NOT NULL,
    `from_status`     VARCHAR(24)  NOT NULL,
    `to_status`       VARCHAR(24)  NOT NULL,
    `source`          VARCHAR(32)  NOT NULL COMMENT 'channel_webhook / admin / system',
    `actor`           VARCHAR(64)  DEFAULT NULL,
    `note`            VARCHAR(512) DEFAULT NULL,
    `payload`         JSON         DEFAULT NULL COMMENT '触发事件 (webhook body / admin input)',
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_dispute`  (`dispute_id`),
    KEY `idx_pi`       (`payment_intent_id`),
    KEY `idx_created`  (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute 状态流转日志';

-- 10. AccountingOutbox 表 accounting_outbox_22
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_22` (
    `id`                 VARCHAR(64)  NOT NULL,
    `request_id`         VARCHAR(160) NOT NULL,
    `event_type`         VARCHAR(32)  NOT NULL COMMENT 'charge_succeeded/refund_succeeded',
    `payment_intent_id`  VARCHAR(64)  NOT NULL,
    `charge_id`          VARCHAR(64)  DEFAULT NULL,
    `refund_id`          VARCHAR(64)  DEFAULT NULL,
    `owner_type`         VARCHAR(16)  NOT NULL COMMENT 'user/merchant',
    `owner_id`           VARCHAR(64)  NOT NULL,
    `payment_method`     VARCHAR(32)  NOT NULL COMMENT 'gcash/shopeepay/maya/grabpay/...',
    `amount`             BIGINT       NOT NULL,
    `currency`           CHAR(3)      NOT NULL,
    `metadata`           JSON         DEFAULT NULL,
    `status`             VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/sent/failed',
    `attempts`           INT          NOT NULL DEFAULT 0,
    `next_attempt_at`    DATETIME(3)  DEFAULT NULL,
    `last_error`         TEXT         DEFAULT NULL,
    `sent_at`            DATETIME(3)  DEFAULT NULL,
    -- claim_token 用于多 worker / 多副本并发投递时的批次 claim：
    -- worker 用一条 UPDATE ... LIMIT N 把候选行的 next_attempt_at 推到远未来 +
    -- 写入自己生成的 token，其他 worker 的 SELECT 因 next_attempt_at 在未来天然
    -- 跳过；本 worker 再用 SELECT WHERE claim_token = ? 拉回该批次处理。
    -- 模式同 webhook_deliveries.claim_token（见 internal/webhook/delivery.go）。
    `claim_token`        VARCHAR(64)  DEFAULT NULL,
    `created`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_request`       (`request_id`),
    KEY         `idx_pi`           (`payment_intent_id`),
    KEY         `idx_status_next`  (`status`, `next_attempt_at`),
    -- claim_token 反查刚 claim 的批次；带前缀 + 纳秒时间戳 + 随机数，区分度足够
    KEY         `idx_claim`        (`claim_token`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='记账事件 outbox（投递给 accounting-system）';

-- ─── admin_audit_log_22：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_22` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id (from bearer token)',
    `actor_ip`       VARCHAR(64)  DEFAULT NULL,
    `action`         VARCHAR(64)  NOT NULL COMMENT 'merchant.approve / refund.create / risk.rule.update / ...',
    `target_type`    VARCHAR(32)  DEFAULT NULL,
    `target_id`      VARCHAR(64)  DEFAULT NULL,
    `http_method`    VARCHAR(8)   DEFAULT NULL,
    `http_path`      VARCHAR(256) DEFAULT NULL,
    `http_status`    INT          DEFAULT NULL,
    `request_body`   MEDIUMTEXT   DEFAULT NULL COMMENT 'JSON, sensitive fields redacted by caller',
    `response_code`  VARCHAR(32)  DEFAULT NULL,
    `response_msg`   VARCHAR(512) DEFAULT NULL,
    `duration_ms`    INT          DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_action`  (`action`),
    KEY `idx_target`  (`target_type`, `target_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 22';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：2 = 0-9 物理库；23 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_23
CREATE TABLE IF NOT EXISTS `payment_intent_23` (
    `id`                         VARCHAR(64)   NOT NULL,
    `amount`                     BIGINT        NOT NULL COMMENT '应付金额（= subtotal - coupon - points）',
    `amount_subtotal`            BIGINT        NOT NULL DEFAULT 0 COMMENT '原价',
    `amount_coupon`              BIGINT        NOT NULL DEFAULT 0 COMMENT '优惠券抵扣',
    `amount_points`              BIGINT        NOT NULL DEFAULT 0 COMMENT '积分抵扣',
    `currency`                   CHAR(3)       NOT NULL,
    `status`                     VARCHAR(32)   NOT NULL COMMENT '支付生命周期: created/requires_action/processing/succeeded/failed/canceled',
    `refund_phase`               VARCHAR(32)   DEFAULT NULL COMMENT '退款维度状态（独立字段）: refunding/partially_refunded/fully_refunded/refund_failed/refund_canceled',
    `customer_id`                VARCHAR(64)   DEFAULT NULL,
    `description`                VARCHAR(512)  DEFAULT NULL,
    `mch_id`                     VARCHAR(32)   NOT NULL,
    `mch_order_no`               VARCHAR(64)   DEFAULT NULL,
    `business_id`                VARCHAR(64)   DEFAULT NULL COMMENT '分片路由键；pi_id 前缀由此派生（fallback mch_id）',
    `idempotency_key`            VARCHAR(128)  DEFAULT NULL COMMENT '(mch_id,idempotency_key) 唯一；防重',
    `previous_payment_intent_id` VARCHAR(64)   DEFAULT NULL COMMENT '换单重下时指向上一次失败的 PI',
    `capture_method`             VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `confirmation_method`        VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `client_secret`              VARCHAR(128)  DEFAULT NULL,
    `payment_method_types`       JSON          DEFAULT NULL,
    `payment_method`             VARCHAR(32)   DEFAULT NULL,
    `user_id`                    BIGINT        DEFAULT NULL COMMENT '付款用户（可空，匿名 PI 不绑用户）',
    `user_card_id`               BIGINT        DEFAULT NULL COMMENT '卡支付时关联 user_merchant.user_card.id；其它支付方式为空',
    `active_charge_ids`          JSON          DEFAULT NULL COMMENT '进行中的 charge_id 列表（终态后移除）',
    `active_refund_ids`          JSON          DEFAULT NULL COMMENT '进行中的 refund_id 列表（终态后移除）',
    `amount_capturable`          BIGINT        NOT NULL DEFAULT 0,
    `amount_received`            BIGINT        NOT NULL DEFAULT 0,
    `return_url`                 VARCHAR(512)  DEFAULT NULL,
    `notify_url`                 VARCHAR(512)  DEFAULT NULL,
    `statement_descriptor`       VARCHAR(32)   DEFAULT NULL,
    `metadata`                   JSON          DEFAULT NULL,
    `livemode`                   TINYINT(1)    NOT NULL DEFAULT 0,
    `created`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `expired_at`                 DATETIME(3)   DEFAULT NULL,
    `canceled_at`                DATETIME(3)   DEFAULT NULL,
    `cancellation_reason`        VARCHAR(32)   DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_status`     (`status`),
    KEY `idx_mch`        (`mch_id`),
    KEY `idx_customer`   (`customer_id`),
    KEY `idx_business`   (`business_id`),
    UNIQUE KEY `uk_idem` (`mch_id`, `idempotency_key`),
    KEY `idx_prev`       (`previous_payment_intent_id`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Payment Intent 主表';

-- 2. Charge 表 charge_23（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_23` (
    `id`                  VARCHAR(64)   NOT NULL,
    `payment_intent_id`   VARCHAR(64)   NOT NULL,
    `amount`              BIGINT        NOT NULL,
    `amount_captured`     BIGINT        NOT NULL DEFAULT 0,
    `amount_refunded`     BIGINT        NOT NULL DEFAULT 0,
    `currency`            CHAR(3)       NOT NULL,
    `status`              VARCHAR(16)   NOT NULL COMMENT 'pending/succeeded/failed',
    `payment_method`      VARCHAR(32)   NOT NULL,
    `captured`            TINYINT(1)    NOT NULL DEFAULT 0,
    `paid`                TINYINT(1)    NOT NULL DEFAULT 0,
    `refunded`            TINYINT(1)    NOT NULL DEFAULT 0,
    `outcome_risk_level`  VARCHAR(16)   DEFAULT NULL,
    `outcome_risk_score`  INT           DEFAULT NULL,
    `outcome_seller_msg`  VARCHAR(256)  DEFAULT NULL,
    `outcome_type`        VARCHAR(32)   DEFAULT NULL,
    `outcome_reason`      VARCHAR(64)   DEFAULT NULL,
    `outcome_network`     VARCHAR(32)   DEFAULT NULL,
    `failure_code`        VARCHAR(64)   DEFAULT NULL,
    `failure_message`     VARCHAR(512)  DEFAULT NULL,
    `receipt_url`         VARCHAR(512)  DEFAULT NULL,
    `balance_transaction` VARCHAR(64)   DEFAULT NULL,
    `livemode`            TINYINT(1)    NOT NULL DEFAULT 0,
    `metadata`            JSON          DEFAULT NULL,
    `expired_at`          DATETIME(3)   DEFAULT NULL COMMENT '支付单过期时间：超时未完成 → 失败',
    `completed_at`        DATETIME(3)   DEFAULT NULL,
    `created`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_status`     (`status`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Charge 表（一次扣款尝试）';

-- 3. PayAction 表 pay_action_23（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_23` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `action_type`       VARCHAR(32)  NOT NULL COMMENT 'three_d_secure / otp / pay_password',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/expired',
    `payload`           JSON         DEFAULT NULL COMMENT '返回给前端的挑战材料（redirect_url / recipient_masked 等）',
    `expected_secret`   VARCHAR(256) DEFAULT NULL COMMENT 'OTP 明文或密码哈希，不返回给前端',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_attempts`      INT          NOT NULL DEFAULT 0,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `expires_at`        DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_charge`     (`charge_id`),
    KEY `idx_type`       (`action_type`),
    KEY `idx_status`     (`status`),
    KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='PayAction 表（用户挑战：3DS / OTP / PayPassword）';

-- 4. ExceptionCase 表 exception_case_23（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_23` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `case_type`         VARCHAR(32)  NOT NULL COMMENT 'late_refund_success/late_refund_failed/late_charge_success/amount_mismatch/duplicate_webhook',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'open/processing/resolved/ignored',
    `summary`           VARCHAR(512) DEFAULT NULL,
    `payload`           JSON         DEFAULT NULL,
    `resolution`        VARCHAR(512) DEFAULT NULL,
    `assigned_to`       VARCHAR(64)  DEFAULT NULL,
    `resolved_at`       DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`     (`payment_intent_id`),
    KEY `idx_refund` (`refund_id`),
    KEY `idx_type`   (`case_type`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='差错处理单';

-- 5. NotifyLog 表 notify_log_23（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_23` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `event_id`          VARCHAR(64)  NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL COMMENT 'payment_intent.succeeded / charge.refunded / ...',
    `client_type`       VARCHAR(16)  DEFAULT NULL COMMENT 'server/web/ios/android/miniapp/internal',
    `notify_channel`    VARCHAR(16)  NOT NULL COMMENT 'http/apns/fcm/websocket/mq/sms/email',
    `target`            VARCHAR(512) NOT NULL,
    `payload`           BLOB         DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/retrying/succeeded/failed/canceled',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_retries`       INT          NOT NULL DEFAULT 5,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `http_status`       INT          DEFAULT NULL,
    `error_code`        VARCHAR(64)  DEFAULT NULL,
    `error_msg`         TEXT         DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`            (`payment_intent_id`),
    KEY `idx_event`         (`event_id`),
    KEY `idx_event_type`    (`event_type`),
    KEY `idx_status`        (`status`),
    -- wave L: cover the ListDue() predicate (status IN (...) AND next_retry_at)
    -- so the cron scan hits one composite instead of two single-column indexes.
    KEY `idx_status_retry`  (`status`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通知日志';

-- 5. Refund 表 refund_23（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_23` (
    `id`                VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    -- P0 #3 幂等键：(payment_intent_id, idempotency_key) 唯一。
    -- 客户端 retry Create 同一退款请求 → DB 兜底防止双行；service 层先查 GetByIdempotencyKey 返已有。
    `idempotency_key`   VARCHAR(128) DEFAULT NULL,
    `amount`            BIGINT       NOT NULL,
    `currency`          CHAR(3)      NOT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/canceled',
    `reason`            VARCHAR(32)  DEFAULT NULL,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `receipt_number`    VARCHAR(64)  DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `auto_compensate`   TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '系统自动补偿退款：retry worker 无限重试直到成功；商户发起=0',
    `retry_count`       INT          NOT NULL DEFAULT 0,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem`       (`payment_intent_id`, `idempotency_key`),
    KEY `idx_charge`          (`charge_id`),
    KEY `idx_pi`              (`payment_intent_id`),
    KEY `idx_status`          (`status`),
    KEY `idx_auto_compensate` (`auto_compensate`),
    KEY `idx_next_retry`      (`next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Refund 表';

-- 7. InboundWebhook 表 inbound_webhook_23
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_23` (
    `id`                VARCHAR(64)  NOT NULL,
    `channel_name`      VARCHAR(32)  NOT NULL,
    `event_id`          VARCHAR(128) NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  DEFAULT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `dispute_id`        VARCHAR(64)  DEFAULT NULL,
    `signature_ok`      TINYINT(1)   NOT NULL DEFAULT 0,
    `headers`           JSON         DEFAULT NULL,
    `body`              MEDIUMBLOB   DEFAULT NULL,
    `processed_at`      DATETIME(3)  DEFAULT NULL,
    `process_status`    VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/succeeded/failed/duplicate',
    `error_msg`         TEXT         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_event`  (`channel_name`, `event_id`),
    KEY         `idx_pi`            (`payment_intent_id`),
    KEY         `idx_event_type`    (`event_type`),
    KEY         `idx_process_status` (`process_status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='入站 webhook 去重日志';

-- 8. Dispute 表 dispute_23
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_23` (
    `id`                   VARCHAR(64)  NOT NULL COMMENT 'dp_xxx',
    `payment_intent_id`    VARCHAR(64)  NOT NULL,
    `charge_id`            VARCHAR(64)  NOT NULL,
    `merchant_id`          VARCHAR(32)  NOT NULL,
    `channel`              VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/paymongo/...',
    `channel_dispute_id`   VARCHAR(128) DEFAULT NULL COMMENT '渠道侧 dispute id (用于回写)',
    `status`               VARCHAR(24)  NOT NULL COMMENT 'needs_response/under_review/won/lost/warning_closed/charge_refunded/canceled',
    `amount`               BIGINT       NOT NULL COMMENT 'disputed amount in minor units',
    `currency`             CHAR(3)      NOT NULL DEFAULT 'PHP',
    `reason`               VARCHAR(64)  NOT NULL COMMENT 'fraud/product_not_received/unrecognized/duplicate/credit_not_processed/other',
    `reason_detail`        VARCHAR(512) DEFAULT NULL,
    `evidence_due_at`      DATETIME(3)  DEFAULT NULL COMMENT '商户响应截止时间',
    `evidence`             JSON         DEFAULT NULL COMMENT '证据包：{text, photos:[url], shipping:{}, emails:[url], receipt:url, ...}',
    `decided_at`           DATETIME(3)  DEFAULT NULL,
    `outcome_amount`       BIGINT       DEFAULT NULL COMMENT '实际被扣回金额（lost 情形），<= amount',
    `auto_refund_charge`   TINYINT(1)   NOT NULL DEFAULT 1 COMMENT 'lost/charge_refunded 时是否联动退款',
    `metadata`             JSON         DEFAULT NULL,
    `created`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_dispute` (`channel`, `channel_dispute_id`),
    KEY `idx_pi`        (`payment_intent_id`),
    KEY `idx_charge`    (`charge_id`),
    KEY `idx_merchant`  (`merchant_id`),
    KEY `idx_status`    (`status`),
    KEY `idx_due`       (`status`, `evidence_due_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute / Chargeback';

-- 9. DisputeEvent 表 dispute_event_23（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_23` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `dispute_id`      VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64) NOT NULL,
    `from_status`     VARCHAR(24)  NOT NULL,
    `to_status`       VARCHAR(24)  NOT NULL,
    `source`          VARCHAR(32)  NOT NULL COMMENT 'channel_webhook / admin / system',
    `actor`           VARCHAR(64)  DEFAULT NULL,
    `note`            VARCHAR(512) DEFAULT NULL,
    `payload`         JSON         DEFAULT NULL COMMENT '触发事件 (webhook body / admin input)',
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_dispute`  (`dispute_id`),
    KEY `idx_pi`       (`payment_intent_id`),
    KEY `idx_created`  (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute 状态流转日志';

-- 10. AccountingOutbox 表 accounting_outbox_23
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_23` (
    `id`                 VARCHAR(64)  NOT NULL,
    `request_id`         VARCHAR(160) NOT NULL,
    `event_type`         VARCHAR(32)  NOT NULL COMMENT 'charge_succeeded/refund_succeeded',
    `payment_intent_id`  VARCHAR(64)  NOT NULL,
    `charge_id`          VARCHAR(64)  DEFAULT NULL,
    `refund_id`          VARCHAR(64)  DEFAULT NULL,
    `owner_type`         VARCHAR(16)  NOT NULL COMMENT 'user/merchant',
    `owner_id`           VARCHAR(64)  NOT NULL,
    `payment_method`     VARCHAR(32)  NOT NULL COMMENT 'gcash/shopeepay/maya/grabpay/...',
    `amount`             BIGINT       NOT NULL,
    `currency`           CHAR(3)      NOT NULL,
    `metadata`           JSON         DEFAULT NULL,
    `status`             VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/sent/failed',
    `attempts`           INT          NOT NULL DEFAULT 0,
    `next_attempt_at`    DATETIME(3)  DEFAULT NULL,
    `last_error`         TEXT         DEFAULT NULL,
    `sent_at`            DATETIME(3)  DEFAULT NULL,
    -- claim_token 用于多 worker / 多副本并发投递时的批次 claim：
    -- worker 用一条 UPDATE ... LIMIT N 把候选行的 next_attempt_at 推到远未来 +
    -- 写入自己生成的 token，其他 worker 的 SELECT 因 next_attempt_at 在未来天然
    -- 跳过；本 worker 再用 SELECT WHERE claim_token = ? 拉回该批次处理。
    -- 模式同 webhook_deliveries.claim_token（见 internal/webhook/delivery.go）。
    `claim_token`        VARCHAR(64)  DEFAULT NULL,
    `created`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_request`       (`request_id`),
    KEY         `idx_pi`           (`payment_intent_id`),
    KEY         `idx_status_next`  (`status`, `next_attempt_at`),
    -- claim_token 反查刚 claim 的批次；带前缀 + 纳秒时间戳 + 随机数，区分度足够
    KEY         `idx_claim`        (`claim_token`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='记账事件 outbox（投递给 accounting-system）';

-- ─── admin_audit_log_23：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_23` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id (from bearer token)',
    `actor_ip`       VARCHAR(64)  DEFAULT NULL,
    `action`         VARCHAR(64)  NOT NULL COMMENT 'merchant.approve / refund.create / risk.rule.update / ...',
    `target_type`    VARCHAR(32)  DEFAULT NULL,
    `target_id`      VARCHAR(64)  DEFAULT NULL,
    `http_method`    VARCHAR(8)   DEFAULT NULL,
    `http_path`      VARCHAR(256) DEFAULT NULL,
    `http_status`    INT          DEFAULT NULL,
    `request_body`   MEDIUMTEXT   DEFAULT NULL COMMENT 'JSON, sensitive fields redacted by caller',
    `response_code`  VARCHAR(32)  DEFAULT NULL,
    `response_msg`   VARCHAR(512) DEFAULT NULL,
    `duration_ms`    INT          DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_action`  (`action`),
    KEY `idx_target`  (`target_type`, `target_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 23';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：2 = 0-9 物理库；24 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_24
CREATE TABLE IF NOT EXISTS `payment_intent_24` (
    `id`                         VARCHAR(64)   NOT NULL,
    `amount`                     BIGINT        NOT NULL COMMENT '应付金额（= subtotal - coupon - points）',
    `amount_subtotal`            BIGINT        NOT NULL DEFAULT 0 COMMENT '原价',
    `amount_coupon`              BIGINT        NOT NULL DEFAULT 0 COMMENT '优惠券抵扣',
    `amount_points`              BIGINT        NOT NULL DEFAULT 0 COMMENT '积分抵扣',
    `currency`                   CHAR(3)       NOT NULL,
    `status`                     VARCHAR(32)   NOT NULL COMMENT '支付生命周期: created/requires_action/processing/succeeded/failed/canceled',
    `refund_phase`               VARCHAR(32)   DEFAULT NULL COMMENT '退款维度状态（独立字段）: refunding/partially_refunded/fully_refunded/refund_failed/refund_canceled',
    `customer_id`                VARCHAR(64)   DEFAULT NULL,
    `description`                VARCHAR(512)  DEFAULT NULL,
    `mch_id`                     VARCHAR(32)   NOT NULL,
    `mch_order_no`               VARCHAR(64)   DEFAULT NULL,
    `business_id`                VARCHAR(64)   DEFAULT NULL COMMENT '分片路由键；pi_id 前缀由此派生（fallback mch_id）',
    `idempotency_key`            VARCHAR(128)  DEFAULT NULL COMMENT '(mch_id,idempotency_key) 唯一；防重',
    `previous_payment_intent_id` VARCHAR(64)   DEFAULT NULL COMMENT '换单重下时指向上一次失败的 PI',
    `capture_method`             VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `confirmation_method`        VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `client_secret`              VARCHAR(128)  DEFAULT NULL,
    `payment_method_types`       JSON          DEFAULT NULL,
    `payment_method`             VARCHAR(32)   DEFAULT NULL,
    `user_id`                    BIGINT        DEFAULT NULL COMMENT '付款用户（可空，匿名 PI 不绑用户）',
    `user_card_id`               BIGINT        DEFAULT NULL COMMENT '卡支付时关联 user_merchant.user_card.id；其它支付方式为空',
    `active_charge_ids`          JSON          DEFAULT NULL COMMENT '进行中的 charge_id 列表（终态后移除）',
    `active_refund_ids`          JSON          DEFAULT NULL COMMENT '进行中的 refund_id 列表（终态后移除）',
    `amount_capturable`          BIGINT        NOT NULL DEFAULT 0,
    `amount_received`            BIGINT        NOT NULL DEFAULT 0,
    `return_url`                 VARCHAR(512)  DEFAULT NULL,
    `notify_url`                 VARCHAR(512)  DEFAULT NULL,
    `statement_descriptor`       VARCHAR(32)   DEFAULT NULL,
    `metadata`                   JSON          DEFAULT NULL,
    `livemode`                   TINYINT(1)    NOT NULL DEFAULT 0,
    `created`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `expired_at`                 DATETIME(3)   DEFAULT NULL,
    `canceled_at`                DATETIME(3)   DEFAULT NULL,
    `cancellation_reason`        VARCHAR(32)   DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_status`     (`status`),
    KEY `idx_mch`        (`mch_id`),
    KEY `idx_customer`   (`customer_id`),
    KEY `idx_business`   (`business_id`),
    UNIQUE KEY `uk_idem` (`mch_id`, `idempotency_key`),
    KEY `idx_prev`       (`previous_payment_intent_id`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Payment Intent 主表';

-- 2. Charge 表 charge_24（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_24` (
    `id`                  VARCHAR(64)   NOT NULL,
    `payment_intent_id`   VARCHAR(64)   NOT NULL,
    `amount`              BIGINT        NOT NULL,
    `amount_captured`     BIGINT        NOT NULL DEFAULT 0,
    `amount_refunded`     BIGINT        NOT NULL DEFAULT 0,
    `currency`            CHAR(3)       NOT NULL,
    `status`              VARCHAR(16)   NOT NULL COMMENT 'pending/succeeded/failed',
    `payment_method`      VARCHAR(32)   NOT NULL,
    `captured`            TINYINT(1)    NOT NULL DEFAULT 0,
    `paid`                TINYINT(1)    NOT NULL DEFAULT 0,
    `refunded`            TINYINT(1)    NOT NULL DEFAULT 0,
    `outcome_risk_level`  VARCHAR(16)   DEFAULT NULL,
    `outcome_risk_score`  INT           DEFAULT NULL,
    `outcome_seller_msg`  VARCHAR(256)  DEFAULT NULL,
    `outcome_type`        VARCHAR(32)   DEFAULT NULL,
    `outcome_reason`      VARCHAR(64)   DEFAULT NULL,
    `outcome_network`     VARCHAR(32)   DEFAULT NULL,
    `failure_code`        VARCHAR(64)   DEFAULT NULL,
    `failure_message`     VARCHAR(512)  DEFAULT NULL,
    `receipt_url`         VARCHAR(512)  DEFAULT NULL,
    `balance_transaction` VARCHAR(64)   DEFAULT NULL,
    `livemode`            TINYINT(1)    NOT NULL DEFAULT 0,
    `metadata`            JSON          DEFAULT NULL,
    `expired_at`          DATETIME(3)   DEFAULT NULL COMMENT '支付单过期时间：超时未完成 → 失败',
    `completed_at`        DATETIME(3)   DEFAULT NULL,
    `created`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_status`     (`status`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Charge 表（一次扣款尝试）';

-- 3. PayAction 表 pay_action_24（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_24` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `action_type`       VARCHAR(32)  NOT NULL COMMENT 'three_d_secure / otp / pay_password',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/expired',
    `payload`           JSON         DEFAULT NULL COMMENT '返回给前端的挑战材料（redirect_url / recipient_masked 等）',
    `expected_secret`   VARCHAR(256) DEFAULT NULL COMMENT 'OTP 明文或密码哈希，不返回给前端',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_attempts`      INT          NOT NULL DEFAULT 0,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `expires_at`        DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_charge`     (`charge_id`),
    KEY `idx_type`       (`action_type`),
    KEY `idx_status`     (`status`),
    KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='PayAction 表（用户挑战：3DS / OTP / PayPassword）';

-- 4. ExceptionCase 表 exception_case_24（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_24` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `case_type`         VARCHAR(32)  NOT NULL COMMENT 'late_refund_success/late_refund_failed/late_charge_success/amount_mismatch/duplicate_webhook',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'open/processing/resolved/ignored',
    `summary`           VARCHAR(512) DEFAULT NULL,
    `payload`           JSON         DEFAULT NULL,
    `resolution`        VARCHAR(512) DEFAULT NULL,
    `assigned_to`       VARCHAR(64)  DEFAULT NULL,
    `resolved_at`       DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`     (`payment_intent_id`),
    KEY `idx_refund` (`refund_id`),
    KEY `idx_type`   (`case_type`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='差错处理单';

-- 5. NotifyLog 表 notify_log_24（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_24` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `event_id`          VARCHAR(64)  NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL COMMENT 'payment_intent.succeeded / charge.refunded / ...',
    `client_type`       VARCHAR(16)  DEFAULT NULL COMMENT 'server/web/ios/android/miniapp/internal',
    `notify_channel`    VARCHAR(16)  NOT NULL COMMENT 'http/apns/fcm/websocket/mq/sms/email',
    `target`            VARCHAR(512) NOT NULL,
    `payload`           BLOB         DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/retrying/succeeded/failed/canceled',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_retries`       INT          NOT NULL DEFAULT 5,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `http_status`       INT          DEFAULT NULL,
    `error_code`        VARCHAR(64)  DEFAULT NULL,
    `error_msg`         TEXT         DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`            (`payment_intent_id`),
    KEY `idx_event`         (`event_id`),
    KEY `idx_event_type`    (`event_type`),
    KEY `idx_status`        (`status`),
    -- wave L: cover the ListDue() predicate (status IN (...) AND next_retry_at)
    -- so the cron scan hits one composite instead of two single-column indexes.
    KEY `idx_status_retry`  (`status`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通知日志';

-- 5. Refund 表 refund_24（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_24` (
    `id`                VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    -- P0 #3 幂等键：(payment_intent_id, idempotency_key) 唯一。
    -- 客户端 retry Create 同一退款请求 → DB 兜底防止双行；service 层先查 GetByIdempotencyKey 返已有。
    `idempotency_key`   VARCHAR(128) DEFAULT NULL,
    `amount`            BIGINT       NOT NULL,
    `currency`          CHAR(3)      NOT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/canceled',
    `reason`            VARCHAR(32)  DEFAULT NULL,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `receipt_number`    VARCHAR(64)  DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `auto_compensate`   TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '系统自动补偿退款：retry worker 无限重试直到成功；商户发起=0',
    `retry_count`       INT          NOT NULL DEFAULT 0,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem`       (`payment_intent_id`, `idempotency_key`),
    KEY `idx_charge`          (`charge_id`),
    KEY `idx_pi`              (`payment_intent_id`),
    KEY `idx_status`          (`status`),
    KEY `idx_auto_compensate` (`auto_compensate`),
    KEY `idx_next_retry`      (`next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Refund 表';

-- 7. InboundWebhook 表 inbound_webhook_24
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_24` (
    `id`                VARCHAR(64)  NOT NULL,
    `channel_name`      VARCHAR(32)  NOT NULL,
    `event_id`          VARCHAR(128) NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  DEFAULT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `dispute_id`        VARCHAR(64)  DEFAULT NULL,
    `signature_ok`      TINYINT(1)   NOT NULL DEFAULT 0,
    `headers`           JSON         DEFAULT NULL,
    `body`              MEDIUMBLOB   DEFAULT NULL,
    `processed_at`      DATETIME(3)  DEFAULT NULL,
    `process_status`    VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/succeeded/failed/duplicate',
    `error_msg`         TEXT         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_event`  (`channel_name`, `event_id`),
    KEY         `idx_pi`            (`payment_intent_id`),
    KEY         `idx_event_type`    (`event_type`),
    KEY         `idx_process_status` (`process_status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='入站 webhook 去重日志';

-- 8. Dispute 表 dispute_24
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_24` (
    `id`                   VARCHAR(64)  NOT NULL COMMENT 'dp_xxx',
    `payment_intent_id`    VARCHAR(64)  NOT NULL,
    `charge_id`            VARCHAR(64)  NOT NULL,
    `merchant_id`          VARCHAR(32)  NOT NULL,
    `channel`              VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/paymongo/...',
    `channel_dispute_id`   VARCHAR(128) DEFAULT NULL COMMENT '渠道侧 dispute id (用于回写)',
    `status`               VARCHAR(24)  NOT NULL COMMENT 'needs_response/under_review/won/lost/warning_closed/charge_refunded/canceled',
    `amount`               BIGINT       NOT NULL COMMENT 'disputed amount in minor units',
    `currency`             CHAR(3)      NOT NULL DEFAULT 'PHP',
    `reason`               VARCHAR(64)  NOT NULL COMMENT 'fraud/product_not_received/unrecognized/duplicate/credit_not_processed/other',
    `reason_detail`        VARCHAR(512) DEFAULT NULL,
    `evidence_due_at`      DATETIME(3)  DEFAULT NULL COMMENT '商户响应截止时间',
    `evidence`             JSON         DEFAULT NULL COMMENT '证据包：{text, photos:[url], shipping:{}, emails:[url], receipt:url, ...}',
    `decided_at`           DATETIME(3)  DEFAULT NULL,
    `outcome_amount`       BIGINT       DEFAULT NULL COMMENT '实际被扣回金额（lost 情形），<= amount',
    `auto_refund_charge`   TINYINT(1)   NOT NULL DEFAULT 1 COMMENT 'lost/charge_refunded 时是否联动退款',
    `metadata`             JSON         DEFAULT NULL,
    `created`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_dispute` (`channel`, `channel_dispute_id`),
    KEY `idx_pi`        (`payment_intent_id`),
    KEY `idx_charge`    (`charge_id`),
    KEY `idx_merchant`  (`merchant_id`),
    KEY `idx_status`    (`status`),
    KEY `idx_due`       (`status`, `evidence_due_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute / Chargeback';

-- 9. DisputeEvent 表 dispute_event_24（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_24` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `dispute_id`      VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64) NOT NULL,
    `from_status`     VARCHAR(24)  NOT NULL,
    `to_status`       VARCHAR(24)  NOT NULL,
    `source`          VARCHAR(32)  NOT NULL COMMENT 'channel_webhook / admin / system',
    `actor`           VARCHAR(64)  DEFAULT NULL,
    `note`            VARCHAR(512) DEFAULT NULL,
    `payload`         JSON         DEFAULT NULL COMMENT '触发事件 (webhook body / admin input)',
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_dispute`  (`dispute_id`),
    KEY `idx_pi`       (`payment_intent_id`),
    KEY `idx_created`  (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute 状态流转日志';

-- 10. AccountingOutbox 表 accounting_outbox_24
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_24` (
    `id`                 VARCHAR(64)  NOT NULL,
    `request_id`         VARCHAR(160) NOT NULL,
    `event_type`         VARCHAR(32)  NOT NULL COMMENT 'charge_succeeded/refund_succeeded',
    `payment_intent_id`  VARCHAR(64)  NOT NULL,
    `charge_id`          VARCHAR(64)  DEFAULT NULL,
    `refund_id`          VARCHAR(64)  DEFAULT NULL,
    `owner_type`         VARCHAR(16)  NOT NULL COMMENT 'user/merchant',
    `owner_id`           VARCHAR(64)  NOT NULL,
    `payment_method`     VARCHAR(32)  NOT NULL COMMENT 'gcash/shopeepay/maya/grabpay/...',
    `amount`             BIGINT       NOT NULL,
    `currency`           CHAR(3)      NOT NULL,
    `metadata`           JSON         DEFAULT NULL,
    `status`             VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/sent/failed',
    `attempts`           INT          NOT NULL DEFAULT 0,
    `next_attempt_at`    DATETIME(3)  DEFAULT NULL,
    `last_error`         TEXT         DEFAULT NULL,
    `sent_at`            DATETIME(3)  DEFAULT NULL,
    -- claim_token 用于多 worker / 多副本并发投递时的批次 claim：
    -- worker 用一条 UPDATE ... LIMIT N 把候选行的 next_attempt_at 推到远未来 +
    -- 写入自己生成的 token，其他 worker 的 SELECT 因 next_attempt_at 在未来天然
    -- 跳过；本 worker 再用 SELECT WHERE claim_token = ? 拉回该批次处理。
    -- 模式同 webhook_deliveries.claim_token（见 internal/webhook/delivery.go）。
    `claim_token`        VARCHAR(64)  DEFAULT NULL,
    `created`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_request`       (`request_id`),
    KEY         `idx_pi`           (`payment_intent_id`),
    KEY         `idx_status_next`  (`status`, `next_attempt_at`),
    -- claim_token 反查刚 claim 的批次；带前缀 + 纳秒时间戳 + 随机数，区分度足够
    KEY         `idx_claim`        (`claim_token`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='记账事件 outbox（投递给 accounting-system）';

-- ─── admin_audit_log_24：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_24` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id (from bearer token)',
    `actor_ip`       VARCHAR(64)  DEFAULT NULL,
    `action`         VARCHAR(64)  NOT NULL COMMENT 'merchant.approve / refund.create / risk.rule.update / ...',
    `target_type`    VARCHAR(32)  DEFAULT NULL,
    `target_id`      VARCHAR(64)  DEFAULT NULL,
    `http_method`    VARCHAR(8)   DEFAULT NULL,
    `http_path`      VARCHAR(256) DEFAULT NULL,
    `http_status`    INT          DEFAULT NULL,
    `request_body`   MEDIUMTEXT   DEFAULT NULL COMMENT 'JSON, sensitive fields redacted by caller',
    `response_code`  VARCHAR(32)  DEFAULT NULL,
    `response_msg`   VARCHAR(512) DEFAULT NULL,
    `duration_ms`    INT          DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_action`  (`action`),
    KEY `idx_target`  (`target_type`, `target_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 24';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：2 = 0-9 物理库；25 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_25
CREATE TABLE IF NOT EXISTS `payment_intent_25` (
    `id`                         VARCHAR(64)   NOT NULL,
    `amount`                     BIGINT        NOT NULL COMMENT '应付金额（= subtotal - coupon - points）',
    `amount_subtotal`            BIGINT        NOT NULL DEFAULT 0 COMMENT '原价',
    `amount_coupon`              BIGINT        NOT NULL DEFAULT 0 COMMENT '优惠券抵扣',
    `amount_points`              BIGINT        NOT NULL DEFAULT 0 COMMENT '积分抵扣',
    `currency`                   CHAR(3)       NOT NULL,
    `status`                     VARCHAR(32)   NOT NULL COMMENT '支付生命周期: created/requires_action/processing/succeeded/failed/canceled',
    `refund_phase`               VARCHAR(32)   DEFAULT NULL COMMENT '退款维度状态（独立字段）: refunding/partially_refunded/fully_refunded/refund_failed/refund_canceled',
    `customer_id`                VARCHAR(64)   DEFAULT NULL,
    `description`                VARCHAR(512)  DEFAULT NULL,
    `mch_id`                     VARCHAR(32)   NOT NULL,
    `mch_order_no`               VARCHAR(64)   DEFAULT NULL,
    `business_id`                VARCHAR(64)   DEFAULT NULL COMMENT '分片路由键；pi_id 前缀由此派生（fallback mch_id）',
    `idempotency_key`            VARCHAR(128)  DEFAULT NULL COMMENT '(mch_id,idempotency_key) 唯一；防重',
    `previous_payment_intent_id` VARCHAR(64)   DEFAULT NULL COMMENT '换单重下时指向上一次失败的 PI',
    `capture_method`             VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `confirmation_method`        VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `client_secret`              VARCHAR(128)  DEFAULT NULL,
    `payment_method_types`       JSON          DEFAULT NULL,
    `payment_method`             VARCHAR(32)   DEFAULT NULL,
    `user_id`                    BIGINT        DEFAULT NULL COMMENT '付款用户（可空，匿名 PI 不绑用户）',
    `user_card_id`               BIGINT        DEFAULT NULL COMMENT '卡支付时关联 user_merchant.user_card.id；其它支付方式为空',
    `active_charge_ids`          JSON          DEFAULT NULL COMMENT '进行中的 charge_id 列表（终态后移除）',
    `active_refund_ids`          JSON          DEFAULT NULL COMMENT '进行中的 refund_id 列表（终态后移除）',
    `amount_capturable`          BIGINT        NOT NULL DEFAULT 0,
    `amount_received`            BIGINT        NOT NULL DEFAULT 0,
    `return_url`                 VARCHAR(512)  DEFAULT NULL,
    `notify_url`                 VARCHAR(512)  DEFAULT NULL,
    `statement_descriptor`       VARCHAR(32)   DEFAULT NULL,
    `metadata`                   JSON          DEFAULT NULL,
    `livemode`                   TINYINT(1)    NOT NULL DEFAULT 0,
    `created`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `expired_at`                 DATETIME(3)   DEFAULT NULL,
    `canceled_at`                DATETIME(3)   DEFAULT NULL,
    `cancellation_reason`        VARCHAR(32)   DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_status`     (`status`),
    KEY `idx_mch`        (`mch_id`),
    KEY `idx_customer`   (`customer_id`),
    KEY `idx_business`   (`business_id`),
    UNIQUE KEY `uk_idem` (`mch_id`, `idempotency_key`),
    KEY `idx_prev`       (`previous_payment_intent_id`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Payment Intent 主表';

-- 2. Charge 表 charge_25（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_25` (
    `id`                  VARCHAR(64)   NOT NULL,
    `payment_intent_id`   VARCHAR(64)   NOT NULL,
    `amount`              BIGINT        NOT NULL,
    `amount_captured`     BIGINT        NOT NULL DEFAULT 0,
    `amount_refunded`     BIGINT        NOT NULL DEFAULT 0,
    `currency`            CHAR(3)       NOT NULL,
    `status`              VARCHAR(16)   NOT NULL COMMENT 'pending/succeeded/failed',
    `payment_method`      VARCHAR(32)   NOT NULL,
    `captured`            TINYINT(1)    NOT NULL DEFAULT 0,
    `paid`                TINYINT(1)    NOT NULL DEFAULT 0,
    `refunded`            TINYINT(1)    NOT NULL DEFAULT 0,
    `outcome_risk_level`  VARCHAR(16)   DEFAULT NULL,
    `outcome_risk_score`  INT           DEFAULT NULL,
    `outcome_seller_msg`  VARCHAR(256)  DEFAULT NULL,
    `outcome_type`        VARCHAR(32)   DEFAULT NULL,
    `outcome_reason`      VARCHAR(64)   DEFAULT NULL,
    `outcome_network`     VARCHAR(32)   DEFAULT NULL,
    `failure_code`        VARCHAR(64)   DEFAULT NULL,
    `failure_message`     VARCHAR(512)  DEFAULT NULL,
    `receipt_url`         VARCHAR(512)  DEFAULT NULL,
    `balance_transaction` VARCHAR(64)   DEFAULT NULL,
    `livemode`            TINYINT(1)    NOT NULL DEFAULT 0,
    `metadata`            JSON          DEFAULT NULL,
    `expired_at`          DATETIME(3)   DEFAULT NULL COMMENT '支付单过期时间：超时未完成 → 失败',
    `completed_at`        DATETIME(3)   DEFAULT NULL,
    `created`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_status`     (`status`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Charge 表（一次扣款尝试）';

-- 3. PayAction 表 pay_action_25（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_25` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `action_type`       VARCHAR(32)  NOT NULL COMMENT 'three_d_secure / otp / pay_password',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/expired',
    `payload`           JSON         DEFAULT NULL COMMENT '返回给前端的挑战材料（redirect_url / recipient_masked 等）',
    `expected_secret`   VARCHAR(256) DEFAULT NULL COMMENT 'OTP 明文或密码哈希，不返回给前端',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_attempts`      INT          NOT NULL DEFAULT 0,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `expires_at`        DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_charge`     (`charge_id`),
    KEY `idx_type`       (`action_type`),
    KEY `idx_status`     (`status`),
    KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='PayAction 表（用户挑战：3DS / OTP / PayPassword）';

-- 4. ExceptionCase 表 exception_case_25（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_25` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `case_type`         VARCHAR(32)  NOT NULL COMMENT 'late_refund_success/late_refund_failed/late_charge_success/amount_mismatch/duplicate_webhook',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'open/processing/resolved/ignored',
    `summary`           VARCHAR(512) DEFAULT NULL,
    `payload`           JSON         DEFAULT NULL,
    `resolution`        VARCHAR(512) DEFAULT NULL,
    `assigned_to`       VARCHAR(64)  DEFAULT NULL,
    `resolved_at`       DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`     (`payment_intent_id`),
    KEY `idx_refund` (`refund_id`),
    KEY `idx_type`   (`case_type`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='差错处理单';

-- 5. NotifyLog 表 notify_log_25（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_25` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `event_id`          VARCHAR(64)  NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL COMMENT 'payment_intent.succeeded / charge.refunded / ...',
    `client_type`       VARCHAR(16)  DEFAULT NULL COMMENT 'server/web/ios/android/miniapp/internal',
    `notify_channel`    VARCHAR(16)  NOT NULL COMMENT 'http/apns/fcm/websocket/mq/sms/email',
    `target`            VARCHAR(512) NOT NULL,
    `payload`           BLOB         DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/retrying/succeeded/failed/canceled',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_retries`       INT          NOT NULL DEFAULT 5,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `http_status`       INT          DEFAULT NULL,
    `error_code`        VARCHAR(64)  DEFAULT NULL,
    `error_msg`         TEXT         DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`            (`payment_intent_id`),
    KEY `idx_event`         (`event_id`),
    KEY `idx_event_type`    (`event_type`),
    KEY `idx_status`        (`status`),
    -- wave L: cover the ListDue() predicate (status IN (...) AND next_retry_at)
    -- so the cron scan hits one composite instead of two single-column indexes.
    KEY `idx_status_retry`  (`status`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通知日志';

-- 5. Refund 表 refund_25（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_25` (
    `id`                VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    -- P0 #3 幂等键：(payment_intent_id, idempotency_key) 唯一。
    -- 客户端 retry Create 同一退款请求 → DB 兜底防止双行；service 层先查 GetByIdempotencyKey 返已有。
    `idempotency_key`   VARCHAR(128) DEFAULT NULL,
    `amount`            BIGINT       NOT NULL,
    `currency`          CHAR(3)      NOT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/canceled',
    `reason`            VARCHAR(32)  DEFAULT NULL,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `receipt_number`    VARCHAR(64)  DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `auto_compensate`   TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '系统自动补偿退款：retry worker 无限重试直到成功；商户发起=0',
    `retry_count`       INT          NOT NULL DEFAULT 0,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem`       (`payment_intent_id`, `idempotency_key`),
    KEY `idx_charge`          (`charge_id`),
    KEY `idx_pi`              (`payment_intent_id`),
    KEY `idx_status`          (`status`),
    KEY `idx_auto_compensate` (`auto_compensate`),
    KEY `idx_next_retry`      (`next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Refund 表';

-- 7. InboundWebhook 表 inbound_webhook_25
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_25` (
    `id`                VARCHAR(64)  NOT NULL,
    `channel_name`      VARCHAR(32)  NOT NULL,
    `event_id`          VARCHAR(128) NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  DEFAULT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `dispute_id`        VARCHAR(64)  DEFAULT NULL,
    `signature_ok`      TINYINT(1)   NOT NULL DEFAULT 0,
    `headers`           JSON         DEFAULT NULL,
    `body`              MEDIUMBLOB   DEFAULT NULL,
    `processed_at`      DATETIME(3)  DEFAULT NULL,
    `process_status`    VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/succeeded/failed/duplicate',
    `error_msg`         TEXT         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_event`  (`channel_name`, `event_id`),
    KEY         `idx_pi`            (`payment_intent_id`),
    KEY         `idx_event_type`    (`event_type`),
    KEY         `idx_process_status` (`process_status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='入站 webhook 去重日志';

-- 8. Dispute 表 dispute_25
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_25` (
    `id`                   VARCHAR(64)  NOT NULL COMMENT 'dp_xxx',
    `payment_intent_id`    VARCHAR(64)  NOT NULL,
    `charge_id`            VARCHAR(64)  NOT NULL,
    `merchant_id`          VARCHAR(32)  NOT NULL,
    `channel`              VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/paymongo/...',
    `channel_dispute_id`   VARCHAR(128) DEFAULT NULL COMMENT '渠道侧 dispute id (用于回写)',
    `status`               VARCHAR(24)  NOT NULL COMMENT 'needs_response/under_review/won/lost/warning_closed/charge_refunded/canceled',
    `amount`               BIGINT       NOT NULL COMMENT 'disputed amount in minor units',
    `currency`             CHAR(3)      NOT NULL DEFAULT 'PHP',
    `reason`               VARCHAR(64)  NOT NULL COMMENT 'fraud/product_not_received/unrecognized/duplicate/credit_not_processed/other',
    `reason_detail`        VARCHAR(512) DEFAULT NULL,
    `evidence_due_at`      DATETIME(3)  DEFAULT NULL COMMENT '商户响应截止时间',
    `evidence`             JSON         DEFAULT NULL COMMENT '证据包：{text, photos:[url], shipping:{}, emails:[url], receipt:url, ...}',
    `decided_at`           DATETIME(3)  DEFAULT NULL,
    `outcome_amount`       BIGINT       DEFAULT NULL COMMENT '实际被扣回金额（lost 情形），<= amount',
    `auto_refund_charge`   TINYINT(1)   NOT NULL DEFAULT 1 COMMENT 'lost/charge_refunded 时是否联动退款',
    `metadata`             JSON         DEFAULT NULL,
    `created`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_dispute` (`channel`, `channel_dispute_id`),
    KEY `idx_pi`        (`payment_intent_id`),
    KEY `idx_charge`    (`charge_id`),
    KEY `idx_merchant`  (`merchant_id`),
    KEY `idx_status`    (`status`),
    KEY `idx_due`       (`status`, `evidence_due_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute / Chargeback';

-- 9. DisputeEvent 表 dispute_event_25（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_25` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `dispute_id`      VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64) NOT NULL,
    `from_status`     VARCHAR(24)  NOT NULL,
    `to_status`       VARCHAR(24)  NOT NULL,
    `source`          VARCHAR(32)  NOT NULL COMMENT 'channel_webhook / admin / system',
    `actor`           VARCHAR(64)  DEFAULT NULL,
    `note`            VARCHAR(512) DEFAULT NULL,
    `payload`         JSON         DEFAULT NULL COMMENT '触发事件 (webhook body / admin input)',
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_dispute`  (`dispute_id`),
    KEY `idx_pi`       (`payment_intent_id`),
    KEY `idx_created`  (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute 状态流转日志';

-- 10. AccountingOutbox 表 accounting_outbox_25
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_25` (
    `id`                 VARCHAR(64)  NOT NULL,
    `request_id`         VARCHAR(160) NOT NULL,
    `event_type`         VARCHAR(32)  NOT NULL COMMENT 'charge_succeeded/refund_succeeded',
    `payment_intent_id`  VARCHAR(64)  NOT NULL,
    `charge_id`          VARCHAR(64)  DEFAULT NULL,
    `refund_id`          VARCHAR(64)  DEFAULT NULL,
    `owner_type`         VARCHAR(16)  NOT NULL COMMENT 'user/merchant',
    `owner_id`           VARCHAR(64)  NOT NULL,
    `payment_method`     VARCHAR(32)  NOT NULL COMMENT 'gcash/shopeepay/maya/grabpay/...',
    `amount`             BIGINT       NOT NULL,
    `currency`           CHAR(3)      NOT NULL,
    `metadata`           JSON         DEFAULT NULL,
    `status`             VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/sent/failed',
    `attempts`           INT          NOT NULL DEFAULT 0,
    `next_attempt_at`    DATETIME(3)  DEFAULT NULL,
    `last_error`         TEXT         DEFAULT NULL,
    `sent_at`            DATETIME(3)  DEFAULT NULL,
    -- claim_token 用于多 worker / 多副本并发投递时的批次 claim：
    -- worker 用一条 UPDATE ... LIMIT N 把候选行的 next_attempt_at 推到远未来 +
    -- 写入自己生成的 token，其他 worker 的 SELECT 因 next_attempt_at 在未来天然
    -- 跳过；本 worker 再用 SELECT WHERE claim_token = ? 拉回该批次处理。
    -- 模式同 webhook_deliveries.claim_token（见 internal/webhook/delivery.go）。
    `claim_token`        VARCHAR(64)  DEFAULT NULL,
    `created`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_request`       (`request_id`),
    KEY         `idx_pi`           (`payment_intent_id`),
    KEY         `idx_status_next`  (`status`, `next_attempt_at`),
    -- claim_token 反查刚 claim 的批次；带前缀 + 纳秒时间戳 + 随机数，区分度足够
    KEY         `idx_claim`        (`claim_token`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='记账事件 outbox（投递给 accounting-system）';

-- ─── admin_audit_log_25：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_25` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id (from bearer token)',
    `actor_ip`       VARCHAR(64)  DEFAULT NULL,
    `action`         VARCHAR(64)  NOT NULL COMMENT 'merchant.approve / refund.create / risk.rule.update / ...',
    `target_type`    VARCHAR(32)  DEFAULT NULL,
    `target_id`      VARCHAR(64)  DEFAULT NULL,
    `http_method`    VARCHAR(8)   DEFAULT NULL,
    `http_path`      VARCHAR(256) DEFAULT NULL,
    `http_status`    INT          DEFAULT NULL,
    `request_body`   MEDIUMTEXT   DEFAULT NULL COMMENT 'JSON, sensitive fields redacted by caller',
    `response_code`  VARCHAR(32)  DEFAULT NULL,
    `response_msg`   VARCHAR(512) DEFAULT NULL,
    `duration_ms`    INT          DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_action`  (`action`),
    KEY `idx_target`  (`target_type`, `target_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 25';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：2 = 0-9 物理库；26 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_26
CREATE TABLE IF NOT EXISTS `payment_intent_26` (
    `id`                         VARCHAR(64)   NOT NULL,
    `amount`                     BIGINT        NOT NULL COMMENT '应付金额（= subtotal - coupon - points）',
    `amount_subtotal`            BIGINT        NOT NULL DEFAULT 0 COMMENT '原价',
    `amount_coupon`              BIGINT        NOT NULL DEFAULT 0 COMMENT '优惠券抵扣',
    `amount_points`              BIGINT        NOT NULL DEFAULT 0 COMMENT '积分抵扣',
    `currency`                   CHAR(3)       NOT NULL,
    `status`                     VARCHAR(32)   NOT NULL COMMENT '支付生命周期: created/requires_action/processing/succeeded/failed/canceled',
    `refund_phase`               VARCHAR(32)   DEFAULT NULL COMMENT '退款维度状态（独立字段）: refunding/partially_refunded/fully_refunded/refund_failed/refund_canceled',
    `customer_id`                VARCHAR(64)   DEFAULT NULL,
    `description`                VARCHAR(512)  DEFAULT NULL,
    `mch_id`                     VARCHAR(32)   NOT NULL,
    `mch_order_no`               VARCHAR(64)   DEFAULT NULL,
    `business_id`                VARCHAR(64)   DEFAULT NULL COMMENT '分片路由键；pi_id 前缀由此派生（fallback mch_id）',
    `idempotency_key`            VARCHAR(128)  DEFAULT NULL COMMENT '(mch_id,idempotency_key) 唯一；防重',
    `previous_payment_intent_id` VARCHAR(64)   DEFAULT NULL COMMENT '换单重下时指向上一次失败的 PI',
    `capture_method`             VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `confirmation_method`        VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `client_secret`              VARCHAR(128)  DEFAULT NULL,
    `payment_method_types`       JSON          DEFAULT NULL,
    `payment_method`             VARCHAR(32)   DEFAULT NULL,
    `user_id`                    BIGINT        DEFAULT NULL COMMENT '付款用户（可空，匿名 PI 不绑用户）',
    `user_card_id`               BIGINT        DEFAULT NULL COMMENT '卡支付时关联 user_merchant.user_card.id；其它支付方式为空',
    `active_charge_ids`          JSON          DEFAULT NULL COMMENT '进行中的 charge_id 列表（终态后移除）',
    `active_refund_ids`          JSON          DEFAULT NULL COMMENT '进行中的 refund_id 列表（终态后移除）',
    `amount_capturable`          BIGINT        NOT NULL DEFAULT 0,
    `amount_received`            BIGINT        NOT NULL DEFAULT 0,
    `return_url`                 VARCHAR(512)  DEFAULT NULL,
    `notify_url`                 VARCHAR(512)  DEFAULT NULL,
    `statement_descriptor`       VARCHAR(32)   DEFAULT NULL,
    `metadata`                   JSON          DEFAULT NULL,
    `livemode`                   TINYINT(1)    NOT NULL DEFAULT 0,
    `created`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `expired_at`                 DATETIME(3)   DEFAULT NULL,
    `canceled_at`                DATETIME(3)   DEFAULT NULL,
    `cancellation_reason`        VARCHAR(32)   DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_status`     (`status`),
    KEY `idx_mch`        (`mch_id`),
    KEY `idx_customer`   (`customer_id`),
    KEY `idx_business`   (`business_id`),
    UNIQUE KEY `uk_idem` (`mch_id`, `idempotency_key`),
    KEY `idx_prev`       (`previous_payment_intent_id`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Payment Intent 主表';

-- 2. Charge 表 charge_26（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_26` (
    `id`                  VARCHAR(64)   NOT NULL,
    `payment_intent_id`   VARCHAR(64)   NOT NULL,
    `amount`              BIGINT        NOT NULL,
    `amount_captured`     BIGINT        NOT NULL DEFAULT 0,
    `amount_refunded`     BIGINT        NOT NULL DEFAULT 0,
    `currency`            CHAR(3)       NOT NULL,
    `status`              VARCHAR(16)   NOT NULL COMMENT 'pending/succeeded/failed',
    `payment_method`      VARCHAR(32)   NOT NULL,
    `captured`            TINYINT(1)    NOT NULL DEFAULT 0,
    `paid`                TINYINT(1)    NOT NULL DEFAULT 0,
    `refunded`            TINYINT(1)    NOT NULL DEFAULT 0,
    `outcome_risk_level`  VARCHAR(16)   DEFAULT NULL,
    `outcome_risk_score`  INT           DEFAULT NULL,
    `outcome_seller_msg`  VARCHAR(256)  DEFAULT NULL,
    `outcome_type`        VARCHAR(32)   DEFAULT NULL,
    `outcome_reason`      VARCHAR(64)   DEFAULT NULL,
    `outcome_network`     VARCHAR(32)   DEFAULT NULL,
    `failure_code`        VARCHAR(64)   DEFAULT NULL,
    `failure_message`     VARCHAR(512)  DEFAULT NULL,
    `receipt_url`         VARCHAR(512)  DEFAULT NULL,
    `balance_transaction` VARCHAR(64)   DEFAULT NULL,
    `livemode`            TINYINT(1)    NOT NULL DEFAULT 0,
    `metadata`            JSON          DEFAULT NULL,
    `expired_at`          DATETIME(3)   DEFAULT NULL COMMENT '支付单过期时间：超时未完成 → 失败',
    `completed_at`        DATETIME(3)   DEFAULT NULL,
    `created`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_status`     (`status`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Charge 表（一次扣款尝试）';

-- 3. PayAction 表 pay_action_26（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_26` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `action_type`       VARCHAR(32)  NOT NULL COMMENT 'three_d_secure / otp / pay_password',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/expired',
    `payload`           JSON         DEFAULT NULL COMMENT '返回给前端的挑战材料（redirect_url / recipient_masked 等）',
    `expected_secret`   VARCHAR(256) DEFAULT NULL COMMENT 'OTP 明文或密码哈希，不返回给前端',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_attempts`      INT          NOT NULL DEFAULT 0,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `expires_at`        DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_charge`     (`charge_id`),
    KEY `idx_type`       (`action_type`),
    KEY `idx_status`     (`status`),
    KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='PayAction 表（用户挑战：3DS / OTP / PayPassword）';

-- 4. ExceptionCase 表 exception_case_26（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_26` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `case_type`         VARCHAR(32)  NOT NULL COMMENT 'late_refund_success/late_refund_failed/late_charge_success/amount_mismatch/duplicate_webhook',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'open/processing/resolved/ignored',
    `summary`           VARCHAR(512) DEFAULT NULL,
    `payload`           JSON         DEFAULT NULL,
    `resolution`        VARCHAR(512) DEFAULT NULL,
    `assigned_to`       VARCHAR(64)  DEFAULT NULL,
    `resolved_at`       DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`     (`payment_intent_id`),
    KEY `idx_refund` (`refund_id`),
    KEY `idx_type`   (`case_type`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='差错处理单';

-- 5. NotifyLog 表 notify_log_26（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_26` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `event_id`          VARCHAR(64)  NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL COMMENT 'payment_intent.succeeded / charge.refunded / ...',
    `client_type`       VARCHAR(16)  DEFAULT NULL COMMENT 'server/web/ios/android/miniapp/internal',
    `notify_channel`    VARCHAR(16)  NOT NULL COMMENT 'http/apns/fcm/websocket/mq/sms/email',
    `target`            VARCHAR(512) NOT NULL,
    `payload`           BLOB         DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/retrying/succeeded/failed/canceled',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_retries`       INT          NOT NULL DEFAULT 5,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `http_status`       INT          DEFAULT NULL,
    `error_code`        VARCHAR(64)  DEFAULT NULL,
    `error_msg`         TEXT         DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`            (`payment_intent_id`),
    KEY `idx_event`         (`event_id`),
    KEY `idx_event_type`    (`event_type`),
    KEY `idx_status`        (`status`),
    -- wave L: cover the ListDue() predicate (status IN (...) AND next_retry_at)
    -- so the cron scan hits one composite instead of two single-column indexes.
    KEY `idx_status_retry`  (`status`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通知日志';

-- 5. Refund 表 refund_26（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_26` (
    `id`                VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    -- P0 #3 幂等键：(payment_intent_id, idempotency_key) 唯一。
    -- 客户端 retry Create 同一退款请求 → DB 兜底防止双行；service 层先查 GetByIdempotencyKey 返已有。
    `idempotency_key`   VARCHAR(128) DEFAULT NULL,
    `amount`            BIGINT       NOT NULL,
    `currency`          CHAR(3)      NOT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/canceled',
    `reason`            VARCHAR(32)  DEFAULT NULL,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `receipt_number`    VARCHAR(64)  DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `auto_compensate`   TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '系统自动补偿退款：retry worker 无限重试直到成功；商户发起=0',
    `retry_count`       INT          NOT NULL DEFAULT 0,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem`       (`payment_intent_id`, `idempotency_key`),
    KEY `idx_charge`          (`charge_id`),
    KEY `idx_pi`              (`payment_intent_id`),
    KEY `idx_status`          (`status`),
    KEY `idx_auto_compensate` (`auto_compensate`),
    KEY `idx_next_retry`      (`next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Refund 表';

-- 7. InboundWebhook 表 inbound_webhook_26
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_26` (
    `id`                VARCHAR(64)  NOT NULL,
    `channel_name`      VARCHAR(32)  NOT NULL,
    `event_id`          VARCHAR(128) NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  DEFAULT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `dispute_id`        VARCHAR(64)  DEFAULT NULL,
    `signature_ok`      TINYINT(1)   NOT NULL DEFAULT 0,
    `headers`           JSON         DEFAULT NULL,
    `body`              MEDIUMBLOB   DEFAULT NULL,
    `processed_at`      DATETIME(3)  DEFAULT NULL,
    `process_status`    VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/succeeded/failed/duplicate',
    `error_msg`         TEXT         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_event`  (`channel_name`, `event_id`),
    KEY         `idx_pi`            (`payment_intent_id`),
    KEY         `idx_event_type`    (`event_type`),
    KEY         `idx_process_status` (`process_status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='入站 webhook 去重日志';

-- 8. Dispute 表 dispute_26
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_26` (
    `id`                   VARCHAR(64)  NOT NULL COMMENT 'dp_xxx',
    `payment_intent_id`    VARCHAR(64)  NOT NULL,
    `charge_id`            VARCHAR(64)  NOT NULL,
    `merchant_id`          VARCHAR(32)  NOT NULL,
    `channel`              VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/paymongo/...',
    `channel_dispute_id`   VARCHAR(128) DEFAULT NULL COMMENT '渠道侧 dispute id (用于回写)',
    `status`               VARCHAR(24)  NOT NULL COMMENT 'needs_response/under_review/won/lost/warning_closed/charge_refunded/canceled',
    `amount`               BIGINT       NOT NULL COMMENT 'disputed amount in minor units',
    `currency`             CHAR(3)      NOT NULL DEFAULT 'PHP',
    `reason`               VARCHAR(64)  NOT NULL COMMENT 'fraud/product_not_received/unrecognized/duplicate/credit_not_processed/other',
    `reason_detail`        VARCHAR(512) DEFAULT NULL,
    `evidence_due_at`      DATETIME(3)  DEFAULT NULL COMMENT '商户响应截止时间',
    `evidence`             JSON         DEFAULT NULL COMMENT '证据包：{text, photos:[url], shipping:{}, emails:[url], receipt:url, ...}',
    `decided_at`           DATETIME(3)  DEFAULT NULL,
    `outcome_amount`       BIGINT       DEFAULT NULL COMMENT '实际被扣回金额（lost 情形），<= amount',
    `auto_refund_charge`   TINYINT(1)   NOT NULL DEFAULT 1 COMMENT 'lost/charge_refunded 时是否联动退款',
    `metadata`             JSON         DEFAULT NULL,
    `created`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_dispute` (`channel`, `channel_dispute_id`),
    KEY `idx_pi`        (`payment_intent_id`),
    KEY `idx_charge`    (`charge_id`),
    KEY `idx_merchant`  (`merchant_id`),
    KEY `idx_status`    (`status`),
    KEY `idx_due`       (`status`, `evidence_due_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute / Chargeback';

-- 9. DisputeEvent 表 dispute_event_26（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_26` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `dispute_id`      VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64) NOT NULL,
    `from_status`     VARCHAR(24)  NOT NULL,
    `to_status`       VARCHAR(24)  NOT NULL,
    `source`          VARCHAR(32)  NOT NULL COMMENT 'channel_webhook / admin / system',
    `actor`           VARCHAR(64)  DEFAULT NULL,
    `note`            VARCHAR(512) DEFAULT NULL,
    `payload`         JSON         DEFAULT NULL COMMENT '触发事件 (webhook body / admin input)',
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_dispute`  (`dispute_id`),
    KEY `idx_pi`       (`payment_intent_id`),
    KEY `idx_created`  (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute 状态流转日志';

-- 10. AccountingOutbox 表 accounting_outbox_26
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_26` (
    `id`                 VARCHAR(64)  NOT NULL,
    `request_id`         VARCHAR(160) NOT NULL,
    `event_type`         VARCHAR(32)  NOT NULL COMMENT 'charge_succeeded/refund_succeeded',
    `payment_intent_id`  VARCHAR(64)  NOT NULL,
    `charge_id`          VARCHAR(64)  DEFAULT NULL,
    `refund_id`          VARCHAR(64)  DEFAULT NULL,
    `owner_type`         VARCHAR(16)  NOT NULL COMMENT 'user/merchant',
    `owner_id`           VARCHAR(64)  NOT NULL,
    `payment_method`     VARCHAR(32)  NOT NULL COMMENT 'gcash/shopeepay/maya/grabpay/...',
    `amount`             BIGINT       NOT NULL,
    `currency`           CHAR(3)      NOT NULL,
    `metadata`           JSON         DEFAULT NULL,
    `status`             VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/sent/failed',
    `attempts`           INT          NOT NULL DEFAULT 0,
    `next_attempt_at`    DATETIME(3)  DEFAULT NULL,
    `last_error`         TEXT         DEFAULT NULL,
    `sent_at`            DATETIME(3)  DEFAULT NULL,
    -- claim_token 用于多 worker / 多副本并发投递时的批次 claim：
    -- worker 用一条 UPDATE ... LIMIT N 把候选行的 next_attempt_at 推到远未来 +
    -- 写入自己生成的 token，其他 worker 的 SELECT 因 next_attempt_at 在未来天然
    -- 跳过；本 worker 再用 SELECT WHERE claim_token = ? 拉回该批次处理。
    -- 模式同 webhook_deliveries.claim_token（见 internal/webhook/delivery.go）。
    `claim_token`        VARCHAR(64)  DEFAULT NULL,
    `created`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_request`       (`request_id`),
    KEY         `idx_pi`           (`payment_intent_id`),
    KEY         `idx_status_next`  (`status`, `next_attempt_at`),
    -- claim_token 反查刚 claim 的批次；带前缀 + 纳秒时间戳 + 随机数，区分度足够
    KEY         `idx_claim`        (`claim_token`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='记账事件 outbox（投递给 accounting-system）';

-- ─── admin_audit_log_26：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_26` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id (from bearer token)',
    `actor_ip`       VARCHAR(64)  DEFAULT NULL,
    `action`         VARCHAR(64)  NOT NULL COMMENT 'merchant.approve / refund.create / risk.rule.update / ...',
    `target_type`    VARCHAR(32)  DEFAULT NULL,
    `target_id`      VARCHAR(64)  DEFAULT NULL,
    `http_method`    VARCHAR(8)   DEFAULT NULL,
    `http_path`      VARCHAR(256) DEFAULT NULL,
    `http_status`    INT          DEFAULT NULL,
    `request_body`   MEDIUMTEXT   DEFAULT NULL COMMENT 'JSON, sensitive fields redacted by caller',
    `response_code`  VARCHAR(32)  DEFAULT NULL,
    `response_msg`   VARCHAR(512) DEFAULT NULL,
    `duration_ms`    INT          DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_action`  (`action`),
    KEY `idx_target`  (`target_type`, `target_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 26';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：2 = 0-9 物理库；27 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_27
CREATE TABLE IF NOT EXISTS `payment_intent_27` (
    `id`                         VARCHAR(64)   NOT NULL,
    `amount`                     BIGINT        NOT NULL COMMENT '应付金额（= subtotal - coupon - points）',
    `amount_subtotal`            BIGINT        NOT NULL DEFAULT 0 COMMENT '原价',
    `amount_coupon`              BIGINT        NOT NULL DEFAULT 0 COMMENT '优惠券抵扣',
    `amount_points`              BIGINT        NOT NULL DEFAULT 0 COMMENT '积分抵扣',
    `currency`                   CHAR(3)       NOT NULL,
    `status`                     VARCHAR(32)   NOT NULL COMMENT '支付生命周期: created/requires_action/processing/succeeded/failed/canceled',
    `refund_phase`               VARCHAR(32)   DEFAULT NULL COMMENT '退款维度状态（独立字段）: refunding/partially_refunded/fully_refunded/refund_failed/refund_canceled',
    `customer_id`                VARCHAR(64)   DEFAULT NULL,
    `description`                VARCHAR(512)  DEFAULT NULL,
    `mch_id`                     VARCHAR(32)   NOT NULL,
    `mch_order_no`               VARCHAR(64)   DEFAULT NULL,
    `business_id`                VARCHAR(64)   DEFAULT NULL COMMENT '分片路由键；pi_id 前缀由此派生（fallback mch_id）',
    `idempotency_key`            VARCHAR(128)  DEFAULT NULL COMMENT '(mch_id,idempotency_key) 唯一；防重',
    `previous_payment_intent_id` VARCHAR(64)   DEFAULT NULL COMMENT '换单重下时指向上一次失败的 PI',
    `capture_method`             VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `confirmation_method`        VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `client_secret`              VARCHAR(128)  DEFAULT NULL,
    `payment_method_types`       JSON          DEFAULT NULL,
    `payment_method`             VARCHAR(32)   DEFAULT NULL,
    `user_id`                    BIGINT        DEFAULT NULL COMMENT '付款用户（可空，匿名 PI 不绑用户）',
    `user_card_id`               BIGINT        DEFAULT NULL COMMENT '卡支付时关联 user_merchant.user_card.id；其它支付方式为空',
    `active_charge_ids`          JSON          DEFAULT NULL COMMENT '进行中的 charge_id 列表（终态后移除）',
    `active_refund_ids`          JSON          DEFAULT NULL COMMENT '进行中的 refund_id 列表（终态后移除）',
    `amount_capturable`          BIGINT        NOT NULL DEFAULT 0,
    `amount_received`            BIGINT        NOT NULL DEFAULT 0,
    `return_url`                 VARCHAR(512)  DEFAULT NULL,
    `notify_url`                 VARCHAR(512)  DEFAULT NULL,
    `statement_descriptor`       VARCHAR(32)   DEFAULT NULL,
    `metadata`                   JSON          DEFAULT NULL,
    `livemode`                   TINYINT(1)    NOT NULL DEFAULT 0,
    `created`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `expired_at`                 DATETIME(3)   DEFAULT NULL,
    `canceled_at`                DATETIME(3)   DEFAULT NULL,
    `cancellation_reason`        VARCHAR(32)   DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_status`     (`status`),
    KEY `idx_mch`        (`mch_id`),
    KEY `idx_customer`   (`customer_id`),
    KEY `idx_business`   (`business_id`),
    UNIQUE KEY `uk_idem` (`mch_id`, `idempotency_key`),
    KEY `idx_prev`       (`previous_payment_intent_id`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Payment Intent 主表';

-- 2. Charge 表 charge_27（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_27` (
    `id`                  VARCHAR(64)   NOT NULL,
    `payment_intent_id`   VARCHAR(64)   NOT NULL,
    `amount`              BIGINT        NOT NULL,
    `amount_captured`     BIGINT        NOT NULL DEFAULT 0,
    `amount_refunded`     BIGINT        NOT NULL DEFAULT 0,
    `currency`            CHAR(3)       NOT NULL,
    `status`              VARCHAR(16)   NOT NULL COMMENT 'pending/succeeded/failed',
    `payment_method`      VARCHAR(32)   NOT NULL,
    `captured`            TINYINT(1)    NOT NULL DEFAULT 0,
    `paid`                TINYINT(1)    NOT NULL DEFAULT 0,
    `refunded`            TINYINT(1)    NOT NULL DEFAULT 0,
    `outcome_risk_level`  VARCHAR(16)   DEFAULT NULL,
    `outcome_risk_score`  INT           DEFAULT NULL,
    `outcome_seller_msg`  VARCHAR(256)  DEFAULT NULL,
    `outcome_type`        VARCHAR(32)   DEFAULT NULL,
    `outcome_reason`      VARCHAR(64)   DEFAULT NULL,
    `outcome_network`     VARCHAR(32)   DEFAULT NULL,
    `failure_code`        VARCHAR(64)   DEFAULT NULL,
    `failure_message`     VARCHAR(512)  DEFAULT NULL,
    `receipt_url`         VARCHAR(512)  DEFAULT NULL,
    `balance_transaction` VARCHAR(64)   DEFAULT NULL,
    `livemode`            TINYINT(1)    NOT NULL DEFAULT 0,
    `metadata`            JSON          DEFAULT NULL,
    `expired_at`          DATETIME(3)   DEFAULT NULL COMMENT '支付单过期时间：超时未完成 → 失败',
    `completed_at`        DATETIME(3)   DEFAULT NULL,
    `created`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_status`     (`status`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Charge 表（一次扣款尝试）';

-- 3. PayAction 表 pay_action_27（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_27` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `action_type`       VARCHAR(32)  NOT NULL COMMENT 'three_d_secure / otp / pay_password',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/expired',
    `payload`           JSON         DEFAULT NULL COMMENT '返回给前端的挑战材料（redirect_url / recipient_masked 等）',
    `expected_secret`   VARCHAR(256) DEFAULT NULL COMMENT 'OTP 明文或密码哈希，不返回给前端',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_attempts`      INT          NOT NULL DEFAULT 0,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `expires_at`        DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_charge`     (`charge_id`),
    KEY `idx_type`       (`action_type`),
    KEY `idx_status`     (`status`),
    KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='PayAction 表（用户挑战：3DS / OTP / PayPassword）';

-- 4. ExceptionCase 表 exception_case_27（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_27` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `case_type`         VARCHAR(32)  NOT NULL COMMENT 'late_refund_success/late_refund_failed/late_charge_success/amount_mismatch/duplicate_webhook',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'open/processing/resolved/ignored',
    `summary`           VARCHAR(512) DEFAULT NULL,
    `payload`           JSON         DEFAULT NULL,
    `resolution`        VARCHAR(512) DEFAULT NULL,
    `assigned_to`       VARCHAR(64)  DEFAULT NULL,
    `resolved_at`       DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`     (`payment_intent_id`),
    KEY `idx_refund` (`refund_id`),
    KEY `idx_type`   (`case_type`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='差错处理单';

-- 5. NotifyLog 表 notify_log_27（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_27` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `event_id`          VARCHAR(64)  NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL COMMENT 'payment_intent.succeeded / charge.refunded / ...',
    `client_type`       VARCHAR(16)  DEFAULT NULL COMMENT 'server/web/ios/android/miniapp/internal',
    `notify_channel`    VARCHAR(16)  NOT NULL COMMENT 'http/apns/fcm/websocket/mq/sms/email',
    `target`            VARCHAR(512) NOT NULL,
    `payload`           BLOB         DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/retrying/succeeded/failed/canceled',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_retries`       INT          NOT NULL DEFAULT 5,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `http_status`       INT          DEFAULT NULL,
    `error_code`        VARCHAR(64)  DEFAULT NULL,
    `error_msg`         TEXT         DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`            (`payment_intent_id`),
    KEY `idx_event`         (`event_id`),
    KEY `idx_event_type`    (`event_type`),
    KEY `idx_status`        (`status`),
    -- wave L: cover the ListDue() predicate (status IN (...) AND next_retry_at)
    -- so the cron scan hits one composite instead of two single-column indexes.
    KEY `idx_status_retry`  (`status`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通知日志';

-- 5. Refund 表 refund_27（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_27` (
    `id`                VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    -- P0 #3 幂等键：(payment_intent_id, idempotency_key) 唯一。
    -- 客户端 retry Create 同一退款请求 → DB 兜底防止双行；service 层先查 GetByIdempotencyKey 返已有。
    `idempotency_key`   VARCHAR(128) DEFAULT NULL,
    `amount`            BIGINT       NOT NULL,
    `currency`          CHAR(3)      NOT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/canceled',
    `reason`            VARCHAR(32)  DEFAULT NULL,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `receipt_number`    VARCHAR(64)  DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `auto_compensate`   TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '系统自动补偿退款：retry worker 无限重试直到成功；商户发起=0',
    `retry_count`       INT          NOT NULL DEFAULT 0,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem`       (`payment_intent_id`, `idempotency_key`),
    KEY `idx_charge`          (`charge_id`),
    KEY `idx_pi`              (`payment_intent_id`),
    KEY `idx_status`          (`status`),
    KEY `idx_auto_compensate` (`auto_compensate`),
    KEY `idx_next_retry`      (`next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Refund 表';

-- 7. InboundWebhook 表 inbound_webhook_27
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_27` (
    `id`                VARCHAR(64)  NOT NULL,
    `channel_name`      VARCHAR(32)  NOT NULL,
    `event_id`          VARCHAR(128) NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  DEFAULT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `dispute_id`        VARCHAR(64)  DEFAULT NULL,
    `signature_ok`      TINYINT(1)   NOT NULL DEFAULT 0,
    `headers`           JSON         DEFAULT NULL,
    `body`              MEDIUMBLOB   DEFAULT NULL,
    `processed_at`      DATETIME(3)  DEFAULT NULL,
    `process_status`    VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/succeeded/failed/duplicate',
    `error_msg`         TEXT         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_event`  (`channel_name`, `event_id`),
    KEY         `idx_pi`            (`payment_intent_id`),
    KEY         `idx_event_type`    (`event_type`),
    KEY         `idx_process_status` (`process_status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='入站 webhook 去重日志';

-- 8. Dispute 表 dispute_27
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_27` (
    `id`                   VARCHAR(64)  NOT NULL COMMENT 'dp_xxx',
    `payment_intent_id`    VARCHAR(64)  NOT NULL,
    `charge_id`            VARCHAR(64)  NOT NULL,
    `merchant_id`          VARCHAR(32)  NOT NULL,
    `channel`              VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/paymongo/...',
    `channel_dispute_id`   VARCHAR(128) DEFAULT NULL COMMENT '渠道侧 dispute id (用于回写)',
    `status`               VARCHAR(24)  NOT NULL COMMENT 'needs_response/under_review/won/lost/warning_closed/charge_refunded/canceled',
    `amount`               BIGINT       NOT NULL COMMENT 'disputed amount in minor units',
    `currency`             CHAR(3)      NOT NULL DEFAULT 'PHP',
    `reason`               VARCHAR(64)  NOT NULL COMMENT 'fraud/product_not_received/unrecognized/duplicate/credit_not_processed/other',
    `reason_detail`        VARCHAR(512) DEFAULT NULL,
    `evidence_due_at`      DATETIME(3)  DEFAULT NULL COMMENT '商户响应截止时间',
    `evidence`             JSON         DEFAULT NULL COMMENT '证据包：{text, photos:[url], shipping:{}, emails:[url], receipt:url, ...}',
    `decided_at`           DATETIME(3)  DEFAULT NULL,
    `outcome_amount`       BIGINT       DEFAULT NULL COMMENT '实际被扣回金额（lost 情形），<= amount',
    `auto_refund_charge`   TINYINT(1)   NOT NULL DEFAULT 1 COMMENT 'lost/charge_refunded 时是否联动退款',
    `metadata`             JSON         DEFAULT NULL,
    `created`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_dispute` (`channel`, `channel_dispute_id`),
    KEY `idx_pi`        (`payment_intent_id`),
    KEY `idx_charge`    (`charge_id`),
    KEY `idx_merchant`  (`merchant_id`),
    KEY `idx_status`    (`status`),
    KEY `idx_due`       (`status`, `evidence_due_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute / Chargeback';

-- 9. DisputeEvent 表 dispute_event_27（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_27` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `dispute_id`      VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64) NOT NULL,
    `from_status`     VARCHAR(24)  NOT NULL,
    `to_status`       VARCHAR(24)  NOT NULL,
    `source`          VARCHAR(32)  NOT NULL COMMENT 'channel_webhook / admin / system',
    `actor`           VARCHAR(64)  DEFAULT NULL,
    `note`            VARCHAR(512) DEFAULT NULL,
    `payload`         JSON         DEFAULT NULL COMMENT '触发事件 (webhook body / admin input)',
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_dispute`  (`dispute_id`),
    KEY `idx_pi`       (`payment_intent_id`),
    KEY `idx_created`  (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute 状态流转日志';

-- 10. AccountingOutbox 表 accounting_outbox_27
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_27` (
    `id`                 VARCHAR(64)  NOT NULL,
    `request_id`         VARCHAR(160) NOT NULL,
    `event_type`         VARCHAR(32)  NOT NULL COMMENT 'charge_succeeded/refund_succeeded',
    `payment_intent_id`  VARCHAR(64)  NOT NULL,
    `charge_id`          VARCHAR(64)  DEFAULT NULL,
    `refund_id`          VARCHAR(64)  DEFAULT NULL,
    `owner_type`         VARCHAR(16)  NOT NULL COMMENT 'user/merchant',
    `owner_id`           VARCHAR(64)  NOT NULL,
    `payment_method`     VARCHAR(32)  NOT NULL COMMENT 'gcash/shopeepay/maya/grabpay/...',
    `amount`             BIGINT       NOT NULL,
    `currency`           CHAR(3)      NOT NULL,
    `metadata`           JSON         DEFAULT NULL,
    `status`             VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/sent/failed',
    `attempts`           INT          NOT NULL DEFAULT 0,
    `next_attempt_at`    DATETIME(3)  DEFAULT NULL,
    `last_error`         TEXT         DEFAULT NULL,
    `sent_at`            DATETIME(3)  DEFAULT NULL,
    -- claim_token 用于多 worker / 多副本并发投递时的批次 claim：
    -- worker 用一条 UPDATE ... LIMIT N 把候选行的 next_attempt_at 推到远未来 +
    -- 写入自己生成的 token，其他 worker 的 SELECT 因 next_attempt_at 在未来天然
    -- 跳过；本 worker 再用 SELECT WHERE claim_token = ? 拉回该批次处理。
    -- 模式同 webhook_deliveries.claim_token（见 internal/webhook/delivery.go）。
    `claim_token`        VARCHAR(64)  DEFAULT NULL,
    `created`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_request`       (`request_id`),
    KEY         `idx_pi`           (`payment_intent_id`),
    KEY         `idx_status_next`  (`status`, `next_attempt_at`),
    -- claim_token 反查刚 claim 的批次；带前缀 + 纳秒时间戳 + 随机数，区分度足够
    KEY         `idx_claim`        (`claim_token`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='记账事件 outbox（投递给 accounting-system）';

-- ─── admin_audit_log_27：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_27` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id (from bearer token)',
    `actor_ip`       VARCHAR(64)  DEFAULT NULL,
    `action`         VARCHAR(64)  NOT NULL COMMENT 'merchant.approve / refund.create / risk.rule.update / ...',
    `target_type`    VARCHAR(32)  DEFAULT NULL,
    `target_id`      VARCHAR(64)  DEFAULT NULL,
    `http_method`    VARCHAR(8)   DEFAULT NULL,
    `http_path`      VARCHAR(256) DEFAULT NULL,
    `http_status`    INT          DEFAULT NULL,
    `request_body`   MEDIUMTEXT   DEFAULT NULL COMMENT 'JSON, sensitive fields redacted by caller',
    `response_code`  VARCHAR(32)  DEFAULT NULL,
    `response_msg`   VARCHAR(512) DEFAULT NULL,
    `duration_ms`    INT          DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_action`  (`action`),
    KEY `idx_target`  (`target_type`, `target_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 27';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：2 = 0-9 物理库；28 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_28
CREATE TABLE IF NOT EXISTS `payment_intent_28` (
    `id`                         VARCHAR(64)   NOT NULL,
    `amount`                     BIGINT        NOT NULL COMMENT '应付金额（= subtotal - coupon - points）',
    `amount_subtotal`            BIGINT        NOT NULL DEFAULT 0 COMMENT '原价',
    `amount_coupon`              BIGINT        NOT NULL DEFAULT 0 COMMENT '优惠券抵扣',
    `amount_points`              BIGINT        NOT NULL DEFAULT 0 COMMENT '积分抵扣',
    `currency`                   CHAR(3)       NOT NULL,
    `status`                     VARCHAR(32)   NOT NULL COMMENT '支付生命周期: created/requires_action/processing/succeeded/failed/canceled',
    `refund_phase`               VARCHAR(32)   DEFAULT NULL COMMENT '退款维度状态（独立字段）: refunding/partially_refunded/fully_refunded/refund_failed/refund_canceled',
    `customer_id`                VARCHAR(64)   DEFAULT NULL,
    `description`                VARCHAR(512)  DEFAULT NULL,
    `mch_id`                     VARCHAR(32)   NOT NULL,
    `mch_order_no`               VARCHAR(64)   DEFAULT NULL,
    `business_id`                VARCHAR(64)   DEFAULT NULL COMMENT '分片路由键；pi_id 前缀由此派生（fallback mch_id）',
    `idempotency_key`            VARCHAR(128)  DEFAULT NULL COMMENT '(mch_id,idempotency_key) 唯一；防重',
    `previous_payment_intent_id` VARCHAR(64)   DEFAULT NULL COMMENT '换单重下时指向上一次失败的 PI',
    `capture_method`             VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `confirmation_method`        VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `client_secret`              VARCHAR(128)  DEFAULT NULL,
    `payment_method_types`       JSON          DEFAULT NULL,
    `payment_method`             VARCHAR(32)   DEFAULT NULL,
    `user_id`                    BIGINT        DEFAULT NULL COMMENT '付款用户（可空，匿名 PI 不绑用户）',
    `user_card_id`               BIGINT        DEFAULT NULL COMMENT '卡支付时关联 user_merchant.user_card.id；其它支付方式为空',
    `active_charge_ids`          JSON          DEFAULT NULL COMMENT '进行中的 charge_id 列表（终态后移除）',
    `active_refund_ids`          JSON          DEFAULT NULL COMMENT '进行中的 refund_id 列表（终态后移除）',
    `amount_capturable`          BIGINT        NOT NULL DEFAULT 0,
    `amount_received`            BIGINT        NOT NULL DEFAULT 0,
    `return_url`                 VARCHAR(512)  DEFAULT NULL,
    `notify_url`                 VARCHAR(512)  DEFAULT NULL,
    `statement_descriptor`       VARCHAR(32)   DEFAULT NULL,
    `metadata`                   JSON          DEFAULT NULL,
    `livemode`                   TINYINT(1)    NOT NULL DEFAULT 0,
    `created`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `expired_at`                 DATETIME(3)   DEFAULT NULL,
    `canceled_at`                DATETIME(3)   DEFAULT NULL,
    `cancellation_reason`        VARCHAR(32)   DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_status`     (`status`),
    KEY `idx_mch`        (`mch_id`),
    KEY `idx_customer`   (`customer_id`),
    KEY `idx_business`   (`business_id`),
    UNIQUE KEY `uk_idem` (`mch_id`, `idempotency_key`),
    KEY `idx_prev`       (`previous_payment_intent_id`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Payment Intent 主表';

-- 2. Charge 表 charge_28（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_28` (
    `id`                  VARCHAR(64)   NOT NULL,
    `payment_intent_id`   VARCHAR(64)   NOT NULL,
    `amount`              BIGINT        NOT NULL,
    `amount_captured`     BIGINT        NOT NULL DEFAULT 0,
    `amount_refunded`     BIGINT        NOT NULL DEFAULT 0,
    `currency`            CHAR(3)       NOT NULL,
    `status`              VARCHAR(16)   NOT NULL COMMENT 'pending/succeeded/failed',
    `payment_method`      VARCHAR(32)   NOT NULL,
    `captured`            TINYINT(1)    NOT NULL DEFAULT 0,
    `paid`                TINYINT(1)    NOT NULL DEFAULT 0,
    `refunded`            TINYINT(1)    NOT NULL DEFAULT 0,
    `outcome_risk_level`  VARCHAR(16)   DEFAULT NULL,
    `outcome_risk_score`  INT           DEFAULT NULL,
    `outcome_seller_msg`  VARCHAR(256)  DEFAULT NULL,
    `outcome_type`        VARCHAR(32)   DEFAULT NULL,
    `outcome_reason`      VARCHAR(64)   DEFAULT NULL,
    `outcome_network`     VARCHAR(32)   DEFAULT NULL,
    `failure_code`        VARCHAR(64)   DEFAULT NULL,
    `failure_message`     VARCHAR(512)  DEFAULT NULL,
    `receipt_url`         VARCHAR(512)  DEFAULT NULL,
    `balance_transaction` VARCHAR(64)   DEFAULT NULL,
    `livemode`            TINYINT(1)    NOT NULL DEFAULT 0,
    `metadata`            JSON          DEFAULT NULL,
    `expired_at`          DATETIME(3)   DEFAULT NULL COMMENT '支付单过期时间：超时未完成 → 失败',
    `completed_at`        DATETIME(3)   DEFAULT NULL,
    `created`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_status`     (`status`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Charge 表（一次扣款尝试）';

-- 3. PayAction 表 pay_action_28（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_28` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `action_type`       VARCHAR(32)  NOT NULL COMMENT 'three_d_secure / otp / pay_password',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/expired',
    `payload`           JSON         DEFAULT NULL COMMENT '返回给前端的挑战材料（redirect_url / recipient_masked 等）',
    `expected_secret`   VARCHAR(256) DEFAULT NULL COMMENT 'OTP 明文或密码哈希，不返回给前端',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_attempts`      INT          NOT NULL DEFAULT 0,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `expires_at`        DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_charge`     (`charge_id`),
    KEY `idx_type`       (`action_type`),
    KEY `idx_status`     (`status`),
    KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='PayAction 表（用户挑战：3DS / OTP / PayPassword）';

-- 4. ExceptionCase 表 exception_case_28（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_28` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `case_type`         VARCHAR(32)  NOT NULL COMMENT 'late_refund_success/late_refund_failed/late_charge_success/amount_mismatch/duplicate_webhook',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'open/processing/resolved/ignored',
    `summary`           VARCHAR(512) DEFAULT NULL,
    `payload`           JSON         DEFAULT NULL,
    `resolution`        VARCHAR(512) DEFAULT NULL,
    `assigned_to`       VARCHAR(64)  DEFAULT NULL,
    `resolved_at`       DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`     (`payment_intent_id`),
    KEY `idx_refund` (`refund_id`),
    KEY `idx_type`   (`case_type`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='差错处理单';

-- 5. NotifyLog 表 notify_log_28（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_28` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `event_id`          VARCHAR(64)  NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL COMMENT 'payment_intent.succeeded / charge.refunded / ...',
    `client_type`       VARCHAR(16)  DEFAULT NULL COMMENT 'server/web/ios/android/miniapp/internal',
    `notify_channel`    VARCHAR(16)  NOT NULL COMMENT 'http/apns/fcm/websocket/mq/sms/email',
    `target`            VARCHAR(512) NOT NULL,
    `payload`           BLOB         DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/retrying/succeeded/failed/canceled',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_retries`       INT          NOT NULL DEFAULT 5,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `http_status`       INT          DEFAULT NULL,
    `error_code`        VARCHAR(64)  DEFAULT NULL,
    `error_msg`         TEXT         DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`            (`payment_intent_id`),
    KEY `idx_event`         (`event_id`),
    KEY `idx_event_type`    (`event_type`),
    KEY `idx_status`        (`status`),
    -- wave L: cover the ListDue() predicate (status IN (...) AND next_retry_at)
    -- so the cron scan hits one composite instead of two single-column indexes.
    KEY `idx_status_retry`  (`status`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通知日志';

-- 5. Refund 表 refund_28（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_28` (
    `id`                VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    -- P0 #3 幂等键：(payment_intent_id, idempotency_key) 唯一。
    -- 客户端 retry Create 同一退款请求 → DB 兜底防止双行；service 层先查 GetByIdempotencyKey 返已有。
    `idempotency_key`   VARCHAR(128) DEFAULT NULL,
    `amount`            BIGINT       NOT NULL,
    `currency`          CHAR(3)      NOT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/canceled',
    `reason`            VARCHAR(32)  DEFAULT NULL,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `receipt_number`    VARCHAR(64)  DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `auto_compensate`   TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '系统自动补偿退款：retry worker 无限重试直到成功；商户发起=0',
    `retry_count`       INT          NOT NULL DEFAULT 0,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem`       (`payment_intent_id`, `idempotency_key`),
    KEY `idx_charge`          (`charge_id`),
    KEY `idx_pi`              (`payment_intent_id`),
    KEY `idx_status`          (`status`),
    KEY `idx_auto_compensate` (`auto_compensate`),
    KEY `idx_next_retry`      (`next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Refund 表';

-- 7. InboundWebhook 表 inbound_webhook_28
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_28` (
    `id`                VARCHAR(64)  NOT NULL,
    `channel_name`      VARCHAR(32)  NOT NULL,
    `event_id`          VARCHAR(128) NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  DEFAULT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `dispute_id`        VARCHAR(64)  DEFAULT NULL,
    `signature_ok`      TINYINT(1)   NOT NULL DEFAULT 0,
    `headers`           JSON         DEFAULT NULL,
    `body`              MEDIUMBLOB   DEFAULT NULL,
    `processed_at`      DATETIME(3)  DEFAULT NULL,
    `process_status`    VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/succeeded/failed/duplicate',
    `error_msg`         TEXT         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_event`  (`channel_name`, `event_id`),
    KEY         `idx_pi`            (`payment_intent_id`),
    KEY         `idx_event_type`    (`event_type`),
    KEY         `idx_process_status` (`process_status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='入站 webhook 去重日志';

-- 8. Dispute 表 dispute_28
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_28` (
    `id`                   VARCHAR(64)  NOT NULL COMMENT 'dp_xxx',
    `payment_intent_id`    VARCHAR(64)  NOT NULL,
    `charge_id`            VARCHAR(64)  NOT NULL,
    `merchant_id`          VARCHAR(32)  NOT NULL,
    `channel`              VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/paymongo/...',
    `channel_dispute_id`   VARCHAR(128) DEFAULT NULL COMMENT '渠道侧 dispute id (用于回写)',
    `status`               VARCHAR(24)  NOT NULL COMMENT 'needs_response/under_review/won/lost/warning_closed/charge_refunded/canceled',
    `amount`               BIGINT       NOT NULL COMMENT 'disputed amount in minor units',
    `currency`             CHAR(3)      NOT NULL DEFAULT 'PHP',
    `reason`               VARCHAR(64)  NOT NULL COMMENT 'fraud/product_not_received/unrecognized/duplicate/credit_not_processed/other',
    `reason_detail`        VARCHAR(512) DEFAULT NULL,
    `evidence_due_at`      DATETIME(3)  DEFAULT NULL COMMENT '商户响应截止时间',
    `evidence`             JSON         DEFAULT NULL COMMENT '证据包：{text, photos:[url], shipping:{}, emails:[url], receipt:url, ...}',
    `decided_at`           DATETIME(3)  DEFAULT NULL,
    `outcome_amount`       BIGINT       DEFAULT NULL COMMENT '实际被扣回金额（lost 情形），<= amount',
    `auto_refund_charge`   TINYINT(1)   NOT NULL DEFAULT 1 COMMENT 'lost/charge_refunded 时是否联动退款',
    `metadata`             JSON         DEFAULT NULL,
    `created`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_dispute` (`channel`, `channel_dispute_id`),
    KEY `idx_pi`        (`payment_intent_id`),
    KEY `idx_charge`    (`charge_id`),
    KEY `idx_merchant`  (`merchant_id`),
    KEY `idx_status`    (`status`),
    KEY `idx_due`       (`status`, `evidence_due_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute / Chargeback';

-- 9. DisputeEvent 表 dispute_event_28（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_28` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `dispute_id`      VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64) NOT NULL,
    `from_status`     VARCHAR(24)  NOT NULL,
    `to_status`       VARCHAR(24)  NOT NULL,
    `source`          VARCHAR(32)  NOT NULL COMMENT 'channel_webhook / admin / system',
    `actor`           VARCHAR(64)  DEFAULT NULL,
    `note`            VARCHAR(512) DEFAULT NULL,
    `payload`         JSON         DEFAULT NULL COMMENT '触发事件 (webhook body / admin input)',
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_dispute`  (`dispute_id`),
    KEY `idx_pi`       (`payment_intent_id`),
    KEY `idx_created`  (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute 状态流转日志';

-- 10. AccountingOutbox 表 accounting_outbox_28
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_28` (
    `id`                 VARCHAR(64)  NOT NULL,
    `request_id`         VARCHAR(160) NOT NULL,
    `event_type`         VARCHAR(32)  NOT NULL COMMENT 'charge_succeeded/refund_succeeded',
    `payment_intent_id`  VARCHAR(64)  NOT NULL,
    `charge_id`          VARCHAR(64)  DEFAULT NULL,
    `refund_id`          VARCHAR(64)  DEFAULT NULL,
    `owner_type`         VARCHAR(16)  NOT NULL COMMENT 'user/merchant',
    `owner_id`           VARCHAR(64)  NOT NULL,
    `payment_method`     VARCHAR(32)  NOT NULL COMMENT 'gcash/shopeepay/maya/grabpay/...',
    `amount`             BIGINT       NOT NULL,
    `currency`           CHAR(3)      NOT NULL,
    `metadata`           JSON         DEFAULT NULL,
    `status`             VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/sent/failed',
    `attempts`           INT          NOT NULL DEFAULT 0,
    `next_attempt_at`    DATETIME(3)  DEFAULT NULL,
    `last_error`         TEXT         DEFAULT NULL,
    `sent_at`            DATETIME(3)  DEFAULT NULL,
    -- claim_token 用于多 worker / 多副本并发投递时的批次 claim：
    -- worker 用一条 UPDATE ... LIMIT N 把候选行的 next_attempt_at 推到远未来 +
    -- 写入自己生成的 token，其他 worker 的 SELECT 因 next_attempt_at 在未来天然
    -- 跳过；本 worker 再用 SELECT WHERE claim_token = ? 拉回该批次处理。
    -- 模式同 webhook_deliveries.claim_token（见 internal/webhook/delivery.go）。
    `claim_token`        VARCHAR(64)  DEFAULT NULL,
    `created`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_request`       (`request_id`),
    KEY         `idx_pi`           (`payment_intent_id`),
    KEY         `idx_status_next`  (`status`, `next_attempt_at`),
    -- claim_token 反查刚 claim 的批次；带前缀 + 纳秒时间戳 + 随机数，区分度足够
    KEY         `idx_claim`        (`claim_token`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='记账事件 outbox（投递给 accounting-system）';

-- ─── admin_audit_log_28：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_28` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id (from bearer token)',
    `actor_ip`       VARCHAR(64)  DEFAULT NULL,
    `action`         VARCHAR(64)  NOT NULL COMMENT 'merchant.approve / refund.create / risk.rule.update / ...',
    `target_type`    VARCHAR(32)  DEFAULT NULL,
    `target_id`      VARCHAR(64)  DEFAULT NULL,
    `http_method`    VARCHAR(8)   DEFAULT NULL,
    `http_path`      VARCHAR(256) DEFAULT NULL,
    `http_status`    INT          DEFAULT NULL,
    `request_body`   MEDIUMTEXT   DEFAULT NULL COMMENT 'JSON, sensitive fields redacted by caller',
    `response_code`  VARCHAR(32)  DEFAULT NULL,
    `response_msg`   VARCHAR(512) DEFAULT NULL,
    `duration_ms`    INT          DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_action`  (`action`),
    KEY `idx_target`  (`target_type`, `target_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 28';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：2 = 0-9 物理库；29 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_29
CREATE TABLE IF NOT EXISTS `payment_intent_29` (
    `id`                         VARCHAR(64)   NOT NULL,
    `amount`                     BIGINT        NOT NULL COMMENT '应付金额（= subtotal - coupon - points）',
    `amount_subtotal`            BIGINT        NOT NULL DEFAULT 0 COMMENT '原价',
    `amount_coupon`              BIGINT        NOT NULL DEFAULT 0 COMMENT '优惠券抵扣',
    `amount_points`              BIGINT        NOT NULL DEFAULT 0 COMMENT '积分抵扣',
    `currency`                   CHAR(3)       NOT NULL,
    `status`                     VARCHAR(32)   NOT NULL COMMENT '支付生命周期: created/requires_action/processing/succeeded/failed/canceled',
    `refund_phase`               VARCHAR(32)   DEFAULT NULL COMMENT '退款维度状态（独立字段）: refunding/partially_refunded/fully_refunded/refund_failed/refund_canceled',
    `customer_id`                VARCHAR(64)   DEFAULT NULL,
    `description`                VARCHAR(512)  DEFAULT NULL,
    `mch_id`                     VARCHAR(32)   NOT NULL,
    `mch_order_no`               VARCHAR(64)   DEFAULT NULL,
    `business_id`                VARCHAR(64)   DEFAULT NULL COMMENT '分片路由键；pi_id 前缀由此派生（fallback mch_id）',
    `idempotency_key`            VARCHAR(128)  DEFAULT NULL COMMENT '(mch_id,idempotency_key) 唯一；防重',
    `previous_payment_intent_id` VARCHAR(64)   DEFAULT NULL COMMENT '换单重下时指向上一次失败的 PI',
    `capture_method`             VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `confirmation_method`        VARCHAR(16)   NOT NULL DEFAULT 'automatic',
    `client_secret`              VARCHAR(128)  DEFAULT NULL,
    `payment_method_types`       JSON          DEFAULT NULL,
    `payment_method`             VARCHAR(32)   DEFAULT NULL,
    `user_id`                    BIGINT        DEFAULT NULL COMMENT '付款用户（可空，匿名 PI 不绑用户）',
    `user_card_id`               BIGINT        DEFAULT NULL COMMENT '卡支付时关联 user_merchant.user_card.id；其它支付方式为空',
    `active_charge_ids`          JSON          DEFAULT NULL COMMENT '进行中的 charge_id 列表（终态后移除）',
    `active_refund_ids`          JSON          DEFAULT NULL COMMENT '进行中的 refund_id 列表（终态后移除）',
    `amount_capturable`          BIGINT        NOT NULL DEFAULT 0,
    `amount_received`            BIGINT        NOT NULL DEFAULT 0,
    `return_url`                 VARCHAR(512)  DEFAULT NULL,
    `notify_url`                 VARCHAR(512)  DEFAULT NULL,
    `statement_descriptor`       VARCHAR(32)   DEFAULT NULL,
    `metadata`                   JSON          DEFAULT NULL,
    `livemode`                   TINYINT(1)    NOT NULL DEFAULT 0,
    `created`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`                    DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `expired_at`                 DATETIME(3)   DEFAULT NULL,
    `canceled_at`                DATETIME(3)   DEFAULT NULL,
    `cancellation_reason`        VARCHAR(32)   DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_status`     (`status`),
    KEY `idx_mch`        (`mch_id`),
    KEY `idx_customer`   (`customer_id`),
    KEY `idx_business`   (`business_id`),
    UNIQUE KEY `uk_idem` (`mch_id`, `idempotency_key`),
    KEY `idx_prev`       (`previous_payment_intent_id`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Payment Intent 主表';

-- 2. Charge 表 charge_29（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_29` (
    `id`                  VARCHAR(64)   NOT NULL,
    `payment_intent_id`   VARCHAR(64)   NOT NULL,
    `amount`              BIGINT        NOT NULL,
    `amount_captured`     BIGINT        NOT NULL DEFAULT 0,
    `amount_refunded`     BIGINT        NOT NULL DEFAULT 0,
    `currency`            CHAR(3)       NOT NULL,
    `status`              VARCHAR(16)   NOT NULL COMMENT 'pending/succeeded/failed',
    `payment_method`      VARCHAR(32)   NOT NULL,
    `captured`            TINYINT(1)    NOT NULL DEFAULT 0,
    `paid`                TINYINT(1)    NOT NULL DEFAULT 0,
    `refunded`            TINYINT(1)    NOT NULL DEFAULT 0,
    `outcome_risk_level`  VARCHAR(16)   DEFAULT NULL,
    `outcome_risk_score`  INT           DEFAULT NULL,
    `outcome_seller_msg`  VARCHAR(256)  DEFAULT NULL,
    `outcome_type`        VARCHAR(32)   DEFAULT NULL,
    `outcome_reason`      VARCHAR(64)   DEFAULT NULL,
    `outcome_network`     VARCHAR(32)   DEFAULT NULL,
    `failure_code`        VARCHAR(64)   DEFAULT NULL,
    `failure_message`     VARCHAR(512)  DEFAULT NULL,
    `receipt_url`         VARCHAR(512)  DEFAULT NULL,
    `balance_transaction` VARCHAR(64)   DEFAULT NULL,
    `livemode`            TINYINT(1)    NOT NULL DEFAULT 0,
    `metadata`            JSON          DEFAULT NULL,
    `expired_at`          DATETIME(3)   DEFAULT NULL COMMENT '支付单过期时间：超时未完成 → 失败',
    `completed_at`        DATETIME(3)   DEFAULT NULL,
    `created`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`             DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_status`     (`status`),
    KEY `idx_expired_at` (`expired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Charge 表（一次扣款尝试）';

-- 3. PayAction 表 pay_action_29（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_29` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `action_type`       VARCHAR(32)  NOT NULL COMMENT 'three_d_secure / otp / pay_password',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/expired',
    `payload`           JSON         DEFAULT NULL COMMENT '返回给前端的挑战材料（redirect_url / recipient_masked 等）',
    `expected_secret`   VARCHAR(256) DEFAULT NULL COMMENT 'OTP 明文或密码哈希，不返回给前端',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_attempts`      INT          NOT NULL DEFAULT 0,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `expires_at`        DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`         (`payment_intent_id`),
    KEY `idx_charge`     (`charge_id`),
    KEY `idx_type`       (`action_type`),
    KEY `idx_status`     (`status`),
    KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='PayAction 表（用户挑战：3DS / OTP / PayPassword）';

-- 4. ExceptionCase 表 exception_case_29（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_29` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `case_type`         VARCHAR(32)  NOT NULL COMMENT 'late_refund_success/late_refund_failed/late_charge_success/amount_mismatch/duplicate_webhook',
    `status`            VARCHAR(16)  NOT NULL COMMENT 'open/processing/resolved/ignored',
    `summary`           VARCHAR(512) DEFAULT NULL,
    `payload`           JSON         DEFAULT NULL,
    `resolution`        VARCHAR(512) DEFAULT NULL,
    `assigned_to`       VARCHAR(64)  DEFAULT NULL,
    `resolved_at`       DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`     (`payment_intent_id`),
    KEY `idx_refund` (`refund_id`),
    KEY `idx_type`   (`case_type`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='差错处理单';

-- 5. NotifyLog 表 notify_log_29（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_29` (
    `id`                VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `event_id`          VARCHAR(64)  NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL COMMENT 'payment_intent.succeeded / charge.refunded / ...',
    `client_type`       VARCHAR(16)  DEFAULT NULL COMMENT 'server/web/ios/android/miniapp/internal',
    `notify_channel`    VARCHAR(16)  NOT NULL COMMENT 'http/apns/fcm/websocket/mq/sms/email',
    `target`            VARCHAR(512) NOT NULL,
    `payload`           BLOB         DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/retrying/succeeded/failed/canceled',
    `attempt_count`     INT          NOT NULL DEFAULT 0,
    `max_retries`       INT          NOT NULL DEFAULT 5,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `http_status`       INT          DEFAULT NULL,
    `error_code`        VARCHAR(64)  DEFAULT NULL,
    `error_msg`         TEXT         DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_pi`            (`payment_intent_id`),
    KEY `idx_event`         (`event_id`),
    KEY `idx_event_type`    (`event_type`),
    KEY `idx_status`        (`status`),
    -- wave L: cover the ListDue() predicate (status IN (...) AND next_retry_at)
    -- so the cron scan hits one composite instead of two single-column indexes.
    KEY `idx_status_retry`  (`status`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通知日志';

-- 5. Refund 表 refund_29（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_29` (
    `id`                VARCHAR(64)  NOT NULL,
    `charge_id`         VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  NOT NULL,
    -- P0 #3 幂等键：(payment_intent_id, idempotency_key) 唯一。
    -- 客户端 retry Create 同一退款请求 → DB 兜底防止双行；service 层先查 GetByIdempotencyKey 返已有。
    `idempotency_key`   VARCHAR(128) DEFAULT NULL,
    `amount`            BIGINT       NOT NULL,
    `currency`          CHAR(3)      NOT NULL,
    `status`            VARCHAR(16)  NOT NULL COMMENT 'pending/succeeded/failed/canceled',
    `reason`            VARCHAR(32)  DEFAULT NULL,
    `failure_reason`    VARCHAR(256) DEFAULT NULL,
    `receipt_number`    VARCHAR(64)  DEFAULT NULL,
    `metadata`          JSON         DEFAULT NULL,
    `auto_compensate`   TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '系统自动补偿退款：retry worker 无限重试直到成功；商户发起=0',
    `retry_count`       INT          NOT NULL DEFAULT 0,
    `next_retry_at`     DATETIME(3)  DEFAULT NULL,
    `completed_at`      DATETIME(3)  DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem`       (`payment_intent_id`, `idempotency_key`),
    KEY `idx_charge`          (`charge_id`),
    KEY `idx_pi`              (`payment_intent_id`),
    KEY `idx_status`          (`status`),
    KEY `idx_auto_compensate` (`auto_compensate`),
    KEY `idx_next_retry`      (`next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Refund 表';

-- 7. InboundWebhook 表 inbound_webhook_29
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_29` (
    `id`                VARCHAR(64)  NOT NULL,
    `channel_name`      VARCHAR(32)  NOT NULL,
    `event_id`          VARCHAR(128) NOT NULL,
    `event_type`        VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64)  DEFAULT NULL,
    `charge_id`         VARCHAR(64)  DEFAULT NULL,
    `refund_id`         VARCHAR(64)  DEFAULT NULL,
    `dispute_id`        VARCHAR(64)  DEFAULT NULL,
    `signature_ok`      TINYINT(1)   NOT NULL DEFAULT 0,
    `headers`           JSON         DEFAULT NULL,
    `body`              MEDIUMBLOB   DEFAULT NULL,
    `processed_at`      DATETIME(3)  DEFAULT NULL,
    `process_status`    VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/succeeded/failed/duplicate',
    `error_msg`         TEXT         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_event`  (`channel_name`, `event_id`),
    KEY         `idx_pi`            (`payment_intent_id`),
    KEY         `idx_event_type`    (`event_type`),
    KEY         `idx_process_status` (`process_status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='入站 webhook 去重日志';

-- 8. Dispute 表 dispute_29
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_29` (
    `id`                   VARCHAR(64)  NOT NULL COMMENT 'dp_xxx',
    `payment_intent_id`    VARCHAR(64)  NOT NULL,
    `charge_id`            VARCHAR(64)  NOT NULL,
    `merchant_id`          VARCHAR(32)  NOT NULL,
    `channel`              VARCHAR(32)  NOT NULL COMMENT 'gcash/maya/paymongo/...',
    `channel_dispute_id`   VARCHAR(128) DEFAULT NULL COMMENT '渠道侧 dispute id (用于回写)',
    `status`               VARCHAR(24)  NOT NULL COMMENT 'needs_response/under_review/won/lost/warning_closed/charge_refunded/canceled',
    `amount`               BIGINT       NOT NULL COMMENT 'disputed amount in minor units',
    `currency`             CHAR(3)      NOT NULL DEFAULT 'PHP',
    `reason`               VARCHAR(64)  NOT NULL COMMENT 'fraud/product_not_received/unrecognized/duplicate/credit_not_processed/other',
    `reason_detail`        VARCHAR(512) DEFAULT NULL,
    `evidence_due_at`      DATETIME(3)  DEFAULT NULL COMMENT '商户响应截止时间',
    `evidence`             JSON         DEFAULT NULL COMMENT '证据包：{text, photos:[url], shipping:{}, emails:[url], receipt:url, ...}',
    `decided_at`           DATETIME(3)  DEFAULT NULL,
    `outcome_amount`       BIGINT       DEFAULT NULL COMMENT '实际被扣回金额（lost 情形），<= amount',
    `auto_refund_charge`   TINYINT(1)   NOT NULL DEFAULT 1 COMMENT 'lost/charge_refunded 时是否联动退款',
    `metadata`             JSON         DEFAULT NULL,
    `created`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`              DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_channel_dispute` (`channel`, `channel_dispute_id`),
    KEY `idx_pi`        (`payment_intent_id`),
    KEY `idx_charge`    (`charge_id`),
    KEY `idx_merchant`  (`merchant_id`),
    KEY `idx_status`    (`status`),
    KEY `idx_due`       (`status`, `evidence_due_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute / Chargeback';

-- 9. DisputeEvent 表 dispute_event_29（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_29` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `dispute_id`      VARCHAR(64)  NOT NULL,
    `payment_intent_id` VARCHAR(64) NOT NULL,
    `from_status`     VARCHAR(24)  NOT NULL,
    `to_status`       VARCHAR(24)  NOT NULL,
    `source`          VARCHAR(32)  NOT NULL COMMENT 'channel_webhook / admin / system',
    `actor`           VARCHAR(64)  DEFAULT NULL,
    `note`            VARCHAR(512) DEFAULT NULL,
    `payload`         JSON         DEFAULT NULL COMMENT '触发事件 (webhook body / admin input)',
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_dispute`  (`dispute_id`),
    KEY `idx_pi`       (`payment_intent_id`),
    KEY `idx_created`  (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Dispute 状态流转日志';

-- 10. AccountingOutbox 表 accounting_outbox_29
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_29` (
    `id`                 VARCHAR(64)  NOT NULL,
    `request_id`         VARCHAR(160) NOT NULL,
    `event_type`         VARCHAR(32)  NOT NULL COMMENT 'charge_succeeded/refund_succeeded',
    `payment_intent_id`  VARCHAR(64)  NOT NULL,
    `charge_id`          VARCHAR(64)  DEFAULT NULL,
    `refund_id`          VARCHAR(64)  DEFAULT NULL,
    `owner_type`         VARCHAR(16)  NOT NULL COMMENT 'user/merchant',
    `owner_id`           VARCHAR(64)  NOT NULL,
    `payment_method`     VARCHAR(32)  NOT NULL COMMENT 'gcash/shopeepay/maya/grabpay/...',
    `amount`             BIGINT       NOT NULL,
    `currency`           CHAR(3)      NOT NULL,
    `metadata`           JSON         DEFAULT NULL,
    `status`             VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/sent/failed',
    `attempts`           INT          NOT NULL DEFAULT 0,
    `next_attempt_at`    DATETIME(3)  DEFAULT NULL,
    `last_error`         TEXT         DEFAULT NULL,
    `sent_at`            DATETIME(3)  DEFAULT NULL,
    -- claim_token 用于多 worker / 多副本并发投递时的批次 claim：
    -- worker 用一条 UPDATE ... LIMIT N 把候选行的 next_attempt_at 推到远未来 +
    -- 写入自己生成的 token，其他 worker 的 SELECT 因 next_attempt_at 在未来天然
    -- 跳过；本 worker 再用 SELECT WHERE claim_token = ? 拉回该批次处理。
    -- 模式同 webhook_deliveries.claim_token（见 internal/webhook/delivery.go）。
    `claim_token`        VARCHAR(64)  DEFAULT NULL,
    `created`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`            DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_request`       (`request_id`),
    KEY         `idx_pi`           (`payment_intent_id`),
    KEY         `idx_status_next`  (`status`, `next_attempt_at`),
    -- claim_token 反查刚 claim 的批次；带前缀 + 纳秒时间戳 + 随机数，区分度足够
    KEY         `idx_claim`        (`claim_token`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='记账事件 outbox（投递给 accounting-system）';

-- ─── admin_audit_log_29：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_29` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id (from bearer token)',
    `actor_ip`       VARCHAR(64)  DEFAULT NULL,
    `action`         VARCHAR(64)  NOT NULL COMMENT 'merchant.approve / refund.create / risk.rule.update / ...',
    `target_type`    VARCHAR(32)  DEFAULT NULL,
    `target_id`      VARCHAR(64)  DEFAULT NULL,
    `http_method`    VARCHAR(8)   DEFAULT NULL,
    `http_path`      VARCHAR(256) DEFAULT NULL,
    `http_status`    INT          DEFAULT NULL,
    `request_body`   MEDIUMTEXT   DEFAULT NULL COMMENT 'JSON, sensitive fields redacted by caller',
    `response_code`  VARCHAR(32)  DEFAULT NULL,
    `response_msg`   VARCHAR(512) DEFAULT NULL,
    `duration_ms`    INT          DEFAULT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_action`  (`action`),
    KEY `idx_target`  (`target_type`, `target_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 29';


-- ==== accounting-system schema ====
SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS `accounting_db_2` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `accounting_db_2`;

-- ============================================
-- 复式记账账户系统 - 数据库表设计
-- 分库分表架构：10库 × 每库10表 = 100张全局表
--
-- 统一路由规则：
--   n = id % 100
--   dbIndex        = n / 10      （物理库编号 0–9）
--   globalTableIdx = n           （全局表序号 0–99）
--
-- 表名映射（以 account 为例）：
--   DB 0 → account_00 ~ account_09
--   DB 1 → account_10 ~ account_19
--   ...
--   DB 9 → account_90 ~ account_99
-- ============================================

-- ============================================
-- 1. 账户主表 (account_xxx)
-- 分库分表：n = user_id % 100; dbIdx = n/10; tableIdx = n
-- ============================================


CREATE TABLE IF NOT EXISTS `account_20` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号（格式：{dbIdx:02d}{tableIdx:02d}{userId}-{businessType:03d}，编码路由信息）',
    `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
    `account_type` TINYINT NOT NULL COMMENT '账户类型：1-用户账户 2-商户账户 3-平台损益账户 4-中间账户',
    `account_category` VARCHAR(32) NOT NULL COMMENT '账户分类：ASSET资产/LIABILITY负债/EQUITY所有者权益/REVENUE收入/EXPENSE费用',
    `account_business_type` SMALLINT NOT NULL DEFAULT 0 COMMENT '账户业务类型：1-用户余额 2-商户结算 3-商户待结算（最大999）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `balance` BIGINT NOT NULL DEFAULT 0 COMMENT '当前余额（ISO最小单位×100，例如USD $3.42存为34200）',
    `frozen_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '冻结余额',
    `available_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '可用余额',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-禁用 1-正常 2-冻结',
    `account_group` CHAR(1) NOT NULL DEFAULT 'A' COMMENT '账户分组：A=默认/当前；B=轮换预创建的下一组。非轮换账户永远是 A',
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    -- 轮换字段（rotating-suspense-accounts 特性，见 docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md）
    -- 旧账户 logical_account_id=NULL 且 lifecycle_phase=0 (legacy)，写入路径与原行为一致。
    `logical_account_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '关联 logical_account.id；NULL=legacy 账户',
    `period_start` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期起点（包含），UTC',
    `period_end` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期终点（不含），UTC',
    `lifecycle_phase` TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=active 2=draining 3=frozen 4=archived 5=provisioned 9=quarantined',
    `draining_started_at` DATETIME DEFAULT NULL COMMENT '进入 draining 的时刻',
    `frozen_at` DATETIME DEFAULT NULL COMMENT '进入 frozen 的时刻',
    `archived_at` DATETIME DEFAULT NULL COMMENT '进入 archived 的时刻',
    `policy_version_at_birth` BIGINT DEFAULT NULL COMMENT '出生时锁定的 policy_version；后续策略变更不影响本 instance (E-28/E-29)',
    `effective_hard_timeout_secs` INT DEFAULT NULL COMMENT '策略覆盖：本 instance 专用 hard timeout (§11.X 紧急运维)',
    `override_reason` VARCHAR(255) DEFAULT NULL COMMENT 'override 原因',
    `override_by` VARCHAR(64) DEFAULT NULL COMMENT 'override 操作人',
    `override_at` DATETIME DEFAULT NULL COMMENT 'override 时刻',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`, `account_group`) COMMENT 'userId + 业务类型 + 币种 + 组 唯一键 (轮换 fleet 时 GroupA / GroupB 各 1 个)',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`),
    KEY `idx_logical_phase` (`logical_account_id`, `lifecycle_phase`),
    KEY `idx_phase_period_end` (`lifecycle_phase`, `period_end`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_20` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `transaction_id` VARCHAR(64) NOT NULL COMMENT '交易流水号',
    `parent_transaction_id` VARCHAR(64) DEFAULT NULL COMMENT '父交易流水号（用于关联复式记账的多条记录）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型：TRANSFER/PAYMENT/REFUND/WITHDRAW等',
    `debit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '借方金额（ISO最小单位×100）',
    `credit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '贷方金额（ISO最小单位×100）',
    `balance_before` BIGINT NOT NULL COMMENT '交易前余额',
    `balance_after` BIGINT NOT NULL COMMENT '交易后余额',
    `booking_type` TINYINT NOT NULL DEFAULT 1 COMMENT '记账方式：1-同步记账 2-缓冲记账',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `transaction_date` CHAR(10) NOT NULL COMMENT '交易日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑；ISO 格式按字典序与 DATE 同序，索引/范围查询无影响）',
    `transaction_time` DATETIME NOT NULL COMMENT '交易时间',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '交易描述',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-失败 1-成功 2-处理中',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，原因：DSN parseTime 让 gorm 做 TZ 转换 → 1 天位移 bug）。在 booking 入口由 computeCutDate(now, scheduled_time, tz) 一次性确定，propagate 到所有 entry + tcc_coordinator。日切扫描 WHERE cut_date=X AND status=1，同 voucher 所有 entry 共享同一 cut_date → 试算平衡天然成立',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `ext_info` JSON DEFAULT NULL COMMENT '扩展信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_transaction_id` (`transaction_id`),
    KEY `idx_account_no` (`account_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_parent_transaction_id` (`parent_transaction_id`),
    KEY `idx_status` (`status`),
    -- 复合索引：账户流水分页查询（account_no + 日期范围 + 时间排序），取代独立的 idx_account_no + idx_transaction_date
    KEY `idx_account_date_time` (`account_no`, `transaction_date`, `transaction_time`),
    -- 日切扫描主索引：WHERE cut_date=X AND status=1
    KEY `idx_cut_date_status` (`cut_date`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户流水表';

-- ============================================
-- 3. 复式记账凭证表 (accounting_voucher_xxx)
-- 一笔业务对应一张凭证，凭证关联多条分录
-- 分库分表：按 voucher_no 前3字符路由（与 business_no 同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `accounting_voucher_20` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '凭证号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型',
    `total_debit` BIGINT NOT NULL COMMENT '借方总金额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL COMMENT '贷方总金额（ISO最小单位×100）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `voucher_date` CHAR(10) NOT NULL COMMENT '凭证日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-作废 1-生效 2-待审核',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '凭证描述',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_voucher_date` (`voucher_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='复式记账凭证表';

-- ============================================
-- 4. 账户余额快照表 (account_balance_snapshot_xxx)
-- 每日日切生成快照
-- 分库分表：按 account_no 前3字符路由（与账户主表同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_20` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `snapshot_date` CHAR(10) NOT NULL COMMENT '快照日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '关联日切版本号，重跑时递增',
    `beginning_balance` BIGINT NOT NULL COMMENT '期初余额（ISO最小单位×100）',
    `ending_balance` BIGINT NOT NULL COMMENT '期末余额（ISO最小单位×100）',
    `total_debit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日借方总额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日贷方总额（ISO最小单位×100）',
    `transaction_count` INT NOT NULL DEFAULT 0 COMMENT '当日交易笔数',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '该账户本日最后一笔流水ID，仅供审计/对账使用',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_snapshot` (`account_no`, `snapshot_date`, `run_id`),
    KEY `idx_snapshot_date` (`snapshot_date`),
    KEY `idx_snapshot_date_run` (`snapshot_date`, `run_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额快照表';

-- ============================================
-- 5. 日切控制表 (day_cut_control)
-- 全局表，不分库分表，记录每个库每张表的日切点
-- ============================================
CREATE TABLE IF NOT EXISTS `day_cut_control_20` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `database_index` TINYINT NOT NULL COMMENT '库索引 0-9',
    `table_index` SMALLINT NOT NULL COMMENT '表索引 0-99',
    `table_name` VARCHAR(64) NOT NULL COMMENT '表名',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 避开 DATE TZ 转换坑）。扫描走 account_transaction.cut_date = 本字段',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '运行版本号，同一日期每次重跑时递增',
    `last_processed_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '本 run 在分片上已处理到的 account_transaction.id（含）。chunk 内分页游标',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '本 run 完成时最后处理的 transaction_id，仅供审计',
    `currency` CHAR(3) NOT NULL DEFAULT '' COMMENT '币种过滤。空 = 全部币种（兼容历史）；非空 = 仅该币种账户',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-未开始 1-进行中 2-已完成 3-失败',
    `start_time` DATETIME DEFAULT NULL COMMENT '开始时间',
    `end_time` DATETIME DEFAULT NULL COMMENT '结束时间',
    `cut_time` DATETIME(3) DEFAULT NULL COMMENT '日切完成时的服务器墙钟时间，仅供审计',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_day_cut` (`database_index`, `table_index`, `cut_date`, `run_id`),
    KEY `idx_cut_date` (`cut_date`),
    KEY `idx_cut_date_run` (`cut_date`, `run_id`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='日切控制表';

-- ============================================
-- 6. 异步任务表 (async_task)
-- 用于Kafka消息消费失败后的重试
-- 全局表，不分库分表
-- ============================================
CREATE TABLE IF NOT EXISTS `async_task_20` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `task_id` VARCHAR(64) NOT NULL COMMENT '任务ID',
    `task_type` VARCHAR(32) NOT NULL COMMENT '任务类型：ACCOUNTING/SNAPSHOT/DAY_CUT',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `task_data` JSON NOT NULL COMMENT '任务数据',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-待处理 1-处理中 2-成功 3-失败',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `max_retry_count` INT NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `next_retry_time` DATETIME DEFAULT NULL COMMENT '下次重试时间',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status_retry` (`status`, `next_retry_time`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='异步任务表';

-- ============================================
-- 7. TCC 分支记录表 (tcc_transaction_xxx)
-- 与账户表同分片（按 account_no 路由），保证 Try/Confirm/Cancel 在单库事务内完成。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_transaction_20` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `tcc_id` VARCHAR(64) NOT NULL COMMENT '全局 TCC ID（= voucherNo）',
    `branch_id` VARCHAR(64) NOT NULL COMMENT 'TCC 分支 ID（= transactionID）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `balance_delta` BIGINT NOT NULL COMMENT 'Confirm 时对 balance 的增量（负=减少，ISO最小单位×100）',
    `frozen_amount` BIGINT NOT NULL DEFAULT 0 COMMENT 'Try 时冻结的可用余额（0=无需冻结）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-TRYING 1-CONFIRMED 2-CANCELLED',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_branch_id` (`branch_id`),
    KEY `idx_tcc_id` (`tcc_id`),
    KEY `idx_account_no` (`account_no`),
    -- 复合索引：卡住 TCC 分支查询（status=TRYING AND created_at < cutoff），避免全表扫描
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 分布式事务分支记录表';

-- ============================================
-- 7b. UnfreezeAndDebit 补偿 outbox (freeze_compensate_outbox_xxx)
--
-- 与 frozen account 同分片（按 voucher_no 前 3 字符 = dbIdx + tableIdx 路由），
-- 保证 Phase 1 frozen-debit + INSERT outbox PENDING 在单库事务原子。
--
-- 状态机：
--   0 PENDING  : 已写入；可能被 caller mark DONE 或 worker 接管补偿
--   1 DONE     : 终态；caller Phase 2 全成功 或 worker 补偿全成功
--   2 FAILED   : 终态；worker 重试到达上限，需人工处理
--
-- payload (JSON) 包含完整 reverse plan：frozen account 信息 + 全部 credit
-- entries 信息 + 已成功的 credit txID 列表（worker 据此精确反向）。
--
-- worker 选取规则：status=PENDING AND created_at < now - 60s（给 caller
-- 一分钟时间走完 Phase 2 + 标记 DONE，避免误抢）；CAS UPDATE PENDING → COMPENSATING
-- 防多 pod / 多 worker 重复处理。
-- ============================================
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_20` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '关联的 unfreeze voucher（=路由键，前3字符为 dbIdx+tableIdx）',
    `freeze_order_no` VARCHAR(64) NOT NULL COMMENT '关联的 freeze_order 号（运维诊断用）',
    `payload` MEDIUMTEXT NOT NULL COMMENT 'JSON：完整 reverse plan（frozen acct + credit entries + 已成功 txIDs）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=DONE 2=COMPENSATING 3=FAILED',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '补偿重试次数；达 max_retry 置 FAILED',
    `error_msg` VARCHAR(500) DEFAULT NULL COMMENT '最近一次失败原因（FAILED 状态用于审计）',
    `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
    `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UnfreezeAndDebit 补偿 outbox（保证最终一致性）';

-- ============================================
-- 8. 分布式锁表 (distributed_lock)
-- 用于日切等场景的分布式锁
-- ============================================
CREATE TABLE IF NOT EXISTS `distributed_lock_20` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `lock_key` VARCHAR(128) NOT NULL COMMENT '锁键',
    `lock_value` VARCHAR(64) NOT NULL COMMENT '锁值（UUID）',
    `expire_time` DATETIME NOT NULL COMMENT '过期时间',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lock_key` (`lock_key`),
    KEY `idx_expire_time` (`expire_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='分布式锁表';


-- ============================================
-- 10. 商户信息表 (merchant_info)
-- ============================================
CREATE TABLE IF NOT EXISTS `merchant_info_20` (
    `id`                   BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `merchant_id`          BIGINT(20) UNSIGNED NOT NULL COMMENT '商户唯一ID',
    `merchant_name`        VARCHAR(256) NOT NULL COMMENT '商户名称',
    `account_idc`          VARCHAR(32)  NOT NULL COMMENT '商户账户所属 IDC',
    `external_merchant_id` VARCHAR(256) NOT NULL COMMENT '外部商户ID',
    `mid`                  VARCHAR(64)  NOT NULL COMMENT '业务线标识',
    `business_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务ID',
    `extra`                TEXT DEFAULT NULL,
    `byte_rds_ctx`         TEXT DEFAULT NULL,
    `create_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_merchant_id` (`merchant_id`),
    UNIQUE KEY `uniq_external_merchant_id_mid` (`external_merchant_id`, `mid`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户信息表';

-- ============================================
-- 11. 交易订单表 (transaction_order_xxx)
-- 分库分表：n = business_no(numeric) % 100; dbIdx = n/10; tableIdx = n
-- 幂等键：(order_no, business_type, business_no)
-- Extra 字段存储：预生成的 voucher_no、tx_ids、完整请求参数快照（reqParams）
-- ============================================
-- transaction_order 主表：状态字段 + 固定大小业务元数据（VARCHAR，行宽可控）
-- 大块 extra 字段独立存储到 transaction_order_extra（见下文），热路径不访问 extra
CREATE TABLE IF NOT EXISTS `transaction_order_20` (
    `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`       VARCHAR(64)  NOT NULL COMMENT '外部幂等订单号（requestId）',
    `business_no`    VARCHAR(64)  NOT NULL COMMENT '业务订单号（分片键，按后2位路由）',
    `business_type`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '业务类型',
    `product_code`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '产品编码',
    `event_code`     VARCHAR(256) NOT NULL DEFAULT '' COMMENT '事件编码',
    `from_party_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '资金出方 owner id',
    `from_party_type` VARCHAR(32) NOT NULL DEFAULT '' COMMENT 'user | merchant | platform',
    `to_party_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '资金入方 owner id',
    `to_party_type`  VARCHAR(32)  NOT NULL DEFAULT '',
    `amount`         VARCHAR(32)  NOT NULL DEFAULT '0' COMMENT '金额（字符串存储）',
    `currency`       VARCHAR(8)   NOT NULL DEFAULT 'PHP',
    `status`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `retry_count`    INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `max_retry_count` INT         NOT NULL DEFAULT 3 COMMENT '最大允许重试次数',
    `voucher_no`     VARCHAR(64)  DEFAULT NULL COMMENT '记账凭证号（成功后写入）',
    `error_message`  VARCHAR(512) DEFAULT NULL COMMENT '最近一次失败原因',
    `description`    VARCHAR(256) DEFAULT NULL,
    `created_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_biz` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单主表（分库分表）—— extra 独立存储提升缓存命中率';

-- ============================================
-- 12. 交易订单扩展字段表 (transaction_order_extra_xxx)
-- 与 transaction_order_xxx 同分片（按 business_no 路由）
-- 存储：请求参数快照、预生成 ID（req_hash/voucher_no/tx_ids/req_params）
-- 热路径（状态查询/更新）不访问此表；仅创建/重试/审计时读写
-- extra VARCHAR(4096)：4096字符上限，代码层写入前强制截断
-- ============================================
CREATE TABLE IF NOT EXISTS `transaction_order_extra_20` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`   VARCHAR(64)  NOT NULL COMMENT '关联 transaction_order.order_no',
    `business_no` VARCHAR(64) NOT NULL COMMENT '分片键（与主表路由一致）',
    `business_type` VARCHAR(32) NOT NULL DEFAULT '',
    `extra`      VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON: {req_hash,voucher_no,tx_ids,req_params}，最长4096字符',
    `created_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_extra` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单扩展字段表（与主表同分片，热路径不访问）';


-- ============================================
-- 13. 缓冲余额更新表 (account_balance_buffer_xxx)
-- 与账户主表同分片（按 account_no 路由），用于平台/中间账户的批量余额聚合写入。
-- 设计：平台/中间账户允许负余额，写流水后先不更新 account.balance，
--        而是在此表累计增量；后台 flush worker 每100条或每30秒批量更新 account 余额，
--        减少高频账户对 account 表的锁竞争。
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_buffer_20` (
    `account_no`       VARCHAR(64)   NOT NULL COMMENT '账户号',
    `pending_delta`    BIGINT NOT NULL DEFAULT 0 COMMENT '待刷新的余额增量（可为负，ISO最小单位×100）',
    `pending_count`    INT           NOT NULL DEFAULT 0 COMMENT '待刷新的流水笔数',
    `flush_scheduled_at` DATETIME(3) NULL DEFAULT NULL COMMENT '下次计划刷新时间（NULL=立即刷新；worker 按此时间触发，执行后自动延后一个间隔）',
    `last_updated_at`  DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最后写入时间',
    PRIMARY KEY (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额缓冲聚合表（平台/中间账户缓冲记账）';

-- ============================================
-- TCC 全局协调者表 (tcc_coordinator_xxx)
-- 分库 + 分表：按 tcc_id 数字 hash(tcc_id) 路由到 (dbIndex, globalTableIndex)，
-- 与 account_transaction / tcc_transaction 路由规则一致，保证同一笔 booking 所有
-- 相关记录共享分片，避免跨分片事务。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_coordinator_20` (
    `tcc_id`        VARCHAR(64)  NOT NULL COMMENT 'TCC 全局事务 ID（= voucher_no）',
    `phase`         TINYINT      NOT NULL DEFAULT 1 COMMENT '全局阶段：1=TRYING 2=CONFIRMING 3=CONFIRMED 4=CANCELLED',
    `business_no`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '业务订单号',
    `branch_count`  SMALLINT     NOT NULL COMMENT '总分支数',
    `cut_date`      CHAR(10)     NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，避免 gorm parseTime 1 天位移 bug）。booking 入口处 computeCutDate 计算后 propagate；与 account_transaction.cut_date 一致。日切 drain 等待: WHERE cut_date <= X AND phase IN (TRYING, CONFIRMING) → 0',
    `currency`      VARCHAR(8)   NOT NULL DEFAULT '' COMMENT '币种（PHP/CNY/USD/...）。booking 入口写入；TCC Recovery 用 coord.currency 写补 confirm 流水，避免与原 booking 不同币种导致试算 PHP-only / currency-filter 不平。空字符串=旧数据兼容',
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`tcc_id`),
    KEY `idx_phase_updated` (`phase`, `updated_at`),
    KEY `idx_cut_date_phase` (`cut_date`, `phase`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 全局协调者（阶段追踪，100 张分表）';

-- ============================================
-- 索引优化说明
-- ============================================
-- 1. 所有分表都需要创建相同的表结构和索引
-- 2. 查询时优先使用分片键（user_id, account_no）
-- 3. 时间范围查询使用 transaction_date 索引
-- 4. 定期归档历史数据，保持表数据量在合理范围

-- ============================================
-- 13. 原子批量记账订单表 (batch_order)
-- 全局单表（不分库分表），记录原子批量记账的批次状态
-- 批次内各单笔记账通过 TCC 保证整体原子性：全部成功 or 全部回滚
-- extra VARCHAR(4096)：存储批次请求参数快照，最长4096字符
-- ============================================
CREATE TABLE IF NOT EXISTS `batch_order_20` (
    `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `batch_id`        VARCHAR(64)  NOT NULL COMMENT '批次幂等ID（调用方提供）',
    `business_no`     VARCHAR(64)  NOT NULL COMMENT '批次业务号',
    `item_count`      INT          NOT NULL DEFAULT 0 COMMENT '批次中单笔数量',
    `status`          TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `error_message`   VARCHAR(512) DEFAULT NULL COMMENT '失败原因',
    `description`     VARCHAR(256) DEFAULT NULL,
    `extra`           VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON批次请求参数快照，最长4096字符',
    `created_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_batch_id` (`batch_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='原子批量记账订单表（全局表）';

-- ============================================
-- 12. 结算 Outbox 表 (settlement_outbox)
-- 热路径事务预写日志（Transactional Outbox Pattern）
-- 分库，不分表：按 voucher_no 前1字符（dbIndex）路由，每库一张 settlement_outbox 表
-- ============================================
CREATE TABLE IF NOT EXISTS `settlement_outbox_20` (
    `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no`       VARCHAR(64)  NOT NULL COMMENT '凭证号（前3字符={1d-dbIdx}{2d-tableIdx}，用于路由）',
    `event_data`       LONGTEXT     NOT NULL COMMENT 'JSON: SettlementEvent',
    `transaction_date` CHAR(10)     NOT NULL COMMENT '交易日期 YYYY-MM-DD，供日切按天过滤 outbox delta',
    `cut_date`         CHAR(10)     NOT NULL DEFAULT '' COMMENT '日切归属日 YYYY-MM-DD（入口落库那一刻定死，永不重算）；空字符串=老数据按 transaction_date 兼容处理',
    `status`           TINYINT      NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=REDIS_DONE 2=MYSQL_DONE 3=FAILED',
    `retry_count`      INT          NOT NULL DEFAULT 0 COMMENT '重试次数',
    `error_msg`        TEXT         DEFAULT NULL COMMENT '最近一次失败原因',
    `created_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_date` (`status`, `transaction_date`),
    KEY `idx_status_created` (`status`, `created_at`),
    -- OutboxWorker MySQLWriter 高频轮询查询：
    --   SELECT * FROM settlement_outbox WHERE status = ? ORDER BY id ASC LIMIT 500
    -- (status, id) 复合索引让此查询走"索引范围扫描 + 已序输出"，免去 filesort。
    -- InnoDB 二级索引天然把 PK id 附在叶节点，但只有把 id 显式作为索引第二列，
    -- 优化器才会用 index range scan 满足 ORDER BY id；否则可能选 idx_status 后 sort。
    KEY `idx_status_id` (`status`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='热路径事务预写日志（Outbox）';

-- ============================================
-- 复式记账账户系统 - 数据库表设计
-- 分库分表架构：10库 × 每库10表 = 100张全局表
--
-- 统一路由规则：
--   n = id % 100
--   dbIndex        = n / 10      （物理库编号 0–9）
--   globalTableIdx = n           （全局表序号 0–99）
--
-- 表名映射（以 account 为例）：
--   DB 0 → account_00 ~ account_09
--   DB 1 → account_10 ~ account_19
--   ...
--   DB 9 → account_90 ~ account_99
-- ============================================

-- ============================================
-- 1. 账户主表 (account_xxx)
-- 分库分表：n = user_id % 100; dbIdx = n/10; tableIdx = n
-- ============================================


CREATE TABLE IF NOT EXISTS `account_21` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号（格式：{dbIdx:02d}{tableIdx:02d}{userId}-{businessType:03d}，编码路由信息）',
    `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
    `account_type` TINYINT NOT NULL COMMENT '账户类型：1-用户账户 2-商户账户 3-平台损益账户 4-中间账户',
    `account_category` VARCHAR(32) NOT NULL COMMENT '账户分类：ASSET资产/LIABILITY负债/EQUITY所有者权益/REVENUE收入/EXPENSE费用',
    `account_business_type` SMALLINT NOT NULL DEFAULT 0 COMMENT '账户业务类型：1-用户余额 2-商户结算 3-商户待结算（最大999）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `balance` BIGINT NOT NULL DEFAULT 0 COMMENT '当前余额（ISO最小单位×100，例如USD $3.42存为34200）',
    `frozen_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '冻结余额',
    `available_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '可用余额',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-禁用 1-正常 2-冻结',
    `account_group` CHAR(1) NOT NULL DEFAULT 'A' COMMENT '账户分组：A=默认/当前；B=轮换预创建的下一组。非轮换账户永远是 A',
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    -- 轮换字段（rotating-suspense-accounts 特性，见 docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md）
    -- 旧账户 logical_account_id=NULL 且 lifecycle_phase=0 (legacy)，写入路径与原行为一致。
    `logical_account_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '关联 logical_account.id；NULL=legacy 账户',
    `period_start` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期起点（包含），UTC',
    `period_end` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期终点（不含），UTC',
    `lifecycle_phase` TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=active 2=draining 3=frozen 4=archived 5=provisioned 9=quarantined',
    `draining_started_at` DATETIME DEFAULT NULL COMMENT '进入 draining 的时刻',
    `frozen_at` DATETIME DEFAULT NULL COMMENT '进入 frozen 的时刻',
    `archived_at` DATETIME DEFAULT NULL COMMENT '进入 archived 的时刻',
    `policy_version_at_birth` BIGINT DEFAULT NULL COMMENT '出生时锁定的 policy_version；后续策略变更不影响本 instance (E-28/E-29)',
    `effective_hard_timeout_secs` INT DEFAULT NULL COMMENT '策略覆盖：本 instance 专用 hard timeout (§11.X 紧急运维)',
    `override_reason` VARCHAR(255) DEFAULT NULL COMMENT 'override 原因',
    `override_by` VARCHAR(64) DEFAULT NULL COMMENT 'override 操作人',
    `override_at` DATETIME DEFAULT NULL COMMENT 'override 时刻',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`, `account_group`) COMMENT 'userId + 业务类型 + 币种 + 组 唯一键 (轮换 fleet 时 GroupA / GroupB 各 1 个)',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`),
    KEY `idx_logical_phase` (`logical_account_id`, `lifecycle_phase`),
    KEY `idx_phase_period_end` (`lifecycle_phase`, `period_end`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_21` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `transaction_id` VARCHAR(64) NOT NULL COMMENT '交易流水号',
    `parent_transaction_id` VARCHAR(64) DEFAULT NULL COMMENT '父交易流水号（用于关联复式记账的多条记录）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型：TRANSFER/PAYMENT/REFUND/WITHDRAW等',
    `debit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '借方金额（ISO最小单位×100）',
    `credit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '贷方金额（ISO最小单位×100）',
    `balance_before` BIGINT NOT NULL COMMENT '交易前余额',
    `balance_after` BIGINT NOT NULL COMMENT '交易后余额',
    `booking_type` TINYINT NOT NULL DEFAULT 1 COMMENT '记账方式：1-同步记账 2-缓冲记账',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `transaction_date` CHAR(10) NOT NULL COMMENT '交易日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑；ISO 格式按字典序与 DATE 同序，索引/范围查询无影响）',
    `transaction_time` DATETIME NOT NULL COMMENT '交易时间',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '交易描述',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-失败 1-成功 2-处理中',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，原因：DSN parseTime 让 gorm 做 TZ 转换 → 1 天位移 bug）。在 booking 入口由 computeCutDate(now, scheduled_time, tz) 一次性确定，propagate 到所有 entry + tcc_coordinator。日切扫描 WHERE cut_date=X AND status=1，同 voucher 所有 entry 共享同一 cut_date → 试算平衡天然成立',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `ext_info` JSON DEFAULT NULL COMMENT '扩展信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_transaction_id` (`transaction_id`),
    KEY `idx_account_no` (`account_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_parent_transaction_id` (`parent_transaction_id`),
    KEY `idx_status` (`status`),
    -- 复合索引：账户流水分页查询（account_no + 日期范围 + 时间排序），取代独立的 idx_account_no + idx_transaction_date
    KEY `idx_account_date_time` (`account_no`, `transaction_date`, `transaction_time`),
    -- 日切扫描主索引：WHERE cut_date=X AND status=1
    KEY `idx_cut_date_status` (`cut_date`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户流水表';

-- ============================================
-- 3. 复式记账凭证表 (accounting_voucher_xxx)
-- 一笔业务对应一张凭证，凭证关联多条分录
-- 分库分表：按 voucher_no 前3字符路由（与 business_no 同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `accounting_voucher_21` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '凭证号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型',
    `total_debit` BIGINT NOT NULL COMMENT '借方总金额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL COMMENT '贷方总金额（ISO最小单位×100）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `voucher_date` CHAR(10) NOT NULL COMMENT '凭证日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-作废 1-生效 2-待审核',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '凭证描述',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_voucher_date` (`voucher_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='复式记账凭证表';

-- ============================================
-- 4. 账户余额快照表 (account_balance_snapshot_xxx)
-- 每日日切生成快照
-- 分库分表：按 account_no 前3字符路由（与账户主表同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_21` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `snapshot_date` CHAR(10) NOT NULL COMMENT '快照日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '关联日切版本号，重跑时递增',
    `beginning_balance` BIGINT NOT NULL COMMENT '期初余额（ISO最小单位×100）',
    `ending_balance` BIGINT NOT NULL COMMENT '期末余额（ISO最小单位×100）',
    `total_debit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日借方总额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日贷方总额（ISO最小单位×100）',
    `transaction_count` INT NOT NULL DEFAULT 0 COMMENT '当日交易笔数',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '该账户本日最后一笔流水ID，仅供审计/对账使用',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_snapshot` (`account_no`, `snapshot_date`, `run_id`),
    KEY `idx_snapshot_date` (`snapshot_date`),
    KEY `idx_snapshot_date_run` (`snapshot_date`, `run_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额快照表';

-- ============================================
-- 5. 日切控制表 (day_cut_control)
-- 全局表，不分库分表，记录每个库每张表的日切点
-- ============================================
CREATE TABLE IF NOT EXISTS `day_cut_control_21` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `database_index` TINYINT NOT NULL COMMENT '库索引 0-9',
    `table_index` SMALLINT NOT NULL COMMENT '表索引 0-99',
    `table_name` VARCHAR(64) NOT NULL COMMENT '表名',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 避开 DATE TZ 转换坑）。扫描走 account_transaction.cut_date = 本字段',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '运行版本号，同一日期每次重跑时递增',
    `last_processed_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '本 run 在分片上已处理到的 account_transaction.id（含）。chunk 内分页游标',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '本 run 完成时最后处理的 transaction_id，仅供审计',
    `currency` CHAR(3) NOT NULL DEFAULT '' COMMENT '币种过滤。空 = 全部币种（兼容历史）；非空 = 仅该币种账户',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-未开始 1-进行中 2-已完成 3-失败',
    `start_time` DATETIME DEFAULT NULL COMMENT '开始时间',
    `end_time` DATETIME DEFAULT NULL COMMENT '结束时间',
    `cut_time` DATETIME(3) DEFAULT NULL COMMENT '日切完成时的服务器墙钟时间，仅供审计',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_day_cut` (`database_index`, `table_index`, `cut_date`, `run_id`),
    KEY `idx_cut_date` (`cut_date`),
    KEY `idx_cut_date_run` (`cut_date`, `run_id`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='日切控制表';

-- ============================================
-- 6. 异步任务表 (async_task)
-- 用于Kafka消息消费失败后的重试
-- 全局表，不分库分表
-- ============================================
CREATE TABLE IF NOT EXISTS `async_task_21` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `task_id` VARCHAR(64) NOT NULL COMMENT '任务ID',
    `task_type` VARCHAR(32) NOT NULL COMMENT '任务类型：ACCOUNTING/SNAPSHOT/DAY_CUT',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `task_data` JSON NOT NULL COMMENT '任务数据',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-待处理 1-处理中 2-成功 3-失败',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `max_retry_count` INT NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `next_retry_time` DATETIME DEFAULT NULL COMMENT '下次重试时间',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status_retry` (`status`, `next_retry_time`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='异步任务表';

-- ============================================
-- 7. TCC 分支记录表 (tcc_transaction_xxx)
-- 与账户表同分片（按 account_no 路由），保证 Try/Confirm/Cancel 在单库事务内完成。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_transaction_21` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `tcc_id` VARCHAR(64) NOT NULL COMMENT '全局 TCC ID（= voucherNo）',
    `branch_id` VARCHAR(64) NOT NULL COMMENT 'TCC 分支 ID（= transactionID）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `balance_delta` BIGINT NOT NULL COMMENT 'Confirm 时对 balance 的增量（负=减少，ISO最小单位×100）',
    `frozen_amount` BIGINT NOT NULL DEFAULT 0 COMMENT 'Try 时冻结的可用余额（0=无需冻结）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-TRYING 1-CONFIRMED 2-CANCELLED',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_branch_id` (`branch_id`),
    KEY `idx_tcc_id` (`tcc_id`),
    KEY `idx_account_no` (`account_no`),
    -- 复合索引：卡住 TCC 分支查询（status=TRYING AND created_at < cutoff），避免全表扫描
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 分布式事务分支记录表';

-- ============================================
-- 7b. UnfreezeAndDebit 补偿 outbox (freeze_compensate_outbox_xxx)
--
-- 与 frozen account 同分片（按 voucher_no 前 3 字符 = dbIdx + tableIdx 路由），
-- 保证 Phase 1 frozen-debit + INSERT outbox PENDING 在单库事务原子。
--
-- 状态机：
--   0 PENDING  : 已写入；可能被 caller mark DONE 或 worker 接管补偿
--   1 DONE     : 终态；caller Phase 2 全成功 或 worker 补偿全成功
--   2 FAILED   : 终态；worker 重试到达上限，需人工处理
--
-- payload (JSON) 包含完整 reverse plan：frozen account 信息 + 全部 credit
-- entries 信息 + 已成功的 credit txID 列表（worker 据此精确反向）。
--
-- worker 选取规则：status=PENDING AND created_at < now - 60s（给 caller
-- 一分钟时间走完 Phase 2 + 标记 DONE，避免误抢）；CAS UPDATE PENDING → COMPENSATING
-- 防多 pod / 多 worker 重复处理。
-- ============================================
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_21` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '关联的 unfreeze voucher（=路由键，前3字符为 dbIdx+tableIdx）',
    `freeze_order_no` VARCHAR(64) NOT NULL COMMENT '关联的 freeze_order 号（运维诊断用）',
    `payload` MEDIUMTEXT NOT NULL COMMENT 'JSON：完整 reverse plan（frozen acct + credit entries + 已成功 txIDs）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=DONE 2=COMPENSATING 3=FAILED',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '补偿重试次数；达 max_retry 置 FAILED',
    `error_msg` VARCHAR(500) DEFAULT NULL COMMENT '最近一次失败原因（FAILED 状态用于审计）',
    `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
    `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UnfreezeAndDebit 补偿 outbox（保证最终一致性）';

-- ============================================
-- 8. 分布式锁表 (distributed_lock)
-- 用于日切等场景的分布式锁
-- ============================================
CREATE TABLE IF NOT EXISTS `distributed_lock_21` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `lock_key` VARCHAR(128) NOT NULL COMMENT '锁键',
    `lock_value` VARCHAR(64) NOT NULL COMMENT '锁值（UUID）',
    `expire_time` DATETIME NOT NULL COMMENT '过期时间',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lock_key` (`lock_key`),
    KEY `idx_expire_time` (`expire_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='分布式锁表';


-- ============================================
-- 10. 商户信息表 (merchant_info)
-- ============================================
CREATE TABLE IF NOT EXISTS `merchant_info_21` (
    `id`                   BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `merchant_id`          BIGINT(20) UNSIGNED NOT NULL COMMENT '商户唯一ID',
    `merchant_name`        VARCHAR(256) NOT NULL COMMENT '商户名称',
    `account_idc`          VARCHAR(32)  NOT NULL COMMENT '商户账户所属 IDC',
    `external_merchant_id` VARCHAR(256) NOT NULL COMMENT '外部商户ID',
    `mid`                  VARCHAR(64)  NOT NULL COMMENT '业务线标识',
    `business_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务ID',
    `extra`                TEXT DEFAULT NULL,
    `byte_rds_ctx`         TEXT DEFAULT NULL,
    `create_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_merchant_id` (`merchant_id`),
    UNIQUE KEY `uniq_external_merchant_id_mid` (`external_merchant_id`, `mid`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户信息表';

-- ============================================
-- 11. 交易订单表 (transaction_order_xxx)
-- 分库分表：n = business_no(numeric) % 100; dbIdx = n/10; tableIdx = n
-- 幂等键：(order_no, business_type, business_no)
-- Extra 字段存储：预生成的 voucher_no、tx_ids、完整请求参数快照（reqParams）
-- ============================================
-- transaction_order 主表：状态字段 + 固定大小业务元数据（VARCHAR，行宽可控）
-- 大块 extra 字段独立存储到 transaction_order_extra（见下文），热路径不访问 extra
CREATE TABLE IF NOT EXISTS `transaction_order_21` (
    `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`       VARCHAR(64)  NOT NULL COMMENT '外部幂等订单号（requestId）',
    `business_no`    VARCHAR(64)  NOT NULL COMMENT '业务订单号（分片键，按后2位路由）',
    `business_type`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '业务类型',
    `product_code`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '产品编码',
    `event_code`     VARCHAR(256) NOT NULL DEFAULT '' COMMENT '事件编码',
    `from_party_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '资金出方 owner id',
    `from_party_type` VARCHAR(32) NOT NULL DEFAULT '' COMMENT 'user | merchant | platform',
    `to_party_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '资金入方 owner id',
    `to_party_type`  VARCHAR(32)  NOT NULL DEFAULT '',
    `amount`         VARCHAR(32)  NOT NULL DEFAULT '0' COMMENT '金额（字符串存储）',
    `currency`       VARCHAR(8)   NOT NULL DEFAULT 'PHP',
    `status`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `retry_count`    INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `max_retry_count` INT         NOT NULL DEFAULT 3 COMMENT '最大允许重试次数',
    `voucher_no`     VARCHAR(64)  DEFAULT NULL COMMENT '记账凭证号（成功后写入）',
    `error_message`  VARCHAR(512) DEFAULT NULL COMMENT '最近一次失败原因',
    `description`    VARCHAR(256) DEFAULT NULL,
    `created_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_biz` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单主表（分库分表）—— extra 独立存储提升缓存命中率';

-- ============================================
-- 12. 交易订单扩展字段表 (transaction_order_extra_xxx)
-- 与 transaction_order_xxx 同分片（按 business_no 路由）
-- 存储：请求参数快照、预生成 ID（req_hash/voucher_no/tx_ids/req_params）
-- 热路径（状态查询/更新）不访问此表；仅创建/重试/审计时读写
-- extra VARCHAR(4096)：4096字符上限，代码层写入前强制截断
-- ============================================
CREATE TABLE IF NOT EXISTS `transaction_order_extra_21` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`   VARCHAR(64)  NOT NULL COMMENT '关联 transaction_order.order_no',
    `business_no` VARCHAR(64) NOT NULL COMMENT '分片键（与主表路由一致）',
    `business_type` VARCHAR(32) NOT NULL DEFAULT '',
    `extra`      VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON: {req_hash,voucher_no,tx_ids,req_params}，最长4096字符',
    `created_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_extra` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单扩展字段表（与主表同分片，热路径不访问）';


-- ============================================
-- 13. 缓冲余额更新表 (account_balance_buffer_xxx)
-- 与账户主表同分片（按 account_no 路由），用于平台/中间账户的批量余额聚合写入。
-- 设计：平台/中间账户允许负余额，写流水后先不更新 account.balance，
--        而是在此表累计增量；后台 flush worker 每100条或每30秒批量更新 account 余额，
--        减少高频账户对 account 表的锁竞争。
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_buffer_21` (
    `account_no`       VARCHAR(64)   NOT NULL COMMENT '账户号',
    `pending_delta`    BIGINT NOT NULL DEFAULT 0 COMMENT '待刷新的余额增量（可为负，ISO最小单位×100）',
    `pending_count`    INT           NOT NULL DEFAULT 0 COMMENT '待刷新的流水笔数',
    `flush_scheduled_at` DATETIME(3) NULL DEFAULT NULL COMMENT '下次计划刷新时间（NULL=立即刷新；worker 按此时间触发，执行后自动延后一个间隔）',
    `last_updated_at`  DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最后写入时间',
    PRIMARY KEY (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额缓冲聚合表（平台/中间账户缓冲记账）';

-- ============================================
-- TCC 全局协调者表 (tcc_coordinator_xxx)
-- 分库 + 分表：按 tcc_id 数字 hash(tcc_id) 路由到 (dbIndex, globalTableIndex)，
-- 与 account_transaction / tcc_transaction 路由规则一致，保证同一笔 booking 所有
-- 相关记录共享分片，避免跨分片事务。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_coordinator_21` (
    `tcc_id`        VARCHAR(64)  NOT NULL COMMENT 'TCC 全局事务 ID（= voucher_no）',
    `phase`         TINYINT      NOT NULL DEFAULT 1 COMMENT '全局阶段：1=TRYING 2=CONFIRMING 3=CONFIRMED 4=CANCELLED',
    `business_no`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '业务订单号',
    `branch_count`  SMALLINT     NOT NULL COMMENT '总分支数',
    `cut_date`      CHAR(10)     NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，避免 gorm parseTime 1 天位移 bug）。booking 入口处 computeCutDate 计算后 propagate；与 account_transaction.cut_date 一致。日切 drain 等待: WHERE cut_date <= X AND phase IN (TRYING, CONFIRMING) → 0',
    `currency`      VARCHAR(8)   NOT NULL DEFAULT '' COMMENT '币种（PHP/CNY/USD/...）。booking 入口写入；TCC Recovery 用 coord.currency 写补 confirm 流水，避免与原 booking 不同币种导致试算 PHP-only / currency-filter 不平。空字符串=旧数据兼容',
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`tcc_id`),
    KEY `idx_phase_updated` (`phase`, `updated_at`),
    KEY `idx_cut_date_phase` (`cut_date`, `phase`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 全局协调者（阶段追踪，100 张分表）';

-- ============================================
-- 索引优化说明
-- ============================================
-- 1. 所有分表都需要创建相同的表结构和索引
-- 2. 查询时优先使用分片键（user_id, account_no）
-- 3. 时间范围查询使用 transaction_date 索引
-- 4. 定期归档历史数据，保持表数据量在合理范围

-- ============================================
-- 13. 原子批量记账订单表 (batch_order)
-- 全局单表（不分库分表），记录原子批量记账的批次状态
-- 批次内各单笔记账通过 TCC 保证整体原子性：全部成功 or 全部回滚
-- extra VARCHAR(4096)：存储批次请求参数快照，最长4096字符
-- ============================================
CREATE TABLE IF NOT EXISTS `batch_order_21` (
    `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `batch_id`        VARCHAR(64)  NOT NULL COMMENT '批次幂等ID（调用方提供）',
    `business_no`     VARCHAR(64)  NOT NULL COMMENT '批次业务号',
    `item_count`      INT          NOT NULL DEFAULT 0 COMMENT '批次中单笔数量',
    `status`          TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `error_message`   VARCHAR(512) DEFAULT NULL COMMENT '失败原因',
    `description`     VARCHAR(256) DEFAULT NULL,
    `extra`           VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON批次请求参数快照，最长4096字符',
    `created_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_batch_id` (`batch_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='原子批量记账订单表（全局表）';

-- ============================================
-- 12. 结算 Outbox 表 (settlement_outbox)
-- 热路径事务预写日志（Transactional Outbox Pattern）
-- 分库，不分表：按 voucher_no 前1字符（dbIndex）路由，每库一张 settlement_outbox 表
-- ============================================
CREATE TABLE IF NOT EXISTS `settlement_outbox_21` (
    `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no`       VARCHAR(64)  NOT NULL COMMENT '凭证号（前3字符={1d-dbIdx}{2d-tableIdx}，用于路由）',
    `event_data`       LONGTEXT     NOT NULL COMMENT 'JSON: SettlementEvent',
    `transaction_date` CHAR(10)     NOT NULL COMMENT '交易日期 YYYY-MM-DD，供日切按天过滤 outbox delta',
    `cut_date`         CHAR(10)     NOT NULL DEFAULT '' COMMENT '日切归属日 YYYY-MM-DD（入口落库那一刻定死，永不重算）；空字符串=老数据按 transaction_date 兼容处理',
    `status`           TINYINT      NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=REDIS_DONE 2=MYSQL_DONE 3=FAILED',
    `retry_count`      INT          NOT NULL DEFAULT 0 COMMENT '重试次数',
    `error_msg`        TEXT         DEFAULT NULL COMMENT '最近一次失败原因',
    `created_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_date` (`status`, `transaction_date`),
    KEY `idx_status_created` (`status`, `created_at`),
    -- OutboxWorker MySQLWriter 高频轮询查询：
    --   SELECT * FROM settlement_outbox WHERE status = ? ORDER BY id ASC LIMIT 500
    -- (status, id) 复合索引让此查询走"索引范围扫描 + 已序输出"，免去 filesort。
    -- InnoDB 二级索引天然把 PK id 附在叶节点，但只有把 id 显式作为索引第二列，
    -- 优化器才会用 index range scan 满足 ORDER BY id；否则可能选 idx_status 后 sort。
    KEY `idx_status_id` (`status`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='热路径事务预写日志（Outbox）';

-- ============================================
-- 复式记账账户系统 - 数据库表设计
-- 分库分表架构：10库 × 每库10表 = 100张全局表
--
-- 统一路由规则：
--   n = id % 100
--   dbIndex        = n / 10      （物理库编号 0–9）
--   globalTableIdx = n           （全局表序号 0–99）
--
-- 表名映射（以 account 为例）：
--   DB 0 → account_00 ~ account_09
--   DB 1 → account_10 ~ account_19
--   ...
--   DB 9 → account_90 ~ account_99
-- ============================================

-- ============================================
-- 1. 账户主表 (account_xxx)
-- 分库分表：n = user_id % 100; dbIdx = n/10; tableIdx = n
-- ============================================


CREATE TABLE IF NOT EXISTS `account_22` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号（格式：{dbIdx:02d}{tableIdx:02d}{userId}-{businessType:03d}，编码路由信息）',
    `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
    `account_type` TINYINT NOT NULL COMMENT '账户类型：1-用户账户 2-商户账户 3-平台损益账户 4-中间账户',
    `account_category` VARCHAR(32) NOT NULL COMMENT '账户分类：ASSET资产/LIABILITY负债/EQUITY所有者权益/REVENUE收入/EXPENSE费用',
    `account_business_type` SMALLINT NOT NULL DEFAULT 0 COMMENT '账户业务类型：1-用户余额 2-商户结算 3-商户待结算（最大999）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `balance` BIGINT NOT NULL DEFAULT 0 COMMENT '当前余额（ISO最小单位×100，例如USD $3.42存为34200）',
    `frozen_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '冻结余额',
    `available_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '可用余额',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-禁用 1-正常 2-冻结',
    `account_group` CHAR(1) NOT NULL DEFAULT 'A' COMMENT '账户分组：A=默认/当前；B=轮换预创建的下一组。非轮换账户永远是 A',
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    -- 轮换字段（rotating-suspense-accounts 特性，见 docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md）
    -- 旧账户 logical_account_id=NULL 且 lifecycle_phase=0 (legacy)，写入路径与原行为一致。
    `logical_account_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '关联 logical_account.id；NULL=legacy 账户',
    `period_start` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期起点（包含），UTC',
    `period_end` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期终点（不含），UTC',
    `lifecycle_phase` TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=active 2=draining 3=frozen 4=archived 5=provisioned 9=quarantined',
    `draining_started_at` DATETIME DEFAULT NULL COMMENT '进入 draining 的时刻',
    `frozen_at` DATETIME DEFAULT NULL COMMENT '进入 frozen 的时刻',
    `archived_at` DATETIME DEFAULT NULL COMMENT '进入 archived 的时刻',
    `policy_version_at_birth` BIGINT DEFAULT NULL COMMENT '出生时锁定的 policy_version；后续策略变更不影响本 instance (E-28/E-29)',
    `effective_hard_timeout_secs` INT DEFAULT NULL COMMENT '策略覆盖：本 instance 专用 hard timeout (§11.X 紧急运维)',
    `override_reason` VARCHAR(255) DEFAULT NULL COMMENT 'override 原因',
    `override_by` VARCHAR(64) DEFAULT NULL COMMENT 'override 操作人',
    `override_at` DATETIME DEFAULT NULL COMMENT 'override 时刻',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`, `account_group`) COMMENT 'userId + 业务类型 + 币种 + 组 唯一键 (轮换 fleet 时 GroupA / GroupB 各 1 个)',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`),
    KEY `idx_logical_phase` (`logical_account_id`, `lifecycle_phase`),
    KEY `idx_phase_period_end` (`lifecycle_phase`, `period_end`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_22` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `transaction_id` VARCHAR(64) NOT NULL COMMENT '交易流水号',
    `parent_transaction_id` VARCHAR(64) DEFAULT NULL COMMENT '父交易流水号（用于关联复式记账的多条记录）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型：TRANSFER/PAYMENT/REFUND/WITHDRAW等',
    `debit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '借方金额（ISO最小单位×100）',
    `credit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '贷方金额（ISO最小单位×100）',
    `balance_before` BIGINT NOT NULL COMMENT '交易前余额',
    `balance_after` BIGINT NOT NULL COMMENT '交易后余额',
    `booking_type` TINYINT NOT NULL DEFAULT 1 COMMENT '记账方式：1-同步记账 2-缓冲记账',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `transaction_date` CHAR(10) NOT NULL COMMENT '交易日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑；ISO 格式按字典序与 DATE 同序，索引/范围查询无影响）',
    `transaction_time` DATETIME NOT NULL COMMENT '交易时间',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '交易描述',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-失败 1-成功 2-处理中',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，原因：DSN parseTime 让 gorm 做 TZ 转换 → 1 天位移 bug）。在 booking 入口由 computeCutDate(now, scheduled_time, tz) 一次性确定，propagate 到所有 entry + tcc_coordinator。日切扫描 WHERE cut_date=X AND status=1，同 voucher 所有 entry 共享同一 cut_date → 试算平衡天然成立',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `ext_info` JSON DEFAULT NULL COMMENT '扩展信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_transaction_id` (`transaction_id`),
    KEY `idx_account_no` (`account_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_parent_transaction_id` (`parent_transaction_id`),
    KEY `idx_status` (`status`),
    -- 复合索引：账户流水分页查询（account_no + 日期范围 + 时间排序），取代独立的 idx_account_no + idx_transaction_date
    KEY `idx_account_date_time` (`account_no`, `transaction_date`, `transaction_time`),
    -- 日切扫描主索引：WHERE cut_date=X AND status=1
    KEY `idx_cut_date_status` (`cut_date`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户流水表';

-- ============================================
-- 3. 复式记账凭证表 (accounting_voucher_xxx)
-- 一笔业务对应一张凭证，凭证关联多条分录
-- 分库分表：按 voucher_no 前3字符路由（与 business_no 同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `accounting_voucher_22` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '凭证号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型',
    `total_debit` BIGINT NOT NULL COMMENT '借方总金额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL COMMENT '贷方总金额（ISO最小单位×100）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `voucher_date` CHAR(10) NOT NULL COMMENT '凭证日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-作废 1-生效 2-待审核',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '凭证描述',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_voucher_date` (`voucher_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='复式记账凭证表';

-- ============================================
-- 4. 账户余额快照表 (account_balance_snapshot_xxx)
-- 每日日切生成快照
-- 分库分表：按 account_no 前3字符路由（与账户主表同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_22` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `snapshot_date` CHAR(10) NOT NULL COMMENT '快照日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '关联日切版本号，重跑时递增',
    `beginning_balance` BIGINT NOT NULL COMMENT '期初余额（ISO最小单位×100）',
    `ending_balance` BIGINT NOT NULL COMMENT '期末余额（ISO最小单位×100）',
    `total_debit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日借方总额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日贷方总额（ISO最小单位×100）',
    `transaction_count` INT NOT NULL DEFAULT 0 COMMENT '当日交易笔数',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '该账户本日最后一笔流水ID，仅供审计/对账使用',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_snapshot` (`account_no`, `snapshot_date`, `run_id`),
    KEY `idx_snapshot_date` (`snapshot_date`),
    KEY `idx_snapshot_date_run` (`snapshot_date`, `run_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额快照表';

-- ============================================
-- 5. 日切控制表 (day_cut_control)
-- 全局表，不分库分表，记录每个库每张表的日切点
-- ============================================
CREATE TABLE IF NOT EXISTS `day_cut_control_22` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `database_index` TINYINT NOT NULL COMMENT '库索引 0-9',
    `table_index` SMALLINT NOT NULL COMMENT '表索引 0-99',
    `table_name` VARCHAR(64) NOT NULL COMMENT '表名',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 避开 DATE TZ 转换坑）。扫描走 account_transaction.cut_date = 本字段',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '运行版本号，同一日期每次重跑时递增',
    `last_processed_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '本 run 在分片上已处理到的 account_transaction.id（含）。chunk 内分页游标',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '本 run 完成时最后处理的 transaction_id，仅供审计',
    `currency` CHAR(3) NOT NULL DEFAULT '' COMMENT '币种过滤。空 = 全部币种（兼容历史）；非空 = 仅该币种账户',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-未开始 1-进行中 2-已完成 3-失败',
    `start_time` DATETIME DEFAULT NULL COMMENT '开始时间',
    `end_time` DATETIME DEFAULT NULL COMMENT '结束时间',
    `cut_time` DATETIME(3) DEFAULT NULL COMMENT '日切完成时的服务器墙钟时间，仅供审计',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_day_cut` (`database_index`, `table_index`, `cut_date`, `run_id`),
    KEY `idx_cut_date` (`cut_date`),
    KEY `idx_cut_date_run` (`cut_date`, `run_id`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='日切控制表';

-- ============================================
-- 6. 异步任务表 (async_task)
-- 用于Kafka消息消费失败后的重试
-- 全局表，不分库分表
-- ============================================
CREATE TABLE IF NOT EXISTS `async_task_22` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `task_id` VARCHAR(64) NOT NULL COMMENT '任务ID',
    `task_type` VARCHAR(32) NOT NULL COMMENT '任务类型：ACCOUNTING/SNAPSHOT/DAY_CUT',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `task_data` JSON NOT NULL COMMENT '任务数据',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-待处理 1-处理中 2-成功 3-失败',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `max_retry_count` INT NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `next_retry_time` DATETIME DEFAULT NULL COMMENT '下次重试时间',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status_retry` (`status`, `next_retry_time`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='异步任务表';

-- ============================================
-- 7. TCC 分支记录表 (tcc_transaction_xxx)
-- 与账户表同分片（按 account_no 路由），保证 Try/Confirm/Cancel 在单库事务内完成。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_transaction_22` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `tcc_id` VARCHAR(64) NOT NULL COMMENT '全局 TCC ID（= voucherNo）',
    `branch_id` VARCHAR(64) NOT NULL COMMENT 'TCC 分支 ID（= transactionID）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `balance_delta` BIGINT NOT NULL COMMENT 'Confirm 时对 balance 的增量（负=减少，ISO最小单位×100）',
    `frozen_amount` BIGINT NOT NULL DEFAULT 0 COMMENT 'Try 时冻结的可用余额（0=无需冻结）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-TRYING 1-CONFIRMED 2-CANCELLED',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_branch_id` (`branch_id`),
    KEY `idx_tcc_id` (`tcc_id`),
    KEY `idx_account_no` (`account_no`),
    -- 复合索引：卡住 TCC 分支查询（status=TRYING AND created_at < cutoff），避免全表扫描
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 分布式事务分支记录表';

-- ============================================
-- 7b. UnfreezeAndDebit 补偿 outbox (freeze_compensate_outbox_xxx)
--
-- 与 frozen account 同分片（按 voucher_no 前 3 字符 = dbIdx + tableIdx 路由），
-- 保证 Phase 1 frozen-debit + INSERT outbox PENDING 在单库事务原子。
--
-- 状态机：
--   0 PENDING  : 已写入；可能被 caller mark DONE 或 worker 接管补偿
--   1 DONE     : 终态；caller Phase 2 全成功 或 worker 补偿全成功
--   2 FAILED   : 终态；worker 重试到达上限，需人工处理
--
-- payload (JSON) 包含完整 reverse plan：frozen account 信息 + 全部 credit
-- entries 信息 + 已成功的 credit txID 列表（worker 据此精确反向）。
--
-- worker 选取规则：status=PENDING AND created_at < now - 60s（给 caller
-- 一分钟时间走完 Phase 2 + 标记 DONE，避免误抢）；CAS UPDATE PENDING → COMPENSATING
-- 防多 pod / 多 worker 重复处理。
-- ============================================
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_22` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '关联的 unfreeze voucher（=路由键，前3字符为 dbIdx+tableIdx）',
    `freeze_order_no` VARCHAR(64) NOT NULL COMMENT '关联的 freeze_order 号（运维诊断用）',
    `payload` MEDIUMTEXT NOT NULL COMMENT 'JSON：完整 reverse plan（frozen acct + credit entries + 已成功 txIDs）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=DONE 2=COMPENSATING 3=FAILED',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '补偿重试次数；达 max_retry 置 FAILED',
    `error_msg` VARCHAR(500) DEFAULT NULL COMMENT '最近一次失败原因（FAILED 状态用于审计）',
    `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
    `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UnfreezeAndDebit 补偿 outbox（保证最终一致性）';

-- ============================================
-- 8. 分布式锁表 (distributed_lock)
-- 用于日切等场景的分布式锁
-- ============================================
CREATE TABLE IF NOT EXISTS `distributed_lock_22` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `lock_key` VARCHAR(128) NOT NULL COMMENT '锁键',
    `lock_value` VARCHAR(64) NOT NULL COMMENT '锁值（UUID）',
    `expire_time` DATETIME NOT NULL COMMENT '过期时间',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lock_key` (`lock_key`),
    KEY `idx_expire_time` (`expire_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='分布式锁表';


-- ============================================
-- 10. 商户信息表 (merchant_info)
-- ============================================
CREATE TABLE IF NOT EXISTS `merchant_info_22` (
    `id`                   BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `merchant_id`          BIGINT(20) UNSIGNED NOT NULL COMMENT '商户唯一ID',
    `merchant_name`        VARCHAR(256) NOT NULL COMMENT '商户名称',
    `account_idc`          VARCHAR(32)  NOT NULL COMMENT '商户账户所属 IDC',
    `external_merchant_id` VARCHAR(256) NOT NULL COMMENT '外部商户ID',
    `mid`                  VARCHAR(64)  NOT NULL COMMENT '业务线标识',
    `business_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务ID',
    `extra`                TEXT DEFAULT NULL,
    `byte_rds_ctx`         TEXT DEFAULT NULL,
    `create_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_merchant_id` (`merchant_id`),
    UNIQUE KEY `uniq_external_merchant_id_mid` (`external_merchant_id`, `mid`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户信息表';

-- ============================================
-- 11. 交易订单表 (transaction_order_xxx)
-- 分库分表：n = business_no(numeric) % 100; dbIdx = n/10; tableIdx = n
-- 幂等键：(order_no, business_type, business_no)
-- Extra 字段存储：预生成的 voucher_no、tx_ids、完整请求参数快照（reqParams）
-- ============================================
-- transaction_order 主表：状态字段 + 固定大小业务元数据（VARCHAR，行宽可控）
-- 大块 extra 字段独立存储到 transaction_order_extra（见下文），热路径不访问 extra
CREATE TABLE IF NOT EXISTS `transaction_order_22` (
    `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`       VARCHAR(64)  NOT NULL COMMENT '外部幂等订单号（requestId）',
    `business_no`    VARCHAR(64)  NOT NULL COMMENT '业务订单号（分片键，按后2位路由）',
    `business_type`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '业务类型',
    `product_code`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '产品编码',
    `event_code`     VARCHAR(256) NOT NULL DEFAULT '' COMMENT '事件编码',
    `from_party_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '资金出方 owner id',
    `from_party_type` VARCHAR(32) NOT NULL DEFAULT '' COMMENT 'user | merchant | platform',
    `to_party_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '资金入方 owner id',
    `to_party_type`  VARCHAR(32)  NOT NULL DEFAULT '',
    `amount`         VARCHAR(32)  NOT NULL DEFAULT '0' COMMENT '金额（字符串存储）',
    `currency`       VARCHAR(8)   NOT NULL DEFAULT 'PHP',
    `status`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `retry_count`    INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `max_retry_count` INT         NOT NULL DEFAULT 3 COMMENT '最大允许重试次数',
    `voucher_no`     VARCHAR(64)  DEFAULT NULL COMMENT '记账凭证号（成功后写入）',
    `error_message`  VARCHAR(512) DEFAULT NULL COMMENT '最近一次失败原因',
    `description`    VARCHAR(256) DEFAULT NULL,
    `created_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_biz` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单主表（分库分表）—— extra 独立存储提升缓存命中率';

-- ============================================
-- 12. 交易订单扩展字段表 (transaction_order_extra_xxx)
-- 与 transaction_order_xxx 同分片（按 business_no 路由）
-- 存储：请求参数快照、预生成 ID（req_hash/voucher_no/tx_ids/req_params）
-- 热路径（状态查询/更新）不访问此表；仅创建/重试/审计时读写
-- extra VARCHAR(4096)：4096字符上限，代码层写入前强制截断
-- ============================================
CREATE TABLE IF NOT EXISTS `transaction_order_extra_22` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`   VARCHAR(64)  NOT NULL COMMENT '关联 transaction_order.order_no',
    `business_no` VARCHAR(64) NOT NULL COMMENT '分片键（与主表路由一致）',
    `business_type` VARCHAR(32) NOT NULL DEFAULT '',
    `extra`      VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON: {req_hash,voucher_no,tx_ids,req_params}，最长4096字符',
    `created_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_extra` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单扩展字段表（与主表同分片，热路径不访问）';


-- ============================================
-- 13. 缓冲余额更新表 (account_balance_buffer_xxx)
-- 与账户主表同分片（按 account_no 路由），用于平台/中间账户的批量余额聚合写入。
-- 设计：平台/中间账户允许负余额，写流水后先不更新 account.balance，
--        而是在此表累计增量；后台 flush worker 每100条或每30秒批量更新 account 余额，
--        减少高频账户对 account 表的锁竞争。
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_buffer_22` (
    `account_no`       VARCHAR(64)   NOT NULL COMMENT '账户号',
    `pending_delta`    BIGINT NOT NULL DEFAULT 0 COMMENT '待刷新的余额增量（可为负，ISO最小单位×100）',
    `pending_count`    INT           NOT NULL DEFAULT 0 COMMENT '待刷新的流水笔数',
    `flush_scheduled_at` DATETIME(3) NULL DEFAULT NULL COMMENT '下次计划刷新时间（NULL=立即刷新；worker 按此时间触发，执行后自动延后一个间隔）',
    `last_updated_at`  DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最后写入时间',
    PRIMARY KEY (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额缓冲聚合表（平台/中间账户缓冲记账）';

-- ============================================
-- TCC 全局协调者表 (tcc_coordinator_xxx)
-- 分库 + 分表：按 tcc_id 数字 hash(tcc_id) 路由到 (dbIndex, globalTableIndex)，
-- 与 account_transaction / tcc_transaction 路由规则一致，保证同一笔 booking 所有
-- 相关记录共享分片，避免跨分片事务。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_coordinator_22` (
    `tcc_id`        VARCHAR(64)  NOT NULL COMMENT 'TCC 全局事务 ID（= voucher_no）',
    `phase`         TINYINT      NOT NULL DEFAULT 1 COMMENT '全局阶段：1=TRYING 2=CONFIRMING 3=CONFIRMED 4=CANCELLED',
    `business_no`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '业务订单号',
    `branch_count`  SMALLINT     NOT NULL COMMENT '总分支数',
    `cut_date`      CHAR(10)     NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，避免 gorm parseTime 1 天位移 bug）。booking 入口处 computeCutDate 计算后 propagate；与 account_transaction.cut_date 一致。日切 drain 等待: WHERE cut_date <= X AND phase IN (TRYING, CONFIRMING) → 0',
    `currency`      VARCHAR(8)   NOT NULL DEFAULT '' COMMENT '币种（PHP/CNY/USD/...）。booking 入口写入；TCC Recovery 用 coord.currency 写补 confirm 流水，避免与原 booking 不同币种导致试算 PHP-only / currency-filter 不平。空字符串=旧数据兼容',
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`tcc_id`),
    KEY `idx_phase_updated` (`phase`, `updated_at`),
    KEY `idx_cut_date_phase` (`cut_date`, `phase`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 全局协调者（阶段追踪，100 张分表）';

-- ============================================
-- 索引优化说明
-- ============================================
-- 1. 所有分表都需要创建相同的表结构和索引
-- 2. 查询时优先使用分片键（user_id, account_no）
-- 3. 时间范围查询使用 transaction_date 索引
-- 4. 定期归档历史数据，保持表数据量在合理范围

-- ============================================
-- 13. 原子批量记账订单表 (batch_order)
-- 全局单表（不分库分表），记录原子批量记账的批次状态
-- 批次内各单笔记账通过 TCC 保证整体原子性：全部成功 or 全部回滚
-- extra VARCHAR(4096)：存储批次请求参数快照，最长4096字符
-- ============================================
CREATE TABLE IF NOT EXISTS `batch_order_22` (
    `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `batch_id`        VARCHAR(64)  NOT NULL COMMENT '批次幂等ID（调用方提供）',
    `business_no`     VARCHAR(64)  NOT NULL COMMENT '批次业务号',
    `item_count`      INT          NOT NULL DEFAULT 0 COMMENT '批次中单笔数量',
    `status`          TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `error_message`   VARCHAR(512) DEFAULT NULL COMMENT '失败原因',
    `description`     VARCHAR(256) DEFAULT NULL,
    `extra`           VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON批次请求参数快照，最长4096字符',
    `created_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_batch_id` (`batch_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='原子批量记账订单表（全局表）';

-- ============================================
-- 12. 结算 Outbox 表 (settlement_outbox)
-- 热路径事务预写日志（Transactional Outbox Pattern）
-- 分库，不分表：按 voucher_no 前1字符（dbIndex）路由，每库一张 settlement_outbox 表
-- ============================================
CREATE TABLE IF NOT EXISTS `settlement_outbox_22` (
    `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no`       VARCHAR(64)  NOT NULL COMMENT '凭证号（前3字符={1d-dbIdx}{2d-tableIdx}，用于路由）',
    `event_data`       LONGTEXT     NOT NULL COMMENT 'JSON: SettlementEvent',
    `transaction_date` CHAR(10)     NOT NULL COMMENT '交易日期 YYYY-MM-DD，供日切按天过滤 outbox delta',
    `cut_date`         CHAR(10)     NOT NULL DEFAULT '' COMMENT '日切归属日 YYYY-MM-DD（入口落库那一刻定死，永不重算）；空字符串=老数据按 transaction_date 兼容处理',
    `status`           TINYINT      NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=REDIS_DONE 2=MYSQL_DONE 3=FAILED',
    `retry_count`      INT          NOT NULL DEFAULT 0 COMMENT '重试次数',
    `error_msg`        TEXT         DEFAULT NULL COMMENT '最近一次失败原因',
    `created_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_date` (`status`, `transaction_date`),
    KEY `idx_status_created` (`status`, `created_at`),
    -- OutboxWorker MySQLWriter 高频轮询查询：
    --   SELECT * FROM settlement_outbox WHERE status = ? ORDER BY id ASC LIMIT 500
    -- (status, id) 复合索引让此查询走"索引范围扫描 + 已序输出"，免去 filesort。
    -- InnoDB 二级索引天然把 PK id 附在叶节点，但只有把 id 显式作为索引第二列，
    -- 优化器才会用 index range scan 满足 ORDER BY id；否则可能选 idx_status 后 sort。
    KEY `idx_status_id` (`status`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='热路径事务预写日志（Outbox）';

-- ============================================
-- 复式记账账户系统 - 数据库表设计
-- 分库分表架构：10库 × 每库10表 = 100张全局表
--
-- 统一路由规则：
--   n = id % 100
--   dbIndex        = n / 10      （物理库编号 0–9）
--   globalTableIdx = n           （全局表序号 0–99）
--
-- 表名映射（以 account 为例）：
--   DB 0 → account_00 ~ account_09
--   DB 1 → account_10 ~ account_19
--   ...
--   DB 9 → account_90 ~ account_99
-- ============================================

-- ============================================
-- 1. 账户主表 (account_xxx)
-- 分库分表：n = user_id % 100; dbIdx = n/10; tableIdx = n
-- ============================================


CREATE TABLE IF NOT EXISTS `account_23` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号（格式：{dbIdx:02d}{tableIdx:02d}{userId}-{businessType:03d}，编码路由信息）',
    `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
    `account_type` TINYINT NOT NULL COMMENT '账户类型：1-用户账户 2-商户账户 3-平台损益账户 4-中间账户',
    `account_category` VARCHAR(32) NOT NULL COMMENT '账户分类：ASSET资产/LIABILITY负债/EQUITY所有者权益/REVENUE收入/EXPENSE费用',
    `account_business_type` SMALLINT NOT NULL DEFAULT 0 COMMENT '账户业务类型：1-用户余额 2-商户结算 3-商户待结算（最大999）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `balance` BIGINT NOT NULL DEFAULT 0 COMMENT '当前余额（ISO最小单位×100，例如USD $3.42存为34200）',
    `frozen_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '冻结余额',
    `available_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '可用余额',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-禁用 1-正常 2-冻结',
    `account_group` CHAR(1) NOT NULL DEFAULT 'A' COMMENT '账户分组：A=默认/当前；B=轮换预创建的下一组。非轮换账户永远是 A',
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    -- 轮换字段（rotating-suspense-accounts 特性，见 docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md）
    -- 旧账户 logical_account_id=NULL 且 lifecycle_phase=0 (legacy)，写入路径与原行为一致。
    `logical_account_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '关联 logical_account.id；NULL=legacy 账户',
    `period_start` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期起点（包含），UTC',
    `period_end` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期终点（不含），UTC',
    `lifecycle_phase` TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=active 2=draining 3=frozen 4=archived 5=provisioned 9=quarantined',
    `draining_started_at` DATETIME DEFAULT NULL COMMENT '进入 draining 的时刻',
    `frozen_at` DATETIME DEFAULT NULL COMMENT '进入 frozen 的时刻',
    `archived_at` DATETIME DEFAULT NULL COMMENT '进入 archived 的时刻',
    `policy_version_at_birth` BIGINT DEFAULT NULL COMMENT '出生时锁定的 policy_version；后续策略变更不影响本 instance (E-28/E-29)',
    `effective_hard_timeout_secs` INT DEFAULT NULL COMMENT '策略覆盖：本 instance 专用 hard timeout (§11.X 紧急运维)',
    `override_reason` VARCHAR(255) DEFAULT NULL COMMENT 'override 原因',
    `override_by` VARCHAR(64) DEFAULT NULL COMMENT 'override 操作人',
    `override_at` DATETIME DEFAULT NULL COMMENT 'override 时刻',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`, `account_group`) COMMENT 'userId + 业务类型 + 币种 + 组 唯一键 (轮换 fleet 时 GroupA / GroupB 各 1 个)',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`),
    KEY `idx_logical_phase` (`logical_account_id`, `lifecycle_phase`),
    KEY `idx_phase_period_end` (`lifecycle_phase`, `period_end`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_23` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `transaction_id` VARCHAR(64) NOT NULL COMMENT '交易流水号',
    `parent_transaction_id` VARCHAR(64) DEFAULT NULL COMMENT '父交易流水号（用于关联复式记账的多条记录）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型：TRANSFER/PAYMENT/REFUND/WITHDRAW等',
    `debit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '借方金额（ISO最小单位×100）',
    `credit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '贷方金额（ISO最小单位×100）',
    `balance_before` BIGINT NOT NULL COMMENT '交易前余额',
    `balance_after` BIGINT NOT NULL COMMENT '交易后余额',
    `booking_type` TINYINT NOT NULL DEFAULT 1 COMMENT '记账方式：1-同步记账 2-缓冲记账',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `transaction_date` CHAR(10) NOT NULL COMMENT '交易日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑；ISO 格式按字典序与 DATE 同序，索引/范围查询无影响）',
    `transaction_time` DATETIME NOT NULL COMMENT '交易时间',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '交易描述',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-失败 1-成功 2-处理中',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，原因：DSN parseTime 让 gorm 做 TZ 转换 → 1 天位移 bug）。在 booking 入口由 computeCutDate(now, scheduled_time, tz) 一次性确定，propagate 到所有 entry + tcc_coordinator。日切扫描 WHERE cut_date=X AND status=1，同 voucher 所有 entry 共享同一 cut_date → 试算平衡天然成立',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `ext_info` JSON DEFAULT NULL COMMENT '扩展信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_transaction_id` (`transaction_id`),
    KEY `idx_account_no` (`account_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_parent_transaction_id` (`parent_transaction_id`),
    KEY `idx_status` (`status`),
    -- 复合索引：账户流水分页查询（account_no + 日期范围 + 时间排序），取代独立的 idx_account_no + idx_transaction_date
    KEY `idx_account_date_time` (`account_no`, `transaction_date`, `transaction_time`),
    -- 日切扫描主索引：WHERE cut_date=X AND status=1
    KEY `idx_cut_date_status` (`cut_date`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户流水表';

-- ============================================
-- 3. 复式记账凭证表 (accounting_voucher_xxx)
-- 一笔业务对应一张凭证，凭证关联多条分录
-- 分库分表：按 voucher_no 前3字符路由（与 business_no 同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `accounting_voucher_23` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '凭证号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型',
    `total_debit` BIGINT NOT NULL COMMENT '借方总金额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL COMMENT '贷方总金额（ISO最小单位×100）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `voucher_date` CHAR(10) NOT NULL COMMENT '凭证日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-作废 1-生效 2-待审核',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '凭证描述',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_voucher_date` (`voucher_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='复式记账凭证表';

-- ============================================
-- 4. 账户余额快照表 (account_balance_snapshot_xxx)
-- 每日日切生成快照
-- 分库分表：按 account_no 前3字符路由（与账户主表同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_23` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `snapshot_date` CHAR(10) NOT NULL COMMENT '快照日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '关联日切版本号，重跑时递增',
    `beginning_balance` BIGINT NOT NULL COMMENT '期初余额（ISO最小单位×100）',
    `ending_balance` BIGINT NOT NULL COMMENT '期末余额（ISO最小单位×100）',
    `total_debit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日借方总额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日贷方总额（ISO最小单位×100）',
    `transaction_count` INT NOT NULL DEFAULT 0 COMMENT '当日交易笔数',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '该账户本日最后一笔流水ID，仅供审计/对账使用',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_snapshot` (`account_no`, `snapshot_date`, `run_id`),
    KEY `idx_snapshot_date` (`snapshot_date`),
    KEY `idx_snapshot_date_run` (`snapshot_date`, `run_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额快照表';

-- ============================================
-- 5. 日切控制表 (day_cut_control)
-- 全局表，不分库分表，记录每个库每张表的日切点
-- ============================================
CREATE TABLE IF NOT EXISTS `day_cut_control_23` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `database_index` TINYINT NOT NULL COMMENT '库索引 0-9',
    `table_index` SMALLINT NOT NULL COMMENT '表索引 0-99',
    `table_name` VARCHAR(64) NOT NULL COMMENT '表名',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 避开 DATE TZ 转换坑）。扫描走 account_transaction.cut_date = 本字段',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '运行版本号，同一日期每次重跑时递增',
    `last_processed_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '本 run 在分片上已处理到的 account_transaction.id（含）。chunk 内分页游标',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '本 run 完成时最后处理的 transaction_id，仅供审计',
    `currency` CHAR(3) NOT NULL DEFAULT '' COMMENT '币种过滤。空 = 全部币种（兼容历史）；非空 = 仅该币种账户',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-未开始 1-进行中 2-已完成 3-失败',
    `start_time` DATETIME DEFAULT NULL COMMENT '开始时间',
    `end_time` DATETIME DEFAULT NULL COMMENT '结束时间',
    `cut_time` DATETIME(3) DEFAULT NULL COMMENT '日切完成时的服务器墙钟时间，仅供审计',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_day_cut` (`database_index`, `table_index`, `cut_date`, `run_id`),
    KEY `idx_cut_date` (`cut_date`),
    KEY `idx_cut_date_run` (`cut_date`, `run_id`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='日切控制表';

-- ============================================
-- 6. 异步任务表 (async_task)
-- 用于Kafka消息消费失败后的重试
-- 全局表，不分库分表
-- ============================================
CREATE TABLE IF NOT EXISTS `async_task_23` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `task_id` VARCHAR(64) NOT NULL COMMENT '任务ID',
    `task_type` VARCHAR(32) NOT NULL COMMENT '任务类型：ACCOUNTING/SNAPSHOT/DAY_CUT',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `task_data` JSON NOT NULL COMMENT '任务数据',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-待处理 1-处理中 2-成功 3-失败',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `max_retry_count` INT NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `next_retry_time` DATETIME DEFAULT NULL COMMENT '下次重试时间',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status_retry` (`status`, `next_retry_time`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='异步任务表';

-- ============================================
-- 7. TCC 分支记录表 (tcc_transaction_xxx)
-- 与账户表同分片（按 account_no 路由），保证 Try/Confirm/Cancel 在单库事务内完成。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_transaction_23` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `tcc_id` VARCHAR(64) NOT NULL COMMENT '全局 TCC ID（= voucherNo）',
    `branch_id` VARCHAR(64) NOT NULL COMMENT 'TCC 分支 ID（= transactionID）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `balance_delta` BIGINT NOT NULL COMMENT 'Confirm 时对 balance 的增量（负=减少，ISO最小单位×100）',
    `frozen_amount` BIGINT NOT NULL DEFAULT 0 COMMENT 'Try 时冻结的可用余额（0=无需冻结）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-TRYING 1-CONFIRMED 2-CANCELLED',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_branch_id` (`branch_id`),
    KEY `idx_tcc_id` (`tcc_id`),
    KEY `idx_account_no` (`account_no`),
    -- 复合索引：卡住 TCC 分支查询（status=TRYING AND created_at < cutoff），避免全表扫描
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 分布式事务分支记录表';

-- ============================================
-- 7b. UnfreezeAndDebit 补偿 outbox (freeze_compensate_outbox_xxx)
--
-- 与 frozen account 同分片（按 voucher_no 前 3 字符 = dbIdx + tableIdx 路由），
-- 保证 Phase 1 frozen-debit + INSERT outbox PENDING 在单库事务原子。
--
-- 状态机：
--   0 PENDING  : 已写入；可能被 caller mark DONE 或 worker 接管补偿
--   1 DONE     : 终态；caller Phase 2 全成功 或 worker 补偿全成功
--   2 FAILED   : 终态；worker 重试到达上限，需人工处理
--
-- payload (JSON) 包含完整 reverse plan：frozen account 信息 + 全部 credit
-- entries 信息 + 已成功的 credit txID 列表（worker 据此精确反向）。
--
-- worker 选取规则：status=PENDING AND created_at < now - 60s（给 caller
-- 一分钟时间走完 Phase 2 + 标记 DONE，避免误抢）；CAS UPDATE PENDING → COMPENSATING
-- 防多 pod / 多 worker 重复处理。
-- ============================================
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_23` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '关联的 unfreeze voucher（=路由键，前3字符为 dbIdx+tableIdx）',
    `freeze_order_no` VARCHAR(64) NOT NULL COMMENT '关联的 freeze_order 号（运维诊断用）',
    `payload` MEDIUMTEXT NOT NULL COMMENT 'JSON：完整 reverse plan（frozen acct + credit entries + 已成功 txIDs）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=DONE 2=COMPENSATING 3=FAILED',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '补偿重试次数；达 max_retry 置 FAILED',
    `error_msg` VARCHAR(500) DEFAULT NULL COMMENT '最近一次失败原因（FAILED 状态用于审计）',
    `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
    `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UnfreezeAndDebit 补偿 outbox（保证最终一致性）';

-- ============================================
-- 8. 分布式锁表 (distributed_lock)
-- 用于日切等场景的分布式锁
-- ============================================
CREATE TABLE IF NOT EXISTS `distributed_lock_23` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `lock_key` VARCHAR(128) NOT NULL COMMENT '锁键',
    `lock_value` VARCHAR(64) NOT NULL COMMENT '锁值（UUID）',
    `expire_time` DATETIME NOT NULL COMMENT '过期时间',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lock_key` (`lock_key`),
    KEY `idx_expire_time` (`expire_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='分布式锁表';


-- ============================================
-- 10. 商户信息表 (merchant_info)
-- ============================================
CREATE TABLE IF NOT EXISTS `merchant_info_23` (
    `id`                   BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `merchant_id`          BIGINT(20) UNSIGNED NOT NULL COMMENT '商户唯一ID',
    `merchant_name`        VARCHAR(256) NOT NULL COMMENT '商户名称',
    `account_idc`          VARCHAR(32)  NOT NULL COMMENT '商户账户所属 IDC',
    `external_merchant_id` VARCHAR(256) NOT NULL COMMENT '外部商户ID',
    `mid`                  VARCHAR(64)  NOT NULL COMMENT '业务线标识',
    `business_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务ID',
    `extra`                TEXT DEFAULT NULL,
    `byte_rds_ctx`         TEXT DEFAULT NULL,
    `create_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_merchant_id` (`merchant_id`),
    UNIQUE KEY `uniq_external_merchant_id_mid` (`external_merchant_id`, `mid`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户信息表';

-- ============================================
-- 11. 交易订单表 (transaction_order_xxx)
-- 分库分表：n = business_no(numeric) % 100; dbIdx = n/10; tableIdx = n
-- 幂等键：(order_no, business_type, business_no)
-- Extra 字段存储：预生成的 voucher_no、tx_ids、完整请求参数快照（reqParams）
-- ============================================
-- transaction_order 主表：状态字段 + 固定大小业务元数据（VARCHAR，行宽可控）
-- 大块 extra 字段独立存储到 transaction_order_extra（见下文），热路径不访问 extra
CREATE TABLE IF NOT EXISTS `transaction_order_23` (
    `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`       VARCHAR(64)  NOT NULL COMMENT '外部幂等订单号（requestId）',
    `business_no`    VARCHAR(64)  NOT NULL COMMENT '业务订单号（分片键，按后2位路由）',
    `business_type`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '业务类型',
    `product_code`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '产品编码',
    `event_code`     VARCHAR(256) NOT NULL DEFAULT '' COMMENT '事件编码',
    `from_party_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '资金出方 owner id',
    `from_party_type` VARCHAR(32) NOT NULL DEFAULT '' COMMENT 'user | merchant | platform',
    `to_party_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '资金入方 owner id',
    `to_party_type`  VARCHAR(32)  NOT NULL DEFAULT '',
    `amount`         VARCHAR(32)  NOT NULL DEFAULT '0' COMMENT '金额（字符串存储）',
    `currency`       VARCHAR(8)   NOT NULL DEFAULT 'PHP',
    `status`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `retry_count`    INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `max_retry_count` INT         NOT NULL DEFAULT 3 COMMENT '最大允许重试次数',
    `voucher_no`     VARCHAR(64)  DEFAULT NULL COMMENT '记账凭证号（成功后写入）',
    `error_message`  VARCHAR(512) DEFAULT NULL COMMENT '最近一次失败原因',
    `description`    VARCHAR(256) DEFAULT NULL,
    `created_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_biz` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单主表（分库分表）—— extra 独立存储提升缓存命中率';

-- ============================================
-- 12. 交易订单扩展字段表 (transaction_order_extra_xxx)
-- 与 transaction_order_xxx 同分片（按 business_no 路由）
-- 存储：请求参数快照、预生成 ID（req_hash/voucher_no/tx_ids/req_params）
-- 热路径（状态查询/更新）不访问此表；仅创建/重试/审计时读写
-- extra VARCHAR(4096)：4096字符上限，代码层写入前强制截断
-- ============================================
CREATE TABLE IF NOT EXISTS `transaction_order_extra_23` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`   VARCHAR(64)  NOT NULL COMMENT '关联 transaction_order.order_no',
    `business_no` VARCHAR(64) NOT NULL COMMENT '分片键（与主表路由一致）',
    `business_type` VARCHAR(32) NOT NULL DEFAULT '',
    `extra`      VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON: {req_hash,voucher_no,tx_ids,req_params}，最长4096字符',
    `created_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_extra` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单扩展字段表（与主表同分片，热路径不访问）';


-- ============================================
-- 13. 缓冲余额更新表 (account_balance_buffer_xxx)
-- 与账户主表同分片（按 account_no 路由），用于平台/中间账户的批量余额聚合写入。
-- 设计：平台/中间账户允许负余额，写流水后先不更新 account.balance，
--        而是在此表累计增量；后台 flush worker 每100条或每30秒批量更新 account 余额，
--        减少高频账户对 account 表的锁竞争。
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_buffer_23` (
    `account_no`       VARCHAR(64)   NOT NULL COMMENT '账户号',
    `pending_delta`    BIGINT NOT NULL DEFAULT 0 COMMENT '待刷新的余额增量（可为负，ISO最小单位×100）',
    `pending_count`    INT           NOT NULL DEFAULT 0 COMMENT '待刷新的流水笔数',
    `flush_scheduled_at` DATETIME(3) NULL DEFAULT NULL COMMENT '下次计划刷新时间（NULL=立即刷新；worker 按此时间触发，执行后自动延后一个间隔）',
    `last_updated_at`  DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最后写入时间',
    PRIMARY KEY (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额缓冲聚合表（平台/中间账户缓冲记账）';

-- ============================================
-- TCC 全局协调者表 (tcc_coordinator_xxx)
-- 分库 + 分表：按 tcc_id 数字 hash(tcc_id) 路由到 (dbIndex, globalTableIndex)，
-- 与 account_transaction / tcc_transaction 路由规则一致，保证同一笔 booking 所有
-- 相关记录共享分片，避免跨分片事务。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_coordinator_23` (
    `tcc_id`        VARCHAR(64)  NOT NULL COMMENT 'TCC 全局事务 ID（= voucher_no）',
    `phase`         TINYINT      NOT NULL DEFAULT 1 COMMENT '全局阶段：1=TRYING 2=CONFIRMING 3=CONFIRMED 4=CANCELLED',
    `business_no`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '业务订单号',
    `branch_count`  SMALLINT     NOT NULL COMMENT '总分支数',
    `cut_date`      CHAR(10)     NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，避免 gorm parseTime 1 天位移 bug）。booking 入口处 computeCutDate 计算后 propagate；与 account_transaction.cut_date 一致。日切 drain 等待: WHERE cut_date <= X AND phase IN (TRYING, CONFIRMING) → 0',
    `currency`      VARCHAR(8)   NOT NULL DEFAULT '' COMMENT '币种（PHP/CNY/USD/...）。booking 入口写入；TCC Recovery 用 coord.currency 写补 confirm 流水，避免与原 booking 不同币种导致试算 PHP-only / currency-filter 不平。空字符串=旧数据兼容',
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`tcc_id`),
    KEY `idx_phase_updated` (`phase`, `updated_at`),
    KEY `idx_cut_date_phase` (`cut_date`, `phase`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 全局协调者（阶段追踪，100 张分表）';

-- ============================================
-- 索引优化说明
-- ============================================
-- 1. 所有分表都需要创建相同的表结构和索引
-- 2. 查询时优先使用分片键（user_id, account_no）
-- 3. 时间范围查询使用 transaction_date 索引
-- 4. 定期归档历史数据，保持表数据量在合理范围

-- ============================================
-- 13. 原子批量记账订单表 (batch_order)
-- 全局单表（不分库分表），记录原子批量记账的批次状态
-- 批次内各单笔记账通过 TCC 保证整体原子性：全部成功 or 全部回滚
-- extra VARCHAR(4096)：存储批次请求参数快照，最长4096字符
-- ============================================
CREATE TABLE IF NOT EXISTS `batch_order_23` (
    `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `batch_id`        VARCHAR(64)  NOT NULL COMMENT '批次幂等ID（调用方提供）',
    `business_no`     VARCHAR(64)  NOT NULL COMMENT '批次业务号',
    `item_count`      INT          NOT NULL DEFAULT 0 COMMENT '批次中单笔数量',
    `status`          TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `error_message`   VARCHAR(512) DEFAULT NULL COMMENT '失败原因',
    `description`     VARCHAR(256) DEFAULT NULL,
    `extra`           VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON批次请求参数快照，最长4096字符',
    `created_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_batch_id` (`batch_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='原子批量记账订单表（全局表）';

-- ============================================
-- 12. 结算 Outbox 表 (settlement_outbox)
-- 热路径事务预写日志（Transactional Outbox Pattern）
-- 分库，不分表：按 voucher_no 前1字符（dbIndex）路由，每库一张 settlement_outbox 表
-- ============================================
CREATE TABLE IF NOT EXISTS `settlement_outbox_23` (
    `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no`       VARCHAR(64)  NOT NULL COMMENT '凭证号（前3字符={1d-dbIdx}{2d-tableIdx}，用于路由）',
    `event_data`       LONGTEXT     NOT NULL COMMENT 'JSON: SettlementEvent',
    `transaction_date` CHAR(10)     NOT NULL COMMENT '交易日期 YYYY-MM-DD，供日切按天过滤 outbox delta',
    `cut_date`         CHAR(10)     NOT NULL DEFAULT '' COMMENT '日切归属日 YYYY-MM-DD（入口落库那一刻定死，永不重算）；空字符串=老数据按 transaction_date 兼容处理',
    `status`           TINYINT      NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=REDIS_DONE 2=MYSQL_DONE 3=FAILED',
    `retry_count`      INT          NOT NULL DEFAULT 0 COMMENT '重试次数',
    `error_msg`        TEXT         DEFAULT NULL COMMENT '最近一次失败原因',
    `created_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_date` (`status`, `transaction_date`),
    KEY `idx_status_created` (`status`, `created_at`),
    -- OutboxWorker MySQLWriter 高频轮询查询：
    --   SELECT * FROM settlement_outbox WHERE status = ? ORDER BY id ASC LIMIT 500
    -- (status, id) 复合索引让此查询走"索引范围扫描 + 已序输出"，免去 filesort。
    -- InnoDB 二级索引天然把 PK id 附在叶节点，但只有把 id 显式作为索引第二列，
    -- 优化器才会用 index range scan 满足 ORDER BY id；否则可能选 idx_status 后 sort。
    KEY `idx_status_id` (`status`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='热路径事务预写日志（Outbox）';

-- ============================================
-- 复式记账账户系统 - 数据库表设计
-- 分库分表架构：10库 × 每库10表 = 100张全局表
--
-- 统一路由规则：
--   n = id % 100
--   dbIndex        = n / 10      （物理库编号 0–9）
--   globalTableIdx = n           （全局表序号 0–99）
--
-- 表名映射（以 account 为例）：
--   DB 0 → account_00 ~ account_09
--   DB 1 → account_10 ~ account_19
--   ...
--   DB 9 → account_90 ~ account_99
-- ============================================

-- ============================================
-- 1. 账户主表 (account_xxx)
-- 分库分表：n = user_id % 100; dbIdx = n/10; tableIdx = n
-- ============================================


CREATE TABLE IF NOT EXISTS `account_24` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号（格式：{dbIdx:02d}{tableIdx:02d}{userId}-{businessType:03d}，编码路由信息）',
    `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
    `account_type` TINYINT NOT NULL COMMENT '账户类型：1-用户账户 2-商户账户 3-平台损益账户 4-中间账户',
    `account_category` VARCHAR(32) NOT NULL COMMENT '账户分类：ASSET资产/LIABILITY负债/EQUITY所有者权益/REVENUE收入/EXPENSE费用',
    `account_business_type` SMALLINT NOT NULL DEFAULT 0 COMMENT '账户业务类型：1-用户余额 2-商户结算 3-商户待结算（最大999）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `balance` BIGINT NOT NULL DEFAULT 0 COMMENT '当前余额（ISO最小单位×100，例如USD $3.42存为34200）',
    `frozen_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '冻结余额',
    `available_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '可用余额',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-禁用 1-正常 2-冻结',
    `account_group` CHAR(1) NOT NULL DEFAULT 'A' COMMENT '账户分组：A=默认/当前；B=轮换预创建的下一组。非轮换账户永远是 A',
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    -- 轮换字段（rotating-suspense-accounts 特性，见 docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md）
    -- 旧账户 logical_account_id=NULL 且 lifecycle_phase=0 (legacy)，写入路径与原行为一致。
    `logical_account_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '关联 logical_account.id；NULL=legacy 账户',
    `period_start` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期起点（包含），UTC',
    `period_end` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期终点（不含），UTC',
    `lifecycle_phase` TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=active 2=draining 3=frozen 4=archived 5=provisioned 9=quarantined',
    `draining_started_at` DATETIME DEFAULT NULL COMMENT '进入 draining 的时刻',
    `frozen_at` DATETIME DEFAULT NULL COMMENT '进入 frozen 的时刻',
    `archived_at` DATETIME DEFAULT NULL COMMENT '进入 archived 的时刻',
    `policy_version_at_birth` BIGINT DEFAULT NULL COMMENT '出生时锁定的 policy_version；后续策略变更不影响本 instance (E-28/E-29)',
    `effective_hard_timeout_secs` INT DEFAULT NULL COMMENT '策略覆盖：本 instance 专用 hard timeout (§11.X 紧急运维)',
    `override_reason` VARCHAR(255) DEFAULT NULL COMMENT 'override 原因',
    `override_by` VARCHAR(64) DEFAULT NULL COMMENT 'override 操作人',
    `override_at` DATETIME DEFAULT NULL COMMENT 'override 时刻',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`, `account_group`) COMMENT 'userId + 业务类型 + 币种 + 组 唯一键 (轮换 fleet 时 GroupA / GroupB 各 1 个)',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`),
    KEY `idx_logical_phase` (`logical_account_id`, `lifecycle_phase`),
    KEY `idx_phase_period_end` (`lifecycle_phase`, `period_end`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_24` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `transaction_id` VARCHAR(64) NOT NULL COMMENT '交易流水号',
    `parent_transaction_id` VARCHAR(64) DEFAULT NULL COMMENT '父交易流水号（用于关联复式记账的多条记录）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型：TRANSFER/PAYMENT/REFUND/WITHDRAW等',
    `debit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '借方金额（ISO最小单位×100）',
    `credit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '贷方金额（ISO最小单位×100）',
    `balance_before` BIGINT NOT NULL COMMENT '交易前余额',
    `balance_after` BIGINT NOT NULL COMMENT '交易后余额',
    `booking_type` TINYINT NOT NULL DEFAULT 1 COMMENT '记账方式：1-同步记账 2-缓冲记账',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `transaction_date` CHAR(10) NOT NULL COMMENT '交易日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑；ISO 格式按字典序与 DATE 同序，索引/范围查询无影响）',
    `transaction_time` DATETIME NOT NULL COMMENT '交易时间',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '交易描述',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-失败 1-成功 2-处理中',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，原因：DSN parseTime 让 gorm 做 TZ 转换 → 1 天位移 bug）。在 booking 入口由 computeCutDate(now, scheduled_time, tz) 一次性确定，propagate 到所有 entry + tcc_coordinator。日切扫描 WHERE cut_date=X AND status=1，同 voucher 所有 entry 共享同一 cut_date → 试算平衡天然成立',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `ext_info` JSON DEFAULT NULL COMMENT '扩展信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_transaction_id` (`transaction_id`),
    KEY `idx_account_no` (`account_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_parent_transaction_id` (`parent_transaction_id`),
    KEY `idx_status` (`status`),
    -- 复合索引：账户流水分页查询（account_no + 日期范围 + 时间排序），取代独立的 idx_account_no + idx_transaction_date
    KEY `idx_account_date_time` (`account_no`, `transaction_date`, `transaction_time`),
    -- 日切扫描主索引：WHERE cut_date=X AND status=1
    KEY `idx_cut_date_status` (`cut_date`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户流水表';

-- ============================================
-- 3. 复式记账凭证表 (accounting_voucher_xxx)
-- 一笔业务对应一张凭证，凭证关联多条分录
-- 分库分表：按 voucher_no 前3字符路由（与 business_no 同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `accounting_voucher_24` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '凭证号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型',
    `total_debit` BIGINT NOT NULL COMMENT '借方总金额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL COMMENT '贷方总金额（ISO最小单位×100）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `voucher_date` CHAR(10) NOT NULL COMMENT '凭证日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-作废 1-生效 2-待审核',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '凭证描述',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_voucher_date` (`voucher_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='复式记账凭证表';

-- ============================================
-- 4. 账户余额快照表 (account_balance_snapshot_xxx)
-- 每日日切生成快照
-- 分库分表：按 account_no 前3字符路由（与账户主表同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_24` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `snapshot_date` CHAR(10) NOT NULL COMMENT '快照日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '关联日切版本号，重跑时递增',
    `beginning_balance` BIGINT NOT NULL COMMENT '期初余额（ISO最小单位×100）',
    `ending_balance` BIGINT NOT NULL COMMENT '期末余额（ISO最小单位×100）',
    `total_debit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日借方总额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日贷方总额（ISO最小单位×100）',
    `transaction_count` INT NOT NULL DEFAULT 0 COMMENT '当日交易笔数',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '该账户本日最后一笔流水ID，仅供审计/对账使用',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_snapshot` (`account_no`, `snapshot_date`, `run_id`),
    KEY `idx_snapshot_date` (`snapshot_date`),
    KEY `idx_snapshot_date_run` (`snapshot_date`, `run_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额快照表';

-- ============================================
-- 5. 日切控制表 (day_cut_control)
-- 全局表，不分库分表，记录每个库每张表的日切点
-- ============================================
CREATE TABLE IF NOT EXISTS `day_cut_control_24` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `database_index` TINYINT NOT NULL COMMENT '库索引 0-9',
    `table_index` SMALLINT NOT NULL COMMENT '表索引 0-99',
    `table_name` VARCHAR(64) NOT NULL COMMENT '表名',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 避开 DATE TZ 转换坑）。扫描走 account_transaction.cut_date = 本字段',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '运行版本号，同一日期每次重跑时递增',
    `last_processed_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '本 run 在分片上已处理到的 account_transaction.id（含）。chunk 内分页游标',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '本 run 完成时最后处理的 transaction_id，仅供审计',
    `currency` CHAR(3) NOT NULL DEFAULT '' COMMENT '币种过滤。空 = 全部币种（兼容历史）；非空 = 仅该币种账户',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-未开始 1-进行中 2-已完成 3-失败',
    `start_time` DATETIME DEFAULT NULL COMMENT '开始时间',
    `end_time` DATETIME DEFAULT NULL COMMENT '结束时间',
    `cut_time` DATETIME(3) DEFAULT NULL COMMENT '日切完成时的服务器墙钟时间，仅供审计',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_day_cut` (`database_index`, `table_index`, `cut_date`, `run_id`),
    KEY `idx_cut_date` (`cut_date`),
    KEY `idx_cut_date_run` (`cut_date`, `run_id`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='日切控制表';

-- ============================================
-- 6. 异步任务表 (async_task)
-- 用于Kafka消息消费失败后的重试
-- 全局表，不分库分表
-- ============================================
CREATE TABLE IF NOT EXISTS `async_task_24` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `task_id` VARCHAR(64) NOT NULL COMMENT '任务ID',
    `task_type` VARCHAR(32) NOT NULL COMMENT '任务类型：ACCOUNTING/SNAPSHOT/DAY_CUT',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `task_data` JSON NOT NULL COMMENT '任务数据',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-待处理 1-处理中 2-成功 3-失败',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `max_retry_count` INT NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `next_retry_time` DATETIME DEFAULT NULL COMMENT '下次重试时间',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status_retry` (`status`, `next_retry_time`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='异步任务表';

-- ============================================
-- 7. TCC 分支记录表 (tcc_transaction_xxx)
-- 与账户表同分片（按 account_no 路由），保证 Try/Confirm/Cancel 在单库事务内完成。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_transaction_24` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `tcc_id` VARCHAR(64) NOT NULL COMMENT '全局 TCC ID（= voucherNo）',
    `branch_id` VARCHAR(64) NOT NULL COMMENT 'TCC 分支 ID（= transactionID）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `balance_delta` BIGINT NOT NULL COMMENT 'Confirm 时对 balance 的增量（负=减少，ISO最小单位×100）',
    `frozen_amount` BIGINT NOT NULL DEFAULT 0 COMMENT 'Try 时冻结的可用余额（0=无需冻结）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-TRYING 1-CONFIRMED 2-CANCELLED',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_branch_id` (`branch_id`),
    KEY `idx_tcc_id` (`tcc_id`),
    KEY `idx_account_no` (`account_no`),
    -- 复合索引：卡住 TCC 分支查询（status=TRYING AND created_at < cutoff），避免全表扫描
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 分布式事务分支记录表';

-- ============================================
-- 7b. UnfreezeAndDebit 补偿 outbox (freeze_compensate_outbox_xxx)
--
-- 与 frozen account 同分片（按 voucher_no 前 3 字符 = dbIdx + tableIdx 路由），
-- 保证 Phase 1 frozen-debit + INSERT outbox PENDING 在单库事务原子。
--
-- 状态机：
--   0 PENDING  : 已写入；可能被 caller mark DONE 或 worker 接管补偿
--   1 DONE     : 终态；caller Phase 2 全成功 或 worker 补偿全成功
--   2 FAILED   : 终态；worker 重试到达上限，需人工处理
--
-- payload (JSON) 包含完整 reverse plan：frozen account 信息 + 全部 credit
-- entries 信息 + 已成功的 credit txID 列表（worker 据此精确反向）。
--
-- worker 选取规则：status=PENDING AND created_at < now - 60s（给 caller
-- 一分钟时间走完 Phase 2 + 标记 DONE，避免误抢）；CAS UPDATE PENDING → COMPENSATING
-- 防多 pod / 多 worker 重复处理。
-- ============================================
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_24` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '关联的 unfreeze voucher（=路由键，前3字符为 dbIdx+tableIdx）',
    `freeze_order_no` VARCHAR(64) NOT NULL COMMENT '关联的 freeze_order 号（运维诊断用）',
    `payload` MEDIUMTEXT NOT NULL COMMENT 'JSON：完整 reverse plan（frozen acct + credit entries + 已成功 txIDs）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=DONE 2=COMPENSATING 3=FAILED',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '补偿重试次数；达 max_retry 置 FAILED',
    `error_msg` VARCHAR(500) DEFAULT NULL COMMENT '最近一次失败原因（FAILED 状态用于审计）',
    `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
    `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UnfreezeAndDebit 补偿 outbox（保证最终一致性）';

-- ============================================
-- 8. 分布式锁表 (distributed_lock)
-- 用于日切等场景的分布式锁
-- ============================================
CREATE TABLE IF NOT EXISTS `distributed_lock_24` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `lock_key` VARCHAR(128) NOT NULL COMMENT '锁键',
    `lock_value` VARCHAR(64) NOT NULL COMMENT '锁值（UUID）',
    `expire_time` DATETIME NOT NULL COMMENT '过期时间',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lock_key` (`lock_key`),
    KEY `idx_expire_time` (`expire_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='分布式锁表';


-- ============================================
-- 10. 商户信息表 (merchant_info)
-- ============================================
CREATE TABLE IF NOT EXISTS `merchant_info_24` (
    `id`                   BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `merchant_id`          BIGINT(20) UNSIGNED NOT NULL COMMENT '商户唯一ID',
    `merchant_name`        VARCHAR(256) NOT NULL COMMENT '商户名称',
    `account_idc`          VARCHAR(32)  NOT NULL COMMENT '商户账户所属 IDC',
    `external_merchant_id` VARCHAR(256) NOT NULL COMMENT '外部商户ID',
    `mid`                  VARCHAR(64)  NOT NULL COMMENT '业务线标识',
    `business_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务ID',
    `extra`                TEXT DEFAULT NULL,
    `byte_rds_ctx`         TEXT DEFAULT NULL,
    `create_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_merchant_id` (`merchant_id`),
    UNIQUE KEY `uniq_external_merchant_id_mid` (`external_merchant_id`, `mid`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户信息表';

-- ============================================
-- 11. 交易订单表 (transaction_order_xxx)
-- 分库分表：n = business_no(numeric) % 100; dbIdx = n/10; tableIdx = n
-- 幂等键：(order_no, business_type, business_no)
-- Extra 字段存储：预生成的 voucher_no、tx_ids、完整请求参数快照（reqParams）
-- ============================================
-- transaction_order 主表：状态字段 + 固定大小业务元数据（VARCHAR，行宽可控）
-- 大块 extra 字段独立存储到 transaction_order_extra（见下文），热路径不访问 extra
CREATE TABLE IF NOT EXISTS `transaction_order_24` (
    `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`       VARCHAR(64)  NOT NULL COMMENT '外部幂等订单号（requestId）',
    `business_no`    VARCHAR(64)  NOT NULL COMMENT '业务订单号（分片键，按后2位路由）',
    `business_type`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '业务类型',
    `product_code`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '产品编码',
    `event_code`     VARCHAR(256) NOT NULL DEFAULT '' COMMENT '事件编码',
    `from_party_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '资金出方 owner id',
    `from_party_type` VARCHAR(32) NOT NULL DEFAULT '' COMMENT 'user | merchant | platform',
    `to_party_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '资金入方 owner id',
    `to_party_type`  VARCHAR(32)  NOT NULL DEFAULT '',
    `amount`         VARCHAR(32)  NOT NULL DEFAULT '0' COMMENT '金额（字符串存储）',
    `currency`       VARCHAR(8)   NOT NULL DEFAULT 'PHP',
    `status`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `retry_count`    INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `max_retry_count` INT         NOT NULL DEFAULT 3 COMMENT '最大允许重试次数',
    `voucher_no`     VARCHAR(64)  DEFAULT NULL COMMENT '记账凭证号（成功后写入）',
    `error_message`  VARCHAR(512) DEFAULT NULL COMMENT '最近一次失败原因',
    `description`    VARCHAR(256) DEFAULT NULL,
    `created_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_biz` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单主表（分库分表）—— extra 独立存储提升缓存命中率';

-- ============================================
-- 12. 交易订单扩展字段表 (transaction_order_extra_xxx)
-- 与 transaction_order_xxx 同分片（按 business_no 路由）
-- 存储：请求参数快照、预生成 ID（req_hash/voucher_no/tx_ids/req_params）
-- 热路径（状态查询/更新）不访问此表；仅创建/重试/审计时读写
-- extra VARCHAR(4096)：4096字符上限，代码层写入前强制截断
-- ============================================
CREATE TABLE IF NOT EXISTS `transaction_order_extra_24` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`   VARCHAR(64)  NOT NULL COMMENT '关联 transaction_order.order_no',
    `business_no` VARCHAR(64) NOT NULL COMMENT '分片键（与主表路由一致）',
    `business_type` VARCHAR(32) NOT NULL DEFAULT '',
    `extra`      VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON: {req_hash,voucher_no,tx_ids,req_params}，最长4096字符',
    `created_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_extra` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单扩展字段表（与主表同分片，热路径不访问）';


-- ============================================
-- 13. 缓冲余额更新表 (account_balance_buffer_xxx)
-- 与账户主表同分片（按 account_no 路由），用于平台/中间账户的批量余额聚合写入。
-- 设计：平台/中间账户允许负余额，写流水后先不更新 account.balance，
--        而是在此表累计增量；后台 flush worker 每100条或每30秒批量更新 account 余额，
--        减少高频账户对 account 表的锁竞争。
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_buffer_24` (
    `account_no`       VARCHAR(64)   NOT NULL COMMENT '账户号',
    `pending_delta`    BIGINT NOT NULL DEFAULT 0 COMMENT '待刷新的余额增量（可为负，ISO最小单位×100）',
    `pending_count`    INT           NOT NULL DEFAULT 0 COMMENT '待刷新的流水笔数',
    `flush_scheduled_at` DATETIME(3) NULL DEFAULT NULL COMMENT '下次计划刷新时间（NULL=立即刷新；worker 按此时间触发，执行后自动延后一个间隔）',
    `last_updated_at`  DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最后写入时间',
    PRIMARY KEY (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额缓冲聚合表（平台/中间账户缓冲记账）';

-- ============================================
-- TCC 全局协调者表 (tcc_coordinator_xxx)
-- 分库 + 分表：按 tcc_id 数字 hash(tcc_id) 路由到 (dbIndex, globalTableIndex)，
-- 与 account_transaction / tcc_transaction 路由规则一致，保证同一笔 booking 所有
-- 相关记录共享分片，避免跨分片事务。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_coordinator_24` (
    `tcc_id`        VARCHAR(64)  NOT NULL COMMENT 'TCC 全局事务 ID（= voucher_no）',
    `phase`         TINYINT      NOT NULL DEFAULT 1 COMMENT '全局阶段：1=TRYING 2=CONFIRMING 3=CONFIRMED 4=CANCELLED',
    `business_no`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '业务订单号',
    `branch_count`  SMALLINT     NOT NULL COMMENT '总分支数',
    `cut_date`      CHAR(10)     NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，避免 gorm parseTime 1 天位移 bug）。booking 入口处 computeCutDate 计算后 propagate；与 account_transaction.cut_date 一致。日切 drain 等待: WHERE cut_date <= X AND phase IN (TRYING, CONFIRMING) → 0',
    `currency`      VARCHAR(8)   NOT NULL DEFAULT '' COMMENT '币种（PHP/CNY/USD/...）。booking 入口写入；TCC Recovery 用 coord.currency 写补 confirm 流水，避免与原 booking 不同币种导致试算 PHP-only / currency-filter 不平。空字符串=旧数据兼容',
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`tcc_id`),
    KEY `idx_phase_updated` (`phase`, `updated_at`),
    KEY `idx_cut_date_phase` (`cut_date`, `phase`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 全局协调者（阶段追踪，100 张分表）';

-- ============================================
-- 索引优化说明
-- ============================================
-- 1. 所有分表都需要创建相同的表结构和索引
-- 2. 查询时优先使用分片键（user_id, account_no）
-- 3. 时间范围查询使用 transaction_date 索引
-- 4. 定期归档历史数据，保持表数据量在合理范围

-- ============================================
-- 13. 原子批量记账订单表 (batch_order)
-- 全局单表（不分库分表），记录原子批量记账的批次状态
-- 批次内各单笔记账通过 TCC 保证整体原子性：全部成功 or 全部回滚
-- extra VARCHAR(4096)：存储批次请求参数快照，最长4096字符
-- ============================================
CREATE TABLE IF NOT EXISTS `batch_order_24` (
    `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `batch_id`        VARCHAR(64)  NOT NULL COMMENT '批次幂等ID（调用方提供）',
    `business_no`     VARCHAR(64)  NOT NULL COMMENT '批次业务号',
    `item_count`      INT          NOT NULL DEFAULT 0 COMMENT '批次中单笔数量',
    `status`          TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `error_message`   VARCHAR(512) DEFAULT NULL COMMENT '失败原因',
    `description`     VARCHAR(256) DEFAULT NULL,
    `extra`           VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON批次请求参数快照，最长4096字符',
    `created_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_batch_id` (`batch_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='原子批量记账订单表（全局表）';

-- ============================================
-- 12. 结算 Outbox 表 (settlement_outbox)
-- 热路径事务预写日志（Transactional Outbox Pattern）
-- 分库，不分表：按 voucher_no 前1字符（dbIndex）路由，每库一张 settlement_outbox 表
-- ============================================
CREATE TABLE IF NOT EXISTS `settlement_outbox_24` (
    `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no`       VARCHAR(64)  NOT NULL COMMENT '凭证号（前3字符={1d-dbIdx}{2d-tableIdx}，用于路由）',
    `event_data`       LONGTEXT     NOT NULL COMMENT 'JSON: SettlementEvent',
    `transaction_date` CHAR(10)     NOT NULL COMMENT '交易日期 YYYY-MM-DD，供日切按天过滤 outbox delta',
    `cut_date`         CHAR(10)     NOT NULL DEFAULT '' COMMENT '日切归属日 YYYY-MM-DD（入口落库那一刻定死，永不重算）；空字符串=老数据按 transaction_date 兼容处理',
    `status`           TINYINT      NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=REDIS_DONE 2=MYSQL_DONE 3=FAILED',
    `retry_count`      INT          NOT NULL DEFAULT 0 COMMENT '重试次数',
    `error_msg`        TEXT         DEFAULT NULL COMMENT '最近一次失败原因',
    `created_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_date` (`status`, `transaction_date`),
    KEY `idx_status_created` (`status`, `created_at`),
    -- OutboxWorker MySQLWriter 高频轮询查询：
    --   SELECT * FROM settlement_outbox WHERE status = ? ORDER BY id ASC LIMIT 500
    -- (status, id) 复合索引让此查询走"索引范围扫描 + 已序输出"，免去 filesort。
    -- InnoDB 二级索引天然把 PK id 附在叶节点，但只有把 id 显式作为索引第二列，
    -- 优化器才会用 index range scan 满足 ORDER BY id；否则可能选 idx_status 后 sort。
    KEY `idx_status_id` (`status`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='热路径事务预写日志（Outbox）';

-- ============================================
-- 复式记账账户系统 - 数据库表设计
-- 分库分表架构：10库 × 每库10表 = 100张全局表
--
-- 统一路由规则：
--   n = id % 100
--   dbIndex        = n / 10      （物理库编号 0–9）
--   globalTableIdx = n           （全局表序号 0–99）
--
-- 表名映射（以 account 为例）：
--   DB 0 → account_00 ~ account_09
--   DB 1 → account_10 ~ account_19
--   ...
--   DB 9 → account_90 ~ account_99
-- ============================================

-- ============================================
-- 1. 账户主表 (account_xxx)
-- 分库分表：n = user_id % 100; dbIdx = n/10; tableIdx = n
-- ============================================


CREATE TABLE IF NOT EXISTS `account_25` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号（格式：{dbIdx:02d}{tableIdx:02d}{userId}-{businessType:03d}，编码路由信息）',
    `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
    `account_type` TINYINT NOT NULL COMMENT '账户类型：1-用户账户 2-商户账户 3-平台损益账户 4-中间账户',
    `account_category` VARCHAR(32) NOT NULL COMMENT '账户分类：ASSET资产/LIABILITY负债/EQUITY所有者权益/REVENUE收入/EXPENSE费用',
    `account_business_type` SMALLINT NOT NULL DEFAULT 0 COMMENT '账户业务类型：1-用户余额 2-商户结算 3-商户待结算（最大999）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `balance` BIGINT NOT NULL DEFAULT 0 COMMENT '当前余额（ISO最小单位×100，例如USD $3.42存为34200）',
    `frozen_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '冻结余额',
    `available_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '可用余额',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-禁用 1-正常 2-冻结',
    `account_group` CHAR(1) NOT NULL DEFAULT 'A' COMMENT '账户分组：A=默认/当前；B=轮换预创建的下一组。非轮换账户永远是 A',
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    -- 轮换字段（rotating-suspense-accounts 特性，见 docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md）
    -- 旧账户 logical_account_id=NULL 且 lifecycle_phase=0 (legacy)，写入路径与原行为一致。
    `logical_account_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '关联 logical_account.id；NULL=legacy 账户',
    `period_start` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期起点（包含），UTC',
    `period_end` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期终点（不含），UTC',
    `lifecycle_phase` TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=active 2=draining 3=frozen 4=archived 5=provisioned 9=quarantined',
    `draining_started_at` DATETIME DEFAULT NULL COMMENT '进入 draining 的时刻',
    `frozen_at` DATETIME DEFAULT NULL COMMENT '进入 frozen 的时刻',
    `archived_at` DATETIME DEFAULT NULL COMMENT '进入 archived 的时刻',
    `policy_version_at_birth` BIGINT DEFAULT NULL COMMENT '出生时锁定的 policy_version；后续策略变更不影响本 instance (E-28/E-29)',
    `effective_hard_timeout_secs` INT DEFAULT NULL COMMENT '策略覆盖：本 instance 专用 hard timeout (§11.X 紧急运维)',
    `override_reason` VARCHAR(255) DEFAULT NULL COMMENT 'override 原因',
    `override_by` VARCHAR(64) DEFAULT NULL COMMENT 'override 操作人',
    `override_at` DATETIME DEFAULT NULL COMMENT 'override 时刻',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`, `account_group`) COMMENT 'userId + 业务类型 + 币种 + 组 唯一键 (轮换 fleet 时 GroupA / GroupB 各 1 个)',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`),
    KEY `idx_logical_phase` (`logical_account_id`, `lifecycle_phase`),
    KEY `idx_phase_period_end` (`lifecycle_phase`, `period_end`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_25` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `transaction_id` VARCHAR(64) NOT NULL COMMENT '交易流水号',
    `parent_transaction_id` VARCHAR(64) DEFAULT NULL COMMENT '父交易流水号（用于关联复式记账的多条记录）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型：TRANSFER/PAYMENT/REFUND/WITHDRAW等',
    `debit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '借方金额（ISO最小单位×100）',
    `credit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '贷方金额（ISO最小单位×100）',
    `balance_before` BIGINT NOT NULL COMMENT '交易前余额',
    `balance_after` BIGINT NOT NULL COMMENT '交易后余额',
    `booking_type` TINYINT NOT NULL DEFAULT 1 COMMENT '记账方式：1-同步记账 2-缓冲记账',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `transaction_date` CHAR(10) NOT NULL COMMENT '交易日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑；ISO 格式按字典序与 DATE 同序，索引/范围查询无影响）',
    `transaction_time` DATETIME NOT NULL COMMENT '交易时间',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '交易描述',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-失败 1-成功 2-处理中',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，原因：DSN parseTime 让 gorm 做 TZ 转换 → 1 天位移 bug）。在 booking 入口由 computeCutDate(now, scheduled_time, tz) 一次性确定，propagate 到所有 entry + tcc_coordinator。日切扫描 WHERE cut_date=X AND status=1，同 voucher 所有 entry 共享同一 cut_date → 试算平衡天然成立',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `ext_info` JSON DEFAULT NULL COMMENT '扩展信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_transaction_id` (`transaction_id`),
    KEY `idx_account_no` (`account_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_parent_transaction_id` (`parent_transaction_id`),
    KEY `idx_status` (`status`),
    -- 复合索引：账户流水分页查询（account_no + 日期范围 + 时间排序），取代独立的 idx_account_no + idx_transaction_date
    KEY `idx_account_date_time` (`account_no`, `transaction_date`, `transaction_time`),
    -- 日切扫描主索引：WHERE cut_date=X AND status=1
    KEY `idx_cut_date_status` (`cut_date`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户流水表';

-- ============================================
-- 3. 复式记账凭证表 (accounting_voucher_xxx)
-- 一笔业务对应一张凭证，凭证关联多条分录
-- 分库分表：按 voucher_no 前3字符路由（与 business_no 同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `accounting_voucher_25` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '凭证号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型',
    `total_debit` BIGINT NOT NULL COMMENT '借方总金额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL COMMENT '贷方总金额（ISO最小单位×100）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `voucher_date` CHAR(10) NOT NULL COMMENT '凭证日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-作废 1-生效 2-待审核',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '凭证描述',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_voucher_date` (`voucher_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='复式记账凭证表';

-- ============================================
-- 4. 账户余额快照表 (account_balance_snapshot_xxx)
-- 每日日切生成快照
-- 分库分表：按 account_no 前3字符路由（与账户主表同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_25` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `snapshot_date` CHAR(10) NOT NULL COMMENT '快照日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '关联日切版本号，重跑时递增',
    `beginning_balance` BIGINT NOT NULL COMMENT '期初余额（ISO最小单位×100）',
    `ending_balance` BIGINT NOT NULL COMMENT '期末余额（ISO最小单位×100）',
    `total_debit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日借方总额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日贷方总额（ISO最小单位×100）',
    `transaction_count` INT NOT NULL DEFAULT 0 COMMENT '当日交易笔数',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '该账户本日最后一笔流水ID，仅供审计/对账使用',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_snapshot` (`account_no`, `snapshot_date`, `run_id`),
    KEY `idx_snapshot_date` (`snapshot_date`),
    KEY `idx_snapshot_date_run` (`snapshot_date`, `run_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额快照表';

-- ============================================
-- 5. 日切控制表 (day_cut_control)
-- 全局表，不分库分表，记录每个库每张表的日切点
-- ============================================
CREATE TABLE IF NOT EXISTS `day_cut_control_25` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `database_index` TINYINT NOT NULL COMMENT '库索引 0-9',
    `table_index` SMALLINT NOT NULL COMMENT '表索引 0-99',
    `table_name` VARCHAR(64) NOT NULL COMMENT '表名',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 避开 DATE TZ 转换坑）。扫描走 account_transaction.cut_date = 本字段',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '运行版本号，同一日期每次重跑时递增',
    `last_processed_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '本 run 在分片上已处理到的 account_transaction.id（含）。chunk 内分页游标',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '本 run 完成时最后处理的 transaction_id，仅供审计',
    `currency` CHAR(3) NOT NULL DEFAULT '' COMMENT '币种过滤。空 = 全部币种（兼容历史）；非空 = 仅该币种账户',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-未开始 1-进行中 2-已完成 3-失败',
    `start_time` DATETIME DEFAULT NULL COMMENT '开始时间',
    `end_time` DATETIME DEFAULT NULL COMMENT '结束时间',
    `cut_time` DATETIME(3) DEFAULT NULL COMMENT '日切完成时的服务器墙钟时间，仅供审计',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_day_cut` (`database_index`, `table_index`, `cut_date`, `run_id`),
    KEY `idx_cut_date` (`cut_date`),
    KEY `idx_cut_date_run` (`cut_date`, `run_id`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='日切控制表';

-- ============================================
-- 6. 异步任务表 (async_task)
-- 用于Kafka消息消费失败后的重试
-- 全局表，不分库分表
-- ============================================
CREATE TABLE IF NOT EXISTS `async_task_25` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `task_id` VARCHAR(64) NOT NULL COMMENT '任务ID',
    `task_type` VARCHAR(32) NOT NULL COMMENT '任务类型：ACCOUNTING/SNAPSHOT/DAY_CUT',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `task_data` JSON NOT NULL COMMENT '任务数据',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-待处理 1-处理中 2-成功 3-失败',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `max_retry_count` INT NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `next_retry_time` DATETIME DEFAULT NULL COMMENT '下次重试时间',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status_retry` (`status`, `next_retry_time`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='异步任务表';

-- ============================================
-- 7. TCC 分支记录表 (tcc_transaction_xxx)
-- 与账户表同分片（按 account_no 路由），保证 Try/Confirm/Cancel 在单库事务内完成。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_transaction_25` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `tcc_id` VARCHAR(64) NOT NULL COMMENT '全局 TCC ID（= voucherNo）',
    `branch_id` VARCHAR(64) NOT NULL COMMENT 'TCC 分支 ID（= transactionID）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `balance_delta` BIGINT NOT NULL COMMENT 'Confirm 时对 balance 的增量（负=减少，ISO最小单位×100）',
    `frozen_amount` BIGINT NOT NULL DEFAULT 0 COMMENT 'Try 时冻结的可用余额（0=无需冻结）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-TRYING 1-CONFIRMED 2-CANCELLED',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_branch_id` (`branch_id`),
    KEY `idx_tcc_id` (`tcc_id`),
    KEY `idx_account_no` (`account_no`),
    -- 复合索引：卡住 TCC 分支查询（status=TRYING AND created_at < cutoff），避免全表扫描
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 分布式事务分支记录表';

-- ============================================
-- 7b. UnfreezeAndDebit 补偿 outbox (freeze_compensate_outbox_xxx)
--
-- 与 frozen account 同分片（按 voucher_no 前 3 字符 = dbIdx + tableIdx 路由），
-- 保证 Phase 1 frozen-debit + INSERT outbox PENDING 在单库事务原子。
--
-- 状态机：
--   0 PENDING  : 已写入；可能被 caller mark DONE 或 worker 接管补偿
--   1 DONE     : 终态；caller Phase 2 全成功 或 worker 补偿全成功
--   2 FAILED   : 终态；worker 重试到达上限，需人工处理
--
-- payload (JSON) 包含完整 reverse plan：frozen account 信息 + 全部 credit
-- entries 信息 + 已成功的 credit txID 列表（worker 据此精确反向）。
--
-- worker 选取规则：status=PENDING AND created_at < now - 60s（给 caller
-- 一分钟时间走完 Phase 2 + 标记 DONE，避免误抢）；CAS UPDATE PENDING → COMPENSATING
-- 防多 pod / 多 worker 重复处理。
-- ============================================
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_25` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '关联的 unfreeze voucher（=路由键，前3字符为 dbIdx+tableIdx）',
    `freeze_order_no` VARCHAR(64) NOT NULL COMMENT '关联的 freeze_order 号（运维诊断用）',
    `payload` MEDIUMTEXT NOT NULL COMMENT 'JSON：完整 reverse plan（frozen acct + credit entries + 已成功 txIDs）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=DONE 2=COMPENSATING 3=FAILED',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '补偿重试次数；达 max_retry 置 FAILED',
    `error_msg` VARCHAR(500) DEFAULT NULL COMMENT '最近一次失败原因（FAILED 状态用于审计）',
    `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
    `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UnfreezeAndDebit 补偿 outbox（保证最终一致性）';

-- ============================================
-- 8. 分布式锁表 (distributed_lock)
-- 用于日切等场景的分布式锁
-- ============================================
CREATE TABLE IF NOT EXISTS `distributed_lock_25` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `lock_key` VARCHAR(128) NOT NULL COMMENT '锁键',
    `lock_value` VARCHAR(64) NOT NULL COMMENT '锁值（UUID）',
    `expire_time` DATETIME NOT NULL COMMENT '过期时间',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lock_key` (`lock_key`),
    KEY `idx_expire_time` (`expire_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='分布式锁表';


-- ============================================
-- 10. 商户信息表 (merchant_info)
-- ============================================
CREATE TABLE IF NOT EXISTS `merchant_info_25` (
    `id`                   BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `merchant_id`          BIGINT(20) UNSIGNED NOT NULL COMMENT '商户唯一ID',
    `merchant_name`        VARCHAR(256) NOT NULL COMMENT '商户名称',
    `account_idc`          VARCHAR(32)  NOT NULL COMMENT '商户账户所属 IDC',
    `external_merchant_id` VARCHAR(256) NOT NULL COMMENT '外部商户ID',
    `mid`                  VARCHAR(64)  NOT NULL COMMENT '业务线标识',
    `business_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务ID',
    `extra`                TEXT DEFAULT NULL,
    `byte_rds_ctx`         TEXT DEFAULT NULL,
    `create_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_merchant_id` (`merchant_id`),
    UNIQUE KEY `uniq_external_merchant_id_mid` (`external_merchant_id`, `mid`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户信息表';

-- ============================================
-- 11. 交易订单表 (transaction_order_xxx)
-- 分库分表：n = business_no(numeric) % 100; dbIdx = n/10; tableIdx = n
-- 幂等键：(order_no, business_type, business_no)
-- Extra 字段存储：预生成的 voucher_no、tx_ids、完整请求参数快照（reqParams）
-- ============================================
-- transaction_order 主表：状态字段 + 固定大小业务元数据（VARCHAR，行宽可控）
-- 大块 extra 字段独立存储到 transaction_order_extra（见下文），热路径不访问 extra
CREATE TABLE IF NOT EXISTS `transaction_order_25` (
    `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`       VARCHAR(64)  NOT NULL COMMENT '外部幂等订单号（requestId）',
    `business_no`    VARCHAR(64)  NOT NULL COMMENT '业务订单号（分片键，按后2位路由）',
    `business_type`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '业务类型',
    `product_code`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '产品编码',
    `event_code`     VARCHAR(256) NOT NULL DEFAULT '' COMMENT '事件编码',
    `from_party_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '资金出方 owner id',
    `from_party_type` VARCHAR(32) NOT NULL DEFAULT '' COMMENT 'user | merchant | platform',
    `to_party_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '资金入方 owner id',
    `to_party_type`  VARCHAR(32)  NOT NULL DEFAULT '',
    `amount`         VARCHAR(32)  NOT NULL DEFAULT '0' COMMENT '金额（字符串存储）',
    `currency`       VARCHAR(8)   NOT NULL DEFAULT 'PHP',
    `status`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `retry_count`    INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `max_retry_count` INT         NOT NULL DEFAULT 3 COMMENT '最大允许重试次数',
    `voucher_no`     VARCHAR(64)  DEFAULT NULL COMMENT '记账凭证号（成功后写入）',
    `error_message`  VARCHAR(512) DEFAULT NULL COMMENT '最近一次失败原因',
    `description`    VARCHAR(256) DEFAULT NULL,
    `created_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_biz` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单主表（分库分表）—— extra 独立存储提升缓存命中率';

-- ============================================
-- 12. 交易订单扩展字段表 (transaction_order_extra_xxx)
-- 与 transaction_order_xxx 同分片（按 business_no 路由）
-- 存储：请求参数快照、预生成 ID（req_hash/voucher_no/tx_ids/req_params）
-- 热路径（状态查询/更新）不访问此表；仅创建/重试/审计时读写
-- extra VARCHAR(4096)：4096字符上限，代码层写入前强制截断
-- ============================================
CREATE TABLE IF NOT EXISTS `transaction_order_extra_25` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`   VARCHAR(64)  NOT NULL COMMENT '关联 transaction_order.order_no',
    `business_no` VARCHAR(64) NOT NULL COMMENT '分片键（与主表路由一致）',
    `business_type` VARCHAR(32) NOT NULL DEFAULT '',
    `extra`      VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON: {req_hash,voucher_no,tx_ids,req_params}，最长4096字符',
    `created_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_extra` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单扩展字段表（与主表同分片，热路径不访问）';


-- ============================================
-- 13. 缓冲余额更新表 (account_balance_buffer_xxx)
-- 与账户主表同分片（按 account_no 路由），用于平台/中间账户的批量余额聚合写入。
-- 设计：平台/中间账户允许负余额，写流水后先不更新 account.balance，
--        而是在此表累计增量；后台 flush worker 每100条或每30秒批量更新 account 余额，
--        减少高频账户对 account 表的锁竞争。
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_buffer_25` (
    `account_no`       VARCHAR(64)   NOT NULL COMMENT '账户号',
    `pending_delta`    BIGINT NOT NULL DEFAULT 0 COMMENT '待刷新的余额增量（可为负，ISO最小单位×100）',
    `pending_count`    INT           NOT NULL DEFAULT 0 COMMENT '待刷新的流水笔数',
    `flush_scheduled_at` DATETIME(3) NULL DEFAULT NULL COMMENT '下次计划刷新时间（NULL=立即刷新；worker 按此时间触发，执行后自动延后一个间隔）',
    `last_updated_at`  DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最后写入时间',
    PRIMARY KEY (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额缓冲聚合表（平台/中间账户缓冲记账）';

-- ============================================
-- TCC 全局协调者表 (tcc_coordinator_xxx)
-- 分库 + 分表：按 tcc_id 数字 hash(tcc_id) 路由到 (dbIndex, globalTableIndex)，
-- 与 account_transaction / tcc_transaction 路由规则一致，保证同一笔 booking 所有
-- 相关记录共享分片，避免跨分片事务。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_coordinator_25` (
    `tcc_id`        VARCHAR(64)  NOT NULL COMMENT 'TCC 全局事务 ID（= voucher_no）',
    `phase`         TINYINT      NOT NULL DEFAULT 1 COMMENT '全局阶段：1=TRYING 2=CONFIRMING 3=CONFIRMED 4=CANCELLED',
    `business_no`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '业务订单号',
    `branch_count`  SMALLINT     NOT NULL COMMENT '总分支数',
    `cut_date`      CHAR(10)     NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，避免 gorm parseTime 1 天位移 bug）。booking 入口处 computeCutDate 计算后 propagate；与 account_transaction.cut_date 一致。日切 drain 等待: WHERE cut_date <= X AND phase IN (TRYING, CONFIRMING) → 0',
    `currency`      VARCHAR(8)   NOT NULL DEFAULT '' COMMENT '币种（PHP/CNY/USD/...）。booking 入口写入；TCC Recovery 用 coord.currency 写补 confirm 流水，避免与原 booking 不同币种导致试算 PHP-only / currency-filter 不平。空字符串=旧数据兼容',
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`tcc_id`),
    KEY `idx_phase_updated` (`phase`, `updated_at`),
    KEY `idx_cut_date_phase` (`cut_date`, `phase`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 全局协调者（阶段追踪，100 张分表）';

-- ============================================
-- 索引优化说明
-- ============================================
-- 1. 所有分表都需要创建相同的表结构和索引
-- 2. 查询时优先使用分片键（user_id, account_no）
-- 3. 时间范围查询使用 transaction_date 索引
-- 4. 定期归档历史数据，保持表数据量在合理范围

-- ============================================
-- 13. 原子批量记账订单表 (batch_order)
-- 全局单表（不分库分表），记录原子批量记账的批次状态
-- 批次内各单笔记账通过 TCC 保证整体原子性：全部成功 or 全部回滚
-- extra VARCHAR(4096)：存储批次请求参数快照，最长4096字符
-- ============================================
CREATE TABLE IF NOT EXISTS `batch_order_25` (
    `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `batch_id`        VARCHAR(64)  NOT NULL COMMENT '批次幂等ID（调用方提供）',
    `business_no`     VARCHAR(64)  NOT NULL COMMENT '批次业务号',
    `item_count`      INT          NOT NULL DEFAULT 0 COMMENT '批次中单笔数量',
    `status`          TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `error_message`   VARCHAR(512) DEFAULT NULL COMMENT '失败原因',
    `description`     VARCHAR(256) DEFAULT NULL,
    `extra`           VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON批次请求参数快照，最长4096字符',
    `created_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_batch_id` (`batch_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='原子批量记账订单表（全局表）';

-- ============================================
-- 12. 结算 Outbox 表 (settlement_outbox)
-- 热路径事务预写日志（Transactional Outbox Pattern）
-- 分库，不分表：按 voucher_no 前1字符（dbIndex）路由，每库一张 settlement_outbox 表
-- ============================================
CREATE TABLE IF NOT EXISTS `settlement_outbox_25` (
    `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no`       VARCHAR(64)  NOT NULL COMMENT '凭证号（前3字符={1d-dbIdx}{2d-tableIdx}，用于路由）',
    `event_data`       LONGTEXT     NOT NULL COMMENT 'JSON: SettlementEvent',
    `transaction_date` CHAR(10)     NOT NULL COMMENT '交易日期 YYYY-MM-DD，供日切按天过滤 outbox delta',
    `cut_date`         CHAR(10)     NOT NULL DEFAULT '' COMMENT '日切归属日 YYYY-MM-DD（入口落库那一刻定死，永不重算）；空字符串=老数据按 transaction_date 兼容处理',
    `status`           TINYINT      NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=REDIS_DONE 2=MYSQL_DONE 3=FAILED',
    `retry_count`      INT          NOT NULL DEFAULT 0 COMMENT '重试次数',
    `error_msg`        TEXT         DEFAULT NULL COMMENT '最近一次失败原因',
    `created_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_date` (`status`, `transaction_date`),
    KEY `idx_status_created` (`status`, `created_at`),
    -- OutboxWorker MySQLWriter 高频轮询查询：
    --   SELECT * FROM settlement_outbox WHERE status = ? ORDER BY id ASC LIMIT 500
    -- (status, id) 复合索引让此查询走"索引范围扫描 + 已序输出"，免去 filesort。
    -- InnoDB 二级索引天然把 PK id 附在叶节点，但只有把 id 显式作为索引第二列，
    -- 优化器才会用 index range scan 满足 ORDER BY id；否则可能选 idx_status 后 sort。
    KEY `idx_status_id` (`status`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='热路径事务预写日志（Outbox）';

-- ============================================
-- 复式记账账户系统 - 数据库表设计
-- 分库分表架构：10库 × 每库10表 = 100张全局表
--
-- 统一路由规则：
--   n = id % 100
--   dbIndex        = n / 10      （物理库编号 0–9）
--   globalTableIdx = n           （全局表序号 0–99）
--
-- 表名映射（以 account 为例）：
--   DB 0 → account_00 ~ account_09
--   DB 1 → account_10 ~ account_19
--   ...
--   DB 9 → account_90 ~ account_99
-- ============================================

-- ============================================
-- 1. 账户主表 (account_xxx)
-- 分库分表：n = user_id % 100; dbIdx = n/10; tableIdx = n
-- ============================================


CREATE TABLE IF NOT EXISTS `account_26` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号（格式：{dbIdx:02d}{tableIdx:02d}{userId}-{businessType:03d}，编码路由信息）',
    `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
    `account_type` TINYINT NOT NULL COMMENT '账户类型：1-用户账户 2-商户账户 3-平台损益账户 4-中间账户',
    `account_category` VARCHAR(32) NOT NULL COMMENT '账户分类：ASSET资产/LIABILITY负债/EQUITY所有者权益/REVENUE收入/EXPENSE费用',
    `account_business_type` SMALLINT NOT NULL DEFAULT 0 COMMENT '账户业务类型：1-用户余额 2-商户结算 3-商户待结算（最大999）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `balance` BIGINT NOT NULL DEFAULT 0 COMMENT '当前余额（ISO最小单位×100，例如USD $3.42存为34200）',
    `frozen_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '冻结余额',
    `available_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '可用余额',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-禁用 1-正常 2-冻结',
    `account_group` CHAR(1) NOT NULL DEFAULT 'A' COMMENT '账户分组：A=默认/当前；B=轮换预创建的下一组。非轮换账户永远是 A',
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    -- 轮换字段（rotating-suspense-accounts 特性，见 docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md）
    -- 旧账户 logical_account_id=NULL 且 lifecycle_phase=0 (legacy)，写入路径与原行为一致。
    `logical_account_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '关联 logical_account.id；NULL=legacy 账户',
    `period_start` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期起点（包含），UTC',
    `period_end` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期终点（不含），UTC',
    `lifecycle_phase` TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=active 2=draining 3=frozen 4=archived 5=provisioned 9=quarantined',
    `draining_started_at` DATETIME DEFAULT NULL COMMENT '进入 draining 的时刻',
    `frozen_at` DATETIME DEFAULT NULL COMMENT '进入 frozen 的时刻',
    `archived_at` DATETIME DEFAULT NULL COMMENT '进入 archived 的时刻',
    `policy_version_at_birth` BIGINT DEFAULT NULL COMMENT '出生时锁定的 policy_version；后续策略变更不影响本 instance (E-28/E-29)',
    `effective_hard_timeout_secs` INT DEFAULT NULL COMMENT '策略覆盖：本 instance 专用 hard timeout (§11.X 紧急运维)',
    `override_reason` VARCHAR(255) DEFAULT NULL COMMENT 'override 原因',
    `override_by` VARCHAR(64) DEFAULT NULL COMMENT 'override 操作人',
    `override_at` DATETIME DEFAULT NULL COMMENT 'override 时刻',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`, `account_group`) COMMENT 'userId + 业务类型 + 币种 + 组 唯一键 (轮换 fleet 时 GroupA / GroupB 各 1 个)',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`),
    KEY `idx_logical_phase` (`logical_account_id`, `lifecycle_phase`),
    KEY `idx_phase_period_end` (`lifecycle_phase`, `period_end`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_26` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `transaction_id` VARCHAR(64) NOT NULL COMMENT '交易流水号',
    `parent_transaction_id` VARCHAR(64) DEFAULT NULL COMMENT '父交易流水号（用于关联复式记账的多条记录）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型：TRANSFER/PAYMENT/REFUND/WITHDRAW等',
    `debit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '借方金额（ISO最小单位×100）',
    `credit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '贷方金额（ISO最小单位×100）',
    `balance_before` BIGINT NOT NULL COMMENT '交易前余额',
    `balance_after` BIGINT NOT NULL COMMENT '交易后余额',
    `booking_type` TINYINT NOT NULL DEFAULT 1 COMMENT '记账方式：1-同步记账 2-缓冲记账',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `transaction_date` CHAR(10) NOT NULL COMMENT '交易日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑；ISO 格式按字典序与 DATE 同序，索引/范围查询无影响）',
    `transaction_time` DATETIME NOT NULL COMMENT '交易时间',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '交易描述',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-失败 1-成功 2-处理中',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，原因：DSN parseTime 让 gorm 做 TZ 转换 → 1 天位移 bug）。在 booking 入口由 computeCutDate(now, scheduled_time, tz) 一次性确定，propagate 到所有 entry + tcc_coordinator。日切扫描 WHERE cut_date=X AND status=1，同 voucher 所有 entry 共享同一 cut_date → 试算平衡天然成立',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `ext_info` JSON DEFAULT NULL COMMENT '扩展信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_transaction_id` (`transaction_id`),
    KEY `idx_account_no` (`account_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_parent_transaction_id` (`parent_transaction_id`),
    KEY `idx_status` (`status`),
    -- 复合索引：账户流水分页查询（account_no + 日期范围 + 时间排序），取代独立的 idx_account_no + idx_transaction_date
    KEY `idx_account_date_time` (`account_no`, `transaction_date`, `transaction_time`),
    -- 日切扫描主索引：WHERE cut_date=X AND status=1
    KEY `idx_cut_date_status` (`cut_date`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户流水表';

-- ============================================
-- 3. 复式记账凭证表 (accounting_voucher_xxx)
-- 一笔业务对应一张凭证，凭证关联多条分录
-- 分库分表：按 voucher_no 前3字符路由（与 business_no 同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `accounting_voucher_26` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '凭证号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型',
    `total_debit` BIGINT NOT NULL COMMENT '借方总金额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL COMMENT '贷方总金额（ISO最小单位×100）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `voucher_date` CHAR(10) NOT NULL COMMENT '凭证日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-作废 1-生效 2-待审核',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '凭证描述',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_voucher_date` (`voucher_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='复式记账凭证表';

-- ============================================
-- 4. 账户余额快照表 (account_balance_snapshot_xxx)
-- 每日日切生成快照
-- 分库分表：按 account_no 前3字符路由（与账户主表同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_26` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `snapshot_date` CHAR(10) NOT NULL COMMENT '快照日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '关联日切版本号，重跑时递增',
    `beginning_balance` BIGINT NOT NULL COMMENT '期初余额（ISO最小单位×100）',
    `ending_balance` BIGINT NOT NULL COMMENT '期末余额（ISO最小单位×100）',
    `total_debit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日借方总额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日贷方总额（ISO最小单位×100）',
    `transaction_count` INT NOT NULL DEFAULT 0 COMMENT '当日交易笔数',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '该账户本日最后一笔流水ID，仅供审计/对账使用',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_snapshot` (`account_no`, `snapshot_date`, `run_id`),
    KEY `idx_snapshot_date` (`snapshot_date`),
    KEY `idx_snapshot_date_run` (`snapshot_date`, `run_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额快照表';

-- ============================================
-- 5. 日切控制表 (day_cut_control)
-- 全局表，不分库分表，记录每个库每张表的日切点
-- ============================================
CREATE TABLE IF NOT EXISTS `day_cut_control_26` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `database_index` TINYINT NOT NULL COMMENT '库索引 0-9',
    `table_index` SMALLINT NOT NULL COMMENT '表索引 0-99',
    `table_name` VARCHAR(64) NOT NULL COMMENT '表名',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 避开 DATE TZ 转换坑）。扫描走 account_transaction.cut_date = 本字段',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '运行版本号，同一日期每次重跑时递增',
    `last_processed_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '本 run 在分片上已处理到的 account_transaction.id（含）。chunk 内分页游标',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '本 run 完成时最后处理的 transaction_id，仅供审计',
    `currency` CHAR(3) NOT NULL DEFAULT '' COMMENT '币种过滤。空 = 全部币种（兼容历史）；非空 = 仅该币种账户',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-未开始 1-进行中 2-已完成 3-失败',
    `start_time` DATETIME DEFAULT NULL COMMENT '开始时间',
    `end_time` DATETIME DEFAULT NULL COMMENT '结束时间',
    `cut_time` DATETIME(3) DEFAULT NULL COMMENT '日切完成时的服务器墙钟时间，仅供审计',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_day_cut` (`database_index`, `table_index`, `cut_date`, `run_id`),
    KEY `idx_cut_date` (`cut_date`),
    KEY `idx_cut_date_run` (`cut_date`, `run_id`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='日切控制表';

-- ============================================
-- 6. 异步任务表 (async_task)
-- 用于Kafka消息消费失败后的重试
-- 全局表，不分库分表
-- ============================================
CREATE TABLE IF NOT EXISTS `async_task_26` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `task_id` VARCHAR(64) NOT NULL COMMENT '任务ID',
    `task_type` VARCHAR(32) NOT NULL COMMENT '任务类型：ACCOUNTING/SNAPSHOT/DAY_CUT',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `task_data` JSON NOT NULL COMMENT '任务数据',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-待处理 1-处理中 2-成功 3-失败',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `max_retry_count` INT NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `next_retry_time` DATETIME DEFAULT NULL COMMENT '下次重试时间',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status_retry` (`status`, `next_retry_time`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='异步任务表';

-- ============================================
-- 7. TCC 分支记录表 (tcc_transaction_xxx)
-- 与账户表同分片（按 account_no 路由），保证 Try/Confirm/Cancel 在单库事务内完成。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_transaction_26` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `tcc_id` VARCHAR(64) NOT NULL COMMENT '全局 TCC ID（= voucherNo）',
    `branch_id` VARCHAR(64) NOT NULL COMMENT 'TCC 分支 ID（= transactionID）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `balance_delta` BIGINT NOT NULL COMMENT 'Confirm 时对 balance 的增量（负=减少，ISO最小单位×100）',
    `frozen_amount` BIGINT NOT NULL DEFAULT 0 COMMENT 'Try 时冻结的可用余额（0=无需冻结）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-TRYING 1-CONFIRMED 2-CANCELLED',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_branch_id` (`branch_id`),
    KEY `idx_tcc_id` (`tcc_id`),
    KEY `idx_account_no` (`account_no`),
    -- 复合索引：卡住 TCC 分支查询（status=TRYING AND created_at < cutoff），避免全表扫描
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 分布式事务分支记录表';

-- ============================================
-- 7b. UnfreezeAndDebit 补偿 outbox (freeze_compensate_outbox_xxx)
--
-- 与 frozen account 同分片（按 voucher_no 前 3 字符 = dbIdx + tableIdx 路由），
-- 保证 Phase 1 frozen-debit + INSERT outbox PENDING 在单库事务原子。
--
-- 状态机：
--   0 PENDING  : 已写入；可能被 caller mark DONE 或 worker 接管补偿
--   1 DONE     : 终态；caller Phase 2 全成功 或 worker 补偿全成功
--   2 FAILED   : 终态；worker 重试到达上限，需人工处理
--
-- payload (JSON) 包含完整 reverse plan：frozen account 信息 + 全部 credit
-- entries 信息 + 已成功的 credit txID 列表（worker 据此精确反向）。
--
-- worker 选取规则：status=PENDING AND created_at < now - 60s（给 caller
-- 一分钟时间走完 Phase 2 + 标记 DONE，避免误抢）；CAS UPDATE PENDING → COMPENSATING
-- 防多 pod / 多 worker 重复处理。
-- ============================================
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_26` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '关联的 unfreeze voucher（=路由键，前3字符为 dbIdx+tableIdx）',
    `freeze_order_no` VARCHAR(64) NOT NULL COMMENT '关联的 freeze_order 号（运维诊断用）',
    `payload` MEDIUMTEXT NOT NULL COMMENT 'JSON：完整 reverse plan（frozen acct + credit entries + 已成功 txIDs）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=DONE 2=COMPENSATING 3=FAILED',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '补偿重试次数；达 max_retry 置 FAILED',
    `error_msg` VARCHAR(500) DEFAULT NULL COMMENT '最近一次失败原因（FAILED 状态用于审计）',
    `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
    `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UnfreezeAndDebit 补偿 outbox（保证最终一致性）';

-- ============================================
-- 8. 分布式锁表 (distributed_lock)
-- 用于日切等场景的分布式锁
-- ============================================
CREATE TABLE IF NOT EXISTS `distributed_lock_26` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `lock_key` VARCHAR(128) NOT NULL COMMENT '锁键',
    `lock_value` VARCHAR(64) NOT NULL COMMENT '锁值（UUID）',
    `expire_time` DATETIME NOT NULL COMMENT '过期时间',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lock_key` (`lock_key`),
    KEY `idx_expire_time` (`expire_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='分布式锁表';


-- ============================================
-- 10. 商户信息表 (merchant_info)
-- ============================================
CREATE TABLE IF NOT EXISTS `merchant_info_26` (
    `id`                   BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `merchant_id`          BIGINT(20) UNSIGNED NOT NULL COMMENT '商户唯一ID',
    `merchant_name`        VARCHAR(256) NOT NULL COMMENT '商户名称',
    `account_idc`          VARCHAR(32)  NOT NULL COMMENT '商户账户所属 IDC',
    `external_merchant_id` VARCHAR(256) NOT NULL COMMENT '外部商户ID',
    `mid`                  VARCHAR(64)  NOT NULL COMMENT '业务线标识',
    `business_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务ID',
    `extra`                TEXT DEFAULT NULL,
    `byte_rds_ctx`         TEXT DEFAULT NULL,
    `create_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_merchant_id` (`merchant_id`),
    UNIQUE KEY `uniq_external_merchant_id_mid` (`external_merchant_id`, `mid`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户信息表';

-- ============================================
-- 11. 交易订单表 (transaction_order_xxx)
-- 分库分表：n = business_no(numeric) % 100; dbIdx = n/10; tableIdx = n
-- 幂等键：(order_no, business_type, business_no)
-- Extra 字段存储：预生成的 voucher_no、tx_ids、完整请求参数快照（reqParams）
-- ============================================
-- transaction_order 主表：状态字段 + 固定大小业务元数据（VARCHAR，行宽可控）
-- 大块 extra 字段独立存储到 transaction_order_extra（见下文），热路径不访问 extra
CREATE TABLE IF NOT EXISTS `transaction_order_26` (
    `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`       VARCHAR(64)  NOT NULL COMMENT '外部幂等订单号（requestId）',
    `business_no`    VARCHAR(64)  NOT NULL COMMENT '业务订单号（分片键，按后2位路由）',
    `business_type`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '业务类型',
    `product_code`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '产品编码',
    `event_code`     VARCHAR(256) NOT NULL DEFAULT '' COMMENT '事件编码',
    `from_party_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '资金出方 owner id',
    `from_party_type` VARCHAR(32) NOT NULL DEFAULT '' COMMENT 'user | merchant | platform',
    `to_party_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '资金入方 owner id',
    `to_party_type`  VARCHAR(32)  NOT NULL DEFAULT '',
    `amount`         VARCHAR(32)  NOT NULL DEFAULT '0' COMMENT '金额（字符串存储）',
    `currency`       VARCHAR(8)   NOT NULL DEFAULT 'PHP',
    `status`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `retry_count`    INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `max_retry_count` INT         NOT NULL DEFAULT 3 COMMENT '最大允许重试次数',
    `voucher_no`     VARCHAR(64)  DEFAULT NULL COMMENT '记账凭证号（成功后写入）',
    `error_message`  VARCHAR(512) DEFAULT NULL COMMENT '最近一次失败原因',
    `description`    VARCHAR(256) DEFAULT NULL,
    `created_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_biz` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单主表（分库分表）—— extra 独立存储提升缓存命中率';

-- ============================================
-- 12. 交易订单扩展字段表 (transaction_order_extra_xxx)
-- 与 transaction_order_xxx 同分片（按 business_no 路由）
-- 存储：请求参数快照、预生成 ID（req_hash/voucher_no/tx_ids/req_params）
-- 热路径（状态查询/更新）不访问此表；仅创建/重试/审计时读写
-- extra VARCHAR(4096)：4096字符上限，代码层写入前强制截断
-- ============================================
CREATE TABLE IF NOT EXISTS `transaction_order_extra_26` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`   VARCHAR(64)  NOT NULL COMMENT '关联 transaction_order.order_no',
    `business_no` VARCHAR(64) NOT NULL COMMENT '分片键（与主表路由一致）',
    `business_type` VARCHAR(32) NOT NULL DEFAULT '',
    `extra`      VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON: {req_hash,voucher_no,tx_ids,req_params}，最长4096字符',
    `created_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_extra` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单扩展字段表（与主表同分片，热路径不访问）';


-- ============================================
-- 13. 缓冲余额更新表 (account_balance_buffer_xxx)
-- 与账户主表同分片（按 account_no 路由），用于平台/中间账户的批量余额聚合写入。
-- 设计：平台/中间账户允许负余额，写流水后先不更新 account.balance，
--        而是在此表累计增量；后台 flush worker 每100条或每30秒批量更新 account 余额，
--        减少高频账户对 account 表的锁竞争。
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_buffer_26` (
    `account_no`       VARCHAR(64)   NOT NULL COMMENT '账户号',
    `pending_delta`    BIGINT NOT NULL DEFAULT 0 COMMENT '待刷新的余额增量（可为负，ISO最小单位×100）',
    `pending_count`    INT           NOT NULL DEFAULT 0 COMMENT '待刷新的流水笔数',
    `flush_scheduled_at` DATETIME(3) NULL DEFAULT NULL COMMENT '下次计划刷新时间（NULL=立即刷新；worker 按此时间触发，执行后自动延后一个间隔）',
    `last_updated_at`  DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最后写入时间',
    PRIMARY KEY (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额缓冲聚合表（平台/中间账户缓冲记账）';

-- ============================================
-- TCC 全局协调者表 (tcc_coordinator_xxx)
-- 分库 + 分表：按 tcc_id 数字 hash(tcc_id) 路由到 (dbIndex, globalTableIndex)，
-- 与 account_transaction / tcc_transaction 路由规则一致，保证同一笔 booking 所有
-- 相关记录共享分片，避免跨分片事务。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_coordinator_26` (
    `tcc_id`        VARCHAR(64)  NOT NULL COMMENT 'TCC 全局事务 ID（= voucher_no）',
    `phase`         TINYINT      NOT NULL DEFAULT 1 COMMENT '全局阶段：1=TRYING 2=CONFIRMING 3=CONFIRMED 4=CANCELLED',
    `business_no`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '业务订单号',
    `branch_count`  SMALLINT     NOT NULL COMMENT '总分支数',
    `cut_date`      CHAR(10)     NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，避免 gorm parseTime 1 天位移 bug）。booking 入口处 computeCutDate 计算后 propagate；与 account_transaction.cut_date 一致。日切 drain 等待: WHERE cut_date <= X AND phase IN (TRYING, CONFIRMING) → 0',
    `currency`      VARCHAR(8)   NOT NULL DEFAULT '' COMMENT '币种（PHP/CNY/USD/...）。booking 入口写入；TCC Recovery 用 coord.currency 写补 confirm 流水，避免与原 booking 不同币种导致试算 PHP-only / currency-filter 不平。空字符串=旧数据兼容',
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`tcc_id`),
    KEY `idx_phase_updated` (`phase`, `updated_at`),
    KEY `idx_cut_date_phase` (`cut_date`, `phase`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 全局协调者（阶段追踪，100 张分表）';

-- ============================================
-- 索引优化说明
-- ============================================
-- 1. 所有分表都需要创建相同的表结构和索引
-- 2. 查询时优先使用分片键（user_id, account_no）
-- 3. 时间范围查询使用 transaction_date 索引
-- 4. 定期归档历史数据，保持表数据量在合理范围

-- ============================================
-- 13. 原子批量记账订单表 (batch_order)
-- 全局单表（不分库分表），记录原子批量记账的批次状态
-- 批次内各单笔记账通过 TCC 保证整体原子性：全部成功 or 全部回滚
-- extra VARCHAR(4096)：存储批次请求参数快照，最长4096字符
-- ============================================
CREATE TABLE IF NOT EXISTS `batch_order_26` (
    `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `batch_id`        VARCHAR(64)  NOT NULL COMMENT '批次幂等ID（调用方提供）',
    `business_no`     VARCHAR(64)  NOT NULL COMMENT '批次业务号',
    `item_count`      INT          NOT NULL DEFAULT 0 COMMENT '批次中单笔数量',
    `status`          TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `error_message`   VARCHAR(512) DEFAULT NULL COMMENT '失败原因',
    `description`     VARCHAR(256) DEFAULT NULL,
    `extra`           VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON批次请求参数快照，最长4096字符',
    `created_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_batch_id` (`batch_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='原子批量记账订单表（全局表）';

-- ============================================
-- 12. 结算 Outbox 表 (settlement_outbox)
-- 热路径事务预写日志（Transactional Outbox Pattern）
-- 分库，不分表：按 voucher_no 前1字符（dbIndex）路由，每库一张 settlement_outbox 表
-- ============================================
CREATE TABLE IF NOT EXISTS `settlement_outbox_26` (
    `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no`       VARCHAR(64)  NOT NULL COMMENT '凭证号（前3字符={1d-dbIdx}{2d-tableIdx}，用于路由）',
    `event_data`       LONGTEXT     NOT NULL COMMENT 'JSON: SettlementEvent',
    `transaction_date` CHAR(10)     NOT NULL COMMENT '交易日期 YYYY-MM-DD，供日切按天过滤 outbox delta',
    `cut_date`         CHAR(10)     NOT NULL DEFAULT '' COMMENT '日切归属日 YYYY-MM-DD（入口落库那一刻定死，永不重算）；空字符串=老数据按 transaction_date 兼容处理',
    `status`           TINYINT      NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=REDIS_DONE 2=MYSQL_DONE 3=FAILED',
    `retry_count`      INT          NOT NULL DEFAULT 0 COMMENT '重试次数',
    `error_msg`        TEXT         DEFAULT NULL COMMENT '最近一次失败原因',
    `created_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_date` (`status`, `transaction_date`),
    KEY `idx_status_created` (`status`, `created_at`),
    -- OutboxWorker MySQLWriter 高频轮询查询：
    --   SELECT * FROM settlement_outbox WHERE status = ? ORDER BY id ASC LIMIT 500
    -- (status, id) 复合索引让此查询走"索引范围扫描 + 已序输出"，免去 filesort。
    -- InnoDB 二级索引天然把 PK id 附在叶节点，但只有把 id 显式作为索引第二列，
    -- 优化器才会用 index range scan 满足 ORDER BY id；否则可能选 idx_status 后 sort。
    KEY `idx_status_id` (`status`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='热路径事务预写日志（Outbox）';

-- ============================================
-- 复式记账账户系统 - 数据库表设计
-- 分库分表架构：10库 × 每库10表 = 100张全局表
--
-- 统一路由规则：
--   n = id % 100
--   dbIndex        = n / 10      （物理库编号 0–9）
--   globalTableIdx = n           （全局表序号 0–99）
--
-- 表名映射（以 account 为例）：
--   DB 0 → account_00 ~ account_09
--   DB 1 → account_10 ~ account_19
--   ...
--   DB 9 → account_90 ~ account_99
-- ============================================

-- ============================================
-- 1. 账户主表 (account_xxx)
-- 分库分表：n = user_id % 100; dbIdx = n/10; tableIdx = n
-- ============================================


CREATE TABLE IF NOT EXISTS `account_27` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号（格式：{dbIdx:02d}{tableIdx:02d}{userId}-{businessType:03d}，编码路由信息）',
    `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
    `account_type` TINYINT NOT NULL COMMENT '账户类型：1-用户账户 2-商户账户 3-平台损益账户 4-中间账户',
    `account_category` VARCHAR(32) NOT NULL COMMENT '账户分类：ASSET资产/LIABILITY负债/EQUITY所有者权益/REVENUE收入/EXPENSE费用',
    `account_business_type` SMALLINT NOT NULL DEFAULT 0 COMMENT '账户业务类型：1-用户余额 2-商户结算 3-商户待结算（最大999）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `balance` BIGINT NOT NULL DEFAULT 0 COMMENT '当前余额（ISO最小单位×100，例如USD $3.42存为34200）',
    `frozen_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '冻结余额',
    `available_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '可用余额',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-禁用 1-正常 2-冻结',
    `account_group` CHAR(1) NOT NULL DEFAULT 'A' COMMENT '账户分组：A=默认/当前；B=轮换预创建的下一组。非轮换账户永远是 A',
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    -- 轮换字段（rotating-suspense-accounts 特性，见 docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md）
    -- 旧账户 logical_account_id=NULL 且 lifecycle_phase=0 (legacy)，写入路径与原行为一致。
    `logical_account_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '关联 logical_account.id；NULL=legacy 账户',
    `period_start` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期起点（包含），UTC',
    `period_end` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期终点（不含），UTC',
    `lifecycle_phase` TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=active 2=draining 3=frozen 4=archived 5=provisioned 9=quarantined',
    `draining_started_at` DATETIME DEFAULT NULL COMMENT '进入 draining 的时刻',
    `frozen_at` DATETIME DEFAULT NULL COMMENT '进入 frozen 的时刻',
    `archived_at` DATETIME DEFAULT NULL COMMENT '进入 archived 的时刻',
    `policy_version_at_birth` BIGINT DEFAULT NULL COMMENT '出生时锁定的 policy_version；后续策略变更不影响本 instance (E-28/E-29)',
    `effective_hard_timeout_secs` INT DEFAULT NULL COMMENT '策略覆盖：本 instance 专用 hard timeout (§11.X 紧急运维)',
    `override_reason` VARCHAR(255) DEFAULT NULL COMMENT 'override 原因',
    `override_by` VARCHAR(64) DEFAULT NULL COMMENT 'override 操作人',
    `override_at` DATETIME DEFAULT NULL COMMENT 'override 时刻',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`, `account_group`) COMMENT 'userId + 业务类型 + 币种 + 组 唯一键 (轮换 fleet 时 GroupA / GroupB 各 1 个)',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`),
    KEY `idx_logical_phase` (`logical_account_id`, `lifecycle_phase`),
    KEY `idx_phase_period_end` (`lifecycle_phase`, `period_end`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_27` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `transaction_id` VARCHAR(64) NOT NULL COMMENT '交易流水号',
    `parent_transaction_id` VARCHAR(64) DEFAULT NULL COMMENT '父交易流水号（用于关联复式记账的多条记录）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型：TRANSFER/PAYMENT/REFUND/WITHDRAW等',
    `debit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '借方金额（ISO最小单位×100）',
    `credit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '贷方金额（ISO最小单位×100）',
    `balance_before` BIGINT NOT NULL COMMENT '交易前余额',
    `balance_after` BIGINT NOT NULL COMMENT '交易后余额',
    `booking_type` TINYINT NOT NULL DEFAULT 1 COMMENT '记账方式：1-同步记账 2-缓冲记账',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `transaction_date` CHAR(10) NOT NULL COMMENT '交易日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑；ISO 格式按字典序与 DATE 同序，索引/范围查询无影响）',
    `transaction_time` DATETIME NOT NULL COMMENT '交易时间',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '交易描述',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-失败 1-成功 2-处理中',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，原因：DSN parseTime 让 gorm 做 TZ 转换 → 1 天位移 bug）。在 booking 入口由 computeCutDate(now, scheduled_time, tz) 一次性确定，propagate 到所有 entry + tcc_coordinator。日切扫描 WHERE cut_date=X AND status=1，同 voucher 所有 entry 共享同一 cut_date → 试算平衡天然成立',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `ext_info` JSON DEFAULT NULL COMMENT '扩展信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_transaction_id` (`transaction_id`),
    KEY `idx_account_no` (`account_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_parent_transaction_id` (`parent_transaction_id`),
    KEY `idx_status` (`status`),
    -- 复合索引：账户流水分页查询（account_no + 日期范围 + 时间排序），取代独立的 idx_account_no + idx_transaction_date
    KEY `idx_account_date_time` (`account_no`, `transaction_date`, `transaction_time`),
    -- 日切扫描主索引：WHERE cut_date=X AND status=1
    KEY `idx_cut_date_status` (`cut_date`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户流水表';

-- ============================================
-- 3. 复式记账凭证表 (accounting_voucher_xxx)
-- 一笔业务对应一张凭证，凭证关联多条分录
-- 分库分表：按 voucher_no 前3字符路由（与 business_no 同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `accounting_voucher_27` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '凭证号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型',
    `total_debit` BIGINT NOT NULL COMMENT '借方总金额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL COMMENT '贷方总金额（ISO最小单位×100）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `voucher_date` CHAR(10) NOT NULL COMMENT '凭证日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-作废 1-生效 2-待审核',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '凭证描述',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_voucher_date` (`voucher_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='复式记账凭证表';

-- ============================================
-- 4. 账户余额快照表 (account_balance_snapshot_xxx)
-- 每日日切生成快照
-- 分库分表：按 account_no 前3字符路由（与账户主表同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_27` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `snapshot_date` CHAR(10) NOT NULL COMMENT '快照日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '关联日切版本号，重跑时递增',
    `beginning_balance` BIGINT NOT NULL COMMENT '期初余额（ISO最小单位×100）',
    `ending_balance` BIGINT NOT NULL COMMENT '期末余额（ISO最小单位×100）',
    `total_debit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日借方总额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日贷方总额（ISO最小单位×100）',
    `transaction_count` INT NOT NULL DEFAULT 0 COMMENT '当日交易笔数',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '该账户本日最后一笔流水ID，仅供审计/对账使用',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_snapshot` (`account_no`, `snapshot_date`, `run_id`),
    KEY `idx_snapshot_date` (`snapshot_date`),
    KEY `idx_snapshot_date_run` (`snapshot_date`, `run_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额快照表';

-- ============================================
-- 5. 日切控制表 (day_cut_control)
-- 全局表，不分库分表，记录每个库每张表的日切点
-- ============================================
CREATE TABLE IF NOT EXISTS `day_cut_control_27` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `database_index` TINYINT NOT NULL COMMENT '库索引 0-9',
    `table_index` SMALLINT NOT NULL COMMENT '表索引 0-99',
    `table_name` VARCHAR(64) NOT NULL COMMENT '表名',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 避开 DATE TZ 转换坑）。扫描走 account_transaction.cut_date = 本字段',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '运行版本号，同一日期每次重跑时递增',
    `last_processed_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '本 run 在分片上已处理到的 account_transaction.id（含）。chunk 内分页游标',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '本 run 完成时最后处理的 transaction_id，仅供审计',
    `currency` CHAR(3) NOT NULL DEFAULT '' COMMENT '币种过滤。空 = 全部币种（兼容历史）；非空 = 仅该币种账户',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-未开始 1-进行中 2-已完成 3-失败',
    `start_time` DATETIME DEFAULT NULL COMMENT '开始时间',
    `end_time` DATETIME DEFAULT NULL COMMENT '结束时间',
    `cut_time` DATETIME(3) DEFAULT NULL COMMENT '日切完成时的服务器墙钟时间，仅供审计',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_day_cut` (`database_index`, `table_index`, `cut_date`, `run_id`),
    KEY `idx_cut_date` (`cut_date`),
    KEY `idx_cut_date_run` (`cut_date`, `run_id`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='日切控制表';

-- ============================================
-- 6. 异步任务表 (async_task)
-- 用于Kafka消息消费失败后的重试
-- 全局表，不分库分表
-- ============================================
CREATE TABLE IF NOT EXISTS `async_task_27` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `task_id` VARCHAR(64) NOT NULL COMMENT '任务ID',
    `task_type` VARCHAR(32) NOT NULL COMMENT '任务类型：ACCOUNTING/SNAPSHOT/DAY_CUT',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `task_data` JSON NOT NULL COMMENT '任务数据',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-待处理 1-处理中 2-成功 3-失败',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `max_retry_count` INT NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `next_retry_time` DATETIME DEFAULT NULL COMMENT '下次重试时间',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status_retry` (`status`, `next_retry_time`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='异步任务表';

-- ============================================
-- 7. TCC 分支记录表 (tcc_transaction_xxx)
-- 与账户表同分片（按 account_no 路由），保证 Try/Confirm/Cancel 在单库事务内完成。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_transaction_27` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `tcc_id` VARCHAR(64) NOT NULL COMMENT '全局 TCC ID（= voucherNo）',
    `branch_id` VARCHAR(64) NOT NULL COMMENT 'TCC 分支 ID（= transactionID）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `balance_delta` BIGINT NOT NULL COMMENT 'Confirm 时对 balance 的增量（负=减少，ISO最小单位×100）',
    `frozen_amount` BIGINT NOT NULL DEFAULT 0 COMMENT 'Try 时冻结的可用余额（0=无需冻结）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-TRYING 1-CONFIRMED 2-CANCELLED',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_branch_id` (`branch_id`),
    KEY `idx_tcc_id` (`tcc_id`),
    KEY `idx_account_no` (`account_no`),
    -- 复合索引：卡住 TCC 分支查询（status=TRYING AND created_at < cutoff），避免全表扫描
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 分布式事务分支记录表';

-- ============================================
-- 7b. UnfreezeAndDebit 补偿 outbox (freeze_compensate_outbox_xxx)
--
-- 与 frozen account 同分片（按 voucher_no 前 3 字符 = dbIdx + tableIdx 路由），
-- 保证 Phase 1 frozen-debit + INSERT outbox PENDING 在单库事务原子。
--
-- 状态机：
--   0 PENDING  : 已写入；可能被 caller mark DONE 或 worker 接管补偿
--   1 DONE     : 终态；caller Phase 2 全成功 或 worker 补偿全成功
--   2 FAILED   : 终态；worker 重试到达上限，需人工处理
--
-- payload (JSON) 包含完整 reverse plan：frozen account 信息 + 全部 credit
-- entries 信息 + 已成功的 credit txID 列表（worker 据此精确反向）。
--
-- worker 选取规则：status=PENDING AND created_at < now - 60s（给 caller
-- 一分钟时间走完 Phase 2 + 标记 DONE，避免误抢）；CAS UPDATE PENDING → COMPENSATING
-- 防多 pod / 多 worker 重复处理。
-- ============================================
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_27` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '关联的 unfreeze voucher（=路由键，前3字符为 dbIdx+tableIdx）',
    `freeze_order_no` VARCHAR(64) NOT NULL COMMENT '关联的 freeze_order 号（运维诊断用）',
    `payload` MEDIUMTEXT NOT NULL COMMENT 'JSON：完整 reverse plan（frozen acct + credit entries + 已成功 txIDs）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=DONE 2=COMPENSATING 3=FAILED',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '补偿重试次数；达 max_retry 置 FAILED',
    `error_msg` VARCHAR(500) DEFAULT NULL COMMENT '最近一次失败原因（FAILED 状态用于审计）',
    `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
    `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UnfreezeAndDebit 补偿 outbox（保证最终一致性）';

-- ============================================
-- 8. 分布式锁表 (distributed_lock)
-- 用于日切等场景的分布式锁
-- ============================================
CREATE TABLE IF NOT EXISTS `distributed_lock_27` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `lock_key` VARCHAR(128) NOT NULL COMMENT '锁键',
    `lock_value` VARCHAR(64) NOT NULL COMMENT '锁值（UUID）',
    `expire_time` DATETIME NOT NULL COMMENT '过期时间',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lock_key` (`lock_key`),
    KEY `idx_expire_time` (`expire_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='分布式锁表';


-- ============================================
-- 10. 商户信息表 (merchant_info)
-- ============================================
CREATE TABLE IF NOT EXISTS `merchant_info_27` (
    `id`                   BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `merchant_id`          BIGINT(20) UNSIGNED NOT NULL COMMENT '商户唯一ID',
    `merchant_name`        VARCHAR(256) NOT NULL COMMENT '商户名称',
    `account_idc`          VARCHAR(32)  NOT NULL COMMENT '商户账户所属 IDC',
    `external_merchant_id` VARCHAR(256) NOT NULL COMMENT '外部商户ID',
    `mid`                  VARCHAR(64)  NOT NULL COMMENT '业务线标识',
    `business_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务ID',
    `extra`                TEXT DEFAULT NULL,
    `byte_rds_ctx`         TEXT DEFAULT NULL,
    `create_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_merchant_id` (`merchant_id`),
    UNIQUE KEY `uniq_external_merchant_id_mid` (`external_merchant_id`, `mid`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户信息表';

-- ============================================
-- 11. 交易订单表 (transaction_order_xxx)
-- 分库分表：n = business_no(numeric) % 100; dbIdx = n/10; tableIdx = n
-- 幂等键：(order_no, business_type, business_no)
-- Extra 字段存储：预生成的 voucher_no、tx_ids、完整请求参数快照（reqParams）
-- ============================================
-- transaction_order 主表：状态字段 + 固定大小业务元数据（VARCHAR，行宽可控）
-- 大块 extra 字段独立存储到 transaction_order_extra（见下文），热路径不访问 extra
CREATE TABLE IF NOT EXISTS `transaction_order_27` (
    `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`       VARCHAR(64)  NOT NULL COMMENT '外部幂等订单号（requestId）',
    `business_no`    VARCHAR(64)  NOT NULL COMMENT '业务订单号（分片键，按后2位路由）',
    `business_type`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '业务类型',
    `product_code`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '产品编码',
    `event_code`     VARCHAR(256) NOT NULL DEFAULT '' COMMENT '事件编码',
    `from_party_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '资金出方 owner id',
    `from_party_type` VARCHAR(32) NOT NULL DEFAULT '' COMMENT 'user | merchant | platform',
    `to_party_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '资金入方 owner id',
    `to_party_type`  VARCHAR(32)  NOT NULL DEFAULT '',
    `amount`         VARCHAR(32)  NOT NULL DEFAULT '0' COMMENT '金额（字符串存储）',
    `currency`       VARCHAR(8)   NOT NULL DEFAULT 'PHP',
    `status`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `retry_count`    INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `max_retry_count` INT         NOT NULL DEFAULT 3 COMMENT '最大允许重试次数',
    `voucher_no`     VARCHAR(64)  DEFAULT NULL COMMENT '记账凭证号（成功后写入）',
    `error_message`  VARCHAR(512) DEFAULT NULL COMMENT '最近一次失败原因',
    `description`    VARCHAR(256) DEFAULT NULL,
    `created_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_biz` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单主表（分库分表）—— extra 独立存储提升缓存命中率';

-- ============================================
-- 12. 交易订单扩展字段表 (transaction_order_extra_xxx)
-- 与 transaction_order_xxx 同分片（按 business_no 路由）
-- 存储：请求参数快照、预生成 ID（req_hash/voucher_no/tx_ids/req_params）
-- 热路径（状态查询/更新）不访问此表；仅创建/重试/审计时读写
-- extra VARCHAR(4096)：4096字符上限，代码层写入前强制截断
-- ============================================
CREATE TABLE IF NOT EXISTS `transaction_order_extra_27` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`   VARCHAR(64)  NOT NULL COMMENT '关联 transaction_order.order_no',
    `business_no` VARCHAR(64) NOT NULL COMMENT '分片键（与主表路由一致）',
    `business_type` VARCHAR(32) NOT NULL DEFAULT '',
    `extra`      VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON: {req_hash,voucher_no,tx_ids,req_params}，最长4096字符',
    `created_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_extra` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单扩展字段表（与主表同分片，热路径不访问）';


-- ============================================
-- 13. 缓冲余额更新表 (account_balance_buffer_xxx)
-- 与账户主表同分片（按 account_no 路由），用于平台/中间账户的批量余额聚合写入。
-- 设计：平台/中间账户允许负余额，写流水后先不更新 account.balance，
--        而是在此表累计增量；后台 flush worker 每100条或每30秒批量更新 account 余额，
--        减少高频账户对 account 表的锁竞争。
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_buffer_27` (
    `account_no`       VARCHAR(64)   NOT NULL COMMENT '账户号',
    `pending_delta`    BIGINT NOT NULL DEFAULT 0 COMMENT '待刷新的余额增量（可为负，ISO最小单位×100）',
    `pending_count`    INT           NOT NULL DEFAULT 0 COMMENT '待刷新的流水笔数',
    `flush_scheduled_at` DATETIME(3) NULL DEFAULT NULL COMMENT '下次计划刷新时间（NULL=立即刷新；worker 按此时间触发，执行后自动延后一个间隔）',
    `last_updated_at`  DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最后写入时间',
    PRIMARY KEY (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额缓冲聚合表（平台/中间账户缓冲记账）';

-- ============================================
-- TCC 全局协调者表 (tcc_coordinator_xxx)
-- 分库 + 分表：按 tcc_id 数字 hash(tcc_id) 路由到 (dbIndex, globalTableIndex)，
-- 与 account_transaction / tcc_transaction 路由规则一致，保证同一笔 booking 所有
-- 相关记录共享分片，避免跨分片事务。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_coordinator_27` (
    `tcc_id`        VARCHAR(64)  NOT NULL COMMENT 'TCC 全局事务 ID（= voucher_no）',
    `phase`         TINYINT      NOT NULL DEFAULT 1 COMMENT '全局阶段：1=TRYING 2=CONFIRMING 3=CONFIRMED 4=CANCELLED',
    `business_no`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '业务订单号',
    `branch_count`  SMALLINT     NOT NULL COMMENT '总分支数',
    `cut_date`      CHAR(10)     NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，避免 gorm parseTime 1 天位移 bug）。booking 入口处 computeCutDate 计算后 propagate；与 account_transaction.cut_date 一致。日切 drain 等待: WHERE cut_date <= X AND phase IN (TRYING, CONFIRMING) → 0',
    `currency`      VARCHAR(8)   NOT NULL DEFAULT '' COMMENT '币种（PHP/CNY/USD/...）。booking 入口写入；TCC Recovery 用 coord.currency 写补 confirm 流水，避免与原 booking 不同币种导致试算 PHP-only / currency-filter 不平。空字符串=旧数据兼容',
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`tcc_id`),
    KEY `idx_phase_updated` (`phase`, `updated_at`),
    KEY `idx_cut_date_phase` (`cut_date`, `phase`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 全局协调者（阶段追踪，100 张分表）';

-- ============================================
-- 索引优化说明
-- ============================================
-- 1. 所有分表都需要创建相同的表结构和索引
-- 2. 查询时优先使用分片键（user_id, account_no）
-- 3. 时间范围查询使用 transaction_date 索引
-- 4. 定期归档历史数据，保持表数据量在合理范围

-- ============================================
-- 13. 原子批量记账订单表 (batch_order)
-- 全局单表（不分库分表），记录原子批量记账的批次状态
-- 批次内各单笔记账通过 TCC 保证整体原子性：全部成功 or 全部回滚
-- extra VARCHAR(4096)：存储批次请求参数快照，最长4096字符
-- ============================================
CREATE TABLE IF NOT EXISTS `batch_order_27` (
    `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `batch_id`        VARCHAR(64)  NOT NULL COMMENT '批次幂等ID（调用方提供）',
    `business_no`     VARCHAR(64)  NOT NULL COMMENT '批次业务号',
    `item_count`      INT          NOT NULL DEFAULT 0 COMMENT '批次中单笔数量',
    `status`          TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `error_message`   VARCHAR(512) DEFAULT NULL COMMENT '失败原因',
    `description`     VARCHAR(256) DEFAULT NULL,
    `extra`           VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON批次请求参数快照，最长4096字符',
    `created_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_batch_id` (`batch_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='原子批量记账订单表（全局表）';

-- ============================================
-- 12. 结算 Outbox 表 (settlement_outbox)
-- 热路径事务预写日志（Transactional Outbox Pattern）
-- 分库，不分表：按 voucher_no 前1字符（dbIndex）路由，每库一张 settlement_outbox 表
-- ============================================
CREATE TABLE IF NOT EXISTS `settlement_outbox_27` (
    `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no`       VARCHAR(64)  NOT NULL COMMENT '凭证号（前3字符={1d-dbIdx}{2d-tableIdx}，用于路由）',
    `event_data`       LONGTEXT     NOT NULL COMMENT 'JSON: SettlementEvent',
    `transaction_date` CHAR(10)     NOT NULL COMMENT '交易日期 YYYY-MM-DD，供日切按天过滤 outbox delta',
    `cut_date`         CHAR(10)     NOT NULL DEFAULT '' COMMENT '日切归属日 YYYY-MM-DD（入口落库那一刻定死，永不重算）；空字符串=老数据按 transaction_date 兼容处理',
    `status`           TINYINT      NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=REDIS_DONE 2=MYSQL_DONE 3=FAILED',
    `retry_count`      INT          NOT NULL DEFAULT 0 COMMENT '重试次数',
    `error_msg`        TEXT         DEFAULT NULL COMMENT '最近一次失败原因',
    `created_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_date` (`status`, `transaction_date`),
    KEY `idx_status_created` (`status`, `created_at`),
    -- OutboxWorker MySQLWriter 高频轮询查询：
    --   SELECT * FROM settlement_outbox WHERE status = ? ORDER BY id ASC LIMIT 500
    -- (status, id) 复合索引让此查询走"索引范围扫描 + 已序输出"，免去 filesort。
    -- InnoDB 二级索引天然把 PK id 附在叶节点，但只有把 id 显式作为索引第二列，
    -- 优化器才会用 index range scan 满足 ORDER BY id；否则可能选 idx_status 后 sort。
    KEY `idx_status_id` (`status`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='热路径事务预写日志（Outbox）';

-- ============================================
-- 复式记账账户系统 - 数据库表设计
-- 分库分表架构：10库 × 每库10表 = 100张全局表
--
-- 统一路由规则：
--   n = id % 100
--   dbIndex        = n / 10      （物理库编号 0–9）
--   globalTableIdx = n           （全局表序号 0–99）
--
-- 表名映射（以 account 为例）：
--   DB 0 → account_00 ~ account_09
--   DB 1 → account_10 ~ account_19
--   ...
--   DB 9 → account_90 ~ account_99
-- ============================================

-- ============================================
-- 1. 账户主表 (account_xxx)
-- 分库分表：n = user_id % 100; dbIdx = n/10; tableIdx = n
-- ============================================


CREATE TABLE IF NOT EXISTS `account_28` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号（格式：{dbIdx:02d}{tableIdx:02d}{userId}-{businessType:03d}，编码路由信息）',
    `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
    `account_type` TINYINT NOT NULL COMMENT '账户类型：1-用户账户 2-商户账户 3-平台损益账户 4-中间账户',
    `account_category` VARCHAR(32) NOT NULL COMMENT '账户分类：ASSET资产/LIABILITY负债/EQUITY所有者权益/REVENUE收入/EXPENSE费用',
    `account_business_type` SMALLINT NOT NULL DEFAULT 0 COMMENT '账户业务类型：1-用户余额 2-商户结算 3-商户待结算（最大999）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `balance` BIGINT NOT NULL DEFAULT 0 COMMENT '当前余额（ISO最小单位×100，例如USD $3.42存为34200）',
    `frozen_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '冻结余额',
    `available_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '可用余额',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-禁用 1-正常 2-冻结',
    `account_group` CHAR(1) NOT NULL DEFAULT 'A' COMMENT '账户分组：A=默认/当前；B=轮换预创建的下一组。非轮换账户永远是 A',
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    -- 轮换字段（rotating-suspense-accounts 特性，见 docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md）
    -- 旧账户 logical_account_id=NULL 且 lifecycle_phase=0 (legacy)，写入路径与原行为一致。
    `logical_account_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '关联 logical_account.id；NULL=legacy 账户',
    `period_start` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期起点（包含），UTC',
    `period_end` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期终点（不含），UTC',
    `lifecycle_phase` TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=active 2=draining 3=frozen 4=archived 5=provisioned 9=quarantined',
    `draining_started_at` DATETIME DEFAULT NULL COMMENT '进入 draining 的时刻',
    `frozen_at` DATETIME DEFAULT NULL COMMENT '进入 frozen 的时刻',
    `archived_at` DATETIME DEFAULT NULL COMMENT '进入 archived 的时刻',
    `policy_version_at_birth` BIGINT DEFAULT NULL COMMENT '出生时锁定的 policy_version；后续策略变更不影响本 instance (E-28/E-29)',
    `effective_hard_timeout_secs` INT DEFAULT NULL COMMENT '策略覆盖：本 instance 专用 hard timeout (§11.X 紧急运维)',
    `override_reason` VARCHAR(255) DEFAULT NULL COMMENT 'override 原因',
    `override_by` VARCHAR(64) DEFAULT NULL COMMENT 'override 操作人',
    `override_at` DATETIME DEFAULT NULL COMMENT 'override 时刻',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`, `account_group`) COMMENT 'userId + 业务类型 + 币种 + 组 唯一键 (轮换 fleet 时 GroupA / GroupB 各 1 个)',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`),
    KEY `idx_logical_phase` (`logical_account_id`, `lifecycle_phase`),
    KEY `idx_phase_period_end` (`lifecycle_phase`, `period_end`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_28` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `transaction_id` VARCHAR(64) NOT NULL COMMENT '交易流水号',
    `parent_transaction_id` VARCHAR(64) DEFAULT NULL COMMENT '父交易流水号（用于关联复式记账的多条记录）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型：TRANSFER/PAYMENT/REFUND/WITHDRAW等',
    `debit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '借方金额（ISO最小单位×100）',
    `credit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '贷方金额（ISO最小单位×100）',
    `balance_before` BIGINT NOT NULL COMMENT '交易前余额',
    `balance_after` BIGINT NOT NULL COMMENT '交易后余额',
    `booking_type` TINYINT NOT NULL DEFAULT 1 COMMENT '记账方式：1-同步记账 2-缓冲记账',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `transaction_date` CHAR(10) NOT NULL COMMENT '交易日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑；ISO 格式按字典序与 DATE 同序，索引/范围查询无影响）',
    `transaction_time` DATETIME NOT NULL COMMENT '交易时间',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '交易描述',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-失败 1-成功 2-处理中',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，原因：DSN parseTime 让 gorm 做 TZ 转换 → 1 天位移 bug）。在 booking 入口由 computeCutDate(now, scheduled_time, tz) 一次性确定，propagate 到所有 entry + tcc_coordinator。日切扫描 WHERE cut_date=X AND status=1，同 voucher 所有 entry 共享同一 cut_date → 试算平衡天然成立',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `ext_info` JSON DEFAULT NULL COMMENT '扩展信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_transaction_id` (`transaction_id`),
    KEY `idx_account_no` (`account_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_parent_transaction_id` (`parent_transaction_id`),
    KEY `idx_status` (`status`),
    -- 复合索引：账户流水分页查询（account_no + 日期范围 + 时间排序），取代独立的 idx_account_no + idx_transaction_date
    KEY `idx_account_date_time` (`account_no`, `transaction_date`, `transaction_time`),
    -- 日切扫描主索引：WHERE cut_date=X AND status=1
    KEY `idx_cut_date_status` (`cut_date`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户流水表';

-- ============================================
-- 3. 复式记账凭证表 (accounting_voucher_xxx)
-- 一笔业务对应一张凭证，凭证关联多条分录
-- 分库分表：按 voucher_no 前3字符路由（与 business_no 同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `accounting_voucher_28` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '凭证号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型',
    `total_debit` BIGINT NOT NULL COMMENT '借方总金额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL COMMENT '贷方总金额（ISO最小单位×100）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `voucher_date` CHAR(10) NOT NULL COMMENT '凭证日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-作废 1-生效 2-待审核',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '凭证描述',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_voucher_date` (`voucher_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='复式记账凭证表';

-- ============================================
-- 4. 账户余额快照表 (account_balance_snapshot_xxx)
-- 每日日切生成快照
-- 分库分表：按 account_no 前3字符路由（与账户主表同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_28` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `snapshot_date` CHAR(10) NOT NULL COMMENT '快照日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '关联日切版本号，重跑时递增',
    `beginning_balance` BIGINT NOT NULL COMMENT '期初余额（ISO最小单位×100）',
    `ending_balance` BIGINT NOT NULL COMMENT '期末余额（ISO最小单位×100）',
    `total_debit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日借方总额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日贷方总额（ISO最小单位×100）',
    `transaction_count` INT NOT NULL DEFAULT 0 COMMENT '当日交易笔数',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '该账户本日最后一笔流水ID，仅供审计/对账使用',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_snapshot` (`account_no`, `snapshot_date`, `run_id`),
    KEY `idx_snapshot_date` (`snapshot_date`),
    KEY `idx_snapshot_date_run` (`snapshot_date`, `run_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额快照表';

-- ============================================
-- 5. 日切控制表 (day_cut_control)
-- 全局表，不分库分表，记录每个库每张表的日切点
-- ============================================
CREATE TABLE IF NOT EXISTS `day_cut_control_28` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `database_index` TINYINT NOT NULL COMMENT '库索引 0-9',
    `table_index` SMALLINT NOT NULL COMMENT '表索引 0-99',
    `table_name` VARCHAR(64) NOT NULL COMMENT '表名',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 避开 DATE TZ 转换坑）。扫描走 account_transaction.cut_date = 本字段',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '运行版本号，同一日期每次重跑时递增',
    `last_processed_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '本 run 在分片上已处理到的 account_transaction.id（含）。chunk 内分页游标',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '本 run 完成时最后处理的 transaction_id，仅供审计',
    `currency` CHAR(3) NOT NULL DEFAULT '' COMMENT '币种过滤。空 = 全部币种（兼容历史）；非空 = 仅该币种账户',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-未开始 1-进行中 2-已完成 3-失败',
    `start_time` DATETIME DEFAULT NULL COMMENT '开始时间',
    `end_time` DATETIME DEFAULT NULL COMMENT '结束时间',
    `cut_time` DATETIME(3) DEFAULT NULL COMMENT '日切完成时的服务器墙钟时间，仅供审计',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_day_cut` (`database_index`, `table_index`, `cut_date`, `run_id`),
    KEY `idx_cut_date` (`cut_date`),
    KEY `idx_cut_date_run` (`cut_date`, `run_id`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='日切控制表';

-- ============================================
-- 6. 异步任务表 (async_task)
-- 用于Kafka消息消费失败后的重试
-- 全局表，不分库分表
-- ============================================
CREATE TABLE IF NOT EXISTS `async_task_28` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `task_id` VARCHAR(64) NOT NULL COMMENT '任务ID',
    `task_type` VARCHAR(32) NOT NULL COMMENT '任务类型：ACCOUNTING/SNAPSHOT/DAY_CUT',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `task_data` JSON NOT NULL COMMENT '任务数据',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-待处理 1-处理中 2-成功 3-失败',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `max_retry_count` INT NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `next_retry_time` DATETIME DEFAULT NULL COMMENT '下次重试时间',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status_retry` (`status`, `next_retry_time`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='异步任务表';

-- ============================================
-- 7. TCC 分支记录表 (tcc_transaction_xxx)
-- 与账户表同分片（按 account_no 路由），保证 Try/Confirm/Cancel 在单库事务内完成。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_transaction_28` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `tcc_id` VARCHAR(64) NOT NULL COMMENT '全局 TCC ID（= voucherNo）',
    `branch_id` VARCHAR(64) NOT NULL COMMENT 'TCC 分支 ID（= transactionID）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `balance_delta` BIGINT NOT NULL COMMENT 'Confirm 时对 balance 的增量（负=减少，ISO最小单位×100）',
    `frozen_amount` BIGINT NOT NULL DEFAULT 0 COMMENT 'Try 时冻结的可用余额（0=无需冻结）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-TRYING 1-CONFIRMED 2-CANCELLED',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_branch_id` (`branch_id`),
    KEY `idx_tcc_id` (`tcc_id`),
    KEY `idx_account_no` (`account_no`),
    -- 复合索引：卡住 TCC 分支查询（status=TRYING AND created_at < cutoff），避免全表扫描
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 分布式事务分支记录表';

-- ============================================
-- 7b. UnfreezeAndDebit 补偿 outbox (freeze_compensate_outbox_xxx)
--
-- 与 frozen account 同分片（按 voucher_no 前 3 字符 = dbIdx + tableIdx 路由），
-- 保证 Phase 1 frozen-debit + INSERT outbox PENDING 在单库事务原子。
--
-- 状态机：
--   0 PENDING  : 已写入；可能被 caller mark DONE 或 worker 接管补偿
--   1 DONE     : 终态；caller Phase 2 全成功 或 worker 补偿全成功
--   2 FAILED   : 终态；worker 重试到达上限，需人工处理
--
-- payload (JSON) 包含完整 reverse plan：frozen account 信息 + 全部 credit
-- entries 信息 + 已成功的 credit txID 列表（worker 据此精确反向）。
--
-- worker 选取规则：status=PENDING AND created_at < now - 60s（给 caller
-- 一分钟时间走完 Phase 2 + 标记 DONE，避免误抢）；CAS UPDATE PENDING → COMPENSATING
-- 防多 pod / 多 worker 重复处理。
-- ============================================
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_28` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '关联的 unfreeze voucher（=路由键，前3字符为 dbIdx+tableIdx）',
    `freeze_order_no` VARCHAR(64) NOT NULL COMMENT '关联的 freeze_order 号（运维诊断用）',
    `payload` MEDIUMTEXT NOT NULL COMMENT 'JSON：完整 reverse plan（frozen acct + credit entries + 已成功 txIDs）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=DONE 2=COMPENSATING 3=FAILED',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '补偿重试次数；达 max_retry 置 FAILED',
    `error_msg` VARCHAR(500) DEFAULT NULL COMMENT '最近一次失败原因（FAILED 状态用于审计）',
    `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
    `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UnfreezeAndDebit 补偿 outbox（保证最终一致性）';

-- ============================================
-- 8. 分布式锁表 (distributed_lock)
-- 用于日切等场景的分布式锁
-- ============================================
CREATE TABLE IF NOT EXISTS `distributed_lock_28` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `lock_key` VARCHAR(128) NOT NULL COMMENT '锁键',
    `lock_value` VARCHAR(64) NOT NULL COMMENT '锁值（UUID）',
    `expire_time` DATETIME NOT NULL COMMENT '过期时间',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lock_key` (`lock_key`),
    KEY `idx_expire_time` (`expire_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='分布式锁表';


-- ============================================
-- 10. 商户信息表 (merchant_info)
-- ============================================
CREATE TABLE IF NOT EXISTS `merchant_info_28` (
    `id`                   BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `merchant_id`          BIGINT(20) UNSIGNED NOT NULL COMMENT '商户唯一ID',
    `merchant_name`        VARCHAR(256) NOT NULL COMMENT '商户名称',
    `account_idc`          VARCHAR(32)  NOT NULL COMMENT '商户账户所属 IDC',
    `external_merchant_id` VARCHAR(256) NOT NULL COMMENT '外部商户ID',
    `mid`                  VARCHAR(64)  NOT NULL COMMENT '业务线标识',
    `business_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务ID',
    `extra`                TEXT DEFAULT NULL,
    `byte_rds_ctx`         TEXT DEFAULT NULL,
    `create_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_merchant_id` (`merchant_id`),
    UNIQUE KEY `uniq_external_merchant_id_mid` (`external_merchant_id`, `mid`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户信息表';

-- ============================================
-- 11. 交易订单表 (transaction_order_xxx)
-- 分库分表：n = business_no(numeric) % 100; dbIdx = n/10; tableIdx = n
-- 幂等键：(order_no, business_type, business_no)
-- Extra 字段存储：预生成的 voucher_no、tx_ids、完整请求参数快照（reqParams）
-- ============================================
-- transaction_order 主表：状态字段 + 固定大小业务元数据（VARCHAR，行宽可控）
-- 大块 extra 字段独立存储到 transaction_order_extra（见下文），热路径不访问 extra
CREATE TABLE IF NOT EXISTS `transaction_order_28` (
    `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`       VARCHAR(64)  NOT NULL COMMENT '外部幂等订单号（requestId）',
    `business_no`    VARCHAR(64)  NOT NULL COMMENT '业务订单号（分片键，按后2位路由）',
    `business_type`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '业务类型',
    `product_code`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '产品编码',
    `event_code`     VARCHAR(256) NOT NULL DEFAULT '' COMMENT '事件编码',
    `from_party_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '资金出方 owner id',
    `from_party_type` VARCHAR(32) NOT NULL DEFAULT '' COMMENT 'user | merchant | platform',
    `to_party_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '资金入方 owner id',
    `to_party_type`  VARCHAR(32)  NOT NULL DEFAULT '',
    `amount`         VARCHAR(32)  NOT NULL DEFAULT '0' COMMENT '金额（字符串存储）',
    `currency`       VARCHAR(8)   NOT NULL DEFAULT 'PHP',
    `status`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `retry_count`    INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `max_retry_count` INT         NOT NULL DEFAULT 3 COMMENT '最大允许重试次数',
    `voucher_no`     VARCHAR(64)  DEFAULT NULL COMMENT '记账凭证号（成功后写入）',
    `error_message`  VARCHAR(512) DEFAULT NULL COMMENT '最近一次失败原因',
    `description`    VARCHAR(256) DEFAULT NULL,
    `created_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_biz` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单主表（分库分表）—— extra 独立存储提升缓存命中率';

-- ============================================
-- 12. 交易订单扩展字段表 (transaction_order_extra_xxx)
-- 与 transaction_order_xxx 同分片（按 business_no 路由）
-- 存储：请求参数快照、预生成 ID（req_hash/voucher_no/tx_ids/req_params）
-- 热路径（状态查询/更新）不访问此表；仅创建/重试/审计时读写
-- extra VARCHAR(4096)：4096字符上限，代码层写入前强制截断
-- ============================================
CREATE TABLE IF NOT EXISTS `transaction_order_extra_28` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`   VARCHAR(64)  NOT NULL COMMENT '关联 transaction_order.order_no',
    `business_no` VARCHAR(64) NOT NULL COMMENT '分片键（与主表路由一致）',
    `business_type` VARCHAR(32) NOT NULL DEFAULT '',
    `extra`      VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON: {req_hash,voucher_no,tx_ids,req_params}，最长4096字符',
    `created_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_extra` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单扩展字段表（与主表同分片，热路径不访问）';


-- ============================================
-- 13. 缓冲余额更新表 (account_balance_buffer_xxx)
-- 与账户主表同分片（按 account_no 路由），用于平台/中间账户的批量余额聚合写入。
-- 设计：平台/中间账户允许负余额，写流水后先不更新 account.balance，
--        而是在此表累计增量；后台 flush worker 每100条或每30秒批量更新 account 余额，
--        减少高频账户对 account 表的锁竞争。
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_buffer_28` (
    `account_no`       VARCHAR(64)   NOT NULL COMMENT '账户号',
    `pending_delta`    BIGINT NOT NULL DEFAULT 0 COMMENT '待刷新的余额增量（可为负，ISO最小单位×100）',
    `pending_count`    INT           NOT NULL DEFAULT 0 COMMENT '待刷新的流水笔数',
    `flush_scheduled_at` DATETIME(3) NULL DEFAULT NULL COMMENT '下次计划刷新时间（NULL=立即刷新；worker 按此时间触发，执行后自动延后一个间隔）',
    `last_updated_at`  DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最后写入时间',
    PRIMARY KEY (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额缓冲聚合表（平台/中间账户缓冲记账）';

-- ============================================
-- TCC 全局协调者表 (tcc_coordinator_xxx)
-- 分库 + 分表：按 tcc_id 数字 hash(tcc_id) 路由到 (dbIndex, globalTableIndex)，
-- 与 account_transaction / tcc_transaction 路由规则一致，保证同一笔 booking 所有
-- 相关记录共享分片，避免跨分片事务。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_coordinator_28` (
    `tcc_id`        VARCHAR(64)  NOT NULL COMMENT 'TCC 全局事务 ID（= voucher_no）',
    `phase`         TINYINT      NOT NULL DEFAULT 1 COMMENT '全局阶段：1=TRYING 2=CONFIRMING 3=CONFIRMED 4=CANCELLED',
    `business_no`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '业务订单号',
    `branch_count`  SMALLINT     NOT NULL COMMENT '总分支数',
    `cut_date`      CHAR(10)     NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，避免 gorm parseTime 1 天位移 bug）。booking 入口处 computeCutDate 计算后 propagate；与 account_transaction.cut_date 一致。日切 drain 等待: WHERE cut_date <= X AND phase IN (TRYING, CONFIRMING) → 0',
    `currency`      VARCHAR(8)   NOT NULL DEFAULT '' COMMENT '币种（PHP/CNY/USD/...）。booking 入口写入；TCC Recovery 用 coord.currency 写补 confirm 流水，避免与原 booking 不同币种导致试算 PHP-only / currency-filter 不平。空字符串=旧数据兼容',
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`tcc_id`),
    KEY `idx_phase_updated` (`phase`, `updated_at`),
    KEY `idx_cut_date_phase` (`cut_date`, `phase`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 全局协调者（阶段追踪，100 张分表）';

-- ============================================
-- 索引优化说明
-- ============================================
-- 1. 所有分表都需要创建相同的表结构和索引
-- 2. 查询时优先使用分片键（user_id, account_no）
-- 3. 时间范围查询使用 transaction_date 索引
-- 4. 定期归档历史数据，保持表数据量在合理范围

-- ============================================
-- 13. 原子批量记账订单表 (batch_order)
-- 全局单表（不分库分表），记录原子批量记账的批次状态
-- 批次内各单笔记账通过 TCC 保证整体原子性：全部成功 or 全部回滚
-- extra VARCHAR(4096)：存储批次请求参数快照，最长4096字符
-- ============================================
CREATE TABLE IF NOT EXISTS `batch_order_28` (
    `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `batch_id`        VARCHAR(64)  NOT NULL COMMENT '批次幂等ID（调用方提供）',
    `business_no`     VARCHAR(64)  NOT NULL COMMENT '批次业务号',
    `item_count`      INT          NOT NULL DEFAULT 0 COMMENT '批次中单笔数量',
    `status`          TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `error_message`   VARCHAR(512) DEFAULT NULL COMMENT '失败原因',
    `description`     VARCHAR(256) DEFAULT NULL,
    `extra`           VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON批次请求参数快照，最长4096字符',
    `created_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_batch_id` (`batch_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='原子批量记账订单表（全局表）';

-- ============================================
-- 12. 结算 Outbox 表 (settlement_outbox)
-- 热路径事务预写日志（Transactional Outbox Pattern）
-- 分库，不分表：按 voucher_no 前1字符（dbIndex）路由，每库一张 settlement_outbox 表
-- ============================================
CREATE TABLE IF NOT EXISTS `settlement_outbox_28` (
    `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no`       VARCHAR(64)  NOT NULL COMMENT '凭证号（前3字符={1d-dbIdx}{2d-tableIdx}，用于路由）',
    `event_data`       LONGTEXT     NOT NULL COMMENT 'JSON: SettlementEvent',
    `transaction_date` CHAR(10)     NOT NULL COMMENT '交易日期 YYYY-MM-DD，供日切按天过滤 outbox delta',
    `cut_date`         CHAR(10)     NOT NULL DEFAULT '' COMMENT '日切归属日 YYYY-MM-DD（入口落库那一刻定死，永不重算）；空字符串=老数据按 transaction_date 兼容处理',
    `status`           TINYINT      NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=REDIS_DONE 2=MYSQL_DONE 3=FAILED',
    `retry_count`      INT          NOT NULL DEFAULT 0 COMMENT '重试次数',
    `error_msg`        TEXT         DEFAULT NULL COMMENT '最近一次失败原因',
    `created_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_date` (`status`, `transaction_date`),
    KEY `idx_status_created` (`status`, `created_at`),
    -- OutboxWorker MySQLWriter 高频轮询查询：
    --   SELECT * FROM settlement_outbox WHERE status = ? ORDER BY id ASC LIMIT 500
    -- (status, id) 复合索引让此查询走"索引范围扫描 + 已序输出"，免去 filesort。
    -- InnoDB 二级索引天然把 PK id 附在叶节点，但只有把 id 显式作为索引第二列，
    -- 优化器才会用 index range scan 满足 ORDER BY id；否则可能选 idx_status 后 sort。
    KEY `idx_status_id` (`status`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='热路径事务预写日志（Outbox）';

-- ============================================
-- 复式记账账户系统 - 数据库表设计
-- 分库分表架构：10库 × 每库10表 = 100张全局表
--
-- 统一路由规则：
--   n = id % 100
--   dbIndex        = n / 10      （物理库编号 0–9）
--   globalTableIdx = n           （全局表序号 0–99）
--
-- 表名映射（以 account 为例）：
--   DB 0 → account_00 ~ account_09
--   DB 1 → account_10 ~ account_19
--   ...
--   DB 9 → account_90 ~ account_99
-- ============================================

-- ============================================
-- 1. 账户主表 (account_xxx)
-- 分库分表：n = user_id % 100; dbIdx = n/10; tableIdx = n
-- ============================================


CREATE TABLE IF NOT EXISTS `account_29` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号（格式：{dbIdx:02d}{tableIdx:02d}{userId}-{businessType:03d}，编码路由信息）',
    `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
    `account_type` TINYINT NOT NULL COMMENT '账户类型：1-用户账户 2-商户账户 3-平台损益账户 4-中间账户',
    `account_category` VARCHAR(32) NOT NULL COMMENT '账户分类：ASSET资产/LIABILITY负债/EQUITY所有者权益/REVENUE收入/EXPENSE费用',
    `account_business_type` SMALLINT NOT NULL DEFAULT 0 COMMENT '账户业务类型：1-用户余额 2-商户结算 3-商户待结算（最大999）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `balance` BIGINT NOT NULL DEFAULT 0 COMMENT '当前余额（ISO最小单位×100，例如USD $3.42存为34200）',
    `frozen_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '冻结余额',
    `available_balance` BIGINT NOT NULL DEFAULT 0 COMMENT '可用余额',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-禁用 1-正常 2-冻结',
    `account_group` CHAR(1) NOT NULL DEFAULT 'A' COMMENT '账户分组：A=默认/当前；B=轮换预创建的下一组。非轮换账户永远是 A',
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    -- 轮换字段（rotating-suspense-accounts 特性，见 docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md）
    -- 旧账户 logical_account_id=NULL 且 lifecycle_phase=0 (legacy)，写入路径与原行为一致。
    `logical_account_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '关联 logical_account.id；NULL=legacy 账户',
    `period_start` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期起点（包含），UTC',
    `period_end` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期终点（不含），UTC',
    `lifecycle_phase` TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=active 2=draining 3=frozen 4=archived 5=provisioned 9=quarantined',
    `draining_started_at` DATETIME DEFAULT NULL COMMENT '进入 draining 的时刻',
    `frozen_at` DATETIME DEFAULT NULL COMMENT '进入 frozen 的时刻',
    `archived_at` DATETIME DEFAULT NULL COMMENT '进入 archived 的时刻',
    `policy_version_at_birth` BIGINT DEFAULT NULL COMMENT '出生时锁定的 policy_version；后续策略变更不影响本 instance (E-28/E-29)',
    `effective_hard_timeout_secs` INT DEFAULT NULL COMMENT '策略覆盖：本 instance 专用 hard timeout (§11.X 紧急运维)',
    `override_reason` VARCHAR(255) DEFAULT NULL COMMENT 'override 原因',
    `override_by` VARCHAR(64) DEFAULT NULL COMMENT 'override 操作人',
    `override_at` DATETIME DEFAULT NULL COMMENT 'override 时刻',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`, `account_group`) COMMENT 'userId + 业务类型 + 币种 + 组 唯一键 (轮换 fleet 时 GroupA / GroupB 各 1 个)',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`),
    KEY `idx_logical_phase` (`logical_account_id`, `lifecycle_phase`),
    KEY `idx_phase_period_end` (`lifecycle_phase`, `period_end`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_29` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `transaction_id` VARCHAR(64) NOT NULL COMMENT '交易流水号',
    `parent_transaction_id` VARCHAR(64) DEFAULT NULL COMMENT '父交易流水号（用于关联复式记账的多条记录）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型：TRANSFER/PAYMENT/REFUND/WITHDRAW等',
    `debit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '借方金额（ISO最小单位×100）',
    `credit_amount` BIGINT NOT NULL DEFAULT 0 COMMENT '贷方金额（ISO最小单位×100）',
    `balance_before` BIGINT NOT NULL COMMENT '交易前余额',
    `balance_after` BIGINT NOT NULL COMMENT '交易后余额',
    `booking_type` TINYINT NOT NULL DEFAULT 1 COMMENT '记账方式：1-同步记账 2-缓冲记账',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `transaction_date` CHAR(10) NOT NULL COMMENT '交易日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑；ISO 格式按字典序与 DATE 同序，索引/范围查询无影响）',
    `transaction_time` DATETIME NOT NULL COMMENT '交易时间',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '交易描述',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-失败 1-成功 2-处理中',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，原因：DSN parseTime 让 gorm 做 TZ 转换 → 1 天位移 bug）。在 booking 入口由 computeCutDate(now, scheduled_time, tz) 一次性确定，propagate 到所有 entry + tcc_coordinator。日切扫描 WHERE cut_date=X AND status=1，同 voucher 所有 entry 共享同一 cut_date → 试算平衡天然成立',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `ext_info` JSON DEFAULT NULL COMMENT '扩展信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_transaction_id` (`transaction_id`),
    KEY `idx_account_no` (`account_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_parent_transaction_id` (`parent_transaction_id`),
    KEY `idx_status` (`status`),
    -- 复合索引：账户流水分页查询（account_no + 日期范围 + 时间排序），取代独立的 idx_account_no + idx_transaction_date
    KEY `idx_account_date_time` (`account_no`, `transaction_date`, `transaction_time`),
    -- 日切扫描主索引：WHERE cut_date=X AND status=1
    KEY `idx_cut_date_status` (`cut_date`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户流水表';

-- ============================================
-- 3. 复式记账凭证表 (accounting_voucher_xxx)
-- 一笔业务对应一张凭证，凭证关联多条分录
-- 分库分表：按 voucher_no 前3字符路由（与 business_no 同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `accounting_voucher_29` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '凭证号',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `business_type` VARCHAR(32) NOT NULL COMMENT '业务类型',
    `total_debit` BIGINT NOT NULL COMMENT '借方总金额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL COMMENT '贷方总金额（ISO最小单位×100）',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `voucher_date` CHAR(10) NOT NULL COMMENT '凭证日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：0-作废 1-生效 2-待审核',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '凭证描述',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_voucher_date` (`voucher_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='复式记账凭证表';

-- ============================================
-- 4. 账户余额快照表 (account_balance_snapshot_xxx)
-- 每日日切生成快照
-- 分库分表：按 account_no 前3字符路由（与账户主表同分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_29` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `snapshot_date` CHAR(10) NOT NULL COMMENT '快照日期 YYYY-MM-DD（CHAR(10) 避开 gorm parseTime 1 天位移坑）',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '关联日切版本号，重跑时递增',
    `beginning_balance` BIGINT NOT NULL COMMENT '期初余额（ISO最小单位×100）',
    `ending_balance` BIGINT NOT NULL COMMENT '期末余额（ISO最小单位×100）',
    `total_debit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日借方总额（ISO最小单位×100）',
    `total_credit` BIGINT NOT NULL DEFAULT 0 COMMENT '当日贷方总额（ISO最小单位×100）',
    `transaction_count` INT NOT NULL DEFAULT 0 COMMENT '当日交易笔数',
    `currency` VARCHAR(8) NOT NULL DEFAULT 'PHP' COMMENT '币种',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '该账户本日最后一笔流水ID，仅供审计/对账使用',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_snapshot` (`account_no`, `snapshot_date`, `run_id`),
    KEY `idx_snapshot_date` (`snapshot_date`),
    KEY `idx_snapshot_date_run` (`snapshot_date`, `run_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额快照表';

-- ============================================
-- 5. 日切控制表 (day_cut_control)
-- 全局表，不分库分表，记录每个库每张表的日切点
-- ============================================
CREATE TABLE IF NOT EXISTS `day_cut_control_29` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `database_index` TINYINT NOT NULL COMMENT '库索引 0-9',
    `table_index` SMALLINT NOT NULL COMMENT '表索引 0-99',
    `table_name` VARCHAR(64) NOT NULL COMMENT '表名',
    `cut_date` CHAR(10) NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 避开 DATE TZ 转换坑）。扫描走 account_transaction.cut_date = 本字段',
    `run_id` INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '运行版本号，同一日期每次重跑时递增',
    `last_processed_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '本 run 在分片上已处理到的 account_transaction.id（含）。chunk 内分页游标',
    `last_transaction_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '本 run 完成时最后处理的 transaction_id，仅供审计',
    `currency` CHAR(3) NOT NULL DEFAULT '' COMMENT '币种过滤。空 = 全部币种（兼容历史）；非空 = 仅该币种账户',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-未开始 1-进行中 2-已完成 3-失败',
    `start_time` DATETIME DEFAULT NULL COMMENT '开始时间',
    `end_time` DATETIME DEFAULT NULL COMMENT '结束时间',
    `cut_time` DATETIME(3) DEFAULT NULL COMMENT '日切完成时的服务器墙钟时间，仅供审计',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_day_cut` (`database_index`, `table_index`, `cut_date`, `run_id`),
    KEY `idx_cut_date` (`cut_date`),
    KEY `idx_cut_date_run` (`cut_date`, `run_id`),
    KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='日切控制表';

-- ============================================
-- 6. 异步任务表 (async_task)
-- 用于Kafka消息消费失败后的重试
-- 全局表，不分库分表
-- ============================================
CREATE TABLE IF NOT EXISTS `async_task_29` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `task_id` VARCHAR(64) NOT NULL COMMENT '任务ID',
    `task_type` VARCHAR(32) NOT NULL COMMENT '任务类型：ACCOUNTING/SNAPSHOT/DAY_CUT',
    `business_no` VARCHAR(64) NOT NULL COMMENT '业务订单号',
    `task_data` JSON NOT NULL COMMENT '任务数据',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-待处理 1-处理中 2-成功 3-失败',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '重试次数',
    `max_retry_count` INT NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `next_retry_time` DATETIME DEFAULT NULL COMMENT '下次重试时间',
    `error_message` VARCHAR(512) DEFAULT NULL COMMENT '错误信息',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status_retry` (`status`, `next_retry_time`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='异步任务表';

-- ============================================
-- 7. TCC 分支记录表 (tcc_transaction_xxx)
-- 与账户表同分片（按 account_no 路由），保证 Try/Confirm/Cancel 在单库事务内完成。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_transaction_29` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `tcc_id` VARCHAR(64) NOT NULL COMMENT '全局 TCC ID（= voucherNo）',
    `branch_id` VARCHAR(64) NOT NULL COMMENT 'TCC 分支 ID（= transactionID）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '账户号',
    `balance_delta` BIGINT NOT NULL COMMENT 'Confirm 时对 balance 的增量（负=减少，ISO最小单位×100）',
    `frozen_amount` BIGINT NOT NULL DEFAULT 0 COMMENT 'Try 时冻结的可用余额（0=无需冻结）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '状态：0-TRYING 1-CONFIRMED 2-CANCELLED',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_branch_id` (`branch_id`),
    KEY `idx_tcc_id` (`tcc_id`),
    KEY `idx_account_no` (`account_no`),
    -- 复合索引：卡住 TCC 分支查询（status=TRYING AND created_at < cutoff），避免全表扫描
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 分布式事务分支记录表';

-- ============================================
-- 7b. UnfreezeAndDebit 补偿 outbox (freeze_compensate_outbox_xxx)
--
-- 与 frozen account 同分片（按 voucher_no 前 3 字符 = dbIdx + tableIdx 路由），
-- 保证 Phase 1 frozen-debit + INSERT outbox PENDING 在单库事务原子。
--
-- 状态机：
--   0 PENDING  : 已写入；可能被 caller mark DONE 或 worker 接管补偿
--   1 DONE     : 终态；caller Phase 2 全成功 或 worker 补偿全成功
--   2 FAILED   : 终态；worker 重试到达上限，需人工处理
--
-- payload (JSON) 包含完整 reverse plan：frozen account 信息 + 全部 credit
-- entries 信息 + 已成功的 credit txID 列表（worker 据此精确反向）。
--
-- worker 选取规则：status=PENDING AND created_at < now - 60s（给 caller
-- 一分钟时间走完 Phase 2 + 标记 DONE，避免误抢）；CAS UPDATE PENDING → COMPENSATING
-- 防多 pod / 多 worker 重复处理。
-- ============================================
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_29` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no` VARCHAR(64) NOT NULL COMMENT '关联的 unfreeze voucher（=路由键，前3字符为 dbIdx+tableIdx）',
    `freeze_order_no` VARCHAR(64) NOT NULL COMMENT '关联的 freeze_order 号（运维诊断用）',
    `payload` MEDIUMTEXT NOT NULL COMMENT 'JSON：完整 reverse plan（frozen acct + credit entries + 已成功 txIDs）',
    `status` TINYINT NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=DONE 2=COMPENSATING 3=FAILED',
    `retry_count` INT NOT NULL DEFAULT 0 COMMENT '补偿重试次数；达 max_retry 置 FAILED',
    `error_msg` VARCHAR(500) DEFAULT NULL COMMENT '最近一次失败原因（FAILED 状态用于审计）',
    `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
    `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UnfreezeAndDebit 补偿 outbox（保证最终一致性）';

-- ============================================
-- 8. 分布式锁表 (distributed_lock)
-- 用于日切等场景的分布式锁
-- ============================================
CREATE TABLE IF NOT EXISTS `distributed_lock_29` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `lock_key` VARCHAR(128) NOT NULL COMMENT '锁键',
    `lock_value` VARCHAR(64) NOT NULL COMMENT '锁值（UUID）',
    `expire_time` DATETIME NOT NULL COMMENT '过期时间',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lock_key` (`lock_key`),
    KEY `idx_expire_time` (`expire_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='分布式锁表';


-- ============================================
-- 10. 商户信息表 (merchant_info)
-- ============================================
CREATE TABLE IF NOT EXISTS `merchant_info_29` (
    `id`                   BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `merchant_id`          BIGINT(20) UNSIGNED NOT NULL COMMENT '商户唯一ID',
    `merchant_name`        VARCHAR(256) NOT NULL COMMENT '商户名称',
    `account_idc`          VARCHAR(32)  NOT NULL COMMENT '商户账户所属 IDC',
    `external_merchant_id` VARCHAR(256) NOT NULL COMMENT '外部商户ID',
    `mid`                  VARCHAR(64)  NOT NULL COMMENT '业务线标识',
    `business_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务ID',
    `extra`                TEXT DEFAULT NULL,
    `byte_rds_ctx`         TEXT DEFAULT NULL,
    `create_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`          TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_merchant_id` (`merchant_id`),
    UNIQUE KEY `uniq_external_merchant_id_mid` (`external_merchant_id`, `mid`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户信息表';

-- ============================================
-- 11. 交易订单表 (transaction_order_xxx)
-- 分库分表：n = business_no(numeric) % 100; dbIdx = n/10; tableIdx = n
-- 幂等键：(order_no, business_type, business_no)
-- Extra 字段存储：预生成的 voucher_no、tx_ids、完整请求参数快照（reqParams）
-- ============================================
-- transaction_order 主表：状态字段 + 固定大小业务元数据（VARCHAR，行宽可控）
-- 大块 extra 字段独立存储到 transaction_order_extra（见下文），热路径不访问 extra
CREATE TABLE IF NOT EXISTS `transaction_order_29` (
    `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`       VARCHAR(64)  NOT NULL COMMENT '外部幂等订单号（requestId）',
    `business_no`    VARCHAR(64)  NOT NULL COMMENT '业务订单号（分片键，按后2位路由）',
    `business_type`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '业务类型',
    `product_code`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '产品编码',
    `event_code`     VARCHAR(256) NOT NULL DEFAULT '' COMMENT '事件编码',
    `from_party_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '资金出方 owner id',
    `from_party_type` VARCHAR(32) NOT NULL DEFAULT '' COMMENT 'user | merchant | platform',
    `to_party_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '资金入方 owner id',
    `to_party_type`  VARCHAR(32)  NOT NULL DEFAULT '',
    `amount`         VARCHAR(32)  NOT NULL DEFAULT '0' COMMENT '金额（字符串存储）',
    `currency`       VARCHAR(8)   NOT NULL DEFAULT 'PHP',
    `status`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `retry_count`    INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `max_retry_count` INT         NOT NULL DEFAULT 3 COMMENT '最大允许重试次数',
    `voucher_no`     VARCHAR(64)  DEFAULT NULL COMMENT '记账凭证号（成功后写入）',
    `error_message`  VARCHAR(512) DEFAULT NULL COMMENT '最近一次失败原因',
    `description`    VARCHAR(256) DEFAULT NULL,
    `created_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_biz` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单主表（分库分表）—— extra 独立存储提升缓存命中率';

-- ============================================
-- 12. 交易订单扩展字段表 (transaction_order_extra_xxx)
-- 与 transaction_order_xxx 同分片（按 business_no 路由）
-- 存储：请求参数快照、预生成 ID（req_hash/voucher_no/tx_ids/req_params）
-- 热路径（状态查询/更新）不访问此表；仅创建/重试/审计时读写
-- extra VARCHAR(4096)：4096字符上限，代码层写入前强制截断
-- ============================================
CREATE TABLE IF NOT EXISTS `transaction_order_extra_29` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `order_no`   VARCHAR(64)  NOT NULL COMMENT '关联 transaction_order.order_no',
    `business_no` VARCHAR(64) NOT NULL COMMENT '分片键（与主表路由一致）',
    `business_type` VARCHAR(32) NOT NULL DEFAULT '',
    `extra`      VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON: {req_hash,voucher_no,tx_ids,req_params}，最长4096字符',
    `created_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_order_extra` (`order_no`, `business_type`, `business_no`),
    KEY `idx_business_no` (`business_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易订单扩展字段表（与主表同分片，热路径不访问）';


-- ============================================
-- 13. 缓冲余额更新表 (account_balance_buffer_xxx)
-- 与账户主表同分片（按 account_no 路由），用于平台/中间账户的批量余额聚合写入。
-- 设计：平台/中间账户允许负余额，写流水后先不更新 account.balance，
--        而是在此表累计增量；后台 flush worker 每100条或每30秒批量更新 account 余额，
--        减少高频账户对 account 表的锁竞争。
-- ============================================
CREATE TABLE IF NOT EXISTS `account_balance_buffer_29` (
    `account_no`       VARCHAR(64)   NOT NULL COMMENT '账户号',
    `pending_delta`    BIGINT NOT NULL DEFAULT 0 COMMENT '待刷新的余额增量（可为负，ISO最小单位×100）',
    `pending_count`    INT           NOT NULL DEFAULT 0 COMMENT '待刷新的流水笔数',
    `flush_scheduled_at` DATETIME(3) NULL DEFAULT NULL COMMENT '下次计划刷新时间（NULL=立即刷新；worker 按此时间触发，执行后自动延后一个间隔）',
    `last_updated_at`  DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最后写入时间',
    PRIMARY KEY (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户余额缓冲聚合表（平台/中间账户缓冲记账）';

-- ============================================
-- TCC 全局协调者表 (tcc_coordinator_xxx)
-- 分库 + 分表：按 tcc_id 数字 hash(tcc_id) 路由到 (dbIndex, globalTableIndex)，
-- 与 account_transaction / tcc_transaction 路由规则一致，保证同一笔 booking 所有
-- 相关记录共享分片，避免跨分片事务。
-- ============================================
CREATE TABLE IF NOT EXISTS `tcc_coordinator_29` (
    `tcc_id`        VARCHAR(64)  NOT NULL COMMENT 'TCC 全局事务 ID（= voucher_no）',
    `phase`         TINYINT      NOT NULL DEFAULT 1 COMMENT '全局阶段：1=TRYING 2=CONFIRMING 3=CONFIRMED 4=CANCELLED',
    `business_no`   VARCHAR(256) NOT NULL DEFAULT '' COMMENT '业务订单号',
    `branch_count`  SMALLINT     NOT NULL COMMENT '总分支数',
    `cut_date`      CHAR(10)     NOT NULL COMMENT '日切归属日 YYYY-MM-DD（CHAR(10) 故意避开 DATE，避免 gorm parseTime 1 天位移 bug）。booking 入口处 computeCutDate 计算后 propagate；与 account_transaction.cut_date 一致。日切 drain 等待: WHERE cut_date <= X AND phase IN (TRYING, CONFIRMING) → 0',
    `currency`      VARCHAR(8)   NOT NULL DEFAULT '' COMMENT '币种（PHP/CNY/USD/...）。booking 入口写入；TCC Recovery 用 coord.currency 写补 confirm 流水，避免与原 booking 不同币种导致试算 PHP-only / currency-filter 不平。空字符串=旧数据兼容',
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`tcc_id`),
    KEY `idx_phase_updated` (`phase`, `updated_at`),
    KEY `idx_cut_date_phase` (`cut_date`, `phase`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='TCC 全局协调者（阶段追踪，100 张分表）';

-- ============================================
-- 索引优化说明
-- ============================================
-- 1. 所有分表都需要创建相同的表结构和索引
-- 2. 查询时优先使用分片键（user_id, account_no）
-- 3. 时间范围查询使用 transaction_date 索引
-- 4. 定期归档历史数据，保持表数据量在合理范围

-- ============================================
-- 13. 原子批量记账订单表 (batch_order)
-- 全局单表（不分库分表），记录原子批量记账的批次状态
-- 批次内各单笔记账通过 TCC 保证整体原子性：全部成功 or 全部回滚
-- extra VARCHAR(4096)：存储批次请求参数快照，最长4096字符
-- ============================================
CREATE TABLE IF NOT EXISTS `batch_order_29` (
    `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `batch_id`        VARCHAR(64)  NOT NULL COMMENT '批次幂等ID（调用方提供）',
    `business_no`     VARCHAR(64)  NOT NULL COMMENT '批次业务号',
    `item_count`      INT          NOT NULL DEFAULT 0 COMMENT '批次中单笔数量',
    `status`          TINYINT      NOT NULL DEFAULT 0 COMMENT '0=pending 1=processing 2=success 3=failed',
    `error_message`   VARCHAR(512) DEFAULT NULL COMMENT '失败原因',
    `description`     VARCHAR(256) DEFAULT NULL,
    `extra`           VARCHAR(4096) NOT NULL DEFAULT '' COMMENT 'JSON批次请求参数快照，最长4096字符',
    `created_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_batch_id` (`batch_id`),
    KEY `idx_business_no` (`business_no`),
    KEY `idx_status` (`status`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='原子批量记账订单表（全局表）';

-- ============================================
-- 12. 结算 Outbox 表 (settlement_outbox)
-- 热路径事务预写日志（Transactional Outbox Pattern）
-- 分库，不分表：按 voucher_no 前1字符（dbIndex）路由，每库一张 settlement_outbox 表
-- ============================================
CREATE TABLE IF NOT EXISTS `settlement_outbox_29` (
    `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
    `voucher_no`       VARCHAR(64)  NOT NULL COMMENT '凭证号（前3字符={1d-dbIdx}{2d-tableIdx}，用于路由）',
    `event_data`       LONGTEXT     NOT NULL COMMENT 'JSON: SettlementEvent',
    `transaction_date` CHAR(10)     NOT NULL COMMENT '交易日期 YYYY-MM-DD，供日切按天过滤 outbox delta',
    `cut_date`         CHAR(10)     NOT NULL DEFAULT '' COMMENT '日切归属日 YYYY-MM-DD（入口落库那一刻定死，永不重算）；空字符串=老数据按 transaction_date 兼容处理',
    `status`           TINYINT      NOT NULL DEFAULT 0 COMMENT '0=PENDING 1=REDIS_DONE 2=MYSQL_DONE 3=FAILED',
    `retry_count`      INT          NOT NULL DEFAULT 0 COMMENT '重试次数',
    `error_msg`        TEXT         DEFAULT NULL COMMENT '最近一次失败原因',
    `created_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_voucher_no` (`voucher_no`),
    KEY `idx_status_date` (`status`, `transaction_date`),
    KEY `idx_status_created` (`status`, `created_at`),
    -- OutboxWorker MySQLWriter 高频轮询查询：
    --   SELECT * FROM settlement_outbox WHERE status = ? ORDER BY id ASC LIMIT 500
    -- (status, id) 复合索引让此查询走"索引范围扫描 + 已序输出"，免去 filesort。
    -- InnoDB 二级索引天然把 PK id 附在叶节点，但只有把 id 显式作为索引第二列，
    -- 优化器才会用 index range scan 满足 ORDER BY id；否则可能选 idx_status 后 sort。
    KEY `idx_status_id` (`status`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='热路径事务预写日志（Outbox）';












-- ============================================
-- tx_account_anchor_20: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_20` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（业务方 business_id）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id（per-shard 二级索引使用）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '锚定到的 instance account_no（分片键）',
    `direction_mask` TINYINT NOT NULL DEFAULT 0 COMMENT 'bit0=曾借记 bit1=曾贷记',
    `anchored_at` DATETIME NOT NULL COMMENT '锚定时刻',
    `last_posting_at` DATETIME NOT NULL COMMENT '最近一笔分录时刻',
    `posting_count` INT NOT NULL DEFAULT 1 COMMENT '已写入的分录数',
    `migrated_to_account_no` VARCHAR(64) DEFAULT NULL COMMENT '若发生强制迁移，指向新 instance',
    `migration_voucher_no` VARCHAR(64) DEFAULT NULL COMMENT '迁移凭证号（审计）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=trying 1=active 2=settled 3=migrated 4=stuck',
    `reuse_source` TINYINT NOT NULL DEFAULT 0 COMMENT '0=primary 1=refund-of 2=reverse-of',
    `reuse_source_anchor_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '复用了哪条 anchor（审计）',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '已发生过多少次强制迁移；>5 进 quarantined (E-50)',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_account` (`flow_id`, `account_no`),
    KEY `idx_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_status` (`account_no`, `status`),
    KEY `idx_status_chain_depth` (`status`, `migration_chain_depth`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流账户锚点表（方向B：按 account_no 分片）';

-- ============================================
-- flow_anchor_route_20: 资金流 → instance 路由索引表
-- 【方向 B 关键】按 flow_id FNV-1a 哈希到 100 片
-- 作用：通过 (flow_id, logical_account_id) → account_no 解析，让 router 知道
--      "本资金流在某 logical_account 上锁定到了哪个 instance"
-- 不变量：(flow_id, logical_account_id) 唯一 — 一个 flow 在同一 LA 上仅锁定一个 instance（I0）
--
-- 写入：
--   - 首次锚定（router 决定 account_no）→ INSERT 一行（在 flow_id shard 本地事务）
--   - 强制迁移：UPDATE account_no 字段（chain_depth++）
--   - 其他情况绝不修改 — immutable lookup record
--
-- 读取：路由层热路径每次都查（5s 缓存）
-- ============================================
CREATE TABLE IF NOT EXISTS `flow_anchor_route_20` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（分片键）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `account_no` VARCHAR(64) NOT NULL COMMENT 'flow 在本 LA 上锁定的 account_no',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '迁移跳数（与 anchor 表一致）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_no` (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流路由索引（方向B）';

-- ============================================
-- tx_account_anchor_21: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_21` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（业务方 business_id）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id（per-shard 二级索引使用）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '锚定到的 instance account_no（分片键）',
    `direction_mask` TINYINT NOT NULL DEFAULT 0 COMMENT 'bit0=曾借记 bit1=曾贷记',
    `anchored_at` DATETIME NOT NULL COMMENT '锚定时刻',
    `last_posting_at` DATETIME NOT NULL COMMENT '最近一笔分录时刻',
    `posting_count` INT NOT NULL DEFAULT 1 COMMENT '已写入的分录数',
    `migrated_to_account_no` VARCHAR(64) DEFAULT NULL COMMENT '若发生强制迁移，指向新 instance',
    `migration_voucher_no` VARCHAR(64) DEFAULT NULL COMMENT '迁移凭证号（审计）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=trying 1=active 2=settled 3=migrated 4=stuck',
    `reuse_source` TINYINT NOT NULL DEFAULT 0 COMMENT '0=primary 1=refund-of 2=reverse-of',
    `reuse_source_anchor_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '复用了哪条 anchor（审计）',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '已发生过多少次强制迁移；>5 进 quarantined (E-50)',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_account` (`flow_id`, `account_no`),
    KEY `idx_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_status` (`account_no`, `status`),
    KEY `idx_status_chain_depth` (`status`, `migration_chain_depth`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流账户锚点表（方向B：按 account_no 分片）';

-- ============================================
-- flow_anchor_route_21: 资金流 → instance 路由索引表
-- 【方向 B 关键】按 flow_id FNV-1a 哈希到 100 片
-- 作用：通过 (flow_id, logical_account_id) → account_no 解析，让 router 知道
--      "本资金流在某 logical_account 上锁定到了哪个 instance"
-- 不变量：(flow_id, logical_account_id) 唯一 — 一个 flow 在同一 LA 上仅锁定一个 instance（I0）
--
-- 写入：
--   - 首次锚定（router 决定 account_no）→ INSERT 一行（在 flow_id shard 本地事务）
--   - 强制迁移：UPDATE account_no 字段（chain_depth++）
--   - 其他情况绝不修改 — immutable lookup record
--
-- 读取：路由层热路径每次都查（5s 缓存）
-- ============================================
CREATE TABLE IF NOT EXISTS `flow_anchor_route_21` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（分片键）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `account_no` VARCHAR(64) NOT NULL COMMENT 'flow 在本 LA 上锁定的 account_no',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '迁移跳数（与 anchor 表一致）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_no` (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流路由索引（方向B）';

-- ============================================
-- tx_account_anchor_22: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_22` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（业务方 business_id）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id（per-shard 二级索引使用）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '锚定到的 instance account_no（分片键）',
    `direction_mask` TINYINT NOT NULL DEFAULT 0 COMMENT 'bit0=曾借记 bit1=曾贷记',
    `anchored_at` DATETIME NOT NULL COMMENT '锚定时刻',
    `last_posting_at` DATETIME NOT NULL COMMENT '最近一笔分录时刻',
    `posting_count` INT NOT NULL DEFAULT 1 COMMENT '已写入的分录数',
    `migrated_to_account_no` VARCHAR(64) DEFAULT NULL COMMENT '若发生强制迁移，指向新 instance',
    `migration_voucher_no` VARCHAR(64) DEFAULT NULL COMMENT '迁移凭证号（审计）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=trying 1=active 2=settled 3=migrated 4=stuck',
    `reuse_source` TINYINT NOT NULL DEFAULT 0 COMMENT '0=primary 1=refund-of 2=reverse-of',
    `reuse_source_anchor_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '复用了哪条 anchor（审计）',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '已发生过多少次强制迁移；>5 进 quarantined (E-50)',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_account` (`flow_id`, `account_no`),
    KEY `idx_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_status` (`account_no`, `status`),
    KEY `idx_status_chain_depth` (`status`, `migration_chain_depth`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流账户锚点表（方向B：按 account_no 分片）';

-- ============================================
-- flow_anchor_route_22: 资金流 → instance 路由索引表
-- 【方向 B 关键】按 flow_id FNV-1a 哈希到 100 片
-- 作用：通过 (flow_id, logical_account_id) → account_no 解析，让 router 知道
--      "本资金流在某 logical_account 上锁定到了哪个 instance"
-- 不变量：(flow_id, logical_account_id) 唯一 — 一个 flow 在同一 LA 上仅锁定一个 instance（I0）
--
-- 写入：
--   - 首次锚定（router 决定 account_no）→ INSERT 一行（在 flow_id shard 本地事务）
--   - 强制迁移：UPDATE account_no 字段（chain_depth++）
--   - 其他情况绝不修改 — immutable lookup record
--
-- 读取：路由层热路径每次都查（5s 缓存）
-- ============================================
CREATE TABLE IF NOT EXISTS `flow_anchor_route_22` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（分片键）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `account_no` VARCHAR(64) NOT NULL COMMENT 'flow 在本 LA 上锁定的 account_no',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '迁移跳数（与 anchor 表一致）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_no` (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流路由索引（方向B）';

-- ============================================
-- tx_account_anchor_23: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_23` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（业务方 business_id）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id（per-shard 二级索引使用）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '锚定到的 instance account_no（分片键）',
    `direction_mask` TINYINT NOT NULL DEFAULT 0 COMMENT 'bit0=曾借记 bit1=曾贷记',
    `anchored_at` DATETIME NOT NULL COMMENT '锚定时刻',
    `last_posting_at` DATETIME NOT NULL COMMENT '最近一笔分录时刻',
    `posting_count` INT NOT NULL DEFAULT 1 COMMENT '已写入的分录数',
    `migrated_to_account_no` VARCHAR(64) DEFAULT NULL COMMENT '若发生强制迁移，指向新 instance',
    `migration_voucher_no` VARCHAR(64) DEFAULT NULL COMMENT '迁移凭证号（审计）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=trying 1=active 2=settled 3=migrated 4=stuck',
    `reuse_source` TINYINT NOT NULL DEFAULT 0 COMMENT '0=primary 1=refund-of 2=reverse-of',
    `reuse_source_anchor_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '复用了哪条 anchor（审计）',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '已发生过多少次强制迁移；>5 进 quarantined (E-50)',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_account` (`flow_id`, `account_no`),
    KEY `idx_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_status` (`account_no`, `status`),
    KEY `idx_status_chain_depth` (`status`, `migration_chain_depth`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流账户锚点表（方向B：按 account_no 分片）';

-- ============================================
-- flow_anchor_route_23: 资金流 → instance 路由索引表
-- 【方向 B 关键】按 flow_id FNV-1a 哈希到 100 片
-- 作用：通过 (flow_id, logical_account_id) → account_no 解析，让 router 知道
--      "本资金流在某 logical_account 上锁定到了哪个 instance"
-- 不变量：(flow_id, logical_account_id) 唯一 — 一个 flow 在同一 LA 上仅锁定一个 instance（I0）
--
-- 写入：
--   - 首次锚定（router 决定 account_no）→ INSERT 一行（在 flow_id shard 本地事务）
--   - 强制迁移：UPDATE account_no 字段（chain_depth++）
--   - 其他情况绝不修改 — immutable lookup record
--
-- 读取：路由层热路径每次都查（5s 缓存）
-- ============================================
CREATE TABLE IF NOT EXISTS `flow_anchor_route_23` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（分片键）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `account_no` VARCHAR(64) NOT NULL COMMENT 'flow 在本 LA 上锁定的 account_no',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '迁移跳数（与 anchor 表一致）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_no` (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流路由索引（方向B）';

-- ============================================
-- tx_account_anchor_24: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_24` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（业务方 business_id）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id（per-shard 二级索引使用）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '锚定到的 instance account_no（分片键）',
    `direction_mask` TINYINT NOT NULL DEFAULT 0 COMMENT 'bit0=曾借记 bit1=曾贷记',
    `anchored_at` DATETIME NOT NULL COMMENT '锚定时刻',
    `last_posting_at` DATETIME NOT NULL COMMENT '最近一笔分录时刻',
    `posting_count` INT NOT NULL DEFAULT 1 COMMENT '已写入的分录数',
    `migrated_to_account_no` VARCHAR(64) DEFAULT NULL COMMENT '若发生强制迁移，指向新 instance',
    `migration_voucher_no` VARCHAR(64) DEFAULT NULL COMMENT '迁移凭证号（审计）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=trying 1=active 2=settled 3=migrated 4=stuck',
    `reuse_source` TINYINT NOT NULL DEFAULT 0 COMMENT '0=primary 1=refund-of 2=reverse-of',
    `reuse_source_anchor_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '复用了哪条 anchor（审计）',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '已发生过多少次强制迁移；>5 进 quarantined (E-50)',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_account` (`flow_id`, `account_no`),
    KEY `idx_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_status` (`account_no`, `status`),
    KEY `idx_status_chain_depth` (`status`, `migration_chain_depth`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流账户锚点表（方向B：按 account_no 分片）';

-- ============================================
-- flow_anchor_route_24: 资金流 → instance 路由索引表
-- 【方向 B 关键】按 flow_id FNV-1a 哈希到 100 片
-- 作用：通过 (flow_id, logical_account_id) → account_no 解析，让 router 知道
--      "本资金流在某 logical_account 上锁定到了哪个 instance"
-- 不变量：(flow_id, logical_account_id) 唯一 — 一个 flow 在同一 LA 上仅锁定一个 instance（I0）
--
-- 写入：
--   - 首次锚定（router 决定 account_no）→ INSERT 一行（在 flow_id shard 本地事务）
--   - 强制迁移：UPDATE account_no 字段（chain_depth++）
--   - 其他情况绝不修改 — immutable lookup record
--
-- 读取：路由层热路径每次都查（5s 缓存）
-- ============================================
CREATE TABLE IF NOT EXISTS `flow_anchor_route_24` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（分片键）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `account_no` VARCHAR(64) NOT NULL COMMENT 'flow 在本 LA 上锁定的 account_no',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '迁移跳数（与 anchor 表一致）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_no` (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流路由索引（方向B）';

-- ============================================
-- tx_account_anchor_25: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_25` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（业务方 business_id）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id（per-shard 二级索引使用）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '锚定到的 instance account_no（分片键）',
    `direction_mask` TINYINT NOT NULL DEFAULT 0 COMMENT 'bit0=曾借记 bit1=曾贷记',
    `anchored_at` DATETIME NOT NULL COMMENT '锚定时刻',
    `last_posting_at` DATETIME NOT NULL COMMENT '最近一笔分录时刻',
    `posting_count` INT NOT NULL DEFAULT 1 COMMENT '已写入的分录数',
    `migrated_to_account_no` VARCHAR(64) DEFAULT NULL COMMENT '若发生强制迁移，指向新 instance',
    `migration_voucher_no` VARCHAR(64) DEFAULT NULL COMMENT '迁移凭证号（审计）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=trying 1=active 2=settled 3=migrated 4=stuck',
    `reuse_source` TINYINT NOT NULL DEFAULT 0 COMMENT '0=primary 1=refund-of 2=reverse-of',
    `reuse_source_anchor_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '复用了哪条 anchor（审计）',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '已发生过多少次强制迁移；>5 进 quarantined (E-50)',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_account` (`flow_id`, `account_no`),
    KEY `idx_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_status` (`account_no`, `status`),
    KEY `idx_status_chain_depth` (`status`, `migration_chain_depth`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流账户锚点表（方向B：按 account_no 分片）';

-- ============================================
-- flow_anchor_route_25: 资金流 → instance 路由索引表
-- 【方向 B 关键】按 flow_id FNV-1a 哈希到 100 片
-- 作用：通过 (flow_id, logical_account_id) → account_no 解析，让 router 知道
--      "本资金流在某 logical_account 上锁定到了哪个 instance"
-- 不变量：(flow_id, logical_account_id) 唯一 — 一个 flow 在同一 LA 上仅锁定一个 instance（I0）
--
-- 写入：
--   - 首次锚定（router 决定 account_no）→ INSERT 一行（在 flow_id shard 本地事务）
--   - 强制迁移：UPDATE account_no 字段（chain_depth++）
--   - 其他情况绝不修改 — immutable lookup record
--
-- 读取：路由层热路径每次都查（5s 缓存）
-- ============================================
CREATE TABLE IF NOT EXISTS `flow_anchor_route_25` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（分片键）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `account_no` VARCHAR(64) NOT NULL COMMENT 'flow 在本 LA 上锁定的 account_no',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '迁移跳数（与 anchor 表一致）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_no` (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流路由索引（方向B）';

-- ============================================
-- tx_account_anchor_26: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_26` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（业务方 business_id）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id（per-shard 二级索引使用）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '锚定到的 instance account_no（分片键）',
    `direction_mask` TINYINT NOT NULL DEFAULT 0 COMMENT 'bit0=曾借记 bit1=曾贷记',
    `anchored_at` DATETIME NOT NULL COMMENT '锚定时刻',
    `last_posting_at` DATETIME NOT NULL COMMENT '最近一笔分录时刻',
    `posting_count` INT NOT NULL DEFAULT 1 COMMENT '已写入的分录数',
    `migrated_to_account_no` VARCHAR(64) DEFAULT NULL COMMENT '若发生强制迁移，指向新 instance',
    `migration_voucher_no` VARCHAR(64) DEFAULT NULL COMMENT '迁移凭证号（审计）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=trying 1=active 2=settled 3=migrated 4=stuck',
    `reuse_source` TINYINT NOT NULL DEFAULT 0 COMMENT '0=primary 1=refund-of 2=reverse-of',
    `reuse_source_anchor_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '复用了哪条 anchor（审计）',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '已发生过多少次强制迁移；>5 进 quarantined (E-50)',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_account` (`flow_id`, `account_no`),
    KEY `idx_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_status` (`account_no`, `status`),
    KEY `idx_status_chain_depth` (`status`, `migration_chain_depth`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流账户锚点表（方向B：按 account_no 分片）';

-- ============================================
-- flow_anchor_route_26: 资金流 → instance 路由索引表
-- 【方向 B 关键】按 flow_id FNV-1a 哈希到 100 片
-- 作用：通过 (flow_id, logical_account_id) → account_no 解析，让 router 知道
--      "本资金流在某 logical_account 上锁定到了哪个 instance"
-- 不变量：(flow_id, logical_account_id) 唯一 — 一个 flow 在同一 LA 上仅锁定一个 instance（I0）
--
-- 写入：
--   - 首次锚定（router 决定 account_no）→ INSERT 一行（在 flow_id shard 本地事务）
--   - 强制迁移：UPDATE account_no 字段（chain_depth++）
--   - 其他情况绝不修改 — immutable lookup record
--
-- 读取：路由层热路径每次都查（5s 缓存）
-- ============================================
CREATE TABLE IF NOT EXISTS `flow_anchor_route_26` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（分片键）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `account_no` VARCHAR(64) NOT NULL COMMENT 'flow 在本 LA 上锁定的 account_no',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '迁移跳数（与 anchor 表一致）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_no` (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流路由索引（方向B）';

-- ============================================
-- tx_account_anchor_27: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_27` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（业务方 business_id）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id（per-shard 二级索引使用）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '锚定到的 instance account_no（分片键）',
    `direction_mask` TINYINT NOT NULL DEFAULT 0 COMMENT 'bit0=曾借记 bit1=曾贷记',
    `anchored_at` DATETIME NOT NULL COMMENT '锚定时刻',
    `last_posting_at` DATETIME NOT NULL COMMENT '最近一笔分录时刻',
    `posting_count` INT NOT NULL DEFAULT 1 COMMENT '已写入的分录数',
    `migrated_to_account_no` VARCHAR(64) DEFAULT NULL COMMENT '若发生强制迁移，指向新 instance',
    `migration_voucher_no` VARCHAR(64) DEFAULT NULL COMMENT '迁移凭证号（审计）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=trying 1=active 2=settled 3=migrated 4=stuck',
    `reuse_source` TINYINT NOT NULL DEFAULT 0 COMMENT '0=primary 1=refund-of 2=reverse-of',
    `reuse_source_anchor_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '复用了哪条 anchor（审计）',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '已发生过多少次强制迁移；>5 进 quarantined (E-50)',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_account` (`flow_id`, `account_no`),
    KEY `idx_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_status` (`account_no`, `status`),
    KEY `idx_status_chain_depth` (`status`, `migration_chain_depth`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流账户锚点表（方向B：按 account_no 分片）';

-- ============================================
-- flow_anchor_route_27: 资金流 → instance 路由索引表
-- 【方向 B 关键】按 flow_id FNV-1a 哈希到 100 片
-- 作用：通过 (flow_id, logical_account_id) → account_no 解析，让 router 知道
--      "本资金流在某 logical_account 上锁定到了哪个 instance"
-- 不变量：(flow_id, logical_account_id) 唯一 — 一个 flow 在同一 LA 上仅锁定一个 instance（I0）
--
-- 写入：
--   - 首次锚定（router 决定 account_no）→ INSERT 一行（在 flow_id shard 本地事务）
--   - 强制迁移：UPDATE account_no 字段（chain_depth++）
--   - 其他情况绝不修改 — immutable lookup record
--
-- 读取：路由层热路径每次都查（5s 缓存）
-- ============================================
CREATE TABLE IF NOT EXISTS `flow_anchor_route_27` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（分片键）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `account_no` VARCHAR(64) NOT NULL COMMENT 'flow 在本 LA 上锁定的 account_no',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '迁移跳数（与 anchor 表一致）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_no` (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流路由索引（方向B）';

-- ============================================
-- tx_account_anchor_28: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_28` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（业务方 business_id）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id（per-shard 二级索引使用）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '锚定到的 instance account_no（分片键）',
    `direction_mask` TINYINT NOT NULL DEFAULT 0 COMMENT 'bit0=曾借记 bit1=曾贷记',
    `anchored_at` DATETIME NOT NULL COMMENT '锚定时刻',
    `last_posting_at` DATETIME NOT NULL COMMENT '最近一笔分录时刻',
    `posting_count` INT NOT NULL DEFAULT 1 COMMENT '已写入的分录数',
    `migrated_to_account_no` VARCHAR(64) DEFAULT NULL COMMENT '若发生强制迁移，指向新 instance',
    `migration_voucher_no` VARCHAR(64) DEFAULT NULL COMMENT '迁移凭证号（审计）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=trying 1=active 2=settled 3=migrated 4=stuck',
    `reuse_source` TINYINT NOT NULL DEFAULT 0 COMMENT '0=primary 1=refund-of 2=reverse-of',
    `reuse_source_anchor_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '复用了哪条 anchor（审计）',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '已发生过多少次强制迁移；>5 进 quarantined (E-50)',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_account` (`flow_id`, `account_no`),
    KEY `idx_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_status` (`account_no`, `status`),
    KEY `idx_status_chain_depth` (`status`, `migration_chain_depth`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流账户锚点表（方向B：按 account_no 分片）';

-- ============================================
-- flow_anchor_route_28: 资金流 → instance 路由索引表
-- 【方向 B 关键】按 flow_id FNV-1a 哈希到 100 片
-- 作用：通过 (flow_id, logical_account_id) → account_no 解析，让 router 知道
--      "本资金流在某 logical_account 上锁定到了哪个 instance"
-- 不变量：(flow_id, logical_account_id) 唯一 — 一个 flow 在同一 LA 上仅锁定一个 instance（I0）
--
-- 写入：
--   - 首次锚定（router 决定 account_no）→ INSERT 一行（在 flow_id shard 本地事务）
--   - 强制迁移：UPDATE account_no 字段（chain_depth++）
--   - 其他情况绝不修改 — immutable lookup record
--
-- 读取：路由层热路径每次都查（5s 缓存）
-- ============================================
CREATE TABLE IF NOT EXISTS `flow_anchor_route_28` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（分片键）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `account_no` VARCHAR(64) NOT NULL COMMENT 'flow 在本 LA 上锁定的 account_no',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '迁移跳数（与 anchor 表一致）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_no` (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流路由索引（方向B）';

-- ============================================
-- tx_account_anchor_29: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_29` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（业务方 business_id）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id（per-shard 二级索引使用）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '锚定到的 instance account_no（分片键）',
    `direction_mask` TINYINT NOT NULL DEFAULT 0 COMMENT 'bit0=曾借记 bit1=曾贷记',
    `anchored_at` DATETIME NOT NULL COMMENT '锚定时刻',
    `last_posting_at` DATETIME NOT NULL COMMENT '最近一笔分录时刻',
    `posting_count` INT NOT NULL DEFAULT 1 COMMENT '已写入的分录数',
    `migrated_to_account_no` VARCHAR(64) DEFAULT NULL COMMENT '若发生强制迁移，指向新 instance',
    `migration_voucher_no` VARCHAR(64) DEFAULT NULL COMMENT '迁移凭证号（审计）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=trying 1=active 2=settled 3=migrated 4=stuck',
    `reuse_source` TINYINT NOT NULL DEFAULT 0 COMMENT '0=primary 1=refund-of 2=reverse-of',
    `reuse_source_anchor_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '复用了哪条 anchor（审计）',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '已发生过多少次强制迁移；>5 进 quarantined (E-50)',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_account` (`flow_id`, `account_no`),
    KEY `idx_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_status` (`account_no`, `status`),
    KEY `idx_status_chain_depth` (`status`, `migration_chain_depth`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流账户锚点表（方向B：按 account_no 分片）';

-- ============================================
-- flow_anchor_route_29: 资金流 → instance 路由索引表
-- 【方向 B 关键】按 flow_id FNV-1a 哈希到 100 片
-- 作用：通过 (flow_id, logical_account_id) → account_no 解析，让 router 知道
--      "本资金流在某 logical_account 上锁定到了哪个 instance"
-- 不变量：(flow_id, logical_account_id) 唯一 — 一个 flow 在同一 LA 上仅锁定一个 instance（I0）
--
-- 写入：
--   - 首次锚定（router 决定 account_no）→ INSERT 一行（在 flow_id shard 本地事务）
--   - 强制迁移：UPDATE account_no 字段（chain_depth++）
--   - 其他情况绝不修改 — immutable lookup record
--
-- 读取：路由层热路径每次都查（5s 缓存）
-- ============================================
CREATE TABLE IF NOT EXISTS `flow_anchor_route_29` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（分片键）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `account_no` VARCHAR(64) NOT NULL COMMENT 'flow 在本 LA 上锁定的 account_no',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '迁移跳数（与 anchor 表一致）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_no` (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流路由索引（方向B）';

-- ==== accounting-system 平台账户 seed (按位编码 account_no) ====
SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;
USE `accounting_db_2`;

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 20 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 0 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (20)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (20)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_20` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 20 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 20, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_20` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 20 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 20, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_20` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 20 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 20, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_20` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 20 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 20, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_20` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 20 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 20, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_20` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 20 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 20, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 21 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 0 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (21)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (21)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_21` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 21 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 21, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_21` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 21 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 21, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_21` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 21 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 21, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_21` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 21 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 21, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_21` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 21 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 21, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_21` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 21 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 21, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 22 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 0 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (22)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (22)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_22` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 22 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 22, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_22` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 22 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 22, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_22` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 22 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 22, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_22` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 22 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 22, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_22` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 22 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 22, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_22` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 22 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 22, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 23 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 0 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (23)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (23)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_23` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 23 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 23, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_23` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 23 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 23, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_23` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 23 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 23, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_23` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 23 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 23, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_23` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 23 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 23, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_23` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 23 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 23, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 24 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 0 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (24)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (24)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_24` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 24 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 24, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_24` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 24 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 24, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_24` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 24 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 24, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_24` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 24 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 24, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_24` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 24 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 24, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_24` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 24 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 24, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 25 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 0 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (25)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (25)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_25` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 25 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 25, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_25` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 25 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 25, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_25` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 25 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 25, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_25` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 25 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 25, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_25` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 25 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 25, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_25` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 25 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 25, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 26 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 0 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (26)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (26)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_26` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 26 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 26, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_26` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 26 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 26, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_26` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 26 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 26, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_26` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 26 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 26, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_26` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 26 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 26, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_26` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 26 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 26, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 27 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 0 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (27)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (27)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_27` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 27 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 27, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_27` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 27 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 27, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_27` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 27 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 27, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_27` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 27 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 27, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_27` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 27 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 27, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_27` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 27 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 27, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 28 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 0 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (28)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (28)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_28` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 28 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 28, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_28` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 28 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 28, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_28` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 28 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 28, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_28` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 28 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 28, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_28` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 28 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 28, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_28` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 28 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 28, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 29 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 0 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (29)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (29)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_29` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 29 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 29, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_29` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 29 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 29, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_29` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 29 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 29, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_29` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 29 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 29, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_29` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 29 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 29, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_29` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 29 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 29, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);


-- ==== user-merchant-core ====
SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `user_merchant_db_2` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `user_merchant_db_2`;

-- 分片表 schema 模板。2 和 20 由 generate.sh 替换：
--   2     = 0..9         分库 idx
--   20  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_20` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 20)';

CREATE TABLE IF NOT EXISTS `user_profiles_20` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 20)';

CREATE TABLE IF NOT EXISTS `user_auths_20` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 20)';

CREATE TABLE IF NOT EXISTS `login_logs_20` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 20)';

CREATE TABLE IF NOT EXISTS `user_sessions_20` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 20)';

CREATE TABLE IF NOT EXISTS `user_roles_20` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 20)';

CREATE TABLE IF NOT EXISTS `user_accounts_20` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 20)';

CREATE TABLE IF NOT EXISTS `user_settings_20` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 20)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_20` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 20)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_20` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
    `name`              VARCHAR(128) NOT NULL,
    `legal_name`        VARCHAR(256) DEFAULT NULL,
    `country`           CHAR(2)      NOT NULL DEFAULT 'PH',
    `business_type`     VARCHAR(16)  NOT NULL DEFAULT 'individual',
    `tax_id`            VARCHAR(64)  DEFAULT NULL,
    `contact_email`     VARCHAR(128) NOT NULL,
    `contact_phone`     VARCHAR(32)  DEFAULT NULL,
    `website`           VARCHAR(256) DEFAULT NULL,
    `mcc`               VARCHAR(8)   DEFAULT NULL,
    `live_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `test_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `webhook_url`       VARCHAR(512) NOT NULL DEFAULT '',
    `webhook_secret`    VARCHAR(64)  NOT NULL DEFAULT '',
    `kyc_status`        VARCHAR(24)  NOT NULL DEFAULT 'pending',
    `kyc_level`         TINYINT      NOT NULL DEFAULT 0,
    `kyc_reason`        VARCHAR(512) DEFAULT NULL,
    `kyc_reviewer`      VARCHAR(64)  DEFAULT NULL,
    `kyc_reviewed_at`   DATETIME(3)  DEFAULT NULL,
    `risk_tier`         VARCHAR(16)  NOT NULL DEFAULT 'standard',
    `rate_limit_rps`    INT          NOT NULL DEFAULT 100,
    `settle_currency`   CHAR(3)      NOT NULL DEFAULT 'PHP',
    `settle_method`     VARCHAR(16)  DEFAULT NULL,
    `settle_account`    VARCHAR(128) DEFAULT NULL,
    `settle_bank`       VARCHAR(128) DEFAULT NULL,
    `settle_holder`     VARCHAR(128) DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `metadata`          JSON         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 20)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_20` (
    `id`              VARCHAR(32)  NOT NULL,
    `merchant_id`     VARCHAR(32)  NOT NULL,
    `doc_type`        VARCHAR(32)  NOT NULL,
    `doc_number`      VARCHAR(128) DEFAULT NULL,
    `file_url`        VARCHAR(512) NOT NULL,
    `mime_type`       VARCHAR(64)  DEFAULT NULL,
    `size_bytes`      INT          DEFAULT NULL,
    `uploaded_by`     VARCHAR(64)  DEFAULT NULL,
    `review_status`   VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `review_note`     VARCHAR(512) DEFAULT NULL,
    `expires_at`      DATETIME(3)  DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant`      (`merchant_id`),
    KEY `idx_review_status` (`review_status`),
    KEY `idx_doc_type`      (`doc_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 20)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_20` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `channel`      VARCHAR(32)  NOT NULL,
    `field_name`   VARCHAR(64)  NOT NULL,
    `ciphertext`   VARBINARY(4096) NOT NULL,
    `context`      VARCHAR(256) NOT NULL,
    `masked_hint`  VARCHAR(64)  DEFAULT NULL,
    `version`      INT          NOT NULL DEFAULT 1,
    `created_by`   VARCHAR(64)  DEFAULT NULL,
    `created`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_merchant_channel_field` (`merchant_id`, `channel`, `field_name`),
    KEY `idx_merchant_channel` (`merchant_id`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 20)';

-- ─── admin_audit_log_20：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_20` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 20';

-- 分片表 schema 模板。2 和 21 由 generate.sh 替换：
--   2     = 0..9         分库 idx
--   21  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_21` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 21)';

CREATE TABLE IF NOT EXISTS `user_profiles_21` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 21)';

CREATE TABLE IF NOT EXISTS `user_auths_21` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 21)';

CREATE TABLE IF NOT EXISTS `login_logs_21` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 21)';

CREATE TABLE IF NOT EXISTS `user_sessions_21` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 21)';

CREATE TABLE IF NOT EXISTS `user_roles_21` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 21)';

CREATE TABLE IF NOT EXISTS `user_accounts_21` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 21)';

CREATE TABLE IF NOT EXISTS `user_settings_21` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 21)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_21` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 21)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_21` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
    `name`              VARCHAR(128) NOT NULL,
    `legal_name`        VARCHAR(256) DEFAULT NULL,
    `country`           CHAR(2)      NOT NULL DEFAULT 'PH',
    `business_type`     VARCHAR(16)  NOT NULL DEFAULT 'individual',
    `tax_id`            VARCHAR(64)  DEFAULT NULL,
    `contact_email`     VARCHAR(128) NOT NULL,
    `contact_phone`     VARCHAR(32)  DEFAULT NULL,
    `website`           VARCHAR(256) DEFAULT NULL,
    `mcc`               VARCHAR(8)   DEFAULT NULL,
    `live_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `test_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `webhook_url`       VARCHAR(512) NOT NULL DEFAULT '',
    `webhook_secret`    VARCHAR(64)  NOT NULL DEFAULT '',
    `kyc_status`        VARCHAR(24)  NOT NULL DEFAULT 'pending',
    `kyc_level`         TINYINT      NOT NULL DEFAULT 0,
    `kyc_reason`        VARCHAR(512) DEFAULT NULL,
    `kyc_reviewer`      VARCHAR(64)  DEFAULT NULL,
    `kyc_reviewed_at`   DATETIME(3)  DEFAULT NULL,
    `risk_tier`         VARCHAR(16)  NOT NULL DEFAULT 'standard',
    `rate_limit_rps`    INT          NOT NULL DEFAULT 100,
    `settle_currency`   CHAR(3)      NOT NULL DEFAULT 'PHP',
    `settle_method`     VARCHAR(16)  DEFAULT NULL,
    `settle_account`    VARCHAR(128) DEFAULT NULL,
    `settle_bank`       VARCHAR(128) DEFAULT NULL,
    `settle_holder`     VARCHAR(128) DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `metadata`          JSON         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 21)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_21` (
    `id`              VARCHAR(32)  NOT NULL,
    `merchant_id`     VARCHAR(32)  NOT NULL,
    `doc_type`        VARCHAR(32)  NOT NULL,
    `doc_number`      VARCHAR(128) DEFAULT NULL,
    `file_url`        VARCHAR(512) NOT NULL,
    `mime_type`       VARCHAR(64)  DEFAULT NULL,
    `size_bytes`      INT          DEFAULT NULL,
    `uploaded_by`     VARCHAR(64)  DEFAULT NULL,
    `review_status`   VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `review_note`     VARCHAR(512) DEFAULT NULL,
    `expires_at`      DATETIME(3)  DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant`      (`merchant_id`),
    KEY `idx_review_status` (`review_status`),
    KEY `idx_doc_type`      (`doc_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 21)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_21` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `channel`      VARCHAR(32)  NOT NULL,
    `field_name`   VARCHAR(64)  NOT NULL,
    `ciphertext`   VARBINARY(4096) NOT NULL,
    `context`      VARCHAR(256) NOT NULL,
    `masked_hint`  VARCHAR(64)  DEFAULT NULL,
    `version`      INT          NOT NULL DEFAULT 1,
    `created_by`   VARCHAR(64)  DEFAULT NULL,
    `created`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_merchant_channel_field` (`merchant_id`, `channel`, `field_name`),
    KEY `idx_merchant_channel` (`merchant_id`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 21)';

-- ─── admin_audit_log_21：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_21` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 21';

-- 分片表 schema 模板。2 和 22 由 generate.sh 替换：
--   2     = 0..9         分库 idx
--   22  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_22` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 22)';

CREATE TABLE IF NOT EXISTS `user_profiles_22` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 22)';

CREATE TABLE IF NOT EXISTS `user_auths_22` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 22)';

CREATE TABLE IF NOT EXISTS `login_logs_22` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 22)';

CREATE TABLE IF NOT EXISTS `user_sessions_22` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 22)';

CREATE TABLE IF NOT EXISTS `user_roles_22` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 22)';

CREATE TABLE IF NOT EXISTS `user_accounts_22` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 22)';

CREATE TABLE IF NOT EXISTS `user_settings_22` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 22)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_22` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 22)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_22` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
    `name`              VARCHAR(128) NOT NULL,
    `legal_name`        VARCHAR(256) DEFAULT NULL,
    `country`           CHAR(2)      NOT NULL DEFAULT 'PH',
    `business_type`     VARCHAR(16)  NOT NULL DEFAULT 'individual',
    `tax_id`            VARCHAR(64)  DEFAULT NULL,
    `contact_email`     VARCHAR(128) NOT NULL,
    `contact_phone`     VARCHAR(32)  DEFAULT NULL,
    `website`           VARCHAR(256) DEFAULT NULL,
    `mcc`               VARCHAR(8)   DEFAULT NULL,
    `live_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `test_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `webhook_url`       VARCHAR(512) NOT NULL DEFAULT '',
    `webhook_secret`    VARCHAR(64)  NOT NULL DEFAULT '',
    `kyc_status`        VARCHAR(24)  NOT NULL DEFAULT 'pending',
    `kyc_level`         TINYINT      NOT NULL DEFAULT 0,
    `kyc_reason`        VARCHAR(512) DEFAULT NULL,
    `kyc_reviewer`      VARCHAR(64)  DEFAULT NULL,
    `kyc_reviewed_at`   DATETIME(3)  DEFAULT NULL,
    `risk_tier`         VARCHAR(16)  NOT NULL DEFAULT 'standard',
    `rate_limit_rps`    INT          NOT NULL DEFAULT 100,
    `settle_currency`   CHAR(3)      NOT NULL DEFAULT 'PHP',
    `settle_method`     VARCHAR(16)  DEFAULT NULL,
    `settle_account`    VARCHAR(128) DEFAULT NULL,
    `settle_bank`       VARCHAR(128) DEFAULT NULL,
    `settle_holder`     VARCHAR(128) DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `metadata`          JSON         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 22)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_22` (
    `id`              VARCHAR(32)  NOT NULL,
    `merchant_id`     VARCHAR(32)  NOT NULL,
    `doc_type`        VARCHAR(32)  NOT NULL,
    `doc_number`      VARCHAR(128) DEFAULT NULL,
    `file_url`        VARCHAR(512) NOT NULL,
    `mime_type`       VARCHAR(64)  DEFAULT NULL,
    `size_bytes`      INT          DEFAULT NULL,
    `uploaded_by`     VARCHAR(64)  DEFAULT NULL,
    `review_status`   VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `review_note`     VARCHAR(512) DEFAULT NULL,
    `expires_at`      DATETIME(3)  DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant`      (`merchant_id`),
    KEY `idx_review_status` (`review_status`),
    KEY `idx_doc_type`      (`doc_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 22)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_22` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `channel`      VARCHAR(32)  NOT NULL,
    `field_name`   VARCHAR(64)  NOT NULL,
    `ciphertext`   VARBINARY(4096) NOT NULL,
    `context`      VARCHAR(256) NOT NULL,
    `masked_hint`  VARCHAR(64)  DEFAULT NULL,
    `version`      INT          NOT NULL DEFAULT 1,
    `created_by`   VARCHAR(64)  DEFAULT NULL,
    `created`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_merchant_channel_field` (`merchant_id`, `channel`, `field_name`),
    KEY `idx_merchant_channel` (`merchant_id`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 22)';

-- ─── admin_audit_log_22：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_22` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 22';

-- 分片表 schema 模板。2 和 23 由 generate.sh 替换：
--   2     = 0..9         分库 idx
--   23  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_23` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 23)';

CREATE TABLE IF NOT EXISTS `user_profiles_23` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 23)';

CREATE TABLE IF NOT EXISTS `user_auths_23` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 23)';

CREATE TABLE IF NOT EXISTS `login_logs_23` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 23)';

CREATE TABLE IF NOT EXISTS `user_sessions_23` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 23)';

CREATE TABLE IF NOT EXISTS `user_roles_23` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 23)';

CREATE TABLE IF NOT EXISTS `user_accounts_23` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 23)';

CREATE TABLE IF NOT EXISTS `user_settings_23` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 23)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_23` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 23)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_23` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
    `name`              VARCHAR(128) NOT NULL,
    `legal_name`        VARCHAR(256) DEFAULT NULL,
    `country`           CHAR(2)      NOT NULL DEFAULT 'PH',
    `business_type`     VARCHAR(16)  NOT NULL DEFAULT 'individual',
    `tax_id`            VARCHAR(64)  DEFAULT NULL,
    `contact_email`     VARCHAR(128) NOT NULL,
    `contact_phone`     VARCHAR(32)  DEFAULT NULL,
    `website`           VARCHAR(256) DEFAULT NULL,
    `mcc`               VARCHAR(8)   DEFAULT NULL,
    `live_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `test_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `webhook_url`       VARCHAR(512) NOT NULL DEFAULT '',
    `webhook_secret`    VARCHAR(64)  NOT NULL DEFAULT '',
    `kyc_status`        VARCHAR(24)  NOT NULL DEFAULT 'pending',
    `kyc_level`         TINYINT      NOT NULL DEFAULT 0,
    `kyc_reason`        VARCHAR(512) DEFAULT NULL,
    `kyc_reviewer`      VARCHAR(64)  DEFAULT NULL,
    `kyc_reviewed_at`   DATETIME(3)  DEFAULT NULL,
    `risk_tier`         VARCHAR(16)  NOT NULL DEFAULT 'standard',
    `rate_limit_rps`    INT          NOT NULL DEFAULT 100,
    `settle_currency`   CHAR(3)      NOT NULL DEFAULT 'PHP',
    `settle_method`     VARCHAR(16)  DEFAULT NULL,
    `settle_account`    VARCHAR(128) DEFAULT NULL,
    `settle_bank`       VARCHAR(128) DEFAULT NULL,
    `settle_holder`     VARCHAR(128) DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `metadata`          JSON         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 23)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_23` (
    `id`              VARCHAR(32)  NOT NULL,
    `merchant_id`     VARCHAR(32)  NOT NULL,
    `doc_type`        VARCHAR(32)  NOT NULL,
    `doc_number`      VARCHAR(128) DEFAULT NULL,
    `file_url`        VARCHAR(512) NOT NULL,
    `mime_type`       VARCHAR(64)  DEFAULT NULL,
    `size_bytes`      INT          DEFAULT NULL,
    `uploaded_by`     VARCHAR(64)  DEFAULT NULL,
    `review_status`   VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `review_note`     VARCHAR(512) DEFAULT NULL,
    `expires_at`      DATETIME(3)  DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant`      (`merchant_id`),
    KEY `idx_review_status` (`review_status`),
    KEY `idx_doc_type`      (`doc_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 23)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_23` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `channel`      VARCHAR(32)  NOT NULL,
    `field_name`   VARCHAR(64)  NOT NULL,
    `ciphertext`   VARBINARY(4096) NOT NULL,
    `context`      VARCHAR(256) NOT NULL,
    `masked_hint`  VARCHAR(64)  DEFAULT NULL,
    `version`      INT          NOT NULL DEFAULT 1,
    `created_by`   VARCHAR(64)  DEFAULT NULL,
    `created`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_merchant_channel_field` (`merchant_id`, `channel`, `field_name`),
    KEY `idx_merchant_channel` (`merchant_id`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 23)';

-- ─── admin_audit_log_23：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_23` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 23';

-- 分片表 schema 模板。2 和 24 由 generate.sh 替换：
--   2     = 0..9         分库 idx
--   24  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_24` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 24)';

CREATE TABLE IF NOT EXISTS `user_profiles_24` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 24)';

CREATE TABLE IF NOT EXISTS `user_auths_24` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 24)';

CREATE TABLE IF NOT EXISTS `login_logs_24` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 24)';

CREATE TABLE IF NOT EXISTS `user_sessions_24` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 24)';

CREATE TABLE IF NOT EXISTS `user_roles_24` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 24)';

CREATE TABLE IF NOT EXISTS `user_accounts_24` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 24)';

CREATE TABLE IF NOT EXISTS `user_settings_24` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 24)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_24` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 24)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_24` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
    `name`              VARCHAR(128) NOT NULL,
    `legal_name`        VARCHAR(256) DEFAULT NULL,
    `country`           CHAR(2)      NOT NULL DEFAULT 'PH',
    `business_type`     VARCHAR(16)  NOT NULL DEFAULT 'individual',
    `tax_id`            VARCHAR(64)  DEFAULT NULL,
    `contact_email`     VARCHAR(128) NOT NULL,
    `contact_phone`     VARCHAR(32)  DEFAULT NULL,
    `website`           VARCHAR(256) DEFAULT NULL,
    `mcc`               VARCHAR(8)   DEFAULT NULL,
    `live_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `test_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `webhook_url`       VARCHAR(512) NOT NULL DEFAULT '',
    `webhook_secret`    VARCHAR(64)  NOT NULL DEFAULT '',
    `kyc_status`        VARCHAR(24)  NOT NULL DEFAULT 'pending',
    `kyc_level`         TINYINT      NOT NULL DEFAULT 0,
    `kyc_reason`        VARCHAR(512) DEFAULT NULL,
    `kyc_reviewer`      VARCHAR(64)  DEFAULT NULL,
    `kyc_reviewed_at`   DATETIME(3)  DEFAULT NULL,
    `risk_tier`         VARCHAR(16)  NOT NULL DEFAULT 'standard',
    `rate_limit_rps`    INT          NOT NULL DEFAULT 100,
    `settle_currency`   CHAR(3)      NOT NULL DEFAULT 'PHP',
    `settle_method`     VARCHAR(16)  DEFAULT NULL,
    `settle_account`    VARCHAR(128) DEFAULT NULL,
    `settle_bank`       VARCHAR(128) DEFAULT NULL,
    `settle_holder`     VARCHAR(128) DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `metadata`          JSON         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 24)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_24` (
    `id`              VARCHAR(32)  NOT NULL,
    `merchant_id`     VARCHAR(32)  NOT NULL,
    `doc_type`        VARCHAR(32)  NOT NULL,
    `doc_number`      VARCHAR(128) DEFAULT NULL,
    `file_url`        VARCHAR(512) NOT NULL,
    `mime_type`       VARCHAR(64)  DEFAULT NULL,
    `size_bytes`      INT          DEFAULT NULL,
    `uploaded_by`     VARCHAR(64)  DEFAULT NULL,
    `review_status`   VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `review_note`     VARCHAR(512) DEFAULT NULL,
    `expires_at`      DATETIME(3)  DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant`      (`merchant_id`),
    KEY `idx_review_status` (`review_status`),
    KEY `idx_doc_type`      (`doc_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 24)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_24` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `channel`      VARCHAR(32)  NOT NULL,
    `field_name`   VARCHAR(64)  NOT NULL,
    `ciphertext`   VARBINARY(4096) NOT NULL,
    `context`      VARCHAR(256) NOT NULL,
    `masked_hint`  VARCHAR(64)  DEFAULT NULL,
    `version`      INT          NOT NULL DEFAULT 1,
    `created_by`   VARCHAR(64)  DEFAULT NULL,
    `created`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_merchant_channel_field` (`merchant_id`, `channel`, `field_name`),
    KEY `idx_merchant_channel` (`merchant_id`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 24)';

-- ─── admin_audit_log_24：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_24` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 24';

-- 分片表 schema 模板。2 和 25 由 generate.sh 替换：
--   2     = 0..9         分库 idx
--   25  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_25` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 25)';

CREATE TABLE IF NOT EXISTS `user_profiles_25` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 25)';

CREATE TABLE IF NOT EXISTS `user_auths_25` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 25)';

CREATE TABLE IF NOT EXISTS `login_logs_25` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 25)';

CREATE TABLE IF NOT EXISTS `user_sessions_25` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 25)';

CREATE TABLE IF NOT EXISTS `user_roles_25` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 25)';

CREATE TABLE IF NOT EXISTS `user_accounts_25` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 25)';

CREATE TABLE IF NOT EXISTS `user_settings_25` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 25)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_25` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 25)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_25` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
    `name`              VARCHAR(128) NOT NULL,
    `legal_name`        VARCHAR(256) DEFAULT NULL,
    `country`           CHAR(2)      NOT NULL DEFAULT 'PH',
    `business_type`     VARCHAR(16)  NOT NULL DEFAULT 'individual',
    `tax_id`            VARCHAR(64)  DEFAULT NULL,
    `contact_email`     VARCHAR(128) NOT NULL,
    `contact_phone`     VARCHAR(32)  DEFAULT NULL,
    `website`           VARCHAR(256) DEFAULT NULL,
    `mcc`               VARCHAR(8)   DEFAULT NULL,
    `live_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `test_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `webhook_url`       VARCHAR(512) NOT NULL DEFAULT '',
    `webhook_secret`    VARCHAR(64)  NOT NULL DEFAULT '',
    `kyc_status`        VARCHAR(24)  NOT NULL DEFAULT 'pending',
    `kyc_level`         TINYINT      NOT NULL DEFAULT 0,
    `kyc_reason`        VARCHAR(512) DEFAULT NULL,
    `kyc_reviewer`      VARCHAR(64)  DEFAULT NULL,
    `kyc_reviewed_at`   DATETIME(3)  DEFAULT NULL,
    `risk_tier`         VARCHAR(16)  NOT NULL DEFAULT 'standard',
    `rate_limit_rps`    INT          NOT NULL DEFAULT 100,
    `settle_currency`   CHAR(3)      NOT NULL DEFAULT 'PHP',
    `settle_method`     VARCHAR(16)  DEFAULT NULL,
    `settle_account`    VARCHAR(128) DEFAULT NULL,
    `settle_bank`       VARCHAR(128) DEFAULT NULL,
    `settle_holder`     VARCHAR(128) DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `metadata`          JSON         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 25)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_25` (
    `id`              VARCHAR(32)  NOT NULL,
    `merchant_id`     VARCHAR(32)  NOT NULL,
    `doc_type`        VARCHAR(32)  NOT NULL,
    `doc_number`      VARCHAR(128) DEFAULT NULL,
    `file_url`        VARCHAR(512) NOT NULL,
    `mime_type`       VARCHAR(64)  DEFAULT NULL,
    `size_bytes`      INT          DEFAULT NULL,
    `uploaded_by`     VARCHAR(64)  DEFAULT NULL,
    `review_status`   VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `review_note`     VARCHAR(512) DEFAULT NULL,
    `expires_at`      DATETIME(3)  DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant`      (`merchant_id`),
    KEY `idx_review_status` (`review_status`),
    KEY `idx_doc_type`      (`doc_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 25)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_25` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `channel`      VARCHAR(32)  NOT NULL,
    `field_name`   VARCHAR(64)  NOT NULL,
    `ciphertext`   VARBINARY(4096) NOT NULL,
    `context`      VARCHAR(256) NOT NULL,
    `masked_hint`  VARCHAR(64)  DEFAULT NULL,
    `version`      INT          NOT NULL DEFAULT 1,
    `created_by`   VARCHAR(64)  DEFAULT NULL,
    `created`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_merchant_channel_field` (`merchant_id`, `channel`, `field_name`),
    KEY `idx_merchant_channel` (`merchant_id`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 25)';

-- ─── admin_audit_log_25：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_25` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 25';

-- 分片表 schema 模板。2 和 26 由 generate.sh 替换：
--   2     = 0..9         分库 idx
--   26  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_26` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 26)';

CREATE TABLE IF NOT EXISTS `user_profiles_26` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 26)';

CREATE TABLE IF NOT EXISTS `user_auths_26` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 26)';

CREATE TABLE IF NOT EXISTS `login_logs_26` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 26)';

CREATE TABLE IF NOT EXISTS `user_sessions_26` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 26)';

CREATE TABLE IF NOT EXISTS `user_roles_26` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 26)';

CREATE TABLE IF NOT EXISTS `user_accounts_26` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 26)';

CREATE TABLE IF NOT EXISTS `user_settings_26` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 26)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_26` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 26)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_26` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
    `name`              VARCHAR(128) NOT NULL,
    `legal_name`        VARCHAR(256) DEFAULT NULL,
    `country`           CHAR(2)      NOT NULL DEFAULT 'PH',
    `business_type`     VARCHAR(16)  NOT NULL DEFAULT 'individual',
    `tax_id`            VARCHAR(64)  DEFAULT NULL,
    `contact_email`     VARCHAR(128) NOT NULL,
    `contact_phone`     VARCHAR(32)  DEFAULT NULL,
    `website`           VARCHAR(256) DEFAULT NULL,
    `mcc`               VARCHAR(8)   DEFAULT NULL,
    `live_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `test_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `webhook_url`       VARCHAR(512) NOT NULL DEFAULT '',
    `webhook_secret`    VARCHAR(64)  NOT NULL DEFAULT '',
    `kyc_status`        VARCHAR(24)  NOT NULL DEFAULT 'pending',
    `kyc_level`         TINYINT      NOT NULL DEFAULT 0,
    `kyc_reason`        VARCHAR(512) DEFAULT NULL,
    `kyc_reviewer`      VARCHAR(64)  DEFAULT NULL,
    `kyc_reviewed_at`   DATETIME(3)  DEFAULT NULL,
    `risk_tier`         VARCHAR(16)  NOT NULL DEFAULT 'standard',
    `rate_limit_rps`    INT          NOT NULL DEFAULT 100,
    `settle_currency`   CHAR(3)      NOT NULL DEFAULT 'PHP',
    `settle_method`     VARCHAR(16)  DEFAULT NULL,
    `settle_account`    VARCHAR(128) DEFAULT NULL,
    `settle_bank`       VARCHAR(128) DEFAULT NULL,
    `settle_holder`     VARCHAR(128) DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `metadata`          JSON         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 26)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_26` (
    `id`              VARCHAR(32)  NOT NULL,
    `merchant_id`     VARCHAR(32)  NOT NULL,
    `doc_type`        VARCHAR(32)  NOT NULL,
    `doc_number`      VARCHAR(128) DEFAULT NULL,
    `file_url`        VARCHAR(512) NOT NULL,
    `mime_type`       VARCHAR(64)  DEFAULT NULL,
    `size_bytes`      INT          DEFAULT NULL,
    `uploaded_by`     VARCHAR(64)  DEFAULT NULL,
    `review_status`   VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `review_note`     VARCHAR(512) DEFAULT NULL,
    `expires_at`      DATETIME(3)  DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant`      (`merchant_id`),
    KEY `idx_review_status` (`review_status`),
    KEY `idx_doc_type`      (`doc_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 26)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_26` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `channel`      VARCHAR(32)  NOT NULL,
    `field_name`   VARCHAR(64)  NOT NULL,
    `ciphertext`   VARBINARY(4096) NOT NULL,
    `context`      VARCHAR(256) NOT NULL,
    `masked_hint`  VARCHAR(64)  DEFAULT NULL,
    `version`      INT          NOT NULL DEFAULT 1,
    `created_by`   VARCHAR(64)  DEFAULT NULL,
    `created`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_merchant_channel_field` (`merchant_id`, `channel`, `field_name`),
    KEY `idx_merchant_channel` (`merchant_id`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 26)';

-- ─── admin_audit_log_26：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_26` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 26';

-- 分片表 schema 模板。2 和 27 由 generate.sh 替换：
--   2     = 0..9         分库 idx
--   27  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_27` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 27)';

CREATE TABLE IF NOT EXISTS `user_profiles_27` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 27)';

CREATE TABLE IF NOT EXISTS `user_auths_27` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 27)';

CREATE TABLE IF NOT EXISTS `login_logs_27` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 27)';

CREATE TABLE IF NOT EXISTS `user_sessions_27` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 27)';

CREATE TABLE IF NOT EXISTS `user_roles_27` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 27)';

CREATE TABLE IF NOT EXISTS `user_accounts_27` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 27)';

CREATE TABLE IF NOT EXISTS `user_settings_27` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 27)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_27` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 27)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_27` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
    `name`              VARCHAR(128) NOT NULL,
    `legal_name`        VARCHAR(256) DEFAULT NULL,
    `country`           CHAR(2)      NOT NULL DEFAULT 'PH',
    `business_type`     VARCHAR(16)  NOT NULL DEFAULT 'individual',
    `tax_id`            VARCHAR(64)  DEFAULT NULL,
    `contact_email`     VARCHAR(128) NOT NULL,
    `contact_phone`     VARCHAR(32)  DEFAULT NULL,
    `website`           VARCHAR(256) DEFAULT NULL,
    `mcc`               VARCHAR(8)   DEFAULT NULL,
    `live_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `test_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `webhook_url`       VARCHAR(512) NOT NULL DEFAULT '',
    `webhook_secret`    VARCHAR(64)  NOT NULL DEFAULT '',
    `kyc_status`        VARCHAR(24)  NOT NULL DEFAULT 'pending',
    `kyc_level`         TINYINT      NOT NULL DEFAULT 0,
    `kyc_reason`        VARCHAR(512) DEFAULT NULL,
    `kyc_reviewer`      VARCHAR(64)  DEFAULT NULL,
    `kyc_reviewed_at`   DATETIME(3)  DEFAULT NULL,
    `risk_tier`         VARCHAR(16)  NOT NULL DEFAULT 'standard',
    `rate_limit_rps`    INT          NOT NULL DEFAULT 100,
    `settle_currency`   CHAR(3)      NOT NULL DEFAULT 'PHP',
    `settle_method`     VARCHAR(16)  DEFAULT NULL,
    `settle_account`    VARCHAR(128) DEFAULT NULL,
    `settle_bank`       VARCHAR(128) DEFAULT NULL,
    `settle_holder`     VARCHAR(128) DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `metadata`          JSON         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 27)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_27` (
    `id`              VARCHAR(32)  NOT NULL,
    `merchant_id`     VARCHAR(32)  NOT NULL,
    `doc_type`        VARCHAR(32)  NOT NULL,
    `doc_number`      VARCHAR(128) DEFAULT NULL,
    `file_url`        VARCHAR(512) NOT NULL,
    `mime_type`       VARCHAR(64)  DEFAULT NULL,
    `size_bytes`      INT          DEFAULT NULL,
    `uploaded_by`     VARCHAR(64)  DEFAULT NULL,
    `review_status`   VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `review_note`     VARCHAR(512) DEFAULT NULL,
    `expires_at`      DATETIME(3)  DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant`      (`merchant_id`),
    KEY `idx_review_status` (`review_status`),
    KEY `idx_doc_type`      (`doc_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 27)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_27` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `channel`      VARCHAR(32)  NOT NULL,
    `field_name`   VARCHAR(64)  NOT NULL,
    `ciphertext`   VARBINARY(4096) NOT NULL,
    `context`      VARCHAR(256) NOT NULL,
    `masked_hint`  VARCHAR(64)  DEFAULT NULL,
    `version`      INT          NOT NULL DEFAULT 1,
    `created_by`   VARCHAR(64)  DEFAULT NULL,
    `created`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_merchant_channel_field` (`merchant_id`, `channel`, `field_name`),
    KEY `idx_merchant_channel` (`merchant_id`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 27)';

-- ─── admin_audit_log_27：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_27` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 27';

-- 分片表 schema 模板。2 和 28 由 generate.sh 替换：
--   2     = 0..9         分库 idx
--   28  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_28` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 28)';

CREATE TABLE IF NOT EXISTS `user_profiles_28` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 28)';

CREATE TABLE IF NOT EXISTS `user_auths_28` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 28)';

CREATE TABLE IF NOT EXISTS `login_logs_28` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 28)';

CREATE TABLE IF NOT EXISTS `user_sessions_28` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 28)';

CREATE TABLE IF NOT EXISTS `user_roles_28` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 28)';

CREATE TABLE IF NOT EXISTS `user_accounts_28` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 28)';

CREATE TABLE IF NOT EXISTS `user_settings_28` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 28)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_28` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 28)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_28` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
    `name`              VARCHAR(128) NOT NULL,
    `legal_name`        VARCHAR(256) DEFAULT NULL,
    `country`           CHAR(2)      NOT NULL DEFAULT 'PH',
    `business_type`     VARCHAR(16)  NOT NULL DEFAULT 'individual',
    `tax_id`            VARCHAR(64)  DEFAULT NULL,
    `contact_email`     VARCHAR(128) NOT NULL,
    `contact_phone`     VARCHAR(32)  DEFAULT NULL,
    `website`           VARCHAR(256) DEFAULT NULL,
    `mcc`               VARCHAR(8)   DEFAULT NULL,
    `live_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `test_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `webhook_url`       VARCHAR(512) NOT NULL DEFAULT '',
    `webhook_secret`    VARCHAR(64)  NOT NULL DEFAULT '',
    `kyc_status`        VARCHAR(24)  NOT NULL DEFAULT 'pending',
    `kyc_level`         TINYINT      NOT NULL DEFAULT 0,
    `kyc_reason`        VARCHAR(512) DEFAULT NULL,
    `kyc_reviewer`      VARCHAR(64)  DEFAULT NULL,
    `kyc_reviewed_at`   DATETIME(3)  DEFAULT NULL,
    `risk_tier`         VARCHAR(16)  NOT NULL DEFAULT 'standard',
    `rate_limit_rps`    INT          NOT NULL DEFAULT 100,
    `settle_currency`   CHAR(3)      NOT NULL DEFAULT 'PHP',
    `settle_method`     VARCHAR(16)  DEFAULT NULL,
    `settle_account`    VARCHAR(128) DEFAULT NULL,
    `settle_bank`       VARCHAR(128) DEFAULT NULL,
    `settle_holder`     VARCHAR(128) DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `metadata`          JSON         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 28)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_28` (
    `id`              VARCHAR(32)  NOT NULL,
    `merchant_id`     VARCHAR(32)  NOT NULL,
    `doc_type`        VARCHAR(32)  NOT NULL,
    `doc_number`      VARCHAR(128) DEFAULT NULL,
    `file_url`        VARCHAR(512) NOT NULL,
    `mime_type`       VARCHAR(64)  DEFAULT NULL,
    `size_bytes`      INT          DEFAULT NULL,
    `uploaded_by`     VARCHAR(64)  DEFAULT NULL,
    `review_status`   VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `review_note`     VARCHAR(512) DEFAULT NULL,
    `expires_at`      DATETIME(3)  DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant`      (`merchant_id`),
    KEY `idx_review_status` (`review_status`),
    KEY `idx_doc_type`      (`doc_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 28)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_28` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `channel`      VARCHAR(32)  NOT NULL,
    `field_name`   VARCHAR(64)  NOT NULL,
    `ciphertext`   VARBINARY(4096) NOT NULL,
    `context`      VARCHAR(256) NOT NULL,
    `masked_hint`  VARCHAR(64)  DEFAULT NULL,
    `version`      INT          NOT NULL DEFAULT 1,
    `created_by`   VARCHAR(64)  DEFAULT NULL,
    `created`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_merchant_channel_field` (`merchant_id`, `channel`, `field_name`),
    KEY `idx_merchant_channel` (`merchant_id`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 28)';

-- ─── admin_audit_log_28：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_28` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 28';

-- 分片表 schema 模板。2 和 29 由 generate.sh 替换：
--   2     = 0..9         分库 idx
--   29  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_29` (
    `id`                   BIGINT       NOT NULL,
    `username`             VARCHAR(50)  NULL,
    `email`                VARCHAR(100) NULL,
    `phone`                VARCHAR(20)  NULL,
    `password_hash`        VARCHAR(255) NOT NULL,
    `status`               TINYINT      NOT NULL DEFAULT 1 COMMENT '1=active 0=disabled 2=locked 3=deleted',
    `totp_secret`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'base32 TOTP；空=未启 2FA',
    `totp_enabled`         TINYINT(1)   NOT NULL DEFAULT 0,
    `email_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `phone_verified`       TINYINT(1)   NOT NULL DEFAULT 0,
    `failed_login_count`   INT          NOT NULL DEFAULT 0,
    `locked_until`         DATETIME(3)  NULL,
    `last_login_at`        DATETIME(3)  NULL,
    `password_changed_at`  DATETIME(3)  NULL COMMENT '最后改密时间，给风控 ATO 规则用',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_username` (`username`),
    UNIQUE KEY `uk_email`    (`email`),
    UNIQUE KEY `uk_phone`    (`phone`),
    KEY `idx_status`         (`status`),
    KEY `idx_locked_until`   (`locked_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 29)';

CREATE TABLE IF NOT EXISTS `user_profiles_29` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 29)';

CREATE TABLE IF NOT EXISTS `user_auths_29` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `auth_type`   VARCHAR(20)  NOT NULL,
    `identifier`  VARCHAR(100) NOT NULL,
    `credential`  VARCHAR(255) NOT NULL DEFAULT '',
    `verified`    TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_auth` (`auth_type`, `identifier`),
    KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 29)';

CREATE TABLE IF NOT EXISTS `login_logs_29` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NULL COMMENT '失败时可空（用户名不存在）',
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `auth_type`   VARCHAR(20)  NOT NULL DEFAULT '',
    `status`      TINYINT      NOT NULL DEFAULT 0 COMMENT '1=success 0=failed',
    `reason`      VARCHAR(64)  NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_user_id`    (`user_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 29)';

CREATE TABLE IF NOT EXISTS `user_sessions_29` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    -- token VARCHAR(512)：JWT 加 kid claim 后约 264 字符，旧 VARCHAR(255) 截断。
    `token`       VARCHAR(512) NOT NULL,
    `ip`          VARCHAR(45)  NOT NULL DEFAULT '',
    `user_agent`  VARCHAR(255) NOT NULL DEFAULT '',
    `expires_at`  DATETIME(3)  NOT NULL,
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    KEY `idx_user_id`     (`user_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 29)';

CREATE TABLE IF NOT EXISTS `user_roles_29` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 29)';

CREATE TABLE IF NOT EXISTS `user_accounts_29` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 29)';

CREATE TABLE IF NOT EXISTS `user_settings_29` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 29)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_29` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`        BIGINT       NOT NULL,
    `stored_token`   VARCHAR(1024) NOT NULL COMMENT 'card-center 颁发的长期 token',
    `token_hash`     CHAR(64)     NOT NULL COMMENT 'sha256(stored_token)',
    `masked_pan`     VARCHAR(20)  NOT NULL COMMENT 'BIN+last4',
    `network`        VARCHAR(16)  NOT NULL COMMENT 'visa / mastercard / jcb / amex / unionpay',
    `exp_month`      TINYINT      NOT NULL,
    `exp_year`       SMALLINT     NOT NULL,
    `holder_name`    VARCHAR(64)  NOT NULL DEFAULT '',
    `is_default`     BOOLEAN      NOT NULL DEFAULT FALSE,
    `status`         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active / deleted / expired',
    `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`     DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),
    KEY `idx_user_status` (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 29)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_29` (
    `id`                VARCHAR(32)  NOT NULL COMMENT 'mch_xxx (按位编码 numeric)',
    `name`              VARCHAR(128) NOT NULL,
    `legal_name`        VARCHAR(256) DEFAULT NULL,
    `country`           CHAR(2)      NOT NULL DEFAULT 'PH',
    `business_type`     VARCHAR(16)  NOT NULL DEFAULT 'individual',
    `tax_id`            VARCHAR(64)  DEFAULT NULL,
    `contact_email`     VARCHAR(128) NOT NULL,
    `contact_phone`     VARCHAR(32)  DEFAULT NULL,
    `website`           VARCHAR(256) DEFAULT NULL,
    `mcc`               VARCHAR(8)   DEFAULT NULL,
    `live_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `test_key_hash`     VARCHAR(64)  NOT NULL DEFAULT '',
    `webhook_url`       VARCHAR(512) NOT NULL DEFAULT '',
    `webhook_secret`    VARCHAR(64)  NOT NULL DEFAULT '',
    `kyc_status`        VARCHAR(24)  NOT NULL DEFAULT 'pending',
    `kyc_level`         TINYINT      NOT NULL DEFAULT 0,
    `kyc_reason`        VARCHAR(512) DEFAULT NULL,
    `kyc_reviewer`      VARCHAR(64)  DEFAULT NULL,
    `kyc_reviewed_at`   DATETIME(3)  DEFAULT NULL,
    `risk_tier`         VARCHAR(16)  NOT NULL DEFAULT 'standard',
    `rate_limit_rps`    INT          NOT NULL DEFAULT 100,
    `settle_currency`   CHAR(3)      NOT NULL DEFAULT 'PHP',
    `settle_method`     VARCHAR(16)  DEFAULT NULL,
    `settle_account`    VARCHAR(128) DEFAULT NULL,
    `settle_bank`       VARCHAR(128) DEFAULT NULL,
    `settle_holder`     VARCHAR(128) DEFAULT NULL,
    `status`            VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `metadata`          JSON         DEFAULT NULL,
    `created`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`        DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_email`        (`contact_email`),
    INDEX       `idx_live_key`    (`live_key_hash`),
    INDEX       `idx_test_key`    (`test_key_hash`),
    INDEX       `idx_kyc_status`  (`kyc_status`),
    INDEX       `idx_status`      (`status`),
    INDEX       `idx_country`     (`country`),
    INDEX       `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 29)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_29` (
    `id`              VARCHAR(32)  NOT NULL,
    `merchant_id`     VARCHAR(32)  NOT NULL,
    `doc_type`        VARCHAR(32)  NOT NULL,
    `doc_number`      VARCHAR(128) DEFAULT NULL,
    `file_url`        VARCHAR(512) NOT NULL,
    `mime_type`       VARCHAR(64)  DEFAULT NULL,
    `size_bytes`      INT          DEFAULT NULL,
    `uploaded_by`     VARCHAR(64)  DEFAULT NULL,
    `review_status`   VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `review_note`     VARCHAR(512) DEFAULT NULL,
    `expires_at`      DATETIME(3)  DEFAULT NULL,
    `created`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_merchant`      (`merchant_id`),
    KEY `idx_review_status` (`review_status`),
    KEY `idx_doc_type`      (`doc_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 29)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_29` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `merchant_id`  VARCHAR(32)  NOT NULL,
    `channel`      VARCHAR(32)  NOT NULL,
    `field_name`   VARCHAR(64)  NOT NULL,
    `ciphertext`   VARBINARY(4096) NOT NULL,
    `context`      VARCHAR(256) NOT NULL,
    `masked_hint`  VARCHAR(64)  DEFAULT NULL,
    `version`      INT          NOT NULL DEFAULT 1,
    `created_by`   VARCHAR(64)  DEFAULT NULL,
    `created`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_merchant_channel_field` (`merchant_id`, `channel`, `field_name`),
    KEY `idx_merchant_channel` (`merchant_id`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 29)';

-- ─── admin_audit_log_29：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_29` (
    `id`             BIGINT       NOT NULL AUTO_INCREMENT,
    `actor`          VARCHAR(64)  NOT NULL COMMENT 'admin user id / service account',
    `actor_ip`       VARCHAR(64)           DEFAULT NULL,
    `method`         VARCHAR(128) NOT NULL COMMENT 'gRPC fullMethod',
    `target_id`      VARCHAR(64)           DEFAULT NULL,
    `request_body`   MEDIUMTEXT            DEFAULT NULL COMMENT 'JSON; sensitive fields redacted by caller',
    `status_code`    VARCHAR(32)  NOT NULL,
    `response_err`   VARCHAR(512)          DEFAULT NULL,
    `duration_ms`    INT                   DEFAULT NULL,
    `trace_id`       VARCHAR(64)           DEFAULT NULL,
    `prev_hash`      CHAR(64)              DEFAULT NULL COMMENT 'per-shard chain head',
    `row_hash`       CHAR(64)     NOT NULL,
    `created`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_actor`   (`actor`),
    KEY `idx_method`  (`method`),
    KEY `idx_target`  (`target_id`),
    KEY `idx_trace`   (`trace_id`),
    KEY `idx_created` (`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 29';


-- ==== payment-channel _shadow ====
-- paychan_db_2 的影子表（压测 / shadow 流量）
-- 依赖：2_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_2`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_20_shadow` LIKE `acquirer_tx_20`;
CREATE TABLE IF NOT EXISTS `webhook_raw_20_shadow` LIKE `webhook_raw_20`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_20_shadow` LIKE `webhook_raw_rejected_20`;
CREATE TABLE IF NOT EXISTS `channel_token_20_shadow` LIKE `channel_token_20`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_21_shadow` LIKE `acquirer_tx_21`;
CREATE TABLE IF NOT EXISTS `webhook_raw_21_shadow` LIKE `webhook_raw_21`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_21_shadow` LIKE `webhook_raw_rejected_21`;
CREATE TABLE IF NOT EXISTS `channel_token_21_shadow` LIKE `channel_token_21`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_22_shadow` LIKE `acquirer_tx_22`;
CREATE TABLE IF NOT EXISTS `webhook_raw_22_shadow` LIKE `webhook_raw_22`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_22_shadow` LIKE `webhook_raw_rejected_22`;
CREATE TABLE IF NOT EXISTS `channel_token_22_shadow` LIKE `channel_token_22`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_23_shadow` LIKE `acquirer_tx_23`;
CREATE TABLE IF NOT EXISTS `webhook_raw_23_shadow` LIKE `webhook_raw_23`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_23_shadow` LIKE `webhook_raw_rejected_23`;
CREATE TABLE IF NOT EXISTS `channel_token_23_shadow` LIKE `channel_token_23`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_24_shadow` LIKE `acquirer_tx_24`;
CREATE TABLE IF NOT EXISTS `webhook_raw_24_shadow` LIKE `webhook_raw_24`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_24_shadow` LIKE `webhook_raw_rejected_24`;
CREATE TABLE IF NOT EXISTS `channel_token_24_shadow` LIKE `channel_token_24`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_25_shadow` LIKE `acquirer_tx_25`;
CREATE TABLE IF NOT EXISTS `webhook_raw_25_shadow` LIKE `webhook_raw_25`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_25_shadow` LIKE `webhook_raw_rejected_25`;
CREATE TABLE IF NOT EXISTS `channel_token_25_shadow` LIKE `channel_token_25`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_26_shadow` LIKE `acquirer_tx_26`;
CREATE TABLE IF NOT EXISTS `webhook_raw_26_shadow` LIKE `webhook_raw_26`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_26_shadow` LIKE `webhook_raw_rejected_26`;
CREATE TABLE IF NOT EXISTS `channel_token_26_shadow` LIKE `channel_token_26`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_27_shadow` LIKE `acquirer_tx_27`;
CREATE TABLE IF NOT EXISTS `webhook_raw_27_shadow` LIKE `webhook_raw_27`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_27_shadow` LIKE `webhook_raw_rejected_27`;
CREATE TABLE IF NOT EXISTS `channel_token_27_shadow` LIKE `channel_token_27`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_28_shadow` LIKE `acquirer_tx_28`;
CREATE TABLE IF NOT EXISTS `webhook_raw_28_shadow` LIKE `webhook_raw_28`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_28_shadow` LIKE `webhook_raw_rejected_28`;
CREATE TABLE IF NOT EXISTS `channel_token_28_shadow` LIKE `channel_token_28`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_29_shadow` LIKE `acquirer_tx_29`;
CREATE TABLE IF NOT EXISTS `webhook_raw_29_shadow` LIKE `webhook_raw_29`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_29_shadow` LIKE `webhook_raw_rejected_29`;
CREATE TABLE IF NOT EXISTS `channel_token_29_shadow` LIKE `channel_token_29`;


-- ==== order-core _shadow ====
-- order_db_2 的影子表（压测 / shadow 流量）
-- 依赖：2_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_2`;

CREATE TABLE IF NOT EXISTS `payment_intent_20_shadow` LIKE `payment_intent_20`;
CREATE TABLE IF NOT EXISTS `charge_20_shadow` LIKE `charge_20`;
CREATE TABLE IF NOT EXISTS `refund_20_shadow` LIKE `refund_20`;
CREATE TABLE IF NOT EXISTS `pay_action_20_shadow` LIKE `pay_action_20`;
CREATE TABLE IF NOT EXISTS `dispute_20_shadow` LIKE `dispute_20`;
CREATE TABLE IF NOT EXISTS `dispute_event_20_shadow` LIKE `dispute_event_20`;
CREATE TABLE IF NOT EXISTS `exception_case_20_shadow` LIKE `exception_case_20`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_20_shadow` LIKE `inbound_webhook_20`;
CREATE TABLE IF NOT EXISTS `notify_log_20_shadow` LIKE `notify_log_20`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_20_shadow` LIKE `accounting_outbox_20`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_20_shadow` LIKE `admin_audit_log_20`;

CREATE TABLE IF NOT EXISTS `payment_intent_21_shadow` LIKE `payment_intent_21`;
CREATE TABLE IF NOT EXISTS `charge_21_shadow` LIKE `charge_21`;
CREATE TABLE IF NOT EXISTS `refund_21_shadow` LIKE `refund_21`;
CREATE TABLE IF NOT EXISTS `pay_action_21_shadow` LIKE `pay_action_21`;
CREATE TABLE IF NOT EXISTS `dispute_21_shadow` LIKE `dispute_21`;
CREATE TABLE IF NOT EXISTS `dispute_event_21_shadow` LIKE `dispute_event_21`;
CREATE TABLE IF NOT EXISTS `exception_case_21_shadow` LIKE `exception_case_21`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_21_shadow` LIKE `inbound_webhook_21`;
CREATE TABLE IF NOT EXISTS `notify_log_21_shadow` LIKE `notify_log_21`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_21_shadow` LIKE `accounting_outbox_21`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_21_shadow` LIKE `admin_audit_log_21`;

CREATE TABLE IF NOT EXISTS `payment_intent_22_shadow` LIKE `payment_intent_22`;
CREATE TABLE IF NOT EXISTS `charge_22_shadow` LIKE `charge_22`;
CREATE TABLE IF NOT EXISTS `refund_22_shadow` LIKE `refund_22`;
CREATE TABLE IF NOT EXISTS `pay_action_22_shadow` LIKE `pay_action_22`;
CREATE TABLE IF NOT EXISTS `dispute_22_shadow` LIKE `dispute_22`;
CREATE TABLE IF NOT EXISTS `dispute_event_22_shadow` LIKE `dispute_event_22`;
CREATE TABLE IF NOT EXISTS `exception_case_22_shadow` LIKE `exception_case_22`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_22_shadow` LIKE `inbound_webhook_22`;
CREATE TABLE IF NOT EXISTS `notify_log_22_shadow` LIKE `notify_log_22`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_22_shadow` LIKE `accounting_outbox_22`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_22_shadow` LIKE `admin_audit_log_22`;

CREATE TABLE IF NOT EXISTS `payment_intent_23_shadow` LIKE `payment_intent_23`;
CREATE TABLE IF NOT EXISTS `charge_23_shadow` LIKE `charge_23`;
CREATE TABLE IF NOT EXISTS `refund_23_shadow` LIKE `refund_23`;
CREATE TABLE IF NOT EXISTS `pay_action_23_shadow` LIKE `pay_action_23`;
CREATE TABLE IF NOT EXISTS `dispute_23_shadow` LIKE `dispute_23`;
CREATE TABLE IF NOT EXISTS `dispute_event_23_shadow` LIKE `dispute_event_23`;
CREATE TABLE IF NOT EXISTS `exception_case_23_shadow` LIKE `exception_case_23`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_23_shadow` LIKE `inbound_webhook_23`;
CREATE TABLE IF NOT EXISTS `notify_log_23_shadow` LIKE `notify_log_23`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_23_shadow` LIKE `accounting_outbox_23`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_23_shadow` LIKE `admin_audit_log_23`;

CREATE TABLE IF NOT EXISTS `payment_intent_24_shadow` LIKE `payment_intent_24`;
CREATE TABLE IF NOT EXISTS `charge_24_shadow` LIKE `charge_24`;
CREATE TABLE IF NOT EXISTS `refund_24_shadow` LIKE `refund_24`;
CREATE TABLE IF NOT EXISTS `pay_action_24_shadow` LIKE `pay_action_24`;
CREATE TABLE IF NOT EXISTS `dispute_24_shadow` LIKE `dispute_24`;
CREATE TABLE IF NOT EXISTS `dispute_event_24_shadow` LIKE `dispute_event_24`;
CREATE TABLE IF NOT EXISTS `exception_case_24_shadow` LIKE `exception_case_24`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_24_shadow` LIKE `inbound_webhook_24`;
CREATE TABLE IF NOT EXISTS `notify_log_24_shadow` LIKE `notify_log_24`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_24_shadow` LIKE `accounting_outbox_24`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_24_shadow` LIKE `admin_audit_log_24`;

CREATE TABLE IF NOT EXISTS `payment_intent_25_shadow` LIKE `payment_intent_25`;
CREATE TABLE IF NOT EXISTS `charge_25_shadow` LIKE `charge_25`;
CREATE TABLE IF NOT EXISTS `refund_25_shadow` LIKE `refund_25`;
CREATE TABLE IF NOT EXISTS `pay_action_25_shadow` LIKE `pay_action_25`;
CREATE TABLE IF NOT EXISTS `dispute_25_shadow` LIKE `dispute_25`;
CREATE TABLE IF NOT EXISTS `dispute_event_25_shadow` LIKE `dispute_event_25`;
CREATE TABLE IF NOT EXISTS `exception_case_25_shadow` LIKE `exception_case_25`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_25_shadow` LIKE `inbound_webhook_25`;
CREATE TABLE IF NOT EXISTS `notify_log_25_shadow` LIKE `notify_log_25`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_25_shadow` LIKE `accounting_outbox_25`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_25_shadow` LIKE `admin_audit_log_25`;

CREATE TABLE IF NOT EXISTS `payment_intent_26_shadow` LIKE `payment_intent_26`;
CREATE TABLE IF NOT EXISTS `charge_26_shadow` LIKE `charge_26`;
CREATE TABLE IF NOT EXISTS `refund_26_shadow` LIKE `refund_26`;
CREATE TABLE IF NOT EXISTS `pay_action_26_shadow` LIKE `pay_action_26`;
CREATE TABLE IF NOT EXISTS `dispute_26_shadow` LIKE `dispute_26`;
CREATE TABLE IF NOT EXISTS `dispute_event_26_shadow` LIKE `dispute_event_26`;
CREATE TABLE IF NOT EXISTS `exception_case_26_shadow` LIKE `exception_case_26`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_26_shadow` LIKE `inbound_webhook_26`;
CREATE TABLE IF NOT EXISTS `notify_log_26_shadow` LIKE `notify_log_26`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_26_shadow` LIKE `accounting_outbox_26`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_26_shadow` LIKE `admin_audit_log_26`;

CREATE TABLE IF NOT EXISTS `payment_intent_27_shadow` LIKE `payment_intent_27`;
CREATE TABLE IF NOT EXISTS `charge_27_shadow` LIKE `charge_27`;
CREATE TABLE IF NOT EXISTS `refund_27_shadow` LIKE `refund_27`;
CREATE TABLE IF NOT EXISTS `pay_action_27_shadow` LIKE `pay_action_27`;
CREATE TABLE IF NOT EXISTS `dispute_27_shadow` LIKE `dispute_27`;
CREATE TABLE IF NOT EXISTS `dispute_event_27_shadow` LIKE `dispute_event_27`;
CREATE TABLE IF NOT EXISTS `exception_case_27_shadow` LIKE `exception_case_27`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_27_shadow` LIKE `inbound_webhook_27`;
CREATE TABLE IF NOT EXISTS `notify_log_27_shadow` LIKE `notify_log_27`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_27_shadow` LIKE `accounting_outbox_27`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_27_shadow` LIKE `admin_audit_log_27`;

CREATE TABLE IF NOT EXISTS `payment_intent_28_shadow` LIKE `payment_intent_28`;
CREATE TABLE IF NOT EXISTS `charge_28_shadow` LIKE `charge_28`;
CREATE TABLE IF NOT EXISTS `refund_28_shadow` LIKE `refund_28`;
CREATE TABLE IF NOT EXISTS `pay_action_28_shadow` LIKE `pay_action_28`;
CREATE TABLE IF NOT EXISTS `dispute_28_shadow` LIKE `dispute_28`;
CREATE TABLE IF NOT EXISTS `dispute_event_28_shadow` LIKE `dispute_event_28`;
CREATE TABLE IF NOT EXISTS `exception_case_28_shadow` LIKE `exception_case_28`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_28_shadow` LIKE `inbound_webhook_28`;
CREATE TABLE IF NOT EXISTS `notify_log_28_shadow` LIKE `notify_log_28`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_28_shadow` LIKE `accounting_outbox_28`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_28_shadow` LIKE `admin_audit_log_28`;

CREATE TABLE IF NOT EXISTS `payment_intent_29_shadow` LIKE `payment_intent_29`;
CREATE TABLE IF NOT EXISTS `charge_29_shadow` LIKE `charge_29`;
CREATE TABLE IF NOT EXISTS `refund_29_shadow` LIKE `refund_29`;
CREATE TABLE IF NOT EXISTS `pay_action_29_shadow` LIKE `pay_action_29`;
CREATE TABLE IF NOT EXISTS `dispute_29_shadow` LIKE `dispute_29`;
CREATE TABLE IF NOT EXISTS `dispute_event_29_shadow` LIKE `dispute_event_29`;
CREATE TABLE IF NOT EXISTS `exception_case_29_shadow` LIKE `exception_case_29`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_29_shadow` LIKE `inbound_webhook_29`;
CREATE TABLE IF NOT EXISTS `notify_log_29_shadow` LIKE `notify_log_29`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_29_shadow` LIKE `accounting_outbox_29`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_29_shadow` LIKE `admin_audit_log_29`;


-- ==== accounting-system _shadow ====
-- accounting_db_2 的影子表（压测 / shadow 流量）
-- 依赖：2_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
SET NAMES utf8mb4;
USE `accounting_db_2`;

CREATE TABLE IF NOT EXISTS `account_20_shadow` LIKE `account_20`;
CREATE TABLE IF NOT EXISTS `account_transaction_20_shadow` LIKE `account_transaction_20`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_20_shadow` LIKE `accounting_voucher_20`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_20_shadow` LIKE `account_balance_snapshot_20`;
CREATE TABLE IF NOT EXISTS `day_cut_control_20_shadow` LIKE `day_cut_control_20`;
CREATE TABLE IF NOT EXISTS `async_task_20_shadow` LIKE `async_task_20`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_20_shadow` LIKE `tcc_transaction_20`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_20_shadow` LIKE `freeze_compensate_outbox_20`;
CREATE TABLE IF NOT EXISTS `distributed_lock_20_shadow` LIKE `distributed_lock_20`;
CREATE TABLE IF NOT EXISTS `merchant_info_20_shadow` LIKE `merchant_info_20`;
CREATE TABLE IF NOT EXISTS `transaction_order_20_shadow` LIKE `transaction_order_20`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_20_shadow` LIKE `transaction_order_extra_20`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_20_shadow` LIKE `account_balance_buffer_20`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_20_shadow` LIKE `tcc_coordinator_20`;
CREATE TABLE IF NOT EXISTS `batch_order_20_shadow` LIKE `batch_order_20`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_20_shadow` LIKE `settlement_outbox_20`;

CREATE TABLE IF NOT EXISTS `account_21_shadow` LIKE `account_21`;
CREATE TABLE IF NOT EXISTS `account_transaction_21_shadow` LIKE `account_transaction_21`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_21_shadow` LIKE `accounting_voucher_21`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_21_shadow` LIKE `account_balance_snapshot_21`;
CREATE TABLE IF NOT EXISTS `day_cut_control_21_shadow` LIKE `day_cut_control_21`;
CREATE TABLE IF NOT EXISTS `async_task_21_shadow` LIKE `async_task_21`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_21_shadow` LIKE `tcc_transaction_21`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_21_shadow` LIKE `freeze_compensate_outbox_21`;
CREATE TABLE IF NOT EXISTS `distributed_lock_21_shadow` LIKE `distributed_lock_21`;
CREATE TABLE IF NOT EXISTS `merchant_info_21_shadow` LIKE `merchant_info_21`;
CREATE TABLE IF NOT EXISTS `transaction_order_21_shadow` LIKE `transaction_order_21`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_21_shadow` LIKE `transaction_order_extra_21`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_21_shadow` LIKE `account_balance_buffer_21`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_21_shadow` LIKE `tcc_coordinator_21`;
CREATE TABLE IF NOT EXISTS `batch_order_21_shadow` LIKE `batch_order_21`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_21_shadow` LIKE `settlement_outbox_21`;

CREATE TABLE IF NOT EXISTS `account_22_shadow` LIKE `account_22`;
CREATE TABLE IF NOT EXISTS `account_transaction_22_shadow` LIKE `account_transaction_22`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_22_shadow` LIKE `accounting_voucher_22`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_22_shadow` LIKE `account_balance_snapshot_22`;
CREATE TABLE IF NOT EXISTS `day_cut_control_22_shadow` LIKE `day_cut_control_22`;
CREATE TABLE IF NOT EXISTS `async_task_22_shadow` LIKE `async_task_22`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_22_shadow` LIKE `tcc_transaction_22`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_22_shadow` LIKE `freeze_compensate_outbox_22`;
CREATE TABLE IF NOT EXISTS `distributed_lock_22_shadow` LIKE `distributed_lock_22`;
CREATE TABLE IF NOT EXISTS `merchant_info_22_shadow` LIKE `merchant_info_22`;
CREATE TABLE IF NOT EXISTS `transaction_order_22_shadow` LIKE `transaction_order_22`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_22_shadow` LIKE `transaction_order_extra_22`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_22_shadow` LIKE `account_balance_buffer_22`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_22_shadow` LIKE `tcc_coordinator_22`;
CREATE TABLE IF NOT EXISTS `batch_order_22_shadow` LIKE `batch_order_22`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_22_shadow` LIKE `settlement_outbox_22`;

CREATE TABLE IF NOT EXISTS `account_23_shadow` LIKE `account_23`;
CREATE TABLE IF NOT EXISTS `account_transaction_23_shadow` LIKE `account_transaction_23`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_23_shadow` LIKE `accounting_voucher_23`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_23_shadow` LIKE `account_balance_snapshot_23`;
CREATE TABLE IF NOT EXISTS `day_cut_control_23_shadow` LIKE `day_cut_control_23`;
CREATE TABLE IF NOT EXISTS `async_task_23_shadow` LIKE `async_task_23`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_23_shadow` LIKE `tcc_transaction_23`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_23_shadow` LIKE `freeze_compensate_outbox_23`;
CREATE TABLE IF NOT EXISTS `distributed_lock_23_shadow` LIKE `distributed_lock_23`;
CREATE TABLE IF NOT EXISTS `merchant_info_23_shadow` LIKE `merchant_info_23`;
CREATE TABLE IF NOT EXISTS `transaction_order_23_shadow` LIKE `transaction_order_23`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_23_shadow` LIKE `transaction_order_extra_23`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_23_shadow` LIKE `account_balance_buffer_23`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_23_shadow` LIKE `tcc_coordinator_23`;
CREATE TABLE IF NOT EXISTS `batch_order_23_shadow` LIKE `batch_order_23`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_23_shadow` LIKE `settlement_outbox_23`;

CREATE TABLE IF NOT EXISTS `account_24_shadow` LIKE `account_24`;
CREATE TABLE IF NOT EXISTS `account_transaction_24_shadow` LIKE `account_transaction_24`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_24_shadow` LIKE `accounting_voucher_24`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_24_shadow` LIKE `account_balance_snapshot_24`;
CREATE TABLE IF NOT EXISTS `day_cut_control_24_shadow` LIKE `day_cut_control_24`;
CREATE TABLE IF NOT EXISTS `async_task_24_shadow` LIKE `async_task_24`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_24_shadow` LIKE `tcc_transaction_24`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_24_shadow` LIKE `freeze_compensate_outbox_24`;
CREATE TABLE IF NOT EXISTS `distributed_lock_24_shadow` LIKE `distributed_lock_24`;
CREATE TABLE IF NOT EXISTS `merchant_info_24_shadow` LIKE `merchant_info_24`;
CREATE TABLE IF NOT EXISTS `transaction_order_24_shadow` LIKE `transaction_order_24`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_24_shadow` LIKE `transaction_order_extra_24`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_24_shadow` LIKE `account_balance_buffer_24`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_24_shadow` LIKE `tcc_coordinator_24`;
CREATE TABLE IF NOT EXISTS `batch_order_24_shadow` LIKE `batch_order_24`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_24_shadow` LIKE `settlement_outbox_24`;

CREATE TABLE IF NOT EXISTS `account_25_shadow` LIKE `account_25`;
CREATE TABLE IF NOT EXISTS `account_transaction_25_shadow` LIKE `account_transaction_25`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_25_shadow` LIKE `accounting_voucher_25`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_25_shadow` LIKE `account_balance_snapshot_25`;
CREATE TABLE IF NOT EXISTS `day_cut_control_25_shadow` LIKE `day_cut_control_25`;
CREATE TABLE IF NOT EXISTS `async_task_25_shadow` LIKE `async_task_25`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_25_shadow` LIKE `tcc_transaction_25`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_25_shadow` LIKE `freeze_compensate_outbox_25`;
CREATE TABLE IF NOT EXISTS `distributed_lock_25_shadow` LIKE `distributed_lock_25`;
CREATE TABLE IF NOT EXISTS `merchant_info_25_shadow` LIKE `merchant_info_25`;
CREATE TABLE IF NOT EXISTS `transaction_order_25_shadow` LIKE `transaction_order_25`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_25_shadow` LIKE `transaction_order_extra_25`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_25_shadow` LIKE `account_balance_buffer_25`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_25_shadow` LIKE `tcc_coordinator_25`;
CREATE TABLE IF NOT EXISTS `batch_order_25_shadow` LIKE `batch_order_25`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_25_shadow` LIKE `settlement_outbox_25`;

CREATE TABLE IF NOT EXISTS `account_26_shadow` LIKE `account_26`;
CREATE TABLE IF NOT EXISTS `account_transaction_26_shadow` LIKE `account_transaction_26`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_26_shadow` LIKE `accounting_voucher_26`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_26_shadow` LIKE `account_balance_snapshot_26`;
CREATE TABLE IF NOT EXISTS `day_cut_control_26_shadow` LIKE `day_cut_control_26`;
CREATE TABLE IF NOT EXISTS `async_task_26_shadow` LIKE `async_task_26`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_26_shadow` LIKE `tcc_transaction_26`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_26_shadow` LIKE `freeze_compensate_outbox_26`;
CREATE TABLE IF NOT EXISTS `distributed_lock_26_shadow` LIKE `distributed_lock_26`;
CREATE TABLE IF NOT EXISTS `merchant_info_26_shadow` LIKE `merchant_info_26`;
CREATE TABLE IF NOT EXISTS `transaction_order_26_shadow` LIKE `transaction_order_26`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_26_shadow` LIKE `transaction_order_extra_26`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_26_shadow` LIKE `account_balance_buffer_26`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_26_shadow` LIKE `tcc_coordinator_26`;
CREATE TABLE IF NOT EXISTS `batch_order_26_shadow` LIKE `batch_order_26`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_26_shadow` LIKE `settlement_outbox_26`;

CREATE TABLE IF NOT EXISTS `account_27_shadow` LIKE `account_27`;
CREATE TABLE IF NOT EXISTS `account_transaction_27_shadow` LIKE `account_transaction_27`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_27_shadow` LIKE `accounting_voucher_27`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_27_shadow` LIKE `account_balance_snapshot_27`;
CREATE TABLE IF NOT EXISTS `day_cut_control_27_shadow` LIKE `day_cut_control_27`;
CREATE TABLE IF NOT EXISTS `async_task_27_shadow` LIKE `async_task_27`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_27_shadow` LIKE `tcc_transaction_27`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_27_shadow` LIKE `freeze_compensate_outbox_27`;
CREATE TABLE IF NOT EXISTS `distributed_lock_27_shadow` LIKE `distributed_lock_27`;
CREATE TABLE IF NOT EXISTS `merchant_info_27_shadow` LIKE `merchant_info_27`;
CREATE TABLE IF NOT EXISTS `transaction_order_27_shadow` LIKE `transaction_order_27`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_27_shadow` LIKE `transaction_order_extra_27`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_27_shadow` LIKE `account_balance_buffer_27`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_27_shadow` LIKE `tcc_coordinator_27`;
CREATE TABLE IF NOT EXISTS `batch_order_27_shadow` LIKE `batch_order_27`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_27_shadow` LIKE `settlement_outbox_27`;

CREATE TABLE IF NOT EXISTS `account_28_shadow` LIKE `account_28`;
CREATE TABLE IF NOT EXISTS `account_transaction_28_shadow` LIKE `account_transaction_28`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_28_shadow` LIKE `accounting_voucher_28`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_28_shadow` LIKE `account_balance_snapshot_28`;
CREATE TABLE IF NOT EXISTS `day_cut_control_28_shadow` LIKE `day_cut_control_28`;
CREATE TABLE IF NOT EXISTS `async_task_28_shadow` LIKE `async_task_28`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_28_shadow` LIKE `tcc_transaction_28`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_28_shadow` LIKE `freeze_compensate_outbox_28`;
CREATE TABLE IF NOT EXISTS `distributed_lock_28_shadow` LIKE `distributed_lock_28`;
CREATE TABLE IF NOT EXISTS `merchant_info_28_shadow` LIKE `merchant_info_28`;
CREATE TABLE IF NOT EXISTS `transaction_order_28_shadow` LIKE `transaction_order_28`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_28_shadow` LIKE `transaction_order_extra_28`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_28_shadow` LIKE `account_balance_buffer_28`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_28_shadow` LIKE `tcc_coordinator_28`;
CREATE TABLE IF NOT EXISTS `batch_order_28_shadow` LIKE `batch_order_28`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_28_shadow` LIKE `settlement_outbox_28`;

CREATE TABLE IF NOT EXISTS `account_29_shadow` LIKE `account_29`;
CREATE TABLE IF NOT EXISTS `account_transaction_29_shadow` LIKE `account_transaction_29`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_29_shadow` LIKE `accounting_voucher_29`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_29_shadow` LIKE `account_balance_snapshot_29`;
CREATE TABLE IF NOT EXISTS `day_cut_control_29_shadow` LIKE `day_cut_control_29`;
CREATE TABLE IF NOT EXISTS `async_task_29_shadow` LIKE `async_task_29`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_29_shadow` LIKE `tcc_transaction_29`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_29_shadow` LIKE `freeze_compensate_outbox_29`;
CREATE TABLE IF NOT EXISTS `distributed_lock_29_shadow` LIKE `distributed_lock_29`;
CREATE TABLE IF NOT EXISTS `merchant_info_29_shadow` LIKE `merchant_info_29`;
CREATE TABLE IF NOT EXISTS `transaction_order_29_shadow` LIKE `transaction_order_29`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_29_shadow` LIKE `transaction_order_extra_29`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_29_shadow` LIKE `account_balance_buffer_29`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_29_shadow` LIKE `tcc_coordinator_29`;
CREATE TABLE IF NOT EXISTS `batch_order_29_shadow` LIKE `batch_order_29`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_29_shadow` LIKE `settlement_outbox_29`;



CREATE TABLE IF NOT EXISTS `tx_account_anchor_20_shadow` LIKE `tx_account_anchor_20`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_20_shadow` LIKE `flow_anchor_route_20`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_21_shadow` LIKE `tx_account_anchor_21`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_21_shadow` LIKE `flow_anchor_route_21`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_22_shadow` LIKE `tx_account_anchor_22`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_22_shadow` LIKE `flow_anchor_route_22`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_23_shadow` LIKE `tx_account_anchor_23`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_23_shadow` LIKE `flow_anchor_route_23`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_24_shadow` LIKE `tx_account_anchor_24`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_24_shadow` LIKE `flow_anchor_route_24`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_25_shadow` LIKE `tx_account_anchor_25`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_25_shadow` LIKE `flow_anchor_route_25`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_26_shadow` LIKE `tx_account_anchor_26`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_26_shadow` LIKE `flow_anchor_route_26`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_27_shadow` LIKE `tx_account_anchor_27`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_27_shadow` LIKE `flow_anchor_route_27`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_28_shadow` LIKE `tx_account_anchor_28`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_28_shadow` LIKE `flow_anchor_route_28`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_29_shadow` LIKE `tx_account_anchor_29`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_29_shadow` LIKE `flow_anchor_route_29`;

-- ==== user-merchant-core _shadow ====
-- user_merchant_db_2 的影子表（压测 / shadow 流量）
-- 依赖：2_init.sql 已经导入完成（CREATE TABLE LIKE 需要主表存在）。
SET NAMES utf8mb4;
USE `user_merchant_db_2`;

CREATE TABLE IF NOT EXISTS `users_20_shadow` LIKE `users_20`;
CREATE TABLE IF NOT EXISTS `user_profiles_20_shadow` LIKE `user_profiles_20`;
CREATE TABLE IF NOT EXISTS `user_auths_20_shadow` LIKE `user_auths_20`;
CREATE TABLE IF NOT EXISTS `login_logs_20_shadow` LIKE `login_logs_20`;
CREATE TABLE IF NOT EXISTS `user_sessions_20_shadow` LIKE `user_sessions_20`;
CREATE TABLE IF NOT EXISTS `user_roles_20_shadow` LIKE `user_roles_20`;
CREATE TABLE IF NOT EXISTS `user_accounts_20_shadow` LIKE `user_accounts_20`;
CREATE TABLE IF NOT EXISTS `user_settings_20_shadow` LIKE `user_settings_20`;
CREATE TABLE IF NOT EXISTS `user_card_20_shadow` LIKE `user_card_20`;
CREATE TABLE IF NOT EXISTS `merchants_20_shadow` LIKE `merchants_20`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_20_shadow` LIKE `merchant_kyc_document_20`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_20_shadow` LIKE `merchant_channel_secret_20`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_20_shadow` LIKE `admin_audit_log_20`;

CREATE TABLE IF NOT EXISTS `users_21_shadow` LIKE `users_21`;
CREATE TABLE IF NOT EXISTS `user_profiles_21_shadow` LIKE `user_profiles_21`;
CREATE TABLE IF NOT EXISTS `user_auths_21_shadow` LIKE `user_auths_21`;
CREATE TABLE IF NOT EXISTS `login_logs_21_shadow` LIKE `login_logs_21`;
CREATE TABLE IF NOT EXISTS `user_sessions_21_shadow` LIKE `user_sessions_21`;
CREATE TABLE IF NOT EXISTS `user_roles_21_shadow` LIKE `user_roles_21`;
CREATE TABLE IF NOT EXISTS `user_accounts_21_shadow` LIKE `user_accounts_21`;
CREATE TABLE IF NOT EXISTS `user_settings_21_shadow` LIKE `user_settings_21`;
CREATE TABLE IF NOT EXISTS `user_card_21_shadow` LIKE `user_card_21`;
CREATE TABLE IF NOT EXISTS `merchants_21_shadow` LIKE `merchants_21`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_21_shadow` LIKE `merchant_kyc_document_21`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_21_shadow` LIKE `merchant_channel_secret_21`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_21_shadow` LIKE `admin_audit_log_21`;

CREATE TABLE IF NOT EXISTS `users_22_shadow` LIKE `users_22`;
CREATE TABLE IF NOT EXISTS `user_profiles_22_shadow` LIKE `user_profiles_22`;
CREATE TABLE IF NOT EXISTS `user_auths_22_shadow` LIKE `user_auths_22`;
CREATE TABLE IF NOT EXISTS `login_logs_22_shadow` LIKE `login_logs_22`;
CREATE TABLE IF NOT EXISTS `user_sessions_22_shadow` LIKE `user_sessions_22`;
CREATE TABLE IF NOT EXISTS `user_roles_22_shadow` LIKE `user_roles_22`;
CREATE TABLE IF NOT EXISTS `user_accounts_22_shadow` LIKE `user_accounts_22`;
CREATE TABLE IF NOT EXISTS `user_settings_22_shadow` LIKE `user_settings_22`;
CREATE TABLE IF NOT EXISTS `user_card_22_shadow` LIKE `user_card_22`;
CREATE TABLE IF NOT EXISTS `merchants_22_shadow` LIKE `merchants_22`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_22_shadow` LIKE `merchant_kyc_document_22`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_22_shadow` LIKE `merchant_channel_secret_22`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_22_shadow` LIKE `admin_audit_log_22`;

CREATE TABLE IF NOT EXISTS `users_23_shadow` LIKE `users_23`;
CREATE TABLE IF NOT EXISTS `user_profiles_23_shadow` LIKE `user_profiles_23`;
CREATE TABLE IF NOT EXISTS `user_auths_23_shadow` LIKE `user_auths_23`;
CREATE TABLE IF NOT EXISTS `login_logs_23_shadow` LIKE `login_logs_23`;
CREATE TABLE IF NOT EXISTS `user_sessions_23_shadow` LIKE `user_sessions_23`;
CREATE TABLE IF NOT EXISTS `user_roles_23_shadow` LIKE `user_roles_23`;
CREATE TABLE IF NOT EXISTS `user_accounts_23_shadow` LIKE `user_accounts_23`;
CREATE TABLE IF NOT EXISTS `user_settings_23_shadow` LIKE `user_settings_23`;
CREATE TABLE IF NOT EXISTS `user_card_23_shadow` LIKE `user_card_23`;
CREATE TABLE IF NOT EXISTS `merchants_23_shadow` LIKE `merchants_23`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_23_shadow` LIKE `merchant_kyc_document_23`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_23_shadow` LIKE `merchant_channel_secret_23`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_23_shadow` LIKE `admin_audit_log_23`;

CREATE TABLE IF NOT EXISTS `users_24_shadow` LIKE `users_24`;
CREATE TABLE IF NOT EXISTS `user_profiles_24_shadow` LIKE `user_profiles_24`;
CREATE TABLE IF NOT EXISTS `user_auths_24_shadow` LIKE `user_auths_24`;
CREATE TABLE IF NOT EXISTS `login_logs_24_shadow` LIKE `login_logs_24`;
CREATE TABLE IF NOT EXISTS `user_sessions_24_shadow` LIKE `user_sessions_24`;
CREATE TABLE IF NOT EXISTS `user_roles_24_shadow` LIKE `user_roles_24`;
CREATE TABLE IF NOT EXISTS `user_accounts_24_shadow` LIKE `user_accounts_24`;
CREATE TABLE IF NOT EXISTS `user_settings_24_shadow` LIKE `user_settings_24`;
CREATE TABLE IF NOT EXISTS `user_card_24_shadow` LIKE `user_card_24`;
CREATE TABLE IF NOT EXISTS `merchants_24_shadow` LIKE `merchants_24`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_24_shadow` LIKE `merchant_kyc_document_24`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_24_shadow` LIKE `merchant_channel_secret_24`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_24_shadow` LIKE `admin_audit_log_24`;

CREATE TABLE IF NOT EXISTS `users_25_shadow` LIKE `users_25`;
CREATE TABLE IF NOT EXISTS `user_profiles_25_shadow` LIKE `user_profiles_25`;
CREATE TABLE IF NOT EXISTS `user_auths_25_shadow` LIKE `user_auths_25`;
CREATE TABLE IF NOT EXISTS `login_logs_25_shadow` LIKE `login_logs_25`;
CREATE TABLE IF NOT EXISTS `user_sessions_25_shadow` LIKE `user_sessions_25`;
CREATE TABLE IF NOT EXISTS `user_roles_25_shadow` LIKE `user_roles_25`;
CREATE TABLE IF NOT EXISTS `user_accounts_25_shadow` LIKE `user_accounts_25`;
CREATE TABLE IF NOT EXISTS `user_settings_25_shadow` LIKE `user_settings_25`;
CREATE TABLE IF NOT EXISTS `user_card_25_shadow` LIKE `user_card_25`;
CREATE TABLE IF NOT EXISTS `merchants_25_shadow` LIKE `merchants_25`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_25_shadow` LIKE `merchant_kyc_document_25`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_25_shadow` LIKE `merchant_channel_secret_25`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_25_shadow` LIKE `admin_audit_log_25`;

CREATE TABLE IF NOT EXISTS `users_26_shadow` LIKE `users_26`;
CREATE TABLE IF NOT EXISTS `user_profiles_26_shadow` LIKE `user_profiles_26`;
CREATE TABLE IF NOT EXISTS `user_auths_26_shadow` LIKE `user_auths_26`;
CREATE TABLE IF NOT EXISTS `login_logs_26_shadow` LIKE `login_logs_26`;
CREATE TABLE IF NOT EXISTS `user_sessions_26_shadow` LIKE `user_sessions_26`;
CREATE TABLE IF NOT EXISTS `user_roles_26_shadow` LIKE `user_roles_26`;
CREATE TABLE IF NOT EXISTS `user_accounts_26_shadow` LIKE `user_accounts_26`;
CREATE TABLE IF NOT EXISTS `user_settings_26_shadow` LIKE `user_settings_26`;
CREATE TABLE IF NOT EXISTS `user_card_26_shadow` LIKE `user_card_26`;
CREATE TABLE IF NOT EXISTS `merchants_26_shadow` LIKE `merchants_26`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_26_shadow` LIKE `merchant_kyc_document_26`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_26_shadow` LIKE `merchant_channel_secret_26`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_26_shadow` LIKE `admin_audit_log_26`;

CREATE TABLE IF NOT EXISTS `users_27_shadow` LIKE `users_27`;
CREATE TABLE IF NOT EXISTS `user_profiles_27_shadow` LIKE `user_profiles_27`;
CREATE TABLE IF NOT EXISTS `user_auths_27_shadow` LIKE `user_auths_27`;
CREATE TABLE IF NOT EXISTS `login_logs_27_shadow` LIKE `login_logs_27`;
CREATE TABLE IF NOT EXISTS `user_sessions_27_shadow` LIKE `user_sessions_27`;
CREATE TABLE IF NOT EXISTS `user_roles_27_shadow` LIKE `user_roles_27`;
CREATE TABLE IF NOT EXISTS `user_accounts_27_shadow` LIKE `user_accounts_27`;
CREATE TABLE IF NOT EXISTS `user_settings_27_shadow` LIKE `user_settings_27`;
CREATE TABLE IF NOT EXISTS `user_card_27_shadow` LIKE `user_card_27`;
CREATE TABLE IF NOT EXISTS `merchants_27_shadow` LIKE `merchants_27`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_27_shadow` LIKE `merchant_kyc_document_27`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_27_shadow` LIKE `merchant_channel_secret_27`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_27_shadow` LIKE `admin_audit_log_27`;

CREATE TABLE IF NOT EXISTS `users_28_shadow` LIKE `users_28`;
CREATE TABLE IF NOT EXISTS `user_profiles_28_shadow` LIKE `user_profiles_28`;
CREATE TABLE IF NOT EXISTS `user_auths_28_shadow` LIKE `user_auths_28`;
CREATE TABLE IF NOT EXISTS `login_logs_28_shadow` LIKE `login_logs_28`;
CREATE TABLE IF NOT EXISTS `user_sessions_28_shadow` LIKE `user_sessions_28`;
CREATE TABLE IF NOT EXISTS `user_roles_28_shadow` LIKE `user_roles_28`;
CREATE TABLE IF NOT EXISTS `user_accounts_28_shadow` LIKE `user_accounts_28`;
CREATE TABLE IF NOT EXISTS `user_settings_28_shadow` LIKE `user_settings_28`;
CREATE TABLE IF NOT EXISTS `user_card_28_shadow` LIKE `user_card_28`;
CREATE TABLE IF NOT EXISTS `merchants_28_shadow` LIKE `merchants_28`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_28_shadow` LIKE `merchant_kyc_document_28`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_28_shadow` LIKE `merchant_channel_secret_28`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_28_shadow` LIKE `admin_audit_log_28`;

CREATE TABLE IF NOT EXISTS `users_29_shadow` LIKE `users_29`;
CREATE TABLE IF NOT EXISTS `user_profiles_29_shadow` LIKE `user_profiles_29`;
CREATE TABLE IF NOT EXISTS `user_auths_29_shadow` LIKE `user_auths_29`;
CREATE TABLE IF NOT EXISTS `login_logs_29_shadow` LIKE `login_logs_29`;
CREATE TABLE IF NOT EXISTS `user_sessions_29_shadow` LIKE `user_sessions_29`;
CREATE TABLE IF NOT EXISTS `user_roles_29_shadow` LIKE `user_roles_29`;
CREATE TABLE IF NOT EXISTS `user_accounts_29_shadow` LIKE `user_accounts_29`;
CREATE TABLE IF NOT EXISTS `user_settings_29_shadow` LIKE `user_settings_29`;
CREATE TABLE IF NOT EXISTS `user_card_29_shadow` LIKE `user_card_29`;
CREATE TABLE IF NOT EXISTS `merchants_29_shadow` LIKE `merchants_29`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_29_shadow` LIKE `merchant_kyc_document_29`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_29_shadow` LIKE `merchant_channel_secret_29`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_29_shadow` LIKE `admin_audit_log_29`;


-- ==== card-center ====
SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `card_center_db_2` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_center_db_2`;

-- card-center 分片表模板。2 = 0..9，20 = 00..99（globalTblIdx）。
-- generate.sh 把所有 2 / 20 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_2`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_20` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_20` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- ─── audit_log：每次 Tokenize / Detokenize / DeleteCard 写一行 ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈（meta 单库 audit insert ~30K/s 上限，
-- 全平台 audit 量峰值会达到这个）。
--
-- 路由 key：user_id（同 card_stored_token），保证同 user 的所有 audit 都在同 shard，
-- 取证 / 客服 / 合规审查时一次 SELECT 拿全。user_id 为空（系统级操作）走 trace_id hash。
--
-- 链式签名变 per-shard：每个 (db_idx, table_idx) 维护独立 prev_hash 链。
-- 跨 shard 完整性靠 Kafka append-only canonical store + 数据湖归档保证。
-- verify CLI 工具按 (db_idx, table_idx) 走 100 条独立链。
--
-- 7 年留存（PCI-DSS 10.7）。冷热分离：30d 后归档到数据湖，DB 只留近 30d 热查询。
CREATE TABLE IF NOT EXISTS `audit_log_20` (
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
    `prev_hash`    CHAR(64)     DEFAULT NULL,         -- 前一行 row_hash（per-shard 链 head）
    `row_hash`     CHAR(64)     NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_op_created`  (`op`, `created_at`),
    KEY `idx_caller`      (`caller`),
    KEY `idx_user`        (`user_id`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_token_hash`  (`token_hash`),
    KEY `idx_trace`       (`trace_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 20 (PCI 10.7 7y retention)';

-- card-center 分片表模板。2 = 0..9，21 = 00..99（globalTblIdx）。
-- generate.sh 把所有 2 / 21 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_2`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_21` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_21` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- ─── audit_log：每次 Tokenize / Detokenize / DeleteCard 写一行 ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈（meta 单库 audit insert ~30K/s 上限，
-- 全平台 audit 量峰值会达到这个）。
--
-- 路由 key：user_id（同 card_stored_token），保证同 user 的所有 audit 都在同 shard，
-- 取证 / 客服 / 合规审查时一次 SELECT 拿全。user_id 为空（系统级操作）走 trace_id hash。
--
-- 链式签名变 per-shard：每个 (db_idx, table_idx) 维护独立 prev_hash 链。
-- 跨 shard 完整性靠 Kafka append-only canonical store + 数据湖归档保证。
-- verify CLI 工具按 (db_idx, table_idx) 走 100 条独立链。
--
-- 7 年留存（PCI-DSS 10.7）。冷热分离：30d 后归档到数据湖，DB 只留近 30d 热查询。
CREATE TABLE IF NOT EXISTS `audit_log_21` (
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
    `prev_hash`    CHAR(64)     DEFAULT NULL,         -- 前一行 row_hash（per-shard 链 head）
    `row_hash`     CHAR(64)     NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_op_created`  (`op`, `created_at`),
    KEY `idx_caller`      (`caller`),
    KEY `idx_user`        (`user_id`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_token_hash`  (`token_hash`),
    KEY `idx_trace`       (`trace_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 21 (PCI 10.7 7y retention)';

-- card-center 分片表模板。2 = 0..9，22 = 00..99（globalTblIdx）。
-- generate.sh 把所有 2 / 22 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_2`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_22` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_22` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- ─── audit_log：每次 Tokenize / Detokenize / DeleteCard 写一行 ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈（meta 单库 audit insert ~30K/s 上限，
-- 全平台 audit 量峰值会达到这个）。
--
-- 路由 key：user_id（同 card_stored_token），保证同 user 的所有 audit 都在同 shard，
-- 取证 / 客服 / 合规审查时一次 SELECT 拿全。user_id 为空（系统级操作）走 trace_id hash。
--
-- 链式签名变 per-shard：每个 (db_idx, table_idx) 维护独立 prev_hash 链。
-- 跨 shard 完整性靠 Kafka append-only canonical store + 数据湖归档保证。
-- verify CLI 工具按 (db_idx, table_idx) 走 100 条独立链。
--
-- 7 年留存（PCI-DSS 10.7）。冷热分离：30d 后归档到数据湖，DB 只留近 30d 热查询。
CREATE TABLE IF NOT EXISTS `audit_log_22` (
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
    `prev_hash`    CHAR(64)     DEFAULT NULL,         -- 前一行 row_hash（per-shard 链 head）
    `row_hash`     CHAR(64)     NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_op_created`  (`op`, `created_at`),
    KEY `idx_caller`      (`caller`),
    KEY `idx_user`        (`user_id`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_token_hash`  (`token_hash`),
    KEY `idx_trace`       (`trace_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 22 (PCI 10.7 7y retention)';

-- card-center 分片表模板。2 = 0..9，23 = 00..99（globalTblIdx）。
-- generate.sh 把所有 2 / 23 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_2`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_23` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_23` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- ─── audit_log：每次 Tokenize / Detokenize / DeleteCard 写一行 ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈（meta 单库 audit insert ~30K/s 上限，
-- 全平台 audit 量峰值会达到这个）。
--
-- 路由 key：user_id（同 card_stored_token），保证同 user 的所有 audit 都在同 shard，
-- 取证 / 客服 / 合规审查时一次 SELECT 拿全。user_id 为空（系统级操作）走 trace_id hash。
--
-- 链式签名变 per-shard：每个 (db_idx, table_idx) 维护独立 prev_hash 链。
-- 跨 shard 完整性靠 Kafka append-only canonical store + 数据湖归档保证。
-- verify CLI 工具按 (db_idx, table_idx) 走 100 条独立链。
--
-- 7 年留存（PCI-DSS 10.7）。冷热分离：30d 后归档到数据湖，DB 只留近 30d 热查询。
CREATE TABLE IF NOT EXISTS `audit_log_23` (
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
    `prev_hash`    CHAR(64)     DEFAULT NULL,         -- 前一行 row_hash（per-shard 链 head）
    `row_hash`     CHAR(64)     NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_op_created`  (`op`, `created_at`),
    KEY `idx_caller`      (`caller`),
    KEY `idx_user`        (`user_id`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_token_hash`  (`token_hash`),
    KEY `idx_trace`       (`trace_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 23 (PCI 10.7 7y retention)';

-- card-center 分片表模板。2 = 0..9，24 = 00..99（globalTblIdx）。
-- generate.sh 把所有 2 / 24 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_2`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_24` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_24` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- ─── audit_log：每次 Tokenize / Detokenize / DeleteCard 写一行 ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈（meta 单库 audit insert ~30K/s 上限，
-- 全平台 audit 量峰值会达到这个）。
--
-- 路由 key：user_id（同 card_stored_token），保证同 user 的所有 audit 都在同 shard，
-- 取证 / 客服 / 合规审查时一次 SELECT 拿全。user_id 为空（系统级操作）走 trace_id hash。
--
-- 链式签名变 per-shard：每个 (db_idx, table_idx) 维护独立 prev_hash 链。
-- 跨 shard 完整性靠 Kafka append-only canonical store + 数据湖归档保证。
-- verify CLI 工具按 (db_idx, table_idx) 走 100 条独立链。
--
-- 7 年留存（PCI-DSS 10.7）。冷热分离：30d 后归档到数据湖，DB 只留近 30d 热查询。
CREATE TABLE IF NOT EXISTS `audit_log_24` (
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
    `prev_hash`    CHAR(64)     DEFAULT NULL,         -- 前一行 row_hash（per-shard 链 head）
    `row_hash`     CHAR(64)     NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_op_created`  (`op`, `created_at`),
    KEY `idx_caller`      (`caller`),
    KEY `idx_user`        (`user_id`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_token_hash`  (`token_hash`),
    KEY `idx_trace`       (`trace_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 24 (PCI 10.7 7y retention)';

-- card-center 分片表模板。2 = 0..9，25 = 00..99（globalTblIdx）。
-- generate.sh 把所有 2 / 25 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_2`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_25` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_25` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- ─── audit_log：每次 Tokenize / Detokenize / DeleteCard 写一行 ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈（meta 单库 audit insert ~30K/s 上限，
-- 全平台 audit 量峰值会达到这个）。
--
-- 路由 key：user_id（同 card_stored_token），保证同 user 的所有 audit 都在同 shard，
-- 取证 / 客服 / 合规审查时一次 SELECT 拿全。user_id 为空（系统级操作）走 trace_id hash。
--
-- 链式签名变 per-shard：每个 (db_idx, table_idx) 维护独立 prev_hash 链。
-- 跨 shard 完整性靠 Kafka append-only canonical store + 数据湖归档保证。
-- verify CLI 工具按 (db_idx, table_idx) 走 100 条独立链。
--
-- 7 年留存（PCI-DSS 10.7）。冷热分离：30d 后归档到数据湖，DB 只留近 30d 热查询。
CREATE TABLE IF NOT EXISTS `audit_log_25` (
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
    `prev_hash`    CHAR(64)     DEFAULT NULL,         -- 前一行 row_hash（per-shard 链 head）
    `row_hash`     CHAR(64)     NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_op_created`  (`op`, `created_at`),
    KEY `idx_caller`      (`caller`),
    KEY `idx_user`        (`user_id`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_token_hash`  (`token_hash`),
    KEY `idx_trace`       (`trace_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 25 (PCI 10.7 7y retention)';

-- card-center 分片表模板。2 = 0..9，26 = 00..99（globalTblIdx）。
-- generate.sh 把所有 2 / 26 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_2`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_26` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_26` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- ─── audit_log：每次 Tokenize / Detokenize / DeleteCard 写一行 ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈（meta 单库 audit insert ~30K/s 上限，
-- 全平台 audit 量峰值会达到这个）。
--
-- 路由 key：user_id（同 card_stored_token），保证同 user 的所有 audit 都在同 shard，
-- 取证 / 客服 / 合规审查时一次 SELECT 拿全。user_id 为空（系统级操作）走 trace_id hash。
--
-- 链式签名变 per-shard：每个 (db_idx, table_idx) 维护独立 prev_hash 链。
-- 跨 shard 完整性靠 Kafka append-only canonical store + 数据湖归档保证。
-- verify CLI 工具按 (db_idx, table_idx) 走 100 条独立链。
--
-- 7 年留存（PCI-DSS 10.7）。冷热分离：30d 后归档到数据湖，DB 只留近 30d 热查询。
CREATE TABLE IF NOT EXISTS `audit_log_26` (
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
    `prev_hash`    CHAR(64)     DEFAULT NULL,         -- 前一行 row_hash（per-shard 链 head）
    `row_hash`     CHAR(64)     NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_op_created`  (`op`, `created_at`),
    KEY `idx_caller`      (`caller`),
    KEY `idx_user`        (`user_id`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_token_hash`  (`token_hash`),
    KEY `idx_trace`       (`trace_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 26 (PCI 10.7 7y retention)';

-- card-center 分片表模板。2 = 0..9，27 = 00..99（globalTblIdx）。
-- generate.sh 把所有 2 / 27 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_2`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_27` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_27` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- ─── audit_log：每次 Tokenize / Detokenize / DeleteCard 写一行 ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈（meta 单库 audit insert ~30K/s 上限，
-- 全平台 audit 量峰值会达到这个）。
--
-- 路由 key：user_id（同 card_stored_token），保证同 user 的所有 audit 都在同 shard，
-- 取证 / 客服 / 合规审查时一次 SELECT 拿全。user_id 为空（系统级操作）走 trace_id hash。
--
-- 链式签名变 per-shard：每个 (db_idx, table_idx) 维护独立 prev_hash 链。
-- 跨 shard 完整性靠 Kafka append-only canonical store + 数据湖归档保证。
-- verify CLI 工具按 (db_idx, table_idx) 走 100 条独立链。
--
-- 7 年留存（PCI-DSS 10.7）。冷热分离：30d 后归档到数据湖，DB 只留近 30d 热查询。
CREATE TABLE IF NOT EXISTS `audit_log_27` (
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
    `prev_hash`    CHAR(64)     DEFAULT NULL,         -- 前一行 row_hash（per-shard 链 head）
    `row_hash`     CHAR(64)     NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_op_created`  (`op`, `created_at`),
    KEY `idx_caller`      (`caller`),
    KEY `idx_user`        (`user_id`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_token_hash`  (`token_hash`),
    KEY `idx_trace`       (`trace_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 27 (PCI 10.7 7y retention)';

-- card-center 分片表模板。2 = 0..9，28 = 00..99（globalTblIdx）。
-- generate.sh 把所有 2 / 28 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_2`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_28` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_28` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- ─── audit_log：每次 Tokenize / Detokenize / DeleteCard 写一行 ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈（meta 单库 audit insert ~30K/s 上限，
-- 全平台 audit 量峰值会达到这个）。
--
-- 路由 key：user_id（同 card_stored_token），保证同 user 的所有 audit 都在同 shard，
-- 取证 / 客服 / 合规审查时一次 SELECT 拿全。user_id 为空（系统级操作）走 trace_id hash。
--
-- 链式签名变 per-shard：每个 (db_idx, table_idx) 维护独立 prev_hash 链。
-- 跨 shard 完整性靠 Kafka append-only canonical store + 数据湖归档保证。
-- verify CLI 工具按 (db_idx, table_idx) 走 100 条独立链。
--
-- 7 年留存（PCI-DSS 10.7）。冷热分离：30d 后归档到数据湖，DB 只留近 30d 热查询。
CREATE TABLE IF NOT EXISTS `audit_log_28` (
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
    `prev_hash`    CHAR(64)     DEFAULT NULL,         -- 前一行 row_hash（per-shard 链 head）
    `row_hash`     CHAR(64)     NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_op_created`  (`op`, `created_at`),
    KEY `idx_caller`      (`caller`),
    KEY `idx_user`        (`user_id`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_token_hash`  (`token_hash`),
    KEY `idx_trace`       (`trace_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 28 (PCI 10.7 7y retention)';

-- card-center 分片表模板。2 = 0..9，29 = 00..99（globalTblIdx）。
-- generate.sh 把所有 2 / 29 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_2`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_29` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_29` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- ─── audit_log：每次 Tokenize / Detokenize / DeleteCard 写一行 ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈（meta 单库 audit insert ~30K/s 上限，
-- 全平台 audit 量峰值会达到这个）。
--
-- 路由 key：user_id（同 card_stored_token），保证同 user 的所有 audit 都在同 shard，
-- 取证 / 客服 / 合规审查时一次 SELECT 拿全。user_id 为空（系统级操作）走 trace_id hash。
--
-- 链式签名变 per-shard：每个 (db_idx, table_idx) 维护独立 prev_hash 链。
-- 跨 shard 完整性靠 Kafka append-only canonical store + 数据湖归档保证。
-- verify CLI 工具按 (db_idx, table_idx) 走 100 条独立链。
--
-- 7 年留存（PCI-DSS 10.7）。冷热分离：30d 后归档到数据湖，DB 只留近 30d 热查询。
CREATE TABLE IF NOT EXISTS `audit_log_29` (
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
    `prev_hash`    CHAR(64)     DEFAULT NULL,         -- 前一行 row_hash（per-shard 链 head）
    `row_hash`     CHAR(64)     NOT NULL,
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_op_created`  (`op`, `created_at`),
    KEY `idx_caller`      (`caller`),
    KEY `idx_user`        (`user_id`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_token_hash`  (`token_hash`),
    KEY `idx_trace`       (`trace_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 29 (PCI 10.7 7y retention)';


-- ==== card-payment ====
SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `card_payment_db_2` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_payment_db_2`;

-- card-payment 分片表模板。2 = 0..9，20 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_2`;

CREATE TABLE IF NOT EXISTS `card_transaction_20` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。2 = 0..9，21 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_2`;

CREATE TABLE IF NOT EXISTS `card_transaction_21` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。2 = 0..9，22 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_2`;

CREATE TABLE IF NOT EXISTS `card_transaction_22` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。2 = 0..9，23 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_2`;

CREATE TABLE IF NOT EXISTS `card_transaction_23` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。2 = 0..9，24 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_2`;

CREATE TABLE IF NOT EXISTS `card_transaction_24` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。2 = 0..9，25 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_2`;

CREATE TABLE IF NOT EXISTS `card_transaction_25` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。2 = 0..9，26 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_2`;

CREATE TABLE IF NOT EXISTS `card_transaction_26` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。2 = 0..9，27 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_2`;

CREATE TABLE IF NOT EXISTS `card_transaction_27` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。2 = 0..9，28 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_2`;

CREATE TABLE IF NOT EXISTS `card_transaction_28` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。2 = 0..9，29 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_2`;

CREATE TABLE IF NOT EXISTS `card_transaction_29` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';


-- ==== card-center _shadow ====
-- card_center_db_2 shadow 表（压测）
USE `card_center_db_2`;

CREATE TABLE IF NOT EXISTS `card_stored_token_20_shadow` LIKE `card_stored_token_20`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_20_shadow` LIKE `card_payment_token_used_20`;
CREATE TABLE IF NOT EXISTS `audit_log_20_shadow` LIKE `audit_log_20`;

CREATE TABLE IF NOT EXISTS `card_stored_token_21_shadow` LIKE `card_stored_token_21`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_21_shadow` LIKE `card_payment_token_used_21`;
CREATE TABLE IF NOT EXISTS `audit_log_21_shadow` LIKE `audit_log_21`;

CREATE TABLE IF NOT EXISTS `card_stored_token_22_shadow` LIKE `card_stored_token_22`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_22_shadow` LIKE `card_payment_token_used_22`;
CREATE TABLE IF NOT EXISTS `audit_log_22_shadow` LIKE `audit_log_22`;

CREATE TABLE IF NOT EXISTS `card_stored_token_23_shadow` LIKE `card_stored_token_23`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_23_shadow` LIKE `card_payment_token_used_23`;
CREATE TABLE IF NOT EXISTS `audit_log_23_shadow` LIKE `audit_log_23`;

CREATE TABLE IF NOT EXISTS `card_stored_token_24_shadow` LIKE `card_stored_token_24`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_24_shadow` LIKE `card_payment_token_used_24`;
CREATE TABLE IF NOT EXISTS `audit_log_24_shadow` LIKE `audit_log_24`;

CREATE TABLE IF NOT EXISTS `card_stored_token_25_shadow` LIKE `card_stored_token_25`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_25_shadow` LIKE `card_payment_token_used_25`;
CREATE TABLE IF NOT EXISTS `audit_log_25_shadow` LIKE `audit_log_25`;

CREATE TABLE IF NOT EXISTS `card_stored_token_26_shadow` LIKE `card_stored_token_26`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_26_shadow` LIKE `card_payment_token_used_26`;
CREATE TABLE IF NOT EXISTS `audit_log_26_shadow` LIKE `audit_log_26`;

CREATE TABLE IF NOT EXISTS `card_stored_token_27_shadow` LIKE `card_stored_token_27`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_27_shadow` LIKE `card_payment_token_used_27`;
CREATE TABLE IF NOT EXISTS `audit_log_27_shadow` LIKE `audit_log_27`;

CREATE TABLE IF NOT EXISTS `card_stored_token_28_shadow` LIKE `card_stored_token_28`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_28_shadow` LIKE `card_payment_token_used_28`;
CREATE TABLE IF NOT EXISTS `audit_log_28_shadow` LIKE `audit_log_28`;

CREATE TABLE IF NOT EXISTS `card_stored_token_29_shadow` LIKE `card_stored_token_29`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_29_shadow` LIKE `card_payment_token_used_29`;
CREATE TABLE IF NOT EXISTS `audit_log_29_shadow` LIKE `audit_log_29`;


-- ==== card-payment _shadow ====
-- card_payment_db_2 shadow
USE `card_payment_db_2`;

CREATE TABLE IF NOT EXISTS `card_transaction_20_shadow` LIKE `card_transaction_20`;

CREATE TABLE IF NOT EXISTS `card_transaction_21_shadow` LIKE `card_transaction_21`;

CREATE TABLE IF NOT EXISTS `card_transaction_22_shadow` LIKE `card_transaction_22`;

CREATE TABLE IF NOT EXISTS `card_transaction_23_shadow` LIKE `card_transaction_23`;

CREATE TABLE IF NOT EXISTS `card_transaction_24_shadow` LIKE `card_transaction_24`;

CREATE TABLE IF NOT EXISTS `card_transaction_25_shadow` LIKE `card_transaction_25`;

CREATE TABLE IF NOT EXISTS `card_transaction_26_shadow` LIKE `card_transaction_26`;

CREATE TABLE IF NOT EXISTS `card_transaction_27_shadow` LIKE `card_transaction_27`;

CREATE TABLE IF NOT EXISTS `card_transaction_28_shadow` LIKE `card_transaction_28`;

CREATE TABLE IF NOT EXISTS `card_transaction_29_shadow` LIKE `card_transaction_29`;


-- ==== split-payment shardb (split_payment_db_2) ====
-- ============================================================================
-- split_payment_db_2 —— 高频事件流水分片库（DB-split Batch 7 极简版）
-- ⚠ 模板文件。由 ../gen.sh 用 sed 替换 2 + 注入 tables block 生成 N_init.sql。
--
-- 唯一表族 moneyflow_event_NN —— 按 idempotency_key / charge_id / saga_id hash 路由：
--   hash(key) % 100 → globalIdx (0..99)
--   dbIdx   = globalIdx / 10  (0..9)
--   tblIdx  = globalIdx       (00..99)
--
-- 设计原则：
--   split-payment **不再存业务账本**（transfers / fees / payouts / reversals 等都
--   在 accounting-system 已有）。它只关注"事件 / 状态机执行流水"。
--
-- 同一张表覆盖 4 种 event_type：
--   trigger          一次 TriggerEvent 触发的 RunPlan 执行记录（原 moneyflow_runs）
--   saga             SaveGraph / refund / payout 等 saga 的状态机持久化（原 moneyflow_sagas）
--   outbox           可靠事件发布的待发队列（原 event_outbox）
--   reversal_retry   退款失败的重试队列（原 reversal_retry_outbox）
--
-- 字段按"通用执行状态" + "按 type 含义的可选字段" 设计：
--   通用：id / event_type / event_id / status / retry_count / max_retry /
--          payload_json / error_msg / next_retry_at / hold_until / 时间戳
--   特定：graph_id / graph_version / charge_id / merchant_id / amount_minor /
--          currency / plan_json / voucher_no / trace_id / correlation_id /
--          current_step / steps_json
--   (event_type 不需要的字段填 NULL/0; 不浪费多少空间)
-- ============================================================================

CREATE DATABASE IF NOT EXISTS split_payment_db_2 CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

-- split_user 在 meta init.sql 里也建一遍；但 shared-db 把每个 shard SQL 灌到不同
-- mysql 实例（shared-shard-N），mysql user 是 per-instance 的不跨实例共享，所以
-- 每个 shard 自己也必须 CREATE USER。IF NOT EXISTS 幂等。
CREATE USER IF NOT EXISTS 'split_user'@'%' IDENTIFIED BY 'password';
ALTER USER 'split_user'@'%' IDENTIFIED BY 'password';

GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, DROP, REFERENCES
  ON split_payment_db_2.* TO 'split_user'@'%';
FLUSH PRIVILEGES;

USE split_payment_db_2;
-- ─── moneyflow_event (10 张主表 + 10 张 _shadow 镜像) ──────────────────────

CREATE TABLE IF NOT EXISTS moneyflow_event_20 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_20_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_21 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_21_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_22 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_22_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_23 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_23_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_24 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_24_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_25 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_25_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_26 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_26_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_27 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_27_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_28 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_28_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_29 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_29_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;



-- ==== recon_cdc binlog 用户 ====
CREATE USER IF NOT EXISTS 'recon_cdc'@'%' IDENTIFIED WITH mysql_native_password BY 'recon_cdc_pwd';
ALTER USER 'recon_cdc'@'%' IDENTIFIED WITH mysql_native_password BY 'recon_cdc_pwd';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'recon_cdc'@'%';
GRANT SELECT ON *.* TO 'recon_cdc'@'%';
FLUSH PRIVILEGES;
