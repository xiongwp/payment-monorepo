-- ============================================================================
-- split_payment_db_3 —— 高频流水分片库（10 库 × 10 子表/库 = 100 全局表/族）
-- ⚠ 模板文件。由 ../gen.sh 用 sed 替换 3 / {TBLINDICES} 生成 N_init.sql。
--
-- 8 个表族都按 idempotency_key / charge_id hash 路由：
--   hash(key) % 1000 → globalIdx (00..99 实际只用 0..99 因为 table_count=100)
--   dbIdx   = globalIdx / 10  (0..9)
--   tblIdx  = globalIdx       (00..99)
-- 这跟 accounting Router (router.go RouteByID) 完全对齐：
--   total = dbCount(10) * tablePerDB(10) = 100, n = id % 100, db=n/10, tbl=n
--
-- 8 个表族:
--   moneyflow_runs_NN          一次 TriggerEvent 的 RunPlan
--   transfers_NN               Stripe-style transfer
--   application_fees_NN        Stripe-style application fee
--   payouts_NN                 Stripe-style payout
--   reversals_NN               Stripe-style reversal
--   moneyflow_sagas_NN         SP-3A 持久化 saga 状态
--   event_outbox_NN            L5 事件 outbox
--   reversal_retry_outbox_NN   R5 反转重试 outbox
--
-- 注：跨 shard 的 AUTO_INCREMENT 不全局唯一，但每个 graph_run_id 只在同 shard 内被
--    子表引用（同 charge_id hash 必落同 shard），所以 collision 不影响业务。
-- ============================================================================

CREATE DATABASE IF NOT EXISTS split_payment_db_3 CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, DROP, REFERENCES
  ON split_payment_db_3.* TO 'split_user'@'%';
FLUSH PRIVILEGES;

USE split_payment_db_3;

-- ─── moneyflow_runs (10 张主表 + 10 张 _shadow 镜像) ───────────────────────

CREATE TABLE IF NOT EXISTS moneyflow_runs_30 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_30_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_31 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_31_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_32 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_32_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_33 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_33_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_34 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_34_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_35 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_35_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_36 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_36_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_37 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_37_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_38 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_38_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_39 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_39_shadow (
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

-- ─── transfers (10 张主表 + 10 张 _shadow 镜像) ───────────────────────

CREATE TABLE IF NOT EXISTS transfers_30 (
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

CREATE TABLE IF NOT EXISTS transfers_30_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_31 (
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

CREATE TABLE IF NOT EXISTS transfers_31_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_32 (
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

CREATE TABLE IF NOT EXISTS transfers_32_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_33 (
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

CREATE TABLE IF NOT EXISTS transfers_33_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_34 (
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

CREATE TABLE IF NOT EXISTS transfers_34_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_35 (
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

CREATE TABLE IF NOT EXISTS transfers_35_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_36 (
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

CREATE TABLE IF NOT EXISTS transfers_36_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_37 (
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

CREATE TABLE IF NOT EXISTS transfers_37_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_38 (
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

CREATE TABLE IF NOT EXISTS transfers_38_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_39 (
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

CREATE TABLE IF NOT EXISTS transfers_39_shadow (
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

-- ─── application_fees (10 张主表 + 10 张 _shadow 镜像) ───────────────────────

CREATE TABLE IF NOT EXISTS application_fees_30 (
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

CREATE TABLE IF NOT EXISTS application_fees_30_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_31 (
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

CREATE TABLE IF NOT EXISTS application_fees_31_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_32 (
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

CREATE TABLE IF NOT EXISTS application_fees_32_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_33 (
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

CREATE TABLE IF NOT EXISTS application_fees_33_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_34 (
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

CREATE TABLE IF NOT EXISTS application_fees_34_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_35 (
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

CREATE TABLE IF NOT EXISTS application_fees_35_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_36 (
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

CREATE TABLE IF NOT EXISTS application_fees_36_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_37 (
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

CREATE TABLE IF NOT EXISTS application_fees_37_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_38 (
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

CREATE TABLE IF NOT EXISTS application_fees_38_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_39 (
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

CREATE TABLE IF NOT EXISTS application_fees_39_shadow (
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

-- ─── payouts (10 张主表 + 10 张 _shadow 镜像) ───────────────────────

CREATE TABLE IF NOT EXISTS payouts_30 (
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

CREATE TABLE IF NOT EXISTS payouts_30_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_31 (
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

CREATE TABLE IF NOT EXISTS payouts_31_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_32 (
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

CREATE TABLE IF NOT EXISTS payouts_32_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_33 (
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

CREATE TABLE IF NOT EXISTS payouts_33_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_34 (
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

CREATE TABLE IF NOT EXISTS payouts_34_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_35 (
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

CREATE TABLE IF NOT EXISTS payouts_35_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_36 (
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

CREATE TABLE IF NOT EXISTS payouts_36_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_37 (
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

CREATE TABLE IF NOT EXISTS payouts_37_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_38 (
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

CREATE TABLE IF NOT EXISTS payouts_38_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_39 (
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

CREATE TABLE IF NOT EXISTS payouts_39_shadow (
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

-- ─── reversals (10 张主表 + 10 张 _shadow 镜像) ───────────────────────

CREATE TABLE IF NOT EXISTS reversals_30 (
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

CREATE TABLE IF NOT EXISTS reversals_30_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_31 (
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

CREATE TABLE IF NOT EXISTS reversals_31_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_32 (
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

CREATE TABLE IF NOT EXISTS reversals_32_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_33 (
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

CREATE TABLE IF NOT EXISTS reversals_33_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_34 (
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

CREATE TABLE IF NOT EXISTS reversals_34_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_35 (
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

CREATE TABLE IF NOT EXISTS reversals_35_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_36 (
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

CREATE TABLE IF NOT EXISTS reversals_36_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_37 (
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

CREATE TABLE IF NOT EXISTS reversals_37_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_38 (
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

CREATE TABLE IF NOT EXISTS reversals_38_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_39 (
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

CREATE TABLE IF NOT EXISTS reversals_39_shadow (
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

-- ─── moneyflow_sagas (10 张主表 + 10 张 _shadow 镜像) ───────────────────────

CREATE TABLE IF NOT EXISTS moneyflow_sagas_30 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_30_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_31 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_31_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_32 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_32_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_33 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_33_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_34 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_34_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_35 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_35_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_36 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_36_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_37 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_37_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_38 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_38_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_39 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_39_shadow (
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

-- ─── event_outbox (10 张主表 + 10 张 _shadow 镜像) ───────────────────────

CREATE TABLE IF NOT EXISTS event_outbox_30 (
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

CREATE TABLE IF NOT EXISTS event_outbox_30_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_31 (
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

CREATE TABLE IF NOT EXISTS event_outbox_31_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_32 (
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

CREATE TABLE IF NOT EXISTS event_outbox_32_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_33 (
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

CREATE TABLE IF NOT EXISTS event_outbox_33_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_34 (
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

CREATE TABLE IF NOT EXISTS event_outbox_34_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_35 (
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

CREATE TABLE IF NOT EXISTS event_outbox_35_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_36 (
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

CREATE TABLE IF NOT EXISTS event_outbox_36_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_37 (
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

CREATE TABLE IF NOT EXISTS event_outbox_37_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_38 (
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

CREATE TABLE IF NOT EXISTS event_outbox_38_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_39 (
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

CREATE TABLE IF NOT EXISTS event_outbox_39_shadow (
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

-- ─── reversal_retry_outbox (10 张主表 + 10 张 _shadow 镜像) ───────────────────────

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_30 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_30_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_31 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_31_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_32 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_32_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_33 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_33_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_34 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_34_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_35 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_35_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_36 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_36_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_37 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_37_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_38 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_38_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_39 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_39_shadow (
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


