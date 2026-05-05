-- user_merchant_db_5 的影子表（压测 / shadow 流量）
-- 依赖：5_init.sql 已经导入完成（CREATE TABLE LIKE 需要主表存在）。
SET NAMES utf8mb4;
USE `user_merchant_db_5`;

CREATE TABLE IF NOT EXISTS `users_50_shadow` LIKE `users_50`;
CREATE TABLE IF NOT EXISTS `user_profiles_50_shadow` LIKE `user_profiles_50`;
CREATE TABLE IF NOT EXISTS `user_auths_50_shadow` LIKE `user_auths_50`;
CREATE TABLE IF NOT EXISTS `login_logs_50_shadow` LIKE `login_logs_50`;
CREATE TABLE IF NOT EXISTS `user_sessions_50_shadow` LIKE `user_sessions_50`;
CREATE TABLE IF NOT EXISTS `user_roles_50_shadow` LIKE `user_roles_50`;
CREATE TABLE IF NOT EXISTS `user_accounts_50_shadow` LIKE `user_accounts_50`;
CREATE TABLE IF NOT EXISTS `user_settings_50_shadow` LIKE `user_settings_50`;
CREATE TABLE IF NOT EXISTS `user_card_50_shadow` LIKE `user_card_50`;
CREATE TABLE IF NOT EXISTS `merchants_50_shadow` LIKE `merchants_50`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_50_shadow` LIKE `merchant_kyc_document_50`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_50_shadow` LIKE `merchant_channel_secret_50`;

CREATE TABLE IF NOT EXISTS `users_51_shadow` LIKE `users_51`;
CREATE TABLE IF NOT EXISTS `user_profiles_51_shadow` LIKE `user_profiles_51`;
CREATE TABLE IF NOT EXISTS `user_auths_51_shadow` LIKE `user_auths_51`;
CREATE TABLE IF NOT EXISTS `login_logs_51_shadow` LIKE `login_logs_51`;
CREATE TABLE IF NOT EXISTS `user_sessions_51_shadow` LIKE `user_sessions_51`;
CREATE TABLE IF NOT EXISTS `user_roles_51_shadow` LIKE `user_roles_51`;
CREATE TABLE IF NOT EXISTS `user_accounts_51_shadow` LIKE `user_accounts_51`;
CREATE TABLE IF NOT EXISTS `user_settings_51_shadow` LIKE `user_settings_51`;
CREATE TABLE IF NOT EXISTS `user_card_51_shadow` LIKE `user_card_51`;
CREATE TABLE IF NOT EXISTS `merchants_51_shadow` LIKE `merchants_51`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_51_shadow` LIKE `merchant_kyc_document_51`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_51_shadow` LIKE `merchant_channel_secret_51`;

CREATE TABLE IF NOT EXISTS `users_52_shadow` LIKE `users_52`;
CREATE TABLE IF NOT EXISTS `user_profiles_52_shadow` LIKE `user_profiles_52`;
CREATE TABLE IF NOT EXISTS `user_auths_52_shadow` LIKE `user_auths_52`;
CREATE TABLE IF NOT EXISTS `login_logs_52_shadow` LIKE `login_logs_52`;
CREATE TABLE IF NOT EXISTS `user_sessions_52_shadow` LIKE `user_sessions_52`;
CREATE TABLE IF NOT EXISTS `user_roles_52_shadow` LIKE `user_roles_52`;
CREATE TABLE IF NOT EXISTS `user_accounts_52_shadow` LIKE `user_accounts_52`;
CREATE TABLE IF NOT EXISTS `user_settings_52_shadow` LIKE `user_settings_52`;
CREATE TABLE IF NOT EXISTS `user_card_52_shadow` LIKE `user_card_52`;
CREATE TABLE IF NOT EXISTS `merchants_52_shadow` LIKE `merchants_52`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_52_shadow` LIKE `merchant_kyc_document_52`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_52_shadow` LIKE `merchant_channel_secret_52`;

CREATE TABLE IF NOT EXISTS `users_53_shadow` LIKE `users_53`;
CREATE TABLE IF NOT EXISTS `user_profiles_53_shadow` LIKE `user_profiles_53`;
CREATE TABLE IF NOT EXISTS `user_auths_53_shadow` LIKE `user_auths_53`;
CREATE TABLE IF NOT EXISTS `login_logs_53_shadow` LIKE `login_logs_53`;
CREATE TABLE IF NOT EXISTS `user_sessions_53_shadow` LIKE `user_sessions_53`;
CREATE TABLE IF NOT EXISTS `user_roles_53_shadow` LIKE `user_roles_53`;
CREATE TABLE IF NOT EXISTS `user_accounts_53_shadow` LIKE `user_accounts_53`;
CREATE TABLE IF NOT EXISTS `user_settings_53_shadow` LIKE `user_settings_53`;
CREATE TABLE IF NOT EXISTS `user_card_53_shadow` LIKE `user_card_53`;
CREATE TABLE IF NOT EXISTS `merchants_53_shadow` LIKE `merchants_53`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_53_shadow` LIKE `merchant_kyc_document_53`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_53_shadow` LIKE `merchant_channel_secret_53`;

CREATE TABLE IF NOT EXISTS `users_54_shadow` LIKE `users_54`;
CREATE TABLE IF NOT EXISTS `user_profiles_54_shadow` LIKE `user_profiles_54`;
CREATE TABLE IF NOT EXISTS `user_auths_54_shadow` LIKE `user_auths_54`;
CREATE TABLE IF NOT EXISTS `login_logs_54_shadow` LIKE `login_logs_54`;
CREATE TABLE IF NOT EXISTS `user_sessions_54_shadow` LIKE `user_sessions_54`;
CREATE TABLE IF NOT EXISTS `user_roles_54_shadow` LIKE `user_roles_54`;
CREATE TABLE IF NOT EXISTS `user_accounts_54_shadow` LIKE `user_accounts_54`;
CREATE TABLE IF NOT EXISTS `user_settings_54_shadow` LIKE `user_settings_54`;
CREATE TABLE IF NOT EXISTS `user_card_54_shadow` LIKE `user_card_54`;
CREATE TABLE IF NOT EXISTS `merchants_54_shadow` LIKE `merchants_54`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_54_shadow` LIKE `merchant_kyc_document_54`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_54_shadow` LIKE `merchant_channel_secret_54`;

CREATE TABLE IF NOT EXISTS `users_55_shadow` LIKE `users_55`;
CREATE TABLE IF NOT EXISTS `user_profiles_55_shadow` LIKE `user_profiles_55`;
CREATE TABLE IF NOT EXISTS `user_auths_55_shadow` LIKE `user_auths_55`;
CREATE TABLE IF NOT EXISTS `login_logs_55_shadow` LIKE `login_logs_55`;
CREATE TABLE IF NOT EXISTS `user_sessions_55_shadow` LIKE `user_sessions_55`;
CREATE TABLE IF NOT EXISTS `user_roles_55_shadow` LIKE `user_roles_55`;
CREATE TABLE IF NOT EXISTS `user_accounts_55_shadow` LIKE `user_accounts_55`;
CREATE TABLE IF NOT EXISTS `user_settings_55_shadow` LIKE `user_settings_55`;
CREATE TABLE IF NOT EXISTS `user_card_55_shadow` LIKE `user_card_55`;
CREATE TABLE IF NOT EXISTS `merchants_55_shadow` LIKE `merchants_55`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_55_shadow` LIKE `merchant_kyc_document_55`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_55_shadow` LIKE `merchant_channel_secret_55`;

CREATE TABLE IF NOT EXISTS `users_56_shadow` LIKE `users_56`;
CREATE TABLE IF NOT EXISTS `user_profiles_56_shadow` LIKE `user_profiles_56`;
CREATE TABLE IF NOT EXISTS `user_auths_56_shadow` LIKE `user_auths_56`;
CREATE TABLE IF NOT EXISTS `login_logs_56_shadow` LIKE `login_logs_56`;
CREATE TABLE IF NOT EXISTS `user_sessions_56_shadow` LIKE `user_sessions_56`;
CREATE TABLE IF NOT EXISTS `user_roles_56_shadow` LIKE `user_roles_56`;
CREATE TABLE IF NOT EXISTS `user_accounts_56_shadow` LIKE `user_accounts_56`;
CREATE TABLE IF NOT EXISTS `user_settings_56_shadow` LIKE `user_settings_56`;
CREATE TABLE IF NOT EXISTS `user_card_56_shadow` LIKE `user_card_56`;
CREATE TABLE IF NOT EXISTS `merchants_56_shadow` LIKE `merchants_56`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_56_shadow` LIKE `merchant_kyc_document_56`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_56_shadow` LIKE `merchant_channel_secret_56`;

CREATE TABLE IF NOT EXISTS `users_57_shadow` LIKE `users_57`;
CREATE TABLE IF NOT EXISTS `user_profiles_57_shadow` LIKE `user_profiles_57`;
CREATE TABLE IF NOT EXISTS `user_auths_57_shadow` LIKE `user_auths_57`;
CREATE TABLE IF NOT EXISTS `login_logs_57_shadow` LIKE `login_logs_57`;
CREATE TABLE IF NOT EXISTS `user_sessions_57_shadow` LIKE `user_sessions_57`;
CREATE TABLE IF NOT EXISTS `user_roles_57_shadow` LIKE `user_roles_57`;
CREATE TABLE IF NOT EXISTS `user_accounts_57_shadow` LIKE `user_accounts_57`;
CREATE TABLE IF NOT EXISTS `user_settings_57_shadow` LIKE `user_settings_57`;
CREATE TABLE IF NOT EXISTS `user_card_57_shadow` LIKE `user_card_57`;
CREATE TABLE IF NOT EXISTS `merchants_57_shadow` LIKE `merchants_57`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_57_shadow` LIKE `merchant_kyc_document_57`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_57_shadow` LIKE `merchant_channel_secret_57`;

CREATE TABLE IF NOT EXISTS `users_58_shadow` LIKE `users_58`;
CREATE TABLE IF NOT EXISTS `user_profiles_58_shadow` LIKE `user_profiles_58`;
CREATE TABLE IF NOT EXISTS `user_auths_58_shadow` LIKE `user_auths_58`;
CREATE TABLE IF NOT EXISTS `login_logs_58_shadow` LIKE `login_logs_58`;
CREATE TABLE IF NOT EXISTS `user_sessions_58_shadow` LIKE `user_sessions_58`;
CREATE TABLE IF NOT EXISTS `user_roles_58_shadow` LIKE `user_roles_58`;
CREATE TABLE IF NOT EXISTS `user_accounts_58_shadow` LIKE `user_accounts_58`;
CREATE TABLE IF NOT EXISTS `user_settings_58_shadow` LIKE `user_settings_58`;
CREATE TABLE IF NOT EXISTS `user_card_58_shadow` LIKE `user_card_58`;
CREATE TABLE IF NOT EXISTS `merchants_58_shadow` LIKE `merchants_58`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_58_shadow` LIKE `merchant_kyc_document_58`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_58_shadow` LIKE `merchant_channel_secret_58`;

CREATE TABLE IF NOT EXISTS `users_59_shadow` LIKE `users_59`;
CREATE TABLE IF NOT EXISTS `user_profiles_59_shadow` LIKE `user_profiles_59`;
CREATE TABLE IF NOT EXISTS `user_auths_59_shadow` LIKE `user_auths_59`;
CREATE TABLE IF NOT EXISTS `login_logs_59_shadow` LIKE `login_logs_59`;
CREATE TABLE IF NOT EXISTS `user_sessions_59_shadow` LIKE `user_sessions_59`;
CREATE TABLE IF NOT EXISTS `user_roles_59_shadow` LIKE `user_roles_59`;
CREATE TABLE IF NOT EXISTS `user_accounts_59_shadow` LIKE `user_accounts_59`;
CREATE TABLE IF NOT EXISTS `user_settings_59_shadow` LIKE `user_settings_59`;
CREATE TABLE IF NOT EXISTS `user_card_59_shadow` LIKE `user_card_59`;
CREATE TABLE IF NOT EXISTS `merchants_59_shadow` LIKE `merchants_59`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_59_shadow` LIKE `merchant_kyc_document_59`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_59_shadow` LIKE `merchant_channel_secret_59`;

