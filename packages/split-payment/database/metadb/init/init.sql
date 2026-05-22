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

-- ─── ⚠ TRANSITION（B 路径 Phase 1）─────────────────────────────────────
-- 下面 7 张表本应分片到 split_payment_db_0..9，但 stripe_entities / outbox / saga
-- repo 还没改造（Phase 2 任务）。先在 meta 库里建一套单表骨架，让 worker 周期扫
-- 不报 "table doesn't exist" 错。Phase 2 迁完后 DROP 这一段。
-- 表结构跟 shardb/_template.sql 的 family schema 完全一致（不带 _NN 后缀）。

CREATE TABLE IF NOT EXISTS transfers (
    id VARCHAR(64) NOT NULL PRIMARY KEY,
    transfer_group VARCHAR(64),
    source_account VARCHAR(64) NOT NULL,
    destination_account VARCHAR(64) NOT NULL,
    amount_minor BIGINT NOT NULL,
    currency VARCHAR(8) NOT NULL,
    description VARCHAR(256),
    source_transaction VARCHAR(64),
    application_fee VARCHAR(64),
    status VARCHAR(32) NOT NULL DEFAULT 'created',
    reversed_amount BIGINT NOT NULL DEFAULT 0,
    graph_run_id BIGINT,
    idempotency_key VARCHAR(128),
    metadata_json JSON,
    created_at DATETIME NOT NULL,
    posted_at DATETIME,
    UNIQUE KEY uk_idem (idempotency_key),
    KEY idx_group (transfer_group),
    KEY idx_src_tx (source_transaction),
    KEY idx_dest (destination_account),
    KEY idx_run (graph_run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS application_fees (
    id VARCHAR(64) NOT NULL PRIMARY KEY,
    charge VARCHAR(64) NOT NULL,
    account VARCHAR(64),
    amount_minor BIGINT NOT NULL,
    currency VARCHAR(8) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    refunded_amount BIGINT NOT NULL DEFAULT 0,
    graph_run_id BIGINT,
    idempotency_key VARCHAR(128),
    metadata_json JSON,
    created_at DATETIME NOT NULL,
    UNIQUE KEY uk_idem (idempotency_key),
    KEY idx_charge (charge),
    KEY idx_account (account)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS payouts (
    id VARCHAR(64) NOT NULL PRIMARY KEY,
    account VARCHAR(64) NOT NULL,
    amount_minor BIGINT NOT NULL,
    currency VARCHAR(8) NOT NULL,
    method VARCHAR(16) NOT NULL DEFAULT 'standard',
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    arrival_date DATETIME,
    failure_code VARCHAR(64),
    failure_message TEXT,
    statement_descriptor VARCHAR(128),
    destination_json JSON,
    graph_run_id BIGINT,
    idempotency_key VARCHAR(128),
    metadata_json JSON,
    created_at DATETIME NOT NULL,
    UNIQUE KEY uk_idem (idempotency_key),
    KEY idx_account (account),
    KEY idx_status_arrival (status, arrival_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS reversals (
    id VARCHAR(64) NOT NULL PRIMARY KEY,
    transfer VARCHAR(64) NOT NULL,
    amount_minor BIGINT NOT NULL,
    currency VARCHAR(8) NOT NULL,
    reason VARCHAR(64),
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    failure_message TEXT,
    idempotency_key VARCHAR(128),
    graph_run_id BIGINT,
    metadata_json JSON,
    created_at DATETIME NOT NULL,
    UNIQUE KEY uk_idem (idempotency_key),
    KEY idx_transfer (transfer)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_sagas (
    saga_id        VARCHAR(64) NOT NULL PRIMARY KEY,
    graph_run_id   BIGINT,
    correlation_id VARCHAR(128),
    state          VARCHAR(32) NOT NULL,
    current_step   INT NOT NULL DEFAULT 0,
    steps_json     JSON NOT NULL,
    started_at     DATETIME NOT NULL,
    completed_at   DATETIME,
    updated_at     DATETIME NOT NULL,
    KEY idx_state (state),
    KEY idx_run (graph_run_id),
    KEY idx_started (started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS event_outbox (
    id            BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    event_type    VARCHAR(64) NOT NULL,
    payload_json  JSON NOT NULL,
    status        VARCHAR(32) NOT NULL DEFAULT 'pending',
    retry_count   INT NOT NULL DEFAULT 0,
    max_retry     INT NOT NULL DEFAULT 10,
    last_error    TEXT,
    next_retry_at DATETIME NOT NULL,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    KEY idx_status_next (status, next_retry_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS reversal_retry_outbox (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    reversal_id     VARCHAR(64) NOT NULL,
    transfer_id     VARCHAR(64) NOT NULL,
    delta_minor     BIGINT NOT NULL,
    status          VARCHAR(32) NOT NULL DEFAULT 'pending',
    retry_count     INT NOT NULL DEFAULT 0,
    max_retry       INT NOT NULL DEFAULT 5,
    last_error      TEXT,
    next_retry_at   DATETIME NOT NULL,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_reversal_id (reversal_id),
    KEY idx_status_next (status, next_retry_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ─── 3. Cron lease（X3 多副本互斥）─────────────────────────────────────

CREATE TABLE IF NOT EXISTS cron_lease (
    name         VARCHAR(64)  NOT NULL PRIMARY KEY,
    holder       VARCHAR(128) NOT NULL DEFAULT '',
    leased_until DATETIME     NOT NULL DEFAULT '1970-01-01 00:00:00',
    updated_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
