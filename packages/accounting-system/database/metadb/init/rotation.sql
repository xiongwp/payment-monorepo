-- ============================================================================
-- Rotating Suspense / Receivable / Payable Accounts — metadb DDL
--
-- 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md (§3.1, §3.2, §3.5)
-- 关联代码：packages/accounting-system/internal/domain/model/rotation.go
--
-- 本文件包含三类变更：
--   1. logical_account            — 跨周期稳定的逻辑账户（全局表）
--   2. logical_account_rotation_policy — 周期与超时策略（全局表）
--   3. account_business_type_info 新增 4 个预置 business_type (10/11/12/13)
--
-- 分片表 (tx_account_anchor) 的 DDL 与 account 表 ALTER 在
-- database/accountingdb/init/rotation.sql。
--
-- 回滚脚本：database/rollback/rotation_rollback.sql
-- ============================================================================

SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;

USE `account_meta`;

-- ============================================
-- 1. logical_account
-- 全局表。业务侧稳定的"账户"，跨周期不变。
-- 多个 Account instance 在不同周期承接其流量。
--
-- 关键不变量：
--   I1：同一 logical_account_id 下任意时刻至多一个 instance phase=active
--       由 scheduler 切换事务保证，并由 invariant_audit_job 巡检兜底。
--
-- 反范式化字段：
--   current_active_account_no / current_active_period_end —
--   路由层热路径只查本表即可，避免跨片 account 表 scan。
--   写入唯一入口：rotation scheduler 的切换事务（§5.2.1）。
-- ============================================
CREATE TABLE IF NOT EXISTS `logical_account` (
    `id`                          BIGINT UNSIGNED NOT NULL                            COMMENT '主键（Leaf 号段生成）',
    `logical_account_key`         VARCHAR(64)     NOT NULL                            COMMENT '业务稳定 key，如 "transit:channel-payable:alipay:CNY"，命名前缀白名单见 rotation.go AllowedKeyPrefixes',
    `account_type`                TINYINT         NOT NULL                            COMMENT '复用 AccountType (期望值 5/6/9)',
    `account_business_type`       SMALLINT        NOT NULL                            COMMENT '复用 AccountBusinessType (1-999)',
    `currency`                    CHAR(3)         NOT NULL                            COMMENT 'ISO 4217 三字母币种码',
    `description`                 VARCHAR(255)    DEFAULT NULL                        COMMENT '业务描述',
    `rotation_enabled`            TINYINT         NOT NULL DEFAULT 0                  COMMENT '0=不轮换(兼容旧账户) 1=轮换',
    `current_active_account_no`   VARCHAR(64)     DEFAULT NULL                        COMMENT '反范式化：当期 active 的 account_no；轮换 tick 原子更新',
    `current_active_period_end`   DATETIME        DEFAULT NULL                        COMMENT '当期 active 的 period_end',
    `status`                      TINYINT         NOT NULL DEFAULT 1                  COMMENT '0=disabled 1=enabled',
    `registered_by`               VARCHAR(64)     NOT NULL                            COMMENT '注册者，必填，禁止 lazy create（审计用）',
    `created_at`                  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP  COMMENT '创建时间',
    `updated_at`                  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    `version`                     BIGINT UNSIGNED NOT NULL DEFAULT 0                  COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lak` (`logical_account_key`),
    KEY         `idx_type_biz_currency` (`account_type`, `account_business_type`, `currency`),
    KEY         `idx_rotation_enabled` (`rotation_enabled`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='逻辑账户（跨周期稳定）';


-- ============================================
-- 2. logical_account_rotation_policy
-- 全局表。单个 logical_account 的轮换策略。
--
-- 关键字段：
--   drain_p99_seconds       — draining 保留下限（业务 P99 生命周期）
--   drain_hard_timeout_secs — draining 保留上限，超过强制迁移（§8）
--   archive_grace_secs      — frozen → archived 缓冲
--   provision_lead_secs     — scheduler 提前多久预创建下一期 (default 24h)
--   config_version          — 配置版本号，路由层用它判断缓存是否过期 (E-30)
--
-- 策略变更影响：
--   - 旧 instance 用出生时锁定的 policy_version_at_birth 走完生命周期
--   - 新 instance 用最新配置
--   - 紧急时通过 account.effective_hard_timeout_secs 单实例 override
-- ============================================
CREATE TABLE IF NOT EXISTS `logical_account_rotation_policy` (
    `logical_account_id`        BIGINT UNSIGNED NOT NULL                            COMMENT 'logical_account.id',
    `period_unit`               VARCHAR(8)      NOT NULL                            COMMENT 'DAY (测试) / MONTH (应付应收) / QUARTER (通用中间)',
    `period_count`              INT             NOT NULL DEFAULT 1                  COMMENT '周期倍数：1=每月，3=每三个月',
    `rotation_anchor_tz`        VARCHAR(32)     NOT NULL                            COMMENT 'IANA 时区，决定周期边界 (如 Asia/Shanghai)',
    `drain_p99_seconds`         INT             NOT NULL                            COMMENT '业务 P99 生命周期（秒），draining 最短保留',
    `drain_hard_timeout_secs`   INT             NOT NULL                            COMMENT 'draining 最长保留（秒），超过强制迁移',
    `archive_grace_secs`        INT             NOT NULL DEFAULT 604800             COMMENT 'frozen → archived 缓冲（秒），默认 7 天',
    `provision_lead_secs`       INT             NOT NULL DEFAULT 86400              COMMENT '提前预创建下一期的时间（秒），默认 24h',
    `config_version`            BIGINT          NOT NULL DEFAULT 1                  COMMENT '配置版本号；每次变更 +1',
    `effective_from`            DATETIME        NOT NULL                            COMMENT '策略生效时刻（UTC）',
    `updated_at`                DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`logical_account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='轮换策略';


-- ============================================
-- 3. 新增预置 AccountBusinessType (10, 11, 12, 13)
--
-- 10 MigrationSuspense          — 跨期强制迁移过渡科目，余额恒为 0
-- 11 ResidualWriteOff           — 归档时核销小额尾差到损益
-- 12 RotationOpsAdjust          — 人工运维调整（独立科目，便于审计）
-- 13 RotationCarryforward       — 跨期结转科目，余额恒为 0（仅显式启用时使用）
--
-- 必须使用 INSERT IGNORE：本脚本可重复执行（idempotent）。
-- ============================================
INSERT IGNORE INTO `account_business_type_info`
    (`business_type`, `business_type_code`, `account_type`, `description`, `enabled`)
VALUES
    (10, 'ROTATION_MIGRATION_SUSPENSE',  9, '跨期强制迁移过渡科目，余额恒为 0', 1),
    (11, 'ROTATION_RESIDUAL_WRITEOFF',   4, '轮换归档残值核销账户', 1),
    (12, 'ROTATION_OPS_ADJUST',          4, '轮换人工运维调整账户', 1),
    (13, 'ROTATION_CARRYFORWARD',        2, '轮换跨期结转科目，余额恒为 0', 1);

-- ============================================
-- 完成 metadb 部分
-- 后续：执行 database/accountingdb/init/rotation.sql 完成分片表 DDL
-- ============================================
