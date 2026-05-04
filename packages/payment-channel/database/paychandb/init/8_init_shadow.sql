-- paychan_db_8 的影子表（压测 / shadow 流量）
-- 依赖：8_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_8`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_80_shadow` LIKE `acquirer_tx_80`;
CREATE TABLE IF NOT EXISTS `webhook_raw_80_shadow` LIKE `webhook_raw_80`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_80_shadow` LIKE `webhook_raw_rejected_80`;
CREATE TABLE IF NOT EXISTS `channel_token_80_shadow` LIKE `channel_token_80`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_81_shadow` LIKE `acquirer_tx_81`;
CREATE TABLE IF NOT EXISTS `webhook_raw_81_shadow` LIKE `webhook_raw_81`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_81_shadow` LIKE `webhook_raw_rejected_81`;
CREATE TABLE IF NOT EXISTS `channel_token_81_shadow` LIKE `channel_token_81`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_82_shadow` LIKE `acquirer_tx_82`;
CREATE TABLE IF NOT EXISTS `webhook_raw_82_shadow` LIKE `webhook_raw_82`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_82_shadow` LIKE `webhook_raw_rejected_82`;
CREATE TABLE IF NOT EXISTS `channel_token_82_shadow` LIKE `channel_token_82`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_83_shadow` LIKE `acquirer_tx_83`;
CREATE TABLE IF NOT EXISTS `webhook_raw_83_shadow` LIKE `webhook_raw_83`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_83_shadow` LIKE `webhook_raw_rejected_83`;
CREATE TABLE IF NOT EXISTS `channel_token_83_shadow` LIKE `channel_token_83`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_84_shadow` LIKE `acquirer_tx_84`;
CREATE TABLE IF NOT EXISTS `webhook_raw_84_shadow` LIKE `webhook_raw_84`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_84_shadow` LIKE `webhook_raw_rejected_84`;
CREATE TABLE IF NOT EXISTS `channel_token_84_shadow` LIKE `channel_token_84`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_85_shadow` LIKE `acquirer_tx_85`;
CREATE TABLE IF NOT EXISTS `webhook_raw_85_shadow` LIKE `webhook_raw_85`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_85_shadow` LIKE `webhook_raw_rejected_85`;
CREATE TABLE IF NOT EXISTS `channel_token_85_shadow` LIKE `channel_token_85`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_86_shadow` LIKE `acquirer_tx_86`;
CREATE TABLE IF NOT EXISTS `webhook_raw_86_shadow` LIKE `webhook_raw_86`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_86_shadow` LIKE `webhook_raw_rejected_86`;
CREATE TABLE IF NOT EXISTS `channel_token_86_shadow` LIKE `channel_token_86`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_87_shadow` LIKE `acquirer_tx_87`;
CREATE TABLE IF NOT EXISTS `webhook_raw_87_shadow` LIKE `webhook_raw_87`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_87_shadow` LIKE `webhook_raw_rejected_87`;
CREATE TABLE IF NOT EXISTS `channel_token_87_shadow` LIKE `channel_token_87`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_88_shadow` LIKE `acquirer_tx_88`;
CREATE TABLE IF NOT EXISTS `webhook_raw_88_shadow` LIKE `webhook_raw_88`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_88_shadow` LIKE `webhook_raw_rejected_88`;
CREATE TABLE IF NOT EXISTS `channel_token_88_shadow` LIKE `channel_token_88`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_89_shadow` LIKE `acquirer_tx_89`;
CREATE TABLE IF NOT EXISTS `webhook_raw_89_shadow` LIKE `webhook_raw_89`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_89_shadow` LIKE `webhook_raw_rejected_89`;
CREATE TABLE IF NOT EXISTS `channel_token_89_shadow` LIKE `channel_token_89`;

