-- data-rights DB schema. 量小, 单库就够.

CREATE DATABASE IF NOT EXISTS data_rights DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE data_rights;

CREATE TABLE IF NOT EXISTS `requests` (
  `request_id`     VARCHAR(48)  NOT NULL,
  `type`           ENUM('access','erasure','portability','rectification','restriction','objection') NOT NULL,
  `subject_type`   ENUM('merchant','customer','staff') NOT NULL,
  `subject_id`     VARCHAR(128) NOT NULL,
  `subject_email`  VARCHAR(255) NULL,
  `subject_country` CHAR(2)     NULL,
  `jurisdiction`   ENUM('EU','UK','CA','BR','CN','other') NOT NULL,
  `state`          ENUM('received','verifying','collecting','review','approved','fulfilled','rejected','failed','expired') NOT NULL DEFAULT 'received',
  `submitted_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `deadline_at`    DATETIME(3)  NOT NULL COMMENT '默认 +30d (GDPR 法定)',
  `approved_by`    VARCHAR(128) NULL,
  `approved_at`    DATETIME(3)  NULL,
  `fulfilled_at`   DATETIME(3)  NULL,
  `reject_reason`  VARCHAR(512) NULL,
  `export_url`     VARCHAR(512) NULL,
  `export_sha256`  CHAR(64)     NULL,
  `verification_json` JSON      NULL,
  PRIMARY KEY (`request_id`),
  KEY `idx_state_deadline` (`state`, `deadline_at`) COMMENT 'overdue 扫描用',
  KEY `idx_subject` (`subject_type`, `subject_id`),
  KEY `idx_juris_year` (`jurisdiction`, `submitted_at`)
) ENGINE=InnoDB COMMENT='DSAR / RTBF 工单';

CREATE TABLE IF NOT EXISTS `service_statuses` (
  `request_id`     VARCHAR(48)  NOT NULL,
  `service`        VARCHAR(64)  NOT NULL,
  `endpoint`       VARCHAR(255) NOT NULL,
  `state`          VARCHAR(32)  NOT NULL,
  `attempts`       INT          NOT NULL DEFAULT 0,
  `last_attempt_at` DATETIME(3) NULL,
  `error`          VARCHAR(512) NULL,
  `export_size`    BIGINT       NULL,
  `export_sha`     CHAR(64)     NULL,
  `held`           TINYINT(1)   NOT NULL DEFAULT 0,
  `hold_reason`    VARCHAR(512) NULL,
  PRIMARY KEY (`request_id`, `service`),
  KEY `idx_state` (`state`),
  CONSTRAINT `fk_dr_request` FOREIGN KEY (`request_id`) REFERENCES `requests`(`request_id`) ON DELETE CASCADE
) ENGINE=InnoDB COMMENT='per-service fan-out 进度';
