-- user_merchant_db_7 的影子表（压测 / shadow 流量）
-- 依赖：7_init.sql 已经导入完成（CREATE TABLE LIKE 需要主表存在）。
SET NAMES utf8mb4;
USE `user_merchant_db_7`;

CREATE TABLE IF NOT EXISTS `users_70_shadow` LIKE `users_70`;
CREATE TABLE IF NOT EXISTS `user_profiles_70_shadow` LIKE `user_profiles_70`;
CREATE TABLE IF NOT EXISTS `user_auths_70_shadow` LIKE `user_auths_70`;
CREATE TABLE IF NOT EXISTS `login_logs_70_shadow` LIKE `login_logs_70`;
CREATE TABLE IF NOT EXISTS `user_sessions_70_shadow` LIKE `user_sessions_70`;
CREATE TABLE IF NOT EXISTS `user_roles_70_shadow` LIKE `user_roles_70`;
CREATE TABLE IF NOT EXISTS `user_accounts_70_shadow` LIKE `user_accounts_70`;
CREATE TABLE IF NOT EXISTS `user_settings_70_shadow` LIKE `user_settings_70`;
CREATE TABLE IF NOT EXISTS `user_card_70_shadow` LIKE `user_card_70`;
CREATE TABLE IF NOT EXISTS `merchants_70_shadow` LIKE `merchants_70`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_70_shadow` LIKE `merchant_kyc_document_70`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_70_shadow` LIKE `merchant_channel_secret_70`;

CREATE TABLE IF NOT EXISTS `users_71_shadow` LIKE `users_71`;
CREATE TABLE IF NOT EXISTS `user_profiles_71_shadow` LIKE `user_profiles_71`;
CREATE TABLE IF NOT EXISTS `user_auths_71_shadow` LIKE `user_auths_71`;
CREATE TABLE IF NOT EXISTS `login_logs_71_shadow` LIKE `login_logs_71`;
CREATE TABLE IF NOT EXISTS `user_sessions_71_shadow` LIKE `user_sessions_71`;
CREATE TABLE IF NOT EXISTS `user_roles_71_shadow` LIKE `user_roles_71`;
CREATE TABLE IF NOT EXISTS `user_accounts_71_shadow` LIKE `user_accounts_71`;
CREATE TABLE IF NOT EXISTS `user_settings_71_shadow` LIKE `user_settings_71`;
CREATE TABLE IF NOT EXISTS `user_card_71_shadow` LIKE `user_card_71`;
CREATE TABLE IF NOT EXISTS `merchants_71_shadow` LIKE `merchants_71`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_71_shadow` LIKE `merchant_kyc_document_71`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_71_shadow` LIKE `merchant_channel_secret_71`;

CREATE TABLE IF NOT EXISTS `users_72_shadow` LIKE `users_72`;
CREATE TABLE IF NOT EXISTS `user_profiles_72_shadow` LIKE `user_profiles_72`;
CREATE TABLE IF NOT EXISTS `user_auths_72_shadow` LIKE `user_auths_72`;
CREATE TABLE IF NOT EXISTS `login_logs_72_shadow` LIKE `login_logs_72`;
CREATE TABLE IF NOT EXISTS `user_sessions_72_shadow` LIKE `user_sessions_72`;
CREATE TABLE IF NOT EXISTS `user_roles_72_shadow` LIKE `user_roles_72`;
CREATE TABLE IF NOT EXISTS `user_accounts_72_shadow` LIKE `user_accounts_72`;
CREATE TABLE IF NOT EXISTS `user_settings_72_shadow` LIKE `user_settings_72`;
CREATE TABLE IF NOT EXISTS `user_card_72_shadow` LIKE `user_card_72`;
CREATE TABLE IF NOT EXISTS `merchants_72_shadow` LIKE `merchants_72`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_72_shadow` LIKE `merchant_kyc_document_72`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_72_shadow` LIKE `merchant_channel_secret_72`;

CREATE TABLE IF NOT EXISTS `users_73_shadow` LIKE `users_73`;
CREATE TABLE IF NOT EXISTS `user_profiles_73_shadow` LIKE `user_profiles_73`;
CREATE TABLE IF NOT EXISTS `user_auths_73_shadow` LIKE `user_auths_73`;
CREATE TABLE IF NOT EXISTS `login_logs_73_shadow` LIKE `login_logs_73`;
CREATE TABLE IF NOT EXISTS `user_sessions_73_shadow` LIKE `user_sessions_73`;
CREATE TABLE IF NOT EXISTS `user_roles_73_shadow` LIKE `user_roles_73`;
CREATE TABLE IF NOT EXISTS `user_accounts_73_shadow` LIKE `user_accounts_73`;
CREATE TABLE IF NOT EXISTS `user_settings_73_shadow` LIKE `user_settings_73`;
CREATE TABLE IF NOT EXISTS `user_card_73_shadow` LIKE `user_card_73`;
CREATE TABLE IF NOT EXISTS `merchants_73_shadow` LIKE `merchants_73`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_73_shadow` LIKE `merchant_kyc_document_73`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_73_shadow` LIKE `merchant_channel_secret_73`;

CREATE TABLE IF NOT EXISTS `users_74_shadow` LIKE `users_74`;
CREATE TABLE IF NOT EXISTS `user_profiles_74_shadow` LIKE `user_profiles_74`;
CREATE TABLE IF NOT EXISTS `user_auths_74_shadow` LIKE `user_auths_74`;
CREATE TABLE IF NOT EXISTS `login_logs_74_shadow` LIKE `login_logs_74`;
CREATE TABLE IF NOT EXISTS `user_sessions_74_shadow` LIKE `user_sessions_74`;
CREATE TABLE IF NOT EXISTS `user_roles_74_shadow` LIKE `user_roles_74`;
CREATE TABLE IF NOT EXISTS `user_accounts_74_shadow` LIKE `user_accounts_74`;
CREATE TABLE IF NOT EXISTS `user_settings_74_shadow` LIKE `user_settings_74`;
CREATE TABLE IF NOT EXISTS `user_card_74_shadow` LIKE `user_card_74`;
CREATE TABLE IF NOT EXISTS `merchants_74_shadow` LIKE `merchants_74`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_74_shadow` LIKE `merchant_kyc_document_74`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_74_shadow` LIKE `merchant_channel_secret_74`;

CREATE TABLE IF NOT EXISTS `users_75_shadow` LIKE `users_75`;
CREATE TABLE IF NOT EXISTS `user_profiles_75_shadow` LIKE `user_profiles_75`;
CREATE TABLE IF NOT EXISTS `user_auths_75_shadow` LIKE `user_auths_75`;
CREATE TABLE IF NOT EXISTS `login_logs_75_shadow` LIKE `login_logs_75`;
CREATE TABLE IF NOT EXISTS `user_sessions_75_shadow` LIKE `user_sessions_75`;
CREATE TABLE IF NOT EXISTS `user_roles_75_shadow` LIKE `user_roles_75`;
CREATE TABLE IF NOT EXISTS `user_accounts_75_shadow` LIKE `user_accounts_75`;
CREATE TABLE IF NOT EXISTS `user_settings_75_shadow` LIKE `user_settings_75`;
CREATE TABLE IF NOT EXISTS `user_card_75_shadow` LIKE `user_card_75`;
CREATE TABLE IF NOT EXISTS `merchants_75_shadow` LIKE `merchants_75`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_75_shadow` LIKE `merchant_kyc_document_75`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_75_shadow` LIKE `merchant_channel_secret_75`;

CREATE TABLE IF NOT EXISTS `users_76_shadow` LIKE `users_76`;
CREATE TABLE IF NOT EXISTS `user_profiles_76_shadow` LIKE `user_profiles_76`;
CREATE TABLE IF NOT EXISTS `user_auths_76_shadow` LIKE `user_auths_76`;
CREATE TABLE IF NOT EXISTS `login_logs_76_shadow` LIKE `login_logs_76`;
CREATE TABLE IF NOT EXISTS `user_sessions_76_shadow` LIKE `user_sessions_76`;
CREATE TABLE IF NOT EXISTS `user_roles_76_shadow` LIKE `user_roles_76`;
CREATE TABLE IF NOT EXISTS `user_accounts_76_shadow` LIKE `user_accounts_76`;
CREATE TABLE IF NOT EXISTS `user_settings_76_shadow` LIKE `user_settings_76`;
CREATE TABLE IF NOT EXISTS `user_card_76_shadow` LIKE `user_card_76`;
CREATE TABLE IF NOT EXISTS `merchants_76_shadow` LIKE `merchants_76`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_76_shadow` LIKE `merchant_kyc_document_76`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_76_shadow` LIKE `merchant_channel_secret_76`;

CREATE TABLE IF NOT EXISTS `users_77_shadow` LIKE `users_77`;
CREATE TABLE IF NOT EXISTS `user_profiles_77_shadow` LIKE `user_profiles_77`;
CREATE TABLE IF NOT EXISTS `user_auths_77_shadow` LIKE `user_auths_77`;
CREATE TABLE IF NOT EXISTS `login_logs_77_shadow` LIKE `login_logs_77`;
CREATE TABLE IF NOT EXISTS `user_sessions_77_shadow` LIKE `user_sessions_77`;
CREATE TABLE IF NOT EXISTS `user_roles_77_shadow` LIKE `user_roles_77`;
CREATE TABLE IF NOT EXISTS `user_accounts_77_shadow` LIKE `user_accounts_77`;
CREATE TABLE IF NOT EXISTS `user_settings_77_shadow` LIKE `user_settings_77`;
CREATE TABLE IF NOT EXISTS `user_card_77_shadow` LIKE `user_card_77`;
CREATE TABLE IF NOT EXISTS `merchants_77_shadow` LIKE `merchants_77`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_77_shadow` LIKE `merchant_kyc_document_77`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_77_shadow` LIKE `merchant_channel_secret_77`;

CREATE TABLE IF NOT EXISTS `users_78_shadow` LIKE `users_78`;
CREATE TABLE IF NOT EXISTS `user_profiles_78_shadow` LIKE `user_profiles_78`;
CREATE TABLE IF NOT EXISTS `user_auths_78_shadow` LIKE `user_auths_78`;
CREATE TABLE IF NOT EXISTS `login_logs_78_shadow` LIKE `login_logs_78`;
CREATE TABLE IF NOT EXISTS `user_sessions_78_shadow` LIKE `user_sessions_78`;
CREATE TABLE IF NOT EXISTS `user_roles_78_shadow` LIKE `user_roles_78`;
CREATE TABLE IF NOT EXISTS `user_accounts_78_shadow` LIKE `user_accounts_78`;
CREATE TABLE IF NOT EXISTS `user_settings_78_shadow` LIKE `user_settings_78`;
CREATE TABLE IF NOT EXISTS `user_card_78_shadow` LIKE `user_card_78`;
CREATE TABLE IF NOT EXISTS `merchants_78_shadow` LIKE `merchants_78`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_78_shadow` LIKE `merchant_kyc_document_78`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_78_shadow` LIKE `merchant_channel_secret_78`;

CREATE TABLE IF NOT EXISTS `users_79_shadow` LIKE `users_79`;
CREATE TABLE IF NOT EXISTS `user_profiles_79_shadow` LIKE `user_profiles_79`;
CREATE TABLE IF NOT EXISTS `user_auths_79_shadow` LIKE `user_auths_79`;
CREATE TABLE IF NOT EXISTS `login_logs_79_shadow` LIKE `login_logs_79`;
CREATE TABLE IF NOT EXISTS `user_sessions_79_shadow` LIKE `user_sessions_79`;
CREATE TABLE IF NOT EXISTS `user_roles_79_shadow` LIKE `user_roles_79`;
CREATE TABLE IF NOT EXISTS `user_accounts_79_shadow` LIKE `user_accounts_79`;
CREATE TABLE IF NOT EXISTS `user_settings_79_shadow` LIKE `user_settings_79`;
CREATE TABLE IF NOT EXISTS `user_card_79_shadow` LIKE `user_card_79`;
CREATE TABLE IF NOT EXISTS `merchants_79_shadow` LIKE `merchants_79`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_79_shadow` LIKE `merchant_kyc_document_79`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_79_shadow` LIKE `merchant_channel_secret_79`;

