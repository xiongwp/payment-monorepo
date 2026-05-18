-- 1_moneyflow_core.sql — split-payment 核心表 (moneyflow_graphs + moneyflow_runs).
--
-- 由 MySQL 容器启动时通过 /docker-entrypoint-initdb.d/ 自动灌入 (跟 card-center /
-- order-core 一致); 应用启动期不再做 DDL.
--
-- 表用途:
--   moneyflow_graphs  : 用户保存的 Graph DSL (key 唯一, status=draft|active|archived)
--   moneyflow_runs    : 一次 TriggerEvent 实例化的 RunPlan (含 movements/voucher/状态)

CREATE TABLE IF NOT EXISTS moneyflow_graphs (
    id          BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `key`       VARCHAR(128) NOT NULL,
    name        VARCHAR(256) NOT NULL,
    version     VARCHAR(32)  NOT NULL DEFAULT '1.0.0',
    status      VARCHAR(32)  NOT NULL DEFAULT 'draft',
    owner_type  VARCHAR(32)  DEFAULT NULL,
    owner_id    VARCHAR(128) DEFAULT NULL,
    spec_json   JSON         NOT NULL,
    -- SP-7: 当前 active 版本指向 moneyflow_graph_versions.id.
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
    -- SP-AC-7 PH3-7: hold-period 字段供 HoldUnstickWorker 用.
    hold_until      DATETIME     DEFAULT NULL,
    hold_released   TINYINT(1)   NOT NULL DEFAULT 0,
    created_at      DATETIME     NOT NULL,
    KEY idx_charge (charge_id),
    KEY idx_graph (graph_id),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
