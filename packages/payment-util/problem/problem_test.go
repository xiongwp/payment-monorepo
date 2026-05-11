package problem

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNew_DefaultStatus(t *testing.T) {
	p := New()
	if p.Status != 500 || p.Title != "Internal Error" {
		t.Errorf("default status %d title %q", p.Status, p.Title)
	}
}

func TestNew_WithOptions(t *testing.T) {
	p := New(WithStatus(400), WithCode("invalid_argument"),
		WithTitle("Bad"), WithDetail("field %s required", "name"))
	if p.Code != "invalid_argument" || p.Status != 400 {
		t.Fatal("options not applied")
	}
	if !strings.Contains(p.Detail, "name") {
		t.Errorf("detail %q missing format arg", p.Detail)
	}
	if !strings.HasSuffix(p.Type, "/invalid_argument") {
		t.Errorf("auto type missing: %s", p.Type)
	}
}

func TestWrite_SetsHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/foo", nil)
	Write(w, r, Forbidden("charge:write"))
	if got := w.Header().Get("Content-Type"); got != ContentType {
		t.Errorf("content-type=%s want %s", got, ContentType)
	}
	if w.Code != 403 {
		t.Errorf("status=%d want 403", w.Code)
	}
	var p Problem
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.Code != "insufficient_scope" {
		t.Errorf("code=%s", p.Code)
	}
	if p.Meta["required_scope"] != "charge:write" {
		t.Error("meta missing required_scope")
	}
	if p.Instance != "/api/v1/foo" {
		t.Errorf("auto-instance failed: %s", p.Instance)
	}
}

func TestInsufficientBalance_Helper(t *testing.T) {
	p := InsufficientBalance("acc_1", 50, 100, "USD")
	if p.Status != 402 || p.Code != "insufficient_balance" {
		t.Fatal("preset wrong")
	}
	if p.Meta["have"] != int64(50) {
		t.Errorf("meta have=%v", p.Meta["have"])
	}
}

func TestFromError_PassesProblem(t *testing.T) {
	orig := NotFound("merchant_001")
	got := FromError(orig)
	if got != orig {
		t.Error("FromError should pass *Problem through")
	}
}

func TestFromError_WrapsPlain(t *testing.T) {
	err := errString("boom")
	p := FromError(err)
	if p.Status != 500 || !strings.Contains(p.Detail, "boom") {
		t.Errorf("FromError wrap: %+v", p)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestTraceIDFromCtx(t *testing.T) {
	ctx := context.WithValue(context.Background(), traceCtxKey, "tid-abc")
	r := httptest.NewRequest("GET", "/x", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	Write(w, r, Unauthorized("no token"))
	var p Problem
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.TraceID != "tid-abc" {
		t.Errorf("trace_id not auto-injected: %s", p.TraceID)
	}
}
