package paymentmw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseMode(t *testing.T) {
	cases := []struct {
		clientID string
		want     Mode
	}{
		{"pk_test_abc123", ModeTest},
		{"pk_live_xyz456", ModeLive},
		{"svc_test_billing", ModeTest},
		{"svc_live_billing", ModeLive},
		{"ops_test_alice", ModeTest},
		{"ops_live_alice", ModeLive},
		{"legacy_id_no_prefix", ModeLive}, // 老 ID 兜底 live
		{"", ModeLive},
	}
	for _, c := range cases {
		got := ParseMode(c.clientID)
		if got != c.want {
			t.Errorf("ParseMode(%q) = %s, want %s", c.clientID, got, c.want)
		}
	}
}

func TestCtxRoundTrip(t *testing.T) {
	ctx := WithMode(context.Background(), ModeTest)
	if ModeFromCtx(ctx) != ModeTest {
		t.Error("ctx mode round-trip failed")
	}
	if !IsTest(ctx) {
		t.Error("IsTest false for test mode")
	}
}

func TestEnforceLiveOnly_BlocksTest(t *testing.T) {
	handler := EnforceLiveOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/admin/anything", nil)
	req = req.WithContext(WithMode(req.Context(), ModeTest))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("test key on live-only got %d, want 403", rec.Code)
	}
}

func TestEnforceLiveOnly_AllowsLive(t *testing.T) {
	handler := EnforceLiveOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/admin/anything", nil)
	req = req.WithContext(WithMode(req.Context(), ModeLive))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("live key got %d, want 200", rec.Code)
	}
}

func TestSelectBackend(t *testing.T) {
	opt := RouteByModeOpt{LiveBaseURL: "http://live", TestBaseURL: "http://mock"}
	testCtx := WithMode(context.Background(), ModeTest)
	if got := SelectBackend(testCtx, opt); got != "http://mock" {
		t.Errorf("test ctx → %s, want mock", got)
	}
	liveCtx := WithMode(context.Background(), ModeLive)
	if got := SelectBackend(liveCtx, opt); got != "http://live" {
		t.Errorf("live ctx → %s, want live", got)
	}
}
