-- ============================================================================
-- Rotating Suspense / Receivable / Payable Accounts — ROLLBACK (metadb side)
--
-- WARNING: This script DESTROYS the rotation feature data permanently.
-- ONLY run when:
--   1. Phase 0 deploy 失败需要彻底回退；AND
--   2. 已确认 logical_account / tx_account_anchor 表中无任何业务数据；AND
--   3. account 表上无 lifecycle_phase != 0 (legacy) 的行
--
-- 验收 SQL（执行前必须 ALL 返回 0）：
--   USE account_meta;
--   SELECT COUNT(*) FROM logical_account WHERE rotation_enabled = 1;
--   USE accounting_db_0; SELECT COUNT(*) FROM tx_account_anchor_00;  -- 重复每一片
--   USE accounting_db_0; SELECT COUNT(*) FROM account_00 WHERE lifecycle_phase != 0;
--
-- 执行顺序（强制）：
--   1. 先跑分片回滚：rotation_rollback_sharded.sql  (drops tx_account_anchor_NN x100
--      + drops account_NN rotation columns x100)
--   2. 再跑本脚本：清理 metadb 全局表 + 预置 business_type
--
-- 反方向部署：先跑 metadb forward (database/metadb/init/rotation.sql)
--             再跑分片 forward (database/accountingdb/init/rotation.sql)
--
-- 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §13 Review Checklist
-- ============================================================================

SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;

USE `account_meta`;

-- 1. 删除新增的预置 business_type (10/11/12/13)
DELETE FROM `account_business_type_info`
 WHERE `business_type` IN (10, 11, 12, 13)
   AND `business_type_code` IN (
       'ROTATION_MIGRATION_SUSPENSE',
       'ROTATION_RESIDUAL_WRITEOFF',
       'ROTATION_OPS_ADJUST',
       'ROTATION_CARRYFORWARD'
   );

-- 2. DROP 全局表（顺序：policy 先于 logical_account 以避免外部引用担忧）
DROP TABLE IF EXISTS `logical_account_rotation_policy`;
DROP TABLE IF EXISTS `logical_account`;

-- ============================================================================
-- END metadb rollback
-- 完整回滚还需运行分片端：
--   mysql < packages/accounting-system/database/rollback/rotation_rollback_sharded.sql
-- ============================================================================
