package safety

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIPLimiter_FirstAllowsBurst(t *testing.T) {
	l := NewIPLimiter(1, 5) // 1 rps, 5 burst
	for i := 0; i < 5; i++ {
		if !l.Allow("1.1.1.1") {
			t.Fatalf("burst %d should pass", i)
		}
	}
}

func TestIPLimiter_RejectsAfterBurst(t *testing.T) {
	l := NewIPLimiter(1, 3)
	for i := 0; i < 3; i++ {
		if !l.Allow("2.2.2.2") {
			t.Fatalf("burst %d should pass", i)
		}
	}
	// 第 4 次应该被拒（< 1 token，refill rate 1 rps 还没回填）
	if l.Allow("2.2.2.2") {
		t.Fatal("expected rate limited after burst exhausted")
	}
}

func TestIPLimiter_PerIPIsolation(t *testing.T) {
	l := NewIPLimiter(1, 1)
	if !l.Allow("a") || !l.Allow("b") {
		t.Fatal("first call per IP should pass")
	}
	if l.Allow("a") {
		t.Fatal("a should be limited")
	}
	if l.Allow("b") {
		t.Fatal("b should be limited")
	}
}

func TestIPLimiter_NilSafe(t *testing.T) {
	var l *IPLimiter
	if !l.Allow("x") {
		t.Fatal("nil limiter should allow all")
	}
	if l.Size() != 0 {
		t.Fatal("nil size should be 0")
	}
}

func TestIPRateLimit_Middleware(t *testing.T) {
	l := NewIPLimiter(0.0001, 1) // 几乎不补 token，第二次必拒
	mw := IPRateLimit(l)
	called := 0
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called++
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("first should pass; got %d", rr.Code)
	}

	// 第二个请求 → 应限流
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusTooManyRequests {
		t.Fatalf("second should be 429; got %d", rr2.Code)
	}
	if called != 1 {
		t.Fatalf("handler should be called once; got %d", called)
	}
}

func TestIPRateLimit_XForwardedForHeader(t *testing.T) {
	l := NewIPLimiter(0.0001, 1)
	mw := IPRateLimit(l)
	called := 0
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called++ }))

	// 两个不同 X-Forwarded-For（同 RemoteAddr） → 应该独立限
	req1 := httptest.NewRequest("GET", "/", nil)
	req1.RemoteAddr = "127.0.0.1:1"
	req1.Header.Set("X-Forwarded-For", "real_client_1")
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "127.0.0.1:1"
	req2.Header.Set("X-Forwarded-For", "real_client_2")

	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, req1)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr1.Code != 200 || rr2.Code != 200 {
		t.Fatalf("different XFF should be independently allowed; got %d %d", rr1.Code, rr2.Code)
	}
	if called != 2 {
		t.Fatalf("expected 2 handler calls; got %d", called)
	}
}

func TestIPRateLimit_NilLimiterPasses(t *testing.T) {
	mw := IPRateLimit(nil)
	called := false
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Fatal("nil limiter should be no-op")
	}
}

func TestClientIP_PriorityHeaders(t *testing.T) {
	tests := []struct {
		name string
		set  func(r *http.Request)
		want string
	}{
		{"xff", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "1.2.3.4") }, "1.2.3.4"},
		{"xff-multi", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1") }, "1.2.3.4"},
		{"x-real-ip", func(r *http.Request) { r.Header.Set("X-Real-IP", "5.6.7.8") }, "5.6.7.8"},
		{"remote-addr", func(r *http.Request) {}, "127.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			tt.set(req)
			if got := clientIP(req); got != tt.want {
				t.Errorf("clientIP=%q; want %q", got, tt.want)
			}
		})
	}
}
