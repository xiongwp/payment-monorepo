package channel

import (
	"testing"
	"time"
)

// roundtrip 构造 + 反解 必须无损还原 PaymentID / Provider / PartnerID / SignMethod / Extra。
// 同时确保 Type / ChallengeRef / ExpiresAt 设置正确——前端依赖 ChallengeRef 做幂等确认。
func TestMiniProgramInvokeRequiredAction_Roundtrip(t *testing.T) {
	exp := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second)
	in := MiniProgramInvokeDetails{
		Provider:   "gcash",
		PaymentID:  "PAY999",
		PartnerID:  "P-TEST",
		SignMethod: "RSA256",
		Extra: map[string]string{
			"app_id":   "wx-clone-test",
			"trace_id": "abc-123",
		},
	}
	a := NewMiniProgramInvokeRequiredAction(in, exp)

	if a.Type != RequiredActionMiniProgramInvoke {
		t.Fatalf("Type = %q, want %q", a.Type, RequiredActionMiniProgramInvoke)
	}
	if a.ChallengeRef != "PAY999" {
		t.Fatalf("ChallengeRef = %q, want PAY999 (== payment_id)", a.ChallengeRef)
	}
	if !a.ExpiresAt.Equal(exp) {
		t.Fatalf("ExpiresAt = %v, want %v", a.ExpiresAt, exp)
	}

	out := ParseMiniProgramInvoke(a)
	if out.Provider != in.Provider {
		t.Fatalf("Provider lost: got %q, want %q", out.Provider, in.Provider)
	}
	if out.PaymentID != in.PaymentID {
		t.Fatalf("PaymentID lost: got %q, want %q", out.PaymentID, in.PaymentID)
	}
	if out.PartnerID != in.PartnerID {
		t.Fatalf("PartnerID lost")
	}
	if out.SignMethod != in.SignMethod {
		t.Fatalf("SignMethod lost")
	}
	if out.Extra["app_id"] != "wx-clone-test" || out.Extra["trace_id"] != "abc-123" {
		t.Fatalf("Extra fields lost: %+v", out.Extra)
	}
}

// nil 输入返回零值，不 panic
func TestParseMiniProgramInvoke_Nil(t *testing.T) {
	out := ParseMiniProgramInvoke(nil)
	if out.PaymentID != "" || out.Provider != "" {
		t.Fatalf("nil input should yield zero value, got %+v", out)
	}
}

// 仅 PaymentID 非空时其余字段不应在 Details 里出现（避免 nil dereference 的反向）
func TestNewMiniProgramInvokeRequiredAction_MinimalDetails(t *testing.T) {
	a := NewMiniProgramInvokeRequiredAction(MiniProgramInvokeDetails{PaymentID: "PAY1"}, time.Now())
	if a.Details["payment_id"] != "PAY1" {
		t.Fatalf("payment_id missing: %+v", a.Details)
	}
	for _, k := range []string{"provider", "partner_id", "sign_method"} {
		if _, ok := a.Details[k]; ok {
			t.Fatalf("optional field %q must not be present when zero, got %+v", k, a.Details)
		}
	}
}
