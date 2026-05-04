package channel

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
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
func ConstantTimeEqHex(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var d byte
	for i := 0; i < len(a); i++ {
		d |= a[i] ^ b[i]
	}
	return d == 0
}
