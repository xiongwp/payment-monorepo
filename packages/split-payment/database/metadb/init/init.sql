-- split-payment / split_payment meta schema —— 由 stack/init-db/bootstrap.sh 灌入.
--
-- 风格跟 user-merchant-core / payment-channel / order-core / accounting-system /
-- card-center / card-payment 一致 (single init.sql, CREATE IF NOT EXISTS 幂等).
--
-- 表清单 (11 张):
--   moneyflow_graphs        Graph DSL
--   moneyflow_runs          一次 TriggerEvent 的 RunPlan
--   moneyflow_graph_versions SP-7 版本快照
--   connected_accounts      Stripe-style connected account
--   transfers               Stripe-style transfer
--   application_fees        Stripe-style fee
--   payouts                 Stripe-style payout
--   reversals               Stripe-style reversal
--   moneyflow_sagas         SP-3A 持久化 saga 状态
--   event_outbox            L5 事件 outbox
--   reversal_retry_outbox   R5 反转重试 outbox
--   cron_lease              X3 多副本 cron 互斥 lease

CREATE DATABASE IF NOT EXISTS split_payment CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

-- 应用账号 split_user (跟 SPLIT_PAYMENT_DSN 对齐: split_user:password@tcp(shared-meta:3306)/split_payment).
-- IF NOT EXISTS + ALTER 保证幂等; 已存在用户改密码也安全.
CREATE USER IF NOT EXISTS 'split_user'@'%' IDENTIFIED BY 'password';
ALTER USER 'split_user'@'%' IDENTIFIED BY 'password';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, DROP, REFERENCES
  ON split_payment.* TO 'split_user'@'%';
FLUSH PRIVILEGES;

USE split_payment;

-- ─── 1. moneyflow core ─────────────────────────────────────────────────

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

CREATE TABLE IF NOT EXISTS moneyflow_runs (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    graph_id        BIGINT NOT NULL,
    graph_version   VARCHAR(32),
    trigger_event   VARCHAR(64)  NOT NULL,
    charge_id       VARCHAR(128) DEFAULT NULL,
    merchant_id     VARCHAR(128) DEFAULT NULL,
    amount_minor    BIGINT NOT NULL DEFAULT 0,
    currency        VARCHAR(8)   DEFAULT NULL,
    attributes_json JSON         DEFAULT NULL,
    movements_json  JSON         DEFAULT NULL,
    status          VARCHAR(32)  NOT NULL DEFAULT 'created',
    voucher_no      VARCHAR(64)  DEFAULT NULL,
    error_msg       TEXT         DEFAULT NULL,
    trace_id        VARCHAR(64)  DEFAULT NULL,
    hold_until      DATETIME     DEFAULT NULL,
    hold_released   TINYINT(1)   NOT NULL DEFAULT 0,
    created_at      DATETIME     NOT NULL,
    KEY idx_charge (charge_id),
    KEY idx_graph (graph_id),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
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

-- ─── 2. Stripe-style 资金原语 ──────────────────────────────────────────

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

-- ─── 3. Saga store ─────────────────────────────────────────────────────

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

-- ─── 4. Outbox (events + reversal retry) ───────────────────────────────

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

-- ─── 5. Cron lease (X3 多副本互斥) ─────────────────────────────────────

CREATE TABLE IF NOT EXISTS cron_lease (
    name         VARCHAR(64)  NOT NULL PRIMARY KEY,
    holder       VARCHAR(128) NOT NULL DEFAULT '',
    leased_until DATETIME     NOT NULL DEFAULT '1970-01-01 00:00:00',
    updated_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
