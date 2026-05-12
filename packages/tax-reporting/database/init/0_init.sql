-- tax-reporting DB schema. 真生产 shard by merchant_id → 100 shard.

CREATE DATABASE IF NOT EXISTS tax_reporting DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE tax_reporting;

CREATE TABLE IF NOT EXISTS `payout_events` (
  `event_id`        VARCHAR(64)  NOT NULL,
  `merchant_id`     VARCHAR(64)  NOT NULL,
  `occurred_at`     DATETIME(3)  NOT NULL,
  `gross_amount`    BIGINT       NOT NULL COMMENT '分',
  `net_amount`      BIGINT       NOT NULL,
  `fees_amount`     BIGINT       NOT NULL,
  `currency`        CHAR(3)      NOT NULL,
  `jurisdiction`    VARCHAR(8)   NOT NULL,
  `txn_count`       INT          NOT NULL DEFAULT 1,
  `source`          VARCHAR(32)  NOT NULL,
  `channel`         VARCHAR(32)  NULL,
  `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`event_id`),
  KEY `idx_merchant_year` (`merchant_id`, `occurred_at`),
  KEY `idx_juris_year`    (`jurisdiction`, `occurred_at`)
) ENGINE=InnoDB COMMENT='per-payout 流水 — idempotent by event_id';

CREATE TABLE IF NOT EXISTS `merchant_tax_profile` (
  `merchant_id`     VARCHAR(64)  NOT NULL,
  `legal_name`      VARCHAR(255) NOT NULL,
  `tin_enc`         TEXT         NULL COMMENT 'KMS 加密 EIN/SSN',
  `tin_hash`        CHAR(64)     NULL COMMENT 'sha256 tin 用于去重',
  `tin_type`        ENUM('ein','ssn','itin','foreign') NULL,
  `tax_class`       VARCHAR(32)  NOT NULL DEFAULT 'individual',
  `country`         CHAR(2)      NOT NULL,
  `business_addr`   JSON         NOT NULL,
  `w9_submitted`    TINYINT(1)   NOT NULL DEFAULT 0,
  `w9_date`         DATETIME(3)  NULL,
  `w8_submitted`    TINYINT(1)   NOT NULL DEFAULT 0,
  `w8_date`         DATETIME(3)  NULL,
  `w8_type`         VARCHAR(16)  NULL,
  `vat_number`      VARCHAR(32)  NULL,
  `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`merchant_id`),
  KEY `idx_country`   (`country`),
  KEY `idx_w9`        (`w9_submitted`, `updated_at`)
) ENGINE=InnoDB COMMENT='商户税务档案 (W-9 / W-8 / VAT)';

CREATE TABLE IF NOT EXISTS `annual_aggregate` (
  `merchant_id`    VARCHAR(64)  NOT NULL,
  `year`           SMALLINT     NOT NULL,
  `jurisdiction`   VARCHAR(8)   NOT NULL,
  `currency`       CHAR(3)      NOT NULL,
  `monthly_gross`  JSON         NOT NULL COMMENT '12 个月数组',
  `monthly_count`  JSON         NOT NULL,
  `total_gross`    BIGINT       NOT NULL,
  `total_count`    INT          NOT NULL,
  `by_channel`     JSON         NULL,
  `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`merchant_id`, `year`, `jurisdiction`),
  KEY `idx_year_juris` (`year`, `jurisdiction`, `total_gross`)
) ENGINE=InnoDB COMMENT='年度按月汇总 (PayoutEvent → aggregate)';

CREATE TABLE IF NOT EXISTS `tax_forms` (
  `form_id`        VARCHAR(64)  NOT NULL,
  `form_type`      VARCHAR(16)  NOT NULL COMMENT '1099-k / w-9 / w-8ben ...',
  `merchant_id`    VARCHAR(64)  NOT NULL,
  `year`           SMALLINT     NULL,
  `jurisdiction`   VARCHAR(8)   NULL,
  `gross_amount`   BIGINT       NULL,
  `currency`       CHAR(3)      NULL,
  `status`         ENUM('draft','ready','filed','corrected') NOT NULL DEFAULT 'ready',
  `generated_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `filed_at`       DATETIME(3)  NULL,
  `efile_id`       VARCHAR(128) NULL,
  `pdf_ref`        VARCHAR(512) NULL,
  `payload_json`   JSON         NOT NULL,
  PRIMARY KEY (`form_id`),
  KEY `idx_merchant_year` (`merchant_id`, `year`),
  KEY `idx_status_time`   (`status`, `generated_at`)
) ENGINE=InnoDB COMMENT='生成的税表 (1099-K / W-9 / VAT OSS / ...)';
