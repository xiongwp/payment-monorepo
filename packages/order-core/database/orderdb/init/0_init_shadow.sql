-- order_db_0 的影子表（压测 / shadow 流量）
-- 依赖：0_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_0`;

CREATE TABLE IF NOT EXISTS `payment_intent_00_shadow` LIKE `payment_intent_00`;
CREATE TABLE IF NOT EXISTS `charge_00_shadow` LIKE `charge_00`;
CREATE TABLE IF NOT EXISTS `refund_00_shadow` LIKE `refund_00`;
CREATE TABLE IF NOT EXISTS `pay_action_00_shadow` LIKE `pay_action_00`;
CREATE TABLE IF NOT EXISTS `dispute_00_shadow` LIKE `dispute_00`;
CREATE TABLE IF NOT EXISTS `dispute_event_00_shadow` LIKE `dispute_event_00`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_00_shadow` LIKE `inbound_webhook_00`;
CREATE TABLE IF NOT EXISTS `notify_log_00_shadow` LIKE `notify_log_00`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_00_shadow` LIKE `accounting_outbox_00`;

CREATE TABLE IF NOT EXISTS `payment_intent_01_shadow` LIKE `payment_intent_01`;
CREATE TABLE IF NOT EXISTS `charge_01_shadow` LIKE `charge_01`;
CREATE TABLE IF NOT EXISTS `refund_01_shadow` LIKE `refund_01`;
CREATE TABLE IF NOT EXISTS `pay_action_01_shadow` LIKE `pay_action_01`;
CREATE TABLE IF NOT EXISTS `dispute_01_shadow` LIKE `dispute_01`;
CREATE TABLE IF NOT EXISTS `dispute_event_01_shadow` LIKE `dispute_event_01`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_01_shadow` LIKE `inbound_webhook_01`;
CREATE TABLE IF NOT EXISTS `notify_log_01_shadow` LIKE `notify_log_01`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_01_shadow` LIKE `accounting_outbox_01`;

CREATE TABLE IF NOT EXISTS `payment_intent_02_shadow` LIKE `payment_intent_02`;
CREATE TABLE IF NOT EXISTS `charge_02_shadow` LIKE `charge_02`;
CREATE TABLE IF NOT EXISTS `refund_02_shadow` LIKE `refund_02`;
CREATE TABLE IF NOT EXISTS `pay_action_02_shadow` LIKE `pay_action_02`;
CREATE TABLE IF NOT EXISTS `dispute_02_shadow` LIKE `dispute_02`;
CREATE TABLE IF NOT EXISTS `dispute_event_02_shadow` LIKE `dispute_event_02`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_02_shadow` LIKE `inbound_webhook_02`;
CREATE TABLE IF NOT EXISTS `notify_log_02_shadow` LIKE `notify_log_02`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_02_shadow` LIKE `accounting_outbox_02`;

CREATE TABLE IF NOT EXISTS `payment_intent_03_shadow` LIKE `payment_intent_03`;
CREATE TABLE IF NOT EXISTS `charge_03_shadow` LIKE `charge_03`;
CREATE TABLE IF NOT EXISTS `refund_03_shadow` LIKE `refund_03`;
CREATE TABLE IF NOT EXISTS `pay_action_03_shadow` LIKE `pay_action_03`;
CREATE TABLE IF NOT EXISTS `dispute_03_shadow` LIKE `dispute_03`;
CREATE TABLE IF NOT EXISTS `dispute_event_03_shadow` LIKE `dispute_event_03`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_03_shadow` LIKE `inbound_webhook_03`;
CREATE TABLE IF NOT EXISTS `notify_log_03_shadow` LIKE `notify_log_03`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_03_shadow` LIKE `accounting_outbox_03`;

CREATE TABLE IF NOT EXISTS `payment_intent_04_shadow` LIKE `payment_intent_04`;
CREATE TABLE IF NOT EXISTS `charge_04_shadow` LIKE `charge_04`;
CREATE TABLE IF NOT EXISTS `refund_04_shadow` LIKE `refund_04`;
CREATE TABLE IF NOT EXISTS `pay_action_04_shadow` LIKE `pay_action_04`;
CREATE TABLE IF NOT EXISTS `dispute_04_shadow` LIKE `dispute_04`;
CREATE TABLE IF NOT EXISTS `dispute_event_04_shadow` LIKE `dispute_event_04`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_04_shadow` LIKE `inbound_webhook_04`;
CREATE TABLE IF NOT EXISTS `notify_log_04_shadow` LIKE `notify_log_04`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_04_shadow` LIKE `accounting_outbox_04`;

CREATE TABLE IF NOT EXISTS `payment_intent_05_shadow` LIKE `payment_intent_05`;
CREATE TABLE IF NOT EXISTS `charge_05_shadow` LIKE `charge_05`;
CREATE TABLE IF NOT EXISTS `refund_05_shadow` LIKE `refund_05`;
CREATE TABLE IF NOT EXISTS `pay_action_05_shadow` LIKE `pay_action_05`;
CREATE TABLE IF NOT EXISTS `dispute_05_shadow` LIKE `dispute_05`;
CREATE TABLE IF NOT EXISTS `dispute_event_05_shadow` LIKE `dispute_event_05`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_05_shadow` LIKE `inbound_webhook_05`;
CREATE TABLE IF NOT EXISTS `notify_log_05_shadow` LIKE `notify_log_05`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_05_shadow` LIKE `accounting_outbox_05`;

CREATE TABLE IF NOT EXISTS `payment_intent_06_shadow` LIKE `payment_intent_06`;
CREATE TABLE IF NOT EXISTS `charge_06_shadow` LIKE `charge_06`;
CREATE TABLE IF NOT EXISTS `refund_06_shadow` LIKE `refund_06`;
CREATE TABLE IF NOT EXISTS `pay_action_06_shadow` LIKE `pay_action_06`;
CREATE TABLE IF NOT EXISTS `dispute_06_shadow` LIKE `dispute_06`;
CREATE TABLE IF NOT EXISTS `dispute_event_06_shadow` LIKE `dispute_event_06`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_06_shadow` LIKE `inbound_webhook_06`;
CREATE TABLE IF NOT EXISTS `notify_log_06_shadow` LIKE `notify_log_06`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_06_shadow` LIKE `accounting_outbox_06`;

CREATE TABLE IF NOT EXISTS `payment_intent_07_shadow` LIKE `payment_intent_07`;
CREATE TABLE IF NOT EXISTS `charge_07_shadow` LIKE `charge_07`;
CREATE TABLE IF NOT EXISTS `refund_07_shadow` LIKE `refund_07`;
CREATE TABLE IF NOT EXISTS `pay_action_07_shadow` LIKE `pay_action_07`;
CREATE TABLE IF NOT EXISTS `dispute_07_shadow` LIKE `dispute_07`;
CREATE TABLE IF NOT EXISTS `dispute_event_07_shadow` LIKE `dispute_event_07`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_07_shadow` LIKE `inbound_webhook_07`;
CREATE TABLE IF NOT EXISTS `notify_log_07_shadow` LIKE `notify_log_07`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_07_shadow` LIKE `accounting_outbox_07`;

CREATE TABLE IF NOT EXISTS `payment_intent_08_shadow` LIKE `payment_intent_08`;
CREATE TABLE IF NOT EXISTS `charge_08_shadow` LIKE `charge_08`;
CREATE TABLE IF NOT EXISTS `refund_08_shadow` LIKE `refund_08`;
CREATE TABLE IF NOT EXISTS `pay_action_08_shadow` LIKE `pay_action_08`;
CREATE TABLE IF NOT EXISTS `dispute_08_shadow` LIKE `dispute_08`;
CREATE TABLE IF NOT EXISTS `dispute_event_08_shadow` LIKE `dispute_event_08`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_08_shadow` LIKE `inbound_webhook_08`;
CREATE TABLE IF NOT EXISTS `notify_log_08_shadow` LIKE `notify_log_08`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_08_shadow` LIKE `accounting_outbox_08`;

CREATE TABLE IF NOT EXISTS `payment_intent_09_shadow` LIKE `payment_intent_09`;
CREATE TABLE IF NOT EXISTS `charge_09_shadow` LIKE `charge_09`;
CREATE TABLE IF NOT EXISTS `refund_09_shadow` LIKE `refund_09`;
CREATE TABLE IF NOT EXISTS `pay_action_09_shadow` LIKE `pay_action_09`;
CREATE TABLE IF NOT EXISTS `dispute_09_shadow` LIKE `dispute_09`;
CREATE TABLE IF NOT EXISTS `dispute_event_09_shadow` LIKE `dispute_event_09`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_09_shadow` LIKE `inbound_webhook_09`;
CREATE TABLE IF NOT EXISTS `notify_log_09_shadow` LIKE `notify_log_09`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_09_shadow` LIKE `accounting_outbox_09`;

