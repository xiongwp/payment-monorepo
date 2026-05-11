-- oauth2-server 分库分表 schema — 10 库 × 10 表 = 100 个全局分片
--
-- Layout (跟 user-merchant-core / order-core / accounting-system 完全对齐):
--   10 个物理库:  oauth2_db_0 ... oauth2_db_9
--   每库 10 张表 (但表的 idx 是全局 0-99, e.g. oauth2_db_3 持有 clients_30..clients_39)
--   总分片: 100
--
-- 分片策略 (Router.RouteByKey, FNV-1a hash):
--   - clients         按 client_id      hash → 100 shard
--   - revoked_tokens  按 jti            hash → 100 shard
--   - admin_audit     按 actor          hash → 100 shard
--   - rsa_keys        不分 (key 极少, 单表)
--
-- 部署:
--   mysql -h <host> -uroot -p < 01_schema_sharded.sql
--
-- 这份 SQL 用生成器思路写: 每个分片表都是相同 schema + 不同 suffix。
-- 完整 100 张表写下来 ~5000 行, 这里只展示模板 + 第 1/最后 1 个,
-- 真生产用 schema/build.sh 或 Liquibase changeset 批量生成。

-- ──────────────────────────────────────────────────────────────────
-- 第一步: 建 10 个物理库
-- ──────────────────────────────────────────────────────────────────

CREATE DATABASE IF NOT EXISTS oauth2_db_0 DEFAULT CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS oauth2_db_1 DEFAULT CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS oauth2_db_2 DEFAULT CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS oauth2_db_3 DEFAULT CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS oauth2_db_4 DEFAULT CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS oauth2_db_5 DEFAULT CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS oauth2_db_6 DEFAULT CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS oauth2_db_7 DEFAULT CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS oauth2_db_8 DEFAULT CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS oauth2_db_9 DEFAULT CHARACTER SET utf8mb4;

-- ──────────────────────────────────────────────────────────────────
-- 模板表 (跑 generate.sh 把 0/00 替换 0..9 / 00..99 = 100 张)
-- ──────────────────────────────────────────────────────────────────

USE oauth2_db_0;

-- 1) clients_00 (示例; 用 generate.sh 生成 clients_00..99)
CREATE TABLE IF NOT EXISTS `clients_00` (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  client_id       VARCHAR(128) NOT NULL,
  secret_hash     VARCHAR(255) NOT NULL,
  secret_last4    VARCHAR(8)   NOT NULL,
  name            VARCHAR(255) NOT NULL,
  owner_type      ENUM('merchant','service','ops') NOT NULL,
  owner_id        VARCHAR(128) NOT NULL,
  allowed_scopes  TEXT NOT NULL,
  allowed_ips     TEXT NULL,
  rate_limit_rps  INT NOT NULL DEFAULT 0,
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

-- 2) revoked_tokens_00 (高频写; 用 jti 主键避免重复; expires_at 索引给 GC 用)
CREATE TABLE IF NOT EXISTS `revoked_tokens_00` (
  jti          VARCHAR(128) NOT NULL,
  revoked_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at   DATETIME NOT NULL,
  client_id    VARCHAR(128) NULL,
  PRIMARY KEY (jti),
  KEY idx_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 3) admin_audit_00 (跟 user-merchant-core admin_audit_log_00 同样 schema)
CREATE TABLE IF NOT EXISTS `admin_audit_00` (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  occurred_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  actor        VARCHAR(128) NOT NULL,
  action       VARCHAR(64)  NOT NULL,
  client_id    VARCHAR(128) NULL,
  source_ip    VARCHAR(64)  NULL,
  details      JSON NULL,
  prev_hash    CHAR(64)     DEFAULT NULL,
  row_hash     CHAR(64)     DEFAULT NULL,
  PRIMARY KEY (id),
  KEY idx_actor (actor),
  KEY idx_client (client_id),
  KEY idx_occurred (occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ──────────────────────────────────────────────────────────────────
-- 末尾分片 (oauth2_db_9 上的 _99 表) — 仅展示, 中间 98 张跑 generate.sh
-- ──────────────────────────────────────────────────────────────────

USE oauth2_db_9;

CREATE TABLE IF NOT EXISTS `clients_99`         LIKE oauth2_db_0.clients_00;
CREATE TABLE IF NOT EXISTS `revoked_tokens_99`  LIKE oauth2_db_0.revoked_tokens_00;
CREATE TABLE IF NOT EXISTS `admin_audit_99`     LIKE oauth2_db_0.admin_audit_00;

-- ──────────────────────────────────────────────────────────────────
-- 全局单表: rsa_keys (放 oauth2_db_0 / meta 库, 不分片)
-- ──────────────────────────────────────────────────────────────────

USE oauth2_db_0;

CREATE TABLE IF NOT EXISTS `rsa_keys` (
  kid          VARCHAR(64)  NOT NULL,
  private_pem  TEXT         NOT NULL,
  public_pem   TEXT         NOT NULL,
  status       ENUM('active','retired') NOT NULL DEFAULT 'active',
  created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  retired_at   DATETIME NULL,
  PRIMARY KEY (kid),
  KEY idx_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 全局 client 名 → shard 索引 (帮 ops 反查; 跨 shard 用)
CREATE TABLE IF NOT EXISTS `client_index` (
  client_id    VARCHAR(128) NOT NULL,
  db_index     TINYINT      NOT NULL,
  table_index  TINYINT      NOT NULL,
  created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (client_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='全局 client_id → (db,table) 映射; 创建 client 时同步写, 删 client 时同步删';
