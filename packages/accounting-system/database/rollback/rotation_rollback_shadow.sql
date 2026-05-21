-- ============================================================================
-- Rotating Suspense / Receivable / Payable Accounts — metadb shadow ROLLBACK
--
-- 影子表回滚：先于主表 rollback 执行，避免 shadow 引用主表已删除的列。
--
-- 完整顺序（生产回滚）：
--   1. 跑分片 shadow 回滚： database/rollback/rotation_rollback_shadow_sharded.sql
--   2. 跑本文件（metadb shadow 回滚）
--   3. 跑分片主表回滚：     database/rollback/rotation_rollback_sharded.sql
--   4. 跑 metadb 主表回滚： database/rollback/rotation_rollback.sql
-- ============================================================================

SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;

USE `account_meta`;

-- 1. 删除 shadow 字典中新增的 4 条预置 business_type
DELETE FROM `account_business_type_info_shadow`
 WHERE `business_type` IN (10, 11, 12, 13)
   AND `business_type_code` IN (
       'ROTATION_MIGRATION_SUSPENSE',
       'ROTATION_RESIDUAL_WRITEOFF',
       'ROTATION_OPS_ADJUST',
       'ROTATION_CARRYFORWARD'
   );

-- 2. DROP shadow 全局表
DROP TABLE IF EXISTS `logical_account_rotation_policy_shadow`;
DROP TABLE IF EXISTS `logical_account_shadow`;
