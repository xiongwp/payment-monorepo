package healthx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLiveness_AlwaysOK(t *testing.T) {
	rec := httptest.NewRecorder()
	Liveness(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("expected body ok, got %q", rec.Body.String())
	}
}

func TestReadiness_AllProbesPass(t *testing.T) {
	h := Readiness(
		ProbeFunc{N: "p1", F: func(_ context.Context) error { return nil }},
		ProbeFunc{N: "p2", F: func(_ context.Context) error { return nil }},
	)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body struct {
		Status string                            `json:"status"`
		Probes map[string]struct{ OK bool `json:"ok"` } `json:"probes"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" {
		t.Errorf("expected status ok, got %s", body.Status)
	}
	if len(body.Probes) != 2 {
		t.Errorf("expected 2 probes, got %d", len(body.Probes))
	}
	for name, p := range body.Probes {
		if !p.OK {
			t.Errorf("probe %s should be OK", name)
		}
	}
}

func TestReadiness_AnyProbeFails503(t *testing.T) {
	h := Readiness(
		ProbeFunc{N: "good", F: func(_ context.Context) error { return nil }},
		ProbeFunc{N: "bad", F: func(_ context.Context) error { return errors.New("boom") }},
	)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("expected error message in body, got %s", rec.Body.String())
	}
}

func TestReadiness_NoProbesPass(t *testing.T) {
	// 无 probe 注册 → 直接 OK
	h := Readiness()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// 稳定性回归：probe 内 panic 不能让 Readiness handler 挂死。
// 之前 goroutine panic 时 channel 没被 send，主循环 <-ch 永远阻塞。
func TestReadiness_PanicInProbe_StillResponds(t *testing.T) {
	panicy := ProbeFunc{
		N: "panicy",
		F: func(_ context.Context) error {
			panic("simulated probe panic")
		},
	}
	healthy := ProbeFunc{
		N: "healthy",
		F: func(_ context.Context) error { return nil },
	}
	h := Readiness(panicy, healthy)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	done := make(chan struct{})
	go func() {
		h(rec, req)
		close(done)
	}()
	select {
	case <-done:
		// good - handler returned within reasonable time
	case <-time.After(5 * time.Second):
		t.Fatal("Readiness handler hung when probe panicked (regression: goroutine 没 recover)")
	}

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 (one probe panicked = fail), got %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "panicy") || !contains(body, "panic") {
		t.Errorf("response should mention the panicky probe, got: %s", body)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
