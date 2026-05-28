// Package auth — TOTP (RFC 6238) MFA verifier。
//
// 用途：admin SSO 二步验证。OIDC ID Token 通过 = "你登录过 IdP"；TOTP code 通过 =
// "你当前真实控制设备"。两者结合满足 SOC2 CC6.1 MFA requirement。
//
// 实现要点（RFC 6238 / 4226）：
//   - Time step = 30s，code = HOTP(secret, floor(unixtime/30))，6 digit。
//   - HMAC-SHA1（兼容 Google Authenticator / Authy；SHA256 留 future）。
//   - Verify 容忍 ±1 step（±30s）抵御客户端 / 服务端时钟漂移；不放宽到 ±2 防 replay 攻
//     击窗变大。生产应再加 replay cache（已用过的 code 30s 内拒）。
//   - Secret 16 字节随机，Base32 编码（otpauth:// URI 标准）。
//
// 存储：本任务用 mem store，生产换 PG (admin_user.totp_secret_enc, KMS-wrapped)
// 或 Redis (短期 enroll 流程)。Secret 永不在 audit log / metrics 出现。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"sync"
	"time"
)

const (
	totpDigits = 6
	totpStep   = 30 * time.Second
)

// TOTPVerifier user_id → secret。本任务 mem 版；生产换 PG-backed (Lookup/Save 接口
// 抽象出来即可，此处暂内嵌)。
type TOTPVerifier struct {
	mu      sync.RWMutex
	secrets map[string]string // user_id → base32 secret
	// replay 缓存：(user_id, step_counter) → 用过；防 30s 内同一 code 重放。
	used map[string]struct{}
}

// NewTOTPVerifier 空 mem store。
func NewTOTPVerifier() *TOTPVerifier {
	return &TOTPVerifier{
		secrets: map[string]string{},
		used:    map[string]struct{}{},
	}
}

// Enroll 给 user 生成新 secret + 返回 otpauth:// URI（前端渲 QR）。
// 老 secret 覆盖（rotate 流程）。
//
// issuer / accountName 出现在 Authenticator app 列表里（如 "Risk Admin: alice@x.com"）。
func (v *TOTPVerifier) Enroll(userID, issuer, accountName string) (secret, otpauthURI string, err error) {
	if userID == "" {
		return "", "", fmt.Errorf("user_id required")
	}
	raw := make([]byte, 16) // 128bit; RFC 4226 §4 要求 ≥ 128bit
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	secret = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	v.mu.Lock()
	v.secrets[userID] = secret
	v.mu.Unlock()
	// otpauth://totp/Issuer:account?secret=...&issuer=Issuer&digits=6&period=30&algorithm=SHA1
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("digits", fmt.Sprintf("%d", totpDigits))
	q.Set("period", "30")
	q.Set("algorithm", "SHA1")
	label := url.PathEscape(issuer + ":" + accountName)
	otpauthURI = fmt.Sprintf("otpauth://totp/%s?%s", label, q.Encode())
	return secret, otpauthURI, nil
}

// Has 是否已 enroll。
func (v *TOTPVerifier) Has(userID string) bool {
	v.mu.RLock()
	_, ok := v.secrets[userID]
	v.mu.RUnlock()
	return ok
}

// Verify 校验 code。容忍 ±1 step。匹配后标记 (user_id, counter) 防 replay。
func (v *TOTPVerifier) Verify(userID, code string) bool {
	if userID == "" || len(code) != totpDigits {
		return false
	}
	v.mu.RLock()
	secret := v.secrets[userID]
	v.mu.RUnlock()
	if secret == "" {
		return false
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return false
	}
	now := time.Now().Unix()
	step := now / int64(totpStep.Seconds())
	for _, offset := range []int64{0, -1, 1} {
		expect := hotp(key, step+offset)
		if subtle.ConstantTimeCompare([]byte(expect), []byte(code)) == 1 {
			repKey := fmt.Sprintf("%s/%d", userID, step+offset)
			v.mu.Lock()
			if _, used := v.used[repKey]; used {
				v.mu.Unlock()
				return false // replay
			}
			v.used[repKey] = struct{}{}
			// 简单 GC：mem 超 10000 条清空（生产用 LRU + TTL）
			if len(v.used) > 10000 {
				v.used = map[string]struct{}{repKey: {}}
			}
			v.mu.Unlock()
			return true
		}
	}
	return false
}

// hotp HMAC-SHA1 6-digit truncation（RFC 4226 §5.3）。
func hotp(key []byte, counter int64) string {
	var ctrBuf [8]byte
	binary.BigEndian.PutUint64(ctrBuf[:], uint64(counter))
	h := hmac.New(sha1.New, key)
	h.Write(ctrBuf[:])
	sum := h.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off])&0x7f)<<24 |
		uint32(sum[off+1])<<16 |
		uint32(sum[off+2])<<8 |
		uint32(sum[off+3])
	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, bin%mod)
}

// GenerateAt 给测试用：拿任意时刻应该出的 code。生产 admin endpoint 不暴露。
func GenerateAt(secret string, t time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return "", err
	}
	step := t.Unix() / int64(totpStep.Seconds())
	return hotp(key, step), nil
}
