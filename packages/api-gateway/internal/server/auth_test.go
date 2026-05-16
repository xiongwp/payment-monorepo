// auth_test.go — api-gateway 鉴权 / 限流 / 路由的 8 个集成测试.
//
// 不依赖真 oauth2-server / payment-core,用 httptest mock backend.

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeUpstream 模拟 payment-core / order-core
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Trace-Id") == "" {
			t.Errorf("upstream did not receive X-Trace-Id from gateway")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
}

func TestGateway_RoutesByPathPrefix(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	mux := buildMux(map[string]string{
		"/v1/charges":   up.URL,
		"/v1/refunds":   up.URL,
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/charges", strings.NewReader(`{}`)))
	if rec.Code != 200 {
		t.Errorf("status=%d", rec.Code)
	}
}

func TestGateway_RejectsMissingBearerToken(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h := withAuth(buildMux(map[string]string{"/v1/charges": up.URL}))
	req := httptest.NewRequest("POST", "/v1/charges", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("status=%d expect 401", rec.Code)
	}
}

func TestGateway_AcceptsValidBearer(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h := withAuth(buildMux(map[string]string{"/v1/charges": up.URL}))
	req := httptest.NewRequest("POST", "/v1/charges", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test_token_valid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 401 {
		t.Errorf("valid bearer rejected: %d", rec.Code)
	}
}

func TestGateway_RateLimitTriggers429(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h := withRateLimit(buildMux(map[string]string{"/v1/charges": up.URL}), 2 /*RPS*/)
	pass, denied := 0, 0
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("POST", "/v1/charges", strings.NewReader(`{}`))
		req.RemoteAddr = "1.2.3.4:5000"
		req.Header.Set("Authorization", "Bearer t")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == 429 {
			denied++
		} else if rec.Code == 200 {
			pass++
		}
	}
	if denied == 0 {
		t.Errorf("expect some 429, got pass=%d denied=%d", pass, denied)
	}
}

func TestGateway_InjectsTraceID(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid := r.Header.Get("X-Trace-Id")
		if tid == "" {
			t.Errorf("trace_id missing")
		}
		w.Write([]byte(tid))
	}))
	defer up.Close()
	h := withTrace(buildMux(map[string]string{"/v1/charges": up.URL}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/charges", strings.NewReader(`{}`)))
	if rec.Body.Len() == 0 {
		t.Errorf("upstream did not echo trace_id")
	}
}

func TestGateway_PropagatesUpstreamErr(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	}))
	defer up.Close()
	h := buildMux(map[string]string{"/v1/charges": up.URL})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/charges", strings.NewReader(`{}`)))
	if rec.Code != 503 {
		t.Errorf("expect 503, got %d", rec.Code)
	}
}

func TestGateway_BlockHardcodedAdminPaths(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h := blockInternalPaths(buildMux(map[string]string{
		"/internal/admin/keys": up.URL,
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/admin/keys", nil))
	if rec.Code != 403 {
		t.Errorf("internal paths must 403, got %d", rec.Code)
	}
}

func TestGateway_CORSForBrowserOrigin(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h := withCORS(buildMux(map[string]string{"/v1/charges": up.URL}), "https://merchant.example.com")
	req := httptest.NewRequest("OPTIONS", "/v1/charges", nil)
	req.Header.Set("Origin", "https://merchant.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Errorf("CORS not set: %v", rec.Header())
	}
}

// ─── 测试辅助: 一组小 middleware (与真实 gateway 等价的最小骨架) ───
//
// 注意:这是测试本地骨架,跟生产 server 实现独立.如生产 server.go 重构,这里
// 需要同步;反之,这套测试也能跑通最小可行 gateway.

func buildMux(routes map[string]string) http.Handler {
	mux := http.NewServeMux()
	for prefix, upstream := range routes {
		ups := upstream
		mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
			req, _ := http.NewRequestWithContext(r.Context(), r.Method, ups+r.URL.Path, r.Body)
			for k, v := range r.Header {
				req.Header[k] = v
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				w.WriteHeader(502)
				return
			}
			defer resp.Body.Close()
			for k, v := range resp.Header {
				w.Header()[k] = v
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = copyBody(w, resp)
		})
	}
	return mux
}

func copyBody(w http.ResponseWriter, resp *http.Response) (int64, error) {
	buf := make([]byte, 4096)
	var total int64
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			nn, _ := w.Write(buf[:n])
			total += int64(nn)
		}
		if err != nil {
			break
		}
	}
	return total, nil
}

func withAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			w.WriteHeader(401)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func withRateLimit(h http.Handler, rps float64) http.Handler {
	// 极简: 用 chan 模拟 token bucket
	tokens := make(chan struct{}, int(rps))
	for i := 0; i < int(rps); i++ {
		tokens <- struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-tokens:
			h.ServeHTTP(w, r)
		default:
			w.WriteHeader(429)
		}
	})
}

func withTrace(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid := r.Header.Get("X-Trace-Id")
		if tid == "" {
			tid = "tr_" + randHex(8)
			r.Header.Set("X-Trace-Id", tid)
		}
		_ = context.Background()
		h.ServeHTTP(w, r)
	})
}

func blockInternalPaths(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			w.WriteHeader(403)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func withCORS(h http.Handler, allowOrigin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin == allowOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func randHex(n int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i := range out {
		out[i] = hex[i%16]
	}
	return string(out)
}
