package vault

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeKMS test double：echoes plaintext as ciphertext, AAD strict match.
// 同 KMS 实现：encrypt(p, aad) -> base64 + checksum；decrypt 校验 aad。
type fakeKMS struct {
	failNext bool
}

func (f *fakeKMS) Encrypt(_ context.Context, plaintext []byte, aad string) (string, string, error) {
	if f.failNext {
		f.failNext = false
		return "", "", errors.New("kms boom")
	}
	wrap := struct {
		AAD string `json:"aad"`
		PT  []byte `json:"pt"`
	}{aad, plaintext}
	b, _ := json.Marshal(&wrap)
	return string(b), "v1", nil
}

func (f *fakeKMS) Decrypt(_ context.Context, ct string, aad string) ([]byte, string, error) {
	var wrap struct {
		AAD string `json:"aad"`
		PT  []byte `json:"pt"`
	}
	if err := json.Unmarshal([]byte(ct), &wrap); err != nil {
		return nil, "", err
	}
	if wrap.AAD != aad {
		return nil, "", errors.New("aad mismatch")
	}
	return wrap.PT, "v1", nil
}

func TestTokenize_AndDetokenizeViaPayment(t *testing.T) {
	v := NewVault(&fakeKMS{}, time.Now)
	ctx := context.Background()

	// Tokenize
	stored, kid, err := v.Tokenize(ctx, "user_001", "4111111111111111", 12, 2030, "Alice")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	if !hasPrefix(stored, StoredTokenPrefix) || kid != "v1" {
		t.Fatalf("token=%q kid=%q", stored, kid)
	}

	// CreatePaymentToken
	payTok, _, expAt, err := v.CreatePaymentToken(ctx, "user_001", stored, "pi_abc", 1000, "PHP", 0)
	if err != nil {
		t.Fatalf("CreatePaymentToken: %v", err)
	}
	if expAt.Before(time.Now().Add(29 * time.Minute)) {
		t.Fatalf("payment token TTL too short: %v", expAt)
	}

	// Detokenize
	got, err := v.Detokenize(ctx, payTok, "pi_abc")
	if err != nil {
		t.Fatalf("Detokenize: %v", err)
	}
	if got.PAN != "4111111111111111" {
		t.Fatalf("pan mismatch: %s", got.PAN)
	}
	if got.PIID != "pi_abc" {
		t.Fatalf("pi_id: %s", got.PIID)
	}
}

func TestTokenize_WrongUserCannotDetokenize(t *testing.T) {
	v := NewVault(&fakeKMS{}, time.Now)
	ctx := context.Background()
	stored, _, err := v.Tokenize(ctx, "user_001", "4111111111111111", 12, 2030, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	// 用别的 user_id 派生 → AAD 不一致 → 拒
	_, _, _, err = v.CreatePaymentToken(ctx, "user_002", stored, "pi_abc", 1000, "PHP", 0)
	if !errors.Is(err, ErrAADMismatch) {
		t.Fatalf("want ErrAADMismatch, got %v", err)
	}
}

func TestDetokenize_WrongPI(t *testing.T) {
	v := NewVault(&fakeKMS{}, time.Now)
	ctx := context.Background()
	stored, _, _ := v.Tokenize(ctx, "user_001", "4111111111111111", 12, 2030, "Alice")
	payTok, _, _, _ := v.CreatePaymentToken(ctx, "user_001", stored, "pi_abc", 1000, "PHP", 0)

	_, err := v.Detokenize(ctx, payTok, "pi_other")
	if !errors.Is(err, ErrAADMismatch) {
		t.Fatalf("want ErrAADMismatch, got %v", err)
	}
}

func TestDetokenize_Expired(t *testing.T) {
	now := time.Now()
	clock := &clockStub{t: now}
	v := NewVault(&fakeKMS{}, clock.Now)
	ctx := context.Background()
	stored, _, _ := v.Tokenize(ctx, "user_001", "4111111111111111", 12, 2030, "Alice")
	payTok, _, _, _ := v.CreatePaymentToken(ctx, "user_001", stored, "pi_abc", 1000, "PHP", 5*time.Second)

	clock.t = now.Add(10 * time.Second) // 已过期
	_, err := v.Detokenize(ctx, payTok, "pi_abc")
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("want ErrTokenExpired, got %v", err)
	}
}

func TestCreatePaymentToken_TTLClampedTo30Min(t *testing.T) {
	v := NewVault(&fakeKMS{}, time.Now)
	ctx := context.Background()
	stored, _, _ := v.Tokenize(ctx, "user_001", "4111111111111111", 12, 2030, "Alice")

	// 传 1 hour，应该被 clamp 到 30min
	_, _, expAt, _ := v.CreatePaymentToken(ctx, "user_001", stored, "pi_abc", 1000, "PHP", 1*time.Hour)
	maxAt := time.Now().Add(MaxPaymentTTL + time.Second)
	if expAt.After(maxAt) {
		t.Fatalf("ttl not clamped: expAt=%v maxAllowed=%v", expAt, maxAt)
	}
}

func TestMaskPAN(t *testing.T) {
	cases := map[string]string{
		"4111111111111111": "411111******1111",
		"345678901234567":  "345678*****4567",
		"1234":             "1234",
	}
	for in, want := range cases {
		if got := MaskPAN(in); got != want {
			t.Errorf("MaskPAN(%q)=%q want %q", in, got, want)
		}
	}
}

func TestDetectNetwork(t *testing.T) {
	cases := map[string]string{
		"4111111111111111": "visa",
		"5234567890123456": "mastercard",
		"371234567890123":  "amex",
		"3528888888888888": "jcb",
		"6212345678901234": "unionpay",
		"9999999999999999": "unknown",
	}
	for pan, want := range cases {
		if got := DetectNetwork(pan); got != want {
			t.Errorf("DetectNetwork(%q)=%q want %q", pan, got, want)
		}
	}
}

type clockStub struct{ t time.Time }

func (c *clockStub) Now() time.Time { return c.t }
