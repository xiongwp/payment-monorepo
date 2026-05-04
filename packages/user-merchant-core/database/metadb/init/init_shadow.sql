-- user_merchant_meta 影子表（压测 / shadow 流量）。
-- 依赖：init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）。
--
-- 19 张主表 + leaf_alloc_shadow（影子号段独立，避免压测消耗主用户号段）。
-- 影子表不复制 init.sql 里的 INSERT IGNORE 种子（roles / permissions 等）；
-- 压测如需要这些 seed，由测试 fixture 单独写入 _shadow 表。
USE `user_merchant_meta`;

-- 号段独立
CREATE TABLE IF NOT EXISTS `leaf_alloc_shadow`            LIKE `leaf_alloc`;

-- 商户域
CREATE TABLE IF NOT EXISTS `merchants_shadow`             LIKE `merchants`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_shadow` LIKE `merchant_kyc_document`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_audit_shadow`    LIKE `merchant_kyc_audit`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_shadow` LIKE `merchant_channel_secret`;

-- 用户域
CREATE TABLE IF NOT EXISTS `users_shadow`                 LIKE `users`;
CREATE TABLE IF NOT EXISTS `user_profiles_shadow`         LIKE `user_profiles`;
CREATE TABLE IF NOT EXISTS `user_auths_shadow`            LIKE `user_auths`;
CREATE TABLE IF NOT EXISTS `login_logs_shadow`            LIKE `login_logs`;
CREATE TABLE IF NOT EXISTS `user_sessions_shadow`         LIKE `user_sessions`;
CREATE TABLE IF NOT EXISTS `user_accounts_shadow`         LIKE `user_accounts`;
CREATE TABLE IF NOT EXISTS `user_settings_shadow`         LIKE `user_settings`;
CREATE TABLE IF NOT EXISTS `email_codes_shadow`           LIKE `email_codes`;

-- RBAC（角色 / 权限通常静态，但 shadow 隔离避免压测期 admin 写覆盖主表）
CREATE TABLE IF NOT EXISTS `roles_shadow`                 LIKE `roles`;
CREATE TABLE IF NOT EXISTS `user_roles_shadow`            LIKE `user_roles`;
CREATE TABLE IF NOT EXISTS `permissions_shadow`           LIKE `permissions`;
CREATE TABLE IF NOT EXISTS `role_permissions_shadow`      LIKE `role_permissions`;

-- 审计 + 幂等
CREATE TABLE IF NOT EXISTS `admin_audit_log_shadow`       LIKE `admin_audit_log`;
CREATE TABLE IF NOT EXISTS `idempotency_key_shadow`       LIKE `idempotency_key`;

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
