-- user_merchant_db_6 的影子表（压测 / shadow 流量）
-- 依赖：6_init.sql 已经导入完成（CREATE TABLE LIKE 需要主表存在）。
SET NAMES utf8mb4;
USE `user_merchant_db_6`;

CREATE TABLE IF NOT EXISTS `users_60_shadow` LIKE `users_60`;
CREATE TABLE IF NOT EXISTS `user_profiles_60_shadow` LIKE `user_profiles_60`;
CREATE TABLE IF NOT EXISTS `user_auths_60_shadow` LIKE `user_auths_60`;
CREATE TABLE IF NOT EXISTS `login_logs_60_shadow` LIKE `login_logs_60`;
CREATE TABLE IF NOT EXISTS `user_sessions_60_shadow` LIKE `user_sessions_60`;
CREATE TABLE IF NOT EXISTS `user_roles_60_shadow` LIKE `user_roles_60`;
CREATE TABLE IF NOT EXISTS `user_accounts_60_shadow` LIKE `user_accounts_60`;
CREATE TABLE IF NOT EXISTS `user_settings_60_shadow` LIKE `user_settings_60`;
CREATE TABLE IF NOT EXISTS `user_card_60_shadow` LIKE `user_card_60`;
CREATE TABLE IF NOT EXISTS `merchants_60_shadow` LIKE `merchants_60`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_60_shadow` LIKE `merchant_kyc_document_60`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_60_shadow` LIKE `merchant_channel_secret_60`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_60_shadow` LIKE `admin_audit_log_60`;

CREATE TABLE IF NOT EXISTS `users_61_shadow` LIKE `users_61`;
CREATE TABLE IF NOT EXISTS `user_profiles_61_shadow` LIKE `user_profiles_61`;
CREATE TABLE IF NOT EXISTS `user_auths_61_shadow` LIKE `user_auths_61`;
CREATE TABLE IF NOT EXISTS `login_logs_61_shadow` LIKE `login_logs_61`;
CREATE TABLE IF NOT EXISTS `user_sessions_61_shadow` LIKE `user_sessions_61`;
CREATE TABLE IF NOT EXISTS `user_roles_61_shadow` LIKE `user_roles_61`;
CREATE TABLE IF NOT EXISTS `user_accounts_61_shadow` LIKE `user_accounts_61`;
CREATE TABLE IF NOT EXISTS `user_settings_61_shadow` LIKE `user_settings_61`;
CREATE TABLE IF NOT EXISTS `user_card_61_shadow` LIKE `user_card_61`;
CREATE TABLE IF NOT EXISTS `merchants_61_shadow` LIKE `merchants_61`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_61_shadow` LIKE `merchant_kyc_document_61`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_61_shadow` LIKE `merchant_channel_secret_61`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_61_shadow` LIKE `admin_audit_log_61`;

CREATE TABLE IF NOT EXISTS `users_62_shadow` LIKE `users_62`;
CREATE TABLE IF NOT EXISTS `user_profiles_62_shadow` LIKE `user_profiles_62`;
CREATE TABLE IF NOT EXISTS `user_auths_62_shadow` LIKE `user_auths_62`;
CREATE TABLE IF NOT EXISTS `login_logs_62_shadow` LIKE `login_logs_62`;
CREATE TABLE IF NOT EXISTS `user_sessions_62_shadow` LIKE `user_sessions_62`;
CREATE TABLE IF NOT EXISTS `user_roles_62_shadow` LIKE `user_roles_62`;
CREATE TABLE IF NOT EXISTS `user_accounts_62_shadow` LIKE `user_accounts_62`;
CREATE TABLE IF NOT EXISTS `user_settings_62_shadow` LIKE `user_settings_62`;
CREATE TABLE IF NOT EXISTS `user_card_62_shadow` LIKE `user_card_62`;
CREATE TABLE IF NOT EXISTS `merchants_62_shadow` LIKE `merchants_62`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_62_shadow` LIKE `merchant_kyc_document_62`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_62_shadow` LIKE `merchant_channel_secret_62`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_62_shadow` LIKE `admin_audit_log_62`;

CREATE TABLE IF NOT EXISTS `users_63_shadow` LIKE `users_63`;
CREATE TABLE IF NOT EXISTS `user_profiles_63_shadow` LIKE `user_profiles_63`;
CREATE TABLE IF NOT EXISTS `user_auths_63_shadow` LIKE `user_auths_63`;
CREATE TABLE IF NOT EXISTS `login_logs_63_shadow` LIKE `login_logs_63`;
CREATE TABLE IF NOT EXISTS `user_sessions_63_shadow` LIKE `user_sessions_63`;
CREATE TABLE IF NOT EXISTS `user_roles_63_shadow` LIKE `user_roles_63`;
CREATE TABLE IF NOT EXISTS `user_accounts_63_shadow` LIKE `user_accounts_63`;
CREATE TABLE IF NOT EXISTS `user_settings_63_shadow` LIKE `user_settings_63`;
CREATE TABLE IF NOT EXISTS `user_card_63_shadow` LIKE `user_card_63`;
CREATE TABLE IF NOT EXISTS `merchants_63_shadow` LIKE `merchants_63`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_63_shadow` LIKE `merchant_kyc_document_63`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_63_shadow` LIKE `merchant_channel_secret_63`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_63_shadow` LIKE `admin_audit_log_63`;

CREATE TABLE IF NOT EXISTS `users_64_shadow` LIKE `users_64`;
CREATE TABLE IF NOT EXISTS `user_profiles_64_shadow` LIKE `user_profiles_64`;
CREATE TABLE IF NOT EXISTS `user_auths_64_shadow` LIKE `user_auths_64`;
CREATE TABLE IF NOT EXISTS `login_logs_64_shadow` LIKE `login_logs_64`;
CREATE TABLE IF NOT EXISTS `user_sessions_64_shadow` LIKE `user_sessions_64`;
CREATE TABLE IF NOT EXISTS `user_roles_64_shadow` LIKE `user_roles_64`;
CREATE TABLE IF NOT EXISTS `user_accounts_64_shadow` LIKE `user_accounts_64`;
CREATE TABLE IF NOT EXISTS `user_settings_64_shadow` LIKE `user_settings_64`;
CREATE TABLE IF NOT EXISTS `user_card_64_shadow` LIKE `user_card_64`;
CREATE TABLE IF NOT EXISTS `merchants_64_shadow` LIKE `merchants_64`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_64_shadow` LIKE `merchant_kyc_document_64`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_64_shadow` LIKE `merchant_channel_secret_64`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_64_shadow` LIKE `admin_audit_log_64`;

