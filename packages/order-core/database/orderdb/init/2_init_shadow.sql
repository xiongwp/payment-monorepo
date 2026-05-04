-- order_db_2 的影子表（压测 / shadow 流量）
-- 依赖：2_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_2`;

CREATE TABLE IF NOT EXISTS `payment_intent_20_shadow` LIKE `payment_intent_20`;
CREATE TABLE IF NOT EXISTS `charge_20_shadow` LIKE `charge_20`;
CREATE TABLE IF NOT EXISTS `refund_20_shadow` LIKE `refund_20`;
CREATE TABLE IF NOT EXISTS `pay_action_20_shadow` LIKE `pay_action_20`;
CREATE TABLE IF NOT EXISTS `dispute_20_shadow` LIKE `dispute_20`;
CREATE TABLE IF NOT EXISTS `dispute_event_20_shadow` LIKE `dispute_event_20`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_20_shadow` LIKE `inbound_webhook_20`;
CREATE TABLE IF NOT EXISTS `notify_log_20_shadow` LIKE `notify_log_20`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_20_shadow` LIKE `accounting_outbox_20`;

CREATE TABLE IF NOT EXISTS `payment_intent_21_shadow` LIKE `payment_intent_21`;
CREATE TABLE IF NOT EXISTS `charge_21_shadow` LIKE `charge_21`;
CREATE TABLE IF NOT EXISTS `refund_21_shadow` LIKE `refund_21`;
CREATE TABLE IF NOT EXISTS `pay_action_21_shadow` LIKE `pay_action_21`;
CREATE TABLE IF NOT EXISTS `dispute_21_shadow` LIKE `dispute_21`;
CREATE TABLE IF NOT EXISTS `dispute_event_21_shadow` LIKE `dispute_event_21`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_21_shadow` LIKE `inbound_webhook_21`;
CREATE TABLE IF NOT EXISTS `notify_log_21_shadow` LIKE `notify_log_21`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_21_shadow` LIKE `accounting_outbox_21`;

CREATE TABLE IF NOT EXISTS `payment_intent_22_shadow` LIKE `payment_intent_22`;
CREATE TABLE IF NOT EXISTS `charge_22_shadow` LIKE `charge_22`;
CREATE TABLE IF NOT EXISTS `refund_22_shadow` LIKE `refund_22`;
CREATE TABLE IF NOT EXISTS `pay_action_22_shadow` LIKE `pay_action_22`;
CREATE TABLE IF NOT EXISTS `dispute_22_shadow` LIKE `dispute_22`;
CREATE TABLE IF NOT EXISTS `dispute_event_22_shadow` LIKE `dispute_event_22`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_22_shadow` LIKE `inbound_webhook_22`;
CREATE TABLE IF NOT EXISTS `notify_log_22_shadow` LIKE `notify_log_22`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_22_shadow` LIKE `accounting_outbox_22`;

CREATE TABLE IF NOT EXISTS `payment_intent_23_shadow` LIKE `payment_intent_23`;
CREATE TABLE IF NOT EXISTS `charge_23_shadow` LIKE `charge_23`;
CREATE TABLE IF NOT EXISTS `refund_23_shadow` LIKE `refund_23`;
CREATE TABLE IF NOT EXISTS `pay_action_23_shadow` LIKE `pay_action_23`;
CREATE TABLE IF NOT EXISTS `dispute_23_shadow` LIKE `dispute_23`;
CREATE TABLE IF NOT EXISTS `dispute_event_23_shadow` LIKE `dispute_event_23`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_23_shadow` LIKE `inbound_webhook_23`;
CREATE TABLE IF NOT EXISTS `notify_log_23_shadow` LIKE `notify_log_23`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_23_shadow` LIKE `accounting_outbox_23`;

CREATE TABLE IF NOT EXISTS `payment_intent_24_shadow` LIKE `payment_intent_24`;
CREATE TABLE IF NOT EXISTS `charge_24_shadow` LIKE `charge_24`;
CREATE TABLE IF NOT EXISTS `refund_24_shadow` LIKE `refund_24`;
CREATE TABLE IF NOT EXISTS `pay_action_24_shadow` LIKE `pay_action_24`;
CREATE TABLE IF NOT EXISTS `dispute_24_shadow` LIKE `dispute_24`;
CREATE TABLE IF NOT EXISTS `dispute_event_24_shadow` LIKE `dispute_event_24`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_24_shadow` LIKE `inbound_webhook_24`;
CREATE TABLE IF NOT EXISTS `notify_log_24_shadow` LIKE `notify_log_24`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_24_shadow` LIKE `accounting_outbox_24`;

CREATE TABLE IF NOT EXISTS `payment_intent_25_shadow` LIKE `payment_intent_25`;
CREATE TABLE IF NOT EXISTS `charge_25_shadow` LIKE `charge_25`;
CREATE TABLE IF NOT EXISTS `refund_25_shadow` LIKE `refund_25`;
CREATE TABLE IF NOT EXISTS `pay_action_25_shadow` LIKE `pay_action_25`;
CREATE TABLE IF NOT EXISTS `dispute_25_shadow` LIKE `dispute_25`;
CREATE TABLE IF NOT EXISTS `dispute_event_25_shadow` LIKE `dispute_event_25`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_25_shadow` LIKE `inbound_webhook_25`;
CREATE TABLE IF NOT EXISTS `notify_log_25_shadow` LIKE `notify_log_25`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_25_shadow` LIKE `accounting_outbox_25`;

CREATE TABLE IF NOT EXISTS `payment_intent_26_shadow` LIKE `payment_intent_26`;
CREATE TABLE IF NOT EXISTS `charge_26_shadow` LIKE `charge_26`;
CREATE TABLE IF NOT EXISTS `refund_26_shadow` LIKE `refund_26`;
CREATE TABLE IF NOT EXISTS `pay_action_26_shadow` LIKE `pay_action_26`;
CREATE TABLE IF NOT EXISTS `dispute_26_shadow` LIKE `dispute_26`;
CREATE TABLE IF NOT EXISTS `dispute_event_26_shadow` LIKE `dispute_event_26`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_26_shadow` LIKE `inbound_webhook_26`;
CREATE TABLE IF NOT EXISTS `notify_log_26_shadow` LIKE `notify_log_26`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_26_shadow` LIKE `accounting_outbox_26`;

CREATE TABLE IF NOT EXISTS `payment_intent_27_shadow` LIKE `payment_intent_27`;
CREATE TABLE IF NOT EXISTS `charge_27_shadow` LIKE `charge_27`;
CREATE TABLE IF NOT EXISTS `refund_27_shadow` LIKE `refund_27`;
CREATE TABLE IF NOT EXISTS `pay_action_27_shadow` LIKE `pay_action_27`;
CREATE TABLE IF NOT EXISTS `dispute_27_shadow` LIKE `dispute_27`;
CREATE TABLE IF NOT EXISTS `dispute_event_27_shadow` LIKE `dispute_event_27`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_27_shadow` LIKE `inbound_webhook_27`;
CREATE TABLE IF NOT EXISTS `notify_log_27_shadow` LIKE `notify_log_27`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_27_shadow` LIKE `accounting_outbox_27`;

CREATE TABLE IF NOT EXISTS `payment_intent_28_shadow` LIKE `payment_intent_28`;
CREATE TABLE IF NOT EXISTS `charge_28_shadow` LIKE `charge_28`;
CREATE TABLE IF NOT EXISTS `refund_28_shadow` LIKE `refund_28`;
CREATE TABLE IF NOT EXISTS `pay_action_28_shadow` LIKE `pay_action_28`;
CREATE TABLE IF NOT EXISTS `dispute_28_shadow` LIKE `dispute_28`;
CREATE TABLE IF NOT EXISTS `dispute_event_28_shadow` LIKE `dispute_event_28`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_28_shadow` LIKE `inbound_webhook_28`;
CREATE TABLE IF NOT EXISTS `notify_log_28_shadow` LIKE `notify_log_28`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_28_shadow` LIKE `accounting_outbox_28`;

CREATE TABLE IF NOT EXISTS `payment_intent_29_shadow` LIKE `payment_intent_29`;
CREATE TABLE IF NOT EXISTS `charge_29_shadow` LIKE `charge_29`;
CREATE TABLE IF NOT EXISTS `refund_29_shadow` LIKE `refund_29`;
CREATE TABLE IF NOT EXISTS `pay_action_29_shadow` LIKE `pay_action_29`;
CREATE TABLE IF NOT EXISTS `dispute_29_shadow` LIKE `dispute_29`;
CREATE TABLE IF NOT EXISTS `dispute_event_29_shadow` LIKE `dispute_event_29`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_29_shadow` LIKE `inbound_webhook_29`;
CREATE TABLE IF NOT EXISTS `notify_log_29_shadow` LIKE `notify_log_29`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_29_shadow` LIKE `accounting_outbox_29`;

