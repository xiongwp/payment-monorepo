-- user_merchant_db_8 的影子表（压测 / shadow 流量）
-- 依赖：8_init.sql 已经导入完成（CREATE TABLE LIKE 需要主表存在）。
SET NAMES utf8mb4;
USE `user_merchant_db_8`;

CREATE TABLE IF NOT EXISTS `users_80_shadow` LIKE `users_80`;
CREATE TABLE IF NOT EXISTS `user_profiles_80_shadow` LIKE `user_profiles_80`;
CREATE TABLE IF NOT EXISTS `user_auths_80_shadow` LIKE `user_auths_80`;
CREATE TABLE IF NOT EXISTS `login_logs_80_shadow` LIKE `login_logs_80`;
CREATE TABLE IF NOT EXISTS `user_sessions_80_shadow` LIKE `user_sessions_80`;
CREATE TABLE IF NOT EXISTS `user_roles_80_shadow` LIKE `user_roles_80`;
CREATE TABLE IF NOT EXISTS `user_accounts_80_shadow` LIKE `user_accounts_80`;
CREATE TABLE IF NOT EXISTS `user_settings_80_shadow` LIKE `user_settings_80`;
CREATE TABLE IF NOT EXISTS `merchants_80_shadow` LIKE `merchants_80`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_80_shadow` LIKE `merchant_kyc_document_80`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_80_shadow` LIKE `merchant_channel_secret_80`;

CREATE TABLE IF NOT EXISTS `users_81_shadow` LIKE `users_81`;
CREATE TABLE IF NOT EXISTS `user_profiles_81_shadow` LIKE `user_profiles_81`;
CREATE TABLE IF NOT EXISTS `user_auths_81_shadow` LIKE `user_auths_81`;
CREATE TABLE IF NOT EXISTS `login_logs_81_shadow` LIKE `login_logs_81`;
CREATE TABLE IF NOT EXISTS `user_sessions_81_shadow` LIKE `user_sessions_81`;
CREATE TABLE IF NOT EXISTS `user_roles_81_shadow` LIKE `user_roles_81`;
CREATE TABLE IF NOT EXISTS `user_accounts_81_shadow` LIKE `user_accounts_81`;
CREATE TABLE IF NOT EXISTS `user_settings_81_shadow` LIKE `user_settings_81`;
CREATE TABLE IF NOT EXISTS `merchants_81_shadow` LIKE `merchants_81`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_81_shadow` LIKE `merchant_kyc_document_81`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_81_shadow` LIKE `merchant_channel_secret_81`;

CREATE TABLE IF NOT EXISTS `users_82_shadow` LIKE `users_82`;
CREATE TABLE IF NOT EXISTS `user_profiles_82_shadow` LIKE `user_profiles_82`;
CREATE TABLE IF NOT EXISTS `user_auths_82_shadow` LIKE `user_auths_82`;
CREATE TABLE IF NOT EXISTS `login_logs_82_shadow` LIKE `login_logs_82`;
CREATE TABLE IF NOT EXISTS `user_sessions_82_shadow` LIKE `user_sessions_82`;
CREATE TABLE IF NOT EXISTS `user_roles_82_shadow` LIKE `user_roles_82`;
CREATE TABLE IF NOT EXISTS `user_accounts_82_shadow` LIKE `user_accounts_82`;
CREATE TABLE IF NOT EXISTS `user_settings_82_shadow` LIKE `user_settings_82`;
CREATE TABLE IF NOT EXISTS `merchants_82_shadow` LIKE `merchants_82`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_82_shadow` LIKE `merchant_kyc_document_82`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_82_shadow` LIKE `merchant_channel_secret_82`;

CREATE TABLE IF NOT EXISTS `users_83_shadow` LIKE `users_83`;
CREATE TABLE IF NOT EXISTS `user_profiles_83_shadow` LIKE `user_profiles_83`;
CREATE TABLE IF NOT EXISTS `user_auths_83_shadow` LIKE `user_auths_83`;
CREATE TABLE IF NOT EXISTS `login_logs_83_shadow` LIKE `login_logs_83`;
CREATE TABLE IF NOT EXISTS `user_sessions_83_shadow` LIKE `user_sessions_83`;
CREATE TABLE IF NOT EXISTS `user_roles_83_shadow` LIKE `user_roles_83`;
CREATE TABLE IF NOT EXISTS `user_accounts_83_shadow` LIKE `user_accounts_83`;
CREATE TABLE IF NOT EXISTS `user_settings_83_shadow` LIKE `user_settings_83`;
CREATE TABLE IF NOT EXISTS `merchants_83_shadow` LIKE `merchants_83`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_83_shadow` LIKE `merchant_kyc_document_83`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_83_shadow` LIKE `merchant_channel_secret_83`;

CREATE TABLE IF NOT EXISTS `users_84_shadow` LIKE `users_84`;
CREATE TABLE IF NOT EXISTS `user_profiles_84_shadow` LIKE `user_profiles_84`;
CREATE TABLE IF NOT EXISTS `user_auths_84_shadow` LIKE `user_auths_84`;
CREATE TABLE IF NOT EXISTS `login_logs_84_shadow` LIKE `login_logs_84`;
CREATE TABLE IF NOT EXISTS `user_sessions_84_shadow` LIKE `user_sessions_84`;
CREATE TABLE IF NOT EXISTS `user_roles_84_shadow` LIKE `user_roles_84`;
CREATE TABLE IF NOT EXISTS `user_accounts_84_shadow` LIKE `user_accounts_84`;
CREATE TABLE IF NOT EXISTS `user_settings_84_shadow` LIKE `user_settings_84`;
CREATE TABLE IF NOT EXISTS `merchants_84_shadow` LIKE `merchants_84`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_84_shadow` LIKE `merchant_kyc_document_84`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_84_shadow` LIKE `merchant_channel_secret_84`;

CREATE TABLE IF NOT EXISTS `users_85_shadow` LIKE `users_85`;
CREATE TABLE IF NOT EXISTS `user_profiles_85_shadow` LIKE `user_profiles_85`;
CREATE TABLE IF NOT EXISTS `user_auths_85_shadow` LIKE `user_auths_85`;
CREATE TABLE IF NOT EXISTS `login_logs_85_shadow` LIKE `login_logs_85`;
CREATE TABLE IF NOT EXISTS `user_sessions_85_shadow` LIKE `user_sessions_85`;
CREATE TABLE IF NOT EXISTS `user_roles_85_shadow` LIKE `user_roles_85`;
CREATE TABLE IF NOT EXISTS `user_accounts_85_shadow` LIKE `user_accounts_85`;
CREATE TABLE IF NOT EXISTS `user_settings_85_shadow` LIKE `user_settings_85`;
CREATE TABLE IF NOT EXISTS `merchants_85_shadow` LIKE `merchants_85`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_85_shadow` LIKE `merchant_kyc_document_85`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_85_shadow` LIKE `merchant_channel_secret_85`;

CREATE TABLE IF NOT EXISTS `users_86_shadow` LIKE `users_86`;
CREATE TABLE IF NOT EXISTS `user_profiles_86_shadow` LIKE `user_profiles_86`;
CREATE TABLE IF NOT EXISTS `user_auths_86_shadow` LIKE `user_auths_86`;
CREATE TABLE IF NOT EXISTS `login_logs_86_shadow` LIKE `login_logs_86`;
CREATE TABLE IF NOT EXISTS `user_sessions_86_shadow` LIKE `user_sessions_86`;
CREATE TABLE IF NOT EXISTS `user_roles_86_shadow` LIKE `user_roles_86`;
CREATE TABLE IF NOT EXISTS `user_accounts_86_shadow` LIKE `user_accounts_86`;
CREATE TABLE IF NOT EXISTS `user_settings_86_shadow` LIKE `user_settings_86`;
CREATE TABLE IF NOT EXISTS `merchants_86_shadow` LIKE `merchants_86`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_86_shadow` LIKE `merchant_kyc_document_86`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_86_shadow` LIKE `merchant_channel_secret_86`;

CREATE TABLE IF NOT EXISTS `users_87_shadow` LIKE `users_87`;
CREATE TABLE IF NOT EXISTS `user_profiles_87_shadow` LIKE `user_profiles_87`;
CREATE TABLE IF NOT EXISTS `user_auths_87_shadow` LIKE `user_auths_87`;
CREATE TABLE IF NOT EXISTS `login_logs_87_shadow` LIKE `login_logs_87`;
CREATE TABLE IF NOT EXISTS `user_sessions_87_shadow` LIKE `user_sessions_87`;
CREATE TABLE IF NOT EXISTS `user_roles_87_shadow` LIKE `user_roles_87`;
CREATE TABLE IF NOT EXISTS `user_accounts_87_shadow` LIKE `user_accounts_87`;
CREATE TABLE IF NOT EXISTS `user_settings_87_shadow` LIKE `user_settings_87`;
CREATE TABLE IF NOT EXISTS `merchants_87_shadow` LIKE `merchants_87`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_87_shadow` LIKE `merchant_kyc_document_87`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_87_shadow` LIKE `merchant_channel_secret_87`;

CREATE TABLE IF NOT EXISTS `users_88_shadow` LIKE `users_88`;
CREATE TABLE IF NOT EXISTS `user_profiles_88_shadow` LIKE `user_profiles_88`;
CREATE TABLE IF NOT EXISTS `user_auths_88_shadow` LIKE `user_auths_88`;
CREATE TABLE IF NOT EXISTS `login_logs_88_shadow` LIKE `login_logs_88`;
CREATE TABLE IF NOT EXISTS `user_sessions_88_shadow` LIKE `user_sessions_88`;
CREATE TABLE IF NOT EXISTS `user_roles_88_shadow` LIKE `user_roles_88`;
CREATE TABLE IF NOT EXISTS `user_accounts_88_shadow` LIKE `user_accounts_88`;
CREATE TABLE IF NOT EXISTS `user_settings_88_shadow` LIKE `user_settings_88`;
CREATE TABLE IF NOT EXISTS `merchants_88_shadow` LIKE `merchants_88`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_88_shadow` LIKE `merchant_kyc_document_88`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_88_shadow` LIKE `merchant_channel_secret_88`;

CREATE TABLE IF NOT EXISTS `users_89_shadow` LIKE `users_89`;
CREATE TABLE IF NOT EXISTS `user_profiles_89_shadow` LIKE `user_profiles_89`;
CREATE TABLE IF NOT EXISTS `user_auths_89_shadow` LIKE `user_auths_89`;
CREATE TABLE IF NOT EXISTS `login_logs_89_shadow` LIKE `login_logs_89`;
CREATE TABLE IF NOT EXISTS `user_sessions_89_shadow` LIKE `user_sessions_89`;
CREATE TABLE IF NOT EXISTS `user_roles_89_shadow` LIKE `user_roles_89`;
CREATE TABLE IF NOT EXISTS `user_accounts_89_shadow` LIKE `user_accounts_89`;
CREATE TABLE IF NOT EXISTS `user_settings_89_shadow` LIKE `user_settings_89`;
CREATE TABLE IF NOT EXISTS `merchants_89_shadow` LIKE `merchants_89`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_89_shadow` LIKE `merchant_kyc_document_89`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_89_shadow` LIKE `merchant_channel_secret_89`;

