-- split-payment / split_payment_meta schema —— 由 docker entrypoint 灌入.
--
-- ⚠ 设计原则（DB-split Batch 7 之后，极简纯粹版）：
--   split-payment 的本质 = 根据 scenario 查 graph DSL → 翻译成有序 leg → 严格按
--   定义顺序调 accounting 控制资金流向。不存业务账本（accounting 全有），不跑
--   background cron / outbox worker（删了 cron_lease）。
--
-- meta 库表清单（仅 2 张 graph 配置表）：
--   moneyflow_graphs           Graph DSL（admin-web 维护，低频改动）
--   moneyflow_graph_versions   版本快照（历史 + 回滚）
--
-- 事件流水（moneyflow_event_NN）在 shard 库里，不在 meta。

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

-- ─── cron_lease 已删除 ─────────────────────────────────────────────────
-- 极简版 split-payment 不跑 cron / outbox worker（不再有 HoldUnstickWorker /
-- OutboxWorker / ReversalRetryWorker），所以多副本互斥 lease 也不需要。
-- 后续若需 cron 互斥，建议走 etcd lease 而不是 mysql 表。
