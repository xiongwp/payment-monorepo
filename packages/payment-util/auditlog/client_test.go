package auditlog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestClient_BatchesAndSends(t *testing.T) {
	var received int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(Config{
		BaseURL:       ts.URL,
		Service:       "test-svc",
		BatchSize:     3,
		FlushInterval: 50 * time.Millisecond,
		BufferSize:    100,
	}, zap.NewNop())
	defer c.Stop()

	for i := 0; i < 5; i++ {
		_ = c.Emit(context.Background(), Event{
			Action:       "test_action",
			ResourceType: "test",
			ResourceID:   "id_x",
		})
	}

	// 等 flush
	time.Sleep(200 * time.Millisecond)
	if atomic.LoadInt64(&received) != 5 {
		t.Errorf("want 5 received, got %d", atomic.LoadInt64(&received))
	}
}

func TestClient_DropsWhenBufferFull(t *testing.T) {
	// 服务端慢响应, 阻塞 worker
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(Config{
		BaseURL:       ts.URL,
		Service:       "test-svc",
		BatchSize:     100,
		FlushInterval: 1 * time.Second,
		BufferSize:    2,
	}, zap.NewNop())
	defer c.Stop()

	for i := 0; i < 20; i++ {
		_ = c.Emit(context.Background(), Event{Action: "spam"})
	}
	time.Sleep(10 * time.Millisecond)
	if c.Dropped() == 0 {
		t.Error("expected at least some events dropped")
	}
}

func TestClient_StopFlushesPendingEvents(t *testing.T) {
	var received int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(Config{
		BaseURL:       ts.URL,
		Service:       "test-svc",
		BatchSize:     100,
		FlushInterval: 1 * time.Hour, // 不会触发 ticker
		BufferSize:    100,
	}, zap.NewNop())

	for i := 0; i < 5; i++ {
		_ = c.Emit(context.Background(), Event{Action: "test"})
	}
	c.Stop() // 应当 drain
	if atomic.LoadInt64(&received) != 5 {
		t.Errorf("after stop, want 5 received, got %d", atomic.LoadInt64(&received))
	}
}
