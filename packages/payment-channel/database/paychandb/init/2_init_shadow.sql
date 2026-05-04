-- paychan_db_2 的影子表（压测 / shadow 流量）
-- 依赖：2_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_2`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_20_shadow` LIKE `acquirer_tx_20`;
CREATE TABLE IF NOT EXISTS `webhook_raw_20_shadow` LIKE `webhook_raw_20`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_20_shadow` LIKE `webhook_raw_rejected_20`;
CREATE TABLE IF NOT EXISTS `channel_token_20_shadow` LIKE `channel_token_20`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_21_shadow` LIKE `acquirer_tx_21`;
CREATE TABLE IF NOT EXISTS `webhook_raw_21_shadow` LIKE `webhook_raw_21`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_21_shadow` LIKE `webhook_raw_rejected_21`;
CREATE TABLE IF NOT EXISTS `channel_token_21_shadow` LIKE `channel_token_21`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_22_shadow` LIKE `acquirer_tx_22`;
CREATE TABLE IF NOT EXISTS `webhook_raw_22_shadow` LIKE `webhook_raw_22`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_22_shadow` LIKE `webhook_raw_rejected_22`;
CREATE TABLE IF NOT EXISTS `channel_token_22_shadow` LIKE `channel_token_22`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_23_shadow` LIKE `acquirer_tx_23`;
CREATE TABLE IF NOT EXISTS `webhook_raw_23_shadow` LIKE `webhook_raw_23`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_23_shadow` LIKE `webhook_raw_rejected_23`;
CREATE TABLE IF NOT EXISTS `channel_token_23_shadow` LIKE `channel_token_23`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_24_shadow` LIKE `acquirer_tx_24`;
CREATE TABLE IF NOT EXISTS `webhook_raw_24_shadow` LIKE `webhook_raw_24`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_24_shadow` LIKE `webhook_raw_rejected_24`;
CREATE TABLE IF NOT EXISTS `channel_token_24_shadow` LIKE `channel_token_24`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_25_shadow` LIKE `acquirer_tx_25`;
CREATE TABLE IF NOT EXISTS `webhook_raw_25_shadow` LIKE `webhook_raw_25`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_25_shadow` LIKE `webhook_raw_rejected_25`;
CREATE TABLE IF NOT EXISTS `channel_token_25_shadow` LIKE `channel_token_25`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_26_shadow` LIKE `acquirer_tx_26`;
CREATE TABLE IF NOT EXISTS `webhook_raw_26_shadow` LIKE `webhook_raw_26`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_26_shadow` LIKE `webhook_raw_rejected_26`;
CREATE TABLE IF NOT EXISTS `channel_token_26_shadow` LIKE `channel_token_26`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_27_shadow` LIKE `acquirer_tx_27`;
CREATE TABLE IF NOT EXISTS `webhook_raw_27_shadow` LIKE `webhook_raw_27`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_27_shadow` LIKE `webhook_raw_rejected_27`;
CREATE TABLE IF NOT EXISTS `channel_token_27_shadow` LIKE `channel_token_27`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_28_shadow` LIKE `acquirer_tx_28`;
CREATE TABLE IF NOT EXISTS `webhook_raw_28_shadow` LIKE `webhook_raw_28`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_28_shadow` LIKE `webhook_raw_rejected_28`;
CREATE TABLE IF NOT EXISTS `channel_token_28_shadow` LIKE `channel_token_28`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_29_shadow` LIKE `acquirer_tx_29`;
CREATE TABLE IF NOT EXISTS `webhook_raw_29_shadow` LIKE `webhook_raw_29`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_29_shadow` LIKE `webhook_raw_rejected_29`;
CREATE TABLE IF NOT EXISTS `channel_token_29_shadow` LIKE `channel_token_29`;

