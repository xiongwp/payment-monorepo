-- config_center_meta：配置中心后端存储。单 meta 库（写量小，KV 业务）。
--
-- 4 张表：
--   config_namespace  命名空间（"card-payment" / "order-core" 等）
--   config_item       当前生效的 key + version 指针（hot path SELECT）
--   config_version    历史版本（不可变 append-only，rollback 取这表）
--   config_audit_log  谁改了什么（PCI Req 10 / 内部审计）
CREATE DATABASE IF NOT EXISTS `config_center_meta` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `config_center_meta`;

-- namespace：粗粒度分组。一个 namespace 通常 = 一个服务名。
CREATE TABLE IF NOT EXISTS `config_namespace` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `name`         VARCHAR(64)  NOT NULL,                    -- "card-payment"
    `description`  VARCHAR(256) NOT NULL DEFAULT '',
    `owner`        VARCHAR(64)  NOT NULL DEFAULT '',          -- team / oncall
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Config namespace 元数据';

-- config_item：(namespace, key) 当前生效 version 的指针。
-- hot path：客户端 GetConfig / WatchConfig 只读这张表 + JOIN config_version。
-- 历史 version 全在 config_version 里；rollback = 把 active_version 指回旧的。
CREATE TABLE IF NOT EXISTS `config_item` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `namespace`       VARCHAR(64)  NOT NULL,
    `key_name`        VARCHAR(128) NOT NULL,                   -- "rate_limit.rps" 等
    `active_version`  BIGINT       NOT NULL DEFAULT 0,         -- 当前生效的 version_id（FK to config_version.id；0=未发布）
    `latest_version`  BIGINT       NOT NULL DEFAULT 0,         -- 历史最大 version_id（=active 当 admin 写后未 rollback；rollback 后 latest > active）
    `deleted`         TINYINT      NOT NULL DEFAULT 0,
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_ns_key` (`namespace`, `key_name`),
    KEY `idx_namespace`  (`namespace`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Config 当前指针（namespace,key→active_version_id）';

-- config_version：每条 admin 写入产一行；不可变 append-only。
-- rollback 不删旧 version，只产新 version 复制旧值。
CREATE TABLE IF NOT EXISTS `config_version` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `namespace`       VARCHAR(64)  NOT NULL,
    `key_name`        VARCHAR(128) NOT NULL,
    `version`         BIGINT       NOT NULL,                   -- per (namespace, key) 单调递增；从 1 开始
    `value`           MEDIUMTEXT   NOT NULL,                   -- 配置值（JSON / YAML / plain）
    `format`          VARCHAR(16)  NOT NULL DEFAULT 'json',
    `effective_at`    DATETIME(3)  DEFAULT NULL,               -- NULL = 立即生效
    `expire_at`       DATETIME(3)  DEFAULT NULL,               -- NULL = 永不过期
    `strategy`        VARCHAR(16)  NOT NULL DEFAULT 'FULL',    -- FULL / CANARY / TARGETED / SCHEDULED
    `strategy_spec`   JSON         DEFAULT NULL,               -- CanarySpec / TargetedSpec 序列化
    `created_by`      VARCHAR(64)  NOT NULL,                   -- admin user id
    `change_reason`   VARCHAR(512) NOT NULL DEFAULT '',
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_ns_key_version` (`namespace`, `key_name`, `version`),
    KEY `idx_ns_key_created` (`namespace`, `key_name`, `created_at`),
    KEY `idx_effective`  (`effective_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Config 不可变版本历史';

-- config_subscription：配置项 ↔ 订阅服务多对多关系。
--
-- 用法：
--   1. admin 新建一个 config_item（如 namespace="shared", key="kafka.brokers"）
--   2. 在 admin UI 勾选哪些服务订阅它（card-payment / order-core / risk-manage 等）
--   3. server 在 WatchNamespace 时，除了客户端订阅的 namespace 自身，还把
--      "subscriber=客户端 namespace" 的所有 config_item 一起 stream 给它
--
-- 这样一个"全局"配置（如 kafka_brokers / kms_endpoint / ratelimit_default）
-- 可以由多个服务共享，admin 改一次所有订阅服务都收到。
CREATE TABLE IF NOT EXISTS `config_subscription` (
    `id`               BIGINT       NOT NULL AUTO_INCREMENT,
    `item_id`          BIGINT       NOT NULL,                      -- FK config_item.id
    `subscriber`       VARCHAR(64)  NOT NULL,                      -- 订阅方服务名 (= 该服务的 namespace)
    `created_at`       DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_item_sub` (`item_id`, `subscriber`),
    KEY `idx_subscriber`  (`subscriber`),
    KEY `idx_item`        (`item_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Config 项 ↔ 订阅服务 (多对多)';

-- config_audit_log：每次 admin 操作记一条，含**修改前/修改后完整快照**。
--
-- 设计原则：
--   - 每个 admin 动作（PUT / ROLLBACK / DELETE / CREATE / SUBSCRIBE_CHANGE）
--     都写一条；append-only，admin UI 不允许删
--   - 完整保存 value_before / value_after 完整文本（即使可以从 config_version
--     表 join 出来也冗余存一份，避免老 version GC 后审计失效）
--   - strategy_before / strategy_after：策略也变了要看出来
--   - subscribers_before / subscribers_after：订阅关系变更也算操作
--   - actor_ip + user_agent：取证需要
--   - 7 年留存（PCI / 内审），30d 后归档到数据湖
CREATE TABLE IF NOT EXISTS `config_audit_log` (
    `id`                  BIGINT       NOT NULL AUTO_INCREMENT,
    `namespace`           VARCHAR(64)  NOT NULL,
    `key_name`            VARCHAR(128) NOT NULL,
    -- 操作类型
    `op`                  VARCHAR(20)  NOT NULL COMMENT 'CREATE / PUT / ROLLBACK / DELETE / SUBSCRIBE_ADD / SUBSCRIBE_REMOVE',
    -- 版本号前后对照（PUT/ROLLBACK 会变；CREATE 时 before=NULL after=1）
    `version_before`      BIGINT       DEFAULT NULL,
    `version_after`       BIGINT       DEFAULT NULL,
    -- 完整 value 快照（冗余存以防 version 被归档）
    `value_before`        MEDIUMTEXT   DEFAULT NULL,
    `value_after`         MEDIUMTEXT   DEFAULT NULL,
    `format_before`       VARCHAR(16)  DEFAULT NULL,
    `format_after`        VARCHAR(16)  DEFAULT NULL,
    -- 策略 & 时间窗变化
    `strategy_before`     VARCHAR(16)  DEFAULT NULL,
    `strategy_after`      VARCHAR(16)  DEFAULT NULL,
    `strategy_spec_before` JSON        DEFAULT NULL,
    `strategy_spec_after` JSON         DEFAULT NULL,
    `effective_at_before` DATETIME(3)  DEFAULT NULL,
    `effective_at_after`  DATETIME(3)  DEFAULT NULL,
    `expire_at_before`    DATETIME(3)  DEFAULT NULL,
    `expire_at_after`     DATETIME(3)  DEFAULT NULL,
    -- 订阅关系变更（SUBSCRIBE_ADD/REMOVE 时填）
    `subscribers_before`  JSON         DEFAULT NULL COMMENT 'string[] of subscriber namespace',
    `subscribers_after`   JSON         DEFAULT NULL,
    -- 操作者上下文
    `actor`               VARCHAR(64)  NOT NULL,
    `actor_ip`            VARCHAR(64)  DEFAULT NULL,
    `user_agent`          VARCHAR(256) DEFAULT NULL,
    `change_reason`       VARCHAR(512) NOT NULL DEFAULT '' COMMENT 'admin 必填的变更原因',
    `trace_id`            VARCHAR(64)  DEFAULT NULL,
    `created_at`          DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_ns_key_created` (`namespace`, `key_name`, `created_at`),
    KEY `idx_actor_created`  (`actor`, `created_at`),
    KEY `idx_op_created`     (`op`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Config 操作审计 — 含完整 before/after 快照';
