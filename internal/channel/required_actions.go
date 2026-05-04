package channel

import (
	"strconv"
	"time"
)

// 本文件给每种 RequiredAction 定义强类型结构与构造函数。
// 渠道 adapter 用 NewXxxRequiredAction(...) 直接构造 RequiredAction，
// 不必手动拼 Details map，减少错误字段名与拼写。
//
// 服务层读取 RequiredAction 时可用对应的 ParseXxx(action) 反解回强类型结构。

// ─── 3D Secure ────────────────────────────────────────────────────────────────

// ThreeDSDetails 3DS 挑战的强类型 payload
type ThreeDSDetails struct {
	RedirectURL string // 前端跳转的 ACS URL（必填）
	SessionID   string // 渠道留给服务端反查的 session id（必填）
	ReturnURL   string // 挑战完成后 ACS 把用户跳回的地址
	// 附加渠道特定字段（如 ds_trans_id, version）
	Extra map[string]string
}

// NewThreeDSRequiredAction 构造 3DS RequiredAction
func NewThreeDSRequiredAction(d ThreeDSDetails, expiresAt time.Time) *RequiredAction {
	details := map[string]string{
		"redirect_url": d.RedirectURL,
		"session_id":   d.SessionID,
		"return_url":   d.ReturnURL,
	}
	for k, v := range d.Extra {
		details[k] = v
	}
	return &RequiredAction{
		Type:         RequiredAction3DSRedirect,
		Details:      details,
		ChallengeRef: d.SessionID,
		ExpiresAt:    expiresAt,
	}
}

// ParseThreeDS 从 RequiredAction 读回 ThreeDSDetails
func ParseThreeDS(a *RequiredAction) ThreeDSDetails {
	if a == nil {
		return ThreeDSDetails{}
	}
	return ThreeDSDetails{
		RedirectURL: a.Details["redirect_url"],
		SessionID:   a.Details["session_id"],
		ReturnURL:   a.Details["return_url"],
		Extra:       copyMapExcept(a.Details, "redirect_url", "session_id", "return_url"),
	}
}

// ─── OTP ──────────────────────────────────────────────────────────────────────

// OTPDetails OTP 挑战的强类型 payload
type OTPDetails struct {
	// RecipientMasked 脱敏后的接收方："+86 138****1234" / "a***@gmail.com"
	RecipientMasked string
	// Channel sms / email / voice
	Channel string
	// ChallengeID 渠道分配的挑战 ID（若渠道不返回明文 Code 则必填）
	ChallengeID string
	// Length 期望的 OTP 长度（前端做输入校验）
	Length int
	// ResendAfterSec 前端"重发"按钮冷却秒数
	ResendAfterSec int
	Extra          map[string]string
}

// NewOTPRequiredAction 构造 OTP RequiredAction。
// code 为空时把 challenge_ref 设为 d.ChallengeID（走 OTPVerifier 反查路径）；
// code 非空时服务端直接留存用于本地比对。
func NewOTPRequiredAction(d OTPDetails, code string, expiresAt time.Time) *RequiredAction {
	details := map[string]string{
		"recipient_masked": d.RecipientMasked,
		"channel":          d.Channel,
	}
	if d.Length > 0 {
		details["length"] = strconv.Itoa(d.Length)
	}
	if d.ResendAfterSec > 0 {
		details["resend_after_sec"] = strconv.Itoa(d.ResendAfterSec)
	}
	for k, v := range d.Extra {
		details[k] = v
	}
	ref := code
	if ref == "" {
		ref = d.ChallengeID
	}
	return &RequiredAction{
		Type:         RequiredActionOTP,
		Details:      details,
		ChallengeRef: ref,
		ExpiresAt:    expiresAt,
	}
}

// ParseOTP 从 RequiredAction 读回 OTPDetails
func ParseOTP(a *RequiredAction) OTPDetails {
	if a == nil {
		return OTPDetails{}
	}
	length, _ := strconv.Atoi(a.Details["length"])
	resend, _ := strconv.Atoi(a.Details["resend_after_sec"])
	return OTPDetails{
		RecipientMasked: a.Details["recipient_masked"],
		Channel:         a.Details["channel"],
		ChallengeID:     a.ChallengeRef,
		Length:          length,
		ResendAfterSec:  resend,
		Extra:           copyMapExcept(a.Details, "recipient_masked", "channel", "length", "resend_after_sec"),
	}
}

// ─── Pay Password ─────────────────────────────────────────────────────────────

// PayPasswordDetails 支付密码挑战的强类型 payload
type PayPasswordDetails struct {
	// Algorithm 哈希算法："sha256" / "bcrypt" / ...（前端可据此决定是否先哈希）
	Algorithm string
	// SaltHint 算法需要盐时返回的盐提示（非敏感，仅用于派生相同摘要）
	SaltHint string
	// MaxAttempts 给前端提示的最大尝试次数
	MaxAttempts int
	Extra       map[string]string
}

// NewPayPasswordRequiredAction 构造支付密码 RequiredAction。
// expectedHash 为服务端期望的密码哈希，存入 ChallengeRef 供 Verify 比对。
func NewPayPasswordRequiredAction(d PayPasswordDetails, expectedHash string, expiresAt time.Time) *RequiredAction {
	details := map[string]string{
		"algorithm": d.Algorithm,
	}
	if d.SaltHint != "" {
		details["salt_hint"] = d.SaltHint
	}
	if d.MaxAttempts > 0 {
		details["max_attempts"] = strconv.Itoa(d.MaxAttempts)
	}
	for k, v := range d.Extra {
		details[k] = v
	}
	return &RequiredAction{
		Type:         RequiredActionPayPassword,
		Details:      details,
		ChallengeRef: expectedHash,
		ExpiresAt:    expiresAt,
	}
}

// ParsePayPassword 从 RequiredAction 读回
func ParsePayPassword(a *RequiredAction) PayPasswordDetails {
	if a == nil {
		return PayPasswordDetails{}
	}
	max, _ := strconv.Atoi(a.Details["max_attempts"])
	return PayPasswordDetails{
		Algorithm:   a.Details["algorithm"],
		SaltHint:    a.Details["salt_hint"],
		MaxAttempts: max,
		Extra:       copyMapExcept(a.Details, "algorithm", "salt_hint", "max_attempts"),
	}
}

// ─── QR Code（扫码支付，如支付宝 / 微信）───────────────────────────────────────

// QRCodeDetails 扫码挑战的强类型 payload
type QRCodeDetails struct {
	CodeURL        string // 字符串形式的二维码内容（alipay://... / weixin://...）
	ImageBase64    string // 渠道若直接给 PNG base64，前端可直接渲染
	ExpiresAt      time.Time
	PollIntervalMs int // 前端建议的轮询周期
	Extra          map[string]string
}

// NewQRCodeRequiredAction 构造二维码 RequiredAction
func NewQRCodeRequiredAction(d QRCodeDetails) *RequiredAction {
	details := map[string]string{}
	if d.CodeURL != "" {
		details["qrcode_url"] = d.CodeURL
	}
	if d.ImageBase64 != "" {
		details["qrcode_image_base64"] = d.ImageBase64
	}
	if d.PollIntervalMs > 0 {
		details["poll_interval_ms"] = strconv.Itoa(d.PollIntervalMs)
	}
	for k, v := range d.Extra {
		details[k] = v
	}
	return &RequiredAction{
		Type:      RequiredActionQRCodeScan,
		Details:   details,
		ExpiresAt: d.ExpiresAt,
	}
}

// ParseQRCode 从 RequiredAction 读回
func ParseQRCode(a *RequiredAction) QRCodeDetails {
	if a == nil {
		return QRCodeDetails{}
	}
	poll, _ := strconv.Atoi(a.Details["poll_interval_ms"])
	return QRCodeDetails{
		CodeURL:        a.Details["qrcode_url"],
		ImageBase64:    a.Details["qrcode_image_base64"],
		ExpiresAt:      a.ExpiresAt,
		PollIntervalMs: poll,
		Extra:          copyMapExcept(a.Details, "qrcode_url", "qrcode_image_base64", "poll_interval_ms"),
	}
}

// ─── App Redirect（跳支付宝 / 微信 / 其它 App）─────────────────────────────────

// AppRedirectDetails 跳转 App 挑战的强类型 payload
type AppRedirectDetails struct {
	RedirectURL string // universal link 或 scheme（alipays://... / weixin://...）
	Scheme      string // ios / android
	ReturnURL   string // App 完成后回跳 H5/native 的地址
	Extra       map[string]string
}

// NewAppRedirectRequiredAction 构造 RequiredAction
func NewAppRedirectRequiredAction(d AppRedirectDetails, expiresAt time.Time) *RequiredAction {
	details := map[string]string{
		"redirect_url": d.RedirectURL,
	}
	if d.Scheme != "" {
		details["scheme"] = d.Scheme
	}
	if d.ReturnURL != "" {
		details["return_url"] = d.ReturnURL
	}
	for k, v := range d.Extra {
		details[k] = v
	}
	return &RequiredAction{
		Type:      RequiredActionAppRedirect,
		Details:   details,
		ExpiresAt: expiresAt,
	}
}

// ParseAppRedirect 从 RequiredAction 读回
func ParseAppRedirect(a *RequiredAction) AppRedirectDetails {
	if a == nil {
		return AppRedirectDetails{}
	}
	return AppRedirectDetails{
		RedirectURL: a.Details["redirect_url"],
		Scheme:      a.Details["scheme"],
		ReturnURL:   a.Details["return_url"],
		Extra:       copyMapExcept(a.Details, "redirect_url", "scheme", "return_url"),
	}
}

// ─── Mini-Program JSAPI（小程序内唤起支付） ────────────────────────────────────

// MiniProgramInvokeDetails 小程序 JSAPI 唤起支付的强类型 payload。
//
// 适用：用户已在宿主 App 的小程序内，渠道（GCash / WeChat / Alipay 等）提供
// my.tradePay({paymentId}) / wx.requestPayment(...) JSAPI。Provider 区分宿主
// 渠道，前端据此选 SDK；PaymentID 是渠道生成的本笔订单标识，**唯一**必备字段。
//
// 注意：渠道签名（如 WeChat 的 paySign）若需要在前端校验，由各 adapter 在
// Extra 里附加（PartnerID / NonceStr / TimeStamp / SignType / PaySign 等）。
type MiniProgramInvokeDetails struct {
	// Provider 宿主渠道：gcash / wechat / alipay
	Provider string
	// PaymentID 渠道返回的订单 ID（必填）
	PaymentID string
	// PartnerID 商户号（GCash partnerId / WeChat mch_id），仅审计/调试用，
	// 小程序后台已绑定，前端不需要它做任何签名
	PartnerID string
	// SignMethod 渠道侧已经使用的签名算法标记（RSA256 / HMAC-SHA256），
	// 仅供前端在做服务端二次校验时识别
	SignMethod string
	// Extra 渠道特有字段（WeChat 的 nonceStr/paySign/timeStamp 等）
	Extra map[string]string
}

// NewMiniProgramInvokeRequiredAction 构造小程序 JSAPI RequiredAction。
// PaymentID 必填——前端没有它就唤不起 JSAPI。
func NewMiniProgramInvokeRequiredAction(d MiniProgramInvokeDetails, expiresAt time.Time) *RequiredAction {
	details := map[string]string{
		"payment_id": d.PaymentID,
	}
	if d.Provider != "" {
		details["provider"] = d.Provider
	}
	if d.PartnerID != "" {
		details["partner_id"] = d.PartnerID
	}
	if d.SignMethod != "" {
		details["sign_method"] = d.SignMethod
	}
	for k, v := range d.Extra {
		details[k] = v
	}
	return &RequiredAction{
		Type:         RequiredActionMiniProgramInvoke,
		Details:      details,
		ChallengeRef: d.PaymentID,
		ExpiresAt:    expiresAt,
	}
}

// ParseMiniProgramInvoke 从 RequiredAction 反解
func ParseMiniProgramInvoke(a *RequiredAction) MiniProgramInvokeDetails {
	if a == nil {
		return MiniProgramInvokeDetails{}
	}
	return MiniProgramInvokeDetails{
		Provider:   a.Details["provider"],
		PaymentID:  a.Details["payment_id"],
		PartnerID:  a.Details["partner_id"],
		SignMethod: a.Details["sign_method"],
		Extra:      copyMapExcept(a.Details, "provider", "payment_id", "partner_id", "sign_method"),
	}
}

// ─── internal helpers ────────────────────────────────────────────────────────

func copyMapExcept(m map[string]string, keys ...string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	excl := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		excl[k] = struct{}{}
	}
	out := map[string]string{}
	for k, v := range m {
		if _, drop := excl[k]; drop {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
