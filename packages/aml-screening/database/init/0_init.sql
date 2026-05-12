-- aml-screening DB schema (单库版; 真生产可 shard by first_letter → 26 库).

CREATE DATABASE IF NOT EXISTS aml_screening DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE aml_screening;

-- ── list_entries: OFAC / EU / UN / UK HMT / PEP / 内部黑名单 ──
CREATE TABLE IF NOT EXISTS `list_entries` (
  `entry_id`         VARCHAR(64)  NOT NULL COMMENT '源给的稳定 ID',
  `source`           VARCHAR(32)  NOT NULL COMMENT 'ofac_sdn / eu_cons / un_sc / pep / internal_block',
  `entity_type`      ENUM('individual','entity','address','vessel','aircraft') NOT NULL,
  `primary_name`     VARCHAR(512) NOT NULL,
  `first_letter`     CHAR(2)      NOT NULL COMMENT '标准化后小写首字符, 给候选粗筛索引',
  `dob`              VARCHAR(16)  NULL,
  `birth_place`      VARCHAR(255) NULL,
  `aliases_json`     JSON         NULL,
  `nationality_json` JSON         NULL,
  `addresses_json`   JSON         NULL,
  `id_docs_json`     JSON         NULL,
  `program`          VARCHAR(255) NULL,
  `remarks`          TEXT         NULL,
  `listed_at`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`       DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`source`, `entry_id`),
  KEY `idx_first_letter` (`first_letter`, `source`),
  KEY `idx_primary_name` (`primary_name`(64)),
  KEY `idx_program`      (`program`)
) ENGINE=InnoDB COMMENT='制裁 / PEP 名单条目';

-- ── screen_results: 历史筛查 (idempotency + audit) ──
CREATE TABLE IF NOT EXISTS `screen_results` (
  `request_id`   VARCHAR(64)  NOT NULL,
  `action`       ENUM('pass','review','block') NOT NULL,
  `highest_hit`  INT          NOT NULL DEFAULT 0,
  `screened_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `latency_ms`   INT          NOT NULL,
  `result_json`  JSON         NOT NULL,
  `request_json` JSON         NOT NULL,
  PRIMARY KEY (`request_id`),
  KEY `idx_action_time` (`action`, `screened_at`)
) ENGINE=InnoDB COMMENT='screen 调用历史';

-- ── hit_records: 待复核 / 已复核命中 ──
CREATE TABLE IF NOT EXISTS `hit_records` (
  `hit_id`           VARCHAR(64)  NOT NULL,
  `request_id`       VARCHAR(64)  NOT NULL,
  `source`           VARCHAR(32)  NOT NULL,
  `confidence`       INT          NOT NULL,
  `matched_on_json`  JSON         NULL,
  `algorithm`        VARCHAR(32)  NOT NULL,
  `state`            ENUM('pending_review','cleared','frozen','escalated') NOT NULL DEFAULT 'pending_review',
  `entry_json`       JSON         NOT NULL COMMENT '命中名单条目快照, 供 review 看',
  `reviewer`         VARCHAR(128) NULL,
  `reason`           VARCHAR(512) NULL,
  `evidence`         TEXT         NULL,
  `created_at`       DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `resolved_at`      DATETIME(3)  NULL,
  PRIMARY KEY (`hit_id`),
  KEY `idx_state_time`  (`state`, `created_at`),
  KEY `idx_request_id`  (`request_id`),
  KEY `idx_source`      (`source`)
) ENGINE=InnoDB COMMENT='screen 命中 + 复核工单';
