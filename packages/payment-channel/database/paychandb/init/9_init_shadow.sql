-- paychan_db_9 的影子表（压测 / shadow 流量）
-- 依赖：9_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_9`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_90_shadow` LIKE `acquirer_tx_90`;
CREATE TABLE IF NOT EXISTS `webhook_raw_90_shadow` LIKE `webhook_raw_90`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_90_shadow` LIKE `webhook_raw_rejected_90`;
CREATE TABLE IF NOT EXISTS `channel_token_90_shadow` LIKE `channel_token_90`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_91_shadow` LIKE `acquirer_tx_91`;
CREATE TABLE IF NOT EXISTS `webhook_raw_91_shadow` LIKE `webhook_raw_91`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_91_shadow` LIKE `webhook_raw_rejected_91`;
CREATE TABLE IF NOT EXISTS `channel_token_91_shadow` LIKE `channel_token_91`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_92_shadow` LIKE `acquirer_tx_92`;
CREATE TABLE IF NOT EXISTS `webhook_raw_92_shadow` LIKE `webhook_raw_92`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_92_shadow` LIKE `webhook_raw_rejected_92`;
CREATE TABLE IF NOT EXISTS `channel_token_92_shadow` LIKE `channel_token_92`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_93_shadow` LIKE `acquirer_tx_93`;
CREATE TABLE IF NOT EXISTS `webhook_raw_93_shadow` LIKE `webhook_raw_93`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_93_shadow` LIKE `webhook_raw_rejected_93`;
CREATE TABLE IF NOT EXISTS `channel_token_93_shadow` LIKE `channel_token_93`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_94_shadow` LIKE `acquirer_tx_94`;
CREATE TABLE IF NOT EXISTS `webhook_raw_94_shadow` LIKE `webhook_raw_94`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_94_shadow` LIKE `webhook_raw_rejected_94`;
CREATE TABLE IF NOT EXISTS `channel_token_94_shadow` LIKE `channel_token_94`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_95_shadow` LIKE `acquirer_tx_95`;
CREATE TABLE IF NOT EXISTS `webhook_raw_95_shadow` LIKE `webhook_raw_95`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_95_shadow` LIKE `webhook_raw_rejected_95`;
CREATE TABLE IF NOT EXISTS `channel_token_95_shadow` LIKE `channel_token_95`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_96_shadow` LIKE `acquirer_tx_96`;
CREATE TABLE IF NOT EXISTS `webhook_raw_96_shadow` LIKE `webhook_raw_96`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_96_shadow` LIKE `webhook_raw_rejected_96`;
CREATE TABLE IF NOT EXISTS `channel_token_96_shadow` LIKE `channel_token_96`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_97_shadow` LIKE `acquirer_tx_97`;
CREATE TABLE IF NOT EXISTS `webhook_raw_97_shadow` LIKE `webhook_raw_97`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_97_shadow` LIKE `webhook_raw_rejected_97`;
CREATE TABLE IF NOT EXISTS `channel_token_97_shadow` LIKE `channel_token_97`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_98_shadow` LIKE `acquirer_tx_98`;
CREATE TABLE IF NOT EXISTS `webhook_raw_98_shadow` LIKE `webhook_raw_98`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_98_shadow` LIKE `webhook_raw_rejected_98`;
CREATE TABLE IF NOT EXISTS `channel_token_98_shadow` LIKE `channel_token_98`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_99_shadow` LIKE `acquirer_tx_99`;
CREATE TABLE IF NOT EXISTS `webhook_raw_99_shadow` LIKE `webhook_raw_99`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_99_shadow` LIKE `webhook_raw_rejected_99`;
CREATE TABLE IF NOT EXISTS `channel_token_99_shadow` LIKE `channel_token_99`;

