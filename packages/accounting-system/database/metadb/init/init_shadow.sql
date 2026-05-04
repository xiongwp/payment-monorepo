-- account_meta 影子表（压测 / shadow 流量）。
-- 依赖：init.sql 必须已经导入（CREATE TABLE LIKE 需要主表存在）。
--
-- 影子表 = 主表的 LIKE 副本 + 字典 seed 同步 + 号段起点对齐 shadow 段。
USE `account_meta`;

-- 号段独立
CREATE TABLE IF NOT EXISTS `leaf_alloc_shadow`              LIKE `leaf_alloc`;

-- 字典 / 配置类表
CREATE TABLE IF NOT EXISTS `account_type_info_shadow`        LIKE `account_type_info`;
CREATE TABLE IF NOT EXISTS `account_business_type_info_shadow` LIKE `account_business_type_info`;
CREATE TABLE IF NOT EXISTS `transaction_rule_shadow`         LIKE `transaction_rule`;
CREATE TABLE IF NOT EXISTS `hot_account_config_shadow`       LIKE `hot_account_config`;
CREATE TABLE IF NOT EXISTS `buffer_account_config_shadow`    LIKE `buffer_account_config`;
CREATE TABLE IF NOT EXISTS `service_instance_shadow`         LIKE `service_instance`;
CREATE TABLE IF NOT EXISTS `system_config_shadow`            LIKE `system_config`;

-- ─── 字典 seed 复制：shadow 流量也要能查这些固定枚举 ──────────────────────
-- 业务字典（业务类型 / 账户类型 / 交易规则 / 系统配置）在主和影流量下语义一致，
-- 直接拷贝主表的 seed 即可；shadow 流量绝不修改这些字典。
INSERT IGNORE INTO `account_business_type_info_shadow` SELECT * FROM `account_business_type_info`;
INSERT IGNORE INTO `account_type_info_shadow`           SELECT * FROM `account_type_info`;
INSERT IGNORE INTO `transaction_rule_shadow`            SELECT * FROM `transaction_rule`;
INSERT IGNORE INTO `system_config_shadow`               SELECT * FROM `system_config`;
INSERT IGNORE INTO `hot_account_config_shadow`          SELECT * FROM `hot_account_config`;
INSERT IGNORE INTO `buffer_account_config_shadow`       SELECT * FROM `buffer_account_config`;

-- ─── leaf_alloc_shadow seed：起点对齐 payment-util/shadow 数字 layout ────
--
-- 与 payment-util/shadow/identity.go 段定义对齐：
--   ShadowUserIDMin       = 9_000_000_000              (1e9 × 9)，shadow 真实用户段
--   EntityIDShadowMul     = 1_000_000_000_000_000_000  (1e18)，shadow entity ID 段
--
-- shadow 号段从这个起点开始递增，业务侧 caller 拿到的 ID 直接带 shadow 高位标识。
-- 注：accounting-system 内部生成的 user_id（fleet 平台账户）走 [9e9, 9e9+99]
-- 段，业务用户 user_id 走 [9e9+100, 9.9e9]；fleet seed 见各 shard 的
-- *_init_tmp_shadow.sql。
INSERT IGNORE INTO `leaf_alloc_shadow` (`biz_tag`, `max_id`, `step`, `description`) VALUES
    ('accounting.user',           9000000100,          100000, 'Shadow user id (≥ 9e9 + 100，预留 fleet)'),
    ('accounting.voucher',        1000000000000000001, 100000, 'Shadow voucher id (entity layout high bit = shadow)'),
    ('accounting.tcc',            1000000000000000001, 100000, 'Shadow tcc id (entity layout high bit = shadow)'),
    ('accounting.batch_order',    1000000000000000001, 100000, 'Shadow batch order id'),
    ('accounting.account',        90000000000,         100000, 'Shadow account_id (high bit 1 in 19-digit account layout)');
