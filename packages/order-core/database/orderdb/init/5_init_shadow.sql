-- order_db_5 的影子表（压测 / shadow 流量）
-- 依赖：5_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_5`;

CREATE TABLE IF NOT EXISTS `payment_intent_50_shadow` LIKE `payment_intent_50`;
CREATE TABLE IF NOT EXISTS `charge_50_shadow` LIKE `charge_50`;
CREATE TABLE IF NOT EXISTS `refund_50_shadow` LIKE `refund_50`;
CREATE TABLE IF NOT EXISTS `pay_action_50_shadow` LIKE `pay_action_50`;
CREATE TABLE IF NOT EXISTS `dispute_50_shadow` LIKE `dispute_50`;
CREATE TABLE IF NOT EXISTS `dispute_event_50_shadow` LIKE `dispute_event_50`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_50_shadow` LIKE `inbound_webhook_50`;
CREATE TABLE IF NOT EXISTS `notify_log_50_shadow` LIKE `notify_log_50`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_50_shadow` LIKE `accounting_outbox_50`;

CREATE TABLE IF NOT EXISTS `payment_intent_51_shadow` LIKE `payment_intent_51`;
CREATE TABLE IF NOT EXISTS `charge_51_shadow` LIKE `charge_51`;
CREATE TABLE IF NOT EXISTS `refund_51_shadow` LIKE `refund_51`;
CREATE TABLE IF NOT EXISTS `pay_action_51_shadow` LIKE `pay_action_51`;
CREATE TABLE IF NOT EXISTS `dispute_51_shadow` LIKE `dispute_51`;
CREATE TABLE IF NOT EXISTS `dispute_event_51_shadow` LIKE `dispute_event_51`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_51_shadow` LIKE `inbound_webhook_51`;
CREATE TABLE IF NOT EXISTS `notify_log_51_shadow` LIKE `notify_log_51`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_51_shadow` LIKE `accounting_outbox_51`;

CREATE TABLE IF NOT EXISTS `payment_intent_52_shadow` LIKE `payment_intent_52`;
CREATE TABLE IF NOT EXISTS `charge_52_shadow` LIKE `charge_52`;
CREATE TABLE IF NOT EXISTS `refund_52_shadow` LIKE `refund_52`;
CREATE TABLE IF NOT EXISTS `pay_action_52_shadow` LIKE `pay_action_52`;
CREATE TABLE IF NOT EXISTS `dispute_52_shadow` LIKE `dispute_52`;
CREATE TABLE IF NOT EXISTS `dispute_event_52_shadow` LIKE `dispute_event_52`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_52_shadow` LIKE `inbound_webhook_52`;
CREATE TABLE IF NOT EXISTS `notify_log_52_shadow` LIKE `notify_log_52`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_52_shadow` LIKE `accounting_outbox_52`;

CREATE TABLE IF NOT EXISTS `payment_intent_53_shadow` LIKE `payment_intent_53`;
CREATE TABLE IF NOT EXISTS `charge_53_shadow` LIKE `charge_53`;
CREATE TABLE IF NOT EXISTS `refund_53_shadow` LIKE `refund_53`;
CREATE TABLE IF NOT EXISTS `pay_action_53_shadow` LIKE `pay_action_53`;
CREATE TABLE IF NOT EXISTS `dispute_53_shadow` LIKE `dispute_53`;
CREATE TABLE IF NOT EXISTS `dispute_event_53_shadow` LIKE `dispute_event_53`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_53_shadow` LIKE `inbound_webhook_53`;
CREATE TABLE IF NOT EXISTS `notify_log_53_shadow` LIKE `notify_log_53`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_53_shadow` LIKE `accounting_outbox_53`;

CREATE TABLE IF NOT EXISTS `payment_intent_54_shadow` LIKE `payment_intent_54`;
CREATE TABLE IF NOT EXISTS `charge_54_shadow` LIKE `charge_54`;
CREATE TABLE IF NOT EXISTS `refund_54_shadow` LIKE `refund_54`;
CREATE TABLE IF NOT EXISTS `pay_action_54_shadow` LIKE `pay_action_54`;
CREATE TABLE IF NOT EXISTS `dispute_54_shadow` LIKE `dispute_54`;
CREATE TABLE IF NOT EXISTS `dispute_event_54_shadow` LIKE `dispute_event_54`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_54_shadow` LIKE `inbound_webhook_54`;
CREATE TABLE IF NOT EXISTS `notify_log_54_shadow` LIKE `notify_log_54`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_54_shadow` LIKE `accounting_outbox_54`;

CREATE TABLE IF NOT EXISTS `payment_intent_55_shadow` LIKE `payment_intent_55`;
CREATE TABLE IF NOT EXISTS `charge_55_shadow` LIKE `charge_55`;
CREATE TABLE IF NOT EXISTS `refund_55_shadow` LIKE `refund_55`;
CREATE TABLE IF NOT EXISTS `pay_action_55_shadow` LIKE `pay_action_55`;
CREATE TABLE IF NOT EXISTS `dispute_55_shadow` LIKE `dispute_55`;
CREATE TABLE IF NOT EXISTS `dispute_event_55_shadow` LIKE `dispute_event_55`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_55_shadow` LIKE `inbound_webhook_55`;
CREATE TABLE IF NOT EXISTS `notify_log_55_shadow` LIKE `notify_log_55`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_55_shadow` LIKE `accounting_outbox_55`;

CREATE TABLE IF NOT EXISTS `payment_intent_56_shadow` LIKE `payment_intent_56`;
CREATE TABLE IF NOT EXISTS `charge_56_shadow` LIKE `charge_56`;
CREATE TABLE IF NOT EXISTS `refund_56_shadow` LIKE `refund_56`;
CREATE TABLE IF NOT EXISTS `pay_action_56_shadow` LIKE `pay_action_56`;
CREATE TABLE IF NOT EXISTS `dispute_56_shadow` LIKE `dispute_56`;
CREATE TABLE IF NOT EXISTS `dispute_event_56_shadow` LIKE `dispute_event_56`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_56_shadow` LIKE `inbound_webhook_56`;
CREATE TABLE IF NOT EXISTS `notify_log_56_shadow` LIKE `notify_log_56`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_56_shadow` LIKE `accounting_outbox_56`;

CREATE TABLE IF NOT EXISTS `payment_intent_57_shadow` LIKE `payment_intent_57`;
CREATE TABLE IF NOT EXISTS `charge_57_shadow` LIKE `charge_57`;
CREATE TABLE IF NOT EXISTS `refund_57_shadow` LIKE `refund_57`;
CREATE TABLE IF NOT EXISTS `pay_action_57_shadow` LIKE `pay_action_57`;
CREATE TABLE IF NOT EXISTS `dispute_57_shadow` LIKE `dispute_57`;
CREATE TABLE IF NOT EXISTS `dispute_event_57_shadow` LIKE `dispute_event_57`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_57_shadow` LIKE `inbound_webhook_57`;
CREATE TABLE IF NOT EXISTS `notify_log_57_shadow` LIKE `notify_log_57`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_57_shadow` LIKE `accounting_outbox_57`;

CREATE TABLE IF NOT EXISTS `payment_intent_58_shadow` LIKE `payment_intent_58`;
CREATE TABLE IF NOT EXISTS `charge_58_shadow` LIKE `charge_58`;
CREATE TABLE IF NOT EXISTS `refund_58_shadow` LIKE `refund_58`;
CREATE TABLE IF NOT EXISTS `pay_action_58_shadow` LIKE `pay_action_58`;
CREATE TABLE IF NOT EXISTS `dispute_58_shadow` LIKE `dispute_58`;
CREATE TABLE IF NOT EXISTS `dispute_event_58_shadow` LIKE `dispute_event_58`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_58_shadow` LIKE `inbound_webhook_58`;
CREATE TABLE IF NOT EXISTS `notify_log_58_shadow` LIKE `notify_log_58`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_58_shadow` LIKE `accounting_outbox_58`;

CREATE TABLE IF NOT EXISTS `payment_intent_59_shadow` LIKE `payment_intent_59`;
CREATE TABLE IF NOT EXISTS `charge_59_shadow` LIKE `charge_59`;
CREATE TABLE IF NOT EXISTS `refund_59_shadow` LIKE `refund_59`;
CREATE TABLE IF NOT EXISTS `pay_action_59_shadow` LIKE `pay_action_59`;
CREATE TABLE IF NOT EXISTS `dispute_59_shadow` LIKE `dispute_59`;
CREATE TABLE IF NOT EXISTS `dispute_event_59_shadow` LIKE `dispute_event_59`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_59_shadow` LIKE `inbound_webhook_59`;
CREATE TABLE IF NOT EXISTS `notify_log_59_shadow` LIKE `notify_log_59`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_59_shadow` LIKE `accounting_outbox_59`;

