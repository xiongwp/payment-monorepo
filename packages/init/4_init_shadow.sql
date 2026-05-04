-- user_merchant_db_4 的影子表（压测 / shadow 流量）
-- 依赖：4_init.sql 已经导入完成（CREATE TABLE LIKE 需要主表存在）。
SET NAMES utf8mb4;
USE `user_merchant_db_4`;

CREATE TABLE IF NOT EXISTS `users_40_shadow` LIKE `users_40`;
CREATE TABLE IF NOT EXISTS `user_profiles_40_shadow` LIKE `user_profiles_40`;
CREATE TABLE IF NOT EXISTS `user_auths_40_shadow` LIKE `user_auths_40`;
CREATE TABLE IF NOT EXISTS `login_logs_40_shadow` LIKE `login_logs_40`;
CREATE TABLE IF NOT EXISTS `user_sessions_40_shadow` LIKE `user_sessions_40`;
CREATE TABLE IF NOT EXISTS `user_roles_40_shadow` LIKE `user_roles_40`;
CREATE TABLE IF NOT EXISTS `user_accounts_40_shadow` LIKE `user_accounts_40`;
CREATE TABLE IF NOT EXISTS `user_settings_40_shadow` LIKE `user_settings_40`;
CREATE TABLE IF NOT EXISTS `merchants_40_shadow` LIKE `merchants_40`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_40_shadow` LIKE `merchant_kyc_document_40`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_40_shadow` LIKE `merchant_channel_secret_40`;

CREATE TABLE IF NOT EXISTS `users_41_shadow` LIKE `users_41`;
CREATE TABLE IF NOT EXISTS `user_profiles_41_shadow` LIKE `user_profiles_41`;
CREATE TABLE IF NOT EXISTS `user_auths_41_shadow` LIKE `user_auths_41`;
CREATE TABLE IF NOT EXISTS `login_logs_41_shadow` LIKE `login_logs_41`;
CREATE TABLE IF NOT EXISTS `user_sessions_41_shadow` LIKE `user_sessions_41`;
CREATE TABLE IF NOT EXISTS `user_roles_41_shadow` LIKE `user_roles_41`;
CREATE TABLE IF NOT EXISTS `user_accounts_41_shadow` LIKE `user_accounts_41`;
CREATE TABLE IF NOT EXISTS `user_settings_41_shadow` LIKE `user_settings_41`;
CREATE TABLE IF NOT EXISTS `merchants_41_shadow` LIKE `merchants_41`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_41_shadow` LIKE `merchant_kyc_document_41`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_41_shadow` LIKE `merchant_channel_secret_41`;

CREATE TABLE IF NOT EXISTS `users_42_shadow` LIKE `users_42`;
CREATE TABLE IF NOT EXISTS `user_profiles_42_shadow` LIKE `user_profiles_42`;
CREATE TABLE IF NOT EXISTS `user_auths_42_shadow` LIKE `user_auths_42`;
CREATE TABLE IF NOT EXISTS `login_logs_42_shadow` LIKE `login_logs_42`;
CREATE TABLE IF NOT EXISTS `user_sessions_42_shadow` LIKE `user_sessions_42`;
CREATE TABLE IF NOT EXISTS `user_roles_42_shadow` LIKE `user_roles_42`;
CREATE TABLE IF NOT EXISTS `user_accounts_42_shadow` LIKE `user_accounts_42`;
CREATE TABLE IF NOT EXISTS `user_settings_42_shadow` LIKE `user_settings_42`;
CREATE TABLE IF NOT EXISTS `merchants_42_shadow` LIKE `merchants_42`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_42_shadow` LIKE `merchant_kyc_document_42`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_42_shadow` LIKE `merchant_channel_secret_42`;

CREATE TABLE IF NOT EXISTS `users_43_shadow` LIKE `users_43`;
CREATE TABLE IF NOT EXISTS `user_profiles_43_shadow` LIKE `user_profiles_43`;
CREATE TABLE IF NOT EXISTS `user_auths_43_shadow` LIKE `user_auths_43`;
CREATE TABLE IF NOT EXISTS `login_logs_43_shadow` LIKE `login_logs_43`;
CREATE TABLE IF NOT EXISTS `user_sessions_43_shadow` LIKE `user_sessions_43`;
CREATE TABLE IF NOT EXISTS `user_roles_43_shadow` LIKE `user_roles_43`;
CREATE TABLE IF NOT EXISTS `user_accounts_43_shadow` LIKE `user_accounts_43`;
CREATE TABLE IF NOT EXISTS `user_settings_43_shadow` LIKE `user_settings_43`;
CREATE TABLE IF NOT EXISTS `merchants_43_shadow` LIKE `merchants_43`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_43_shadow` LIKE `merchant_kyc_document_43`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_43_shadow` LIKE `merchant_channel_secret_43`;

CREATE TABLE IF NOT EXISTS `users_44_shadow` LIKE `users_44`;
CREATE TABLE IF NOT EXISTS `user_profiles_44_shadow` LIKE `user_profiles_44`;
CREATE TABLE IF NOT EXISTS `user_auths_44_shadow` LIKE `user_auths_44`;
CREATE TABLE IF NOT EXISTS `login_logs_44_shadow` LIKE `login_logs_44`;
CREATE TABLE IF NOT EXISTS `user_sessions_44_shadow` LIKE `user_sessions_44`;
CREATE TABLE IF NOT EXISTS `user_roles_44_shadow` LIKE `user_roles_44`;
CREATE TABLE IF NOT EXISTS `user_accounts_44_shadow` LIKE `user_accounts_44`;
CREATE TABLE IF NOT EXISTS `user_settings_44_shadow` LIKE `user_settings_44`;
CREATE TABLE IF NOT EXISTS `merchants_44_shadow` LIKE `merchants_44`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_44_shadow` LIKE `merchant_kyc_document_44`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_44_shadow` LIKE `merchant_channel_secret_44`;

CREATE TABLE IF NOT EXISTS `users_45_shadow` LIKE `users_45`;
CREATE TABLE IF NOT EXISTS `user_profiles_45_shadow` LIKE `user_profiles_45`;
CREATE TABLE IF NOT EXISTS `user_auths_45_shadow` LIKE `user_auths_45`;
CREATE TABLE IF NOT EXISTS `login_logs_45_shadow` LIKE `login_logs_45`;
CREATE TABLE IF NOT EXISTS `user_sessions_45_shadow` LIKE `user_sessions_45`;
CREATE TABLE IF NOT EXISTS `user_roles_45_shadow` LIKE `user_roles_45`;
CREATE TABLE IF NOT EXISTS `user_accounts_45_shadow` LIKE `user_accounts_45`;
CREATE TABLE IF NOT EXISTS `user_settings_45_shadow` LIKE `user_settings_45`;
CREATE TABLE IF NOT EXISTS `merchants_45_shadow` LIKE `merchants_45`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_45_shadow` LIKE `merchant_kyc_document_45`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_45_shadow` LIKE `merchant_channel_secret_45`;

CREATE TABLE IF NOT EXISTS `users_46_shadow` LIKE `users_46`;
CREATE TABLE IF NOT EXISTS `user_profiles_46_shadow` LIKE `user_profiles_46`;
CREATE TABLE IF NOT EXISTS `user_auths_46_shadow` LIKE `user_auths_46`;
CREATE TABLE IF NOT EXISTS `login_logs_46_shadow` LIKE `login_logs_46`;
CREATE TABLE IF NOT EXISTS `user_sessions_46_shadow` LIKE `user_sessions_46`;
CREATE TABLE IF NOT EXISTS `user_roles_46_shadow` LIKE `user_roles_46`;
CREATE TABLE IF NOT EXISTS `user_accounts_46_shadow` LIKE `user_accounts_46`;
CREATE TABLE IF NOT EXISTS `user_settings_46_shadow` LIKE `user_settings_46`;
CREATE TABLE IF NOT EXISTS `merchants_46_shadow` LIKE `merchants_46`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_46_shadow` LIKE `merchant_kyc_document_46`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_46_shadow` LIKE `merchant_channel_secret_46`;

CREATE TABLE IF NOT EXISTS `users_47_shadow` LIKE `users_47`;
CREATE TABLE IF NOT EXISTS `user_profiles_47_shadow` LIKE `user_profiles_47`;
CREATE TABLE IF NOT EXISTS `user_auths_47_shadow` LIKE `user_auths_47`;
CREATE TABLE IF NOT EXISTS `login_logs_47_shadow` LIKE `login_logs_47`;
CREATE TABLE IF NOT EXISTS `user_sessions_47_shadow` LIKE `user_sessions_47`;
CREATE TABLE IF NOT EXISTS `user_roles_47_shadow` LIKE `user_roles_47`;
CREATE TABLE IF NOT EXISTS `user_accounts_47_shadow` LIKE `user_accounts_47`;
CREATE TABLE IF NOT EXISTS `user_settings_47_shadow` LIKE `user_settings_47`;
CREATE TABLE IF NOT EXISTS `merchants_47_shadow` LIKE `merchants_47`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_47_shadow` LIKE `merchant_kyc_document_47`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_47_shadow` LIKE `merchant_channel_secret_47`;

CREATE TABLE IF NOT EXISTS `users_48_shadow` LIKE `users_48`;
CREATE TABLE IF NOT EXISTS `user_profiles_48_shadow` LIKE `user_profiles_48`;
CREATE TABLE IF NOT EXISTS `user_auths_48_shadow` LIKE `user_auths_48`;
CREATE TABLE IF NOT EXISTS `login_logs_48_shadow` LIKE `login_logs_48`;
CREATE TABLE IF NOT EXISTS `user_sessions_48_shadow` LIKE `user_sessions_48`;
CREATE TABLE IF NOT EXISTS `user_roles_48_shadow` LIKE `user_roles_48`;
CREATE TABLE IF NOT EXISTS `user_accounts_48_shadow` LIKE `user_accounts_48`;
CREATE TABLE IF NOT EXISTS `user_settings_48_shadow` LIKE `user_settings_48`;
CREATE TABLE IF NOT EXISTS `merchants_48_shadow` LIKE `merchants_48`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_48_shadow` LIKE `merchant_kyc_document_48`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_48_shadow` LIKE `merchant_channel_secret_48`;

CREATE TABLE IF NOT EXISTS `users_49_shadow` LIKE `users_49`;
CREATE TABLE IF NOT EXISTS `user_profiles_49_shadow` LIKE `user_profiles_49`;
CREATE TABLE IF NOT EXISTS `user_auths_49_shadow` LIKE `user_auths_49`;
CREATE TABLE IF NOT EXISTS `login_logs_49_shadow` LIKE `login_logs_49`;
CREATE TABLE IF NOT EXISTS `user_sessions_49_shadow` LIKE `user_sessions_49`;
CREATE TABLE IF NOT EXISTS `user_roles_49_shadow` LIKE `user_roles_49`;
CREATE TABLE IF NOT EXISTS `user_accounts_49_shadow` LIKE `user_accounts_49`;
CREATE TABLE IF NOT EXISTS `user_settings_49_shadow` LIKE `user_settings_49`;
CREATE TABLE IF NOT EXISTS `merchants_49_shadow` LIKE `merchants_49`;
CREATE TABLE IF NOT EXISTS `merchant_kyc_document_49_shadow` LIKE `merchant_kyc_document_49`;
CREATE TABLE IF NOT EXISTS `merchant_channel_secret_49_shadow` LIKE `merchant_channel_secret_49`;

