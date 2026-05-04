-- paychan_db_1 的影子表（压测 / shadow 流量）
-- 依赖：1_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_1`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_10_shadow` LIKE `acquirer_tx_10`;
CREATE TABLE IF NOT EXISTS `webhook_raw_10_shadow` LIKE `webhook_raw_10`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_10_shadow` LIKE `webhook_raw_rejected_10`;
CREATE TABLE IF NOT EXISTS `channel_token_10_shadow` LIKE `channel_token_10`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_11_shadow` LIKE `acquirer_tx_11`;
CREATE TABLE IF NOT EXISTS `webhook_raw_11_shadow` LIKE `webhook_raw_11`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_11_shadow` LIKE `webhook_raw_rejected_11`;
CREATE TABLE IF NOT EXISTS `channel_token_11_shadow` LIKE `channel_token_11`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_12_shadow` LIKE `acquirer_tx_12`;
CREATE TABLE IF NOT EXISTS `webhook_raw_12_shadow` LIKE `webhook_raw_12`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_12_shadow` LIKE `webhook_raw_rejected_12`;
CREATE TABLE IF NOT EXISTS `channel_token_12_shadow` LIKE `channel_token_12`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_13_shadow` LIKE `acquirer_tx_13`;
CREATE TABLE IF NOT EXISTS `webhook_raw_13_shadow` LIKE `webhook_raw_13`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_13_shadow` LIKE `webhook_raw_rejected_13`;
CREATE TABLE IF NOT EXISTS `channel_token_13_shadow` LIKE `channel_token_13`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_14_shadow` LIKE `acquirer_tx_14`;
CREATE TABLE IF NOT EXISTS `webhook_raw_14_shadow` LIKE `webhook_raw_14`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_14_shadow` LIKE `webhook_raw_rejected_14`;
CREATE TABLE IF NOT EXISTS `channel_token_14_shadow` LIKE `channel_token_14`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_15_shadow` LIKE `acquirer_tx_15`;
CREATE TABLE IF NOT EXISTS `webhook_raw_15_shadow` LIKE `webhook_raw_15`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_15_shadow` LIKE `webhook_raw_rejected_15`;
CREATE TABLE IF NOT EXISTS `channel_token_15_shadow` LIKE `channel_token_15`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_16_shadow` LIKE `acquirer_tx_16`;
CREATE TABLE IF NOT EXISTS `webhook_raw_16_shadow` LIKE `webhook_raw_16`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_16_shadow` LIKE `webhook_raw_rejected_16`;
CREATE TABLE IF NOT EXISTS `channel_token_16_shadow` LIKE `channel_token_16`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_17_shadow` LIKE `acquirer_tx_17`;
CREATE TABLE IF NOT EXISTS `webhook_raw_17_shadow` LIKE `webhook_raw_17`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_17_shadow` LIKE `webhook_raw_rejected_17`;
CREATE TABLE IF NOT EXISTS `channel_token_17_shadow` LIKE `channel_token_17`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_18_shadow` LIKE `acquirer_tx_18`;
CREATE TABLE IF NOT EXISTS `webhook_raw_18_shadow` LIKE `webhook_raw_18`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_18_shadow` LIKE `webhook_raw_rejected_18`;
CREATE TABLE IF NOT EXISTS `channel_token_18_shadow` LIKE `channel_token_18`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_19_shadow` LIKE `acquirer_tx_19`;
CREATE TABLE IF NOT EXISTS `webhook_raw_19_shadow` LIKE `webhook_raw_19`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_19_shadow` LIKE `webhook_raw_rejected_19`;
CREATE TABLE IF NOT EXISTS `channel_token_19_shadow` LIKE `channel_token_19`;

