-- ┌──────────────────────────────────────────────────────────────────────┐
-- │ 共享 MySQL shard 8 —— paychan_db_8 + order_db_8 +
-- │ accounting_db_8 + user_merchant_db_8                            │
-- └──────────────────────────────────────────────────────────────────────┘

-- ==== payment-channel ====
CREATE DATABASE IF NOT EXISTS `paychan_db_8` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `paychan_db_8`;

-- payment-channel 分片表模板。按 pi_id 分库分表，10 库 × 10 表。
-- apply.sh 把 80 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_80` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_80` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_80` (
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
CREATE TABLE IF NOT EXISTS `channel_token_80` (
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
-- apply.sh 把 81 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_81` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_81` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_81` (
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
CREATE TABLE IF NOT EXISTS `channel_token_81` (
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
-- apply.sh 把 82 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_82` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_82` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_82` (
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
CREATE TABLE IF NOT EXISTS `channel_token_82` (
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
-- apply.sh 把 83 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_83` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_83` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_83` (
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
CREATE TABLE IF NOT EXISTS `channel_token_83` (
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
-- apply.sh 把 84 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_84` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_84` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_84` (
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
CREATE TABLE IF NOT EXISTS `channel_token_84` (
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
-- apply.sh 把 85 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_85` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_85` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_85` (
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
CREATE TABLE IF NOT EXISTS `channel_token_85` (
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
-- apply.sh 把 86 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_86` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_86` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_86` (
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
CREATE TABLE IF NOT EXISTS `channel_token_86` (
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
-- apply.sh 把 87 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_87` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_87` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_87` (
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
CREATE TABLE IF NOT EXISTS `channel_token_87` (
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
-- apply.sh 把 88 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_88` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_88` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_88` (
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
CREATE TABLE IF NOT EXISTS `channel_token_88` (
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
-- apply.sh 把 89 替换为 00..99 跑遍全部分片表。

-- 1. 渠道调用流水：每次出站请求都先落 pending 行，返回后 UPDATE。
--    state 取值（修复 P0-1 后）：
--      pending   = 已落库，尚未拿到第三方响应（save-first-then-call 中间态）
--      unknown   = 已发出请求但未拿到明确成败（网络错/超时/5xx）；
--                   PendingQueryWorker 调 Query 推进，**不重发原请求**
--      succeeded = 第三方明确返回成功
--      failed    = 第三方明确返回失败（4xx 业务错误、明确 result=failed）
CREATE TABLE IF NOT EXISTS `acquirer_tx_89` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_89` (
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
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_89` (
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
CREATE TABLE IF NOT EXISTS `channel_token_89` (
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
CREATE DATABASE IF NOT EXISTS `order_db_8` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `order_db_8`;

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：8 = 0-9 物理库；80 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_80
CREATE TABLE IF NOT EXISTS `payment_intent_80` (
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

-- 2. Charge 表 charge_80（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_80` (
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

-- 3. PayAction 表 pay_action_80（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_80` (
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

-- 4. ExceptionCase 表 exception_case_80（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_80` (
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

-- 5. NotifyLog 表 notify_log_80（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_80` (
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

-- 5. Refund 表 refund_80（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_80` (
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

-- 7. InboundWebhook 表 inbound_webhook_80
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_80` (
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

-- 8. Dispute 表 dispute_80
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_80` (
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

-- 9. DisputeEvent 表 dispute_event_80（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_80` (
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

-- 10. AccountingOutbox 表 accounting_outbox_80
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_80` (
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

-- ─── admin_audit_log_80：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 80';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：8 = 0-9 物理库；81 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_81
CREATE TABLE IF NOT EXISTS `payment_intent_81` (
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

-- 2. Charge 表 charge_81（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_81` (
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

-- 3. PayAction 表 pay_action_81（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_81` (
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

-- 4. ExceptionCase 表 exception_case_81（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_81` (
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

-- 5. NotifyLog 表 notify_log_81（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_81` (
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

-- 5. Refund 表 refund_81（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_81` (
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

-- 7. InboundWebhook 表 inbound_webhook_81
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_81` (
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

-- 8. Dispute 表 dispute_81
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_81` (
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

-- 9. DisputeEvent 表 dispute_event_81（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_81` (
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

-- 10. AccountingOutbox 表 accounting_outbox_81
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_81` (
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

-- ─── admin_audit_log_81：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 81';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：8 = 0-9 物理库；82 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_82
CREATE TABLE IF NOT EXISTS `payment_intent_82` (
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

-- 2. Charge 表 charge_82（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_82` (
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

-- 3. PayAction 表 pay_action_82（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_82` (
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

-- 4. ExceptionCase 表 exception_case_82（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_82` (
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

-- 5. NotifyLog 表 notify_log_82（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_82` (
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

-- 5. Refund 表 refund_82（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_82` (
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

-- 7. InboundWebhook 表 inbound_webhook_82
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_82` (
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

-- 8. Dispute 表 dispute_82
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_82` (
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

-- 9. DisputeEvent 表 dispute_event_82（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_82` (
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

-- 10. AccountingOutbox 表 accounting_outbox_82
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_82` (
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

-- ─── admin_audit_log_82：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 82';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：8 = 0-9 物理库；83 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_83
CREATE TABLE IF NOT EXISTS `payment_intent_83` (
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

-- 2. Charge 表 charge_83（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_83` (
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

-- 3. PayAction 表 pay_action_83（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_83` (
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

-- 4. ExceptionCase 表 exception_case_83（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_83` (
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

-- 5. NotifyLog 表 notify_log_83（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_83` (
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

-- 5. Refund 表 refund_83（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_83` (
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

-- 7. InboundWebhook 表 inbound_webhook_83
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_83` (
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

-- 8. Dispute 表 dispute_83
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_83` (
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

-- 9. DisputeEvent 表 dispute_event_83（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_83` (
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

-- 10. AccountingOutbox 表 accounting_outbox_83
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_83` (
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

-- ─── admin_audit_log_83：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 83';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：8 = 0-9 物理库；84 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_84
CREATE TABLE IF NOT EXISTS `payment_intent_84` (
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

-- 2. Charge 表 charge_84（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_84` (
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

-- 3. PayAction 表 pay_action_84（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_84` (
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

-- 4. ExceptionCase 表 exception_case_84（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_84` (
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

-- 5. NotifyLog 表 notify_log_84（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_84` (
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

-- 5. Refund 表 refund_84（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_84` (
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

-- 7. InboundWebhook 表 inbound_webhook_84
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_84` (
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

-- 8. Dispute 表 dispute_84
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_84` (
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

-- 9. DisputeEvent 表 dispute_event_84（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_84` (
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

-- 10. AccountingOutbox 表 accounting_outbox_84
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_84` (
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

-- ─── admin_audit_log_84：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 84';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：8 = 0-9 物理库；85 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_85
CREATE TABLE IF NOT EXISTS `payment_intent_85` (
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

-- 2. Charge 表 charge_85（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_85` (
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

-- 3. PayAction 表 pay_action_85（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_85` (
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

-- 4. ExceptionCase 表 exception_case_85（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_85` (
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

-- 5. NotifyLog 表 notify_log_85（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_85` (
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

-- 5. Refund 表 refund_85（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_85` (
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

-- 7. InboundWebhook 表 inbound_webhook_85
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_85` (
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

-- 8. Dispute 表 dispute_85
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_85` (
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

-- 9. DisputeEvent 表 dispute_event_85（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_85` (
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

-- 10. AccountingOutbox 表 accounting_outbox_85
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_85` (
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

-- ─── admin_audit_log_85：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 85';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：8 = 0-9 物理库；86 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_86
CREATE TABLE IF NOT EXISTS `payment_intent_86` (
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

-- 2. Charge 表 charge_86（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_86` (
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

-- 3. PayAction 表 pay_action_86（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_86` (
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

-- 4. ExceptionCase 表 exception_case_86（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_86` (
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

-- 5. NotifyLog 表 notify_log_86（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_86` (
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

-- 5. Refund 表 refund_86（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_86` (
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

-- 7. InboundWebhook 表 inbound_webhook_86
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_86` (
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

-- 8. Dispute 表 dispute_86
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_86` (
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

-- 9. DisputeEvent 表 dispute_event_86（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_86` (
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

-- 10. AccountingOutbox 表 accounting_outbox_86
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_86` (
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

-- ─── admin_audit_log_86：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 86';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：8 = 0-9 物理库；87 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_87
CREATE TABLE IF NOT EXISTS `payment_intent_87` (
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

-- 2. Charge 表 charge_87（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_87` (
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

-- 3. PayAction 表 pay_action_87（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_87` (
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

-- 4. ExceptionCase 表 exception_case_87（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_87` (
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

-- 5. NotifyLog 表 notify_log_87（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_87` (
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

-- 5. Refund 表 refund_87（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_87` (
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

-- 7. InboundWebhook 表 inbound_webhook_87
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_87` (
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

-- 8. Dispute 表 dispute_87
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_87` (
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

-- 9. DisputeEvent 表 dispute_event_87（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_87` (
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

-- 10. AccountingOutbox 表 accounting_outbox_87
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_87` (
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

-- ─── admin_audit_log_87：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 87';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：8 = 0-9 物理库；88 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_88
CREATE TABLE IF NOT EXISTS `payment_intent_88` (
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

-- 2. Charge 表 charge_88（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_88` (
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

-- 3. PayAction 表 pay_action_88（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_88` (
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

-- 4. ExceptionCase 表 exception_case_88（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_88` (
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

-- 5. NotifyLog 表 notify_log_88（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_88` (
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

-- 5. Refund 表 refund_88（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_88` (
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

-- 7. InboundWebhook 表 inbound_webhook_88
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_88` (
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

-- 8. Dispute 表 dispute_88
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_88` (
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

-- 9. DisputeEvent 表 dispute_event_88（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_88` (
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

-- 10. AccountingOutbox 表 accounting_outbox_88
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_88` (
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

-- ─── admin_audit_log_88：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 88';

-- ============================================
-- order-core 表结构模板（Stripe 风格）
-- 10 库 × 10 表 = 100 张全局表
-- 占位符：8 = 0-9 物理库；89 = 00-99 全局表序号
-- 路由规则：
--   pi_id 前缀编码分片信息：pi_{dbIdx:1d}{tblIdx:02d}{seq}
--   Charge / Refund 与其 PaymentIntent 同分片
-- ============================================

-- 1. PaymentIntent 主表 payment_intent_89
CREATE TABLE IF NOT EXISTS `payment_intent_89` (
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

-- 2. Charge 表 charge_89（一个 PI 可有多个 Charge：失败 / 重试）
CREATE TABLE IF NOT EXISTS `charge_89` (
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

-- 3. PayAction 表 pay_action_89（3DS / OTP / PayPassword 等用户挑战条目）
CREATE TABLE IF NOT EXISTS `pay_action_89` (
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

-- 4. ExceptionCase 表 exception_case_89（差错处理单：过期后迟到回调等）
CREATE TABLE IF NOT EXISTS `exception_case_89` (
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

-- 5. NotifyLog 表 notify_log_89（每次通知客户端的尝试都落一条；cron 扫失败的重发）
CREATE TABLE IF NOT EXISTS `notify_log_89` (
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

-- 5. Refund 表 refund_89（一个 Charge 可有多个 Refund：部分退款）
CREATE TABLE IF NOT EXISTS `refund_89` (
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

-- 7. InboundWebhook 表 inbound_webhook_89
--    入站 webhook 去重日志：payment-channel/payment-core 转发过来的 channel webhook
--    用 (channel_name, event_id) 唯一约束保证同一事件不会重复推进 PI/Charge/Refund 状态。
--    路由：优先按 payment_intent_id 路由（与 PI 同分片）；event 不带 PI 时按 event_id 哈希路由。
CREATE TABLE IF NOT EXISTS `inbound_webhook_89` (
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

-- 8. Dispute 表 dispute_89
--    渠道发起的 dispute / chargeback。PH 场景以电子钱包争议为主（卡组织 chargeback 极少），
--    但流程抽象一致：needs_response → under_review → won / lost / warning_closed / canceled。
--    路由：按 payment_intent_id 与 PI 同分片，所有相关实体（charge/refund/dispute）查询落在一个分片。
CREATE TABLE IF NOT EXISTS `dispute_89` (
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

-- 9. DisputeEvent 表 dispute_event_89（状态流转日志）
CREATE TABLE IF NOT EXISTS `dispute_event_89` (
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

-- 10. AccountingOutbox 表 accounting_outbox_89
--     order-core 记账事件 outbox：payment/refund 成交后 webhook_service 写一条
--     pending 行，accounting_outbox_worker 轮询调 accounting-system 的
--     HybridDoubleEntryBooking 落账。payment_method 供 worker 从 config
--     counter_accounts[payment_method] 查渠道应收平台 accountNo（对端分录）。
--     路由：按 payment_intent_id 与 PI 同分片。
--     幂等键：(request_id) 唯一，request_id = {pi_id}:{event_type}:{charge_or_refund_id}。
CREATE TABLE IF NOT EXISTS `accounting_outbox_89` (
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

-- ─── admin_audit_log_89：管理台操作审计（按 actor 路由） ────────────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。同 actor 落同 shard，取证查全。
CREATE TABLE IF NOT EXISTS `admin_audit_log_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 89';


-- ==== accounting-system schema ====
SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS `accounting_db_8` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `accounting_db_8`;

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


CREATE TABLE IF NOT EXISTS `account_80` (
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
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
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
CREATE TABLE IF NOT EXISTS `account_transaction_80` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_80` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_80` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_80` (
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
CREATE TABLE IF NOT EXISTS `async_task_80` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_80` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_80` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_80` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_80` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_80` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_80` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_80` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_80` (
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
CREATE TABLE IF NOT EXISTS `batch_order_80` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_80` (
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


CREATE TABLE IF NOT EXISTS `account_81` (
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
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
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
CREATE TABLE IF NOT EXISTS `account_transaction_81` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_81` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_81` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_81` (
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
CREATE TABLE IF NOT EXISTS `async_task_81` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_81` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_81` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_81` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_81` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_81` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_81` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_81` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_81` (
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
CREATE TABLE IF NOT EXISTS `batch_order_81` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_81` (
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


CREATE TABLE IF NOT EXISTS `account_82` (
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
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
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
CREATE TABLE IF NOT EXISTS `account_transaction_82` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_82` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_82` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_82` (
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
CREATE TABLE IF NOT EXISTS `async_task_82` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_82` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_82` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_82` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_82` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_82` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_82` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_82` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_82` (
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
CREATE TABLE IF NOT EXISTS `batch_order_82` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_82` (
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


CREATE TABLE IF NOT EXISTS `account_83` (
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
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
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
CREATE TABLE IF NOT EXISTS `account_transaction_83` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_83` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_83` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_83` (
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
CREATE TABLE IF NOT EXISTS `async_task_83` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_83` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_83` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_83` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_83` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_83` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_83` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_83` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_83` (
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
CREATE TABLE IF NOT EXISTS `batch_order_83` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_83` (
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


CREATE TABLE IF NOT EXISTS `account_84` (
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
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
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
CREATE TABLE IF NOT EXISTS `account_transaction_84` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_84` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_84` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_84` (
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
CREATE TABLE IF NOT EXISTS `async_task_84` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_84` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_84` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_84` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_84` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_84` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_84` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_84` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_84` (
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
CREATE TABLE IF NOT EXISTS `batch_order_84` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_84` (
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


CREATE TABLE IF NOT EXISTS `account_85` (
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
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
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
CREATE TABLE IF NOT EXISTS `account_transaction_85` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_85` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_85` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_85` (
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
CREATE TABLE IF NOT EXISTS `async_task_85` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_85` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_85` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_85` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_85` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_85` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_85` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_85` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_85` (
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
CREATE TABLE IF NOT EXISTS `batch_order_85` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_85` (
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


CREATE TABLE IF NOT EXISTS `account_86` (
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
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
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
CREATE TABLE IF NOT EXISTS `account_transaction_86` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_86` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_86` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_86` (
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
CREATE TABLE IF NOT EXISTS `async_task_86` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_86` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_86` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_86` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_86` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_86` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_86` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_86` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_86` (
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
CREATE TABLE IF NOT EXISTS `batch_order_86` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_86` (
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


CREATE TABLE IF NOT EXISTS `account_87` (
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
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
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
CREATE TABLE IF NOT EXISTS `account_transaction_87` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_87` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_87` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_87` (
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
CREATE TABLE IF NOT EXISTS `async_task_87` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_87` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_87` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_87` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_87` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_87` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_87` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_87` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_87` (
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
CREATE TABLE IF NOT EXISTS `batch_order_87` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_87` (
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


CREATE TABLE IF NOT EXISTS `account_88` (
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
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
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
CREATE TABLE IF NOT EXISTS `account_transaction_88` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_88` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_88` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_88` (
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
CREATE TABLE IF NOT EXISTS `async_task_88` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_88` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_88` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_88` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_88` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_88` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_88` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_88` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_88` (
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
CREATE TABLE IF NOT EXISTS `batch_order_88` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_88` (
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


CREATE TABLE IF NOT EXISTS `account_89` (
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
    UNIQUE KEY `uk_user_business_type` (`user_id`, `account_business_type`, `currency`) COMMENT 'userId + 业务类型 + 币种唯一键',
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
CREATE TABLE IF NOT EXISTS `account_transaction_89` (
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
CREATE TABLE IF NOT EXISTS `accounting_voucher_89` (
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
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_89` (
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
CREATE TABLE IF NOT EXISTS `day_cut_control_89` (
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
CREATE TABLE IF NOT EXISTS `async_task_89` (
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
CREATE TABLE IF NOT EXISTS `tcc_transaction_89` (
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
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_89` (
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
CREATE TABLE IF NOT EXISTS `distributed_lock_89` (
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
CREATE TABLE IF NOT EXISTS `merchant_info_89` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_89` (
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
CREATE TABLE IF NOT EXISTS `transaction_order_extra_89` (
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
CREATE TABLE IF NOT EXISTS `account_balance_buffer_89` (
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
CREATE TABLE IF NOT EXISTS `tcc_coordinator_89` (
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
CREATE TABLE IF NOT EXISTS `batch_order_89` (
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
CREATE TABLE IF NOT EXISTS `settlement_outbox_89` (
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
-- tx_account_anchor_80: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_80` (
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
-- flow_anchor_route_80: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_80` (
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
-- tx_account_anchor_81: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_81` (
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
-- flow_anchor_route_81: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_81` (
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
-- tx_account_anchor_82: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_82` (
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
-- flow_anchor_route_82: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_82` (
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
-- tx_account_anchor_83: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_83` (
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
-- flow_anchor_route_83: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_83` (
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
-- tx_account_anchor_84: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_84` (
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
-- flow_anchor_route_84: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_84` (
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
-- tx_account_anchor_85: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_85` (
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
-- flow_anchor_route_85: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_85` (
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
-- tx_account_anchor_86: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_86` (
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
-- flow_anchor_route_86: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_86` (
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
-- tx_account_anchor_87: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_87` (
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
-- flow_anchor_route_87: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_87` (
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
-- tx_account_anchor_88: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_88` (
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
-- flow_anchor_route_88: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_88` (
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
-- tx_account_anchor_89: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_89` (
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
-- flow_anchor_route_89: 资金流 → instance 路由索引表
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
CREATE TABLE IF NOT EXISTS `flow_anchor_route_89` (
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
USE `accounting_db_8`;

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 80 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (80)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (80)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_80` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 80 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 80, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_80` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 80 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 80, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_80` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 80 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 80, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_80` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 80 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 80, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_80` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 80 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 80, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_80` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 80 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 80, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 81 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (81)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (81)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_81` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 81 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 81, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_81` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 81 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 81, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_81` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 81 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 81, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_81` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 81 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 81, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_81` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 81 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 81, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_81` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 81 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 81, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 82 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (82)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (82)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_82` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 82 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 82, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_82` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 82 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 82, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_82` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 82 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 82, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_82` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 82 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 82, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_82` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 82 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 82, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_82` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 82 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 82, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 83 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (83)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (83)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_83` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 83 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 83, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_83` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 83 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 83, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_83` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 83 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 83, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_83` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 83 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 83, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_83` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 83 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 83, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_83` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 83 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 83, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 84 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (84)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (84)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_84` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 84 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 84, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_84` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 84 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 84, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_84` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 84 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 84, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_84` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 84 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 84, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_84` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 84 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 84, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_84` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 84 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 84, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 85 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (85)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (85)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_85` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 85 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 85, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_85` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 85 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 85, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_85` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 85 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 85, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_85` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 85 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 85, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_85` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 85 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 85, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_85` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 85 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 85, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 86 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (86)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (86)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_86` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 86 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 86, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_86` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 86 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 86, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_86` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 86 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 86, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_86` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 86 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 86, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_86` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 86 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 86, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_86` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 86 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 86, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 87 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (87)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (87)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_87` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 87 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 87, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_87` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 87 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 87, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_87` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 87 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 87, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_87` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 87 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 87, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_87` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 87 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 87, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_87` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 87 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 87, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 88 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (88)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (88)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_88` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 88 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 88, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_88` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 88 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 88, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_88` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 88 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 88, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_88` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 88 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 88, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_88` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 88 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 88, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_88` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 88 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 88, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 89 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (89)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (89)
--
-- 1000000 / 0 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_89` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 89 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  1000000 + 89, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_89` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 89 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  1000000 + 89, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_89` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 89 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  1000000 + 89, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_89` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 89 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  1000000 + 89, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_89` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 89 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  1000000 + 89, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_89` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(0 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 89 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  1000000 + 89, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);


-- ==== user-merchant-core ====
SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `user_merchant_db_8` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `user_merchant_db_8`;

-- 分片表 schema 模板。8 和 80 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   80  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 80)';

CREATE TABLE IF NOT EXISTS `user_profiles_80` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 80)';

CREATE TABLE IF NOT EXISTS `user_auths_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 80)';

CREATE TABLE IF NOT EXISTS `login_logs_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 80)';

CREATE TABLE IF NOT EXISTS `user_sessions_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 80)';

CREATE TABLE IF NOT EXISTS `user_roles_80` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 80)';

CREATE TABLE IF NOT EXISTS `user_accounts_80` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 80)';

CREATE TABLE IF NOT EXISTS `user_settings_80` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 80)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 80)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 80)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 80)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 80)';

-- ─── admin_audit_log_80：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 80';

-- 分片表 schema 模板。8 和 81 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   81  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 81)';

CREATE TABLE IF NOT EXISTS `user_profiles_81` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 81)';

CREATE TABLE IF NOT EXISTS `user_auths_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 81)';

CREATE TABLE IF NOT EXISTS `login_logs_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 81)';

CREATE TABLE IF NOT EXISTS `user_sessions_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 81)';

CREATE TABLE IF NOT EXISTS `user_roles_81` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 81)';

CREATE TABLE IF NOT EXISTS `user_accounts_81` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 81)';

CREATE TABLE IF NOT EXISTS `user_settings_81` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 81)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 81)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 81)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 81)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 81)';

-- ─── admin_audit_log_81：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 81';

-- 分片表 schema 模板。8 和 82 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   82  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 82)';

CREATE TABLE IF NOT EXISTS `user_profiles_82` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 82)';

CREATE TABLE IF NOT EXISTS `user_auths_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 82)';

CREATE TABLE IF NOT EXISTS `login_logs_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 82)';

CREATE TABLE IF NOT EXISTS `user_sessions_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 82)';

CREATE TABLE IF NOT EXISTS `user_roles_82` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 82)';

CREATE TABLE IF NOT EXISTS `user_accounts_82` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 82)';

CREATE TABLE IF NOT EXISTS `user_settings_82` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 82)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 82)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 82)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 82)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 82)';

-- ─── admin_audit_log_82：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 82';

-- 分片表 schema 模板。8 和 83 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   83  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 83)';

CREATE TABLE IF NOT EXISTS `user_profiles_83` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 83)';

CREATE TABLE IF NOT EXISTS `user_auths_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 83)';

CREATE TABLE IF NOT EXISTS `login_logs_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 83)';

CREATE TABLE IF NOT EXISTS `user_sessions_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 83)';

CREATE TABLE IF NOT EXISTS `user_roles_83` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 83)';

CREATE TABLE IF NOT EXISTS `user_accounts_83` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 83)';

CREATE TABLE IF NOT EXISTS `user_settings_83` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 83)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 83)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 83)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 83)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 83)';

-- ─── admin_audit_log_83：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 83';

-- 分片表 schema 模板。8 和 84 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   84  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 84)';

CREATE TABLE IF NOT EXISTS `user_profiles_84` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 84)';

CREATE TABLE IF NOT EXISTS `user_auths_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 84)';

CREATE TABLE IF NOT EXISTS `login_logs_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 84)';

CREATE TABLE IF NOT EXISTS `user_sessions_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 84)';

CREATE TABLE IF NOT EXISTS `user_roles_84` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 84)';

CREATE TABLE IF NOT EXISTS `user_accounts_84` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 84)';

CREATE TABLE IF NOT EXISTS `user_settings_84` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 84)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 84)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 84)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 84)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 84)';

-- ─── admin_audit_log_84：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 84';

-- 分片表 schema 模板。8 和 85 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   85  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 85)';

CREATE TABLE IF NOT EXISTS `user_profiles_85` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 85)';

CREATE TABLE IF NOT EXISTS `user_auths_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 85)';

CREATE TABLE IF NOT EXISTS `login_logs_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 85)';

CREATE TABLE IF NOT EXISTS `user_sessions_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 85)';

CREATE TABLE IF NOT EXISTS `user_roles_85` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 85)';

CREATE TABLE IF NOT EXISTS `user_accounts_85` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 85)';

CREATE TABLE IF NOT EXISTS `user_settings_85` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 85)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 85)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 85)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 85)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 85)';

-- ─── admin_audit_log_85：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 85';

-- 分片表 schema 模板。8 和 86 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   86  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 86)';

CREATE TABLE IF NOT EXISTS `user_profiles_86` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 86)';

CREATE TABLE IF NOT EXISTS `user_auths_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 86)';

CREATE TABLE IF NOT EXISTS `login_logs_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 86)';

CREATE TABLE IF NOT EXISTS `user_sessions_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 86)';

CREATE TABLE IF NOT EXISTS `user_roles_86` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 86)';

CREATE TABLE IF NOT EXISTS `user_accounts_86` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 86)';

CREATE TABLE IF NOT EXISTS `user_settings_86` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 86)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 86)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 86)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 86)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 86)';

-- ─── admin_audit_log_86：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 86';

-- 分片表 schema 模板。8 和 87 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   87  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 87)';

CREATE TABLE IF NOT EXISTS `user_profiles_87` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 87)';

CREATE TABLE IF NOT EXISTS `user_auths_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 87)';

CREATE TABLE IF NOT EXISTS `login_logs_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 87)';

CREATE TABLE IF NOT EXISTS `user_sessions_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 87)';

CREATE TABLE IF NOT EXISTS `user_roles_87` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 87)';

CREATE TABLE IF NOT EXISTS `user_accounts_87` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 87)';

CREATE TABLE IF NOT EXISTS `user_settings_87` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 87)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 87)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 87)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 87)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 87)';

-- ─── admin_audit_log_87：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 87';

-- 分片表 schema 模板。8 和 88 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   88  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 88)';

CREATE TABLE IF NOT EXISTS `user_profiles_88` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 88)';

CREATE TABLE IF NOT EXISTS `user_auths_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 88)';

CREATE TABLE IF NOT EXISTS `login_logs_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 88)';

CREATE TABLE IF NOT EXISTS `user_sessions_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 88)';

CREATE TABLE IF NOT EXISTS `user_roles_88` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 88)';

CREATE TABLE IF NOT EXISTS `user_accounts_88` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 88)';

CREATE TABLE IF NOT EXISTS `user_settings_88` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 88)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 88)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 88)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 88)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 88)';

-- ─── admin_audit_log_88：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 88';

-- 分片表 schema 模板。8 和 89 由 generate.sh 替换：
--   8     = 0..9         分库 idx
--   89  = 00..99       全局表 idx (zero-padded)
--
-- 路由：globalTblIdx = id % 100；dbIdx = globalTblIdx / 10
--
-- 9 张分片表：
--   按 user_id 分片：users / user_profiles / user_auths / login_logs /
--                    user_sessions / user_roles / user_accounts / user_settings
--   按 merchant_id 分片：merchants / merchant_kyc_document / merchant_channel_secret

-- ─── users 分片表 ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS `users_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户 (shard 89)';

CREATE TABLE IF NOT EXISTS `user_profiles_89` (
    `user_id`   BIGINT       NOT NULL,
    `nickname`  VARCHAR(50)  NULL,
    `avatar`    VARCHAR(255) NULL,
    `gender`    TINYINT      NOT NULL DEFAULT 0,
    `birthday`  DATE         NULL,
    `bio`       TEXT         NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户资料 (shard 89)';

CREATE TABLE IF NOT EXISTS `user_auths_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='多渠道认证 (shard 89)';

CREATE TABLE IF NOT EXISTS `login_logs_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录日志 (shard 89)';

CREATE TABLE IF NOT EXISTS `user_sessions_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='登录会话 (shard 89)';

CREATE TABLE IF NOT EXISTS `user_roles_89` (
    `user_id`  BIGINT NOT NULL,
    `role_id`  BIGINT NOT NULL,
    PRIMARY KEY (`user_id`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='RBAC user-role (shard 89)';

CREATE TABLE IF NOT EXISTS `user_accounts_89` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`     BIGINT       NOT NULL,
    `currency`    VARCHAR(8)   NOT NULL,
    `account_id`  VARCHAR(64)  NOT NULL,
    `status`      VARCHAR(16)  NOT NULL DEFAULT 'active',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_user_currency` (`user_id`, `currency`),
    KEY `idx_account_id` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='C 端用户多币种账户引用 (shard 89)';

CREATE TABLE IF NOT EXISTS `user_settings_89` (
    `user_id`   BIGINT NOT NULL,
    `settings`  JSON   NULL,
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='UI 偏好 (shard 89)';

-- ─── user_card：用户存卡引用（指向 card-center 的 stored_token）──────────────
-- 本表**不存** PAN / CVV / 任何敏感数据；只存 card-center 颁发的 stored_token
-- 加业务展示字段（masked_pan / network / 过期月年 / 持卡人姓名）。
-- 真实 PAN 在 card-center 内的 KMS-encrypted token 里，本服务永不见。
-- 删除 = soft delete（status=deleted + deleted_at）。物理失效靠 card-center
-- 那边的 KMS key rotation。
CREATE TABLE IF NOT EXISTS `user_card_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card index (NO PAN, only token + masked) (shard 89)';

-- ─── merchants 分片表（按 merchant_id 路由）─────────────────────────────────
CREATE TABLE IF NOT EXISTS `merchants_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户主表 (shard 89)';

CREATE TABLE IF NOT EXISTS `merchant_kyc_document_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户 KYC 文档 (shard 89)';

CREATE TABLE IF NOT EXISTS `merchant_channel_secret_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='商户渠道凭据 (shard 89)';

-- ─── admin_audit_log_89：管理台 / 用户操作审计（按 actor 路由） ────────
-- 从 meta 移到 shard 解 10K TPS 写瓶颈。actor 哈希路由：同一 admin/user
-- 的所有操作落同一 shard，取证查全。链式签名 per-shard。
CREATE TABLE IF NOT EXISTS `admin_audit_log_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='admin 审计日志 shard 89';


-- ==== payment-channel _shadow ====
-- paychan_db_8 的影子表（压测 / shadow 流量）
-- 依赖：8_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_8`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_80_shadow` LIKE `acquirer_tx_80`;
CREATE TABLE IF NOT EXISTS `webhook_raw_80_shadow` LIKE `webhook_raw_80`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_80_shadow` LIKE `webhook_raw_rejected_80`;
CREATE TABLE IF NOT EXISTS `channel_token_80_shadow` LIKE `channel_token_80`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_81_shadow` LIKE `acquirer_tx_81`;
CREATE TABLE IF NOT EXISTS `webhook_raw_81_shadow` LIKE `webhook_raw_81`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_81_shadow` LIKE `webhook_raw_rejected_81`;
CREATE TABLE IF NOT EXISTS `channel_token_81_shadow` LIKE `channel_token_81`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_82_shadow` LIKE `acquirer_tx_82`;
CREATE TABLE IF NOT EXISTS `webhook_raw_82_shadow` LIKE `webhook_raw_82`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_82_shadow` LIKE `webhook_raw_rejected_82`;
CREATE TABLE IF NOT EXISTS `channel_token_82_shadow` LIKE `channel_token_82`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_83_shadow` LIKE `acquirer_tx_83`;
CREATE TABLE IF NOT EXISTS `webhook_raw_83_shadow` LIKE `webhook_raw_83`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_83_shadow` LIKE `webhook_raw_rejected_83`;
CREATE TABLE IF NOT EXISTS `channel_token_83_shadow` LIKE `channel_token_83`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_84_shadow` LIKE `acquirer_tx_84`;
CREATE TABLE IF NOT EXISTS `webhook_raw_84_shadow` LIKE `webhook_raw_84`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_84_shadow` LIKE `webhook_raw_rejected_84`;
CREATE TABLE IF NOT EXISTS `channel_token_84_shadow` LIKE `channel_token_84`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_85_shadow` LIKE `acquirer_tx_85`;
CREATE TABLE IF NOT EXISTS `webhook_raw_85_shadow` LIKE `webhook_raw_85`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_85_shadow` LIKE `webhook_raw_rejected_85`;
CREATE TABLE IF NOT EXISTS `channel_token_85_shadow` LIKE `channel_token_85`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_86_shadow` LIKE `acquirer_tx_86`;
CREATE TABLE IF NOT EXISTS `webhook_raw_86_shadow` LIKE `webhook_raw_86`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_86_shadow` LIKE `webhook_raw_rejected_86`;
CREATE TABLE IF NOT EXISTS `channel_token_86_shadow` LIKE `channel_token_86`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_87_shadow` LIKE `acquirer_tx_87`;
CREATE TABLE IF NOT EXISTS `webhook_raw_87_shadow` LIKE `webhook_raw_87`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_87_shadow` LIKE `webhook_raw_rejected_87`;
CREATE TABLE IF NOT EXISTS `channel_token_87_shadow` LIKE `channel_token_87`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_88_shadow` LIKE `acquirer_tx_88`;
CREATE TABLE IF NOT EXISTS `webhook_raw_88_shadow` LIKE `webhook_raw_88`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_88_shadow` LIKE `webhook_raw_rejected_88`;
CREATE TABLE IF NOT EXISTS `channel_token_88_shadow` LIKE `channel_token_88`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_89_shadow` LIKE `acquirer_tx_89`;
CREATE TABLE IF NOT EXISTS `webhook_raw_89_shadow` LIKE `webhook_raw_89`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_89_shadow` LIKE `webhook_raw_rejected_89`;
CREATE TABLE IF NOT EXISTS `channel_token_89_shadow` LIKE `channel_token_89`;


-- ==== order-core _shadow ====
-- order_db_8 的影子表（压测 / shadow 流量）
-- 依赖：8_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_8`;

CREATE TABLE IF NOT EXISTS `payment_intent_80_shadow` LIKE `payment_intent_80`;
CREATE TABLE IF NOT EXISTS `charge_80_shadow` LIKE `charge_80`;
CREATE TABLE IF NOT EXISTS `refund_80_shadow` LIKE `refund_80`;
CREATE TABLE IF NOT EXISTS `pay_action_80_shadow` LIKE `pay_action_80`;
CREATE TABLE IF NOT EXISTS `dispute_80_shadow` LIKE `dispute_80`;
CREATE TABLE IF NOT EXISTS `dispute_event_80_shadow` LIKE `dispute_event_80`;
CREATE TABLE IF NOT EXISTS `exception_case_80_shadow` LIKE `exception_case_80`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_80_shadow` LIKE `inbound_webhook_80`;
CREATE TABLE IF NOT EXISTS `notify_log_80_shadow` LIKE `notify_log_80`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_80_shadow` LIKE `accounting_outbox_80`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_80_shadow` LIKE `admin_audit_log_80`;

CREATE TABLE IF NOT EXISTS `payment_intent_81_shadow` LIKE `payment_intent_81`;
CREATE TABLE IF NOT EXISTS `charge_81_shadow` LIKE `charge_81`;
CREATE TABLE IF NOT EXISTS `refund_81_shadow` LIKE `refund_81`;
CREATE TABLE IF NOT EXISTS `pay_action_81_shadow` LIKE `pay_action_81`;
CREATE TABLE IF NOT EXISTS `dispute_81_shadow` LIKE `dispute_81`;
CREATE TABLE IF NOT EXISTS `dispute_event_81_shadow` LIKE `dispute_event_81`;
CREATE TABLE IF NOT EXISTS `exception_case_81_shadow` LIKE `exception_case_81`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_81_shadow` LIKE `inbound_webhook_81`;
CREATE TABLE IF NOT EXISTS `notify_log_81_shadow` LIKE `notify_log_81`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_81_shadow` LIKE `accounting_outbox_81`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_81_shadow` LIKE `admin_audit_log_81`;

CREATE TABLE IF NOT EXISTS `payment_intent_82_shadow` LIKE `payment_intent_82`;
CREATE TABLE IF NOT EXISTS `charge_82_shadow` LIKE `charge_82`;
CREATE TABLE IF NOT EXISTS `refund_82_shadow` LIKE `refund_82`;
CREATE TABLE IF NOT EXISTS `pay_action_82_shadow` LIKE `pay_action_82`;
CREATE TABLE IF NOT EXISTS `dispute_82_shadow` LIKE `dispute_82`;
CREATE TABLE IF NOT EXISTS `dispute_event_82_shadow` LIKE `dispute_event_82`;
CREATE TABLE IF NOT EXISTS `exception_case_82_shadow` LIKE `exception_case_82`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_82_shadow` LIKE `inbound_webhook_82`;
CREATE TABLE IF NOT EXISTS `notify_log_82_shadow` LIKE `notify_log_82`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_82_shadow` LIKE `accounting_outbox_82`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_82_shadow` LIKE `admin_audit_log_82`;

CREATE TABLE IF NOT EXISTS `payment_intent_83_shadow` LIKE `payment_intent_83`;
CREATE TABLE IF NOT EXISTS `charge_83_shadow` LIKE `charge_83`;
CREATE TABLE IF NOT EXISTS `refund_83_shadow` LIKE `refund_83`;
CREATE TABLE IF NOT EXISTS `pay_action_83_shadow` LIKE `pay_action_83`;
CREATE TABLE IF NOT EXISTS `dispute_83_shadow` LIKE `dispute_83`;
CREATE TABLE IF NOT EXISTS `dispute_event_83_shadow` LIKE `dispute_event_83`;
CREATE TABLE IF NOT EXISTS `exception_case_83_shadow` LIKE `exception_case_83`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_83_shadow` LIKE `inbound_webhook_83`;
CREATE TABLE IF NOT EXISTS `notify_log_83_shadow` LIKE `notify_log_83`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_83_shadow` LIKE `accounting_outbox_83`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_83_shadow` LIKE `admin_audit_log_83`;

CREATE TABLE IF NOT EXISTS `payment_intent_84_shadow` LIKE `payment_intent_84`;
CREATE TABLE IF NOT EXISTS `charge_84_shadow` LIKE `charge_84`;
CREATE TABLE IF NOT EXISTS `refund_84_shadow` LIKE `refund_84`;
CREATE TABLE IF NOT EXISTS `pay_action_84_shadow` LIKE `pay_action_84`;
CREATE TABLE IF NOT EXISTS `dispute_84_shadow` LIKE `dispute_84`;
CREATE TABLE IF NOT EXISTS `dispute_event_84_shadow` LIKE `dispute_event_84`;
CREATE TABLE IF NOT EXISTS `exception_case_84_shadow` LIKE `exception_case_84`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_84_shadow` LIKE `inbound_webhook_84`;
CREATE TABLE IF NOT EXISTS `notify_log_84_shadow` LIKE `notify_log_84`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_84_shadow` LIKE `accounting_outbox_84`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_84_shadow` LIKE `admin_audit_log_84`;

CREATE TABLE IF NOT EXISTS `payment_intent_85_shadow` LIKE `payment_intent_85`;
CREATE TABLE IF NOT EXISTS `charge_85_shadow` LIKE `charge_85`;
CREATE TABLE IF NOT EXISTS `refund_85_shadow` LIKE `refund_85`;
CREATE TABLE IF NOT EXISTS `pay_action_85_shadow` LIKE `pay_action_85`;
CREATE TABLE IF NOT EXISTS `dispute_85_shadow` LIKE `dispute_85`;
CREATE TABLE IF NOT EXISTS `dispute_event_85_shadow` LIKE `dispute_event_85`;
CREATE TABLE IF NOT EXISTS `exception_case_85_shadow` LIKE `exception_case_85`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_85_shadow` LIKE `inbound_webhook_85`;
CREATE TABLE IF NOT EXISTS `notify_log_85_shadow` LIKE `notify_log_85`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_85_shadow` LIKE `accounting_outbox_85`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_85_shadow` LIKE `admin_audit_log_85`;

CREATE TABLE IF NOT EXISTS `payment_intent_86_shadow` LIKE `payment_intent_86`;
CREATE TABLE IF NOT EXISTS `charge_86_shadow` LIKE `charge_86`;
CREATE TABLE IF NOT EXISTS `refund_86_shadow` LIKE `refund_86`;
CREATE TABLE IF NOT EXISTS `pay_action_86_shadow` LIKE `pay_action_86`;
CREATE TABLE IF NOT EXISTS `dispute_86_shadow` LIKE `dispute_86`;
CREATE TABLE IF NOT EXISTS `dispute_event_86_shadow` LIKE `dispute_event_86`;
CREATE TABLE IF NOT EXISTS `exception_case_86_shadow` LIKE `exception_case_86`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_86_shadow` LIKE `inbound_webhook_86`;
CREATE TABLE IF NOT EXISTS `notify_log_86_shadow` LIKE `notify_log_86`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_86_shadow` LIKE `accounting_outbox_86`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_86_shadow` LIKE `admin_audit_log_86`;

CREATE TABLE IF NOT EXISTS `payment_intent_87_shadow` LIKE `payment_intent_87`;
CREATE TABLE IF NOT EXISTS `charge_87_shadow` LIKE `charge_87`;
CREATE TABLE IF NOT EXISTS `refund_87_shadow` LIKE `refund_87`;
CREATE TABLE IF NOT EXISTS `pay_action_87_shadow` LIKE `pay_action_87`;
CREATE TABLE IF NOT EXISTS `dispute_87_shadow` LIKE `dispute_87`;
CREATE TABLE IF NOT EXISTS `dispute_event_87_shadow` LIKE `dispute_event_87`;
CREATE TABLE IF NOT EXISTS `exception_case_87_shadow` LIKE `exception_case_87`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_87_shadow` LIKE `inbound_webhook_87`;
CREATE TABLE IF NOT EXISTS `notify_log_87_shadow` LIKE `notify_log_87`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_87_shadow` LIKE `accounting_outbox_87`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_87_shadow` LIKE `admin_audit_log_87`;

CREATE TABLE IF NOT EXISTS `payment_intent_88_shadow` LIKE `payment_intent_88`;
CREATE TABLE IF NOT EXISTS `charge_88_shadow` LIKE `charge_88`;
CREATE TABLE IF NOT EXISTS `refund_88_shadow` LIKE `refund_88`;
CREATE TABLE IF NOT EXISTS `pay_action_88_shadow` LIKE `pay_action_88`;
CREATE TABLE IF NOT EXISTS `dispute_88_shadow` LIKE `dispute_88`;
CREATE TABLE IF NOT EXISTS `dispute_event_88_shadow` LIKE `dispute_event_88`;
CREATE TABLE IF NOT EXISTS `exception_case_88_shadow` LIKE `exception_case_88`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_88_shadow` LIKE `inbound_webhook_88`;
CREATE TABLE IF NOT EXISTS `notify_log_88_shadow` LIKE `notify_log_88`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_88_shadow` LIKE `accounting_outbox_88`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_88_shadow` LIKE `admin_audit_log_88`;

CREATE TABLE IF NOT EXISTS `payment_intent_89_shadow` LIKE `payment_intent_89`;
CREATE TABLE IF NOT EXISTS `charge_89_shadow` LIKE `charge_89`;
CREATE TABLE IF NOT EXISTS `refund_89_shadow` LIKE `refund_89`;
CREATE TABLE IF NOT EXISTS `pay_action_89_shadow` LIKE `pay_action_89`;
CREATE TABLE IF NOT EXISTS `dispute_89_shadow` LIKE `dispute_89`;
CREATE TABLE IF NOT EXISTS `dispute_event_89_shadow` LIKE `dispute_event_89`;
CREATE TABLE IF NOT EXISTS `exception_case_89_shadow` LIKE `exception_case_89`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_89_shadow` LIKE `inbound_webhook_89`;
CREATE TABLE IF NOT EXISTS `notify_log_89_shadow` LIKE `notify_log_89`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_89_shadow` LIKE `accounting_outbox_89`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_89_shadow` LIKE `admin_audit_log_89`;


-- ==== accounting-system _shadow ====
-- accounting_db_8 的影子表（压测 / shadow 流量）
-- 依赖：8_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
SET NAMES utf8mb4;
USE `accounting_db_8`;

CREATE TABLE IF NOT EXISTS `account_80_shadow` LIKE `account_80`;
CREATE TABLE IF NOT EXISTS `account_transaction_80_shadow` LIKE `account_transaction_80`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_80_shadow` LIKE `accounting_voucher_80`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_80_shadow` LIKE `account_balance_snapshot_80`;
CREATE TABLE IF NOT EXISTS `day_cut_control_80_shadow` LIKE `day_cut_control_80`;
CREATE TABLE IF NOT EXISTS `async_task_80_shadow` LIKE `async_task_80`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_80_shadow` LIKE `tcc_transaction_80`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_80_shadow` LIKE `freeze_compensate_outbox_80`;
CREATE TABLE IF NOT EXISTS `distributed_lock_80_shadow` LIKE `distributed_lock_80`;
CREATE TABLE IF NOT EXISTS `merchant_info_80_shadow` LIKE `merchant_info_80`;
CREATE TABLE IF NOT EXISTS `transaction_order_80_shadow` LIKE `transaction_order_80`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_80_shadow` LIKE `transaction_order_extra_80`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_80_shadow` LIKE `account_balance_buffer_80`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_80_shadow` LIKE `tcc_coordinator_80`;
CREATE TABLE IF NOT EXISTS `batch_order_80_shadow` LIKE `batch_order_80`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_80_shadow` LIKE `settlement_outbox_80`;

CREATE TABLE IF NOT EXISTS `account_81_shadow` LIKE `account_81`;
CREATE TABLE IF NOT EXISTS `account_transaction_81_shadow` LIKE `account_transaction_81`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_81_shadow` LIKE `accounting_voucher_81`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_81_shadow` LIKE `account_balance_snapshot_81`;
CREATE TABLE IF NOT EXISTS `day_cut_control_81_shadow` LIKE `day_cut_control_81`;
CREATE TABLE IF NOT EXISTS `async_task_81_shadow` LIKE `async_task_81`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_81_shadow` LIKE `tcc_transaction_81`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_81_shadow` LIKE `freeze_compensate_outbox_81`;
CREATE TABLE IF NOT EXISTS `distributed_lock_81_shadow` LIKE `distributed_lock_81`;
CREATE TABLE IF NOT EXISTS `merchant_info_81_shadow` LIKE `merchant_info_81`;
CREATE TABLE IF NOT EXISTS `transaction_order_81_shadow` LIKE `transaction_order_81`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_81_shadow` LIKE `transaction_order_extra_81`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_81_shadow` LIKE `account_balance_buffer_81`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_81_shadow` LIKE `tcc_coordinator_81`;
CREATE TABLE IF NOT EXISTS `batch_order_81_shadow` LIKE `batch_order_81`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_81_shadow` LIKE `settlement_outbox_81`;

CREATE TABLE IF NOT EXISTS `account_82_shadow` LIKE `account_82`;
CREATE TABLE IF NOT EXISTS `account_transaction_82_shadow` LIKE `account_transaction_82`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_82_shadow` LIKE `accounting_voucher_82`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_82_shadow` LIKE `account_balance_snapshot_82`;
CREATE TABLE IF NOT EXISTS `day_cut_control_82_shadow` LIKE `day_cut_control_82`;
CREATE TABLE IF NOT EXISTS `async_task_82_shadow` LIKE `async_task_82`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_82_shadow` LIKE `tcc_transaction_82`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_82_shadow` LIKE `freeze_compensate_outbox_82`;
CREATE TABLE IF NOT EXISTS `distributed_lock_82_shadow` LIKE `distributed_lock_82`;
CREATE TABLE IF NOT EXISTS `merchant_info_82_shadow` LIKE `merchant_info_82`;
CREATE TABLE IF NOT EXISTS `transaction_order_82_shadow` LIKE `transaction_order_82`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_82_shadow` LIKE `transaction_order_extra_82`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_82_shadow` LIKE `account_balance_buffer_82`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_82_shadow` LIKE `tcc_coordinator_82`;
CREATE TABLE IF NOT EXISTS `batch_order_82_shadow` LIKE `batch_order_82`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_82_shadow` LIKE `settlement_outbox_82`;

CREATE TABLE IF NOT EXISTS `account_83_shadow` LIKE `account_83`;
CREATE TABLE IF NOT EXISTS `account_transaction_83_shadow` LIKE `account_transaction_83`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_83_shadow` LIKE `accounting_voucher_83`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_83_shadow` LIKE `account_balance_snapshot_83`;
CREATE TABLE IF NOT EXISTS `day_cut_control_83_shadow` LIKE `day_cut_control_83`;
CREATE TABLE IF NOT EXISTS `async_task_83_shadow` LIKE `async_task_83`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_83_shadow` LIKE `tcc_transaction_83`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_83_shadow` LIKE `freeze_compensate_outbox_83`;
CREATE TABLE IF NOT EXISTS `distributed_lock_83_shadow` LIKE `distributed_lock_83`;
CREATE TABLE IF NOT EXISTS `merchant_info_83_shadow` LIKE `merchant_info_83`;
CREATE TABLE IF NOT EXISTS `transaction_order_83_shadow` LIKE `transaction_order_83`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_83_shadow` LIKE `transaction_order_extra_83`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_83_shadow` LIKE `account_balance_buffer_83`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_83_shadow` LIKE `tcc_coordinator_83`;
CREATE TABLE IF NOT EXISTS `batch_order_83_shadow` LIKE `batch_order_83`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_83_shadow` LIKE `settlement_outbox_83`;

CREATE TABLE IF NOT EXISTS `account_84_shadow` LIKE `account_84`;
CREATE TABLE IF NOT EXISTS `account_transaction_84_shadow` LIKE `account_transaction_84`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_84_shadow` LIKE `accounting_voucher_84`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_84_shadow` LIKE `account_balance_snapshot_84`;
CREATE TABLE IF NOT EXISTS `day_cut_control_84_shadow` LIKE `day_cut_control_84`;
CREATE TABLE IF NOT EXISTS `async_task_84_shadow` LIKE `async_task_84`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_84_shadow` LIKE `tcc_transaction_84`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_84_shadow` LIKE `freeze_compensate_outbox_84`;
CREATE TABLE IF NOT EXISTS `distributed_lock_84_shadow` LIKE `distributed_lock_84`;
CREATE TABLE IF NOT EXISTS `merchant_info_84_shadow` LIKE `merchant_info_84`;
CREATE TABLE IF NOT EXISTS `transaction_order_84_shadow` LIKE `transaction_order_84`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_84_shadow` LIKE `transaction_order_extra_84`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_84_shadow` LIKE `account_balance_buffer_84`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_84_shadow` LIKE `tcc_coordinator_84`;
CREATE TABLE IF NOT EXISTS `batch_order_84_shadow` LIKE `batch_order_84`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_84_shadow` LIKE `settlement_outbox_84`;

CREATE TABLE IF NOT EXISTS `account_85_shadow` LIKE `account_85`;
CREATE TABLE IF NOT EXISTS `account_transaction_85_shadow` LIKE `account_transaction_85`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_85_shadow` LIKE `accounting_voucher_85`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_85_shadow` LIKE `account_balance_snapshot_85`;
CREATE TABLE IF NOT EXISTS `day_cut_control_85_shadow` LIKE `day_cut_control_85`;
CREATE TABLE IF NOT EXISTS `async_task_85_shadow` LIKE `async_task_85`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_85_shadow` LIKE `tcc_transaction_85`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_85_shadow` LIKE `freeze_compensate_outbox_85`;
CREATE TABLE IF NOT EXISTS `distributed_lock_85_shadow` LIKE `distributed_lock_85`;
CREATE TABLE IF NOT EXISTS `merchant_info_85_shadow` LIKE `merchant_info_85`;
CREATE TABLE IF NOT EXISTS `transaction_order_85_shadow` LIKE `transaction_order_85`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_85_shadow` LIKE `transaction_order_extra_85`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_85_shadow` LIKE `account_balance_buffer_85`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_85_shadow` LIKE `tcc_coordinator_85`;
CREATE TABLE IF NOT EXISTS `batch_order_85_shadow` LIKE `batch_order_85`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_85_shadow` LIKE `settlement_outbox_85`;

CREATE TABLE IF NOT EXISTS `account_86_shadow` LIKE `account_86`;
CREATE TABLE IF NOT EXISTS `account_transaction_86_shadow` LIKE `account_transaction_86`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_86_shadow` LIKE `accounting_voucher_86`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_86_shadow` LIKE `account_balance_snapshot_86`;
CREATE TABLE IF NOT EXISTS `day_cut_control_86_shadow` LIKE `day_cut_control_86`;
CREATE TABLE IF NOT EXISTS `async_task_86_shadow` LIKE `async_task_86`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_86_shadow` LIKE `tcc_transaction_86`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_86_shadow` LIKE `freeze_compensate_outbox_86`;
CREATE TABLE IF NOT EXISTS `distributed_lock_86_shadow` LIKE `distributed_lock_86`;
CREATE TABLE IF NOT EXISTS `merchant_info_86_shadow` LIKE `merchant_info_86`;
CREATE TABLE IF NOT EXISTS `transaction_order_86_shadow` LIKE `transaction_order_86`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_86_shadow` LIKE `transaction_order_extra_86`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_86_shadow` LIKE `account_balance_buffer_86`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_86_shadow` LIKE `tcc_coordinator_86`;
CREATE TABLE IF NOT EXISTS `batch_order_86_shadow` LIKE `batch_order_86`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_86_shadow` LIKE `settlement_outbox_86`;

CREATE TABLE IF NOT EXISTS `account_87_shadow` LIKE `account_87`;
CREATE TABLE IF NOT EXISTS `account_transaction_87_shadow` LIKE `account_transaction_87`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_87_shadow` LIKE `accounting_voucher_87`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_87_shadow` LIKE `account_balance_snapshot_87`;
CREATE TABLE IF NOT EXISTS `day_cut_control_87_shadow` LIKE `day_cut_control_87`;
CREATE TABLE IF NOT EXISTS `async_task_87_shadow` LIKE `async_task_87`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_87_shadow` LIKE `tcc_transaction_87`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_87_shadow` LIKE `freeze_compensate_outbox_87`;
CREATE TABLE IF NOT EXISTS `distributed_lock_87_shadow` LIKE `distributed_lock_87`;
CREATE TABLE IF NOT EXISTS `merchant_info_87_shadow` LIKE `merchant_info_87`;
CREATE TABLE IF NOT EXISTS `transaction_order_87_shadow` LIKE `transaction_order_87`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_87_shadow` LIKE `transaction_order_extra_87`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_87_shadow` LIKE `account_balance_buffer_87`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_87_shadow` LIKE `tcc_coordinator_87`;
CREATE TABLE IF NOT EXISTS `batch_order_87_shadow` LIKE `batch_order_87`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_87_shadow` LIKE `settlement_outbox_87`;

CREATE TABLE IF NOT EXISTS `account_88_shadow` LIKE `account_88`;
CREATE TABLE IF NOT EXISTS `account_transaction_88_shadow` LIKE `account_transaction_88`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_88_shadow` LIKE `accounting_voucher_88`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_88_shadow` LIKE `account_balance_snapshot_88`;
CREATE TABLE IF NOT EXISTS `day_cut_control_88_shadow` LIKE `day_cut_control_88`;
CREATE TABLE IF NOT EXISTS `async_task_88_shadow` LIKE `async_task_88`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_88_shadow` LIKE `tcc_transaction_88`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_88_shadow` LIKE `freeze_compensate_outbox_88`;
CREATE TABLE IF NOT EXISTS `distributed_lock_88_shadow` LIKE `distributed_lock_88`;
CREATE TABLE IF NOT EXISTS `merchant_info_88_shadow` LIKE `merchant_info_88`;
CREATE TABLE IF NOT EXISTS `transaction_order_88_shadow` LIKE `transaction_order_88`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_88_shadow` LIKE `transaction_order_extra_88`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_88_shadow` LIKE `account_balance_buffer_88`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_88_shadow` LIKE `tcc_coordinator_88`;
CREATE TABLE IF NOT EXISTS `batch_order_88_shadow` LIKE `batch_order_88`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_88_shadow` LIKE `settlement_outbox_88`;

CREATE TABLE IF NOT EXISTS `account_89_shadow` LIKE `account_89`;
CREATE TABLE IF NOT EXISTS `account_transaction_89_shadow` LIKE `account_transaction_89`;
CREATE TABLE IF NOT EXISTS `accounting_voucher_89_shadow` LIKE `accounting_voucher_89`;
CREATE TABLE IF NOT EXISTS `account_balance_snapshot_89_shadow` LIKE `account_balance_snapshot_89`;
CREATE TABLE IF NOT EXISTS `day_cut_control_89_shadow` LIKE `day_cut_control_89`;
CREATE TABLE IF NOT EXISTS `async_task_89_shadow` LIKE `async_task_89`;
CREATE TABLE IF NOT EXISTS `tcc_transaction_89_shadow` LIKE `tcc_transaction_89`;
CREATE TABLE IF NOT EXISTS `freeze_compensate_outbox_89_shadow` LIKE `freeze_compensate_outbox_89`;
CREATE TABLE IF NOT EXISTS `distributed_lock_89_shadow` LIKE `distributed_lock_89`;
CREATE TABLE IF NOT EXISTS `merchant_info_89_shadow` LIKE `merchant_info_89`;
CREATE TABLE IF NOT EXISTS `transaction_order_89_shadow` LIKE `transaction_order_89`;
CREATE TABLE IF NOT EXISTS `transaction_order_extra_89_shadow` LIKE `transaction_order_extra_89`;
CREATE TABLE IF NOT EXISTS `account_balance_buffer_89_shadow` LIKE `account_balance_buffer_89`;
CREATE TABLE IF NOT EXISTS `tcc_coordinator_89_shadow` LIKE `tcc_coordinator_89`;
CREATE TABLE IF NOT EXISTS `batch_order_89_shadow` LIKE `batch_order_89`;
CREATE TABLE IF NOT EXISTS `settlement_outbox_89_shadow` LIKE `settlement_outbox_89`;



CREATE TABLE IF NOT EXISTS `tx_account_anchor_80_shadow` LIKE `tx_account_anchor_80`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_80_shadow` LIKE `flow_anchor_route_80`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_81_shadow` LIKE `tx_account_anchor_81`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_81_shadow` LIKE `flow_anchor_route_81`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_82_shadow` LIKE `tx_account_anchor_82`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_82_shadow` LIKE `flow_anchor_route_82`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_83_shadow` LIKE `tx_account_anchor_83`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_83_shadow` LIKE `flow_anchor_route_83`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_84_shadow` LIKE `tx_account_anchor_84`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_84_shadow` LIKE `flow_anchor_route_84`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_85_shadow` LIKE `tx_account_anchor_85`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_85_shadow` LIKE `flow_anchor_route_85`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_86_shadow` LIKE `tx_account_anchor_86`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_86_shadow` LIKE `flow_anchor_route_86`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_87_shadow` LIKE `tx_account_anchor_87`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_87_shadow` LIKE `flow_anchor_route_87`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_88_shadow` LIKE `tx_account_anchor_88`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_88_shadow` LIKE `flow_anchor_route_88`;
CREATE TABLE IF NOT EXISTS `tx_account_anchor_89_shadow` LIKE `tx_account_anchor_89`;
CREATE TABLE IF NOT EXISTS `flow_anchor_route_89_shadow` LIKE `flow_anchor_route_89`;

-- ==== user-merchant-core _shadow ====
-- user_merchant_db_8 的影子表（压测 / shadow 流量）
-- 依赖：8_init.sql 已经导入完成（CREATE TABLE LIKE 需要主表存在）。
SET NAMES utf8mb4;
USE `user_merchant_db_8`;

CREATE TABLE IF NOT EXISTS `users_80_shadow` LIKE `users_80`;
CREATE TABLE IF NOT EXISTS `user_profiles_80_shadow` LIKE `user_profiles_80`;
CREATE TABLE IF NOT EXISTS `user_auths_80_shadow` LIKE `user_auths_80`;
CREATE TABLE IF NOT EXISTS `login_logs_80_shadow` LIKE `login_logs_80`;
CREATE TABLE IF NOT EXISTS `user_sessions_80_shadow` LIKE `user_sessions_80`;
CREATE TABLE IF NOT EXISTS `user_roles_80_shadow` LIKE `user_roles_80`;
CREATE TABLE IF NOT EXISTS `user_accounts_80_shadow` LIKE `user_accounts_80`;
CREATE TABLE IF NOT EXISTS `user_settings_80_shadow` LIKE `user_settings_80`;
CREATE TABLE IF NOT EXISTS `user_card_80_shadow` LIKE `user_card_80`;
CREATE TABLE IF NOT EXISTS `merchants_80_shadow` LIKE `merchants_80`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_80_shadow` LIKE `merchant_kyc_document_80`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_80_shadow` LIKE `merchant_channel_secret_80`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_80_shadow` LIKE `admin_audit_log_80`;

CREATE TABLE IF NOT EXISTS `users_81_shadow` LIKE `users_81`;
CREATE TABLE IF NOT EXISTS `user_profiles_81_shadow` LIKE `user_profiles_81`;
CREATE TABLE IF NOT EXISTS `user_auths_81_shadow` LIKE `user_auths_81`;
CREATE TABLE IF NOT EXISTS `login_logs_81_shadow` LIKE `login_logs_81`;
CREATE TABLE IF NOT EXISTS `user_sessions_81_shadow` LIKE `user_sessions_81`;
CREATE TABLE IF NOT EXISTS `user_roles_81_shadow` LIKE `user_roles_81`;
CREATE TABLE IF NOT EXISTS `user_accounts_81_shadow` LIKE `user_accounts_81`;
CREATE TABLE IF NOT EXISTS `user_settings_81_shadow` LIKE `user_settings_81`;
CREATE TABLE IF NOT EXISTS `user_card_81_shadow` LIKE `user_card_81`;
CREATE TABLE IF NOT EXISTS `merchants_81_shadow` LIKE `merchants_81`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_81_shadow` LIKE `merchant_kyc_document_81`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_81_shadow` LIKE `merchant_channel_secret_81`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_81_shadow` LIKE `admin_audit_log_81`;

CREATE TABLE IF NOT EXISTS `users_82_shadow` LIKE `users_82`;
CREATE TABLE IF NOT EXISTS `user_profiles_82_shadow` LIKE `user_profiles_82`;
CREATE TABLE IF NOT EXISTS `user_auths_82_shadow` LIKE `user_auths_82`;
CREATE TABLE IF NOT EXISTS `login_logs_82_shadow` LIKE `login_logs_82`;
CREATE TABLE IF NOT EXISTS `user_sessions_82_shadow` LIKE `user_sessions_82`;
CREATE TABLE IF NOT EXISTS `user_roles_82_shadow` LIKE `user_roles_82`;
CREATE TABLE IF NOT EXISTS `user_accounts_82_shadow` LIKE `user_accounts_82`;
CREATE TABLE IF NOT EXISTS `user_settings_82_shadow` LIKE `user_settings_82`;
CREATE TABLE IF NOT EXISTS `user_card_82_shadow` LIKE `user_card_82`;
CREATE TABLE IF NOT EXISTS `merchants_82_shadow` LIKE `merchants_82`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_82_shadow` LIKE `merchant_kyc_document_82`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_82_shadow` LIKE `merchant_channel_secret_82`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_82_shadow` LIKE `admin_audit_log_82`;

CREATE TABLE IF NOT EXISTS `users_83_shadow` LIKE `users_83`;
CREATE TABLE IF NOT EXISTS `user_profiles_83_shadow` LIKE `user_profiles_83`;
CREATE TABLE IF NOT EXISTS `user_auths_83_shadow` LIKE `user_auths_83`;
CREATE TABLE IF NOT EXISTS `login_logs_83_shadow` LIKE `login_logs_83`;
CREATE TABLE IF NOT EXISTS `user_sessions_83_shadow` LIKE `user_sessions_83`;
CREATE TABLE IF NOT EXISTS `user_roles_83_shadow` LIKE `user_roles_83`;
CREATE TABLE IF NOT EXISTS `user_accounts_83_shadow` LIKE `user_accounts_83`;
CREATE TABLE IF NOT EXISTS `user_settings_83_shadow` LIKE `user_settings_83`;
CREATE TABLE IF NOT EXISTS `user_card_83_shadow` LIKE `user_card_83`;
CREATE TABLE IF NOT EXISTS `merchants_83_shadow` LIKE `merchants_83`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_83_shadow` LIKE `merchant_kyc_document_83`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_83_shadow` LIKE `merchant_channel_secret_83`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_83_shadow` LIKE `admin_audit_log_83`;

CREATE TABLE IF NOT EXISTS `users_84_shadow` LIKE `users_84`;
CREATE TABLE IF NOT EXISTS `user_profiles_84_shadow` LIKE `user_profiles_84`;
CREATE TABLE IF NOT EXISTS `user_auths_84_shadow` LIKE `user_auths_84`;
CREATE TABLE IF NOT EXISTS `login_logs_84_shadow` LIKE `login_logs_84`;
CREATE TABLE IF NOT EXISTS `user_sessions_84_shadow` LIKE `user_sessions_84`;
CREATE TABLE IF NOT EXISTS `user_roles_84_shadow` LIKE `user_roles_84`;
CREATE TABLE IF NOT EXISTS `user_accounts_84_shadow` LIKE `user_accounts_84`;
CREATE TABLE IF NOT EXISTS `user_settings_84_shadow` LIKE `user_settings_84`;
CREATE TABLE IF NOT EXISTS `user_card_84_shadow` LIKE `user_card_84`;
CREATE TABLE IF NOT EXISTS `merchants_84_shadow` LIKE `merchants_84`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_84_shadow` LIKE `merchant_kyc_document_84`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_84_shadow` LIKE `merchant_channel_secret_84`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_84_shadow` LIKE `admin_audit_log_84`;

CREATE TABLE IF NOT EXISTS `users_85_shadow` LIKE `users_85`;
CREATE TABLE IF NOT EXISTS `user_profiles_85_shadow` LIKE `user_profiles_85`;
CREATE TABLE IF NOT EXISTS `user_auths_85_shadow` LIKE `user_auths_85`;
CREATE TABLE IF NOT EXISTS `login_logs_85_shadow` LIKE `login_logs_85`;
CREATE TABLE IF NOT EXISTS `user_sessions_85_shadow` LIKE `user_sessions_85`;
CREATE TABLE IF NOT EXISTS `user_roles_85_shadow` LIKE `user_roles_85`;
CREATE TABLE IF NOT EXISTS `user_accounts_85_shadow` LIKE `user_accounts_85`;
CREATE TABLE IF NOT EXISTS `user_settings_85_shadow` LIKE `user_settings_85`;
CREATE TABLE IF NOT EXISTS `user_card_85_shadow` LIKE `user_card_85`;
CREATE TABLE IF NOT EXISTS `merchants_85_shadow` LIKE `merchants_85`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_85_shadow` LIKE `merchant_kyc_document_85`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_85_shadow` LIKE `merchant_channel_secret_85`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_85_shadow` LIKE `admin_audit_log_85`;

CREATE TABLE IF NOT EXISTS `users_86_shadow` LIKE `users_86`;
CREATE TABLE IF NOT EXISTS `user_profiles_86_shadow` LIKE `user_profiles_86`;
CREATE TABLE IF NOT EXISTS `user_auths_86_shadow` LIKE `user_auths_86`;
CREATE TABLE IF NOT EXISTS `login_logs_86_shadow` LIKE `login_logs_86`;
CREATE TABLE IF NOT EXISTS `user_sessions_86_shadow` LIKE `user_sessions_86`;
CREATE TABLE IF NOT EXISTS `user_roles_86_shadow` LIKE `user_roles_86`;
CREATE TABLE IF NOT EXISTS `user_accounts_86_shadow` LIKE `user_accounts_86`;
CREATE TABLE IF NOT EXISTS `user_settings_86_shadow` LIKE `user_settings_86`;
CREATE TABLE IF NOT EXISTS `user_card_86_shadow` LIKE `user_card_86`;
CREATE TABLE IF NOT EXISTS `merchants_86_shadow` LIKE `merchants_86`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_86_shadow` LIKE `merchant_kyc_document_86`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_86_shadow` LIKE `merchant_channel_secret_86`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_86_shadow` LIKE `admin_audit_log_86`;

CREATE TABLE IF NOT EXISTS `users_87_shadow` LIKE `users_87`;
CREATE TABLE IF NOT EXISTS `user_profiles_87_shadow` LIKE `user_profiles_87`;
CREATE TABLE IF NOT EXISTS `user_auths_87_shadow` LIKE `user_auths_87`;
CREATE TABLE IF NOT EXISTS `login_logs_87_shadow` LIKE `login_logs_87`;
CREATE TABLE IF NOT EXISTS `user_sessions_87_shadow` LIKE `user_sessions_87`;
CREATE TABLE IF NOT EXISTS `user_roles_87_shadow` LIKE `user_roles_87`;
CREATE TABLE IF NOT EXISTS `user_accounts_87_shadow` LIKE `user_accounts_87`;
CREATE TABLE IF NOT EXISTS `user_settings_87_shadow` LIKE `user_settings_87`;
CREATE TABLE IF NOT EXISTS `user_card_87_shadow` LIKE `user_card_87`;
CREATE TABLE IF NOT EXISTS `merchants_87_shadow` LIKE `merchants_87`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_87_shadow` LIKE `merchant_kyc_document_87`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_87_shadow` LIKE `merchant_channel_secret_87`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_87_shadow` LIKE `admin_audit_log_87`;

CREATE TABLE IF NOT EXISTS `users_88_shadow` LIKE `users_88`;
CREATE TABLE IF NOT EXISTS `user_profiles_88_shadow` LIKE `user_profiles_88`;
CREATE TABLE IF NOT EXISTS `user_auths_88_shadow` LIKE `user_auths_88`;
CREATE TABLE IF NOT EXISTS `login_logs_88_shadow` LIKE `login_logs_88`;
CREATE TABLE IF NOT EXISTS `user_sessions_88_shadow` LIKE `user_sessions_88`;
CREATE TABLE IF NOT EXISTS `user_roles_88_shadow` LIKE `user_roles_88`;
CREATE TABLE IF NOT EXISTS `user_accounts_88_shadow` LIKE `user_accounts_88`;
CREATE TABLE IF NOT EXISTS `user_settings_88_shadow` LIKE `user_settings_88`;
CREATE TABLE IF NOT EXISTS `user_card_88_shadow` LIKE `user_card_88`;
CREATE TABLE IF NOT EXISTS `merchants_88_shadow` LIKE `merchants_88`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_88_shadow` LIKE `merchant_kyc_document_88`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_88_shadow` LIKE `merchant_channel_secret_88`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_88_shadow` LIKE `admin_audit_log_88`;

CREATE TABLE IF NOT EXISTS `users_89_shadow` LIKE `users_89`;
CREATE TABLE IF NOT EXISTS `user_profiles_89_shadow` LIKE `user_profiles_89`;
CREATE TABLE IF NOT EXISTS `user_auths_89_shadow` LIKE `user_auths_89`;
CREATE TABLE IF NOT EXISTS `login_logs_89_shadow` LIKE `login_logs_89`;
CREATE TABLE IF NOT EXISTS `user_sessions_89_shadow` LIKE `user_sessions_89`;
CREATE TABLE IF NOT EXISTS `user_roles_89_shadow` LIKE `user_roles_89`;
CREATE TABLE IF NOT EXISTS `user_accounts_89_shadow` LIKE `user_accounts_89`;
CREATE TABLE IF NOT EXISTS `user_settings_89_shadow` LIKE `user_settings_89`;
CREATE TABLE IF NOT EXISTS `user_card_89_shadow` LIKE `user_card_89`;
CREATE TABLE IF NOT EXISTS `merchants_89_shadow` LIKE `merchants_89`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_89_shadow` LIKE `merchant_kyc_document_89`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_89_shadow` LIKE `merchant_channel_secret_89`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_89_shadow` LIKE `admin_audit_log_89`;


-- ==== card-center ====
SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `card_center_db_8` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_center_db_8`;

-- card-center 分片表模板。8 = 0..9，80 = 00..99（globalTblIdx）。
-- generate.sh 把所有 8 / 80 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_8`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_80` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_80` (
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
CREATE TABLE IF NOT EXISTS `audit_log_80` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 80 (PCI 10.7 7y retention)';

-- card-center 分片表模板。8 = 0..9，81 = 00..99（globalTblIdx）。
-- generate.sh 把所有 8 / 81 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_8`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_81` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_81` (
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
CREATE TABLE IF NOT EXISTS `audit_log_81` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 81 (PCI 10.7 7y retention)';

-- card-center 分片表模板。8 = 0..9，82 = 00..99（globalTblIdx）。
-- generate.sh 把所有 8 / 82 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_8`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_82` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_82` (
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
CREATE TABLE IF NOT EXISTS `audit_log_82` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 82 (PCI 10.7 7y retention)';

-- card-center 分片表模板。8 = 0..9，83 = 00..99（globalTblIdx）。
-- generate.sh 把所有 8 / 83 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_8`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_83` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_83` (
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
CREATE TABLE IF NOT EXISTS `audit_log_83` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 83 (PCI 10.7 7y retention)';

-- card-center 分片表模板。8 = 0..9，84 = 00..99（globalTblIdx）。
-- generate.sh 把所有 8 / 84 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_8`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_84` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_84` (
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
CREATE TABLE IF NOT EXISTS `audit_log_84` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 84 (PCI 10.7 7y retention)';

-- card-center 分片表模板。8 = 0..9，85 = 00..99（globalTblIdx）。
-- generate.sh 把所有 8 / 85 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_8`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_85` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_85` (
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
CREATE TABLE IF NOT EXISTS `audit_log_85` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 85 (PCI 10.7 7y retention)';

-- card-center 分片表模板。8 = 0..9，86 = 00..99（globalTblIdx）。
-- generate.sh 把所有 8 / 86 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_8`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_86` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_86` (
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
CREATE TABLE IF NOT EXISTS `audit_log_86` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 86 (PCI 10.7 7y retention)';

-- card-center 分片表模板。8 = 0..9，87 = 00..99（globalTblIdx）。
-- generate.sh 把所有 8 / 87 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_8`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_87` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_87` (
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
CREATE TABLE IF NOT EXISTS `audit_log_87` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 87 (PCI 10.7 7y retention)';

-- card-center 分片表模板。8 = 0..9，88 = 00..99（globalTblIdx）。
-- generate.sh 把所有 8 / 88 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_8`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_88` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_88` (
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
CREATE TABLE IF NOT EXISTS `audit_log_88` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 88 (PCI 10.7 7y retention)';

-- card-center 分片表模板。8 = 0..9，89 = 00..99（globalTblIdx）。
-- generate.sh 把所有 8 / 89 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_8`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_89` (
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
CREATE TABLE IF NOT EXISTS `card_payment_token_used_89` (
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
CREATE TABLE IF NOT EXISTS `audit_log_89` (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='card-center audit log shard 89 (PCI 10.7 7y retention)';


-- ==== card-payment ====
SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `card_payment_db_8` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_payment_db_8`;

-- card-payment 分片表模板。8 = 0..9，80 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_8`;

CREATE TABLE IF NOT EXISTS `card_transaction_80` (
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

-- card-payment 分片表模板。8 = 0..9，81 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_8`;

CREATE TABLE IF NOT EXISTS `card_transaction_81` (
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

-- card-payment 分片表模板。8 = 0..9，82 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_8`;

CREATE TABLE IF NOT EXISTS `card_transaction_82` (
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

-- card-payment 分片表模板。8 = 0..9，83 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_8`;

CREATE TABLE IF NOT EXISTS `card_transaction_83` (
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

-- card-payment 分片表模板。8 = 0..9，84 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_8`;

CREATE TABLE IF NOT EXISTS `card_transaction_84` (
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

-- card-payment 分片表模板。8 = 0..9，85 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_8`;

CREATE TABLE IF NOT EXISTS `card_transaction_85` (
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

-- card-payment 分片表模板。8 = 0..9，86 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_8`;

CREATE TABLE IF NOT EXISTS `card_transaction_86` (
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

-- card-payment 分片表模板。8 = 0..9，87 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_8`;

CREATE TABLE IF NOT EXISTS `card_transaction_87` (
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

-- card-payment 分片表模板。8 = 0..9，88 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_8`;

CREATE TABLE IF NOT EXISTS `card_transaction_88` (
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

-- card-payment 分片表模板。8 = 0..9，89 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_8`;

CREATE TABLE IF NOT EXISTS `card_transaction_89` (
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
-- card_center_db_8 shadow 表（压测）
USE `card_center_db_8`;

CREATE TABLE IF NOT EXISTS `card_stored_token_80_shadow` LIKE `card_stored_token_80`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_80_shadow` LIKE `card_payment_token_used_80`;
CREATE TABLE IF NOT EXISTS `audit_log_80_shadow` LIKE `audit_log_80`;

CREATE TABLE IF NOT EXISTS `card_stored_token_81_shadow` LIKE `card_stored_token_81`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_81_shadow` LIKE `card_payment_token_used_81`;
CREATE TABLE IF NOT EXISTS `audit_log_81_shadow` LIKE `audit_log_81`;

CREATE TABLE IF NOT EXISTS `card_stored_token_82_shadow` LIKE `card_stored_token_82`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_82_shadow` LIKE `card_payment_token_used_82`;
CREATE TABLE IF NOT EXISTS `audit_log_82_shadow` LIKE `audit_log_82`;

CREATE TABLE IF NOT EXISTS `card_stored_token_83_shadow` LIKE `card_stored_token_83`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_83_shadow` LIKE `card_payment_token_used_83`;
CREATE TABLE IF NOT EXISTS `audit_log_83_shadow` LIKE `audit_log_83`;

CREATE TABLE IF NOT EXISTS `card_stored_token_84_shadow` LIKE `card_stored_token_84`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_84_shadow` LIKE `card_payment_token_used_84`;
CREATE TABLE IF NOT EXISTS `audit_log_84_shadow` LIKE `audit_log_84`;

CREATE TABLE IF NOT EXISTS `card_stored_token_85_shadow` LIKE `card_stored_token_85`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_85_shadow` LIKE `card_payment_token_used_85`;
CREATE TABLE IF NOT EXISTS `audit_log_85_shadow` LIKE `audit_log_85`;

CREATE TABLE IF NOT EXISTS `card_stored_token_86_shadow` LIKE `card_stored_token_86`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_86_shadow` LIKE `card_payment_token_used_86`;
CREATE TABLE IF NOT EXISTS `audit_log_86_shadow` LIKE `audit_log_86`;

CREATE TABLE IF NOT EXISTS `card_stored_token_87_shadow` LIKE `card_stored_token_87`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_87_shadow` LIKE `card_payment_token_used_87`;
CREATE TABLE IF NOT EXISTS `audit_log_87_shadow` LIKE `audit_log_87`;

CREATE TABLE IF NOT EXISTS `card_stored_token_88_shadow` LIKE `card_stored_token_88`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_88_shadow` LIKE `card_payment_token_used_88`;
CREATE TABLE IF NOT EXISTS `audit_log_88_shadow` LIKE `audit_log_88`;

CREATE TABLE IF NOT EXISTS `card_stored_token_89_shadow` LIKE `card_stored_token_89`;
CREATE TABLE IF NOT EXISTS `card_payment_token_used_89_shadow` LIKE `card_payment_token_used_89`;
CREATE TABLE IF NOT EXISTS `audit_log_89_shadow` LIKE `audit_log_89`;


-- ==== card-payment _shadow ====
-- card_payment_db_8 shadow
USE `card_payment_db_8`;

CREATE TABLE IF NOT EXISTS `card_transaction_80_shadow` LIKE `card_transaction_80`;

CREATE TABLE IF NOT EXISTS `card_transaction_81_shadow` LIKE `card_transaction_81`;

CREATE TABLE IF NOT EXISTS `card_transaction_82_shadow` LIKE `card_transaction_82`;

CREATE TABLE IF NOT EXISTS `card_transaction_83_shadow` LIKE `card_transaction_83`;

CREATE TABLE IF NOT EXISTS `card_transaction_84_shadow` LIKE `card_transaction_84`;

CREATE TABLE IF NOT EXISTS `card_transaction_85_shadow` LIKE `card_transaction_85`;

CREATE TABLE IF NOT EXISTS `card_transaction_86_shadow` LIKE `card_transaction_86`;

CREATE TABLE IF NOT EXISTS `card_transaction_87_shadow` LIKE `card_transaction_87`;

CREATE TABLE IF NOT EXISTS `card_transaction_88_shadow` LIKE `card_transaction_88`;

CREATE TABLE IF NOT EXISTS `card_transaction_89_shadow` LIKE `card_transaction_89`;


-- ==== recon_cdc binlog 用户 ====
CREATE USER IF NOT EXISTS 'recon_cdc'@'%' IDENTIFIED WITH mysql_native_password BY 'recon_cdc_pwd';
ALTER USER 'recon_cdc'@'%' IDENTIFIED WITH mysql_native_password BY 'recon_cdc_pwd';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'recon_cdc'@'%';
GRANT SELECT ON *.* TO 'recon_cdc'@'%';
FLUSH PRIVILEGES;
