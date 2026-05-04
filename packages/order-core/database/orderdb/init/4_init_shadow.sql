-- order_db_4 的影子表（压测 / shadow 流量）
-- 依赖：4_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_4`;

CREATE TABLE IF NOT EXISTS `payment_intent_40_shadow` LIKE `payment_intent_40`;
CREATE TABLE IF NOT EXISTS `charge_40_shadow` LIKE `charge_40`;
CREATE TABLE IF NOT EXISTS `refund_40_shadow` LIKE `refund_40`;
CREATE TABLE IF NOT EXISTS `pay_action_40_shadow` LIKE `pay_action_40`;
CREATE TABLE IF NOT EXISTS `dispute_40_shadow` LIKE `dispute_40`;
CREATE TABLE IF NOT EXISTS `dispute_event_40_shadow` LIKE `dispute_event_40`;
CREATE TABLE IF NOT EXISTS `exception_case_40_shadow` LIKE `exception_case_40`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_40_shadow` LIKE `inbound_webhook_40`;
CREATE TABLE IF NOT EXISTS `notify_log_40_shadow` LIKE `notify_log_40`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_40_shadow` LIKE `accounting_outbox_40`;

CREATE TABLE IF NOT EXISTS `payment_intent_41_shadow` LIKE `payment_intent_41`;
CREATE TABLE IF NOT EXISTS `charge_41_shadow` LIKE `charge_41`;
CREATE TABLE IF NOT EXISTS `refund_41_shadow` LIKE `refund_41`;
CREATE TABLE IF NOT EXISTS `pay_action_41_shadow` LIKE `pay_action_41`;
CREATE TABLE IF NOT EXISTS `dispute_41_shadow` LIKE `dispute_41`;
CREATE TABLE IF NOT EXISTS `dispute_event_41_shadow` LIKE `dispute_event_41`;
CREATE TABLE IF NOT EXISTS `exception_case_41_shadow` LIKE `exception_case_41`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_41_shadow` LIKE `inbound_webhook_41`;
CREATE TABLE IF NOT EXISTS `notify_log_41_shadow` LIKE `notify_log_41`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_41_shadow` LIKE `accounting_outbox_41`;

CREATE TABLE IF NOT EXISTS `payment_intent_42_shadow` LIKE `payment_intent_42`;
CREATE TABLE IF NOT EXISTS `charge_42_shadow` LIKE `charge_42`;
CREATE TABLE IF NOT EXISTS `refund_42_shadow` LIKE `refund_42`;
CREATE TABLE IF NOT EXISTS `pay_action_42_shadow` LIKE `pay_action_42`;
CREATE TABLE IF NOT EXISTS `dispute_42_shadow` LIKE `dispute_42`;
CREATE TABLE IF NOT EXISTS `dispute_event_42_shadow` LIKE `dispute_event_42`;
CREATE TABLE IF NOT EXISTS `exception_case_42_shadow` LIKE `exception_case_42`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_42_shadow` LIKE `inbound_webhook_42`;
CREATE TABLE IF NOT EXISTS `notify_log_42_shadow` LIKE `notify_log_42`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_42_shadow` LIKE `accounting_outbox_42`;

CREATE TABLE IF NOT EXISTS `payment_intent_43_shadow` LIKE `payment_intent_43`;
CREATE TABLE IF NOT EXISTS `charge_43_shadow` LIKE `charge_43`;
CREATE TABLE IF NOT EXISTS `refund_43_shadow` LIKE `refund_43`;
CREATE TABLE IF NOT EXISTS `pay_action_43_shadow` LIKE `pay_action_43`;
CREATE TABLE IF NOT EXISTS `dispute_43_shadow` LIKE `dispute_43`;
CREATE TABLE IF NOT EXISTS `dispute_event_43_shadow` LIKE `dispute_event_43`;
CREATE TABLE IF NOT EXISTS `exception_case_43_shadow` LIKE `exception_case_43`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_43_shadow` LIKE `inbound_webhook_43`;
CREATE TABLE IF NOT EXISTS `notify_log_43_shadow` LIKE `notify_log_43`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_43_shadow` LIKE `accounting_outbox_43`;

CREATE TABLE IF NOT EXISTS `payment_intent_44_shadow` LIKE `payment_intent_44`;
CREATE TABLE IF NOT EXISTS `charge_44_shadow` LIKE `charge_44`;
CREATE TABLE IF NOT EXISTS `refund_44_shadow` LIKE `refund_44`;
CREATE TABLE IF NOT EXISTS `pay_action_44_shadow` LIKE `pay_action_44`;
CREATE TABLE IF NOT EXISTS `dispute_44_shadow` LIKE `dispute_44`;
CREATE TABLE IF NOT EXISTS `dispute_event_44_shadow` LIKE `dispute_event_44`;
CREATE TABLE IF NOT EXISTS `exception_case_44_shadow` LIKE `exception_case_44`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_44_shadow` LIKE `inbound_webhook_44`;
CREATE TABLE IF NOT EXISTS `notify_log_44_shadow` LIKE `notify_log_44`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_44_shadow` LIKE `accounting_outbox_44`;

CREATE TABLE IF NOT EXISTS `payment_intent_45_shadow` LIKE `payment_intent_45`;
CREATE TABLE IF NOT EXISTS `charge_45_shadow` LIKE `charge_45`;
CREATE TABLE IF NOT EXISTS `refund_45_shadow` LIKE `refund_45`;
CREATE TABLE IF NOT EXISTS `pay_action_45_shadow` LIKE `pay_action_45`;
CREATE TABLE IF NOT EXISTS `dispute_45_shadow` LIKE `dispute_45`;
CREATE TABLE IF NOT EXISTS `dispute_event_45_shadow` LIKE `dispute_event_45`;
CREATE TABLE IF NOT EXISTS `exception_case_45_shadow` LIKE `exception_case_45`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_45_shadow` LIKE `inbound_webhook_45`;
CREATE TABLE IF NOT EXISTS `notify_log_45_shadow` LIKE `notify_log_45`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_45_shadow` LIKE `accounting_outbox_45`;

CREATE TABLE IF NOT EXISTS `payment_intent_46_shadow` LIKE `payment_intent_46`;
CREATE TABLE IF NOT EXISTS `charge_46_shadow` LIKE `charge_46`;
CREATE TABLE IF NOT EXISTS `refund_46_shadow` LIKE `refund_46`;
CREATE TABLE IF NOT EXISTS `pay_action_46_shadow` LIKE `pay_action_46`;
CREATE TABLE IF NOT EXISTS `dispute_46_shadow` LIKE `dispute_46`;
CREATE TABLE IF NOT EXISTS `dispute_event_46_shadow` LIKE `dispute_event_46`;
CREATE TABLE IF NOT EXISTS `exception_case_46_shadow` LIKE `exception_case_46`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_46_shadow` LIKE `inbound_webhook_46`;
CREATE TABLE IF NOT EXISTS `notify_log_46_shadow` LIKE `notify_log_46`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_46_shadow` LIKE `accounting_outbox_46`;

CREATE TABLE IF NOT EXISTS `payment_intent_47_shadow` LIKE `payment_intent_47`;
CREATE TABLE IF NOT EXISTS `charge_47_shadow` LIKE `charge_47`;
CREATE TABLE IF NOT EXISTS `refund_47_shadow` LIKE `refund_47`;
CREATE TABLE IF NOT EXISTS `pay_action_47_shadow` LIKE `pay_action_47`;
CREATE TABLE IF NOT EXISTS `dispute_47_shadow` LIKE `dispute_47`;
CREATE TABLE IF NOT EXISTS `dispute_event_47_shadow` LIKE `dispute_event_47`;
CREATE TABLE IF NOT EXISTS `exception_case_47_shadow` LIKE `exception_case_47`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_47_shadow` LIKE `inbound_webhook_47`;
CREATE TABLE IF NOT EXISTS `notify_log_47_shadow` LIKE `notify_log_47`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_47_shadow` LIKE `accounting_outbox_47`;

CREATE TABLE IF NOT EXISTS `payment_intent_48_shadow` LIKE `payment_intent_48`;
CREATE TABLE IF NOT EXISTS `charge_48_shadow` LIKE `charge_48`;
CREATE TABLE IF NOT EXISTS `refund_48_shadow` LIKE `refund_48`;
CREATE TABLE IF NOT EXISTS `pay_action_48_shadow` LIKE `pay_action_48`;
CREATE TABLE IF NOT EXISTS `dispute_48_shadow` LIKE `dispute_48`;
CREATE TABLE IF NOT EXISTS `dispute_event_48_shadow` LIKE `dispute_event_48`;
CREATE TABLE IF NOT EXISTS `exception_case_48_shadow` LIKE `exception_case_48`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_48_shadow` LIKE `inbound_webhook_48`;
CREATE TABLE IF NOT EXISTS `notify_log_48_shadow` LIKE `notify_log_48`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_48_shadow` LIKE `accounting_outbox_48`;

CREATE TABLE IF NOT EXISTS `payment_intent_49_shadow` LIKE `payment_intent_49`;
CREATE TABLE IF NOT EXISTS `charge_49_shadow` LIKE `charge_49`;
CREATE TABLE IF NOT EXISTS `refund_49_shadow` LIKE `refund_49`;
CREATE TABLE IF NOT EXISTS `pay_action_49_shadow` LIKE `pay_action_49`;
CREATE TABLE IF NOT EXISTS `dispute_49_shadow` LIKE `dispute_49`;
CREATE TABLE IF NOT EXISTS `dispute_event_49_shadow` LIKE `dispute_event_49`;
CREATE TABLE IF NOT EXISTS `exception_case_49_shadow` LIKE `exception_case_49`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_49_shadow` LIKE `inbound_webhook_49`;
CREATE TABLE IF NOT EXISTS `notify_log_49_shadow` LIKE `notify_log_49`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_49_shadow` LIKE `accounting_outbox_49`;

