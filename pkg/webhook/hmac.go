// Package webhook — HMAC-SHA256 签名 / 验签，Stripe 风格。
//
// 出站 webhook：
//   header X-Signature: t=<unix_seconds>,v1=<hex_hmac>
//   signed_payload: t + "." + body
//
// 验签端强制时间戳 ±ToleranceSec 内；防重放也防抓包回放。Verify 使用
// constant-time compare。所有 hex 比较都小写，避免 case 差异。
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader 约定 header 名（out / in 对称）
const SignatureHeader = "X-Signature"

// ErrInvalidSignature 签名错 / 过期 / 格式错都归并到一个错误，防侧信道泄露具体原因。
var ErrInvalidSignature = errors.New("invalid webhook signature")

// Sign 计算签名字符串 "t=<now>,v1=<hex>"。secret 以字节为单位。
func Sign(secret []byte, body []byte) string {
	return SignAt(secret, body, time.Now())
}

// SignAt Sign 的可测版；at 应为签发时间。
func SignAt(secret []byte, body []byte, at time.Time) string {
	ts := at.Unix()
	mac := computeMAC(secret, ts, body)
	return fmt.Sprintf("t=%d,v1=%s", ts, mac)
}

// Verify 校验 body 与 header 签名；tolerance 允许的最大时钟偏差。
// 常量时间 compare；分离 parse error 和 verify error 都不暴露给调用方。
func Verify(secret []byte, body []byte, header string, tolerance time.Duration) error {
	return VerifyAt(secret, body, header, tolerance, time.Now())
}

// VerifyAt 可测版；now 允许注入。
func VerifyAt(secret []byte, body []byte, header string, tolerance time.Duration, now time.Time) error {
	ts, sig, err := parseHeader(header)
	if err != nil {
		return ErrInvalidSignature
	}
	if tolerance > 0 && absSeconds(now.Unix()-ts) > int64(tolerance.Seconds()) {
		return ErrInvalidSignature
	}
	expected := computeMAC(secret, ts, body)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return ErrInvalidSignature
	}
	return nil
}

func computeMAC(secret []byte, ts int64, body []byte) string {
	h := hmac.New(sha256.New, secret)
	_, _ = h.Write([]byte(strconv.FormatInt(ts, 10)))
	_, _ = h.Write([]byte{'.'})
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func parseHeader(header string) (int64, string, error) {
	var ts int64 = -1
	var sig string
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "t="):
			n, err := strconv.ParseInt(part[2:], 10, 64)
			if err != nil {
				return 0, "", err
			}
			ts = n
		case strings.HasPrefix(part, "v1="):
			sig = strings.ToLower(part[3:])
		}
	}
	if ts <= 0 || sig == "" {
		return 0, "", errors.New("incomplete header")
	}
	return ts, sig, nil
}

func absSeconds(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
