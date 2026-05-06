-- order_db_3 的影子表（压测 / shadow 流量）
-- 依赖：3_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE `order_db_3`;

CREATE TABLE IF NOT EXISTS `payment_intent_30_shadow` LIKE `payment_intent_30`;
CREATE TABLE IF NOT EXISTS `charge_30_shadow` LIKE `charge_30`;
CREATE TABLE IF NOT EXISTS `refund_30_shadow` LIKE `refund_30`;
CREATE TABLE IF NOT EXISTS `pay_action_30_shadow` LIKE `pay_action_30`;
CREATE TABLE IF NOT EXISTS `dispute_30_shadow` LIKE `dispute_30`;
CREATE TABLE IF NOT EXISTS `dispute_event_30_shadow` LIKE `dispute_event_30`;
CREATE TABLE IF NOT EXISTS `exception_case_30_shadow` LIKE `exception_case_30`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_30_shadow` LIKE `inbound_webhook_30`;
CREATE TABLE IF NOT EXISTS `notify_log_30_shadow` LIKE `notify_log_30`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_30_shadow` LIKE `accounting_outbox_30`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_30_shadow` LIKE `admin_audit_log_30`;

CREATE TABLE IF NOT EXISTS `payment_intent_31_shadow` LIKE `payment_intent_31`;
CREATE TABLE IF NOT EXISTS `charge_31_shadow` LIKE `charge_31`;
CREATE TABLE IF NOT EXISTS `refund_31_shadow` LIKE `refund_31`;
CREATE TABLE IF NOT EXISTS `pay_action_31_shadow` LIKE `pay_action_31`;
CREATE TABLE IF NOT EXISTS `dispute_31_shadow` LIKE `dispute_31`;
CREATE TABLE IF NOT EXISTS `dispute_event_31_shadow` LIKE `dispute_event_31`;
CREATE TABLE IF NOT EXISTS `exception_case_31_shadow` LIKE `exception_case_31`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_31_shadow` LIKE `inbound_webhook_31`;
CREATE TABLE IF NOT EXISTS `notify_log_31_shadow` LIKE `notify_log_31`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_31_shadow` LIKE `accounting_outbox_31`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_31_shadow` LIKE `admin_audit_log_31`;

CREATE TABLE IF NOT EXISTS `payment_intent_32_shadow` LIKE `payment_intent_32`;
CREATE TABLE IF NOT EXISTS `charge_32_shadow` LIKE `charge_32`;
CREATE TABLE IF NOT EXISTS `refund_32_shadow` LIKE `refund_32`;
CREATE TABLE IF NOT EXISTS `pay_action_32_shadow` LIKE `pay_action_32`;
CREATE TABLE IF NOT EXISTS `dispute_32_shadow` LIKE `dispute_32`;
CREATE TABLE IF NOT EXISTS `dispute_event_32_shadow` LIKE `dispute_event_32`;
CREATE TABLE IF NOT EXISTS `exception_case_32_shadow` LIKE `exception_case_32`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_32_shadow` LIKE `inbound_webhook_32`;
CREATE TABLE IF NOT EXISTS `notify_log_32_shadow` LIKE `notify_log_32`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_32_shadow` LIKE `accounting_outbox_32`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_32_shadow` LIKE `admin_audit_log_32`;

CREATE TABLE IF NOT EXISTS `payment_intent_33_shadow` LIKE `payment_intent_33`;
CREATE TABLE IF NOT EXISTS `charge_33_shadow` LIKE `charge_33`;
CREATE TABLE IF NOT EXISTS `refund_33_shadow` LIKE `refund_33`;
CREATE TABLE IF NOT EXISTS `pay_action_33_shadow` LIKE `pay_action_33`;
CREATE TABLE IF NOT EXISTS `dispute_33_shadow` LIKE `dispute_33`;
CREATE TABLE IF NOT EXISTS `dispute_event_33_shadow` LIKE `dispute_event_33`;
CREATE TABLE IF NOT EXISTS `exception_case_33_shadow` LIKE `exception_case_33`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_33_shadow` LIKE `inbound_webhook_33`;
CREATE TABLE IF NOT EXISTS `notify_log_33_shadow` LIKE `notify_log_33`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_33_shadow` LIKE `accounting_outbox_33`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_33_shadow` LIKE `admin_audit_log_33`;

CREATE TABLE IF NOT EXISTS `payment_intent_34_shadow` LIKE `payment_intent_34`;
CREATE TABLE IF NOT EXISTS `charge_34_shadow` LIKE `charge_34`;
CREATE TABLE IF NOT EXISTS `refund_34_shadow` LIKE `refund_34`;
CREATE TABLE IF NOT EXISTS `pay_action_34_shadow` LIKE `pay_action_34`;
CREATE TABLE IF NOT EXISTS `dispute_34_shadow` LIKE `dispute_34`;
CREATE TABLE IF NOT EXISTS `dispute_event_34_shadow` LIKE `dispute_event_34`;
CREATE TABLE IF NOT EXISTS `exception_case_34_shadow` LIKE `exception_case_34`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_34_shadow` LIKE `inbound_webhook_34`;
CREATE TABLE IF NOT EXISTS `notify_log_34_shadow` LIKE `notify_log_34`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_34_shadow` LIKE `accounting_outbox_34`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_34_shadow` LIKE `admin_audit_log_34`;

CREATE TABLE IF NOT EXISTS `payment_intent_35_shadow` LIKE `payment_intent_35`;
CREATE TABLE IF NOT EXISTS `charge_35_shadow` LIKE `charge_35`;
CREATE TABLE IF NOT EXISTS `refund_35_shadow` LIKE `refund_35`;
CREATE TABLE IF NOT EXISTS `pay_action_35_shadow` LIKE `pay_action_35`;
CREATE TABLE IF NOT EXISTS `dispute_35_shadow` LIKE `dispute_35`;
CREATE TABLE IF NOT EXISTS `dispute_event_35_shadow` LIKE `dispute_event_35`;
CREATE TABLE IF NOT EXISTS `exception_case_35_shadow` LIKE `exception_case_35`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_35_shadow` LIKE `inbound_webhook_35`;
CREATE TABLE IF NOT EXISTS `notify_log_35_shadow` LIKE `notify_log_35`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_35_shadow` LIKE `accounting_outbox_35`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_35_shadow` LIKE `admin_audit_log_35`;

CREATE TABLE IF NOT EXISTS `payment_intent_36_shadow` LIKE `payment_intent_36`;
CREATE TABLE IF NOT EXISTS `charge_36_shadow` LIKE `charge_36`;
CREATE TABLE IF NOT EXISTS `refund_36_shadow` LIKE `refund_36`;
CREATE TABLE IF NOT EXISTS `pay_action_36_shadow` LIKE `pay_action_36`;
CREATE TABLE IF NOT EXISTS `dispute_36_shadow` LIKE `dispute_36`;
CREATE TABLE IF NOT EXISTS `dispute_event_36_shadow` LIKE `dispute_event_36`;
CREATE TABLE IF NOT EXISTS `exception_case_36_shadow` LIKE `exception_case_36`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_36_shadow` LIKE `inbound_webhook_36`;
CREATE TABLE IF NOT EXISTS `notify_log_36_shadow` LIKE `notify_log_36`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_36_shadow` LIKE `accounting_outbox_36`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_36_shadow` LIKE `admin_audit_log_36`;

CREATE TABLE IF NOT EXISTS `payment_intent_37_shadow` LIKE `payment_intent_37`;
CREATE TABLE IF NOT EXISTS `charge_37_shadow` LIKE `charge_37`;
CREATE TABLE IF NOT EXISTS `refund_37_shadow` LIKE `refund_37`;
CREATE TABLE IF NOT EXISTS `pay_action_37_shadow` LIKE `pay_action_37`;
CREATE TABLE IF NOT EXISTS `dispute_37_shadow` LIKE `dispute_37`;
CREATE TABLE IF NOT EXISTS `dispute_event_37_shadow` LIKE `dispute_event_37`;
CREATE TABLE IF NOT EXISTS `exception_case_37_shadow` LIKE `exception_case_37`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_37_shadow` LIKE `inbound_webhook_37`;
CREATE TABLE IF NOT EXISTS `notify_log_37_shadow` LIKE `notify_log_37`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_37_shadow` LIKE `accounting_outbox_37`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_37_shadow` LIKE `admin_audit_log_37`;

CREATE TABLE IF NOT EXISTS `payment_intent_38_shadow` LIKE `payment_intent_38`;
CREATE TABLE IF NOT EXISTS `charge_38_shadow` LIKE `charge_38`;
CREATE TABLE IF NOT EXISTS `refund_38_shadow` LIKE `refund_38`;
CREATE TABLE IF NOT EXISTS `pay_action_38_shadow` LIKE `pay_action_38`;
CREATE TABLE IF NOT EXISTS `dispute_38_shadow` LIKE `dispute_38`;
CREATE TABLE IF NOT EXISTS `dispute_event_38_shadow` LIKE `dispute_event_38`;
CREATE TABLE IF NOT EXISTS `exception_case_38_shadow` LIKE `exception_case_38`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_38_shadow` LIKE `inbound_webhook_38`;
CREATE TABLE IF NOT EXISTS `notify_log_38_shadow` LIKE `notify_log_38`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_38_shadow` LIKE `accounting_outbox_38`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_38_shadow` LIKE `admin_audit_log_38`;

CREATE TABLE IF NOT EXISTS `payment_intent_39_shadow` LIKE `payment_intent_39`;
CREATE TABLE IF NOT EXISTS `charge_39_shadow` LIKE `charge_39`;
CREATE TABLE IF NOT EXISTS `refund_39_shadow` LIKE `refund_39`;
CREATE TABLE IF NOT EXISTS `pay_action_39_shadow` LIKE `pay_action_39`;
CREATE TABLE IF NOT EXISTS `dispute_39_shadow` LIKE `dispute_39`;
CREATE TABLE IF NOT EXISTS `dispute_event_39_shadow` LIKE `dispute_event_39`;
CREATE TABLE IF NOT EXISTS `exception_case_39_shadow` LIKE `exception_case_39`;
CREATE TABLE IF NOT EXISTS `inbound_webhook_39_shadow` LIKE `inbound_webhook_39`;
CREATE TABLE IF NOT EXISTS `notify_log_39_shadow` LIKE `notify_log_39`;
CREATE TABLE IF NOT EXISTS `accounting_outbox_39_shadow` LIKE `accounting_outbox_39`;
CREATE TABLE IF NOT EXISTS `admin_audit_log_39_shadow` LIKE `admin_audit_log_39`;

