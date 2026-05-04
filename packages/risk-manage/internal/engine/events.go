// events.go: 风控事件类型常量 + 分类辅助函数。
//
// 业务侧调 Screen / Report 时填 EventType 字段；引擎用它路由不同处理逻辑：
//   - service.Report 按事件类型决定写哪些图边 / 标签 / 计数器
//   - 规则可按 EventType 过滤（如 login_anomaly 只看 login）
//   - audit / metrics 按事件类型分桶统计
//
// 命名约定：小写 + 点分割（如 "payment.succeeded"）；
// 主类别用单词（"register" / "login"），细分状态用 .suffix。
//
// 收集策略（哪类事件走哪个接口）：
//   - Screen：业务等返回值再决定下一步（register / login / payment.intent_create / withdraw / kyc）
//   - Report：fire-and-forget，事件已发生只更新风控状态（payment.succeeded / fraud / chargeback）
package engine

// ── 账户生命周期 ────────────────────────────────────────────────
const (
	EventRegister        = "register"
	EventRegisterFailed  = "register.failed"
	EventLogin           = "login"
	EventLoginFailed     = "login.failed"
	EventLogout          = "logout"
	EventPasswordChange  = "password_change"
	EventPasswordReset   = "password_reset"
	Event2FAEnable       = "2fa.enable"
	Event2FADisable      = "2fa.disable" // 强信号：账号被盗常见首步
	EventEmailChange     = "email_change"
	EventPhoneChange     = "phone_change"
	EventKYCSubmit       = "kyc.submit"
	EventKYCApproved     = "kyc.approved"
	EventKYCRejected     = "kyc.rejected"
)

// ── 支付 / 资金 ─────────────────────────────────────────────────
const (
	EventPaymentIntent      = "payment.intent_create" // Screen 主入口
	EventPaymentConfirm     = "payment.confirm"
	EventPaymentSucceeded   = "payment.succeeded"
	EventPaymentFailed      = "payment.failed"
	EventPaymentRefund      = "payment.refund"
	EventPaymentRefunded    = "payment.refunded"
	EventPaymentFraud       = "payment.fraud"      // 业务侧主动标记欺诈
	EventPaymentChargeback  = "payment.chargeback" // 收到 chargeback
	EventDisputeOpened      = "dispute.opened"
	EventDisputeLost        = "dispute.lost"

	EventWithdrawIntent     = "withdraw.intent"
	EventWithdrawSucceeded  = "withdraw.succeeded"
	EventWithdrawFailed     = "withdraw.failed"
	EventTopup              = "topup"
	EventTransfer           = "transfer"
	EventBindCard           = "bind_card"
	EventUnbindCard         = "unbind_card"
)

// ── 敏感操作 ────────────────────────────────────────────────────
const (
	EventBindEmail              = "bind_email"
	EventBindPhone              = "bind_phone"
	EventWithdrawAddressChange  = "withdraw_address_change"
	EventAPIKeyCreate           = "api_key.create"
	EventAPIKeyRotate           = "api_key.rotate"
	EventPermissionGrant        = "permission.grant"
	EventCouponRedeem           = "coupon.redeem" // 羊毛党
	EventReferralApply          = "referral.apply"
)

// ── 客户端 / 会话（持续上报；通常走 Report 不 Screen）─────────
const (
	EventSessionStart = "session.start"
	EventSessionEnd   = "session.end"
	EventPageView     = "page_view"
	EventClick        = "click"
	EventFormSubmit   = "form_submit"
)

// ── 分类辅助：让 service.Report 按类别批量处理 ─────────────────

// IsAccountEvent 账户生命周期类（写设备/IP/email/phone 图边）。
func IsAccountEvent(t string) bool {
	switch t {
	case EventRegister, EventLogin, EventPasswordChange, EventPasswordReset,
		Event2FAEnable, Event2FADisable, EventEmailChange, EventPhoneChange,
		EventBindEmail, EventBindPhone, EventBindCard, EventUnbindCard,
		EventKYCSubmit, EventKYCApproved, EventKYCRejected:
		return true
	}
	return false
}

// IsAccountFailedEvent 失败类账户事件（撞库 / 暴力破解信号；只写计数器，不写图边）。
func IsAccountFailedEvent(t string) bool {
	switch t {
	case EventRegisterFailed, EventLoginFailed:
		return true
	}
	return false
}

// IsPaymentSuccessEvent 支付成功类（写 Counter + 全图谱边）。
func IsPaymentSuccessEvent(t string) bool {
	switch t {
	case EventPaymentSucceeded, EventTopup, EventTransfer, EventWithdrawSucceeded:
		return true
	}
	return false
}

// IsFraudSignalEvent fraud / chargeback / dispute lost（图谱节点打 fraud tag）。
func IsFraudSignalEvent(t string) bool {
	switch t {
	case EventPaymentFraud, EventPaymentChargeback, EventDisputeLost:
		return true
	}
	return false
}

// IsSensitiveOpEvent 高风险操作（new_account_high_value / 5min-window 类规则关注）。
func IsSensitiveOpEvent(t string) bool {
	switch t {
	case EventWithdrawIntent, EventWithdrawSucceeded, EventBindCard,
		EventPasswordChange, Event2FADisable, EventWithdrawAddressChange,
		EventEmailChange, EventPhoneChange, EventAPIKeyRotate:
		return true
	}
	return false
}

// FraudTagFor 把 fraud 信号事件映射成 tag 名（写到 LinkStore.Tag）。
func FraudTagFor(t string) string {
	switch t {
	case EventPaymentFraud, EventDisputeLost:
		return "fraud"
	case EventPaymentChargeback:
		return "chargeback"
	}
	return ""
}
