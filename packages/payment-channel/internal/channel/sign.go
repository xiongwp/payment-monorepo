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
