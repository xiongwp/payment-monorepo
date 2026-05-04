// Package channel 定义 order-core 对外部下游系统的集成接口。
//
// 真实对接（例如银行 OTP 渠道 / 自研 OTP 发送 / 第三方 3DS acquirer）通过实现
// 这里的接口挂入。service.ProviderRegistry 持有这些实现并由 PayActionService 调用。
package channel

import (
	"context"
	"time"
)

// OTPPurpose 本次 OTP 的业务用途
type OTPPurpose string

const (
	OTPPurposePayment OTPPurpose = "payment"
	OTPPurposeRefund  OTPPurpose = "refund"
	OTPPurposeAuth    OTPPurpose = "auth"
)

// OTPSendRequest 向 OTP 渠道发送一次性验证码的入参
type OTPSendRequest struct {
	PaymentIntentID string     // pi_xxx，用于渠道侧日志与幂等
	CustomerID      string     // 目标用户/商户 ID（渠道内解析手机号/邮箱）
	Recipient       string     // 可选：明确指定的接收方（手机号 / 邮箱）
	Amount          int64      // 交易金额（分），某些渠道会在短信里展示
	Currency        string     // 交易币种
	Locale          string     // zh-CN / en-US，用于模板
	Purpose         OTPPurpose // payment / refund / auth
	Extra           map[string]string
}

// OTPSendResult 渠道返回的结果
type OTPSendResult struct {
	// Code 服务端保留的 OTP 明文，用于 OTPVerifier.Verify 本地比对；
	// 若渠道不允许服务端留存明文，可存储 challenge_id 作为替代，Verify 时走渠道反查。
	Code string
	// RecipientMasked 脱敏后的接收方："+86 138****1234" / "a***@gmail.com"
	RecipientMasked string
	// Channel sms / email / voice
	Channel string
	// ExpiresAt 过期时间；为零值时服务层使用默认 TTL
	ExpiresAt time.Time
}

// OTPSender OTP 发送接口。
//
// 示例实现：
//
//	- BankOTPSender         对接银行卡发卡行 OTP 通道
//	- InHouseOTPSender      对接自研短信 / 邮件 / 语音平台
//	- AliyunSMSOTPSender    对接阿里云短信
type OTPSender interface {
	Send(ctx context.Context, req OTPSendRequest) (*OTPSendResult, error)
}

// OTPVerifier OTP 验证接口（可选，默认由服务层用 Code 做本地比对）。
//
// 当渠道不返回明文 Code、而是用 challenge_id 存根时实现此接口，
// 由渠道侧反查用户输入是否匹配。
type OTPVerifier interface {
	Verify(ctx context.Context, challengeID string, userInput string) (ok bool, reason string, err error)
}
