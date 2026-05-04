-- id-generator 影子号段表（压测 / shadow 流量）。
-- 依赖：init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）。
--
-- 影子号段独立递增：压测 GetID 不消耗主流量号段，主 / 影各自的 max_id
-- 互不影响。
--
-- 起点对齐 payment-util/shadow 的数字 layout：
--   主流量 max_id 起点 1_000_000        （主段 [1, ShadowFlag) 任意位置都可）
--   影子    max_id 起点 1_000_000_000_000_000_001
--                       ^ MerchantIDShadowMul / EntityIDShadowMul = 1e18
--
-- 这样 shadow 流量调 GetID 拿到的 ID 高位 1 = shadow（layout 自动满足），
-- 业务侧 caller 不需要再加偏移。
CREATE TABLE IF NOT EXISTS id_segment_shadow LIKE id_segment;

INSERT IGNORE INTO id_segment_shadow VALUES ('order', 1000000000000000001, 10000);
