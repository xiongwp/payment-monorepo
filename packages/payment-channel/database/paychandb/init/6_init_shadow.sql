-- paychan_db_6 的影子表（压测 / shadow 流量）
-- 依赖：6_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_6`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_60_shadow` LIKE `acquirer_tx_60`;
CREATE TABLE IF NOT EXISTS `webhook_raw_60_shadow` LIKE `webhook_raw_60`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_60_shadow` LIKE `webhook_raw_rejected_60`;
CREATE TABLE IF NOT EXISTS `channel_token_60_shadow` LIKE `channel_token_60`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_61_shadow` LIKE `acquirer_tx_61`;
CREATE TABLE IF NOT EXISTS `webhook_raw_61_shadow` LIKE `webhook_raw_61`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_61_shadow` LIKE `webhook_raw_rejected_61`;
CREATE TABLE IF NOT EXISTS `channel_token_61_shadow` LIKE `channel_token_61`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_62_shadow` LIKE `acquirer_tx_62`;
CREATE TABLE IF NOT EXISTS `webhook_raw_62_shadow` LIKE `webhook_raw_62`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_62_shadow` LIKE `webhook_raw_rejected_62`;
CREATE TABLE IF NOT EXISTS `channel_token_62_shadow` LIKE `channel_token_62`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_63_shadow` LIKE `acquirer_tx_63`;
CREATE TABLE IF NOT EXISTS `webhook_raw_63_shadow` LIKE `webhook_raw_63`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_63_shadow` LIKE `webhook_raw_rejected_63`;
CREATE TABLE IF NOT EXISTS `channel_token_63_shadow` LIKE `channel_token_63`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_64_shadow` LIKE `acquirer_tx_64`;
CREATE TABLE IF NOT EXISTS `webhook_raw_64_shadow` LIKE `webhook_raw_64`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_64_shadow` LIKE `webhook_raw_rejected_64`;
CREATE TABLE IF NOT EXISTS `channel_token_64_shadow` LIKE `channel_token_64`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_65_shadow` LIKE `acquirer_tx_65`;
CREATE TABLE IF NOT EXISTS `webhook_raw_65_shadow` LIKE `webhook_raw_65`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_65_shadow` LIKE `webhook_raw_rejected_65`;
CREATE TABLE IF NOT EXISTS `channel_token_65_shadow` LIKE `channel_token_65`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_66_shadow` LIKE `acquirer_tx_66`;
CREATE TABLE IF NOT EXISTS `webhook_raw_66_shadow` LIKE `webhook_raw_66`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_66_shadow` LIKE `webhook_raw_rejected_66`;
CREATE TABLE IF NOT EXISTS `channel_token_66_shadow` LIKE `channel_token_66`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_67_shadow` LIKE `acquirer_tx_67`;
CREATE TABLE IF NOT EXISTS `webhook_raw_67_shadow` LIKE `webhook_raw_67`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_67_shadow` LIKE `webhook_raw_rejected_67`;
CREATE TABLE IF NOT EXISTS `channel_token_67_shadow` LIKE `channel_token_67`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_68_shadow` LIKE `acquirer_tx_68`;
CREATE TABLE IF NOT EXISTS `webhook_raw_68_shadow` LIKE `webhook_raw_68`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_68_shadow` LIKE `webhook_raw_rejected_68`;
CREATE TABLE IF NOT EXISTS `channel_token_68_shadow` LIKE `channel_token_68`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_69_shadow` LIKE `acquirer_tx_69`;
CREATE TABLE IF NOT EXISTS `webhook_raw_69_shadow` LIKE `webhook_raw_69`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_69_shadow` LIKE `webhook_raw_rejected_69`;
CREATE TABLE IF NOT EXISTS `channel_token_69_shadow` LIKE `channel_token_69`;

