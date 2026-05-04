-- order_db_6 的影子表（压测 / shadow 流量）
-- 依赖：6_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_6`;

CREATE TABLE IF NOT EXISTS `payment_intent_60_shadow` LIKE `payment_intent_60`;
CREATE TABLE IF NOT EXISTS `charge_60_shadow` LIKE `charge_60`;
CREATE TABLE IF NOT EXISTS `refund_60_shadow` LIKE `refund_60`;
CREATE TABLE IF NOT EXISTS `pay_action_60_shadow` LIKE `pay_action_60`;
CREATE TABLE IF NOT EXISTS `dispute_60_shadow` LIKE `dispute_60`;
CREATE TABLE IF NOT EXISTS `dispute_event_60_shadow` LIKE `dispute_event_60`;
CREATE TABLE IF NOT EXISTS `exception_case_60_shadow` LIKE `exception_case_60`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_60_shadow` LIKE `inbound_webhook_60`;
CREATE TABLE IF NOT EXISTS `notify_log_60_shadow` LIKE `notify_log_60`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_60_shadow` LIKE `accounting_outbox_60`;

CREATE TABLE IF NOT EXISTS `payment_intent_61_shadow` LIKE `payment_intent_61`;
CREATE TABLE IF NOT EXISTS `charge_61_shadow` LIKE `charge_61`;
CREATE TABLE IF NOT EXISTS `refund_61_shadow` LIKE `refund_61`;
CREATE TABLE IF NOT EXISTS `pay_action_61_shadow` LIKE `pay_action_61`;
CREATE TABLE IF NOT EXISTS `dispute_61_shadow` LIKE `dispute_61`;
CREATE TABLE IF NOT EXISTS `dispute_event_61_shadow` LIKE `dispute_event_61`;
CREATE TABLE IF NOT EXISTS `exception_case_61_shadow` LIKE `exception_case_61`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_61_shadow` LIKE `inbound_webhook_61`;
CREATE TABLE IF NOT EXISTS `notify_log_61_shadow` LIKE `notify_log_61`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_61_shadow` LIKE `accounting_outbox_61`;

CREATE TABLE IF NOT EXISTS `payment_intent_62_shadow` LIKE `payment_intent_62`;
CREATE TABLE IF NOT EXISTS `charge_62_shadow` LIKE `charge_62`;
CREATE TABLE IF NOT EXISTS `refund_62_shadow` LIKE `refund_62`;
CREATE TABLE IF NOT EXISTS `pay_action_62_shadow` LIKE `pay_action_62`;
CREATE TABLE IF NOT EXISTS `dispute_62_shadow` LIKE `dispute_62`;
CREATE TABLE IF NOT EXISTS `dispute_event_62_shadow` LIKE `dispute_event_62`;
CREATE TABLE IF NOT EXISTS `exception_case_62_shadow` LIKE `exception_case_62`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_62_shadow` LIKE `inbound_webhook_62`;
CREATE TABLE IF NOT EXISTS `notify_log_62_shadow` LIKE `notify_log_62`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_62_shadow` LIKE `accounting_outbox_62`;

CREATE TABLE IF NOT EXISTS `payment_intent_63_shadow` LIKE `payment_intent_63`;
CREATE TABLE IF NOT EXISTS `charge_63_shadow` LIKE `charge_63`;
CREATE TABLE IF NOT EXISTS `refund_63_shadow` LIKE `refund_63`;
CREATE TABLE IF NOT EXISTS `pay_action_63_shadow` LIKE `pay_action_63`;
CREATE TABLE IF NOT EXISTS `dispute_63_shadow` LIKE `dispute_63`;
CREATE TABLE IF NOT EXISTS `dispute_event_63_shadow` LIKE `dispute_event_63`;
CREATE TABLE IF NOT EXISTS `exception_case_63_shadow` LIKE `exception_case_63`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_63_shadow` LIKE `inbound_webhook_63`;
CREATE TABLE IF NOT EXISTS `notify_log_63_shadow` LIKE `notify_log_63`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_63_shadow` LIKE `accounting_outbox_63`;

CREATE TABLE IF NOT EXISTS `payment_intent_64_shadow` LIKE `payment_intent_64`;
CREATE TABLE IF NOT EXISTS `charge_64_shadow` LIKE `charge_64`;
CREATE TABLE IF NOT EXISTS `refund_64_shadow` LIKE `refund_64`;
CREATE TABLE IF NOT EXISTS `pay_action_64_shadow` LIKE `pay_action_64`;
CREATE TABLE IF NOT EXISTS `dispute_64_shadow` LIKE `dispute_64`;
CREATE TABLE IF NOT EXISTS `dispute_event_64_shadow` LIKE `dispute_event_64`;
CREATE TABLE IF NOT EXISTS `exception_case_64_shadow` LIKE `exception_case_64`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_64_shadow` LIKE `inbound_webhook_64`;
CREATE TABLE IF NOT EXISTS `notify_log_64_shadow` LIKE `notify_log_64`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_64_shadow` LIKE `accounting_outbox_64`;

CREATE TABLE IF NOT EXISTS `payment_intent_65_shadow` LIKE `payment_intent_65`;
CREATE TABLE IF NOT EXISTS `charge_65_shadow` LIKE `charge_65`;
CREATE TABLE IF NOT EXISTS `refund_65_shadow` LIKE `refund_65`;
CREATE TABLE IF NOT EXISTS `pay_action_65_shadow` LIKE `pay_action_65`;
CREATE TABLE IF NOT EXISTS `dispute_65_shadow` LIKE `dispute_65`;
CREATE TABLE IF NOT EXISTS `dispute_event_65_shadow` LIKE `dispute_event_65`;
CREATE TABLE IF NOT EXISTS `exception_case_65_shadow` LIKE `exception_case_65`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_65_shadow` LIKE `inbound_webhook_65`;
CREATE TABLE IF NOT EXISTS `notify_log_65_shadow` LIKE `notify_log_65`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_65_shadow` LIKE `accounting_outbox_65`;

CREATE TABLE IF NOT EXISTS `payment_intent_66_shadow` LIKE `payment_intent_66`;
CREATE TABLE IF NOT EXISTS `charge_66_shadow` LIKE `charge_66`;
CREATE TABLE IF NOT EXISTS `refund_66_shadow` LIKE `refund_66`;
CREATE TABLE IF NOT EXISTS `pay_action_66_shadow` LIKE `pay_action_66`;
CREATE TABLE IF NOT EXISTS `dispute_66_shadow` LIKE `dispute_66`;
CREATE TABLE IF NOT EXISTS `dispute_event_66_shadow` LIKE `dispute_event_66`;
CREATE TABLE IF NOT EXISTS `exception_case_66_shadow` LIKE `exception_case_66`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_66_shadow` LIKE `inbound_webhook_66`;
CREATE TABLE IF NOT EXISTS `notify_log_66_shadow` LIKE `notify_log_66`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_66_shadow` LIKE `accounting_outbox_66`;

CREATE TABLE IF NOT EXISTS `payment_intent_67_shadow` LIKE `payment_intent_67`;
CREATE TABLE IF NOT EXISTS `charge_67_shadow` LIKE `charge_67`;
CREATE TABLE IF NOT EXISTS `refund_67_shadow` LIKE `refund_67`;
CREATE TABLE IF NOT EXISTS `pay_action_67_shadow` LIKE `pay_action_67`;
CREATE TABLE IF NOT EXISTS `dispute_67_shadow` LIKE `dispute_67`;
CREATE TABLE IF NOT EXISTS `dispute_event_67_shadow` LIKE `dispute_event_67`;
CREATE TABLE IF NOT EXISTS `exception_case_67_shadow` LIKE `exception_case_67`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_67_shadow` LIKE `inbound_webhook_67`;
CREATE TABLE IF NOT EXISTS `notify_log_67_shadow` LIKE `notify_log_67`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_67_shadow` LIKE `accounting_outbox_67`;

CREATE TABLE IF NOT EXISTS `payment_intent_68_shadow` LIKE `payment_intent_68`;
CREATE TABLE IF NOT EXISTS `charge_68_shadow` LIKE `charge_68`;
CREATE TABLE IF NOT EXISTS `refund_68_shadow` LIKE `refund_68`;
CREATE TABLE IF NOT EXISTS `pay_action_68_shadow` LIKE `pay_action_68`;
CREATE TABLE IF NOT EXISTS `dispute_68_shadow` LIKE `dispute_68`;
CREATE TABLE IF NOT EXISTS `dispute_event_68_shadow` LIKE `dispute_event_68`;
CREATE TABLE IF NOT EXISTS `exception_case_68_shadow` LIKE `exception_case_68`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_68_shadow` LIKE `inbound_webhook_68`;
CREATE TABLE IF NOT EXISTS `notify_log_68_shadow` LIKE `notify_log_68`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_68_shadow` LIKE `accounting_outbox_68`;

CREATE TABLE IF NOT EXISTS `payment_intent_69_shadow` LIKE `payment_intent_69`;
CREATE TABLE IF NOT EXISTS `charge_69_shadow` LIKE `charge_69`;
CREATE TABLE IF NOT EXISTS `refund_69_shadow` LIKE `refund_69`;
CREATE TABLE IF NOT EXISTS `pay_action_69_shadow` LIKE `pay_action_69`;
CREATE TABLE IF NOT EXISTS `dispute_69_shadow` LIKE `dispute_69`;
CREATE TABLE IF NOT EXISTS `dispute_event_69_shadow` LIKE `dispute_event_69`;
CREATE TABLE IF NOT EXISTS `exception_case_69_shadow` LIKE `exception_case_69`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_69_shadow` LIKE `inbound_webhook_69`;
CREATE TABLE IF NOT EXISTS `notify_log_69_shadow` LIKE `notify_log_69`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_69_shadow` LIKE `accounting_outbox_69`;

