-- order_db_9 的影子表（压测 / shadow 流量）
-- 依赖：9_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_9`;

CREATE TABLE IF NOT EXISTS `payment_intent_90_shadow` LIKE `payment_intent_90`;
CREATE TABLE IF NOT EXISTS `charge_90_shadow` LIKE `charge_90`;
CREATE TABLE IF NOT EXISTS `refund_90_shadow` LIKE `refund_90`;
CREATE TABLE IF NOT EXISTS `pay_action_90_shadow` LIKE `pay_action_90`;
CREATE TABLE IF NOT EXISTS `dispute_90_shadow` LIKE `dispute_90`;
CREATE TABLE IF NOT EXISTS `dispute_event_90_shadow` LIKE `dispute_event_90`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_90_shadow` LIKE `inbound_webhook_90`;
CREATE TABLE IF NOT EXISTS `notify_log_90_shadow` LIKE `notify_log_90`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_90_shadow` LIKE `accounting_outbox_90`;

CREATE TABLE IF NOT EXISTS `payment_intent_91_shadow` LIKE `payment_intent_91`;
CREATE TABLE IF NOT EXISTS `charge_91_shadow` LIKE `charge_91`;
CREATE TABLE IF NOT EXISTS `refund_91_shadow` LIKE `refund_91`;
CREATE TABLE IF NOT EXISTS `pay_action_91_shadow` LIKE `pay_action_91`;
CREATE TABLE IF NOT EXISTS `dispute_91_shadow` LIKE `dispute_91`;
CREATE TABLE IF NOT EXISTS `dispute_event_91_shadow` LIKE `dispute_event_91`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_91_shadow` LIKE `inbound_webhook_91`;
CREATE TABLE IF NOT EXISTS `notify_log_91_shadow` LIKE `notify_log_91`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_91_shadow` LIKE `accounting_outbox_91`;

CREATE TABLE IF NOT EXISTS `payment_intent_92_shadow` LIKE `payment_intent_92`;
CREATE TABLE IF NOT EXISTS `charge_92_shadow` LIKE `charge_92`;
CREATE TABLE IF NOT EXISTS `refund_92_shadow` LIKE `refund_92`;
CREATE TABLE IF NOT EXISTS `pay_action_92_shadow` LIKE `pay_action_92`;
CREATE TABLE IF NOT EXISTS `dispute_92_shadow` LIKE `dispute_92`;
CREATE TABLE IF NOT EXISTS `dispute_event_92_shadow` LIKE `dispute_event_92`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_92_shadow` LIKE `inbound_webhook_92`;
CREATE TABLE IF NOT EXISTS `notify_log_92_shadow` LIKE `notify_log_92`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_92_shadow` LIKE `accounting_outbox_92`;

CREATE TABLE IF NOT EXISTS `payment_intent_93_shadow` LIKE `payment_intent_93`;
CREATE TABLE IF NOT EXISTS `charge_93_shadow` LIKE `charge_93`;
CREATE TABLE IF NOT EXISTS `refund_93_shadow` LIKE `refund_93`;
CREATE TABLE IF NOT EXISTS `pay_action_93_shadow` LIKE `pay_action_93`;
CREATE TABLE IF NOT EXISTS `dispute_93_shadow` LIKE `dispute_93`;
CREATE TABLE IF NOT EXISTS `dispute_event_93_shadow` LIKE `dispute_event_93`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_93_shadow` LIKE `inbound_webhook_93`;
CREATE TABLE IF NOT EXISTS `notify_log_93_shadow` LIKE `notify_log_93`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_93_shadow` LIKE `accounting_outbox_93`;

CREATE TABLE IF NOT EXISTS `payment_intent_94_shadow` LIKE `payment_intent_94`;
CREATE TABLE IF NOT EXISTS `charge_94_shadow` LIKE `charge_94`;
CREATE TABLE IF NOT EXISTS `refund_94_shadow` LIKE `refund_94`;
CREATE TABLE IF NOT EXISTS `pay_action_94_shadow` LIKE `pay_action_94`;
CREATE TABLE IF NOT EXISTS `dispute_94_shadow` LIKE `dispute_94`;
CREATE TABLE IF NOT EXISTS `dispute_event_94_shadow` LIKE `dispute_event_94`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_94_shadow` LIKE `inbound_webhook_94`;
CREATE TABLE IF NOT EXISTS `notify_log_94_shadow` LIKE `notify_log_94`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_94_shadow` LIKE `accounting_outbox_94`;

CREATE TABLE IF NOT EXISTS `payment_intent_95_shadow` LIKE `payment_intent_95`;
CREATE TABLE IF NOT EXISTS `charge_95_shadow` LIKE `charge_95`;
CREATE TABLE IF NOT EXISTS `refund_95_shadow` LIKE `refund_95`;
CREATE TABLE IF NOT EXISTS `pay_action_95_shadow` LIKE `pay_action_95`;
CREATE TABLE IF NOT EXISTS `dispute_95_shadow` LIKE `dispute_95`;
CREATE TABLE IF NOT EXISTS `dispute_event_95_shadow` LIKE `dispute_event_95`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_95_shadow` LIKE `inbound_webhook_95`;
CREATE TABLE IF NOT EXISTS `notify_log_95_shadow` LIKE `notify_log_95`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_95_shadow` LIKE `accounting_outbox_95`;

CREATE TABLE IF NOT EXISTS `payment_intent_96_shadow` LIKE `payment_intent_96`;
CREATE TABLE IF NOT EXISTS `charge_96_shadow` LIKE `charge_96`;
CREATE TABLE IF NOT EXISTS `refund_96_shadow` LIKE `refund_96`;
CREATE TABLE IF NOT EXISTS `pay_action_96_shadow` LIKE `pay_action_96`;
CREATE TABLE IF NOT EXISTS `dispute_96_shadow` LIKE `dispute_96`;
CREATE TABLE IF NOT EXISTS `dispute_event_96_shadow` LIKE `dispute_event_96`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_96_shadow` LIKE `inbound_webhook_96`;
CREATE TABLE IF NOT EXISTS `notify_log_96_shadow` LIKE `notify_log_96`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_96_shadow` LIKE `accounting_outbox_96`;

CREATE TABLE IF NOT EXISTS `payment_intent_97_shadow` LIKE `payment_intent_97`;
CREATE TABLE IF NOT EXISTS `charge_97_shadow` LIKE `charge_97`;
CREATE TABLE IF NOT EXISTS `refund_97_shadow` LIKE `refund_97`;
CREATE TABLE IF NOT EXISTS `pay_action_97_shadow` LIKE `pay_action_97`;
CREATE TABLE IF NOT EXISTS `dispute_97_shadow` LIKE `dispute_97`;
CREATE TABLE IF NOT EXISTS `dispute_event_97_shadow` LIKE `dispute_event_97`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_97_shadow` LIKE `inbound_webhook_97`;
CREATE TABLE IF NOT EXISTS `notify_log_97_shadow` LIKE `notify_log_97`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_97_shadow` LIKE `accounting_outbox_97`;

CREATE TABLE IF NOT EXISTS `payment_intent_98_shadow` LIKE `payment_intent_98`;
CREATE TABLE IF NOT EXISTS `charge_98_shadow` LIKE `charge_98`;
CREATE TABLE IF NOT EXISTS `refund_98_shadow` LIKE `refund_98`;
CREATE TABLE IF NOT EXISTS `pay_action_98_shadow` LIKE `pay_action_98`;
CREATE TABLE IF NOT EXISTS `dispute_98_shadow` LIKE `dispute_98`;
CREATE TABLE IF NOT EXISTS `dispute_event_98_shadow` LIKE `dispute_event_98`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_98_shadow` LIKE `inbound_webhook_98`;
CREATE TABLE IF NOT EXISTS `notify_log_98_shadow` LIKE `notify_log_98`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_98_shadow` LIKE `accounting_outbox_98`;

CREATE TABLE IF NOT EXISTS `payment_intent_99_shadow` LIKE `payment_intent_99`;
CREATE TABLE IF NOT EXISTS `charge_99_shadow` LIKE `charge_99`;
CREATE TABLE IF NOT EXISTS `refund_99_shadow` LIKE `refund_99`;
CREATE TABLE IF NOT EXISTS `pay_action_99_shadow` LIKE `pay_action_99`;
CREATE TABLE IF NOT EXISTS `dispute_99_shadow` LIKE `dispute_99`;
CREATE TABLE IF NOT EXISTS `dispute_event_99_shadow` LIKE `dispute_event_99`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_99_shadow` LIKE `inbound_webhook_99`;
CREATE TABLE IF NOT EXISTS `notify_log_99_shadow` LIKE `notify_log_99`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_99_shadow` LIKE `accounting_outbox_99`;

