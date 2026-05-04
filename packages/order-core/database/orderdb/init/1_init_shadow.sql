-- order_db_1 的影子表（压测 / shadow 流量）
-- 依赖：1_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_1`;

CREATE TABLE IF NOT EXISTS `payment_intent_10_shadow` LIKE `payment_intent_10`;
CREATE TABLE IF NOT EXISTS `charge_10_shadow` LIKE `charge_10`;
CREATE TABLE IF NOT EXISTS `refund_10_shadow` LIKE `refund_10`;
CREATE TABLE IF NOT EXISTS `pay_action_10_shadow` LIKE `pay_action_10`;
CREATE TABLE IF NOT EXISTS `dispute_10_shadow` LIKE `dispute_10`;
CREATE TABLE IF NOT EXISTS `dispute_event_10_shadow` LIKE `dispute_event_10`;
CREATE TABLE IF NOT EXISTS `exception_case_10_shadow` LIKE `exception_case_10`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_10_shadow` LIKE `inbound_webhook_10`;
CREATE TABLE IF NOT EXISTS `notify_log_10_shadow` LIKE `notify_log_10`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_10_shadow` LIKE `accounting_outbox_10`;

CREATE TABLE IF NOT EXISTS `payment_intent_11_shadow` LIKE `payment_intent_11`;
CREATE TABLE IF NOT EXISTS `charge_11_shadow` LIKE `charge_11`;
CREATE TABLE IF NOT EXISTS `refund_11_shadow` LIKE `refund_11`;
CREATE TABLE IF NOT EXISTS `pay_action_11_shadow` LIKE `pay_action_11`;
CREATE TABLE IF NOT EXISTS `dispute_11_shadow` LIKE `dispute_11`;
CREATE TABLE IF NOT EXISTS `dispute_event_11_shadow` LIKE `dispute_event_11`;
CREATE TABLE IF NOT EXISTS `exception_case_11_shadow` LIKE `exception_case_11`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_11_shadow` LIKE `inbound_webhook_11`;
CREATE TABLE IF NOT EXISTS `notify_log_11_shadow` LIKE `notify_log_11`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_11_shadow` LIKE `accounting_outbox_11`;

CREATE TABLE IF NOT EXISTS `payment_intent_12_shadow` LIKE `payment_intent_12`;
CREATE TABLE IF NOT EXISTS `charge_12_shadow` LIKE `charge_12`;
CREATE TABLE IF NOT EXISTS `refund_12_shadow` LIKE `refund_12`;
CREATE TABLE IF NOT EXISTS `pay_action_12_shadow` LIKE `pay_action_12`;
CREATE TABLE IF NOT EXISTS `dispute_12_shadow` LIKE `dispute_12`;
CREATE TABLE IF NOT EXISTS `dispute_event_12_shadow` LIKE `dispute_event_12`;
CREATE TABLE IF NOT EXISTS `exception_case_12_shadow` LIKE `exception_case_12`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_12_shadow` LIKE `inbound_webhook_12`;
CREATE TABLE IF NOT EXISTS `notify_log_12_shadow` LIKE `notify_log_12`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_12_shadow` LIKE `accounting_outbox_12`;

CREATE TABLE IF NOT EXISTS `payment_intent_13_shadow` LIKE `payment_intent_13`;
CREATE TABLE IF NOT EXISTS `charge_13_shadow` LIKE `charge_13`;
CREATE TABLE IF NOT EXISTS `refund_13_shadow` LIKE `refund_13`;
CREATE TABLE IF NOT EXISTS `pay_action_13_shadow` LIKE `pay_action_13`;
CREATE TABLE IF NOT EXISTS `dispute_13_shadow` LIKE `dispute_13`;
CREATE TABLE IF NOT EXISTS `dispute_event_13_shadow` LIKE `dispute_event_13`;
CREATE TABLE IF NOT EXISTS `exception_case_13_shadow` LIKE `exception_case_13`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_13_shadow` LIKE `inbound_webhook_13`;
CREATE TABLE IF NOT EXISTS `notify_log_13_shadow` LIKE `notify_log_13`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_13_shadow` LIKE `accounting_outbox_13`;

CREATE TABLE IF NOT EXISTS `payment_intent_14_shadow` LIKE `payment_intent_14`;
CREATE TABLE IF NOT EXISTS `charge_14_shadow` LIKE `charge_14`;
CREATE TABLE IF NOT EXISTS `refund_14_shadow` LIKE `refund_14`;
CREATE TABLE IF NOT EXISTS `pay_action_14_shadow` LIKE `pay_action_14`;
CREATE TABLE IF NOT EXISTS `dispute_14_shadow` LIKE `dispute_14`;
CREATE TABLE IF NOT EXISTS `dispute_event_14_shadow` LIKE `dispute_event_14`;
CREATE TABLE IF NOT EXISTS `exception_case_14_shadow` LIKE `exception_case_14`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_14_shadow` LIKE `inbound_webhook_14`;
CREATE TABLE IF NOT EXISTS `notify_log_14_shadow` LIKE `notify_log_14`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_14_shadow` LIKE `accounting_outbox_14`;

CREATE TABLE IF NOT EXISTS `payment_intent_15_shadow` LIKE `payment_intent_15`;
CREATE TABLE IF NOT EXISTS `charge_15_shadow` LIKE `charge_15`;
CREATE TABLE IF NOT EXISTS `refund_15_shadow` LIKE `refund_15`;
CREATE TABLE IF NOT EXISTS `pay_action_15_shadow` LIKE `pay_action_15`;
CREATE TABLE IF NOT EXISTS `dispute_15_shadow` LIKE `dispute_15`;
CREATE TABLE IF NOT EXISTS `dispute_event_15_shadow` LIKE `dispute_event_15`;
CREATE TABLE IF NOT EXISTS `exception_case_15_shadow` LIKE `exception_case_15`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_15_shadow` LIKE `inbound_webhook_15`;
CREATE TABLE IF NOT EXISTS `notify_log_15_shadow` LIKE `notify_log_15`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_15_shadow` LIKE `accounting_outbox_15`;

CREATE TABLE IF NOT EXISTS `payment_intent_16_shadow` LIKE `payment_intent_16`;
CREATE TABLE IF NOT EXISTS `charge_16_shadow` LIKE `charge_16`;
CREATE TABLE IF NOT EXISTS `refund_16_shadow` LIKE `refund_16`;
CREATE TABLE IF NOT EXISTS `pay_action_16_shadow` LIKE `pay_action_16`;
CREATE TABLE IF NOT EXISTS `dispute_16_shadow` LIKE `dispute_16`;
CREATE TABLE IF NOT EXISTS `dispute_event_16_shadow` LIKE `dispute_event_16`;
CREATE TABLE IF NOT EXISTS `exception_case_16_shadow` LIKE `exception_case_16`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_16_shadow` LIKE `inbound_webhook_16`;
CREATE TABLE IF NOT EXISTS `notify_log_16_shadow` LIKE `notify_log_16`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_16_shadow` LIKE `accounting_outbox_16`;

CREATE TABLE IF NOT EXISTS `payment_intent_17_shadow` LIKE `payment_intent_17`;
CREATE TABLE IF NOT EXISTS `charge_17_shadow` LIKE `charge_17`;
CREATE TABLE IF NOT EXISTS `refund_17_shadow` LIKE `refund_17`;
CREATE TABLE IF NOT EXISTS `pay_action_17_shadow` LIKE `pay_action_17`;
CREATE TABLE IF NOT EXISTS `dispute_17_shadow` LIKE `dispute_17`;
CREATE TABLE IF NOT EXISTS `dispute_event_17_shadow` LIKE `dispute_event_17`;
CREATE TABLE IF NOT EXISTS `exception_case_17_shadow` LIKE `exception_case_17`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_17_shadow` LIKE `inbound_webhook_17`;
CREATE TABLE IF NOT EXISTS `notify_log_17_shadow` LIKE `notify_log_17`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_17_shadow` LIKE `accounting_outbox_17`;

CREATE TABLE IF NOT EXISTS `payment_intent_18_shadow` LIKE `payment_intent_18`;
CREATE TABLE IF NOT EXISTS `charge_18_shadow` LIKE `charge_18`;
CREATE TABLE IF NOT EXISTS `refund_18_shadow` LIKE `refund_18`;
CREATE TABLE IF NOT EXISTS `pay_action_18_shadow` LIKE `pay_action_18`;
CREATE TABLE IF NOT EXISTS `dispute_18_shadow` LIKE `dispute_18`;
CREATE TABLE IF NOT EXISTS `dispute_event_18_shadow` LIKE `dispute_event_18`;
CREATE TABLE IF NOT EXISTS `exception_case_18_shadow` LIKE `exception_case_18`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_18_shadow` LIKE `inbound_webhook_18`;
CREATE TABLE IF NOT EXISTS `notify_log_18_shadow` LIKE `notify_log_18`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_18_shadow` LIKE `accounting_outbox_18`;

CREATE TABLE IF NOT EXISTS `payment_intent_19_shadow` LIKE `payment_intent_19`;
CREATE TABLE IF NOT EXISTS `charge_19_shadow` LIKE `charge_19`;
CREATE TABLE IF NOT EXISTS `refund_19_shadow` LIKE `refund_19`;
CREATE TABLE IF NOT EXISTS `pay_action_19_shadow` LIKE `pay_action_19`;
CREATE TABLE IF NOT EXISTS `dispute_19_shadow` LIKE `dispute_19`;
CREATE TABLE IF NOT EXISTS `dispute_event_19_shadow` LIKE `dispute_event_19`;
CREATE TABLE IF NOT EXISTS `exception_case_19_shadow` LIKE `exception_case_19`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_19_shadow` LIKE `inbound_webhook_19`;
CREATE TABLE IF NOT EXISTS `notify_log_19_shadow` LIKE `notify_log_19`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_19_shadow` LIKE `accounting_outbox_19`;

