-- paychan_db_7 的影子表（压测 / shadow 流量）
-- 依赖：7_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `paychan_db_7`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_70_shadow` LIKE `acquirer_tx_70`;
CREATE TABLE IF NOT EXISTS `webhook_raw_70_shadow` LIKE `webhook_raw_70`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_70_shadow` LIKE `webhook_raw_rejected_70`;
CREATE TABLE IF NOT EXISTS `channel_token_70_shadow` LIKE `channel_token_70`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_71_shadow` LIKE `acquirer_tx_71`;
CREATE TABLE IF NOT EXISTS `webhook_raw_71_shadow` LIKE `webhook_raw_71`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_71_shadow` LIKE `webhook_raw_rejected_71`;
CREATE TABLE IF NOT EXISTS `channel_token_71_shadow` LIKE `channel_token_71`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_72_shadow` LIKE `acquirer_tx_72`;
CREATE TABLE IF NOT EXISTS `webhook_raw_72_shadow` LIKE `webhook_raw_72`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_72_shadow` LIKE `webhook_raw_rejected_72`;
CREATE TABLE IF NOT EXISTS `channel_token_72_shadow` LIKE `channel_token_72`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_73_shadow` LIKE `acquirer_tx_73`;
CREATE TABLE IF NOT EXISTS `webhook_raw_73_shadow` LIKE `webhook_raw_73`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_73_shadow` LIKE `webhook_raw_rejected_73`;
CREATE TABLE IF NOT EXISTS `channel_token_73_shadow` LIKE `channel_token_73`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_74_shadow` LIKE `acquirer_tx_74`;
CREATE TABLE IF NOT EXISTS `webhook_raw_74_shadow` LIKE `webhook_raw_74`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_74_shadow` LIKE `webhook_raw_rejected_74`;
CREATE TABLE IF NOT EXISTS `channel_token_74_shadow` LIKE `channel_token_74`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_75_shadow` LIKE `acquirer_tx_75`;
CREATE TABLE IF NOT EXISTS `webhook_raw_75_shadow` LIKE `webhook_raw_75`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_75_shadow` LIKE `webhook_raw_rejected_75`;
CREATE TABLE IF NOT EXISTS `channel_token_75_shadow` LIKE `channel_token_75`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_76_shadow` LIKE `acquirer_tx_76`;
CREATE TABLE IF NOT EXISTS `webhook_raw_76_shadow` LIKE `webhook_raw_76`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_76_shadow` LIKE `webhook_raw_rejected_76`;
CREATE TABLE IF NOT EXISTS `channel_token_76_shadow` LIKE `channel_token_76`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_77_shadow` LIKE `acquirer_tx_77`;
CREATE TABLE IF NOT EXISTS `webhook_raw_77_shadow` LIKE `webhook_raw_77`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_77_shadow` LIKE `webhook_raw_rejected_77`;
CREATE TABLE IF NOT EXISTS `channel_token_77_shadow` LIKE `channel_token_77`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_78_shadow` LIKE `acquirer_tx_78`;
CREATE TABLE IF NOT EXISTS `webhook_raw_78_shadow` LIKE `webhook_raw_78`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_78_shadow` LIKE `webhook_raw_rejected_78`;
CREATE TABLE IF NOT EXISTS `channel_token_78_shadow` LIKE `channel_token_78`;

CREATE TABLE IF NOT EXISTS `acquirer_tx_79_shadow` LIKE `acquirer_tx_79`;
CREATE TABLE IF NOT EXISTS `webhook_raw_79_shadow` LIKE `webhook_raw_79`;
CREATE TABLE IF NOT EXISTS `webhook_raw_rejected_79_shadow` LIKE `webhook_raw_rejected_79`;
CREATE TABLE IF NOT EXISTS `channel_token_79_shadow` LIKE `channel_token_79`;

