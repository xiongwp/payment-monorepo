-- paychan_db_0 的影子表（压测 / shadow 流量）
-- 依赖：0_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_0`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_00_shadow` LIKE `acquirer_tx_00`;
CREATE TABLE IF NOT EXISTS `webhook_raw_00_shadow` LIKE `webhook_raw_00`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_00_shadow` LIKE `webhook_raw_rejected_00`;
CREATE TABLE IF NOT EXISTS `channel_token_00_shadow` LIKE `channel_token_00`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_01_shadow` LIKE `acquirer_tx_01`;
CREATE TABLE IF NOT EXISTS `webhook_raw_01_shadow` LIKE `webhook_raw_01`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_01_shadow` LIKE `webhook_raw_rejected_01`;
CREATE TABLE IF NOT EXISTS `channel_token_01_shadow` LIKE `channel_token_01`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_02_shadow` LIKE `acquirer_tx_02`;
CREATE TABLE IF NOT EXISTS `webhook_raw_02_shadow` LIKE `webhook_raw_02`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_02_shadow` LIKE `webhook_raw_rejected_02`;
CREATE TABLE IF NOT EXISTS `channel_token_02_shadow` LIKE `channel_token_02`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_03_shadow` LIKE `acquirer_tx_03`;
CREATE TABLE IF NOT EXISTS `webhook_raw_03_shadow` LIKE `webhook_raw_03`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_03_shadow` LIKE `webhook_raw_rejected_03`;
CREATE TABLE IF NOT EXISTS `channel_token_03_shadow` LIKE `channel_token_03`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_04_shadow` LIKE `acquirer_tx_04`;
CREATE TABLE IF NOT EXISTS `webhook_raw_04_shadow` LIKE `webhook_raw_04`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_04_shadow` LIKE `webhook_raw_rejected_04`;
CREATE TABLE IF NOT EXISTS `channel_token_04_shadow` LIKE `channel_token_04`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_05_shadow` LIKE `acquirer_tx_05`;
CREATE TABLE IF NOT EXISTS `webhook_raw_05_shadow` LIKE `webhook_raw_05`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_05_shadow` LIKE `webhook_raw_rejected_05`;
CREATE TABLE IF NOT EXISTS `channel_token_05_shadow` LIKE `channel_token_05`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_06_shadow` LIKE `acquirer_tx_06`;
CREATE TABLE IF NOT EXISTS `webhook_raw_06_shadow` LIKE `webhook_raw_06`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_06_shadow` LIKE `webhook_raw_rejected_06`;
CREATE TABLE IF NOT EXISTS `channel_token_06_shadow` LIKE `channel_token_06`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_07_shadow` LIKE `acquirer_tx_07`;
CREATE TABLE IF NOT EXISTS `webhook_raw_07_shadow` LIKE `webhook_raw_07`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_07_shadow` LIKE `webhook_raw_rejected_07`;
CREATE TABLE IF NOT EXISTS `channel_token_07_shadow` LIKE `channel_token_07`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_08_shadow` LIKE `acquirer_tx_08`;
CREATE TABLE IF NOT EXISTS `webhook_raw_08_shadow` LIKE `webhook_raw_08`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_08_shadow` LIKE `webhook_raw_rejected_08`;
CREATE TABLE IF NOT EXISTS `channel_token_08_shadow` LIKE `channel_token_08`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_09_shadow` LIKE `acquirer_tx_09`;
CREATE TABLE IF NOT EXISTS `webhook_raw_09_shadow` LIKE `webhook_raw_09`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_09_shadow` LIKE `webhook_raw_rejected_09`;
CREATE TABLE IF NOT EXISTS `channel_token_09_shadow` LIKE `channel_token_09`;

