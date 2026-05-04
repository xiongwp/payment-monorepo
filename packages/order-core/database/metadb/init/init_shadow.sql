-- order_meta 的影子表（压测 / shadow 流量）。
-- 依赖：init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）。
--
-- leaf_alloc 也建一份独立影子号段：压测 shadow 流量从 leaf_alloc_shadow 取号，
-- 主流量号段不被压测消耗。两张表的 max_id 各自递增、互不影响。
USE `order_meta`;

CREATE TABLE IF NOT EXISTS `leaf_alloc_shadow`         LIKE `leaf_alloc`;
CREATE TABLE IF NOT EXISTS `webhook_deliveries_shadow` LIKE `webhook_deliveries`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_shadow`    LIKE `admin_audit_log`;
CREATE TABLE IF NOT EXISTS `gl_account_shadow`         LIKE `gl_account`;
CREATE TABLE IF NOT EXISTS `gl_transaction_shadow`     LIKE `gl_transaction`;
CREATE TABLE IF NOT EXISTS `gl_entry_shadow`           LIKE `gl_entry`;
