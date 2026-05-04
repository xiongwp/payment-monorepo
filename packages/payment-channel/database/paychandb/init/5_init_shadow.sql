-- paychan_db_5 的影子表（压测 / shadow 流量）
-- 依赖：5_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_5`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_50_shadow` LIKE `acquirer_tx_50`;
CREATE TABLE IF NOT EXISTS `webhook_raw_50_shadow` LIKE `webhook_raw_50`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_50_shadow` LIKE `webhook_raw_rejected_50`;
CREATE TABLE IF NOT EXISTS `channel_token_50_shadow` LIKE `channel_token_50`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_51_shadow` LIKE `acquirer_tx_51`;
CREATE TABLE IF NOT EXISTS `webhook_raw_51_shadow` LIKE `webhook_raw_51`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_51_shadow` LIKE `webhook_raw_rejected_51`;
CREATE TABLE IF NOT EXISTS `channel_token_51_shadow` LIKE `channel_token_51`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_52_shadow` LIKE `acquirer_tx_52`;
CREATE TABLE IF NOT EXISTS `webhook_raw_52_shadow` LIKE `webhook_raw_52`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_52_shadow` LIKE `webhook_raw_rejected_52`;
CREATE TABLE IF NOT EXISTS `channel_token_52_shadow` LIKE `channel_token_52`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_53_shadow` LIKE `acquirer_tx_53`;
CREATE TABLE IF NOT EXISTS `webhook_raw_53_shadow` LIKE `webhook_raw_53`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_53_shadow` LIKE `webhook_raw_rejected_53`;
CREATE TABLE IF NOT EXISTS `channel_token_53_shadow` LIKE `channel_token_53`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_54_shadow` LIKE `acquirer_tx_54`;
CREATE TABLE IF NOT EXISTS `webhook_raw_54_shadow` LIKE `webhook_raw_54`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_54_shadow` LIKE `webhook_raw_rejected_54`;
CREATE TABLE IF NOT EXISTS `channel_token_54_shadow` LIKE `channel_token_54`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_55_shadow` LIKE `acquirer_tx_55`;
CREATE TABLE IF NOT EXISTS `webhook_raw_55_shadow` LIKE `webhook_raw_55`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_55_shadow` LIKE `webhook_raw_rejected_55`;
CREATE TABLE IF NOT EXISTS `channel_token_55_shadow` LIKE `channel_token_55`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_56_shadow` LIKE `acquirer_tx_56`;
CREATE TABLE IF NOT EXISTS `webhook_raw_56_shadow` LIKE `webhook_raw_56`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_56_shadow` LIKE `webhook_raw_rejected_56`;
CREATE TABLE IF NOT EXISTS `channel_token_56_shadow` LIKE `channel_token_56`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_57_shadow` LIKE `acquirer_tx_57`;
CREATE TABLE IF NOT EXISTS `webhook_raw_57_shadow` LIKE `webhook_raw_57`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_57_shadow` LIKE `webhook_raw_rejected_57`;
CREATE TABLE IF NOT EXISTS `channel_token_57_shadow` LIKE `channel_token_57`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_58_shadow` LIKE `acquirer_tx_58`;
CREATE TABLE IF NOT EXISTS `webhook_raw_58_shadow` LIKE `webhook_raw_58`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_58_shadow` LIKE `webhook_raw_rejected_58`;
CREATE TABLE IF NOT EXISTS `channel_token_58_shadow` LIKE `channel_token_58`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_59_shadow` LIKE `acquirer_tx_59`;
CREATE TABLE IF NOT EXISTS `webhook_raw_59_shadow` LIKE `webhook_raw_59`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_59_shadow` LIKE `webhook_raw_rejected_59`;
CREATE TABLE IF NOT EXISTS `channel_token_59_shadow` LIKE `channel_token_59`;

