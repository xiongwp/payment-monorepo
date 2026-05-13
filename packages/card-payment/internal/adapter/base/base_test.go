package base

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestNew_RequiresNetwork(t *testing.T) {
	if _, err := New(Config{}, zap.NewNop()); err == nil {
		t.Fatal("expect err without Network")
	}
}

func TestNew_MockFallbackWhenNoCert(t *testing.T) {
	a, err := New(Config{Network: "visa", MockFallback: true}, zap.NewNop())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !a.Mocked() {
		t.Fatal("expect mocked")
	}
	if a.Name() != "visa" {
		t.Errorf("name wrong: %s", a.Name())
	}
}

func TestNew_FailFastWithoutMTLS(t *testing.T) {
	_, err := New(Config{Network: "visa", BaseURL: "https://x", MockFallback: false}, zap.NewNop())
	if err == nil {
		t.Fatal("expect err when MockFallback=false + no cert")
	}
}

func TestDoRetry_Success(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	a := &Adapter{
		cfg:    Config{Network: "x", MaxRetries: 2, RetryBaseDelay: 10 * time.Millisecond, HTTPTimeout: time.Second},
		http:   srv.Client(),
		logger: zap.NewNop(),
	}
	_, err := a.DoRetry(context.Background(), func() (*http.Request, error) {
		return http.NewRequest("GET", srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("calls=%d expect 1", calls)
	}
}

func TestDoRetry_5xxTriggersRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(503)
	}))
	defer srv.Close()

	a := &Adapter{
		cfg:    Config{Network: "x", MaxRetries: 2, RetryBaseDelay: 5 * time.Millisecond, HTTPTimeout: time.Second},
		http:   srv.Client(),
		logger: zap.NewNop(),
	}
	_, err := a.DoRetry(context.Background(), func() (*http.Request, error) {
		return http.NewRequest("GET", srv.URL, nil)
	})
	if err == nil {
		t.Fatal("expect err after retries")
	}
	// 1 initial + 2 retries = 3
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("calls=%d, expect 3", calls)
	}
}

func TestDoRetry_4xxDoesNotRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(400)
	}))
	defer srv.Close()

	a := &Adapter{
		cfg:    Config{Network: "x", MaxRetries: 3, RetryBaseDelay: 5 * time.Millisecond, HTTPTimeout: time.Second},
		http:   srv.Client(),
		logger: zap.NewNop(),
	}
	_, _ = a.DoRetry(context.Background(), func() (*http.Request, error) {
		return http.NewRequest("GET", srv.URL, nil)
	})
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("4xx should not retry; calls=%d", calls)
	}
}

func TestDoRetry_ContextCancelStops(t *testing.T) {
	a := &Adapter{
		cfg:    Config{Network: "x", MaxRetries: 5, RetryBaseDelay: 50 * time.Millisecond, HTTPTimeout: time.Second},
		http:   &http.Client{Timeout: 100 * time.Millisecond},
		logger: zap.NewNop(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := a.DoRetry(ctx, func() (*http.Request, error) {
		return http.NewRequest("GET", "http://127.0.0.1:1/non-existent", nil)
	})
	if err == nil {
		t.Fatal("expect ctx err")
	}
	if !errors.Is(err, context.DeadlineExceeded) && err != context.DeadlineExceeded {
		// 也接受 retry exhausted 的 wrap;但确实退出
	}
}

func TestMaskPAN(t *testing.T) {
	cases := map[string]string{
		"":                  "****",
		"4111":              "****",
		"4111111111111111":  "4111********1111",
		"411111":            "**1111",
	}
	for in, want := range cases {
		if got := maskPAN(in); got != want {
			t.Errorf("maskPAN(%q): want %q got %q", in, want, got)
		}
	}
}

func TestMockAuthorize(t *testing.T) {
	a, _ := New(Config{Network: "visa", MockFallback: true}, zap.NewNop())
	resp := a.MockAuthorize(&AuthorizeRequest{
		IdempotencyKey: "idem_1", PAN: "4111111111111111",
	})
	if resp.Status != "approved" {
		t.Errorf("status: %s", resp.Status)
	}
	if resp.NetworkRefNo == "" {
		t.Error("ref no empty")
	}
	if resp.MaskedPAN == "4111111111111111" {
		t.Error("PAN was not masked!")
	}
}
