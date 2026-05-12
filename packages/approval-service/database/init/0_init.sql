-- approval-service DB schema.

CREATE DATABASE IF NOT EXISTS approval DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE approval;

CREATE TABLE IF NOT EXISTS `approval_actions` (
  `id`                 VARCHAR(48)  NOT NULL,
  `type`               VARCHAR(64)  NOT NULL,
  `resource`           VARCHAR(128) NOT NULL,
  `requester`          VARCHAR(128) NOT NULL,
  `requester_at`       DATETIME(3)  NOT NULL,
  `request_note`       VARCHAR(512) NULL,
  `required_approvals` TINYINT      NOT NULL DEFAULT 2,
  `state`              VARCHAR(16)  NOT NULL DEFAULT 'pending',
  `payload_json`       JSON         NULL,
  `approvals_json`     JSON         NULL,
  `expires_at`         DATETIME(3)  NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_type_state`     (`type`, `state`),
  KEY `idx_resource`       (`resource`),
  KEY `idx_requester`      (`requester`),
  KEY `idx_requester_at`   (`requester_at`),
  KEY `idx_expires`        (`expires_at`)
) ENGINE=InnoDB COMMENT='approval-service 工单';
