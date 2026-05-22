-- split-payment / split_payment_meta schema —— 由 docker entrypoint 灌入.
--
-- ⚠ 这次重构（Batch 1）：拆分 meta 库和流水库
--   旧: 单库 split_payment 含 12 张表（meta + 流水混在一起）
--   新: split_payment_meta (4 张 meta 表) + split_payment_db_0..9 (8 张流水表 × 10 子表/库)
--
-- 这里只放低频改动的 meta 表：
--   moneyflow_graphs           Graph DSL（admin-web 维护）
--   moneyflow_graph_versions   SP-7 版本快照
--   connected_accounts         Stripe-style connected account（业务实体，但读多写极少）
--   cron_lease                 X3 多副本 cron 互斥 lease
--
-- 高频流水（runs/transfers/fees/payouts/reversals/sagas/outbox×2）见
-- database/shardb/init/N_init.sql。

CREATE DATABASE IF NOT EXISTS split_payment_meta CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

-- 应用账号 split_user：meta 库 + 10 个 shard 库共享同一账号
-- IF NOT EXISTS + ALTER 保证幂等
CREATE USER IF NOT EXISTS 'split_user'@'%' IDENTIFIED BY 'password';
ALTER USER 'split_user'@'%' IDENTIFIED BY 'password';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, DROP, REFERENCES
  ON split_payment_meta.* TO 'split_user'@'%';
-- shard 库的 grant 由 shardb/init/N_init.sql 自己加
FLUSH PRIVILEGES;

USE split_payment_meta;

-- ─── 1. moneyflow 配置 ─────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS moneyflow_graphs (
    id          BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `key`       VARCHAR(128) NOT NULL,
    name        VARCHAR(256) NOT NULL,
    version     VARCHAR(32)  NOT NULL DEFAULT '1.0.0',
    status      VARCHAR(32)  NOT NULL DEFAULT 'draft',
    owner_type  VARCHAR(32)  DEFAULT NULL,
    owner_id    VARCHAR(128) DEFAULT NULL,
    spec_json   JSON         NOT NULL,
    active_version_id BIGINT DEFAULT NULL,
    created_at  DATETIME     NOT NULL,
    updated_at  DATETIME     NOT NULL,
    UNIQUE KEY uk_key (`key`),
    KEY idx_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_graph_versions (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    graph_id BIGINT NOT NULL,
    version VARCHAR(32) NOT NULL,
    spec_json JSON NOT NULL,
    immutable_at DATETIME,
    created_by VARCHAR(128),
    change_summary VARCHAR(512),
    created_at DATETIME NOT NULL,
    UNIQUE KEY uk_graph_version (graph_id, version),
    KEY idx_graph_created (graph_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ─── 2. Connected accounts（业务实体，读多写少，归 meta）─────────────────

CREATE TABLE IF NOT EXISTS connected_accounts (
    id VARCHAR(64) NOT NULL PRIMARY KEY,
    type VARCHAR(16) NOT NULL,
    country VARCHAR(8) NOT NULL,
    default_currency VARCHAR(8) NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'pending',
    capabilities_json JSON,
    business_profile_json JSON,
    payout_destination_json JSON,
    payout_schedule_json JSON,
    metadata_json JSON,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    KEY idx_status (status),
    KEY idx_country (country)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- shadow 镜像表（全链路压测影子流量用）
CREATE TABLE IF NOT EXISTS connected_accounts_shadow LIKE connected_accounts;

-- ─── 3. Cron lease（X3 多副本互斥）─────────────────────────────────────

CREATE TABLE IF NOT EXISTS cron_lease (
    name         VARCHAR(64)  NOT NULL PRIMARY KEY,
    holder       VARCHAR(128) NOT NULL DEFAULT '',
    leased_until DATETIME     NOT NULL DEFAULT '1970-01-01 00:00:00',
    updated_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
