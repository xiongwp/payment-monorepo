package auditmw

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingClient struct {
	mu      sync.Mutex
	count   int32
	last    []byte
}

func (r *recordingClient) Do(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&r.count, 1)
	r.mu.Lock()
	b, _ := io.ReadAll(req.Body)
	r.last = b
	r.mu.Unlock()
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader(nil)),
	}, nil
}

func TestHTTP_PostTriggersAudit(t *testing.T) {
	rc := &recordingClient{}
	h := HTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	}), Opts{
		Service:  "biz-admin-web",
		AuditURL: "http://fake",
		Client:   rc,
	})
	req := httptest.NewRequest("POST", "/admin/orders/123/refund", nil)
	req.Header.Set("X-Actor-Email", "alice@example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	// 异步发,等几 ms
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&rc.count) == 0 {
		t.Fatal("expect audit call")
	}
	if !bytes.Contains(rc.last, []byte("alice@example.com")) {
		t.Errorf("actor not in body: %s", string(rc.last))
	}
}

func TestHTTP_GetSkippedWhenMutatingOnly(t *testing.T) {
	rc := &recordingClient{}
	h := HTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
		Opts{Service: "x", AuditURL: "http://fake", Client: rc, MutatingOnly: true})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))
	time.Sleep(20 * time.Millisecond)
	if atomic.LoadInt32(&rc.count) != 0 {
		t.Fatal("GET should be skipped under MutatingOnly")
	}
}

func TestHTTP_CapturesStatus(t *testing.T) {
	rc := &recordingClient{}
	h := HTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(409)
	}), Opts{Service: "x", AuditURL: "http://fake", Client: rc})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("DELETE", "/x", nil))
	time.Sleep(30 * time.Millisecond)
	if !bytes.Contains(rc.last, []byte(`"status":409`)) {
		t.Errorf("status not 409 in audit: %s", string(rc.last))
	}
}
