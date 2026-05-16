package errcode

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"
)

func TestNewAndError(t *testing.T) {
	e := New(CodeIdempotencyConflict, "pi mismatch")
	if e.Code() != CodeIdempotencyConflict {
		t.Errorf("code wrong: %s", e.Code())
	}
	if e.Error() != "idempotency_conflict: pi mismatch" {
		t.Errorf("Error() format: %q", e.Error())
	}
}

func TestHTTPStatus(t *testing.T) {
	cases := map[Code]int{
		CodeInvalidArgument:    400,
		CodeUnauthenticated:    401,
		CodePermissionDenied:   403,
		CodeNotFound:           404,
		CodeIdempotencyConflict: 409,
		CodeRefundExceedsCharge: 422,
		CodeRateLimited:        429,
		CodeUnavailable:        503,
		CodeChannelUnavailable: 503,
		CodeChannelTimeout:     504,
	}
	for code, want := range cases {
		e := New(code)
		if got := e.HTTPStatus(); got != want {
			t.Errorf("HTTPStatus(%s): want %d got %d", code, want, got)
		}
	}
}

func TestGRPCCode(t *testing.T) {
	cases := map[Code]codes.Code{
		CodeInvalidArgument:    codes.InvalidArgument,
		CodeAlreadyExists:      codes.AlreadyExists,
		CodeRateLimited:        codes.ResourceExhausted,
		CodeChannelUnavailable: codes.Unavailable,
	}
	for code, want := range cases {
		if got := New(code).GRPCCode(); got != want {
			t.Errorf("GRPCCode(%s): want %s got %s", code, want, got)
		}
	}
}

func TestToProblem_I18N(t *testing.T) {
	e := New(CodeCardDeclined, "issuer rejected")
	ctxZH := WithLang(context.Background(), "zh")
	p := e.ToProblem(ctxZH)
	if p.Status != http.StatusInternalServerError {
		t.Errorf("expect 500, got %d", p.Status)
	}
	if p.Code != "card_declined" {
		t.Errorf("code wrong: %s", p.Code)
	}
	if p.Title != "卡片拒付" {
		t.Errorf("zh title wrong: %s", p.Title)
	}

	pEN := e.ToProblem(context.Background()) // default en
	if pEN.Title != "Card declined" {
		t.Errorf("en title wrong: %s", pEN.Title)
	}
}

func TestAs_Wrapped(t *testing.T) {
	e := New(CodeNotFound, "user_id=42")
	wrapped := errors.New("wrap: " + e.Error())
	if got := As(wrapped); got != nil {
		t.Errorf("As should be nil for non-Error wrap, got %+v", got)
	}
	if got := As(e); got == nil {
		t.Errorf("As should find Error in direct call")
	}
}

func TestMessage_Fallback(t *testing.T) {
	if Message("unknown_code", "zh") != "unknown_code" {
		t.Errorf("unknown code should return raw")
	}
	if Message(string(CodeCardDeclined), "xx_missing") != "Card declined" {
		t.Errorf("missing lang should fall back to en")
	}
}
