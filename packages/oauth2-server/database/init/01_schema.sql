-- oauth2-server schema —— 客户端 + revocation 黑名单
--
-- 部署:
--   mysql -h <host> -u root -p < 01_schema.sql

CREATE DATABASE IF NOT EXISTS oauth2db
  DEFAULT CHARACTER SET utf8mb4
  DEFAULT COLLATE utf8mb4_unicode_ci;

USE oauth2db;

CREATE TABLE IF NOT EXISTS clients (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  client_id       VARCHAR(128) NOT NULL,
  secret_hash     VARCHAR(255) NOT NULL,        -- bcrypt
  secret_last4    VARCHAR(8)   NOT NULL,
  name            VARCHAR(255) NOT NULL,
  owner_type      ENUM('merchant','service','ops') NOT NULL,
  owner_id        VARCHAR(128) NOT NULL,
  allowed_scopes  TEXT NOT NULL,                -- 空格分隔
  allowed_ips     TEXT NULL,                    -- 逗号分隔；NULL = 不限
  rate_limit_rps  INT NOT NULL DEFAULT 0,       -- 0 = 走 default
  status          ENUM('active','suspended','revoked') NOT NULL DEFAULT 'active',
  expires_at      DATETIME NULL,
  last_used_at    DATETIME NULL,
  created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_client_id (client_id),
  KEY idx_owner (owner_type, owner_id),
  KEY idx_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS revoked_tokens (
  jti          VARCHAR(128) NOT NULL,
  revoked_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at   DATETIME NOT NULL,               -- token 自身 exp，过期后可清掉
  client_id    VARCHAR(128) NULL,
  PRIMARY KEY (jti),
  KEY idx_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- RSA key 持久化 (HA prod 必须)
CREATE TABLE IF NOT EXISTS rsa_keys (
  kid          VARCHAR(64)  NOT NULL,
  private_pem  TEXT         NOT NULL,            -- 实际生产应加密 (KMS envelope)
  public_pem   TEXT         NOT NULL,
  status       ENUM('active','retired') NOT NULL DEFAULT 'active',
  created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  retired_at   DATETIME NULL,
  PRIMARY KEY (kid),
  KEY idx_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 审计日志：谁、何时、对哪个 client 做了什么
CREATE TABLE IF NOT EXISTS admin_audit (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  occurred_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  actor        VARCHAR(128) NOT NULL,            -- ops email
  action       VARCHAR(64)  NOT NULL,            -- create / rotate / suspend / revoke / key-rotate
  client_id    VARCHAR(128) NULL,
  details      JSON NULL,
  source_ip    VARCHAR(64)  NULL,
  PRIMARY KEY (id),
  KEY idx_occurred (occurred_at),
  KEY idx_client (client_id),
  KEY idx_actor (actor)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
