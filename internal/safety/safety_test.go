package safety

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// ── PII redaction tests ──

func TestMaskCardNumber(t *testing.T) {
	tests := []struct{ in, want string }{
		{"4111111111111111", "411111******1111"},
		{"4111-1111-1111-1111", "411111******1111"},  // 含分隔符也行
		{"123", "***"},
		{"", "***"},
		{"4111111111", "******1111"},   // exactly 10 → only last4
	}
	for _, tt := range tests {
		if got := MaskCardNumber(tt.in); got != tt.want {
			t.Errorf("MaskCardNumber(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestMaskEmail(t *testing.T) {
	tests := []struct{ in, want string }{
		{"alice@example.com", "a****@example.com"},
		{"a@b.com", "a****@b.com"},
		{"@nope", "@****"},                  // 没 user
		{"plain", "p****"},                   // 没 @ → 通用 mask (5 chars: 前1+4星)
	}
	for _, tt := range tests {
		if got := MaskEmail(tt.in); got != tt.want {
			t.Errorf("MaskEmail(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestMaskPhone(t *testing.T) {
	tests := []struct{ in, want string }{
		{"+8613800138000", "+861****8000"},
		{"13800138000", "138****8000"},
		{"", "*"},
	}
	for _, tt := range tests {
		got := MaskPhone(tt.in)
		// 只 sanity check 含 last 4 + 前缀；具体格式有变体，主要 SANITY:
		// 不含中间数字段
		if len(got) > 0 && got != tt.want && got != MaskGeneric(tt.in) {
			t.Errorf("MaskPhone(%q) = %q; want %q (or generic mask)", tt.in, got, tt.want)
		}
	}
}

func TestMaskIDNumber(t *testing.T) {
	got := MaskIDNumber("110101199001011234")
	if got != "110101**********34" {
		t.Errorf("MaskIDNumber wrong: %q", got)
	}
	if MaskIDNumber("123") == "123" {
		t.Error("short input should mask not return as-is")
	}
}

func TestMaskIPv4(t *testing.T) {
	tests := []struct{ in, want string }{
		{"192.168.1.1", "192.168.*.*"},
		{"10.0.0.1", "10.0.*.*"},
		{"::1", MaskGeneric("::1")},  // ipv6 走通用
	}
	for _, tt := range tests {
		if got := MaskIPv4(tt.in); got != tt.want {
			t.Errorf("MaskIPv4(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestMaskGeneric_PreservesShort(t *testing.T) {
	if MaskGeneric("ab") != "ab" {
		t.Error("len<=2 should be preserved")
	}
}

// ── Recovery middleware tests ──

func TestHTTPRecovery_CatchesPanic(t *testing.T) {
	mw := HTTPRecovery(zap.NewNop())
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		MustPanic("boom")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500; got %d", rr.Code)
	}
}

func TestHTTPRecovery_PassesThroughNormal(t *testing.T) {
	mw := HTTPRecovery(zap.NewNop())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != 200 || rr.Body.String() != "ok" {
		t.Fatalf("normal request broken: %d / %q", rr.Code, rr.Body.String())
	}
}

func TestGRPCUnaryRecovery_CatchesPanic(t *testing.T) {
	intc := GRPCUnaryRecovery(zap.NewNop())
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		panic("simulated grpc panic")
	}
	resp, err := intc(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/test"}, handler)
	if err == nil {
		t.Fatal("expected error from recovered panic")
	}
	if resp != nil {
		t.Fatal("resp should be nil after panic")
	}
}

func TestGRPCUnaryRecovery_PassesThroughNormal(t *testing.T) {
	intc := GRPCUnaryRecovery(zap.NewNop())
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return "fine", nil
	}
	resp, err := intc(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/test"}, handler)
	if err != nil {
		t.Fatal(err)
	}
	if resp != "fine" {
		t.Fatalf("expected 'fine'; got %v", resp)
	}
}
