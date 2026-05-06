-- reconcile_event 表：记录跨服务原子性补偿 worker 检测到的
-- ledger 与 order-core 之间的不一致及修复动作，用于审计和幂等去重。
--
-- 设计：
--   - event_id：reconcile_event_id（幂等键），确保同一不一致只修复一次
--   - created_at：事件创建时间（便于后续分析历史数据）
--   - status：修复状态 (detected / repaired / failed)
--   - voucher_no, charge_id：关键业务字段，便于关联查询

SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;

USE `account_meta`;

CREATE TABLE IF NOT EXISTS `reconcile_event` (
    `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '内部 ID',
    `event_id`           VARCHAR(256) NOT NULL COMMENT '幂等键（唯一），格式：reconcile_{timestamp}_{voucher}_{chargeID}',
    `voucher_no`         VARCHAR(128) NOT NULL COMMENT '结算单号',
    `charge_id`          VARCHAR(128) DEFAULT NULL COMMENT 'order-core Charge ID',
    `payment_intent_id`  VARCHAR(128) DEFAULT NULL COMMENT 'order-core PaymentIntent ID',
    `order_core_status`  VARCHAR(64) DEFAULT NULL COMMENT 'order-core 权威状态（如 succeeded/failed/pending）',
    `accounting_status`  VARCHAR(64) DEFAULT NULL COMMENT 'accounting-system outbox 状态（pending/redis_done/mysql_done/failed）',
    `mismatch_type`      VARCHAR(64) NOT NULL COMMENT '不一致类型：status_mismatch / missing_reconcile / stale_pending',
    `repair_action`      VARCHAR(64) DEFAULT NULL COMMENT '修复动作：mark_done / rollback / none',
    `repair_result`      VARCHAR(256) DEFAULT NULL COMMENT '修复结果（JSON）：{"success":true,"affected_rows":1}',
    `error_msg`          TEXT DEFAULT NULL COMMENT '错误信息（如修复失败）',
    `status`             TINYINT NOT NULL DEFAULT 0 COMMENT '0=detected 1=repaired 2=failed_to_repair',
    `created_at`         DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '事件时间',
    `updated_at`         DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '最后更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_event_id` (`event_id`),
    KEY `idx_voucher_charge` (`voucher_no`, `charge_id`),
    KEY `idx_status_created` (`status`, `created_at`),
    KEY `idx_mismatch_type` (`mismatch_type`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='跨服务原子性补偿 worker 审计日志表';
