-- paychan_db_3 的影子表（压测 / shadow 流量）
-- 依赖：3_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_3`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_30_shadow` LIKE `acquirer_tx_30`;
CREATE TABLE IF NOT EXISTS `webhook_raw_30_shadow` LIKE `webhook_raw_30`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_30_shadow` LIKE `webhook_raw_rejected_30`;
CREATE TABLE IF NOT EXISTS `channel_token_30_shadow` LIKE `channel_token_30`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_31_shadow` LIKE `acquirer_tx_31`;
CREATE TABLE IF NOT EXISTS `webhook_raw_31_shadow` LIKE `webhook_raw_31`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_31_shadow` LIKE `webhook_raw_rejected_31`;
CREATE TABLE IF NOT EXISTS `channel_token_31_shadow` LIKE `channel_token_31`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_32_shadow` LIKE `acquirer_tx_32`;
CREATE TABLE IF NOT EXISTS `webhook_raw_32_shadow` LIKE `webhook_raw_32`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_32_shadow` LIKE `webhook_raw_rejected_32`;
CREATE TABLE IF NOT EXISTS `channel_token_32_shadow` LIKE `channel_token_32`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_33_shadow` LIKE `acquirer_tx_33`;
CREATE TABLE IF NOT EXISTS `webhook_raw_33_shadow` LIKE `webhook_raw_33`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_33_shadow` LIKE `webhook_raw_rejected_33`;
CREATE TABLE IF NOT EXISTS `channel_token_33_shadow` LIKE `channel_token_33`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_34_shadow` LIKE `acquirer_tx_34`;
CREATE TABLE IF NOT EXISTS `webhook_raw_34_shadow` LIKE `webhook_raw_34`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_34_shadow` LIKE `webhook_raw_rejected_34`;
CREATE TABLE IF NOT EXISTS `channel_token_34_shadow` LIKE `channel_token_34`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_35_shadow` LIKE `acquirer_tx_35`;
CREATE TABLE IF NOT EXISTS `webhook_raw_35_shadow` LIKE `webhook_raw_35`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_35_shadow` LIKE `webhook_raw_rejected_35`;
CREATE TABLE IF NOT EXISTS `channel_token_35_shadow` LIKE `channel_token_35`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_36_shadow` LIKE `acquirer_tx_36`;
CREATE TABLE IF NOT EXISTS `webhook_raw_36_shadow` LIKE `webhook_raw_36`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_36_shadow` LIKE `webhook_raw_rejected_36`;
CREATE TABLE IF NOT EXISTS `channel_token_36_shadow` LIKE `channel_token_36`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_37_shadow` LIKE `acquirer_tx_37`;
CREATE TABLE IF NOT EXISTS `webhook_raw_37_shadow` LIKE `webhook_raw_37`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_37_shadow` LIKE `webhook_raw_rejected_37`;
CREATE TABLE IF NOT EXISTS `channel_token_37_shadow` LIKE `channel_token_37`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_38_shadow` LIKE `acquirer_tx_38`;
CREATE TABLE IF NOT EXISTS `webhook_raw_38_shadow` LIKE `webhook_raw_38`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_38_shadow` LIKE `webhook_raw_rejected_38`;
CREATE TABLE IF NOT EXISTS `channel_token_38_shadow` LIKE `channel_token_38`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_39_shadow` LIKE `acquirer_tx_39`;
CREATE TABLE IF NOT EXISTS `webhook_raw_39_shadow` LIKE `webhook_raw_39`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_39_shadow` LIKE `webhook_raw_rejected_39`;
CREATE TABLE IF NOT EXISTS `channel_token_39_shadow` LIKE `channel_token_39`;

