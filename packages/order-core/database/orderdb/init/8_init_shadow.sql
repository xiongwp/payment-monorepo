-- order_db_8 的影子表（压测 / shadow 流量）
-- 依赖：8_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_8`;

CREATE TABLE IF NOT EXISTS `payment_intent_80_shadow` LIKE `payment_intent_80`;
CREATE TABLE IF NOT EXISTS `charge_80_shadow` LIKE `charge_80`;
CREATE TABLE IF NOT EXISTS `refund_80_shadow` LIKE `refund_80`;
CREATE TABLE IF NOT EXISTS `pay_action_80_shadow` LIKE `pay_action_80`;
CREATE TABLE IF NOT EXISTS `dispute_80_shadow` LIKE `dispute_80`;
CREATE TABLE IF NOT EXISTS `dispute_event_80_shadow` LIKE `dispute_event_80`;
CREATE TABLE IF NOT EXISTS `exception_case_80_shadow` LIKE `exception_case_80`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_80_shadow` LIKE `inbound_webhook_80`;
CREATE TABLE IF NOT EXISTS `notify_log_80_shadow` LIKE `notify_log_80`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_80_shadow` LIKE `accounting_outbox_80`;

CREATE TABLE IF NOT EXISTS `payment_intent_81_shadow` LIKE `payment_intent_81`;
CREATE TABLE IF NOT EXISTS `charge_81_shadow` LIKE `charge_81`;
CREATE TABLE IF NOT EXISTS `refund_81_shadow` LIKE `refund_81`;
CREATE TABLE IF NOT EXISTS `pay_action_81_shadow` LIKE `pay_action_81`;
CREATE TABLE IF NOT EXISTS `dispute_81_shadow` LIKE `dispute_81`;
CREATE TABLE IF NOT EXISTS `dispute_event_81_shadow` LIKE `dispute_event_81`;
CREATE TABLE IF NOT EXISTS `exception_case_81_shadow` LIKE `exception_case_81`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_81_shadow` LIKE `inbound_webhook_81`;
CREATE TABLE IF NOT EXISTS `notify_log_81_shadow` LIKE `notify_log_81`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_81_shadow` LIKE `accounting_outbox_81`;

CREATE TABLE IF NOT EXISTS `payment_intent_82_shadow` LIKE `payment_intent_82`;
CREATE TABLE IF NOT EXISTS `charge_82_shadow` LIKE `charge_82`;
CREATE TABLE IF NOT EXISTS `refund_82_shadow` LIKE `refund_82`;
CREATE TABLE IF NOT EXISTS `pay_action_82_shadow` LIKE `pay_action_82`;
CREATE TABLE IF NOT EXISTS `dispute_82_shadow` LIKE `dispute_82`;
CREATE TABLE IF NOT EXISTS `dispute_event_82_shadow` LIKE `dispute_event_82`;
CREATE TABLE IF NOT EXISTS `exception_case_82_shadow` LIKE `exception_case_82`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_82_shadow` LIKE `inbound_webhook_82`;
CREATE TABLE IF NOT EXISTS `notify_log_82_shadow` LIKE `notify_log_82`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_82_shadow` LIKE `accounting_outbox_82`;

CREATE TABLE IF NOT EXISTS `payment_intent_83_shadow` LIKE `payment_intent_83`;
CREATE TABLE IF NOT EXISTS `charge_83_shadow` LIKE `charge_83`;
CREATE TABLE IF NOT EXISTS `refund_83_shadow` LIKE `refund_83`;
CREATE TABLE IF NOT EXISTS `pay_action_83_shadow` LIKE `pay_action_83`;
CREATE TABLE IF NOT EXISTS `dispute_83_shadow` LIKE `dispute_83`;
CREATE TABLE IF NOT EXISTS `dispute_event_83_shadow` LIKE `dispute_event_83`;
CREATE TABLE IF NOT EXISTS `exception_case_83_shadow` LIKE `exception_case_83`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_83_shadow` LIKE `inbound_webhook_83`;
CREATE TABLE IF NOT EXISTS `notify_log_83_shadow` LIKE `notify_log_83`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_83_shadow` LIKE `accounting_outbox_83`;

CREATE TABLE IF NOT EXISTS `payment_intent_84_shadow` LIKE `payment_intent_84`;
CREATE TABLE IF NOT EXISTS `charge_84_shadow` LIKE `charge_84`;
CREATE TABLE IF NOT EXISTS `refund_84_shadow` LIKE `refund_84`;
CREATE TABLE IF NOT EXISTS `pay_action_84_shadow` LIKE `pay_action_84`;
CREATE TABLE IF NOT EXISTS `dispute_84_shadow` LIKE `dispute_84`;
CREATE TABLE IF NOT EXISTS `dispute_event_84_shadow` LIKE `dispute_event_84`;
CREATE TABLE IF NOT EXISTS `exception_case_84_shadow` LIKE `exception_case_84`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_84_shadow` LIKE `inbound_webhook_84`;
CREATE TABLE IF NOT EXISTS `notify_log_84_shadow` LIKE `notify_log_84`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_84_shadow` LIKE `accounting_outbox_84`;

CREATE TABLE IF NOT EXISTS `payment_intent_85_shadow` LIKE `payment_intent_85`;
CREATE TABLE IF NOT EXISTS `charge_85_shadow` LIKE `charge_85`;
CREATE TABLE IF NOT EXISTS `refund_85_shadow` LIKE `refund_85`;
CREATE TABLE IF NOT EXISTS `pay_action_85_shadow` LIKE `pay_action_85`;
CREATE TABLE IF NOT EXISTS `dispute_85_shadow` LIKE `dispute_85`;
CREATE TABLE IF NOT EXISTS `dispute_event_85_shadow` LIKE `dispute_event_85`;
CREATE TABLE IF NOT EXISTS `exception_case_85_shadow` LIKE `exception_case_85`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_85_shadow` LIKE `inbound_webhook_85`;
CREATE TABLE IF NOT EXISTS `notify_log_85_shadow` LIKE `notify_log_85`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_85_shadow` LIKE `accounting_outbox_85`;

CREATE TABLE IF NOT EXISTS `payment_intent_86_shadow` LIKE `payment_intent_86`;
CREATE TABLE IF NOT EXISTS `charge_86_shadow` LIKE `charge_86`;
CREATE TABLE IF NOT EXISTS `refund_86_shadow` LIKE `refund_86`;
CREATE TABLE IF NOT EXISTS `pay_action_86_shadow` LIKE `pay_action_86`;
CREATE TABLE IF NOT EXISTS `dispute_86_shadow` LIKE `dispute_86`;
CREATE TABLE IF NOT EXISTS `dispute_event_86_shadow` LIKE `dispute_event_86`;
CREATE TABLE IF NOT EXISTS `exception_case_86_shadow` LIKE `exception_case_86`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_86_shadow` LIKE `inbound_webhook_86`;
CREATE TABLE IF NOT EXISTS `notify_log_86_shadow` LIKE `notify_log_86`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_86_shadow` LIKE `accounting_outbox_86`;

CREATE TABLE IF NOT EXISTS `payment_intent_87_shadow` LIKE `payment_intent_87`;
CREATE TABLE IF NOT EXISTS `charge_87_shadow` LIKE `charge_87`;
CREATE TABLE IF NOT EXISTS `refund_87_shadow` LIKE `refund_87`;
CREATE TABLE IF NOT EXISTS `pay_action_87_shadow` LIKE `pay_action_87`;
CREATE TABLE IF NOT EXISTS `dispute_87_shadow` LIKE `dispute_87`;
CREATE TABLE IF NOT EXISTS `dispute_event_87_shadow` LIKE `dispute_event_87`;
CREATE TABLE IF NOT EXISTS `exception_case_87_shadow` LIKE `exception_case_87`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_87_shadow` LIKE `inbound_webhook_87`;
CREATE TABLE IF NOT EXISTS `notify_log_87_shadow` LIKE `notify_log_87`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_87_shadow` LIKE `accounting_outbox_87`;

CREATE TABLE IF NOT EXISTS `payment_intent_88_shadow` LIKE `payment_intent_88`;
CREATE TABLE IF NOT EXISTS `charge_88_shadow` LIKE `charge_88`;
CREATE TABLE IF NOT EXISTS `refund_88_shadow` LIKE `refund_88`;
CREATE TABLE IF NOT EXISTS `pay_action_88_shadow` LIKE `pay_action_88`;
CREATE TABLE IF NOT EXISTS `dispute_88_shadow` LIKE `dispute_88`;
CREATE TABLE IF NOT EXISTS `dispute_event_88_shadow` LIKE `dispute_event_88`;
CREATE TABLE IF NOT EXISTS `exception_case_88_shadow` LIKE `exception_case_88`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_88_shadow` LIKE `inbound_webhook_88`;
CREATE TABLE IF NOT EXISTS `notify_log_88_shadow` LIKE `notify_log_88`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_88_shadow` LIKE `accounting_outbox_88`;

CREATE TABLE IF NOT EXISTS `payment_intent_89_shadow` LIKE `payment_intent_89`;
CREATE TABLE IF NOT EXISTS `charge_89_shadow` LIKE `charge_89`;
CREATE TABLE IF NOT EXISTS `refund_89_shadow` LIKE `refund_89`;
CREATE TABLE IF NOT EXISTS `pay_action_89_shadow` LIKE `pay_action_89`;
CREATE TABLE IF NOT EXISTS `dispute_89_shadow` LIKE `dispute_89`;
CREATE TABLE IF NOT EXISTS `dispute_event_89_shadow` LIKE `dispute_event_89`;
CREATE TABLE IF NOT EXISTS `exception_case_89_shadow` LIKE `exception_case_89`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_89_shadow` LIKE `inbound_webhook_89`;
CREATE TABLE IF NOT EXISTS `notify_log_89_shadow` LIKE `notify_log_89`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_89_shadow` LIKE `accounting_outbox_89`;

