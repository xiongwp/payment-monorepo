package reliability

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func newAdminServer(t *testing.T, lim *AdaptiveLimiter) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	RegisterAdaptiveAdmin(mux, lim, zap.NewNop())
	return httptest.NewServer(mux)
}

func TestAdmin_GetAllAndSingle(t *testing.T) {
	lim := newTestLimiter(10, 5, 50, true)
	lim.stateFor("m1")
	lim.stateFor("m2")
	srv := newAdminServer(t, lim)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/reliability/limits")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("get all: %v %v", err, resp)
	}
	var all allSnapshotResp
	json.NewDecoder(resp.Body).Decode(&all)
	resp.Body.Close()
	if len(all.Merchants) != 2 {
		t.Fatalf("expected 2 merchants, got %d", len(all.Merchants))
	}
	if !all.Enabled {
		t.Fatal("expected enabled=true")
	}

	resp, _ = http.Get(srv.URL + "/admin/reliability/limits/m1")
	if resp.StatusCode != 200 {
		t.Fatalf("get m1: status %d", resp.StatusCode)
	}
	var snap MerchantSnapshot
	json.NewDecoder(resp.Body).Decode(&snap)
	resp.Body.Close()
	if snap.MerchantID != "m1" || snap.Limit != 10 {
		t.Fatalf("m1 snap wrong: %+v", snap)
	}

	resp, _ = http.Get(srv.URL + "/admin/reliability/limits/no_such")
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdmin_SetLimit(t *testing.T) {
	lim := newTestLimiter(10, 5, 50, true)
	srv := newAdminServer(t, lim)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/reliability/limits/m1",
		"application/json", strings.NewReader(`{"limit":25}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("set limit: %v code=%d", err, resp.StatusCode)
	}
	resp.Body.Close()
	snap := findMerchant(lim, "m1")
	if snap == nil || snap.Limit != 25 {
		t.Fatalf("expected limit=25 after POST, got %+v", snap)
	}

	// bad json
	resp, _ = http.Post(srv.URL+"/admin/reliability/limits/m1",
		"application/json", strings.NewReader(`xxx`))
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for bad json, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// zero limit invalid
	resp, _ = http.Post(srv.URL+"/admin/reliability/limits/m1",
		"application/json", strings.NewReader(`{"limit":0}`))
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for limit=0, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdmin_EnableToggle(t *testing.T) {
	lim := newTestLimiter(10, 5, 50, false)
	srv := newAdminServer(t, lim)
	defer srv.Close()

	if lim.Enabled() {
		t.Fatal("precondition: should be disabled")
	}
	resp, err := http.Post(srv.URL+"/admin/reliability/limits/enable",
		"application/json", strings.NewReader(`{"enabled":true}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("enable: %v code=%d", err, resp.StatusCode)
	}
	resp.Body.Close()
	if !lim.Enabled() {
		t.Fatal("expected enabled=true after POST")
	}

	// query string fallback
	resp, _ = http.Post(srv.URL+"/admin/reliability/limits/enable?on=0",
		"application/json", strings.NewReader(``))
	if resp.StatusCode != 200 {
		t.Fatalf("toggle off via query: code=%d", resp.StatusCode)
	}
	resp.Body.Close()
	if lim.Enabled() {
		t.Fatal("expected disabled after query=0")
	}
}

func TestAdmin_Reset(t *testing.T) {
	lim := newTestLimiter(10, 5, 50, true)
	lim.SetLimit("m1", 30)
	resp, _ := http.Post(httpURL(t, lim, "/admin/reliability/limits/reset"),
		"application/json", strings.NewReader(``))
	if resp.StatusCode != 200 {
		t.Fatalf("reset: code=%d", resp.StatusCode)
	}
	resp.Body.Close()
	snap := findMerchant(lim, "m1")
	if snap == nil || snap.Limit != 10 {
		t.Fatalf("after reset expected limit=10, got %+v", snap)
	}
}

// httpURL helper：起一个临时 test server 给单测复用。
func httpURL(t *testing.T, lim *AdaptiveLimiter, path string) string {
	srv := newAdminServer(t, lim)
	t.Cleanup(srv.Close)
	return srv.URL + path
}
