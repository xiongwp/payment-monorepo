-- ┌──────────────────────────────────────────────────────────────────────┐
-- │ 共享 MySQL shard 6 —— paychan_db_6 + order_db_6 +
-- │ accounting_db_6 + user_merchant_db_6                            │
-- └──────────────────────────────────────────────────────────────────────┘

-- ==== payment-channel ====
CREATE DATABASE IF NOT EXISTS `paychan_db_6` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `paychan_db_6`;

-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 60 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_60` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_60` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_60` (
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
CREATE TABLE IF NOT EXISTS `channel_token_60` (
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
-- apply.sh 把 61 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_61` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_61` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_61` (
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
CREATE TABLE IF NOT EXISTS `channel_token_61` (
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
-- apply.sh 把 62 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_62` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_62` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_62` (
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
CREATE TABLE IF NOT EXISTS `channel_token_62` (
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
-- apply.sh 把 63 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_63` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_63` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_63` (
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
CREATE TABLE IF NOT EXISTS `channel_token_63` (
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
-- apply.sh 把 64 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_64` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_64` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_64` (
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
CREATE TABLE IF NOT EXISTS `channel_token_64` (
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
-- apply.sh 把 65 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_65` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_65` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_65` (
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
CREATE TABLE IF NOT EXISTS `channel_token_65` (
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
-- apply.sh 把 66 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_66` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_66` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_66` (
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
CREATE TABLE IF NOT EXISTS `channel_token_66` (
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
-- apply.sh 把 67 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_67` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_67` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_67` (
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
CREATE TABLE IF NOT EXISTS `channel_token_67` (
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
-- apply.sh 把 68 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_68` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_68` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_68` (
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
CREATE TABLE IF NOT EXISTS `channel_token_68` (
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
-- apply.sh 把 69 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_69` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_69` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_69` (
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
CREATE TABLE IF NOT EXISTS `channel_token_69` (
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
CREATE DATABASE IF NOT EXISTS `order_db_6` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `order_db_6`;

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：6 = 0-9 物理库；60 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_60
CREATE TABLE IF NOT EXISTS `payment_intent_60` (
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

-- 2. Charge 表 charge_60（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_60` (
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

-- 3. PayAction 表 pay_action_60（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_60` (
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

-- 4. ExceptionCase 表 exception_case_60（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_60` (
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

-- 5. NotifyLog 表 notify_log_60（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_60` (
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

-- 5. Refund 表 refund_60（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_60` (
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

-- 7. InboundWebhook 表 inbound_webhook_60
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_60` (
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

-- 8. Dispute 表 dispute_60
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_60` (
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

-- 9. DisputeEvent 表 dispute_event_60（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_60` (
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

-- 10. AccountingOutbox 表 accounting_outbox_60
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_60` (
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

-- ─── admin_audit_log_60：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_60` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 60';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：6 = 0-9 物理库；61 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_61
CREATE TABLE IF NOT EXISTS `payment_intent_61` (
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

-- 2. Charge 表 charge_61（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_61` (
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

-- 3. PayAction 表 pay_action_61（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_61` (
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

-- 4. ExceptionCase 表 exception_case_61（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_61` (
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

-- 5. NotifyLog 表 notify_log_61（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_61` (
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

-- 5. Refund 表 refund_61（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_61` (
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

-- 7. InboundWebhook 表 inbound_webhook_61
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_61` (
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

-- 8. Dispute 表 dispute_61
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_61` (
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

-- 9. DisputeEvent 表 dispute_event_61（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_61` (
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

-- 10. AccountingOutbox 表 accounting_outbox_61
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_61` (
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

-- ─── admin_audit_log_61：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_61` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 61';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：6 = 0-9 物理库；62 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_62
CREATE TABLE IF NOT EXISTS `payment_intent_62` (
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

-- 2. Charge 表 charge_62（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_62` (
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

-- 3. PayAction 表 pay_action_62（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_62` (
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

-- 4. ExceptionCase 表 exception_case_62（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_62` (
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

-- 5. NotifyLog 表 notify_log_62（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_62` (
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

-- 5. Refund 表 refund_62（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_62` (
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

-- 7. InboundWebhook 表 inbound_webhook_62
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_62` (
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

-- 8. Dispute 表 dispute_62
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_62` (
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

-- 9. DisputeEvent 表 dispute_event_62（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_62` (
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

-- 10. AccountingOutbox 表 accounting_outbox_62
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_62` (
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

-- ─── admin_audit_log_62：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_62` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 62';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：6 = 0-9 物理库；63 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_63
CREATE TABLE IF NOT EXISTS `payment_intent_63` (
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

-- 2. Charge 表 charge_63（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_63` (
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

-- 3. PayAction 表 pay_action_63（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_63` (
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

-- 4. ExceptionCase 表 exception_case_63（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_63` (
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

-- 5. NotifyLog 表 notify_log_63（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_63` (
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

-- 5. Refund 表 refund_63（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_63` (
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

-- 7. InboundWebhook 表 inbound_webhook_63
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_63` (
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

-- 8. Dispute 表 dispute_63
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_63` (
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

-- 9. DisputeEvent 表 dispute_event_63（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_63` (
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

-- 10. AccountingOutbox 表 accounting_outbox_63
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_63` (
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

-- ─── admin_audit_log_63：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_63` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 63';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：6 = 0-9 物理库；64 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_64
CREATE TABLE IF NOT EXISTS `payment_intent_64` (
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

-- 2. Charge 表 charge_64（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_64` (
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

-- 3. PayAction 表 pay_action_64（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_64` (
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

-- 4. ExceptionCase 表 exception_case_64（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_64` (
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

-- 5. NotifyLog 表 notify_log_64（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_64` (
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

-- 5. Refund 表 refund_64（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_64` (
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

-- 7. InboundWebhook 表 inbound_webhook_64
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_64` (
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

-- 8. Dispute 表 dispute_64
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_64` (
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

-- 9. DisputeEvent 表 dispute_event_64（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_64` (
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

-- 10. AccountingOutbox 表 accounting_outbox_64
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_64` (
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

-- ─── admin_audit_log_64：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_64` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 64';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：6 = 0-9 物理库；65 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_65
CREATE TABLE IF NOT EXISTS `payment_intent_65` (
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

-- 2. Charge 表 charge_65（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_65` (
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

-- 3. PayAction 表 pay_action_65（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_65` (
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

-- 4. ExceptionCase 表 exception_case_65（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_65` (
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

-- 5. NotifyLog 表 notify_log_65（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_65` (
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

-- 5. Refund 表 refund_65（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_65` (
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

-- 7. InboundWebhook 表 inbound_webhook_65
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_65` (
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

-- 8. Dispute 表 dispute_65
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_65` (
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

-- 9. DisputeEvent 表 dispute_event_65（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_65` (
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

-- 10. AccountingOutbox 表 accounting_outbox_65
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_65` (
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

-- ─── admin_audit_log_65：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_65` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 65';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：6 = 0-9 物理库；66 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_66
CREATE TABLE IF NOT EXISTS `payment_intent_66` (
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

-- 2. Charge 表 charge_66（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_66` (
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

-- 3. PayAction 表 pay_action_66（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_66` (
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

-- 4. ExceptionCase 表 exception_case_66（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_66` (
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

-- 5. NotifyLog 表 notify_log_66（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_66` (
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

-- 5. Refund 表 refund_66（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_66` (
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

-- 7. InboundWebhook 表 inbound_webhook_66
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_66` (
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

-- 8. Dispute 表 dispute_66
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_66` (
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

-- 9. DisputeEvent 表 dispute_event_66（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_66` (
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

-- 10. AccountingOutbox 表 accounting_outbox_66
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_66` (
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

-- ─── admin_audit_log_66：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_66` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 66';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：6 = 0-9 物理库；67 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_67
CREATE TABLE IF NOT EXISTS `payment_intent_67` (
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

-- 2. Charge 表 charge_67（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_67` (
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

-- 3. PayAction 表 pay_action_67（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_67` (
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

-- 4. ExceptionCase 表 exception_case_67（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_67` (
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

-- 5. NotifyLog 表 notify_log_67（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_67` (
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

-- 5. Refund 表 refund_67（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_67` (
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

-- 7. InboundWebhook 表 inbound_webhook_67
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_67` (
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

-- 8. Dispute 表 dispute_67
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_67` (
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

-- 9. DisputeEvent 表 dispute_event_67（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_67` (
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

-- 10. AccountingOutbox 表 accounting_outbox_67
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_67` (
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

-- ─── admin_audit_log_67：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_67` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 67';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：6 = 0-9 物理库；68 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_68
CREATE TABLE IF NOT EXISTS `payment_intent_68` (
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

-- 2. Charge 表 charge_68（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_68` (
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

-- 3. PayAction 表 pay_action_68（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_68` (
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

-- 4. ExceptionCase 表 exception_case_68（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_68` (
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

-- 5. NotifyLog 表 notify_log_68（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_68` (
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

-- 5. Refund 表 refund_68（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_68` (
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

-- 7. InboundWebhook 表 inbound_webhook_68
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_68` (
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

-- 8. Dispute 表 dispute_68
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_68` (
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

-- 9. DisputeEvent 表 dispute_event_68（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_68` (
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

-- 10. AccountingOutbox 表 accounting_outbox_68
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_68` (
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

-- ─── admin_audit_log_68：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_68` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 68';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：6 = 0-9 物理库；69 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_69
CREATE TABLE IF NOT EXISTS `payment_intent_69` (
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

-- 2. Charge 表 charge_69（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_69` (
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

-- 3. PayAction 表 pay_action_69（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_69` (
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

-- 4. ExceptionCase 表 exception_case_69（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_69` (
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

-- 5. NotifyLog 表 notify_log_69（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_69` (
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

-- 5. Refund 表 refund_69（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_69` (
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

-- 7. InboundWebhook 表 inbound_webhook_69
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_69` (
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

-- 8. Dispute 表 dispute_69
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_69` (
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

-- 9. DisputeEvent 表 dispute_event_69（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_69` (
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

-- 10. AccountingOutbox 表 accounting_outbox_69
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_69` (
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

-- ─── admin_audit_log_69：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_69` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 69';


-- ==== accounting-system schema ====
SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS `accounting_db_6` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `accounting_db_6`;

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


CREATE TABLE IF NOT EXISTS `account_60` (
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
CREATE TABLE IF NOT EXISTS `account_transaction_60` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_60` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_60` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_60` (
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
CREATE TABLE IF NOT EXISTS `async_task_60` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_60` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_60` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_60` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_60` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_60` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_60` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_60` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_60` (
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
CREATE TABLE IF NOT EXISTS `batch_order_60` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_60` (
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


CREATE TABLE IF NOT EXISTS `account_61` (
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
CREATE TABLE IF NOT EXISTS `account_transaction_61` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_61` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_61` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_61` (
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
CREATE TABLE IF NOT EXISTS `async_task_61` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_61` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_61` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_61` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_61` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_61` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_61` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_61` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_61` (
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
CREATE TABLE IF NOT EXISTS `batch_order_61` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_61` (
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


CREATE TABLE IF NOT EXISTS `account_62` (
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
CREATE TABLE IF NOT EXISTS `account_transaction_62` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_62` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_62` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_62` (
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
CREATE TABLE IF NOT EXISTS `async_task_62` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_62` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_62` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_62` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_62` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_62` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_62` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_62` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_62` (
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
CREATE TABLE IF NOT EXISTS `batch_order_62` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_62` (
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


CREATE TABLE IF NOT EXISTS `account_63` (
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
CREATE TABLE IF NOT EXISTS `account_transaction_63` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_63` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_63` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_63` (
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
CREATE TABLE IF NOT EXISTS `async_task_63` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_63` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_63` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_63` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_63` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_63` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_63` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_63` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_63` (
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
CREATE TABLE IF NOT EXISTS `batch_order_63` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_63` (
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


CREATE TABLE IF NOT EXISTS `account_64` (
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
CREATE TABLE IF NOT EXISTS `account_transaction_64` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_64` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_64` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_64` (
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
CREATE TABLE IF NOT EXISTS `async_task_64` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_64` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_64` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_64` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_64` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_64` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_64` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_64` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_64` (
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
CREATE TABLE IF NOT EXISTS `batch_order_64` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_64` (
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


CREATE TABLE IF NOT EXISTS `account_65` (
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
CREATE TABLE IF NOT EXISTS `account_transaction_65` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_65` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_65` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_65` (
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
CREATE TABLE IF NOT EXISTS `async_task_65` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_65` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_65` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_65` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_65` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_65` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_65` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_65` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_65` (
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
CREATE TABLE IF NOT EXISTS `batch_order_65` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_65` (
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


CREATE TABLE IF NOT EXISTS `account_66` (
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
CREATE TABLE IF NOT EXISTS `account_transaction_66` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_66` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_66` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_66` (
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
CREATE TABLE IF NOT EXISTS `async_task_66` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_66` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_66` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_66` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_66` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_66` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_66` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_66` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_66` (
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
CREATE TABLE IF NOT EXISTS `batch_order_66` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_66` (
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


CREATE TABLE IF NOT EXISTS `account_67` (
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
CREATE TABLE IF NOT EXISTS `account_transaction_67` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_67` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_67` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_67` (
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
CREATE TABLE IF NOT EXISTS `async_task_67` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_67` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_67` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_67` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_67` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_67` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_67` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_67` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_67` (
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
CREATE TABLE IF NOT EXISTS `batch_order_67` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_67` (
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


CREATE TABLE IF NOT EXISTS `account_68` (
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
CREATE TABLE IF NOT EXISTS `account_transaction_68` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_68` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_68` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_68` (
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
CREATE TABLE IF NOT EXISTS `async_task_68` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_68` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_68` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_68` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_68` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_68` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_68` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_68` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_68` (
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
CREATE TABLE IF NOT EXISTS `batch_order_68` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_68` (
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


CREATE TABLE IF NOT EXISTS `account_69` (
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
CREATE TABLE IF NOT EXISTS `account_transaction_69` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_69` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_69` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_69` (
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
CREATE TABLE IF NOT EXISTS `async_task_69` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_69` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_69` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_69` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_69` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_69` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_69` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_69` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_69` (
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
CREATE TABLE IF NOT EXISTS `batch_order_69` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_69` (
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
-- tx_account_anchor_60: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_60` (
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
-- flow_anchor_route_60: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_60` (
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
-- tx_account_anchor_61: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_61` (
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
-- flow_anchor_route_61: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_61` (
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
-- tx_account_anchor_62: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_62` (
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
-- flow_anchor_route_62: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_62` (
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
-- tx_account_anchor_63: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_63` (
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
-- flow_anchor_route_63: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_63` (
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
-- tx_account_anchor_64: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_64` (
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
-- flow_anchor_route_64: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_64` (
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
-- tx_account_anchor_65: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_65` (
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
-- flow_anchor_route_65: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_65` (
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
-- tx_account_anchor_66: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_66` (
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
-- flow_anchor_route_66: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_66` (
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
-- tx_account_anchor_67: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_67` (
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
-- flow_anchor_route_67: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_67` (
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
-- tx_account_anchor_68: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_68` (
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
-- flow_anchor_route_68: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_68` (
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
-- tx_account_anchor_69: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_69` (
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
-- flow_anchor_route_69: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_69` (
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
USE `accounting_db_6`;

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 60 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (60)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (60)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_60` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 60 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 60, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_60` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 60 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 60, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_60` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 60 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 60, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_60` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 60 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 60, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_60` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 60 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 60, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_60` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 60 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 60, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 61 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (61)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (61)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_61` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 61 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 61, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_61` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 61 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 61, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_61` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 61 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 61, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_61` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 61 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 61, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_61` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 61 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 61, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_61` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 61 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 61, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 62 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (62)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (62)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_62` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 62 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 62, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_62` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 62 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 62, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_62` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 62 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 62, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_62` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 62 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 62, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_62` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 62 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 62, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_62` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 62 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 62, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 63 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (63)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (63)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_63` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 63 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 63, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_63` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 63 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 63, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_63` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 63 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 63, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_63` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 63 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 63, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_63` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 63 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 63, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_63` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 63 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 63, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 64 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (64)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (64)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_64` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 64 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 64, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_64` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 64 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 64, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_64` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 64 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 64, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_64` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 64 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 64, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_64` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 64 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 64, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_64` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 64 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 64, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 65 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (65)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (65)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_65` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 65 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 65, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_65` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 65 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 65, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_65` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 65 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 65, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_65` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 65 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 65, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_65` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 65 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 65, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_65` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 65 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 65, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 66 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (66)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (66)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_66` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 66 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 66, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_66` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 66 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 66, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_66` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 66 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 66, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_66` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 66 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 66, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_66` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 66 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 66, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_66` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 66 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 66, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 67 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (67)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (67)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_67` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 67 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 67, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_67` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 67 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 67, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_67` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 67 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 67, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_67` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 67 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 67, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_67` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 67 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 67, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_67` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 67 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 67, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 68 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (68)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (68)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_68` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 68 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 68, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_68` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 68 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 68, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_68` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 68 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 68, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_68` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 68 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 68, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_68` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 68 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 68, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_68` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 68 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 68, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 69 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (69)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (69)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_69` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 69 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 69, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_69` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 69 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 69, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_69` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 69 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 69, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_69` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 69 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 69, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_69` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 69 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 69, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_69` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 69 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 69, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);


-- ==== user-merchant-core ====
SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `user_merchant_db_6` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `user_merchant_db_6`;

-- 分片表 schema 模板。6 和 60 由 generate.sh 替换：
--   6     = 0..9         分库 idx
--   60  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_60` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 60)';

CREATE TABLE IF NOT EXISTS `user_profiles_60` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 60)';

CREATE TABLE IF NOT EXISTS `user_auths_60` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 60)';

CREATE TABLE IF NOT EXISTS `login_logs_60` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 60)';

CREATE TABLE IF NOT EXISTS `user_sessions_60` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 60)';

CREATE TABLE IF NOT EXISTS `user_roles_60` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 60)';

CREATE TABLE IF NOT EXISTS `user_accounts_60` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 60)';

CREATE TABLE IF NOT EXISTS `user_settings_60` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 60)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_60` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 60)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_60` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 60)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_60` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 60)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_60` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 60)';

-- ─── admin_audit_log_60：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_60` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 60';

-- 分片表 schema 模板。6 和 61 由 generate.sh 替换：
--   6     = 0..9         分库 idx
--   61  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_61` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 61)';

CREATE TABLE IF NOT EXISTS `user_profiles_61` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 61)';

CREATE TABLE IF NOT EXISTS `user_auths_61` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 61)';

CREATE TABLE IF NOT EXISTS `login_logs_61` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 61)';

CREATE TABLE IF NOT EXISTS `user_sessions_61` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 61)';

CREATE TABLE IF NOT EXISTS `user_roles_61` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 61)';

CREATE TABLE IF NOT EXISTS `user_accounts_61` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 61)';

CREATE TABLE IF NOT EXISTS `user_settings_61` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 61)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_61` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 61)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_61` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 61)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_61` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 61)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_61` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 61)';

-- ─── admin_audit_log_61：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_61` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 61';

-- 分片表 schema 模板。6 和 62 由 generate.sh 替换：
--   6     = 0..9         分库 idx
--   62  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_62` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 62)';

CREATE TABLE IF NOT EXISTS `user_profiles_62` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 62)';

CREATE TABLE IF NOT EXISTS `user_auths_62` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 62)';

CREATE TABLE IF NOT EXISTS `login_logs_62` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 62)';

CREATE TABLE IF NOT EXISTS `user_sessions_62` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 62)';

CREATE TABLE IF NOT EXISTS `user_roles_62` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 62)';

CREATE TABLE IF NOT EXISTS `user_accounts_62` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 62)';

CREATE TABLE IF NOT EXISTS `user_settings_62` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 62)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_62` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 62)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_62` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 62)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_62` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 62)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_62` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 62)';

-- ─── admin_audit_log_62：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_62` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 62';

-- 分片表 schema 模板。6 和 63 由 generate.sh 替换：
--   6     = 0..9         分库 idx
--   63  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_63` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 63)';

CREATE TABLE IF NOT EXISTS `user_profiles_63` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 63)';

CREATE TABLE IF NOT EXISTS `user_auths_63` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 63)';

CREATE TABLE IF NOT EXISTS `login_logs_63` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 63)';

CREATE TABLE IF NOT EXISTS `user_sessions_63` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 63)';

CREATE TABLE IF NOT EXISTS `user_roles_63` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 63)';

CREATE TABLE IF NOT EXISTS `user_accounts_63` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 63)';

CREATE TABLE IF NOT EXISTS `user_settings_63` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 63)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_63` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 63)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_63` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 63)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_63` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 63)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_63` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 63)';

-- ─── admin_audit_log_63：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_63` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 63';

-- 分片表 schema 模板。6 和 64 由 generate.sh 替换：
--   6     = 0..9         分库 idx
--   64  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_64` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 64)';

CREATE TABLE IF NOT EXISTS `user_profiles_64` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 64)';

CREATE TABLE IF NOT EXISTS `user_auths_64` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 64)';

CREATE TABLE IF NOT EXISTS `login_logs_64` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 64)';

CREATE TABLE IF NOT EXISTS `user_sessions_64` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 64)';

CREATE TABLE IF NOT EXISTS `user_roles_64` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 64)';

CREATE TABLE IF NOT EXISTS `user_accounts_64` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 64)';

CREATE TABLE IF NOT EXISTS `user_settings_64` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 64)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_64` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 64)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_64` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 64)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_64` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 64)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_64` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 64)';

-- ─── admin_audit_log_64：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_64` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 64';

-- 分片表 schema 模板。6 和 65 由 generate.sh 替换：
--   6     = 0..9         分库 idx
--   65  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_65` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 65)';

CREATE TABLE IF NOT EXISTS `user_profiles_65` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 65)';

CREATE TABLE IF NOT EXISTS `user_auths_65` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 65)';

CREATE TABLE IF NOT EXISTS `login_logs_65` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 65)';

CREATE TABLE IF NOT EXISTS `user_sessions_65` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 65)';

CREATE TABLE IF NOT EXISTS `user_roles_65` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 65)';

CREATE TABLE IF NOT EXISTS `user_accounts_65` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 65)';

CREATE TABLE IF NOT EXISTS `user_settings_65` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 65)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_65` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 65)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_65` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 65)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_65` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 65)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_65` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 65)';

-- ─── admin_audit_log_65：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_65` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 65';

-- 分片表 schema 模板。6 和 66 由 generate.sh 替换：
--   6     = 0..9         分库 idx
--   66  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_66` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 66)';

CREATE TABLE IF NOT EXISTS `user_profiles_66` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 66)';

CREATE TABLE IF NOT EXISTS `user_auths_66` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 66)';

CREATE TABLE IF NOT EXISTS `login_logs_66` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 66)';

CREATE TABLE IF NOT EXISTS `user_sessions_66` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 66)';

CREATE TABLE IF NOT EXISTS `user_roles_66` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 66)';

CREATE TABLE IF NOT EXISTS `user_accounts_66` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 66)';

CREATE TABLE IF NOT EXISTS `user_settings_66` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 66)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_66` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 66)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_66` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 66)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_66` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 66)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_66` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 66)';

-- ─── admin_audit_log_66：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_66` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 66';

-- 分片表 schema 模板。6 和 67 由 generate.sh 替换：
--   6     = 0..9         分库 idx
--   67  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_67` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 67)';

CREATE TABLE IF NOT EXISTS `user_profiles_67` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 67)';

CREATE TABLE IF NOT EXISTS `user_auths_67` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 67)';

CREATE TABLE IF NOT EXISTS `login_logs_67` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 67)';

CREATE TABLE IF NOT EXISTS `user_sessions_67` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 67)';

CREATE TABLE IF NOT EXISTS `user_roles_67` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 67)';

CREATE TABLE IF NOT EXISTS `user_accounts_67` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 67)';

CREATE TABLE IF NOT EXISTS `user_settings_67` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 67)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_67` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 67)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_67` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 67)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_67` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 67)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_67` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 67)';

-- ─── admin_audit_log_67：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_67` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 67';

-- 分片表 schema 模板。6 和 68 由 generate.sh 替换：
--   6     = 0..9         分库 idx
--   68  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_68` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 68)';

CREATE TABLE IF NOT EXISTS `user_profiles_68` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 68)';

CREATE TABLE IF NOT EXISTS `user_auths_68` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 68)';

CREATE TABLE IF NOT EXISTS `login_logs_68` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 68)';

CREATE TABLE IF NOT EXISTS `user_sessions_68` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 68)';

CREATE TABLE IF NOT EXISTS `user_roles_68` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 68)';

CREATE TABLE IF NOT EXISTS `user_accounts_68` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 68)';

CREATE TABLE IF NOT EXISTS `user_settings_68` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 68)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_68` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 68)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_68` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 68)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_68` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 68)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_68` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 68)';

-- ─── admin_audit_log_68：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_68` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 68';

-- 分片表 schema 模板。6 和 69 由 generate.sh 替换：
--   6     = 0..9         分库 idx
--   69  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_69` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 69)';

CREATE TABLE IF NOT EXISTS `user_profiles_69` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 69)';

CREATE TABLE IF NOT EXISTS `user_auths_69` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 69)';

CREATE TABLE IF NOT EXISTS `login_logs_69` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 69)';

CREATE TABLE IF NOT EXISTS `user_sessions_69` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 69)';

CREATE TABLE IF NOT EXISTS `user_roles_69` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 69)';

CREATE TABLE IF NOT EXISTS `user_accounts_69` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 69)';

CREATE TABLE IF NOT EXISTS `user_settings_69` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 69)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_69` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 69)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_69` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 69)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_69` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 69)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_69` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 69)';

-- ─── admin_audit_log_69：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_69` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 69';


-- ==== payment-channel _shadow ====
-- paychan_db_6 的影子表（压测 / shadow 流量）
-- 依赖：6_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_6`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_60_shadow` LIKE `acquirer_tx_60`;
CREATE TABLE IF NOT EXISTS `webhook_raw_60_shadow` LIKE `webhook_raw_60`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_60_shadow` LIKE `webhook_raw_rejected_60`;
CREATE TABLE IF NOT EXISTS `channel_token_60_shadow` LIKE `channel_token_60`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_61_shadow` LIKE `acquirer_tx_61`;
CREATE TABLE IF NOT EXISTS `webhook_raw_61_shadow` LIKE `webhook_raw_61`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_61_shadow` LIKE `webhook_raw_rejected_61`;
CREATE TABLE IF NOT EXISTS `channel_token_61_shadow` LIKE `channel_token_61`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_62_shadow` LIKE `acquirer_tx_62`;
CREATE TABLE IF NOT EXISTS `webhook_raw_62_shadow` LIKE `webhook_raw_62`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_62_shadow` LIKE `webhook_raw_rejected_62`;
CREATE TABLE IF NOT EXISTS `channel_token_62_shadow` LIKE `channel_token_62`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_63_shadow` LIKE `acquirer_tx_63`;
CREATE TABLE IF NOT EXISTS `webhook_raw_63_shadow` LIKE `webhook_raw_63`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_63_shadow` LIKE `webhook_raw_rejected_63`;
CREATE TABLE IF NOT EXISTS `channel_token_63_shadow` LIKE `channel_token_63`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_64_shadow` LIKE `acquirer_tx_64`;
CREATE TABLE IF NOT EXISTS `webhook_raw_64_shadow` LIKE `webhook_raw_64`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_64_shadow` LIKE `webhook_raw_rejected_64`;
CREATE TABLE IF NOT EXISTS `channel_token_64_shadow` LIKE `channel_token_64`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_65_shadow` LIKE `acquirer_tx_65`;
CREATE TABLE IF NOT EXISTS `webhook_raw_65_shadow` LIKE `webhook_raw_65`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_65_shadow` LIKE `webhook_raw_rejected_65`;
CREATE TABLE IF NOT EXISTS `channel_token_65_shadow` LIKE `channel_token_65`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_66_shadow` LIKE `acquirer_tx_66`;
CREATE TABLE IF NOT EXISTS `webhook_raw_66_shadow` LIKE `webhook_raw_66`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_66_shadow` LIKE `webhook_raw_rejected_66`;
CREATE TABLE IF NOT EXISTS `channel_token_66_shadow` LIKE `channel_token_66`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_67_shadow` LIKE `acquirer_tx_67`;
CREATE TABLE IF NOT EXISTS `webhook_raw_67_shadow` LIKE `webhook_raw_67`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_67_shadow` LIKE `webhook_raw_rejected_67`;
CREATE TABLE IF NOT EXISTS `channel_token_67_shadow` LIKE `channel_token_67`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_68_shadow` LIKE `acquirer_tx_68`;
CREATE TABLE IF NOT EXISTS `webhook_raw_68_shadow` LIKE `webhook_raw_68`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_68_shadow` LIKE `webhook_raw_rejected_68`;
CREATE TABLE IF NOT EXISTS `channel_token_68_shadow` LIKE `channel_token_68`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_69_shadow` LIKE `acquirer_tx_69`;
CREATE TABLE IF NOT EXISTS `webhook_raw_69_shadow` LIKE `webhook_raw_69`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_69_shadow` LIKE `webhook_raw_rejected_69`;
CREATE TABLE IF NOT EXISTS `channel_token_69_shadow` LIKE `channel_token_69`;


-- ==== order-core _shadow ====
-- order_db_6 的影子表（压测 / shadow 流量）
-- 依赖：6_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_6`;

CREATE TABLE IF NOT EXISTS `payment_intent_60_shadow` LIKE `payment_intent_60`;
CREATE TABLE IF NOT EXISTS `charge_60_shadow` LIKE `charge_60`;
CREATE TABLE IF NOT EXISTS `refund_60_shadow` LIKE `refund_60`;
CREATE TABLE IF NOT EXISTS `pay_action_60_shadow` LIKE `pay_action_60`;
CREATE TABLE IF NOT EXISTS `dispute_60_shadow` LIKE `dispute_60`;
CREATE TABLE IF NOT EXISTS `dispute_event_60_shadow` LIKE `dispute_event_60`;
CREATE TABLE IF NOT EXISTS `exception_case_60_shadow` LIKE `exception_case_60`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_60_shadow` LIKE `inbound_webhook_60`;
CREATE TABLE IF NOT EXISTS `notify_log_60_shadow` LIKE `notify_log_60`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_60_shadow` LIKE `accounting_outbox_60`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_60_shadow` LIKE `admin_audit_log_60`;

CREATE TABLE IF NOT EXISTS `payment_intent_61_shadow` LIKE `payment_intent_61`;
CREATE TABLE IF NOT EXISTS `charge_61_shadow` LIKE `charge_61`;
CREATE TABLE IF NOT EXISTS `refund_61_shadow` LIKE `refund_61`;
CREATE TABLE IF NOT EXISTS `pay_action_61_shadow` LIKE `pay_action_61`;
CREATE TABLE IF NOT EXISTS `dispute_61_shadow` LIKE `dispute_61`;
CREATE TABLE IF NOT EXISTS `dispute_event_61_shadow` LIKE `dispute_event_61`;
CREATE TABLE IF NOT EXISTS `exception_case_61_shadow` LIKE `exception_case_61`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_61_shadow` LIKE `inbound_webhook_61`;
CREATE TABLE IF NOT EXISTS `notify_log_61_shadow` LIKE `notify_log_61`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_61_shadow` LIKE `accounting_outbox_61`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_61_shadow` LIKE `admin_audit_log_61`;

CREATE TABLE IF NOT EXISTS `payment_intent_62_shadow` LIKE `payment_intent_62`;
CREATE TABLE IF NOT EXISTS `charge_62_shadow` LIKE `charge_62`;
CREATE TABLE IF NOT EXISTS `refund_62_shadow` LIKE `refund_62`;
CREATE TABLE IF NOT EXISTS `pay_action_62_shadow` LIKE `pay_action_62`;
CREATE TABLE IF NOT EXISTS `dispute_62_shadow` LIKE `dispute_62`;
CREATE TABLE IF NOT EXISTS `dispute_event_62_shadow` LIKE `dispute_event_62`;
CREATE TABLE IF NOT EXISTS `exception_case_62_shadow` LIKE `exception_case_62`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_62_shadow` LIKE `inbound_webhook_62`;
CREATE TABLE IF NOT EXISTS `notify_log_62_shadow` LIKE `notify_log_62`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_62_shadow` LIKE `accounting_outbox_62`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_62_shadow` LIKE `admin_audit_log_62`;

CREATE TABLE IF NOT EXISTS `payment_intent_63_shadow` LIKE `payment_intent_63`;
CREATE TABLE IF NOT EXISTS `charge_63_shadow` LIKE `charge_63`;
CREATE TABLE IF NOT EXISTS `refund_63_shadow` LIKE `refund_63`;
CREATE TABLE IF NOT EXISTS `pay_action_63_shadow` LIKE `pay_action_63`;
CREATE TABLE IF NOT EXISTS `dispute_63_shadow` LIKE `dispute_63`;
CREATE TABLE IF NOT EXISTS `dispute_event_63_shadow` LIKE `dispute_event_63`;
CREATE TABLE IF NOT EXISTS `exception_case_63_shadow` LIKE `exception_case_63`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_63_shadow` LIKE `inbound_webhook_63`;
CREATE TABLE IF NOT EXISTS `notify_log_63_shadow` LIKE `notify_log_63`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_63_shadow` LIKE `accounting_outbox_63`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_63_shadow` LIKE `admin_audit_log_63`;

CREATE TABLE IF NOT EXISTS `payment_intent_64_shadow` LIKE `payment_intent_64`;
CREATE TABLE IF NOT EXISTS `charge_64_shadow` LIKE `charge_64`;
CREATE TABLE IF NOT EXISTS `refund_64_shadow` LIKE `refund_64`;
CREATE TABLE IF NOT EXISTS `pay_action_64_shadow` LIKE `pay_action_64`;
CREATE TABLE IF NOT EXISTS `dispute_64_shadow` LIKE `dispute_64`;
CREATE TABLE IF NOT EXISTS `dispute_event_64_shadow` LIKE `dispute_event_64`;
CREATE TABLE IF NOT EXISTS `exception_case_64_shadow` LIKE `exception_case_64`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_64_shadow` LIKE `inbound_webhook_64`;
CREATE TABLE IF NOT EXISTS `notify_log_64_shadow` LIKE `notify_log_64`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_64_shadow` LIKE `accounting_outbox_64`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_64_shadow` LIKE `admin_audit_log_64`;

CREATE TABLE IF NOT EXISTS `payment_intent_65_shadow` LIKE `payment_intent_65`;
CREATE TABLE IF NOT EXISTS `charge_65_shadow` LIKE `charge_65`;
CREATE TABLE IF NOT EXISTS `refund_65_shadow` LIKE `refund_65`;
CREATE TABLE IF NOT EXISTS `pay_action_65_shadow` LIKE `pay_action_65`;
CREATE TABLE IF NOT EXISTS `dispute_65_shadow` LIKE `dispute_65`;
CREATE TABLE IF NOT EXISTS `dispute_event_65_shadow` LIKE `dispute_event_65`;
CREATE TABLE IF NOT EXISTS `exception_case_65_shadow` LIKE `exception_case_65`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_65_shadow` LIKE `inbound_webhook_65`;
CREATE TABLE IF NOT EXISTS `notify_log_65_shadow` LIKE `notify_log_65`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_65_shadow` LIKE `accounting_outbox_65`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_65_shadow` LIKE `admin_audit_log_65`;

CREATE TABLE IF NOT EXISTS `payment_intent_66_shadow` LIKE `payment_intent_66`;
CREATE TABLE IF NOT EXISTS `charge_66_shadow` LIKE `charge_66`;
CREATE TABLE IF NOT EXISTS `refund_66_shadow` LIKE `refund_66`;
CREATE TABLE IF NOT EXISTS `pay_action_66_shadow` LIKE `pay_action_66`;
CREATE TABLE IF NOT EXISTS `dispute_66_shadow` LIKE `dispute_66`;
CREATE TABLE IF NOT EXISTS `dispute_event_66_shadow` LIKE `dispute_event_66`;
CREATE TABLE IF NOT EXISTS `exception_case_66_shadow` LIKE `exception_case_66`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_66_shadow` LIKE `inbound_webhook_66`;
CREATE TABLE IF NOT EXISTS `notify_log_66_shadow` LIKE `notify_log_66`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_66_shadow` LIKE `accounting_outbox_66`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_66_shadow` LIKE `admin_audit_log_66`;

CREATE TABLE IF NOT EXISTS `payment_intent_67_shadow` LIKE `payment_intent_67`;
CREATE TABLE IF NOT EXISTS `charge_67_shadow` LIKE `charge_67`;
CREATE TABLE IF NOT EXISTS `refund_67_shadow` LIKE `refund_67`;
CREATE TABLE IF NOT EXISTS `pay_action_67_shadow` LIKE `pay_action_67`;
CREATE TABLE IF NOT EXISTS `dispute_67_shadow` LIKE `dispute_67`;
CREATE TABLE IF NOT EXISTS `dispute_event_67_shadow` LIKE `dispute_event_67`;
CREATE TABLE IF NOT EXISTS `exception_case_67_shadow` LIKE `exception_case_67`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_67_shadow` LIKE `inbound_webhook_67`;
CREATE TABLE IF NOT EXISTS `notify_log_67_shadow` LIKE `notify_log_67`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_67_shadow` LIKE `accounting_outbox_67`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_67_shadow` LIKE `admin_audit_log_67`;

CREATE TABLE IF NOT EXISTS `payment_intent_68_shadow` LIKE `payment_intent_68`;
CREATE TABLE IF NOT EXISTS `charge_68_shadow` LIKE `charge_68`;
CREATE TABLE IF NOT EXISTS `refund_68_shadow` LIKE `refund_68`;
CREATE TABLE IF NOT EXISTS `pay_action_68_shadow` LIKE `pay_action_68`;
CREATE TABLE IF NOT EXISTS `dispute_68_shadow` LIKE `dispute_68`;
CREATE TABLE IF NOT EXISTS `dispute_event_68_shadow` LIKE `dispute_event_68`;
CREATE TABLE IF NOT EXISTS `exception_case_68_shadow` LIKE `exception_case_68`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_68_shadow` LIKE `inbound_webhook_68`;
CREATE TABLE IF NOT EXISTS `notify_log_68_shadow` LIKE `notify_log_68`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_68_shadow` LIKE `accounting_outbox_68`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_68_shadow` LIKE `admin_audit_log_68`;

CREATE TABLE IF NOT EXISTS `payment_intent_69_shadow` LIKE `payment_intent_69`;
CREATE TABLE IF NOT EXISTS `charge_69_shadow` LIKE `charge_69`;
CREATE TABLE IF NOT EXISTS `refund_69_shadow` LIKE `refund_69`;
CREATE TABLE IF NOT EXISTS `pay_action_69_shadow` LIKE `pay_action_69`;
CREATE TABLE IF NOT EXISTS `dispute_69_shadow` LIKE `dispute_69`;
CREATE TABLE IF NOT EXISTS `dispute_event_69_shadow` LIKE `dispute_event_69`;
CREATE TABLE IF NOT EXISTS `exception_case_69_shadow` LIKE `exception_case_69`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_69_shadow` LIKE `inbound_webhook_69`;
CREATE TABLE IF NOT EXISTS `notify_log_69_shadow` LIKE `notify_log_69`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_69_shadow` LIKE `accounting_outbox_69`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_69_shadow` LIKE `admin_audit_log_69`;


-- ==== accounting-system _shadow ====
-- accounting_db_6 的影子表（压测 / shadow 流量）
-- 依赖：6_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
SET NAMES utf8mb4;
USE `accounting_db_6`;

CREATE TABLE IF NOT EXISTS `account_60_shadow` LIKE `account_60`;
CREATE TABLE IF NOT EXISTS `account_transaction_60_shadow` LIKE `account_transaction_60`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_60_shadow` LIKE `accounting_voucher_60`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_60_shadow` LIKE `account_balance_snapshot_60`;
CREATE TABLE IF NOT EXISTS `day_cut_control_60_shadow` LIKE `day_cut_control_60`;
CREATE TABLE IF NOT EXISTS `async_task_60_shadow` LIKE `async_task_60`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_60_shadow` LIKE `tcc_transaction_60`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_60_shadow` LIKE `freeze_compensate_outbox_60`;
CREATE TABLE IF NOT EXISTS `distributed_lock_60_shadow` LIKE `distributed_lock_60`;
CREATE TABLE IF NOT EXISTS `merchant_info_60_shadow` LIKE `merchant_info_60`;
CREATE TABLE IF NOT EXISTS `transaction_order_60_shadow` LIKE `transaction_order_60`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_60_shadow` LIKE `transaction_order_extra_60`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_60_shadow` LIKE `account_balance_buffer_60`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_60_shadow` LIKE `tcc_coordinator_60`;
CREATE TABLE IF NOT EXISTS `batch_order_60_shadow` LIKE `batch_order_60`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_60_shadow` LIKE `settlement_outbox_60`;

CREATE TABLE IF NOT EXISTS `account_61_shadow` LIKE `account_61`;
CREATE TABLE IF NOT EXISTS `account_transaction_61_shadow` LIKE `account_transaction_61`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_61_shadow` LIKE `accounting_voucher_61`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_61_shadow` LIKE `account_balance_snapshot_61`;
CREATE TABLE IF NOT EXISTS `day_cut_control_61_shadow` LIKE `day_cut_control_61`;
CREATE TABLE IF NOT EXISTS `async_task_61_shadow` LIKE `async_task_61`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_61_shadow` LIKE `tcc_transaction_61`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_61_shadow` LIKE `freeze_compensate_outbox_61`;
CREATE TABLE IF NOT EXISTS `distributed_lock_61_shadow` LIKE `distributed_lock_61`;
CREATE TABLE IF NOT EXISTS `merchant_info_61_shadow` LIKE `merchant_info_61`;
CREATE TABLE IF NOT EXISTS `transaction_order_61_shadow` LIKE `transaction_order_61`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_61_shadow` LIKE `transaction_order_extra_61`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_61_shadow` LIKE `account_balance_buffer_61`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_61_shadow` LIKE `tcc_coordinator_61`;
CREATE TABLE IF NOT EXISTS `batch_order_61_shadow` LIKE `batch_order_61`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_61_shadow` LIKE `settlement_outbox_61`;

CREATE TABLE IF NOT EXISTS `account_62_shadow` LIKE `account_62`;
CREATE TABLE IF NOT EXISTS `account_transaction_62_shadow` LIKE `account_transaction_62`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_62_shadow` LIKE `accounting_voucher_62`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_62_shadow` LIKE `account_balance_snapshot_62`;
CREATE TABLE IF NOT EXISTS `day_cut_control_62_shadow` LIKE `day_cut_control_62`;
CREATE TABLE IF NOT EXISTS `async_task_62_shadow` LIKE `async_task_62`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_62_shadow` LIKE `tcc_transaction_62`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_62_shadow` LIKE `freeze_compensate_outbox_62`;
CREATE TABLE IF NOT EXISTS `distributed_lock_62_shadow` LIKE `distributed_lock_62`;
CREATE TABLE IF NOT EXISTS `merchant_info_62_shadow` LIKE `merchant_info_62`;
CREATE TABLE IF NOT EXISTS `transaction_order_62_shadow` LIKE `transaction_order_62`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_62_shadow` LIKE `transaction_order_extra_62`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_62_shadow` LIKE `account_balance_buffer_62`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_62_shadow` LIKE `tcc_coordinator_62`;
CREATE TABLE IF NOT EXISTS `batch_order_62_shadow` LIKE `batch_order_62`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_62_shadow` LIKE `settlement_outbox_62`;

CREATE TABLE IF NOT EXISTS `account_63_shadow` LIKE `account_63`;
CREATE TABLE IF NOT EXISTS `account_transaction_63_shadow` LIKE `account_transaction_63`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_63_shadow` LIKE `accounting_voucher_63`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_63_shadow` LIKE `account_balance_snapshot_63`;
CREATE TABLE IF NOT EXISTS `day_cut_control_63_shadow` LIKE `day_cut_control_63`;
CREATE TABLE IF NOT EXISTS `async_task_63_shadow` LIKE `async_task_63`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_63_shadow` LIKE `tcc_transaction_63`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_63_shadow` LIKE `freeze_compensate_outbox_63`;
CREATE TABLE IF NOT EXISTS `distributed_lock_63_shadow` LIKE `distributed_lock_63`;
CREATE TABLE IF NOT EXISTS `merchant_info_63_shadow` LIKE `merchant_info_63`;
CREATE TABLE IF NOT EXISTS `transaction_order_63_shadow` LIKE `transaction_order_63`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_63_shadow` LIKE `transaction_order_extra_63`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_63_shadow` LIKE `account_balance_buffer_63`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_63_shadow` LIKE `tcc_coordinator_63`;
CREATE TABLE IF NOT EXISTS `batch_order_63_shadow` LIKE `batch_order_63`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_63_shadow` LIKE `settlement_outbox_63`;

CREATE TABLE IF NOT EXISTS `account_64_shadow` LIKE `account_64`;
CREATE TABLE IF NOT EXISTS `account_transaction_64_shadow` LIKE `account_transaction_64`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_64_shadow` LIKE `accounting_voucher_64`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_64_shadow` LIKE `account_balance_snapshot_64`;
CREATE TABLE IF NOT EXISTS `day_cut_control_64_shadow` LIKE `day_cut_control_64`;
CREATE TABLE IF NOT EXISTS `async_task_64_shadow` LIKE `async_task_64`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_64_shadow` LIKE `tcc_transaction_64`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_64_shadow` LIKE `freeze_compensate_outbox_64`;
CREATE TABLE IF NOT EXISTS `distributed_lock_64_shadow` LIKE `distributed_lock_64`;
CREATE TABLE IF NOT EXISTS `merchant_info_64_shadow` LIKE `merchant_info_64`;
CREATE TABLE IF NOT EXISTS `transaction_order_64_shadow` LIKE `transaction_order_64`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_64_shadow` LIKE `transaction_order_extra_64`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_64_shadow` LIKE `account_balance_buffer_64`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_64_shadow` LIKE `tcc_coordinator_64`;
CREATE TABLE IF NOT EXISTS `batch_order_64_shadow` LIKE `batch_order_64`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_64_shadow` LIKE `settlement_outbox_64`;

CREATE TABLE IF NOT EXISTS `account_65_shadow` LIKE `account_65`;
CREATE TABLE IF NOT EXISTS `account_transaction_65_shadow` LIKE `account_transaction_65`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_65_shadow` LIKE `accounting_voucher_65`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_65_shadow` LIKE `account_balance_snapshot_65`;
CREATE TABLE IF NOT EXISTS `day_cut_control_65_shadow` LIKE `day_cut_control_65`;
CREATE TABLE IF NOT EXISTS `async_task_65_shadow` LIKE `async_task_65`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_65_shadow` LIKE `tcc_transaction_65`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_65_shadow` LIKE `freeze_compensate_outbox_65`;
CREATE TABLE IF NOT EXISTS `distributed_lock_65_shadow` LIKE `distributed_lock_65`;
CREATE TABLE IF NOT EXISTS `merchant_info_65_shadow` LIKE `merchant_info_65`;
CREATE TABLE IF NOT EXISTS `transaction_order_65_shadow` LIKE `transaction_order_65`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_65_shadow` LIKE `transaction_order_extra_65`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_65_shadow` LIKE `account_balance_buffer_65`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_65_shadow` LIKE `tcc_coordinator_65`;
CREATE TABLE IF NOT EXISTS `batch_order_65_shadow` LIKE `batch_order_65`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_65_shadow` LIKE `settlement_outbox_65`;

CREATE TABLE IF NOT EXISTS `account_66_shadow` LIKE `account_66`;
CREATE TABLE IF NOT EXISTS `account_transaction_66_shadow` LIKE `account_transaction_66`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_66_shadow` LIKE `accounting_voucher_66`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_66_shadow` LIKE `account_balance_snapshot_66`;
CREATE TABLE IF NOT EXISTS `day_cut_control_66_shadow` LIKE `day_cut_control_66`;
CREATE TABLE IF NOT EXISTS `async_task_66_shadow` LIKE `async_task_66`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_66_shadow` LIKE `tcc_transaction_66`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_66_shadow` LIKE `freeze_compensate_outbox_66`;
CREATE TABLE IF NOT EXISTS `distributed_lock_66_shadow` LIKE `distributed_lock_66`;
CREATE TABLE IF NOT EXISTS `merchant_info_66_shadow` LIKE `merchant_info_66`;
CREATE TABLE IF NOT EXISTS `transaction_order_66_shadow` LIKE `transaction_order_66`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_66_shadow` LIKE `transaction_order_extra_66`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_66_shadow` LIKE `account_balance_buffer_66`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_66_shadow` LIKE `tcc_coordinator_66`;
CREATE TABLE IF NOT EXISTS `batch_order_66_shadow` LIKE `batch_order_66`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_66_shadow` LIKE `settlement_outbox_66`;

CREATE TABLE IF NOT EXISTS `account_67_shadow` LIKE `account_67`;
CREATE TABLE IF NOT EXISTS `account_transaction_67_shadow` LIKE `account_transaction_67`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_67_shadow` LIKE `accounting_voucher_67`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_67_shadow` LIKE `account_balance_snapshot_67`;
CREATE TABLE IF NOT EXISTS `day_cut_control_67_shadow` LIKE `day_cut_control_67`;
CREATE TABLE IF NOT EXISTS `async_task_67_shadow` LIKE `async_task_67`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_67_shadow` LIKE `tcc_transaction_67`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_67_shadow` LIKE `freeze_compensate_outbox_67`;
CREATE TABLE IF NOT EXISTS `distributed_lock_67_shadow` LIKE `distributed_lock_67`;
CREATE TABLE IF NOT EXISTS `merchant_info_67_shadow` LIKE `merchant_info_67`;
CREATE TABLE IF NOT EXISTS `transaction_order_67_shadow` LIKE `transaction_order_67`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_67_shadow` LIKE `transaction_order_extra_67`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_67_shadow` LIKE `account_balance_buffer_67`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_67_shadow` LIKE `tcc_coordinator_67`;
CREATE TABLE IF NOT EXISTS `batch_order_67_shadow` LIKE `batch_order_67`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_67_shadow` LIKE `settlement_outbox_67`;

CREATE TABLE IF NOT EXISTS `account_68_shadow` LIKE `account_68`;
CREATE TABLE IF NOT EXISTS `account_transaction_68_shadow` LIKE `account_transaction_68`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_68_shadow` LIKE `accounting_voucher_68`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_68_shadow` LIKE `account_balance_snapshot_68`;
CREATE TABLE IF NOT EXISTS `day_cut_control_68_shadow` LIKE `day_cut_control_68`;
CREATE TABLE IF NOT EXISTS `async_task_68_shadow` LIKE `async_task_68`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_68_shadow` LIKE `tcc_transaction_68`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_68_shadow` LIKE `freeze_compensate_outbox_68`;
CREATE TABLE IF NOT EXISTS `distributed_lock_68_shadow` LIKE `distributed_lock_68`;
CREATE TABLE IF NOT EXISTS `merchant_info_68_shadow` LIKE `merchant_info_68`;
CREATE TABLE IF NOT EXISTS `transaction_order_68_shadow` LIKE `transaction_order_68`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_68_shadow` LIKE `transaction_order_extra_68`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_68_shadow` LIKE `account_balance_buffer_68`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_68_shadow` LIKE `tcc_coordinator_68`;
CREATE TABLE IF NOT EXISTS `batch_order_68_shadow` LIKE `batch_order_68`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_68_shadow` LIKE `settlement_outbox_68`;

CREATE TABLE IF NOT EXISTS `account_69_shadow` LIKE `account_69`;
CREATE TABLE IF NOT EXISTS `account_transaction_69_shadow` LIKE `account_transaction_69`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_69_shadow` LIKE `accounting_voucher_69`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_69_shadow` LIKE `account_balance_snapshot_69`;
CREATE TABLE IF NOT EXISTS `day_cut_control_69_shadow` LIKE `day_cut_control_69`;
CREATE TABLE IF NOT EXISTS `async_task_69_shadow` LIKE `async_task_69`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_69_shadow` LIKE `tcc_transaction_69`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_69_shadow` LIKE `freeze_compensate_outbox_69`;
CREATE TABLE IF NOT EXISTS `distributed_lock_69_shadow` LIKE `distributed_lock_69`;
CREATE TABLE IF NOT EXISTS `merchant_info_69_shadow` LIKE `merchant_info_69`;
CREATE TABLE IF NOT EXISTS `transaction_order_69_shadow` LIKE `transaction_order_69`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_69_shadow` LIKE `transaction_order_extra_69`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_69_shadow` LIKE `account_balance_buffer_69`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_69_shadow` LIKE `tcc_coordinator_69`;
CREATE TABLE IF NOT EXISTS `batch_order_69_shadow` LIKE `batch_order_69`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_69_shadow` LIKE `settlement_outbox_69`;



CREATE TABLE IF NOT EXISTS `tx_account_anchor_60_shadow` LIKE `tx_account_anchor_60`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_60_shadow` LIKE `flow_anchor_route_60`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_61_shadow` LIKE `tx_account_anchor_61`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_61_shadow` LIKE `flow_anchor_route_61`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_62_shadow` LIKE `tx_account_anchor_62`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_62_shadow` LIKE `flow_anchor_route_62`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_63_shadow` LIKE `tx_account_anchor_63`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_63_shadow` LIKE `flow_anchor_route_63`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_64_shadow` LIKE `tx_account_anchor_64`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_64_shadow` LIKE `flow_anchor_route_64`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_65_shadow` LIKE `tx_account_anchor_65`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_65_shadow` LIKE `flow_anchor_route_65`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_66_shadow` LIKE `tx_account_anchor_66`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_66_shadow` LIKE `flow_anchor_route_66`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_67_shadow` LIKE `tx_account_anchor_67`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_67_shadow` LIKE `flow_anchor_route_67`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_68_shadow` LIKE `tx_account_anchor_68`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_68_shadow` LIKE `flow_anchor_route_68`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_69_shadow` LIKE `tx_account_anchor_69`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_69_shadow` LIKE `flow_anchor_route_69`;

-- ==== user-merchant-core _shadow ====
-- user_merchant_db_6 的影子表（压测 / shadow 流量）
-- 依赖：6_init.sql 已经导入完成（CREATE TABLE LIKE 需要主表存在）。
SET NAMES utf8mb4;
USE `user_merchant_db_6`;

CREATE TABLE IF NOT EXISTS `users_60_shadow` LIKE `users_60`;
CREATE TABLE IF NOT EXISTS `user_profiles_60_shadow` LIKE `user_profiles_60`;
CREATE TABLE IF NOT EXISTS `user_auths_60_shadow` LIKE `user_auths_60`;
CREATE TABLE IF NOT EXISTS `login_logs_60_shadow` LIKE `login_logs_60`;
CREATE TABLE IF NOT EXISTS `user_sessions_60_shadow` LIKE `user_sessions_60`;
CREATE TABLE IF NOT EXISTS `user_roles_60_shadow` LIKE `user_roles_60`;
CREATE TABLE IF NOT EXISTS `user_accounts_60_shadow` LIKE `user_accounts_60`;
CREATE TABLE IF NOT EXISTS `user_settings_60_shadow` LIKE `user_settings_60`;
CREATE TABLE IF NOT EXISTS `user_card_60_shadow` LIKE `user_card_60`;
CREATE TABLE IF NOT EXISTS `merchants_60_shadow` LIKE `merchants_60`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_60_shadow` LIKE `merchant_kyc_document_60`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_60_shadow` LIKE `merchant_channel_secret_60`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_60_shadow` LIKE `admin_audit_log_60`;

CREATE TABLE IF NOT EXISTS `users_61_shadow` LIKE `users_61`;
CREATE TABLE IF NOT EXISTS `user_profiles_61_shadow` LIKE `user_profiles_61`;
CREATE TABLE IF NOT EXISTS `user_auths_61_shadow` LIKE `user_auths_61`;
CREATE TABLE IF NOT EXISTS `login_logs_61_shadow` LIKE `login_logs_61`;
CREATE TABLE IF NOT EXISTS `user_sessions_61_shadow` LIKE `user_sessions_61`;
CREATE TABLE IF NOT EXISTS `user_roles_61_shadow` LIKE `user_roles_61`;
CREATE TABLE IF NOT EXISTS `user_accounts_61_shadow` LIKE `user_accounts_61`;
CREATE TABLE IF NOT EXISTS `user_settings_61_shadow` LIKE `user_settings_61`;
CREATE TABLE IF NOT EXISTS `user_card_61_shadow` LIKE `user_card_61`;
CREATE TABLE IF NOT EXISTS `merchants_61_shadow` LIKE `merchants_61`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_61_shadow` LIKE `merchant_kyc_document_61`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_61_shadow` LIKE `merchant_channel_secret_61`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_61_shadow` LIKE `admin_audit_log_61`;

CREATE TABLE IF NOT EXISTS `users_62_shadow` LIKE `users_62`;
CREATE TABLE IF NOT EXISTS `user_profiles_62_shadow` LIKE `user_profiles_62`;
CREATE TABLE IF NOT EXISTS `user_auths_62_shadow` LIKE `user_auths_62`;
CREATE TABLE IF NOT EXISTS `login_logs_62_shadow` LIKE `login_logs_62`;
CREATE TABLE IF NOT EXISTS `user_sessions_62_shadow` LIKE `user_sessions_62`;
CREATE TABLE IF NOT EXISTS `user_roles_62_shadow` LIKE `user_roles_62`;
CREATE TABLE IF NOT EXISTS `user_accounts_62_shadow` LIKE `user_accounts_62`;
CREATE TABLE IF NOT EXISTS `user_settings_62_shadow` LIKE `user_settings_62`;
CREATE TABLE IF NOT EXISTS `user_card_62_shadow` LIKE `user_card_62`;
CREATE TABLE IF NOT EXISTS `merchants_62_shadow` LIKE `merchants_62`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_62_shadow` LIKE `merchant_kyc_document_62`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_62_shadow` LIKE `merchant_channel_secret_62`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_62_shadow` LIKE `admin_audit_log_62`;

CREATE TABLE IF NOT EXISTS `users_63_shadow` LIKE `users_63`;
CREATE TABLE IF NOT EXISTS `user_profiles_63_shadow` LIKE `user_profiles_63`;
CREATE TABLE IF NOT EXISTS `user_auths_63_shadow` LIKE `user_auths_63`;
CREATE TABLE IF NOT EXISTS `login_logs_63_shadow` LIKE `login_logs_63`;
CREATE TABLE IF NOT EXISTS `user_sessions_63_shadow` LIKE `user_sessions_63`;
CREATE TABLE IF NOT EXISTS `user_roles_63_shadow` LIKE `user_roles_63`;
CREATE TABLE IF NOT EXISTS `user_accounts_63_shadow` LIKE `user_accounts_63`;
CREATE TABLE IF NOT EXISTS `user_settings_63_shadow` LIKE `user_settings_63`;
CREATE TABLE IF NOT EXISTS `user_card_63_shadow` LIKE `user_card_63`;
CREATE TABLE IF NOT EXISTS `merchants_63_shadow` LIKE `merchants_63`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_63_shadow` LIKE `merchant_kyc_document_63`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_63_shadow` LIKE `merchant_channel_secret_63`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_63_shadow` LIKE `admin_audit_log_63`;

CREATE TABLE IF NOT EXISTS `users_64_shadow` LIKE `users_64`;
CREATE TABLE IF NOT EXISTS `user_profiles_64_shadow` LIKE `user_profiles_64`;
CREATE TABLE IF NOT EXISTS `user_auths_64_shadow` LIKE `user_auths_64`;
CREATE TABLE IF NOT EXISTS `login_logs_64_shadow` LIKE `login_logs_64`;
CREATE TABLE IF NOT EXISTS `user_sessions_64_shadow` LIKE `user_sessions_64`;
CREATE TABLE IF NOT EXISTS `user_roles_64_shadow` LIKE `user_roles_64`;
CREATE TABLE IF NOT EXISTS `user_accounts_64_shadow` LIKE `user_accounts_64`;
CREATE TABLE IF NOT EXISTS `user_settings_64_shadow` LIKE `user_settings_64`;
CREATE TABLE IF NOT EXISTS `user_card_64_shadow` LIKE `user_card_64`;
CREATE TABLE IF NOT EXISTS `merchants_64_shadow` LIKE `merchants_64`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_64_shadow` LIKE `merchant_kyc_document_64`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_64_shadow` LIKE `merchant_channel_secret_64`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_64_shadow` LIKE `admin_audit_log_64`;

CREATE TABLE IF NOT EXISTS `users_65_shadow` LIKE `users_65`;
CREATE TABLE IF NOT EXISTS `user_profiles_65_shadow` LIKE `user_profiles_65`;
CREATE TABLE IF NOT EXISTS `user_auths_65_shadow` LIKE `user_auths_65`;
CREATE TABLE IF NOT EXISTS `login_logs_65_shadow` LIKE `login_logs_65`;
CREATE TABLE IF NOT EXISTS `user_sessions_65_shadow` LIKE `user_sessions_65`;
CREATE TABLE IF NOT EXISTS `user_roles_65_shadow` LIKE `user_roles_65`;
CREATE TABLE IF NOT EXISTS `user_accounts_65_shadow` LIKE `user_accounts_65`;
CREATE TABLE IF NOT EXISTS `user_settings_65_shadow` LIKE `user_settings_65`;
CREATE TABLE IF NOT EXISTS `user_card_65_shadow` LIKE `user_card_65`;
CREATE TABLE IF NOT EXISTS `merchants_65_shadow` LIKE `merchants_65`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_65_shadow` LIKE `merchant_kyc_document_65`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_65_shadow` LIKE `merchant_channel_secret_65`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_65_shadow` LIKE `admin_audit_log_65`;

CREATE TABLE IF NOT EXISTS `users_66_shadow` LIKE `users_66`;
CREATE TABLE IF NOT EXISTS `user_profiles_66_shadow` LIKE `user_profiles_66`;
CREATE TABLE IF NOT EXISTS `user_auths_66_shadow` LIKE `user_auths_66`;
CREATE TABLE IF NOT EXISTS `login_logs_66_shadow` LIKE `login_logs_66`;
CREATE TABLE IF NOT EXISTS `user_sessions_66_shadow` LIKE `user_sessions_66`;
CREATE TABLE IF NOT EXISTS `user_roles_66_shadow` LIKE `user_roles_66`;
CREATE TABLE IF NOT EXISTS `user_accounts_66_shadow` LIKE `user_accounts_66`;
CREATE TABLE IF NOT EXISTS `user_settings_66_shadow` LIKE `user_settings_66`;
CREATE TABLE IF NOT EXISTS `user_card_66_shadow` LIKE `user_card_66`;
CREATE TABLE IF NOT EXISTS `merchants_66_shadow` LIKE `merchants_66`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_66_shadow` LIKE `merchant_kyc_document_66`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_66_shadow` LIKE `merchant_channel_secret_66`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_66_shadow` LIKE `admin_audit_log_66`;

CREATE TABLE IF NOT EXISTS `users_67_shadow` LIKE `users_67`;
CREATE TABLE IF NOT EXISTS `user_profiles_67_shadow` LIKE `user_profiles_67`;
CREATE TABLE IF NOT EXISTS `user_auths_67_shadow` LIKE `user_auths_67`;
CREATE TABLE IF NOT EXISTS `login_logs_67_shadow` LIKE `login_logs_67`;
CREATE TABLE IF NOT EXISTS `user_sessions_67_shadow` LIKE `user_sessions_67`;
CREATE TABLE IF NOT EXISTS `user_roles_67_shadow` LIKE `user_roles_67`;
CREATE TABLE IF NOT EXISTS `user_accounts_67_shadow` LIKE `user_accounts_67`;
CREATE TABLE IF NOT EXISTS `user_settings_67_shadow` LIKE `user_settings_67`;
CREATE TABLE IF NOT EXISTS `user_card_67_shadow` LIKE `user_card_67`;
CREATE TABLE IF NOT EXISTS `merchants_67_shadow` LIKE `merchants_67`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_67_shadow` LIKE `merchant_kyc_document_67`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_67_shadow` LIKE `merchant_channel_secret_67`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_67_shadow` LIKE `admin_audit_log_67`;

CREATE TABLE IF NOT EXISTS `users_68_shadow` LIKE `users_68`;
CREATE TABLE IF NOT EXISTS `user_profiles_68_shadow` LIKE `user_profiles_68`;
CREATE TABLE IF NOT EXISTS `user_auths_68_shadow` LIKE `user_auths_68`;
CREATE TABLE IF NOT EXISTS `login_logs_68_shadow` LIKE `login_logs_68`;
CREATE TABLE IF NOT EXISTS `user_sessions_68_shadow` LIKE `user_sessions_68`;
CREATE TABLE IF NOT EXISTS `user_roles_68_shadow` LIKE `user_roles_68`;
CREATE TABLE IF NOT EXISTS `user_accounts_68_shadow` LIKE `user_accounts_68`;
CREATE TABLE IF NOT EXISTS `user_settings_68_shadow` LIKE `user_settings_68`;
CREATE TABLE IF NOT EXISTS `user_card_68_shadow` LIKE `user_card_68`;
CREATE TABLE IF NOT EXISTS `merchants_68_shadow` LIKE `merchants_68`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_68_shadow` LIKE `merchant_kyc_document_68`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_68_shadow` LIKE `merchant_channel_secret_68`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_68_shadow` LIKE `admin_audit_log_68`;

CREATE TABLE IF NOT EXISTS `users_69_shadow` LIKE `users_69`;
CREATE TABLE IF NOT EXISTS `user_profiles_69_shadow` LIKE `user_profiles_69`;
CREATE TABLE IF NOT EXISTS `user_auths_69_shadow` LIKE `user_auths_69`;
CREATE TABLE IF NOT EXISTS `login_logs_69_shadow` LIKE `login_logs_69`;
CREATE TABLE IF NOT EXISTS `user_sessions_69_shadow` LIKE `user_sessions_69`;
CREATE TABLE IF NOT EXISTS `user_roles_69_shadow` LIKE `user_roles_69`;
CREATE TABLE IF NOT EXISTS `user_accounts_69_shadow` LIKE `user_accounts_69`;
CREATE TABLE IF NOT EXISTS `user_settings_69_shadow` LIKE `user_settings_69`;
CREATE TABLE IF NOT EXISTS `user_card_69_shadow` LIKE `user_card_69`;
CREATE TABLE IF NOT EXISTS `merchants_69_shadow` LIKE `merchants_69`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_69_shadow` LIKE `merchant_kyc_document_69`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_69_shadow` LIKE `merchant_channel_secret_69`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_69_shadow` LIKE `admin_audit_log_69`;


-- ==== card-center ====
SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `card_center_db_6` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_center_db_6`;

-- card-center 分片表模板。6 = 0..9，60 = 00..99（globalTblIdx）。
-- generate.sh 把所有 6 / 60 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_6`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_60` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_60` (
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
CREATE TABLE IF NOT EXISTS `audit_log_60` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 60 (PCI 10.7 7y retention)';

-- card-center 分片表模板。6 = 0..9，61 = 00..99（globalTblIdx）。
-- generate.sh 把所有 6 / 61 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_6`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_61` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_61` (
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
CREATE TABLE IF NOT EXISTS `audit_log_61` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 61 (PCI 10.7 7y retention)';

-- card-center 分片表模板。6 = 0..9，62 = 00..99（globalTblIdx）。
-- generate.sh 把所有 6 / 62 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_6`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_62` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_62` (
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
CREATE TABLE IF NOT EXISTS `audit_log_62` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 62 (PCI 10.7 7y retention)';

-- card-center 分片表模板。6 = 0..9，63 = 00..99（globalTblIdx）。
-- generate.sh 把所有 6 / 63 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_6`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_63` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_63` (
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
CREATE TABLE IF NOT EXISTS `audit_log_63` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 63 (PCI 10.7 7y retention)';

-- card-center 分片表模板。6 = 0..9，64 = 00..99（globalTblIdx）。
-- generate.sh 把所有 6 / 64 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_6`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_64` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_64` (
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
CREATE TABLE IF NOT EXISTS `audit_log_64` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 64 (PCI 10.7 7y retention)';

-- card-center 分片表模板。6 = 0..9，65 = 00..99（globalTblIdx）。
-- generate.sh 把所有 6 / 65 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_6`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_65` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_65` (
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
CREATE TABLE IF NOT EXISTS `audit_log_65` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 65 (PCI 10.7 7y retention)';

-- card-center 分片表模板。6 = 0..9，66 = 00..99（globalTblIdx）。
-- generate.sh 把所有 6 / 66 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_6`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_66` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_66` (
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
CREATE TABLE IF NOT EXISTS `audit_log_66` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 66 (PCI 10.7 7y retention)';

-- card-center 分片表模板。6 = 0..9，67 = 00..99（globalTblIdx）。
-- generate.sh 把所有 6 / 67 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_6`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_67` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_67` (
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
CREATE TABLE IF NOT EXISTS `audit_log_67` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 67 (PCI 10.7 7y retention)';

-- card-center 分片表模板。6 = 0..9，68 = 00..99（globalTblIdx）。
-- generate.sh 把所有 6 / 68 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_6`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_68` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_68` (
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
CREATE TABLE IF NOT EXISTS `audit_log_68` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 68 (PCI 10.7 7y retention)';

-- card-center 分片表模板。6 = 0..9，69 = 00..99（globalTblIdx）。
-- generate.sh 把所有 6 / 69 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_6`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_69` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_69` (
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
CREATE TABLE IF NOT EXISTS `audit_log_69` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 69 (PCI 10.7 7y retention)';


-- ==== card-payment ====
SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `card_payment_db_6` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_payment_db_6`;

-- card-payment 分片表模板。6 = 0..9，60 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_6`;

CREATE TABLE IF NOT EXISTS `card_transaction_60` (
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

-- card-payment 分片表模板。6 = 0..9，61 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_6`;

CREATE TABLE IF NOT EXISTS `card_transaction_61` (
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

-- card-payment 分片表模板。6 = 0..9，62 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_6`;

CREATE TABLE IF NOT EXISTS `card_transaction_62` (
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

-- card-payment 分片表模板。6 = 0..9，63 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_6`;

CREATE TABLE IF NOT EXISTS `card_transaction_63` (
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

-- card-payment 分片表模板。6 = 0..9，64 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_6`;

CREATE TABLE IF NOT EXISTS `card_transaction_64` (
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

-- card-payment 分片表模板。6 = 0..9，65 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_6`;

CREATE TABLE IF NOT EXISTS `card_transaction_65` (
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

-- card-payment 分片表模板。6 = 0..9，66 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_6`;

CREATE TABLE IF NOT EXISTS `card_transaction_66` (
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

-- card-payment 分片表模板。6 = 0..9，67 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_6`;

CREATE TABLE IF NOT EXISTS `card_transaction_67` (
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

-- card-payment 分片表模板。6 = 0..9，68 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_6`;

CREATE TABLE IF NOT EXISTS `card_transaction_68` (
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

-- card-payment 分片表模板。6 = 0..9，69 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_6`;

CREATE TABLE IF NOT EXISTS `card_transaction_69` (
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
-- card_center_db_6 shadow 表（压测）
USE `card_center_db_6`;

CREATE TABLE IF NOT EXISTS `card_stored_token_60_shadow` LIKE `card_stored_token_60`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_60_shadow` LIKE `card_payment_token_used_60`;
CREATE TABLE IF NOT EXISTS `audit_log_60_shadow` LIKE `audit_log_60`;

CREATE TABLE IF NOT EXISTS `card_stored_token_61_shadow` LIKE `card_stored_token_61`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_61_shadow` LIKE `card_payment_token_used_61`;
CREATE TABLE IF NOT EXISTS `audit_log_61_shadow` LIKE `audit_log_61`;

CREATE TABLE IF NOT EXISTS `card_stored_token_62_shadow` LIKE `card_stored_token_62`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_62_shadow` LIKE `card_payment_token_used_62`;
CREATE TABLE IF NOT EXISTS `audit_log_62_shadow` LIKE `audit_log_62`;

CREATE TABLE IF NOT EXISTS `card_stored_token_63_shadow` LIKE `card_stored_token_63`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_63_shadow` LIKE `card_payment_token_used_63`;
CREATE TABLE IF NOT EXISTS `audit_log_63_shadow` LIKE `audit_log_63`;

CREATE TABLE IF NOT EXISTS `card_stored_token_64_shadow` LIKE `card_stored_token_64`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_64_shadow` LIKE `card_payment_token_used_64`;
CREATE TABLE IF NOT EXISTS `audit_log_64_shadow` LIKE `audit_log_64`;

CREATE TABLE IF NOT EXISTS `card_stored_token_65_shadow` LIKE `card_stored_token_65`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_65_shadow` LIKE `card_payment_token_used_65`;
CREATE TABLE IF NOT EXISTS `audit_log_65_shadow` LIKE `audit_log_65`;

CREATE TABLE IF NOT EXISTS `card_stored_token_66_shadow` LIKE `card_stored_token_66`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_66_shadow` LIKE `card_payment_token_used_66`;
CREATE TABLE IF NOT EXISTS `audit_log_66_shadow` LIKE `audit_log_66`;

CREATE TABLE IF NOT EXISTS `card_stored_token_67_shadow` LIKE `card_stored_token_67`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_67_shadow` LIKE `card_payment_token_used_67`;
CREATE TABLE IF NOT EXISTS `audit_log_67_shadow` LIKE `audit_log_67`;

CREATE TABLE IF NOT EXISTS `card_stored_token_68_shadow` LIKE `card_stored_token_68`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_68_shadow` LIKE `card_payment_token_used_68`;
CREATE TABLE IF NOT EXISTS `audit_log_68_shadow` LIKE `audit_log_68`;

CREATE TABLE IF NOT EXISTS `card_stored_token_69_shadow` LIKE `card_stored_token_69`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_69_shadow` LIKE `card_payment_token_used_69`;
CREATE TABLE IF NOT EXISTS `audit_log_69_shadow` LIKE `audit_log_69`;


-- ==== card-payment _shadow ====
-- card_payment_db_6 shadow
USE `card_payment_db_6`;

CREATE TABLE IF NOT EXISTS `card_transaction_60_shadow` LIKE `card_transaction_60`;

CREATE TABLE IF NOT EXISTS `card_transaction_61_shadow` LIKE `card_transaction_61`;

CREATE TABLE IF NOT EXISTS `card_transaction_62_shadow` LIKE `card_transaction_62`;

CREATE TABLE IF NOT EXISTS `card_transaction_63_shadow` LIKE `card_transaction_63`;

CREATE TABLE IF NOT EXISTS `card_transaction_64_shadow` LIKE `card_transaction_64`;

CREATE TABLE IF NOT EXISTS `card_transaction_65_shadow` LIKE `card_transaction_65`;

CREATE TABLE IF NOT EXISTS `card_transaction_66_shadow` LIKE `card_transaction_66`;

CREATE TABLE IF NOT EXISTS `card_transaction_67_shadow` LIKE `card_transaction_67`;

CREATE TABLE IF NOT EXISTS `card_transaction_68_shadow` LIKE `card_transaction_68`;

CREATE TABLE IF NOT EXISTS `card_transaction_69_shadow` LIKE `card_transaction_69`;


-- ==== split-payment shardb (split_payment_db_6) ====
-- ============================================================================
-- split_payment_db_6 —— 高频事件流水分片库（DB-split Batch 7 极简版）
-- ⚠ 模板文件。由 ../gen.sh 用 sed 替换 6 + 注入 tables block 生成 N_init.sql。
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

CREATE DATABASE IF NOT EXISTS split_payment_db_6 CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

-- split_user 在 meta init.sql 里也建一遍；但 shared-db 把每个 shard SQL 灌到不同
-- mysql 实例（shared-shard-N），mysql user 是 per-instance 的不跨实例共享，所以
-- 每个 shard 自己也必须 CREATE USER。IF NOT EXISTS 幂等。
CREATE USER IF NOT EXISTS 'split_user'@'%' IDENTIFIED BY 'password';
ALTER USER 'split_user'@'%' IDENTIFIED BY 'password';

GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, DROP, REFERENCES
  ON split_payment_db_6.* TO 'split_user'@'%';
FLUSH PRIVILEGES;

USE split_payment_db_6;
-- ─── moneyflow_event (10 张主表 + 10 张 _shadow 镜像) ──────────────────────

CREATE TABLE IF NOT EXISTS moneyflow_event_60 (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_60_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_61 (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_61_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_62 (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_62_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_63 (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_63_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_64 (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_64_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_65 (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_65_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_66 (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_66_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_67 (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_67_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_68 (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_68_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_69 (
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

CREATE TABLE IF NOT EXISTS moneyflow_event_69_shadow (
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
