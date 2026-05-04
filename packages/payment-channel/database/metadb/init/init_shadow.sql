-- paychan_meta 的影子表（压测 / shadow 流量）。
-- 依赖：init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）。
--
-- leaf_alloc 也建一份独立影子号段：压测 shadow 流量从 leaf_alloc_shadow 取号，
-- 主流量号段不被压测消耗。两张表的 max_id 各自递增、互不影响。
USE `paychan_meta`;

CREATE TABLE IF NOT EXISTS `leaf_alloc_shadow` LIKE `leaf_alloc`;
