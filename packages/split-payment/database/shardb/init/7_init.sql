-- ============================================================================
-- split_payment_db_7 —— 高频流水分片库（10 库 × 10 子表/库 = 100 全局表/族）
-- ⚠ 模板文件。由 ../gen.sh 用 sed 替换 7 / {TBLINDICES} 生成 N_init.sql。
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

CREATE DATABASE IF NOT EXISTS split_payment_db_7 CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, DROP, REFERENCES
  ON split_payment_db_7.* TO 'split_user'@'%';
FLUSH PRIVILEGES;

USE split_payment_db_7;

-- ─── moneyflow_runs (10 张主表 + 10 张 _shadow 镜像) ───────────────────────

CREATE TABLE IF NOT EXISTS moneyflow_runs_70 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_70_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_71 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_71_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_72 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_72_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_73 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_73_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_74 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_74_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_75 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_75_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_76 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_76_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_77 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_77_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_78 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_78_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_79 (
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

CREATE TABLE IF NOT EXISTS moneyflow_runs_79_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_70 (
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

CREATE TABLE IF NOT EXISTS transfers_70_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_71 (
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

CREATE TABLE IF NOT EXISTS transfers_71_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_72 (
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

CREATE TABLE IF NOT EXISTS transfers_72_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_73 (
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

CREATE TABLE IF NOT EXISTS transfers_73_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_74 (
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

CREATE TABLE IF NOT EXISTS transfers_74_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_75 (
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

CREATE TABLE IF NOT EXISTS transfers_75_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_76 (
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

CREATE TABLE IF NOT EXISTS transfers_76_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_77 (
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

CREATE TABLE IF NOT EXISTS transfers_77_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_78 (
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

CREATE TABLE IF NOT EXISTS transfers_78_shadow (
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

CREATE TABLE IF NOT EXISTS transfers_79 (
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

CREATE TABLE IF NOT EXISTS transfers_79_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_70 (
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

CREATE TABLE IF NOT EXISTS application_fees_70_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_71 (
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

CREATE TABLE IF NOT EXISTS application_fees_71_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_72 (
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

CREATE TABLE IF NOT EXISTS application_fees_72_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_73 (
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

CREATE TABLE IF NOT EXISTS application_fees_73_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_74 (
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

CREATE TABLE IF NOT EXISTS application_fees_74_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_75 (
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

CREATE TABLE IF NOT EXISTS application_fees_75_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_76 (
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

CREATE TABLE IF NOT EXISTS application_fees_76_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_77 (
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

CREATE TABLE IF NOT EXISTS application_fees_77_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_78 (
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

CREATE TABLE IF NOT EXISTS application_fees_78_shadow (
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

CREATE TABLE IF NOT EXISTS application_fees_79 (
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

CREATE TABLE IF NOT EXISTS application_fees_79_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_70 (
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

CREATE TABLE IF NOT EXISTS payouts_70_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_71 (
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

CREATE TABLE IF NOT EXISTS payouts_71_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_72 (
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

CREATE TABLE IF NOT EXISTS payouts_72_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_73 (
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

CREATE TABLE IF NOT EXISTS payouts_73_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_74 (
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

CREATE TABLE IF NOT EXISTS payouts_74_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_75 (
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

CREATE TABLE IF NOT EXISTS payouts_75_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_76 (
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

CREATE TABLE IF NOT EXISTS payouts_76_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_77 (
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

CREATE TABLE IF NOT EXISTS payouts_77_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_78 (
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

CREATE TABLE IF NOT EXISTS payouts_78_shadow (
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

CREATE TABLE IF NOT EXISTS payouts_79 (
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

CREATE TABLE IF NOT EXISTS payouts_79_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_70 (
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

CREATE TABLE IF NOT EXISTS reversals_70_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_71 (
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

CREATE TABLE IF NOT EXISTS reversals_71_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_72 (
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

CREATE TABLE IF NOT EXISTS reversals_72_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_73 (
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

CREATE TABLE IF NOT EXISTS reversals_73_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_74 (
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

CREATE TABLE IF NOT EXISTS reversals_74_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_75 (
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

CREATE TABLE IF NOT EXISTS reversals_75_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_76 (
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

CREATE TABLE IF NOT EXISTS reversals_76_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_77 (
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

CREATE TABLE IF NOT EXISTS reversals_77_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_78 (
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

CREATE TABLE IF NOT EXISTS reversals_78_shadow (
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

CREATE TABLE IF NOT EXISTS reversals_79 (
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

CREATE TABLE IF NOT EXISTS reversals_79_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_70 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_70_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_71 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_71_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_72 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_72_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_73 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_73_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_74 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_74_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_75 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_75_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_76 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_76_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_77 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_77_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_78 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_78_shadow (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_79 (
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

CREATE TABLE IF NOT EXISTS moneyflow_sagas_79_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_70 (
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

CREATE TABLE IF NOT EXISTS event_outbox_70_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_71 (
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

CREATE TABLE IF NOT EXISTS event_outbox_71_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_72 (
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

CREATE TABLE IF NOT EXISTS event_outbox_72_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_73 (
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

CREATE TABLE IF NOT EXISTS event_outbox_73_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_74 (
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

CREATE TABLE IF NOT EXISTS event_outbox_74_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_75 (
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

CREATE TABLE IF NOT EXISTS event_outbox_75_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_76 (
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

CREATE TABLE IF NOT EXISTS event_outbox_76_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_77 (
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

CREATE TABLE IF NOT EXISTS event_outbox_77_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_78 (
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

CREATE TABLE IF NOT EXISTS event_outbox_78_shadow (
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

CREATE TABLE IF NOT EXISTS event_outbox_79 (
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

CREATE TABLE IF NOT EXISTS event_outbox_79_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_70 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_70_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_71 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_71_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_72 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_72_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_73 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_73_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_74 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_74_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_75 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_75_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_76 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_76_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_77 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_77_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_78 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_78_shadow (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_79 (
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

CREATE TABLE IF NOT EXISTS reversal_retry_outbox_79_shadow (
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


