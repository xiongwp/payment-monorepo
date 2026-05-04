-- user_merchant_db_0 的影子表（压测 / shadow 流量）
-- 依赖：0_init.sql 已经导入完成（CREATE TABLE LIKE 需要主表存在）。
SET NAMES utf8mb4;
USE `user_merchant_db_0`;

CREATE TABLE IF NOT EXISTS `users_00_shadow` LIKE `users_00`;
CREATE TABLE IF NOT EXISTS `user_profiles_00_shadow` LIKE `user_profiles_00`;
CREATE TABLE IF NOT EXISTS `user_auths_00_shadow` LIKE `user_auths_00`;
CREATE TABLE IF NOT EXISTS `login_logs_00_shadow` LIKE `login_logs_00`;
CREATE TABLE IF NOT EXISTS `user_sessions_00_shadow` LIKE `user_sessions_00`;
CREATE TABLE IF NOT EXISTS `user_roles_00_shadow` LIKE `user_roles_00`;
CREATE TABLE IF NOT EXISTS `user_accounts_00_shadow` LIKE `user_accounts_00`;
CREATE TABLE IF NOT EXISTS `user_settings_00_shadow` LIKE `user_settings_00`;
CREATE TABLE IF NOT EXISTS `merchants_00_shadow` LIKE `merchants_00`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_00_shadow` LIKE `merchant_kyc_document_00`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_00_shadow` LIKE `merchant_channel_secret_00`;

CREATE TABLE IF NOT EXISTS `users_01_shadow` LIKE `users_01`;
CREATE TABLE IF NOT EXISTS `user_profiles_01_shadow` LIKE `user_profiles_01`;
CREATE TABLE IF NOT EXISTS `user_auths_01_shadow` LIKE `user_auths_01`;
CREATE TABLE IF NOT EXISTS `login_logs_01_shadow` LIKE `login_logs_01`;
CREATE TABLE IF NOT EXISTS `user_sessions_01_shadow` LIKE `user_sessions_01`;
CREATE TABLE IF NOT EXISTS `user_roles_01_shadow` LIKE `user_roles_01`;
CREATE TABLE IF NOT EXISTS `user_accounts_01_shadow` LIKE `user_accounts_01`;
CREATE TABLE IF NOT EXISTS `user_settings_01_shadow` LIKE `user_settings_01`;
CREATE TABLE IF NOT EXISTS `merchants_01_shadow` LIKE `merchants_01`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_01_shadow` LIKE `merchant_kyc_document_01`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_01_shadow` LIKE `merchant_channel_secret_01`;

CREATE TABLE IF NOT EXISTS `users_02_shadow` LIKE `users_02`;
CREATE TABLE IF NOT EXISTS `user_profiles_02_shadow` LIKE `user_profiles_02`;
CREATE TABLE IF NOT EXISTS `user_auths_02_shadow` LIKE `user_auths_02`;
CREATE TABLE IF NOT EXISTS `login_logs_02_shadow` LIKE `login_logs_02`;
CREATE TABLE IF NOT EXISTS `user_sessions_02_shadow` LIKE `user_sessions_02`;
CREATE TABLE IF NOT EXISTS `user_roles_02_shadow` LIKE `user_roles_02`;
CREATE TABLE IF NOT EXISTS `user_accounts_02_shadow` LIKE `user_accounts_02`;
CREATE TABLE IF NOT EXISTS `user_settings_02_shadow` LIKE `user_settings_02`;
CREATE TABLE IF NOT EXISTS `merchants_02_shadow` LIKE `merchants_02`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_02_shadow` LIKE `merchant_kyc_document_02`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_02_shadow` LIKE `merchant_channel_secret_02`;

CREATE TABLE IF NOT EXISTS `users_03_shadow` LIKE `users_03`;
CREATE TABLE IF NOT EXISTS `user_profiles_03_shadow` LIKE `user_profiles_03`;
CREATE TABLE IF NOT EXISTS `user_auths_03_shadow` LIKE `user_auths_03`;
CREATE TABLE IF NOT EXISTS `login_logs_03_shadow` LIKE `login_logs_03`;
CREATE TABLE IF NOT EXISTS `user_sessions_03_shadow` LIKE `user_sessions_03`;
CREATE TABLE IF NOT EXISTS `user_roles_03_shadow` LIKE `user_roles_03`;
CREATE TABLE IF NOT EXISTS `user_accounts_03_shadow` LIKE `user_accounts_03`;
CREATE TABLE IF NOT EXISTS `user_settings_03_shadow` LIKE `user_settings_03`;
CREATE TABLE IF NOT EXISTS `merchants_03_shadow` LIKE `merchants_03`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_03_shadow` LIKE `merchant_kyc_document_03`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_03_shadow` LIKE `merchant_channel_secret_03`;

CREATE TABLE IF NOT EXISTS `users_04_shadow` LIKE `users_04`;
CREATE TABLE IF NOT EXISTS `user_profiles_04_shadow` LIKE `user_profiles_04`;
CREATE TABLE IF NOT EXISTS `user_auths_04_shadow` LIKE `user_auths_04`;
CREATE TABLE IF NOT EXISTS `login_logs_04_shadow` LIKE `login_logs_04`;
CREATE TABLE IF NOT EXISTS `user_sessions_04_shadow` LIKE `user_sessions_04`;
CREATE TABLE IF NOT EXISTS `user_roles_04_shadow` LIKE `user_roles_04`;
CREATE TABLE IF NOT EXISTS `user_accounts_04_shadow` LIKE `user_accounts_04`;
CREATE TABLE IF NOT EXISTS `user_settings_04_shadow` LIKE `user_settings_04`;
CREATE TABLE IF NOT EXISTS `merchants_04_shadow` LIKE `merchants_04`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_04_shadow` LIKE `merchant_kyc_document_04`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_04_shadow` LIKE `merchant_channel_secret_04`;

CREATE TABLE IF NOT EXISTS `users_05_shadow` LIKE `users_05`;
CREATE TABLE IF NOT EXISTS `user_profiles_05_shadow` LIKE `user_profiles_05`;
CREATE TABLE IF NOT EXISTS `user_auths_05_shadow` LIKE `user_auths_05`;
CREATE TABLE IF NOT EXISTS `login_logs_05_shadow` LIKE `login_logs_05`;
CREATE TABLE IF NOT EXISTS `user_sessions_05_shadow` LIKE `user_sessions_05`;
CREATE TABLE IF NOT EXISTS `user_roles_05_shadow` LIKE `user_roles_05`;
CREATE TABLE IF NOT EXISTS `user_accounts_05_shadow` LIKE `user_accounts_05`;
CREATE TABLE IF NOT EXISTS `user_settings_05_shadow` LIKE `user_settings_05`;
CREATE TABLE IF NOT EXISTS `merchants_05_shadow` LIKE `merchants_05`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_05_shadow` LIKE `merchant_kyc_document_05`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_05_shadow` LIKE `merchant_channel_secret_05`;

CREATE TABLE IF NOT EXISTS `users_06_shadow` LIKE `users_06`;
CREATE TABLE IF NOT EXISTS `user_profiles_06_shadow` LIKE `user_profiles_06`;
CREATE TABLE IF NOT EXISTS `user_auths_06_shadow` LIKE `user_auths_06`;
CREATE TABLE IF NOT EXISTS `login_logs_06_shadow` LIKE `login_logs_06`;
CREATE TABLE IF NOT EXISTS `user_sessions_06_shadow` LIKE `user_sessions_06`;
CREATE TABLE IF NOT EXISTS `user_roles_06_shadow` LIKE `user_roles_06`;
CREATE TABLE IF NOT EXISTS `user_accounts_06_shadow` LIKE `user_accounts_06`;
CREATE TABLE IF NOT EXISTS `user_settings_06_shadow` LIKE `user_settings_06`;
CREATE TABLE IF NOT EXISTS `merchants_06_shadow` LIKE `merchants_06`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_06_shadow` LIKE `merchant_kyc_document_06`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_06_shadow` LIKE `merchant_channel_secret_06`;

CREATE TABLE IF NOT EXISTS `users_07_shadow` LIKE `users_07`;
CREATE TABLE IF NOT EXISTS `user_profiles_07_shadow` LIKE `user_profiles_07`;
CREATE TABLE IF NOT EXISTS `user_auths_07_shadow` LIKE `user_auths_07`;
CREATE TABLE IF NOT EXISTS `login_logs_07_shadow` LIKE `login_logs_07`;
CREATE TABLE IF NOT EXISTS `user_sessions_07_shadow` LIKE `user_sessions_07`;
CREATE TABLE IF NOT EXISTS `user_roles_07_shadow` LIKE `user_roles_07`;
CREATE TABLE IF NOT EXISTS `user_accounts_07_shadow` LIKE `user_accounts_07`;
CREATE TABLE IF NOT EXISTS `user_settings_07_shadow` LIKE `user_settings_07`;
CREATE TABLE IF NOT EXISTS `merchants_07_shadow` LIKE `merchants_07`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_07_shadow` LIKE `merchant_kyc_document_07`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_07_shadow` LIKE `merchant_channel_secret_07`;

CREATE TABLE IF NOT EXISTS `users_08_shadow` LIKE `users_08`;
CREATE TABLE IF NOT EXISTS `user_profiles_08_shadow` LIKE `user_profiles_08`;
CREATE TABLE IF NOT EXISTS `user_auths_08_shadow` LIKE `user_auths_08`;
CREATE TABLE IF NOT EXISTS `login_logs_08_shadow` LIKE `login_logs_08`;
CREATE TABLE IF NOT EXISTS `user_sessions_08_shadow` LIKE `user_sessions_08`;
CREATE TABLE IF NOT EXISTS `user_roles_08_shadow` LIKE `user_roles_08`;
CREATE TABLE IF NOT EXISTS `user_accounts_08_shadow` LIKE `user_accounts_08`;
CREATE TABLE IF NOT EXISTS `user_settings_08_shadow` LIKE `user_settings_08`;
CREATE TABLE IF NOT EXISTS `merchants_08_shadow` LIKE `merchants_08`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_08_shadow` LIKE `merchant_kyc_document_08`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_08_shadow` LIKE `merchant_channel_secret_08`;

CREATE TABLE IF NOT EXISTS `users_09_shadow` LIKE `users_09`;
CREATE TABLE IF NOT EXISTS `user_profiles_09_shadow` LIKE `user_profiles_09`;
CREATE TABLE IF NOT EXISTS `user_auths_09_shadow` LIKE `user_auths_09`;
CREATE TABLE IF NOT EXISTS `login_logs_09_shadow` LIKE `login_logs_09`;
CREATE TABLE IF NOT EXISTS `user_sessions_09_shadow` LIKE `user_sessions_09`;
CREATE TABLE IF NOT EXISTS `user_roles_09_shadow` LIKE `user_roles_09`;
CREATE TABLE IF NOT EXISTS `user_accounts_09_shadow` LIKE `user_accounts_09`;
CREATE TABLE IF NOT EXISTS `user_settings_09_shadow` LIKE `user_settings_09`;
CREATE TABLE IF NOT EXISTS `merchants_09_shadow` LIKE `merchants_09`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_09_shadow` LIKE `merchant_kyc_document_09`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_09_shadow` LIKE `merchant_channel_secret_09`;

