// rotate.go — 签名 key 滚动支持.
//
// 场景: secret 泄露 / 定期轮换 / 商户主动重置. 必须有平滑过渡窗口, 否则商户验签直接全黑.
//
// 方案: 双 key 重叠期 — 服务端 24-72h 内同时下发两把 v1 签名:
//
//   X-Webhook-Signature: t=1715000000,v1=PRIMARY_SIG,v1=SECONDARY_SIG
//
// 商户用任一 secret 都能验过。
//
// 验签也支持: VerifyMulti([secret1, secret2], header, body) — 任一 secret 命中即 ok.

package sign

import (
	"crypto/hmac"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ComputeMulti 用多个 secret 同时签, 返回 header value.
//
// 输出格式 (跟 Stripe 同 — 多个 v1 用逗号):
//   "t=1715000000,v1=sig1,v1=sig2"
//
// 商户只需用任一 secret 重算 sig 跟头里的 v1 比对, 任一过即认.
func ComputeMulti(secrets []string, timestamp int64, body []byte) string {
	if len(secrets) == 0 {
		return ""
	}
	parts := []string{"t=" + strconv.FormatInt(timestamp, 10)}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		parts = append(parts, "v1="+Compute(s, timestamp, body))
	}
	return strings.Join(parts, ",")
}

// VerifyMulti 用多 secret 验签 — 任一过即 ok.
//
// 商户在轮换期收到带 2 个 v1 的 header; 一般用本服务的 helper 自动处理.
// 也可以用于商户主动校验 (有 2 个生效中 secret 时).
func VerifyMulti(secrets []string, header string, body []byte, toleranceSeconds int) error {
	t, sigs, err := parseHeaderMulti(header)
	if err != nil {
		return err
	}
	if toleranceSeconds <= 0 {
		toleranceSeconds = 300
	}
	if delta := time.Now().Unix() - t; delta > int64(toleranceSeconds) || delta < -int64(toleranceSeconds) {
		return fmt.Errorf("timestamp out of tolerance (delta=%ds)", delta)
	}
	// 用每个 secret 算一次 expected; 跟 header 里的所有 sig 配对; 任一 hmac.Equal 通过即 ok
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		expected := Compute(secret, t, body)
		for _, sig := range sigs {
			if hmac.Equal([]byte(expected), []byte(sig)) {
				return nil
			}
		}
	}
	return fmt.Errorf("signature mismatch (no secret matched)")
}

// parseHeaderMulti 解析 "t=...,v1=A,v1=B" — 跟 parseHeader 类似但 sig 是 []
func parseHeaderMulti(h string) (timestamp int64, sigs []string, err error) {
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "t=") {
			ts, perr := strconv.ParseInt(part[2:], 10, 64)
			if perr != nil {
				return 0, nil, fmt.Errorf("bad timestamp: %w", perr)
			}
			timestamp = ts
		} else if strings.HasPrefix(part, "v1=") {
			sigs = append(sigs, part[3:])
		}
	}
	if timestamp == 0 || len(sigs) == 0 {
		return 0, nil, fmt.Errorf("malformed signature header")
	}
	return timestamp, sigs, nil
}
