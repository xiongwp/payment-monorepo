// hmac_test.go — webhook 签名校验测试。最高 security 优先级。

package sign

import (
	"strings"
	"testing"
	"time"
)

func TestCompute_Deterministic(t *testing.T) {
	secret := "whsec_test"
	ts := int64(1715000000)
	body := []byte(`{"event":"charge.succeeded"}`)
	a := Compute(secret, ts, body)
	b := Compute(secret, ts, body)
	if a != b {
		t.Errorf("compute not deterministic: %s vs %s", a, b)
	}
	if len(a) != 64 { // hex(sha256) = 64 chars
		t.Errorf("expected 64-char hex, got %d", len(a))
	}
}

func TestCompute_DifferentSecret(t *testing.T) {
	ts := int64(1715000000)
	body := []byte(`{"x":1}`)
	a := Compute("secret_a", ts, body)
	b := Compute("secret_b", ts, body)
	if a == b {
		t.Errorf("different secret should produce different sig")
	}
}

func TestCompute_DifferentBody(t *testing.T) {
	ts := int64(1715000000)
	a := Compute("s", ts, []byte("body1"))
	b := Compute("s", ts, []byte("body2"))
	if a == b {
		t.Errorf("different body should produce different sig")
	}
}

func TestVerify_HappyPath(t *testing.T) {
	secret := "whsec_test"
	now := time.Now().Unix()
	body := []byte(`{"event":"charge.succeeded"}`)
	sig := Compute(secret, now, body)
	header := "t=" + itoa(now) + ",v1=" + sig
	if err := Verify(secret, header, body, 300); err != nil {
		t.Errorf("verify failed: %v", err)
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	now := time.Now().Unix()
	body := []byte("x")
	sig := Compute("real", now, body)
	header := "t=" + itoa(now) + ",v1=" + sig
	if err := Verify("attacker", header, body, 300); err == nil {
		t.Errorf("wrong secret should fail")
	}
}

func TestVerify_TamperedBody(t *testing.T) {
	secret := "s"
	now := time.Now().Unix()
	sig := Compute(secret, now, []byte("original"))
	header := "t=" + itoa(now) + ",v1=" + sig
	if err := Verify(secret, header, []byte("tampered"), 300); err == nil {
		t.Errorf("tampered body should fail signature")
	}
}

func TestVerify_ReplayOldTimestamp(t *testing.T) {
	secret := "s"
	old := time.Now().Add(-10 * time.Minute).Unix()
	sig := Compute(secret, old, []byte("x"))
	header := "t=" + itoa(old) + ",v1=" + sig
	// 5min tolerance
	if err := Verify(secret, header, []byte("x"), 300); err == nil {
		t.Errorf("10min old timestamp should fail replay defense")
	}
	// 但 15min tolerance 应该通过
	if err := Verify(secret, header, []byte("x"), 900); err != nil {
		t.Errorf("15min tolerance: expected pass, got %v", err)
	}
}

func TestVerify_FutureTimestamp(t *testing.T) {
	secret := "s"
	future := time.Now().Add(10 * time.Minute).Unix()
	sig := Compute(secret, future, []byte("x"))
	header := "t=" + itoa(future) + ",v1=" + sig
	if err := Verify(secret, header, []byte("x"), 300); err == nil {
		t.Errorf("future timestamp should fail replay defense")
	}
}

func TestVerify_MalformedHeader(t *testing.T) {
	cases := []string{
		"",
		"junk",
		"t=abc,v1=def",  // bad timestamp
		"t=123",         // missing v1
		"v1=abc",        // missing t
	}
	for _, h := range cases {
		if err := Verify("s", h, []byte("x"), 300); err == nil {
			t.Errorf("malformed header %q should fail", h)
		}
	}
}

func TestVerify_ConstantTime(t *testing.T) {
	// 不能通过比较时间差暴露签名内容。
	// 简化测：构造 4 个错误签名（一个 1 字节错，一个全错），
	// Verify 都应该返 mismatch；不真测时序，只测都失败。
	secret := "s"
	now := time.Now().Unix()
	body := []byte("x")
	real := Compute(secret, now, body)
	cases := []string{
		strings.Repeat("0", 64),
		real[:63] + "0",
		"A" + real[1:],
		strings.Repeat("f", 64),
	}
	for _, bad := range cases {
		h := "t=" + itoa(now) + ",v1=" + bad
		if err := Verify(secret, h, body, 300); err == nil {
			t.Errorf("bad sig %q should fail", bad)
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	s := ""
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}
