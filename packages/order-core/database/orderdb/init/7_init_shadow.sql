-- order_db_7 的影子表（压测 / shadow 流量）
-- 依赖：7_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_7`;

CREATE TABLE IF NOT EXISTS `payment_intent_70_shadow` LIKE `payment_intent_70`;
CREATE TABLE IF NOT EXISTS `charge_70_shadow` LIKE `charge_70`;
CREATE TABLE IF NOT EXISTS `refund_70_shadow` LIKE `refund_70`;
CREATE TABLE IF NOT EXISTS `pay_action_70_shadow` LIKE `pay_action_70`;
CREATE TABLE IF NOT EXISTS `dispute_70_shadow` LIKE `dispute_70`;
CREATE TABLE IF NOT EXISTS `dispute_event_70_shadow` LIKE `dispute_event_70`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_70_shadow` LIKE `inbound_webhook_70`;
CREATE TABLE IF NOT EXISTS `notify_log_70_shadow` LIKE `notify_log_70`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_70_shadow` LIKE `accounting_outbox_70`;

CREATE TABLE IF NOT EXISTS `payment_intent_71_shadow` LIKE `payment_intent_71`;
CREATE TABLE IF NOT EXISTS `charge_71_shadow` LIKE `charge_71`;
CREATE TABLE IF NOT EXISTS `refund_71_shadow` LIKE `refund_71`;
CREATE TABLE IF NOT EXISTS `pay_action_71_shadow` LIKE `pay_action_71`;
CREATE TABLE IF NOT EXISTS `dispute_71_shadow` LIKE `dispute_71`;
CREATE TABLE IF NOT EXISTS `dispute_event_71_shadow` LIKE `dispute_event_71`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_71_shadow` LIKE `inbound_webhook_71`;
CREATE TABLE IF NOT EXISTS `notify_log_71_shadow` LIKE `notify_log_71`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_71_shadow` LIKE `accounting_outbox_71`;

CREATE TABLE IF NOT EXISTS `payment_intent_72_shadow` LIKE `payment_intent_72`;
CREATE TABLE IF NOT EXISTS `charge_72_shadow` LIKE `charge_72`;
CREATE TABLE IF NOT EXISTS `refund_72_shadow` LIKE `refund_72`;
CREATE TABLE IF NOT EXISTS `pay_action_72_shadow` LIKE `pay_action_72`;
CREATE TABLE IF NOT EXISTS `dispute_72_shadow` LIKE `dispute_72`;
CREATE TABLE IF NOT EXISTS `dispute_event_72_shadow` LIKE `dispute_event_72`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_72_shadow` LIKE `inbound_webhook_72`;
CREATE TABLE IF NOT EXISTS `notify_log_72_shadow` LIKE `notify_log_72`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_72_shadow` LIKE `accounting_outbox_72`;

CREATE TABLE IF NOT EXISTS `payment_intent_73_shadow` LIKE `payment_intent_73`;
CREATE TABLE IF NOT EXISTS `charge_73_shadow` LIKE `charge_73`;
CREATE TABLE IF NOT EXISTS `refund_73_shadow` LIKE `refund_73`;
CREATE TABLE IF NOT EXISTS `pay_action_73_shadow` LIKE `pay_action_73`;
CREATE TABLE IF NOT EXISTS `dispute_73_shadow` LIKE `dispute_73`;
CREATE TABLE IF NOT EXISTS `dispute_event_73_shadow` LIKE `dispute_event_73`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_73_shadow` LIKE `inbound_webhook_73`;
CREATE TABLE IF NOT EXISTS `notify_log_73_shadow` LIKE `notify_log_73`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_73_shadow` LIKE `accounting_outbox_73`;

CREATE TABLE IF NOT EXISTS `payment_intent_74_shadow` LIKE `payment_intent_74`;
CREATE TABLE IF NOT EXISTS `charge_74_shadow` LIKE `charge_74`;
CREATE TABLE IF NOT EXISTS `refund_74_shadow` LIKE `refund_74`;
CREATE TABLE IF NOT EXISTS `pay_action_74_shadow` LIKE `pay_action_74`;
CREATE TABLE IF NOT EXISTS `dispute_74_shadow` LIKE `dispute_74`;
CREATE TABLE IF NOT EXISTS `dispute_event_74_shadow` LIKE `dispute_event_74`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_74_shadow` LIKE `inbound_webhook_74`;
CREATE TABLE IF NOT EXISTS `notify_log_74_shadow` LIKE `notify_log_74`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_74_shadow` LIKE `accounting_outbox_74`;

CREATE TABLE IF NOT EXISTS `payment_intent_75_shadow` LIKE `payment_intent_75`;
CREATE TABLE IF NOT EXISTS `charge_75_shadow` LIKE `charge_75`;
CREATE TABLE IF NOT EXISTS `refund_75_shadow` LIKE `refund_75`;
CREATE TABLE IF NOT EXISTS `pay_action_75_shadow` LIKE `pay_action_75`;
CREATE TABLE IF NOT EXISTS `dispute_75_shadow` LIKE `dispute_75`;
CREATE TABLE IF NOT EXISTS `dispute_event_75_shadow` LIKE `dispute_event_75`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_75_shadow` LIKE `inbound_webhook_75`;
CREATE TABLE IF NOT EXISTS `notify_log_75_shadow` LIKE `notify_log_75`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_75_shadow` LIKE `accounting_outbox_75`;

CREATE TABLE IF NOT EXISTS `payment_intent_76_shadow` LIKE `payment_intent_76`;
CREATE TABLE IF NOT EXISTS `charge_76_shadow` LIKE `charge_76`;
CREATE TABLE IF NOT EXISTS `refund_76_shadow` LIKE `refund_76`;
CREATE TABLE IF NOT EXISTS `pay_action_76_shadow` LIKE `pay_action_76`;
CREATE TABLE IF NOT EXISTS `dispute_76_shadow` LIKE `dispute_76`;
CREATE TABLE IF NOT EXISTS `dispute_event_76_shadow` LIKE `dispute_event_76`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_76_shadow` LIKE `inbound_webhook_76`;
CREATE TABLE IF NOT EXISTS `notify_log_76_shadow` LIKE `notify_log_76`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_76_shadow` LIKE `accounting_outbox_76`;

CREATE TABLE IF NOT EXISTS `payment_intent_77_shadow` LIKE `payment_intent_77`;
CREATE TABLE IF NOT EXISTS `charge_77_shadow` LIKE `charge_77`;
CREATE TABLE IF NOT EXISTS `refund_77_shadow` LIKE `refund_77`;
CREATE TABLE IF NOT EXISTS `pay_action_77_shadow` LIKE `pay_action_77`;
CREATE TABLE IF NOT EXISTS `dispute_77_shadow` LIKE `dispute_77`;
CREATE TABLE IF NOT EXISTS `dispute_event_77_shadow` LIKE `dispute_event_77`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_77_shadow` LIKE `inbound_webhook_77`;
CREATE TABLE IF NOT EXISTS `notify_log_77_shadow` LIKE `notify_log_77`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_77_shadow` LIKE `accounting_outbox_77`;

CREATE TABLE IF NOT EXISTS `payment_intent_78_shadow` LIKE `payment_intent_78`;
CREATE TABLE IF NOT EXISTS `charge_78_shadow` LIKE `charge_78`;
CREATE TABLE IF NOT EXISTS `refund_78_shadow` LIKE `refund_78`;
CREATE TABLE IF NOT EXISTS `pay_action_78_shadow` LIKE `pay_action_78`;
CREATE TABLE IF NOT EXISTS `dispute_78_shadow` LIKE `dispute_78`;
CREATE TABLE IF NOT EXISTS `dispute_event_78_shadow` LIKE `dispute_event_78`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_78_shadow` LIKE `inbound_webhook_78`;
CREATE TABLE IF NOT EXISTS `notify_log_78_shadow` LIKE `notify_log_78`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_78_shadow` LIKE `accounting_outbox_78`;

CREATE TABLE IF NOT EXISTS `payment_intent_79_shadow` LIKE `payment_intent_79`;
CREATE TABLE IF NOT EXISTS `charge_79_shadow` LIKE `charge_79`;
CREATE TABLE IF NOT EXISTS `refund_79_shadow` LIKE `refund_79`;
CREATE TABLE IF NOT EXISTS `pay_action_79_shadow` LIKE `pay_action_79`;
CREATE TABLE IF NOT EXISTS `dispute_79_shadow` LIKE `dispute_79`;
CREATE TABLE IF NOT EXISTS `dispute_event_79_shadow` LIKE `dispute_event_79`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_79_shadow` LIKE `inbound_webhook_79`;
CREATE TABLE IF NOT EXISTS `notify_log_79_shadow` LIKE `notify_log_79`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_79_shadow` LIKE `accounting_outbox_79`;

