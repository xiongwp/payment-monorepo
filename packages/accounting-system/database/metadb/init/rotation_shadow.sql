-- ============================================================================
-- Rotating Suspense / Receivable / Payable Accounts — metadb shadow DDL
--
-- 影子表（shadow / 压测流量）侧的 metadb 变更。
-- 依赖：database/metadb/init/rotation.sql 必须已经导入。
--
-- 本脚本：
--   1. CREATE shadow 副本 LIKE 主表：logical_account_shadow, logical_account_rotation_policy_shadow
--   2. INSERT IGNORE 新增的 4 个预置 business_type 到 account_business_type_info_shadow
--
-- 与既有 init_shadow.sql 风格一致：LIKE + 字典 seed 同步。
-- ============================================================================

SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;

USE `account_meta`;

-- 全局表的 shadow 副本（schema 通过 LIKE 跟主表完全一致）
CREATE TABLE IF NOT EXISTS `logical_account_shadow`                  LIKE `logical_account`;
CREATE TABLE IF NOT EXISTS `logical_account_rotation_policy_shadow`  LIKE `logical_account_rotation_policy`;

-- shadow 流量也要能查 4 个新预置 business_type，按 init_shadow.sql 的 seed 模式
INSERT IGNORE INTO `account_business_type_info_shadow`
    SELECT * FROM `account_business_type_info`
     WHERE `business_type` IN (10, 11, 12, 13);

-- 注意：logical_account / logical_account_rotation_policy 是业务态表（非字典），
-- shadow 流量会写入自己的实例，不从主表 SELECT seed。
