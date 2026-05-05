package channel

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// HMACSHA256Hex 常用于 GrabPay / UnionBank 回调验签。
func HMACSHA256Hex(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// HMACSHA256Base64 Maya 风格。
func HMACSHA256Base64(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// SHA1Hex Coins.ph / 部分老银行用。
func SHA1Hex(secret, payload []byte) string {
	h := sha1.New()
	h.Write(secret)
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// ConstantTimeEqHex 对 hex 字符串做常数时间比对，避免 timing attack。
//
// Deprecated: 用 ConstantTimeEqStr —— 不止 hex，base64 / 任何固定长度字符串都该用。
func ConstantTimeEqHex(a, b string) bool {
	return ConstantTimeEqStr(a, b)
}

// ConstantTimeEqStr 对任意字符串做常量时间比对（hex / base64 / raw 都可）。
// 长度不同直接返回 false（长度可被外部观察，不构成 timing leak）。
func ConstantTimeEqStr(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// VerifyHMACSHA256Hex 一键调用：算 HMAC-SHA256(secret, body)，hex 编码后跟 sig 常量时间比。
// 推荐 channel adapter 用本函数代替手写 `sig == HMACSHA256Hex(...)`。
func VerifyHMACSHA256Hex(secret, body []byte, sig string) bool {
	return ConstantTimeEqStr(sig, HMACSHA256Hex(secret, body))
}

// VerifyHMACSHA256Base64 base64 版本。
func VerifyHMACSHA256Base64(secret, body []byte, sig string) bool {
	return ConstantTimeEqStr(sig, HMACSHA256Base64(secret, body))
}

// ─── Replay protection helpers ────────────────────────────────────────────────
//
// 渠道回调如带 timestamp，应该校验 timestamp 与本地时钟差不能超过 tolerance。
// tolerance 太松（> 30min）= 被截获后能反复重放；太紧（< 30s）= 时钟漂移误杀。
// 推荐 5min（300s），跟 Stripe 默认一致。
//
// 渠道侧通常只有 timestamp + signature 两个 header，本平台不强制 nonce（多数
// 渠道不发 nonce）。如商户需要绝对幂等，靠 inbound_webhook 表的 (event_id,
// channel) 唯一索引兜底 —— 同 event_id 第二次进入直接命中 dup，不影响业务态。

// VerifyTimestampWithTolerance 校验渠道 timestamp 与本地时钟差不超过 tolerance。
// ts 单位是 unix 秒；now 由调用方注入便于单测（生产传 time.Now().Unix()）。
//
// 返回 true 表示 timestamp 在窗口内，可以继续后续 sig 校验；
// 返回 false 表示 timestamp 太老或太新（防重放 + 防时钟攻击）。
func VerifyTimestampWithTolerance(ts int64, now int64, toleranceSeconds int64) bool {
	if toleranceSeconds <= 0 {
		toleranceSeconds = 300 // 5min default
	}
	diff := now - ts
	if diff < 0 {
		diff = -diff
	}
	return diff <= toleranceSeconds
}

// ─── WebhookVerifier 契约（用于跨 adapter 一致性测试）──────────────────────────
//
// 各渠道 adapter 的 ParseWebhook 内部必然调用一个签名校验逻辑。本接口把那段
// 逻辑暴露成可独立测试的形式，便于跨 adapter 跑同一套安全测试矩阵：
//
//   1. 合法 (headers, body) → ok=true
//   2. body 被篡改 → ok=false
//   3. headers 中 signature 字段被改 → ok=false
//   4. timestamp 超出 tolerance（如该 adapter 检查时间戳）→ ok=false
//
// 每个 adapter 应在 internal/adapter/<name>/<name>_test.go 跑一个
// TestWebhookVerifier_Contract 通过 RunVerifierContract 跑 4 case。
type WebhookVerifier interface {
	// VerifyHeaders 校验 (headers, body) 是否构成合法回调。
	// 返回 (ok, reason)：reason 用于日志，不要泄漏到 webhook response（防探测）。
	VerifyHeaders(headers map[string]string, body []byte) (ok bool, reason string)
}
