-- ============================================================================
-- split_payment_db_6 —— 高频事件流水分片库（DB-split Batch 7 极简版）
-- ⚠ 模板文件。由 ../gen.sh 用 sed 替换 6 + 注入 tables block 生成 N_init.sql。
--
-- 唯一表族 moneyflow_event_NN —— 按 idempotency_key / charge_id / saga_id hash 路由：
--   hash(key) % 100 → globalIdx (0..99)
--   dbIdx   = globalIdx / 10  (0..9)
--   tblIdx  = globalIdx       (00..99)
--
-- 设计原则：
--   split-payment **不再存业务账本**（transfers / fees / payouts / reversals 等都
--   在 accounting-system 已有）。它只关注"事件 / 状态机执行流水"。
--
-- 同一张表覆盖 4 种 event_type：
--   trigger          一次 TriggerEvent 触发的 RunPlan 执行记录（原 moneyflow_runs）
--   saga             SaveGraph / refund / payout 等 saga 的状态机持久化（原 moneyflow_sagas）
--   outbox           可靠事件发布的待发队列（原 event_outbox）
--   reversal_retry   退款失败的重试队列（原 reversal_retry_outbox）
--
-- 字段按"通用执行状态" + "按 type 含义的可选字段" 设计：
--   通用：id / event_type / event_id / status / retry_count / max_retry /
--          payload_json / error_msg / next_retry_at / hold_until / 时间戳
--   特定：graph_id / graph_version / charge_id / merchant_id / amount_minor /
--          currency / plan_json / voucher_no / trace_id / correlation_id /
--          current_step / steps_json
--   (event_type 不需要的字段填 NULL/0; 不浪费多少空间)
-- ============================================================================

CREATE DATABASE IF NOT EXISTS split_payment_db_6 CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

-- split_user 在 meta init.sql 里也建一遍；但 shared-db 把每个 shard SQL 灌到不同
-- mysql 实例（shared-shard-N），mysql user 是 per-instance 的不跨实例共享，所以
-- 每个 shard 自己也必须 CREATE USER。IF NOT EXISTS 幂等。
CREATE USER IF NOT EXISTS 'split_user'@'%' IDENTIFIED BY 'password';
ALTER USER 'split_user'@'%' IDENTIFIED BY 'password';

GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, DROP, REFERENCES
  ON split_payment_db_6.* TO 'split_user'@'%';
FLUSH PRIVILEGES;

USE split_payment_db_6;
-- ─── moneyflow_event (10 张主表 + 10 张 _shadow 镜像) ──────────────────────

CREATE TABLE IF NOT EXISTS moneyflow_event_60 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_60_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_61 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_61_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_62 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_62_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_63 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_63_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_64 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_64_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_65 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_65_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_66 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_66_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_67 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_67_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_68 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_68_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_69 (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS moneyflow_event_69_shadow (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;


