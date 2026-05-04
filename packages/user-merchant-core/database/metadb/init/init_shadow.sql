-- user_merchant_meta 影子表（压测 / shadow 流量）。
-- 依赖：init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）。
--
-- 分库分表后 meta 留下的表（leaf_alloc / RBAC 字典 / idempotency / email_codes /
-- 审计 / lookup 反查索引）每张都有 _shadow 副本；users / merchants 等 11 张
-- 业务分片表的 _shadow 在 user_merchant_db_0..9 里，不在 meta（见
-- database/userdb/init/N_init_shadow.sql）。
USE `user_merchant_meta`;

-- 号段独立（影子流量取 ID 不消耗主用户号段）
CREATE TABLE IF NOT EXISTS `leaf_alloc_shadow`            LIKE `leaf_alloc`;

-- 幂等 / 验证码
CREATE TABLE IF NOT EXISTS `idempotency_key_shadow`       LIKE `idempotency_key`;
CREATE TABLE IF NOT EXISTS `email_codes_shadow`           LIKE `email_codes`;

-- 反查二级索引（meta；指向 user_merchant_db_*.users_NN(_shadow) 等分片表）
CREATE TABLE IF NOT EXISTS `user_lookup_shadow`           LIKE `user_lookup`;
CREATE TABLE IF NOT EXISTS `merchant_lookup_shadow`       LIKE `merchant_lookup`;
CREATE TABLE IF NOT EXISTS `session_lookup_shadow`        LIKE `session_lookup`;
CREATE TABLE IF NOT EXISTS `auth_lookup_shadow`           LIKE `auth_lookup`;

-- 合规审计（append-only）
CREATE TABLE IF NOT EXISTS `merchant_kyc_audit_shadow`    LIKE `merchant_kyc_audit`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_shadow`       LIKE `admin_audit_log`;

-- RBAC 字典（虽然角色 / 权限通常静态，shadow 隔离避免压测期 admin 写覆盖主表）
CREATE TABLE IF NOT EXISTS `roles_shadow`                 LIKE `roles`;
CREATE TABLE IF NOT EXISTS `permissions_shadow`           LIKE `permissions`;
CREATE TABLE IF NOT EXISTS `role_permissions_shadow`      LIKE `role_permissions`;

-- ─── leaf_alloc_shadow seed：起点对齐 payment-util/shadow 的数字 layout ────
--
-- 与 payment-util/shadow/identity.go 的段定义保持一致：
--   ShadowUserIDMin       = 9_000_000_000        (1e9 × 9)
--   MerchantIDShadowMul   = 1_000_000_000_000_000_000  (1e18)
--   EntityIDShadowMul     = 1_000_000_000_000_000_000  (1e18)
--
-- 这样 shadow 流量从 idgen 拿到的 ID 直接落在影子段，不需要后续 caller 加偏移。
INSERT IGNORE INTO `leaf_alloc_shadow` (`biz_tag`, `max_id`, `step`, `description`) VALUES
    ('user_merchant.user',           9000000000,          100000, 'Shadow user id (>= ShadowUserIDMin = 9e9)'),
    ('user_merchant.merchant',       1000000000000000001, 100000, 'Shadow merchant id (high bit 1 = shadow per layout)'),
    ('user_merchant.kyc_document',   1000000000000000001, 100000, 'Shadow KYC document id (entity layout high bit = shadow)');
