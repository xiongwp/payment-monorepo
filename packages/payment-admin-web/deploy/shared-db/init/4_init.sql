-- ┌──────────────────────────────────────────────────────────────────────┐
-- │ 共享 MySQL shard 4 —— paychan_db_4 + order_db_4 +
-- │ accounting_db_4 + user_merchant_db_4                            │
-- └──────────────────────────────────────────────────────────────────────┘

-- ==== payment-channel ====
CREATE DATABASE IF NOT EXISTS `paychan_db_4` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `paychan_db_4`;

-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 40 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_40` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_40` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_40` (
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
CREATE TABLE IF NOT EXISTS `channel_token_40` (
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
-- apply.sh 把 41 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_41` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_41` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_41` (
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
CREATE TABLE IF NOT EXISTS `channel_token_41` (
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
-- apply.sh 把 42 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_42` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_42` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_42` (
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
CREATE TABLE IF NOT EXISTS `channel_token_42` (
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
-- apply.sh 把 43 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_43` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_43` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_43` (
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
CREATE TABLE IF NOT EXISTS `channel_token_43` (
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
-- apply.sh 把 44 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_44` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_44` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_44` (
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
CREATE TABLE IF NOT EXISTS `channel_token_44` (
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
-- apply.sh 把 45 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_45` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_45` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_45` (
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
CREATE TABLE IF NOT EXISTS `channel_token_45` (
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
-- apply.sh 把 46 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_46` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_46` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_46` (
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
CREATE TABLE IF NOT EXISTS `channel_token_46` (
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
-- apply.sh 把 47 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_47` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_47` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_47` (
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
CREATE TABLE IF NOT EXISTS `channel_token_47` (
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
-- apply.sh 把 48 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_48` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_48` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_48` (
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
CREATE TABLE IF NOT EXISTS `channel_token_48` (
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
-- apply.sh 把 49 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_49` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_49` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_49` (
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
CREATE TABLE IF NOT EXISTS `channel_token_49` (
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
CREATE DATABASE IF NOT EXISTS `order_db_4` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `order_db_4`;

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：4 = 0-9 物理库；40 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_40
CREATE TABLE IF NOT EXISTS `payment_intent_40` (
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

-- 2. Charge 表 charge_40（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_40` (
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

-- 3. PayAction 表 pay_action_40（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_40` (
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

-- 4. ExceptionCase 表 exception_case_40（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_40` (
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

-- 5. NotifyLog 表 notify_log_40（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_40` (
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

-- 5. Refund 表 refund_40（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_40` (
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

-- 7. InboundWebhook 表 inbound_webhook_40
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_40` (
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

-- 8. Dispute 表 dispute_40
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_40` (
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

-- 9. DisputeEvent 表 dispute_event_40（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_40` (
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

-- 10. AccountingOutbox 表 accounting_outbox_40
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_40` (
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

-- ─── admin_audit_log_40：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_40` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 40';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：4 = 0-9 物理库；41 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_41
CREATE TABLE IF NOT EXISTS `payment_intent_41` (
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

-- 2. Charge 表 charge_41（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_41` (
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

-- 3. PayAction 表 pay_action_41（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_41` (
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

-- 4. ExceptionCase 表 exception_case_41（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_41` (
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

-- 5. NotifyLog 表 notify_log_41（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_41` (
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

-- 5. Refund 表 refund_41（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_41` (
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

-- 7. InboundWebhook 表 inbound_webhook_41
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_41` (
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

-- 8. Dispute 表 dispute_41
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_41` (
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

-- 9. DisputeEvent 表 dispute_event_41（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_41` (
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

-- 10. AccountingOutbox 表 accounting_outbox_41
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_41` (
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

-- ─── admin_audit_log_41：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_41` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 41';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：4 = 0-9 物理库；42 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_42
CREATE TABLE IF NOT EXISTS `payment_intent_42` (
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

-- 2. Charge 表 charge_42（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_42` (
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

-- 3. PayAction 表 pay_action_42（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_42` (
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

-- 4. ExceptionCase 表 exception_case_42（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_42` (
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

-- 5. NotifyLog 表 notify_log_42（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_42` (
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

-- 5. Refund 表 refund_42（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_42` (
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

-- 7. InboundWebhook 表 inbound_webhook_42
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_42` (
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

-- 8. Dispute 表 dispute_42
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_42` (
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

-- 9. DisputeEvent 表 dispute_event_42（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_42` (
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

-- 10. AccountingOutbox 表 accounting_outbox_42
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_42` (
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

-- ─── admin_audit_log_42：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_42` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 42';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：4 = 0-9 物理库；43 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_43
CREATE TABLE IF NOT EXISTS `payment_intent_43` (
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

-- 2. Charge 表 charge_43（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_43` (
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

-- 3. PayAction 表 pay_action_43（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_43` (
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

-- 4. ExceptionCase 表 exception_case_43（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_43` (
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

-- 5. NotifyLog 表 notify_log_43（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_43` (
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

-- 5. Refund 表 refund_43（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_43` (
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

-- 7. InboundWebhook 表 inbound_webhook_43
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_43` (
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

-- 8. Dispute 表 dispute_43
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_43` (
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

-- 9. DisputeEvent 表 dispute_event_43（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_43` (
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

-- 10. AccountingOutbox 表 accounting_outbox_43
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_43` (
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

-- ─── admin_audit_log_43：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_43` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 43';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：4 = 0-9 物理库；44 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_44
CREATE TABLE IF NOT EXISTS `payment_intent_44` (
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

-- 2. Charge 表 charge_44（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_44` (
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

-- 3. PayAction 表 pay_action_44（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_44` (
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

-- 4. ExceptionCase 表 exception_case_44（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_44` (
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

-- 5. NotifyLog 表 notify_log_44（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_44` (
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

-- 5. Refund 表 refund_44（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_44` (
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

-- 7. InboundWebhook 表 inbound_webhook_44
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_44` (
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

-- 8. Dispute 表 dispute_44
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_44` (
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

-- 9. DisputeEvent 表 dispute_event_44（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_44` (
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

-- 10. AccountingOutbox 表 accounting_outbox_44
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_44` (
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

-- ─── admin_audit_log_44：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_44` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 44';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：4 = 0-9 物理库；45 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_45
CREATE TABLE IF NOT EXISTS `payment_intent_45` (
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

-- 2. Charge 表 charge_45（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_45` (
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

-- 3. PayAction 表 pay_action_45（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_45` (
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

-- 4. ExceptionCase 表 exception_case_45（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_45` (
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

-- 5. NotifyLog 表 notify_log_45（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_45` (
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

-- 5. Refund 表 refund_45（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_45` (
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

-- 7. InboundWebhook 表 inbound_webhook_45
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_45` (
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

-- 8. Dispute 表 dispute_45
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_45` (
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

-- 9. DisputeEvent 表 dispute_event_45（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_45` (
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

-- 10. AccountingOutbox 表 accounting_outbox_45
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_45` (
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

-- ─── admin_audit_log_45：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_45` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 45';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：4 = 0-9 物理库；46 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_46
CREATE TABLE IF NOT EXISTS `payment_intent_46` (
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

-- 2. Charge 表 charge_46（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_46` (
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

-- 3. PayAction 表 pay_action_46（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_46` (
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

-- 4. ExceptionCase 表 exception_case_46（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_46` (
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

-- 5. NotifyLog 表 notify_log_46（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_46` (
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

-- 5. Refund 表 refund_46（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_46` (
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

-- 7. InboundWebhook 表 inbound_webhook_46
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_46` (
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

-- 8. Dispute 表 dispute_46
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_46` (
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

-- 9. DisputeEvent 表 dispute_event_46（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_46` (
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

-- 10. AccountingOutbox 表 accounting_outbox_46
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_46` (
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

-- ─── admin_audit_log_46：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_46` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 46';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：4 = 0-9 物理库；47 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_47
CREATE TABLE IF NOT EXISTS `payment_intent_47` (
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

-- 2. Charge 表 charge_47（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_47` (
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

-- 3. PayAction 表 pay_action_47（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_47` (
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

-- 4. ExceptionCase 表 exception_case_47（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_47` (
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

-- 5. NotifyLog 表 notify_log_47（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_47` (
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

-- 5. Refund 表 refund_47（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_47` (
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

-- 7. InboundWebhook 表 inbound_webhook_47
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_47` (
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

-- 8. Dispute 表 dispute_47
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_47` (
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

-- 9. DisputeEvent 表 dispute_event_47（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_47` (
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

-- 10. AccountingOutbox 表 accounting_outbox_47
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_47` (
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

-- ─── admin_audit_log_47：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_47` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 47';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：4 = 0-9 物理库；48 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_48
CREATE TABLE IF NOT EXISTS `payment_intent_48` (
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

-- 2. Charge 表 charge_48（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_48` (
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

-- 3. PayAction 表 pay_action_48（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_48` (
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

-- 4. ExceptionCase 表 exception_case_48（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_48` (
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

-- 5. NotifyLog 表 notify_log_48（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_48` (
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

-- 5. Refund 表 refund_48（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_48` (
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

-- 7. InboundWebhook 表 inbound_webhook_48
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_48` (
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

-- 8. Dispute 表 dispute_48
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_48` (
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

-- 9. DisputeEvent 表 dispute_event_48（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_48` (
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

-- 10. AccountingOutbox 表 accounting_outbox_48
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_48` (
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

-- ─── admin_audit_log_48：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_48` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 48';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：4 = 0-9 物理库；49 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_49
CREATE TABLE IF NOT EXISTS `payment_intent_49` (
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

-- 2. Charge 表 charge_49（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_49` (
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

-- 3. PayAction 表 pay_action_49（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_49` (
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

-- 4. ExceptionCase 表 exception_case_49（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_49` (
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

-- 5. NotifyLog 表 notify_log_49（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_49` (
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

-- 5. Refund 表 refund_49（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_49` (
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

-- 7. InboundWebhook 表 inbound_webhook_49
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_49` (
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

-- 8. Dispute 表 dispute_49
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_49` (
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

-- 9. DisputeEvent 表 dispute_event_49（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_49` (
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

-- 10. AccountingOutbox 表 accounting_outbox_49
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_49` (
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

-- ─── admin_audit_log_49：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_49` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 49';


-- ==== accounting-system schema ====
SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS `accounting_db_4` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `accounting_db_4`;

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


CREATE TABLE IF NOT EXISTS `account_40` (
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
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_40` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_40` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_40` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_40` (
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
CREATE TABLE IF NOT EXISTS `async_task_40` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_40` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_40` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_40` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_40` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_40` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_40` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_40` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_40` (
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
CREATE TABLE IF NOT EXISTS `batch_order_40` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_40` (
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


CREATE TABLE IF NOT EXISTS `account_41` (
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
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_41` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_41` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_41` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_41` (
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
CREATE TABLE IF NOT EXISTS `async_task_41` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_41` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_41` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_41` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_41` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_41` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_41` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_41` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_41` (
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
CREATE TABLE IF NOT EXISTS `batch_order_41` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_41` (
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


CREATE TABLE IF NOT EXISTS `account_42` (
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
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_42` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_42` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_42` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_42` (
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
CREATE TABLE IF NOT EXISTS `async_task_42` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_42` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_42` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_42` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_42` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_42` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_42` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_42` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_42` (
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
CREATE TABLE IF NOT EXISTS `batch_order_42` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_42` (
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


CREATE TABLE IF NOT EXISTS `account_43` (
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
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_43` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_43` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_43` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_43` (
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
CREATE TABLE IF NOT EXISTS `async_task_43` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_43` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_43` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_43` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_43` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_43` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_43` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_43` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_43` (
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
CREATE TABLE IF NOT EXISTS `batch_order_43` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_43` (
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


CREATE TABLE IF NOT EXISTS `account_44` (
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
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_44` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_44` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_44` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_44` (
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
CREATE TABLE IF NOT EXISTS `async_task_44` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_44` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_44` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_44` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_44` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_44` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_44` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_44` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_44` (
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
CREATE TABLE IF NOT EXISTS `batch_order_44` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_44` (
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


CREATE TABLE IF NOT EXISTS `account_45` (
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
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_45` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_45` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_45` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_45` (
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
CREATE TABLE IF NOT EXISTS `async_task_45` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_45` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_45` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_45` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_45` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_45` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_45` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_45` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_45` (
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
CREATE TABLE IF NOT EXISTS `batch_order_45` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_45` (
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


CREATE TABLE IF NOT EXISTS `account_46` (
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
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_46` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_46` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_46` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_46` (
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
CREATE TABLE IF NOT EXISTS `async_task_46` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_46` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_46` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_46` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_46` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_46` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_46` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_46` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_46` (
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
CREATE TABLE IF NOT EXISTS `batch_order_46` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_46` (
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


CREATE TABLE IF NOT EXISTS `account_47` (
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
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_47` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_47` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_47` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_47` (
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
CREATE TABLE IF NOT EXISTS `async_task_47` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_47` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_47` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_47` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_47` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_47` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_47` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_47` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_47` (
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
CREATE TABLE IF NOT EXISTS `batch_order_47` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_47` (
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


CREATE TABLE IF NOT EXISTS `account_48` (
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
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_48` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_48` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_48` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_48` (
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
CREATE TABLE IF NOT EXISTS `async_task_48` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_48` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_48` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_48` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_48` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_48` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_48` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_48` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_48` (
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
CREATE TABLE IF NOT EXISTS `batch_order_48` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_48` (
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


CREATE TABLE IF NOT EXISTS `account_49` (
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
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_account_no` (`account_no`),
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
    KEY `idx_user_id` (`user_id`),
    KEY `idx_account_type` (`account_type`),
    KEY `idx_type_status` (`account_type`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户主表';

-- ============================================
-- 2. 账户流水表 (account_transaction_xxx)
-- 分库分表：按 account_no 前3字符（{db:1d}{table:02d}）路由
-- ============================================
CREATE TABLE IF NOT EXISTS `account_transaction_49` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_49` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_49` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_49` (
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
CREATE TABLE IF NOT EXISTS `async_task_49` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_49` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_49` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_49` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_49` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_49` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_49` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_49` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_49` (
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
CREATE TABLE IF NOT EXISTS `batch_order_49` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_49` (
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


-- ==== accounting-system 平台账户 seed (按位编码 account_no) ====
SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;
USE `accounting_db_4`;

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 40 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (40)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (40)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_40` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 40 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 40, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_40` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 40 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 40, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_40` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 40 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 40, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_40` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 40 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 40, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_40` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 40 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 40, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_40` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 40 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 40, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 41 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (41)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (41)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_41` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 41 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 41, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_41` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 41 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 41, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_41` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 41 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 41, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_41` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 41 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 41, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_41` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 41 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 41, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_41` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 41 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 41, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 42 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (42)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (42)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_42` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 42 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 42, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_42` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 42 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 42, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_42` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 42 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 42, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_42` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 42 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 42, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_42` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 42 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 42, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_42` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 42 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 42, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 43 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (43)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (43)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_43` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 43 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 43, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_43` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 43 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 43, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_43` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 43 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 43, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_43` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 43 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 43, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_43` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 43 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 43, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_43` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 43 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 43, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 44 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (44)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (44)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_44` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 44 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 44, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_44` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 44 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 44, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_44` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 44 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 44, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_44` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 44 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 44, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_44` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 44 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 44, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_44` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 44 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 44, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 45 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (45)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (45)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_45` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 45 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 45, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_45` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 45 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 45, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_45` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 45 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 45, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_45` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 45 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 45, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_45` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 45 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 45, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_45` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 45 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 45, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 46 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (46)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (46)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_46` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 46 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 46, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_46` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 46 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 46, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_46` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 46 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 46, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_46` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 46 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 46, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_46` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 46 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 46, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_46` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 46 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 46, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 47 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (47)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (47)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_47` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 47 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 47, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_47` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 47 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 47, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_47` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 47 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 47, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_47` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 47 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 47, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_47` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 47 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 47, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_47` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 47 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 47, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 48 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (48)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (48)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_48` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 48 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 48, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_48` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 48 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 48, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_48` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 48 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 48, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_48` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 48 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 48, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_48` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 48 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 48, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_48` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 48 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 48, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 49 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (49)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (49)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_49` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 49 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 49, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_49` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 49 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 49, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_49` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 49 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 49, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_49` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 49 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 49, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_49` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 49 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 49, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_49` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 49 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 49, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);


-- ==== user-merchant-core ====
SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `user_merchant_db_4` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `user_merchant_db_4`;

-- 分片表 schema 模板。4 和 40 由 generate.sh 替换：
--   4     = 0..9         分库 idx
--   40  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_40` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 40)';

CREATE TABLE IF NOT EXISTS `user_profiles_40` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 40)';

CREATE TABLE IF NOT EXISTS `user_auths_40` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 40)';

CREATE TABLE IF NOT EXISTS `login_logs_40` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 40)';

CREATE TABLE IF NOT EXISTS `user_sessions_40` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 40)';

CREATE TABLE IF NOT EXISTS `user_roles_40` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 40)';

CREATE TABLE IF NOT EXISTS `user_accounts_40` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 40)';

CREATE TABLE IF NOT EXISTS `user_settings_40` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 40)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_40` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 40)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_40` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 40)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_40` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 40)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_40` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 40)';

-- ─── admin_audit_log_40：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_40` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 40';

-- 分片表 schema 模板。4 和 41 由 generate.sh 替换：
--   4     = 0..9         分库 idx
--   41  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_41` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 41)';

CREATE TABLE IF NOT EXISTS `user_profiles_41` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 41)';

CREATE TABLE IF NOT EXISTS `user_auths_41` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 41)';

CREATE TABLE IF NOT EXISTS `login_logs_41` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 41)';

CREATE TABLE IF NOT EXISTS `user_sessions_41` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 41)';

CREATE TABLE IF NOT EXISTS `user_roles_41` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 41)';

CREATE TABLE IF NOT EXISTS `user_accounts_41` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 41)';

CREATE TABLE IF NOT EXISTS `user_settings_41` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 41)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_41` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 41)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_41` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 41)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_41` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 41)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_41` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 41)';

-- ─── admin_audit_log_41：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_41` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 41';

-- 分片表 schema 模板。4 和 42 由 generate.sh 替换：
--   4     = 0..9         分库 idx
--   42  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_42` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 42)';

CREATE TABLE IF NOT EXISTS `user_profiles_42` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 42)';

CREATE TABLE IF NOT EXISTS `user_auths_42` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 42)';

CREATE TABLE IF NOT EXISTS `login_logs_42` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 42)';

CREATE TABLE IF NOT EXISTS `user_sessions_42` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 42)';

CREATE TABLE IF NOT EXISTS `user_roles_42` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 42)';

CREATE TABLE IF NOT EXISTS `user_accounts_42` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 42)';

CREATE TABLE IF NOT EXISTS `user_settings_42` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 42)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_42` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 42)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_42` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 42)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_42` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 42)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_42` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 42)';

-- ─── admin_audit_log_42：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_42` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 42';

-- 分片表 schema 模板。4 和 43 由 generate.sh 替换：
--   4     = 0..9         分库 idx
--   43  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_43` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 43)';

CREATE TABLE IF NOT EXISTS `user_profiles_43` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 43)';

CREATE TABLE IF NOT EXISTS `user_auths_43` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 43)';

CREATE TABLE IF NOT EXISTS `login_logs_43` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 43)';

CREATE TABLE IF NOT EXISTS `user_sessions_43` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 43)';

CREATE TABLE IF NOT EXISTS `user_roles_43` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 43)';

CREATE TABLE IF NOT EXISTS `user_accounts_43` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 43)';

CREATE TABLE IF NOT EXISTS `user_settings_43` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 43)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_43` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 43)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_43` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 43)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_43` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 43)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_43` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 43)';

-- ─── admin_audit_log_43：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_43` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 43';

-- 分片表 schema 模板。4 和 44 由 generate.sh 替换：
--   4     = 0..9         分库 idx
--   44  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_44` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 44)';

CREATE TABLE IF NOT EXISTS `user_profiles_44` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 44)';

CREATE TABLE IF NOT EXISTS `user_auths_44` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 44)';

CREATE TABLE IF NOT EXISTS `login_logs_44` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 44)';

CREATE TABLE IF NOT EXISTS `user_sessions_44` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 44)';

CREATE TABLE IF NOT EXISTS `user_roles_44` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 44)';

CREATE TABLE IF NOT EXISTS `user_accounts_44` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 44)';

CREATE TABLE IF NOT EXISTS `user_settings_44` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 44)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_44` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 44)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_44` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 44)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_44` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 44)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_44` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 44)';

-- ─── admin_audit_log_44：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_44` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 44';

-- 分片表 schema 模板。4 和 45 由 generate.sh 替换：
--   4     = 0..9         分库 idx
--   45  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_45` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 45)';

CREATE TABLE IF NOT EXISTS `user_profiles_45` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 45)';

CREATE TABLE IF NOT EXISTS `user_auths_45` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 45)';

CREATE TABLE IF NOT EXISTS `login_logs_45` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 45)';

CREATE TABLE IF NOT EXISTS `user_sessions_45` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 45)';

CREATE TABLE IF NOT EXISTS `user_roles_45` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 45)';

CREATE TABLE IF NOT EXISTS `user_accounts_45` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 45)';

CREATE TABLE IF NOT EXISTS `user_settings_45` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 45)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_45` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 45)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_45` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 45)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_45` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 45)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_45` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 45)';

-- ─── admin_audit_log_45：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_45` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 45';

-- 分片表 schema 模板。4 和 46 由 generate.sh 替换：
--   4     = 0..9         分库 idx
--   46  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_46` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 46)';

CREATE TABLE IF NOT EXISTS `user_profiles_46` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 46)';

CREATE TABLE IF NOT EXISTS `user_auths_46` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 46)';

CREATE TABLE IF NOT EXISTS `login_logs_46` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 46)';

CREATE TABLE IF NOT EXISTS `user_sessions_46` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 46)';

CREATE TABLE IF NOT EXISTS `user_roles_46` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 46)';

CREATE TABLE IF NOT EXISTS `user_accounts_46` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 46)';

CREATE TABLE IF NOT EXISTS `user_settings_46` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 46)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_46` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 46)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_46` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 46)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_46` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 46)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_46` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 46)';

-- ─── admin_audit_log_46：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_46` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 46';

-- 分片表 schema 模板。4 和 47 由 generate.sh 替换：
--   4     = 0..9         分库 idx
--   47  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_47` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 47)';

CREATE TABLE IF NOT EXISTS `user_profiles_47` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 47)';

CREATE TABLE IF NOT EXISTS `user_auths_47` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 47)';

CREATE TABLE IF NOT EXISTS `login_logs_47` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 47)';

CREATE TABLE IF NOT EXISTS `user_sessions_47` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 47)';

CREATE TABLE IF NOT EXISTS `user_roles_47` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 47)';

CREATE TABLE IF NOT EXISTS `user_accounts_47` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 47)';

CREATE TABLE IF NOT EXISTS `user_settings_47` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 47)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_47` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 47)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_47` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 47)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_47` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 47)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_47` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 47)';

-- ─── admin_audit_log_47：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_47` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 47';

-- 分片表 schema 模板。4 和 48 由 generate.sh 替换：
--   4     = 0..9         分库 idx
--   48  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_48` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 48)';

CREATE TABLE IF NOT EXISTS `user_profiles_48` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 48)';

CREATE TABLE IF NOT EXISTS `user_auths_48` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 48)';

CREATE TABLE IF NOT EXISTS `login_logs_48` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 48)';

CREATE TABLE IF NOT EXISTS `user_sessions_48` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 48)';

CREATE TABLE IF NOT EXISTS `user_roles_48` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 48)';

CREATE TABLE IF NOT EXISTS `user_accounts_48` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 48)';

CREATE TABLE IF NOT EXISTS `user_settings_48` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 48)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_48` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 48)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_48` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 48)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_48` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 48)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_48` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 48)';

-- ─── admin_audit_log_48：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_48` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 48';

-- 分片表 schema 模板。4 和 49 由 generate.sh 替换：
--   4     = 0..9         分库 idx
--   49  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_49` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 49)';

CREATE TABLE IF NOT EXISTS `user_profiles_49` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 49)';

CREATE TABLE IF NOT EXISTS `user_auths_49` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 49)';

CREATE TABLE IF NOT EXISTS `login_logs_49` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 49)';

CREATE TABLE IF NOT EXISTS `user_sessions_49` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 49)';

CREATE TABLE IF NOT EXISTS `user_roles_49` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 49)';

CREATE TABLE IF NOT EXISTS `user_accounts_49` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 49)';

CREATE TABLE IF NOT EXISTS `user_settings_49` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 49)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_49` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 49)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_49` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 49)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_49` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 49)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_49` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 49)';

-- ─── admin_audit_log_49：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_49` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 49';


-- ==== payment-channel _shadow ====
-- paychan_db_4 的影子表（压测 / shadow 流量）
-- 依赖：4_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_4`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_40_shadow` LIKE `acquirer_tx_40`;
CREATE TABLE IF NOT EXISTS `webhook_raw_40_shadow` LIKE `webhook_raw_40`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_40_shadow` LIKE `webhook_raw_rejected_40`;
CREATE TABLE IF NOT EXISTS `channel_token_40_shadow` LIKE `channel_token_40`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_41_shadow` LIKE `acquirer_tx_41`;
CREATE TABLE IF NOT EXISTS `webhook_raw_41_shadow` LIKE `webhook_raw_41`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_41_shadow` LIKE `webhook_raw_rejected_41`;
CREATE TABLE IF NOT EXISTS `channel_token_41_shadow` LIKE `channel_token_41`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_42_shadow` LIKE `acquirer_tx_42`;
CREATE TABLE IF NOT EXISTS `webhook_raw_42_shadow` LIKE `webhook_raw_42`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_42_shadow` LIKE `webhook_raw_rejected_42`;
CREATE TABLE IF NOT EXISTS `channel_token_42_shadow` LIKE `channel_token_42`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_43_shadow` LIKE `acquirer_tx_43`;
CREATE TABLE IF NOT EXISTS `webhook_raw_43_shadow` LIKE `webhook_raw_43`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_43_shadow` LIKE `webhook_raw_rejected_43`;
CREATE TABLE IF NOT EXISTS `channel_token_43_shadow` LIKE `channel_token_43`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_44_shadow` LIKE `acquirer_tx_44`;
CREATE TABLE IF NOT EXISTS `webhook_raw_44_shadow` LIKE `webhook_raw_44`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_44_shadow` LIKE `webhook_raw_rejected_44`;
CREATE TABLE IF NOT EXISTS `channel_token_44_shadow` LIKE `channel_token_44`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_45_shadow` LIKE `acquirer_tx_45`;
CREATE TABLE IF NOT EXISTS `webhook_raw_45_shadow` LIKE `webhook_raw_45`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_45_shadow` LIKE `webhook_raw_rejected_45`;
CREATE TABLE IF NOT EXISTS `channel_token_45_shadow` LIKE `channel_token_45`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_46_shadow` LIKE `acquirer_tx_46`;
CREATE TABLE IF NOT EXISTS `webhook_raw_46_shadow` LIKE `webhook_raw_46`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_46_shadow` LIKE `webhook_raw_rejected_46`;
CREATE TABLE IF NOT EXISTS `channel_token_46_shadow` LIKE `channel_token_46`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_47_shadow` LIKE `acquirer_tx_47`;
CREATE TABLE IF NOT EXISTS `webhook_raw_47_shadow` LIKE `webhook_raw_47`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_47_shadow` LIKE `webhook_raw_rejected_47`;
CREATE TABLE IF NOT EXISTS `channel_token_47_shadow` LIKE `channel_token_47`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_48_shadow` LIKE `acquirer_tx_48`;
CREATE TABLE IF NOT EXISTS `webhook_raw_48_shadow` LIKE `webhook_raw_48`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_48_shadow` LIKE `webhook_raw_rejected_48`;
CREATE TABLE IF NOT EXISTS `channel_token_48_shadow` LIKE `channel_token_48`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_49_shadow` LIKE `acquirer_tx_49`;
CREATE TABLE IF NOT EXISTS `webhook_raw_49_shadow` LIKE `webhook_raw_49`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_49_shadow` LIKE `webhook_raw_rejected_49`;
CREATE TABLE IF NOT EXISTS `channel_token_49_shadow` LIKE `channel_token_49`;


-- ==== order-core _shadow ====
-- order_db_4 的影子表（压测 / shadow 流量）
-- 依赖：4_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_4`;

CREATE TABLE IF NOT EXISTS `payment_intent_40_shadow` LIKE `payment_intent_40`;
CREATE TABLE IF NOT EXISTS `charge_40_shadow` LIKE `charge_40`;
CREATE TABLE IF NOT EXISTS `refund_40_shadow` LIKE `refund_40`;
CREATE TABLE IF NOT EXISTS `pay_action_40_shadow` LIKE `pay_action_40`;
CREATE TABLE IF NOT EXISTS `dispute_40_shadow` LIKE `dispute_40`;
CREATE TABLE IF NOT EXISTS `dispute_event_40_shadow` LIKE `dispute_event_40`;
CREATE TABLE IF NOT EXISTS `exception_case_40_shadow` LIKE `exception_case_40`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_40_shadow` LIKE `inbound_webhook_40`;
CREATE TABLE IF NOT EXISTS `notify_log_40_shadow` LIKE `notify_log_40`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_40_shadow` LIKE `accounting_outbox_40`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_40_shadow` LIKE `admin_audit_log_40`;

CREATE TABLE IF NOT EXISTS `payment_intent_41_shadow` LIKE `payment_intent_41`;
CREATE TABLE IF NOT EXISTS `charge_41_shadow` LIKE `charge_41`;
CREATE TABLE IF NOT EXISTS `refund_41_shadow` LIKE `refund_41`;
CREATE TABLE IF NOT EXISTS `pay_action_41_shadow` LIKE `pay_action_41`;
CREATE TABLE IF NOT EXISTS `dispute_41_shadow` LIKE `dispute_41`;
CREATE TABLE IF NOT EXISTS `dispute_event_41_shadow` LIKE `dispute_event_41`;
CREATE TABLE IF NOT EXISTS `exception_case_41_shadow` LIKE `exception_case_41`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_41_shadow` LIKE `inbound_webhook_41`;
CREATE TABLE IF NOT EXISTS `notify_log_41_shadow` LIKE `notify_log_41`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_41_shadow` LIKE `accounting_outbox_41`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_41_shadow` LIKE `admin_audit_log_41`;

CREATE TABLE IF NOT EXISTS `payment_intent_42_shadow` LIKE `payment_intent_42`;
CREATE TABLE IF NOT EXISTS `charge_42_shadow` LIKE `charge_42`;
CREATE TABLE IF NOT EXISTS `refund_42_shadow` LIKE `refund_42`;
CREATE TABLE IF NOT EXISTS `pay_action_42_shadow` LIKE `pay_action_42`;
CREATE TABLE IF NOT EXISTS `dispute_42_shadow` LIKE `dispute_42`;
CREATE TABLE IF NOT EXISTS `dispute_event_42_shadow` LIKE `dispute_event_42`;
CREATE TABLE IF NOT EXISTS `exception_case_42_shadow` LIKE `exception_case_42`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_42_shadow` LIKE `inbound_webhook_42`;
CREATE TABLE IF NOT EXISTS `notify_log_42_shadow` LIKE `notify_log_42`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_42_shadow` LIKE `accounting_outbox_42`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_42_shadow` LIKE `admin_audit_log_42`;

CREATE TABLE IF NOT EXISTS `payment_intent_43_shadow` LIKE `payment_intent_43`;
CREATE TABLE IF NOT EXISTS `charge_43_shadow` LIKE `charge_43`;
CREATE TABLE IF NOT EXISTS `refund_43_shadow` LIKE `refund_43`;
CREATE TABLE IF NOT EXISTS `pay_action_43_shadow` LIKE `pay_action_43`;
CREATE TABLE IF NOT EXISTS `dispute_43_shadow` LIKE `dispute_43`;
CREATE TABLE IF NOT EXISTS `dispute_event_43_shadow` LIKE `dispute_event_43`;
CREATE TABLE IF NOT EXISTS `exception_case_43_shadow` LIKE `exception_case_43`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_43_shadow` LIKE `inbound_webhook_43`;
CREATE TABLE IF NOT EXISTS `notify_log_43_shadow` LIKE `notify_log_43`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_43_shadow` LIKE `accounting_outbox_43`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_43_shadow` LIKE `admin_audit_log_43`;

CREATE TABLE IF NOT EXISTS `payment_intent_44_shadow` LIKE `payment_intent_44`;
CREATE TABLE IF NOT EXISTS `charge_44_shadow` LIKE `charge_44`;
CREATE TABLE IF NOT EXISTS `refund_44_shadow` LIKE `refund_44`;
CREATE TABLE IF NOT EXISTS `pay_action_44_shadow` LIKE `pay_action_44`;
CREATE TABLE IF NOT EXISTS `dispute_44_shadow` LIKE `dispute_44`;
CREATE TABLE IF NOT EXISTS `dispute_event_44_shadow` LIKE `dispute_event_44`;
CREATE TABLE IF NOT EXISTS `exception_case_44_shadow` LIKE `exception_case_44`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_44_shadow` LIKE `inbound_webhook_44`;
CREATE TABLE IF NOT EXISTS `notify_log_44_shadow` LIKE `notify_log_44`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_44_shadow` LIKE `accounting_outbox_44`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_44_shadow` LIKE `admin_audit_log_44`;

CREATE TABLE IF NOT EXISTS `payment_intent_45_shadow` LIKE `payment_intent_45`;
CREATE TABLE IF NOT EXISTS `charge_45_shadow` LIKE `charge_45`;
CREATE TABLE IF NOT EXISTS `refund_45_shadow` LIKE `refund_45`;
CREATE TABLE IF NOT EXISTS `pay_action_45_shadow` LIKE `pay_action_45`;
CREATE TABLE IF NOT EXISTS `dispute_45_shadow` LIKE `dispute_45`;
CREATE TABLE IF NOT EXISTS `dispute_event_45_shadow` LIKE `dispute_event_45`;
CREATE TABLE IF NOT EXISTS `exception_case_45_shadow` LIKE `exception_case_45`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_45_shadow` LIKE `inbound_webhook_45`;
CREATE TABLE IF NOT EXISTS `notify_log_45_shadow` LIKE `notify_log_45`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_45_shadow` LIKE `accounting_outbox_45`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_45_shadow` LIKE `admin_audit_log_45`;

CREATE TABLE IF NOT EXISTS `payment_intent_46_shadow` LIKE `payment_intent_46`;
CREATE TABLE IF NOT EXISTS `charge_46_shadow` LIKE `charge_46`;
CREATE TABLE IF NOT EXISTS `refund_46_shadow` LIKE `refund_46`;
CREATE TABLE IF NOT EXISTS `pay_action_46_shadow` LIKE `pay_action_46`;
CREATE TABLE IF NOT EXISTS `dispute_46_shadow` LIKE `dispute_46`;
CREATE TABLE IF NOT EXISTS `dispute_event_46_shadow` LIKE `dispute_event_46`;
CREATE TABLE IF NOT EXISTS `exception_case_46_shadow` LIKE `exception_case_46`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_46_shadow` LIKE `inbound_webhook_46`;
CREATE TABLE IF NOT EXISTS `notify_log_46_shadow` LIKE `notify_log_46`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_46_shadow` LIKE `accounting_outbox_46`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_46_shadow` LIKE `admin_audit_log_46`;

CREATE TABLE IF NOT EXISTS `payment_intent_47_shadow` LIKE `payment_intent_47`;
CREATE TABLE IF NOT EXISTS `charge_47_shadow` LIKE `charge_47`;
CREATE TABLE IF NOT EXISTS `refund_47_shadow` LIKE `refund_47`;
CREATE TABLE IF NOT EXISTS `pay_action_47_shadow` LIKE `pay_action_47`;
CREATE TABLE IF NOT EXISTS `dispute_47_shadow` LIKE `dispute_47`;
CREATE TABLE IF NOT EXISTS `dispute_event_47_shadow` LIKE `dispute_event_47`;
CREATE TABLE IF NOT EXISTS `exception_case_47_shadow` LIKE `exception_case_47`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_47_shadow` LIKE `inbound_webhook_47`;
CREATE TABLE IF NOT EXISTS `notify_log_47_shadow` LIKE `notify_log_47`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_47_shadow` LIKE `accounting_outbox_47`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_47_shadow` LIKE `admin_audit_log_47`;

CREATE TABLE IF NOT EXISTS `payment_intent_48_shadow` LIKE `payment_intent_48`;
CREATE TABLE IF NOT EXISTS `charge_48_shadow` LIKE `charge_48`;
CREATE TABLE IF NOT EXISTS `refund_48_shadow` LIKE `refund_48`;
CREATE TABLE IF NOT EXISTS `pay_action_48_shadow` LIKE `pay_action_48`;
CREATE TABLE IF NOT EXISTS `dispute_48_shadow` LIKE `dispute_48`;
CREATE TABLE IF NOT EXISTS `dispute_event_48_shadow` LIKE `dispute_event_48`;
CREATE TABLE IF NOT EXISTS `exception_case_48_shadow` LIKE `exception_case_48`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_48_shadow` LIKE `inbound_webhook_48`;
CREATE TABLE IF NOT EXISTS `notify_log_48_shadow` LIKE `notify_log_48`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_48_shadow` LIKE `accounting_outbox_48`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_48_shadow` LIKE `admin_audit_log_48`;

CREATE TABLE IF NOT EXISTS `payment_intent_49_shadow` LIKE `payment_intent_49`;
CREATE TABLE IF NOT EXISTS `charge_49_shadow` LIKE `charge_49`;
CREATE TABLE IF NOT EXISTS `refund_49_shadow` LIKE `refund_49`;
CREATE TABLE IF NOT EXISTS `pay_action_49_shadow` LIKE `pay_action_49`;
CREATE TABLE IF NOT EXISTS `dispute_49_shadow` LIKE `dispute_49`;
CREATE TABLE IF NOT EXISTS `dispute_event_49_shadow` LIKE `dispute_event_49`;
CREATE TABLE IF NOT EXISTS `exception_case_49_shadow` LIKE `exception_case_49`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_49_shadow` LIKE `inbound_webhook_49`;
CREATE TABLE IF NOT EXISTS `notify_log_49_shadow` LIKE `notify_log_49`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_49_shadow` LIKE `accounting_outbox_49`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_49_shadow` LIKE `admin_audit_log_49`;


-- ==== accounting-system _shadow ====
-- accounting_db_4 的影子表（压测 / shadow 流量）
-- 依赖：4_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
SET NAMES utf8mb4;
USE `accounting_db_4`;

CREATE TABLE IF NOT EXISTS `account_40_shadow` LIKE `account_40`;
CREATE TABLE IF NOT EXISTS `account_transaction_40_shadow` LIKE `account_transaction_40`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_40_shadow` LIKE `accounting_voucher_40`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_40_shadow` LIKE `account_balance_snapshot_40`;
CREATE TABLE IF NOT EXISTS `day_cut_control_40_shadow` LIKE `day_cut_control_40`;
CREATE TABLE IF NOT EXISTS `async_task_40_shadow` LIKE `async_task_40`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_40_shadow` LIKE `tcc_transaction_40`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_40_shadow` LIKE `freeze_compensate_outbox_40`;
CREATE TABLE IF NOT EXISTS `distributed_lock_40_shadow` LIKE `distributed_lock_40`;
CREATE TABLE IF NOT EXISTS `merchant_info_40_shadow` LIKE `merchant_info_40`;
CREATE TABLE IF NOT EXISTS `transaction_order_40_shadow` LIKE `transaction_order_40`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_40_shadow` LIKE `transaction_order_extra_40`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_40_shadow` LIKE `account_balance_buffer_40`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_40_shadow` LIKE `tcc_coordinator_40`;
CREATE TABLE IF NOT EXISTS `batch_order_40_shadow` LIKE `batch_order_40`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_40_shadow` LIKE `settlement_outbox_40`;

CREATE TABLE IF NOT EXISTS `account_41_shadow` LIKE `account_41`;
CREATE TABLE IF NOT EXISTS `account_transaction_41_shadow` LIKE `account_transaction_41`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_41_shadow` LIKE `accounting_voucher_41`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_41_shadow` LIKE `account_balance_snapshot_41`;
CREATE TABLE IF NOT EXISTS `day_cut_control_41_shadow` LIKE `day_cut_control_41`;
CREATE TABLE IF NOT EXISTS `async_task_41_shadow` LIKE `async_task_41`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_41_shadow` LIKE `tcc_transaction_41`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_41_shadow` LIKE `freeze_compensate_outbox_41`;
CREATE TABLE IF NOT EXISTS `distributed_lock_41_shadow` LIKE `distributed_lock_41`;
CREATE TABLE IF NOT EXISTS `merchant_info_41_shadow` LIKE `merchant_info_41`;
CREATE TABLE IF NOT EXISTS `transaction_order_41_shadow` LIKE `transaction_order_41`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_41_shadow` LIKE `transaction_order_extra_41`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_41_shadow` LIKE `account_balance_buffer_41`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_41_shadow` LIKE `tcc_coordinator_41`;
CREATE TABLE IF NOT EXISTS `batch_order_41_shadow` LIKE `batch_order_41`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_41_shadow` LIKE `settlement_outbox_41`;

CREATE TABLE IF NOT EXISTS `account_42_shadow` LIKE `account_42`;
CREATE TABLE IF NOT EXISTS `account_transaction_42_shadow` LIKE `account_transaction_42`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_42_shadow` LIKE `accounting_voucher_42`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_42_shadow` LIKE `account_balance_snapshot_42`;
CREATE TABLE IF NOT EXISTS `day_cut_control_42_shadow` LIKE `day_cut_control_42`;
CREATE TABLE IF NOT EXISTS `async_task_42_shadow` LIKE `async_task_42`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_42_shadow` LIKE `tcc_transaction_42`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_42_shadow` LIKE `freeze_compensate_outbox_42`;
CREATE TABLE IF NOT EXISTS `distributed_lock_42_shadow` LIKE `distributed_lock_42`;
CREATE TABLE IF NOT EXISTS `merchant_info_42_shadow` LIKE `merchant_info_42`;
CREATE TABLE IF NOT EXISTS `transaction_order_42_shadow` LIKE `transaction_order_42`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_42_shadow` LIKE `transaction_order_extra_42`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_42_shadow` LIKE `account_balance_buffer_42`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_42_shadow` LIKE `tcc_coordinator_42`;
CREATE TABLE IF NOT EXISTS `batch_order_42_shadow` LIKE `batch_order_42`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_42_shadow` LIKE `settlement_outbox_42`;

CREATE TABLE IF NOT EXISTS `account_43_shadow` LIKE `account_43`;
CREATE TABLE IF NOT EXISTS `account_transaction_43_shadow` LIKE `account_transaction_43`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_43_shadow` LIKE `accounting_voucher_43`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_43_shadow` LIKE `account_balance_snapshot_43`;
CREATE TABLE IF NOT EXISTS `day_cut_control_43_shadow` LIKE `day_cut_control_43`;
CREATE TABLE IF NOT EXISTS `async_task_43_shadow` LIKE `async_task_43`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_43_shadow` LIKE `tcc_transaction_43`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_43_shadow` LIKE `freeze_compensate_outbox_43`;
CREATE TABLE IF NOT EXISTS `distributed_lock_43_shadow` LIKE `distributed_lock_43`;
CREATE TABLE IF NOT EXISTS `merchant_info_43_shadow` LIKE `merchant_info_43`;
CREATE TABLE IF NOT EXISTS `transaction_order_43_shadow` LIKE `transaction_order_43`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_43_shadow` LIKE `transaction_order_extra_43`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_43_shadow` LIKE `account_balance_buffer_43`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_43_shadow` LIKE `tcc_coordinator_43`;
CREATE TABLE IF NOT EXISTS `batch_order_43_shadow` LIKE `batch_order_43`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_43_shadow` LIKE `settlement_outbox_43`;

CREATE TABLE IF NOT EXISTS `account_44_shadow` LIKE `account_44`;
CREATE TABLE IF NOT EXISTS `account_transaction_44_shadow` LIKE `account_transaction_44`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_44_shadow` LIKE `accounting_voucher_44`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_44_shadow` LIKE `account_balance_snapshot_44`;
CREATE TABLE IF NOT EXISTS `day_cut_control_44_shadow` LIKE `day_cut_control_44`;
CREATE TABLE IF NOT EXISTS `async_task_44_shadow` LIKE `async_task_44`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_44_shadow` LIKE `tcc_transaction_44`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_44_shadow` LIKE `freeze_compensate_outbox_44`;
CREATE TABLE IF NOT EXISTS `distributed_lock_44_shadow` LIKE `distributed_lock_44`;
CREATE TABLE IF NOT EXISTS `merchant_info_44_shadow` LIKE `merchant_info_44`;
CREATE TABLE IF NOT EXISTS `transaction_order_44_shadow` LIKE `transaction_order_44`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_44_shadow` LIKE `transaction_order_extra_44`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_44_shadow` LIKE `account_balance_buffer_44`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_44_shadow` LIKE `tcc_coordinator_44`;
CREATE TABLE IF NOT EXISTS `batch_order_44_shadow` LIKE `batch_order_44`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_44_shadow` LIKE `settlement_outbox_44`;

CREATE TABLE IF NOT EXISTS `account_45_shadow` LIKE `account_45`;
CREATE TABLE IF NOT EXISTS `account_transaction_45_shadow` LIKE `account_transaction_45`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_45_shadow` LIKE `accounting_voucher_45`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_45_shadow` LIKE `account_balance_snapshot_45`;
CREATE TABLE IF NOT EXISTS `day_cut_control_45_shadow` LIKE `day_cut_control_45`;
CREATE TABLE IF NOT EXISTS `async_task_45_shadow` LIKE `async_task_45`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_45_shadow` LIKE `tcc_transaction_45`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_45_shadow` LIKE `freeze_compensate_outbox_45`;
CREATE TABLE IF NOT EXISTS `distributed_lock_45_shadow` LIKE `distributed_lock_45`;
CREATE TABLE IF NOT EXISTS `merchant_info_45_shadow` LIKE `merchant_info_45`;
CREATE TABLE IF NOT EXISTS `transaction_order_45_shadow` LIKE `transaction_order_45`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_45_shadow` LIKE `transaction_order_extra_45`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_45_shadow` LIKE `account_balance_buffer_45`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_45_shadow` LIKE `tcc_coordinator_45`;
CREATE TABLE IF NOT EXISTS `batch_order_45_shadow` LIKE `batch_order_45`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_45_shadow` LIKE `settlement_outbox_45`;

CREATE TABLE IF NOT EXISTS `account_46_shadow` LIKE `account_46`;
CREATE TABLE IF NOT EXISTS `account_transaction_46_shadow` LIKE `account_transaction_46`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_46_shadow` LIKE `accounting_voucher_46`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_46_shadow` LIKE `account_balance_snapshot_46`;
CREATE TABLE IF NOT EXISTS `day_cut_control_46_shadow` LIKE `day_cut_control_46`;
CREATE TABLE IF NOT EXISTS `async_task_46_shadow` LIKE `async_task_46`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_46_shadow` LIKE `tcc_transaction_46`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_46_shadow` LIKE `freeze_compensate_outbox_46`;
CREATE TABLE IF NOT EXISTS `distributed_lock_46_shadow` LIKE `distributed_lock_46`;
CREATE TABLE IF NOT EXISTS `merchant_info_46_shadow` LIKE `merchant_info_46`;
CREATE TABLE IF NOT EXISTS `transaction_order_46_shadow` LIKE `transaction_order_46`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_46_shadow` LIKE `transaction_order_extra_46`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_46_shadow` LIKE `account_balance_buffer_46`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_46_shadow` LIKE `tcc_coordinator_46`;
CREATE TABLE IF NOT EXISTS `batch_order_46_shadow` LIKE `batch_order_46`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_46_shadow` LIKE `settlement_outbox_46`;

CREATE TABLE IF NOT EXISTS `account_47_shadow` LIKE `account_47`;
CREATE TABLE IF NOT EXISTS `account_transaction_47_shadow` LIKE `account_transaction_47`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_47_shadow` LIKE `accounting_voucher_47`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_47_shadow` LIKE `account_balance_snapshot_47`;
CREATE TABLE IF NOT EXISTS `day_cut_control_47_shadow` LIKE `day_cut_control_47`;
CREATE TABLE IF NOT EXISTS `async_task_47_shadow` LIKE `async_task_47`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_47_shadow` LIKE `tcc_transaction_47`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_47_shadow` LIKE `freeze_compensate_outbox_47`;
CREATE TABLE IF NOT EXISTS `distributed_lock_47_shadow` LIKE `distributed_lock_47`;
CREATE TABLE IF NOT EXISTS `merchant_info_47_shadow` LIKE `merchant_info_47`;
CREATE TABLE IF NOT EXISTS `transaction_order_47_shadow` LIKE `transaction_order_47`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_47_shadow` LIKE `transaction_order_extra_47`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_47_shadow` LIKE `account_balance_buffer_47`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_47_shadow` LIKE `tcc_coordinator_47`;
CREATE TABLE IF NOT EXISTS `batch_order_47_shadow` LIKE `batch_order_47`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_47_shadow` LIKE `settlement_outbox_47`;

CREATE TABLE IF NOT EXISTS `account_48_shadow` LIKE `account_48`;
CREATE TABLE IF NOT EXISTS `account_transaction_48_shadow` LIKE `account_transaction_48`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_48_shadow` LIKE `accounting_voucher_48`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_48_shadow` LIKE `account_balance_snapshot_48`;
CREATE TABLE IF NOT EXISTS `day_cut_control_48_shadow` LIKE `day_cut_control_48`;
CREATE TABLE IF NOT EXISTS `async_task_48_shadow` LIKE `async_task_48`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_48_shadow` LIKE `tcc_transaction_48`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_48_shadow` LIKE `freeze_compensate_outbox_48`;
CREATE TABLE IF NOT EXISTS `distributed_lock_48_shadow` LIKE `distributed_lock_48`;
CREATE TABLE IF NOT EXISTS `merchant_info_48_shadow` LIKE `merchant_info_48`;
CREATE TABLE IF NOT EXISTS `transaction_order_48_shadow` LIKE `transaction_order_48`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_48_shadow` LIKE `transaction_order_extra_48`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_48_shadow` LIKE `account_balance_buffer_48`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_48_shadow` LIKE `tcc_coordinator_48`;
CREATE TABLE IF NOT EXISTS `batch_order_48_shadow` LIKE `batch_order_48`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_48_shadow` LIKE `settlement_outbox_48`;

CREATE TABLE IF NOT EXISTS `account_49_shadow` LIKE `account_49`;
CREATE TABLE IF NOT EXISTS `account_transaction_49_shadow` LIKE `account_transaction_49`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_49_shadow` LIKE `accounting_voucher_49`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_49_shadow` LIKE `account_balance_snapshot_49`;
CREATE TABLE IF NOT EXISTS `day_cut_control_49_shadow` LIKE `day_cut_control_49`;
CREATE TABLE IF NOT EXISTS `async_task_49_shadow` LIKE `async_task_49`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_49_shadow` LIKE `tcc_transaction_49`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_49_shadow` LIKE `freeze_compensate_outbox_49`;
CREATE TABLE IF NOT EXISTS `distributed_lock_49_shadow` LIKE `distributed_lock_49`;
CREATE TABLE IF NOT EXISTS `merchant_info_49_shadow` LIKE `merchant_info_49`;
CREATE TABLE IF NOT EXISTS `transaction_order_49_shadow` LIKE `transaction_order_49`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_49_shadow` LIKE `transaction_order_extra_49`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_49_shadow` LIKE `account_balance_buffer_49`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_49_shadow` LIKE `tcc_coordinator_49`;
CREATE TABLE IF NOT EXISTS `batch_order_49_shadow` LIKE `batch_order_49`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_49_shadow` LIKE `settlement_outbox_49`;


-- ==== user-merchant-core _shadow ====
-- user_merchant_db_4 的影子表（压测 / shadow 流量）
-- 依赖：4_init.sql 已经导入完成（CREATE TABLE LIKE 需要主表存在）。
SET NAMES utf8mb4;
USE `user_merchant_db_4`;

CREATE TABLE IF NOT EXISTS `users_40_shadow` LIKE `users_40`;
CREATE TABLE IF NOT EXISTS `user_profiles_40_shadow` LIKE `user_profiles_40`;
CREATE TABLE IF NOT EXISTS `user_auths_40_shadow` LIKE `user_auths_40`;
CREATE TABLE IF NOT EXISTS `login_logs_40_shadow` LIKE `login_logs_40`;
CREATE TABLE IF NOT EXISTS `user_sessions_40_shadow` LIKE `user_sessions_40`;
CREATE TABLE IF NOT EXISTS `user_roles_40_shadow` LIKE `user_roles_40`;
CREATE TABLE IF NOT EXISTS `user_accounts_40_shadow` LIKE `user_accounts_40`;
CREATE TABLE IF NOT EXISTS `user_settings_40_shadow` LIKE `user_settings_40`;
CREATE TABLE IF NOT EXISTS `user_card_40_shadow` LIKE `user_card_40`;
CREATE TABLE IF NOT EXISTS `merchants_40_shadow` LIKE `merchants_40`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_40_shadow` LIKE `merchant_kyc_document_40`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_40_shadow` LIKE `merchant_channel_secret_40`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_40_shadow` LIKE `admin_audit_log_40`;

CREATE TABLE IF NOT EXISTS `users_41_shadow` LIKE `users_41`;
CREATE TABLE IF NOT EXISTS `user_profiles_41_shadow` LIKE `user_profiles_41`;
CREATE TABLE IF NOT EXISTS `user_auths_41_shadow` LIKE `user_auths_41`;
CREATE TABLE IF NOT EXISTS `login_logs_41_shadow` LIKE `login_logs_41`;
CREATE TABLE IF NOT EXISTS `user_sessions_41_shadow` LIKE `user_sessions_41`;
CREATE TABLE IF NOT EXISTS `user_roles_41_shadow` LIKE `user_roles_41`;
CREATE TABLE IF NOT EXISTS `user_accounts_41_shadow` LIKE `user_accounts_41`;
CREATE TABLE IF NOT EXISTS `user_settings_41_shadow` LIKE `user_settings_41`;
CREATE TABLE IF NOT EXISTS `user_card_41_shadow` LIKE `user_card_41`;
CREATE TABLE IF NOT EXISTS `merchants_41_shadow` LIKE `merchants_41`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_41_shadow` LIKE `merchant_kyc_document_41`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_41_shadow` LIKE `merchant_channel_secret_41`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_41_shadow` LIKE `admin_audit_log_41`;

CREATE TABLE IF NOT EXISTS `users_42_shadow` LIKE `users_42`;
CREATE TABLE IF NOT EXISTS `user_profiles_42_shadow` LIKE `user_profiles_42`;
CREATE TABLE IF NOT EXISTS `user_auths_42_shadow` LIKE `user_auths_42`;
CREATE TABLE IF NOT EXISTS `login_logs_42_shadow` LIKE `login_logs_42`;
CREATE TABLE IF NOT EXISTS `user_sessions_42_shadow` LIKE `user_sessions_42`;
CREATE TABLE IF NOT EXISTS `user_roles_42_shadow` LIKE `user_roles_42`;
CREATE TABLE IF NOT EXISTS `user_accounts_42_shadow` LIKE `user_accounts_42`;
CREATE TABLE IF NOT EXISTS `user_settings_42_shadow` LIKE `user_settings_42`;
CREATE TABLE IF NOT EXISTS `user_card_42_shadow` LIKE `user_card_42`;
CREATE TABLE IF NOT EXISTS `merchants_42_shadow` LIKE `merchants_42`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_42_shadow` LIKE `merchant_kyc_document_42`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_42_shadow` LIKE `merchant_channel_secret_42`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_42_shadow` LIKE `admin_audit_log_42`;

CREATE TABLE IF NOT EXISTS `users_43_shadow` LIKE `users_43`;
CREATE TABLE IF NOT EXISTS `user_profiles_43_shadow` LIKE `user_profiles_43`;
CREATE TABLE IF NOT EXISTS `user_auths_43_shadow` LIKE `user_auths_43`;
CREATE TABLE IF NOT EXISTS `login_logs_43_shadow` LIKE `login_logs_43`;
CREATE TABLE IF NOT EXISTS `user_sessions_43_shadow` LIKE `user_sessions_43`;
CREATE TABLE IF NOT EXISTS `user_roles_43_shadow` LIKE `user_roles_43`;
CREATE TABLE IF NOT EXISTS `user_accounts_43_shadow` LIKE `user_accounts_43`;
CREATE TABLE IF NOT EXISTS `user_settings_43_shadow` LIKE `user_settings_43`;
CREATE TABLE IF NOT EXISTS `user_card_43_shadow` LIKE `user_card_43`;
CREATE TABLE IF NOT EXISTS `merchants_43_shadow` LIKE `merchants_43`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_43_shadow` LIKE `merchant_kyc_document_43`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_43_shadow` LIKE `merchant_channel_secret_43`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_43_shadow` LIKE `admin_audit_log_43`;

CREATE TABLE IF NOT EXISTS `users_44_shadow` LIKE `users_44`;
CREATE TABLE IF NOT EXISTS `user_profiles_44_shadow` LIKE `user_profiles_44`;
CREATE TABLE IF NOT EXISTS `user_auths_44_shadow` LIKE `user_auths_44`;
CREATE TABLE IF NOT EXISTS `login_logs_44_shadow` LIKE `login_logs_44`;
CREATE TABLE IF NOT EXISTS `user_sessions_44_shadow` LIKE `user_sessions_44`;
CREATE TABLE IF NOT EXISTS `user_roles_44_shadow` LIKE `user_roles_44`;
CREATE TABLE IF NOT EXISTS `user_accounts_44_shadow` LIKE `user_accounts_44`;
CREATE TABLE IF NOT EXISTS `user_settings_44_shadow` LIKE `user_settings_44`;
CREATE TABLE IF NOT EXISTS `user_card_44_shadow` LIKE `user_card_44`;
CREATE TABLE IF NOT EXISTS `merchants_44_shadow` LIKE `merchants_44`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_44_shadow` LIKE `merchant_kyc_document_44`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_44_shadow` LIKE `merchant_channel_secret_44`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_44_shadow` LIKE `admin_audit_log_44`;

CREATE TABLE IF NOT EXISTS `users_45_shadow` LIKE `users_45`;
CREATE TABLE IF NOT EXISTS `user_profiles_45_shadow` LIKE `user_profiles_45`;
CREATE TABLE IF NOT EXISTS `user_auths_45_shadow` LIKE `user_auths_45`;
CREATE TABLE IF NOT EXISTS `login_logs_45_shadow` LIKE `login_logs_45`;
CREATE TABLE IF NOT EXISTS `user_sessions_45_shadow` LIKE `user_sessions_45`;
CREATE TABLE IF NOT EXISTS `user_roles_45_shadow` LIKE `user_roles_45`;
CREATE TABLE IF NOT EXISTS `user_accounts_45_shadow` LIKE `user_accounts_45`;
CREATE TABLE IF NOT EXISTS `user_settings_45_shadow` LIKE `user_settings_45`;
CREATE TABLE IF NOT EXISTS `user_card_45_shadow` LIKE `user_card_45`;
CREATE TABLE IF NOT EXISTS `merchants_45_shadow` LIKE `merchants_45`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_45_shadow` LIKE `merchant_kyc_document_45`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_45_shadow` LIKE `merchant_channel_secret_45`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_45_shadow` LIKE `admin_audit_log_45`;

CREATE TABLE IF NOT EXISTS `users_46_shadow` LIKE `users_46`;
CREATE TABLE IF NOT EXISTS `user_profiles_46_shadow` LIKE `user_profiles_46`;
CREATE TABLE IF NOT EXISTS `user_auths_46_shadow` LIKE `user_auths_46`;
CREATE TABLE IF NOT EXISTS `login_logs_46_shadow` LIKE `login_logs_46`;
CREATE TABLE IF NOT EXISTS `user_sessions_46_shadow` LIKE `user_sessions_46`;
CREATE TABLE IF NOT EXISTS `user_roles_46_shadow` LIKE `user_roles_46`;
CREATE TABLE IF NOT EXISTS `user_accounts_46_shadow` LIKE `user_accounts_46`;
CREATE TABLE IF NOT EXISTS `user_settings_46_shadow` LIKE `user_settings_46`;
CREATE TABLE IF NOT EXISTS `user_card_46_shadow` LIKE `user_card_46`;
CREATE TABLE IF NOT EXISTS `merchants_46_shadow` LIKE `merchants_46`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_46_shadow` LIKE `merchant_kyc_document_46`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_46_shadow` LIKE `merchant_channel_secret_46`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_46_shadow` LIKE `admin_audit_log_46`;

CREATE TABLE IF NOT EXISTS `users_47_shadow` LIKE `users_47`;
CREATE TABLE IF NOT EXISTS `user_profiles_47_shadow` LIKE `user_profiles_47`;
CREATE TABLE IF NOT EXISTS `user_auths_47_shadow` LIKE `user_auths_47`;
CREATE TABLE IF NOT EXISTS `login_logs_47_shadow` LIKE `login_logs_47`;
CREATE TABLE IF NOT EXISTS `user_sessions_47_shadow` LIKE `user_sessions_47`;
CREATE TABLE IF NOT EXISTS `user_roles_47_shadow` LIKE `user_roles_47`;
CREATE TABLE IF NOT EXISTS `user_accounts_47_shadow` LIKE `user_accounts_47`;
CREATE TABLE IF NOT EXISTS `user_settings_47_shadow` LIKE `user_settings_47`;
CREATE TABLE IF NOT EXISTS `user_card_47_shadow` LIKE `user_card_47`;
CREATE TABLE IF NOT EXISTS `merchants_47_shadow` LIKE `merchants_47`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_47_shadow` LIKE `merchant_kyc_document_47`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_47_shadow` LIKE `merchant_channel_secret_47`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_47_shadow` LIKE `admin_audit_log_47`;

CREATE TABLE IF NOT EXISTS `users_48_shadow` LIKE `users_48`;
CREATE TABLE IF NOT EXISTS `user_profiles_48_shadow` LIKE `user_profiles_48`;
CREATE TABLE IF NOT EXISTS `user_auths_48_shadow` LIKE `user_auths_48`;
CREATE TABLE IF NOT EXISTS `login_logs_48_shadow` LIKE `login_logs_48`;
CREATE TABLE IF NOT EXISTS `user_sessions_48_shadow` LIKE `user_sessions_48`;
CREATE TABLE IF NOT EXISTS `user_roles_48_shadow` LIKE `user_roles_48`;
CREATE TABLE IF NOT EXISTS `user_accounts_48_shadow` LIKE `user_accounts_48`;
CREATE TABLE IF NOT EXISTS `user_settings_48_shadow` LIKE `user_settings_48`;
CREATE TABLE IF NOT EXISTS `user_card_48_shadow` LIKE `user_card_48`;
CREATE TABLE IF NOT EXISTS `merchants_48_shadow` LIKE `merchants_48`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_48_shadow` LIKE `merchant_kyc_document_48`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_48_shadow` LIKE `merchant_channel_secret_48`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_48_shadow` LIKE `admin_audit_log_48`;

CREATE TABLE IF NOT EXISTS `users_49_shadow` LIKE `users_49`;
CREATE TABLE IF NOT EXISTS `user_profiles_49_shadow` LIKE `user_profiles_49`;
CREATE TABLE IF NOT EXISTS `user_auths_49_shadow` LIKE `user_auths_49`;
CREATE TABLE IF NOT EXISTS `login_logs_49_shadow` LIKE `login_logs_49`;
CREATE TABLE IF NOT EXISTS `user_sessions_49_shadow` LIKE `user_sessions_49`;
CREATE TABLE IF NOT EXISTS `user_roles_49_shadow` LIKE `user_roles_49`;
CREATE TABLE IF NOT EXISTS `user_accounts_49_shadow` LIKE `user_accounts_49`;
CREATE TABLE IF NOT EXISTS `user_settings_49_shadow` LIKE `user_settings_49`;
CREATE TABLE IF NOT EXISTS `user_card_49_shadow` LIKE `user_card_49`;
CREATE TABLE IF NOT EXISTS `merchants_49_shadow` LIKE `merchants_49`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_49_shadow` LIKE `merchant_kyc_document_49`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_49_shadow` LIKE `merchant_channel_secret_49`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_49_shadow` LIKE `admin_audit_log_49`;


-- ==== card-center ====
SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `card_center_db_4` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_center_db_4`;

-- card-center 分片表模板。4 = 0..9，40 = 00..99（globalTblIdx）。
-- generate.sh 把所有 4 / 40 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_4`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_40` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_40` (
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
CREATE TABLE IF NOT EXISTS `audit_log_40` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 40 (PCI 10.7 7y retention)';

-- card-center 分片表模板。4 = 0..9，41 = 00..99（globalTblIdx）。
-- generate.sh 把所有 4 / 41 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_4`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_41` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_41` (
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
CREATE TABLE IF NOT EXISTS `audit_log_41` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 41 (PCI 10.7 7y retention)';

-- card-center 分片表模板。4 = 0..9，42 = 00..99（globalTblIdx）。
-- generate.sh 把所有 4 / 42 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_4`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_42` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_42` (
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
CREATE TABLE IF NOT EXISTS `audit_log_42` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 42 (PCI 10.7 7y retention)';

-- card-center 分片表模板。4 = 0..9，43 = 00..99（globalTblIdx）。
-- generate.sh 把所有 4 / 43 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_4`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_43` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_43` (
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
CREATE TABLE IF NOT EXISTS `audit_log_43` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 43 (PCI 10.7 7y retention)';

-- card-center 分片表模板。4 = 0..9，44 = 00..99（globalTblIdx）。
-- generate.sh 把所有 4 / 44 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_4`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_44` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_44` (
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
CREATE TABLE IF NOT EXISTS `audit_log_44` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 44 (PCI 10.7 7y retention)';

-- card-center 分片表模板。4 = 0..9，45 = 00..99（globalTblIdx）。
-- generate.sh 把所有 4 / 45 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_4`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_45` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_45` (
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
CREATE TABLE IF NOT EXISTS `audit_log_45` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 45 (PCI 10.7 7y retention)';

-- card-center 分片表模板。4 = 0..9，46 = 00..99（globalTblIdx）。
-- generate.sh 把所有 4 / 46 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_4`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_46` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_46` (
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
CREATE TABLE IF NOT EXISTS `audit_log_46` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 46 (PCI 10.7 7y retention)';

-- card-center 分片表模板。4 = 0..9，47 = 00..99（globalTblIdx）。
-- generate.sh 把所有 4 / 47 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_4`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_47` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_47` (
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
CREATE TABLE IF NOT EXISTS `audit_log_47` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 47 (PCI 10.7 7y retention)';

-- card-center 分片表模板。4 = 0..9，48 = 00..99（globalTblIdx）。
-- generate.sh 把所有 4 / 48 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_4`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_48` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_48` (
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
CREATE TABLE IF NOT EXISTS `audit_log_48` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 48 (PCI 10.7 7y retention)';

-- card-center 分片表模板。4 = 0..9，49 = 00..99（globalTblIdx）。
-- generate.sh 把所有 4 / 49 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_4`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_49` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_49` (
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
CREATE TABLE IF NOT EXISTS `audit_log_49` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 49 (PCI 10.7 7y retention)';


-- ==== card-payment ====
SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `card_payment_db_4` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_payment_db_4`;

-- card-payment 分片表模板。4 = 0..9，40 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_4`;

CREATE TABLE IF NOT EXISTS `card_transaction_40` (
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

-- card-payment 分片表模板。4 = 0..9，41 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_4`;

CREATE TABLE IF NOT EXISTS `card_transaction_41` (
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

-- card-payment 分片表模板。4 = 0..9，42 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_4`;

CREATE TABLE IF NOT EXISTS `card_transaction_42` (
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

-- card-payment 分片表模板。4 = 0..9，43 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_4`;

CREATE TABLE IF NOT EXISTS `card_transaction_43` (
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

-- card-payment 分片表模板。4 = 0..9，44 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_4`;

CREATE TABLE IF NOT EXISTS `card_transaction_44` (
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

-- card-payment 分片表模板。4 = 0..9，45 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_4`;

CREATE TABLE IF NOT EXISTS `card_transaction_45` (
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

-- card-payment 分片表模板。4 = 0..9，46 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_4`;

CREATE TABLE IF NOT EXISTS `card_transaction_46` (
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

-- card-payment 分片表模板。4 = 0..9，47 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_4`;

CREATE TABLE IF NOT EXISTS `card_transaction_47` (
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

-- card-payment 分片表模板。4 = 0..9，48 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_4`;

CREATE TABLE IF NOT EXISTS `card_transaction_48` (
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

-- card-payment 分片表模板。4 = 0..9，49 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_4`;

CREATE TABLE IF NOT EXISTS `card_transaction_49` (
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
-- card_center_db_4 shadow 表（压测）
USE `card_center_db_4`;

CREATE TABLE IF NOT EXISTS `card_stored_token_40_shadow` LIKE `card_stored_token_40`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_40_shadow` LIKE `card_payment_token_used_40`;
CREATE TABLE IF NOT EXISTS `audit_log_40_shadow` LIKE `audit_log_40`;

CREATE TABLE IF NOT EXISTS `card_stored_token_41_shadow` LIKE `card_stored_token_41`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_41_shadow` LIKE `card_payment_token_used_41`;
CREATE TABLE IF NOT EXISTS `audit_log_41_shadow` LIKE `audit_log_41`;

CREATE TABLE IF NOT EXISTS `card_stored_token_42_shadow` LIKE `card_stored_token_42`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_42_shadow` LIKE `card_payment_token_used_42`;
CREATE TABLE IF NOT EXISTS `audit_log_42_shadow` LIKE `audit_log_42`;

CREATE TABLE IF NOT EXISTS `card_stored_token_43_shadow` LIKE `card_stored_token_43`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_43_shadow` LIKE `card_payment_token_used_43`;
CREATE TABLE IF NOT EXISTS `audit_log_43_shadow` LIKE `audit_log_43`;

CREATE TABLE IF NOT EXISTS `card_stored_token_44_shadow` LIKE `card_stored_token_44`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_44_shadow` LIKE `card_payment_token_used_44`;
CREATE TABLE IF NOT EXISTS `audit_log_44_shadow` LIKE `audit_log_44`;

CREATE TABLE IF NOT EXISTS `card_stored_token_45_shadow` LIKE `card_stored_token_45`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_45_shadow` LIKE `card_payment_token_used_45`;
CREATE TABLE IF NOT EXISTS `audit_log_45_shadow` LIKE `audit_log_45`;

CREATE TABLE IF NOT EXISTS `card_stored_token_46_shadow` LIKE `card_stored_token_46`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_46_shadow` LIKE `card_payment_token_used_46`;
CREATE TABLE IF NOT EXISTS `audit_log_46_shadow` LIKE `audit_log_46`;

CREATE TABLE IF NOT EXISTS `card_stored_token_47_shadow` LIKE `card_stored_token_47`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_47_shadow` LIKE `card_payment_token_used_47`;
CREATE TABLE IF NOT EXISTS `audit_log_47_shadow` LIKE `audit_log_47`;

CREATE TABLE IF NOT EXISTS `card_stored_token_48_shadow` LIKE `card_stored_token_48`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_48_shadow` LIKE `card_payment_token_used_48`;
CREATE TABLE IF NOT EXISTS `audit_log_48_shadow` LIKE `audit_log_48`;

CREATE TABLE IF NOT EXISTS `card_stored_token_49_shadow` LIKE `card_stored_token_49`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_49_shadow` LIKE `card_payment_token_used_49`;
CREATE TABLE IF NOT EXISTS `audit_log_49_shadow` LIKE `audit_log_49`;


-- ==== card-payment _shadow ====
-- card_payment_db_4 shadow
USE `card_payment_db_4`;

CREATE TABLE IF NOT EXISTS `card_transaction_40_shadow` LIKE `card_transaction_40`;

CREATE TABLE IF NOT EXISTS `card_transaction_41_shadow` LIKE `card_transaction_41`;

CREATE TABLE IF NOT EXISTS `card_transaction_42_shadow` LIKE `card_transaction_42`;

CREATE TABLE IF NOT EXISTS `card_transaction_43_shadow` LIKE `card_transaction_43`;

CREATE TABLE IF NOT EXISTS `card_transaction_44_shadow` LIKE `card_transaction_44`;

CREATE TABLE IF NOT EXISTS `card_transaction_45_shadow` LIKE `card_transaction_45`;

CREATE TABLE IF NOT EXISTS `card_transaction_46_shadow` LIKE `card_transaction_46`;

CREATE TABLE IF NOT EXISTS `card_transaction_47_shadow` LIKE `card_transaction_47`;

CREATE TABLE IF NOT EXISTS `card_transaction_48_shadow` LIKE `card_transaction_48`;

CREATE TABLE IF NOT EXISTS `card_transaction_49_shadow` LIKE `card_transaction_49`;


-- ==== recon_cdc binlog 用户 ====
CREATE USER IF NOT EXISTS 'recon_cdc'@'%' IDENTIFIED WITH mysql_native_password BY 'recon_cdc_pwd';
ALTER USER 'recon_cdc'@'%' IDENTIFIED WITH mysql_native_password BY 'recon_cdc_pwd';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'recon_cdc'@'%';
GRANT SELECT ON *.* TO 'recon_cdc'@'%';
FLUSH PRIVILEGES;
