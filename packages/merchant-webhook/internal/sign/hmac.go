// Package sign — HMAC-SHA256 签名 + 验签 helper（同 Stripe 风格）。
//
// 签名串构造:
//   payload_to_sign = strconv.FormatInt(timestamp, 10) + "." + raw_body
//   signature = hex(HMAC-SHA256(secret, payload_to_sign))
//
// header 格式:
//   X-Webhook-Signature: t=1715000000,v1=abc123def...
//
// 商户验签步骤:
//   1. 取 header 解析 t / v1
//   2. 检查 |now - t| < 5min（防 replay attack）
//   3. 用 endpoint secret 重算签名
//   4. constant-time 比对（防 timing attack）

package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Compute HMAC-SHA256 签名。
func Compute(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify 给定 header 字符串和 body，校验签名 + timestamp 不超过 toleranceSeconds。
//
// header 格式 "t=1715000000,v1=abc..."
//
// 返:
//   nil       签名正确 + timestamp 在容忍范围内
//   error     签名错 / 过期 / 格式错
func Verify(secret, header string, body []byte, toleranceSeconds int) error {
	t, sig, err := parseHeader(header)
	if err != nil {
		return err
	}
	if toleranceSeconds <= 0 {
		toleranceSeconds = 300 // 5 min
	}
	if delta := time.Now().Unix() - t; delta > int64(toleranceSeconds) || delta < -int64(toleranceSeconds) {
		return fmt.Errorf("timestamp out of tolerance (delta=%ds)", delta)
	}
	expected := Compute(secret, t, body)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

func parseHeader(h string) (timestamp int64, sig string, err error) {
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "t=") {
			ts, perr := strconv.ParseInt(part[2:], 10, 64)
			if perr != nil {
				return 0, "", fmt.Errorf("bad timestamp: %w", perr)
			}
			timestamp = ts
		} else if strings.HasPrefix(part, "v1=") {
			sig = part[3:]
		}
	}
	if timestamp == 0 || sig == "" {
		return 0, "", fmt.Errorf("malformed signature header")
	}
	return timestamp, sig, nil
}
