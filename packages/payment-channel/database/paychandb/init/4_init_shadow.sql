-- paychan_db_4 的影子表（压测 / shadow 流量）
-- 依赖：4_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_4`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_40_shadow` LIKE `acquirer_tx_40`;
CREATE TABLE IF NOT EXISTS `webhook_raw_40_shadow` LIKE `webhook_raw_40`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_40_shadow` LIKE `webhook_raw_rejected_40`;
CREATE TABLE IF NOT EXISTS `channel_token_40_shadow` LIKE `channel_token_40`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_41_shadow` LIKE `acquirer_tx_41`;
CREATE TABLE IF NOT EXISTS `webhook_raw_41_shadow` LIKE `webhook_raw_41`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_41_shadow` LIKE `webhook_raw_rejected_41`;
CREATE TABLE IF NOT EXISTS `channel_token_41_shadow` LIKE `channel_token_41`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_42_shadow` LIKE `acquirer_tx_42`;
CREATE TABLE IF NOT EXISTS `webhook_raw_42_shadow` LIKE `webhook_raw_42`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_42_shadow` LIKE `webhook_raw_rejected_42`;
CREATE TABLE IF NOT EXISTS `channel_token_42_shadow` LIKE `channel_token_42`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_43_shadow` LIKE `acquirer_tx_43`;
CREATE TABLE IF NOT EXISTS `webhook_raw_43_shadow` LIKE `webhook_raw_43`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_43_shadow` LIKE `webhook_raw_rejected_43`;
CREATE TABLE IF NOT EXISTS `channel_token_43_shadow` LIKE `channel_token_43`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_44_shadow` LIKE `acquirer_tx_44`;
CREATE TABLE IF NOT EXISTS `webhook_raw_44_shadow` LIKE `webhook_raw_44`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_44_shadow` LIKE `webhook_raw_rejected_44`;
CREATE TABLE IF NOT EXISTS `channel_token_44_shadow` LIKE `channel_token_44`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_45_shadow` LIKE `acquirer_tx_45`;
CREATE TABLE IF NOT EXISTS `webhook_raw_45_shadow` LIKE `webhook_raw_45`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_45_shadow` LIKE `webhook_raw_rejected_45`;
CREATE TABLE IF NOT EXISTS `channel_token_45_shadow` LIKE `channel_token_45`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_46_shadow` LIKE `acquirer_tx_46`;
CREATE TABLE IF NOT EXISTS `webhook_raw_46_shadow` LIKE `webhook_raw_46`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_46_shadow` LIKE `webhook_raw_rejected_46`;
CREATE TABLE IF NOT EXISTS `channel_token_46_shadow` LIKE `channel_token_46`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_47_shadow` LIKE `acquirer_tx_47`;
CREATE TABLE IF NOT EXISTS `webhook_raw_47_shadow` LIKE `webhook_raw_47`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_47_shadow` LIKE `webhook_raw_rejected_47`;
CREATE TABLE IF NOT EXISTS `channel_token_47_shadow` LIKE `channel_token_47`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_48_shadow` LIKE `acquirer_tx_48`;
CREATE TABLE IF NOT EXISTS `webhook_raw_48_shadow` LIKE `webhook_raw_48`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_48_shadow` LIKE `webhook_raw_rejected_48`;
CREATE TABLE IF NOT EXISTS `channel_token_48_shadow` LIKE `channel_token_48`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_49_shadow` LIKE `acquirer_tx_49`;
CREATE TABLE IF NOT EXISTS `webhook_raw_49_shadow` LIKE `webhook_raw_49`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_49_shadow` LIKE `webhook_raw_rejected_49`;
CREATE TABLE IF NOT EXISTS `channel_token_49_shadow` LIKE `channel_token_49`;

