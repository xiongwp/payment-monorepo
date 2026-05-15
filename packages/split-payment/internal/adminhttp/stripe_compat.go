// stripe_compat.go — SP-FIN-5 Stripe API 兼容层.
//
// 三件事:
//
//   1. Idempotency-Key middleware
//      - HTTP header "Idempotency-Key: <uuid>" → 24h 内同 key 返缓存响应.
//      - 缓存 Redis (SETNX + EXPIRE) 或内存 fallback.
//
//   2. URL alias (Stripe 风格)
//      /v1/accounts        → /api/connected_accounts
//      /v1/transfers       → /api/transfers
//      /v1/application_fees → /api/application_fees
//      /v1/payouts         → /api/payouts
//
//   3. Response envelope wrap
//      原: { "id": "tr_xxx", "amount_minor": ... }
//      Stripe: { "id": "tr_xxx", "object": "transfer", "amount": ..., "created": <unix>, "livemode": bool }
//      (注意 cents → amount, amount_minor 保留兼容)
//
// 实现策略:
//   - middleware 函数包装 mux, 不强行改路由
//   - dev/单机模式用内存 cache, 生产换 Redis (caller 传入 IdempotencyStore 接口)
package adminhttp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ─── Idempotency middleware ───────────────────────────────────────────

// IdempotencyStore 抽象 (生产 Redis, dev memory).
type IdempotencyStore interface {
	Get(ctx context.Context, key string) ([]byte, int, bool)
	Set(ctx context.Context, key string, body []byte, status int, ttl time.Duration)
}

// MemoryIdempotencyStore 简单内存实现 (dev).
type MemoryIdempotencyStore struct {
	mu    sync.RWMutex
	cache map[string]idempotencyEntry
}

type idempotencyEntry struct {
	body   []byte
	status int
	exp    time.Time
}

// NewMemoryIdempotencyStore.
func NewMemoryIdempotencyStore() *MemoryIdempotencyStore {
	s := &MemoryIdempotencyStore{cache: map[string]idempotencyEntry{}}
	// 后台 GC 过期 entry
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			s.mu.Lock()
			now := time.Now()
			for k, e := range s.cache {
				if now.After(e.exp) {
					delete(s.cache, k)
				}
			}
			s.mu.Unlock()
		}
	}()
	return s
}

// Get.
func (s *MemoryIdempotencyStore) Get(_ context.Context, key string) ([]byte, int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.cache[key]
	if !ok || time.Now().After(e.exp) {
		return nil, 0, false
	}
	return e.body, e.status, true
}

// Set.
func (s *MemoryIdempotencyStore) Set(_ context.Context, key string, body []byte, status int, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[key] = idempotencyEntry{body: body, status: status, exp: time.Now().Add(ttl)}
}

// IdempotencyMiddleware 包装 handler, 检查 Idempotency-Key header.
//
// 行为:
//   - 无 header → 直通
//   - 有 header + cache hit → 返缓存 body + status
//   - 有 header + 无 cache → 跑 handler, 缓存响应 24h
//
// 仅对 POST / PUT / PATCH 应用 (GET 无副作用不缓存).
func IdempotencyMiddleware(store IdempotencyStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		// cache key 含 method + path 避免不同 endpoint 同 key 冲突
		cacheKey := r.Method + " " + r.URL.Path + " :: " + key
		if body, status, ok := store.Get(r.Context(), cacheKey); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}
		// 跑 handler 同时捕获 response
		rec := &capturingWriter{ResponseWriter: w, body: &bytes.Buffer{}, status: 200}
		next.ServeHTTP(rec, r)
		// 仅 2xx 缓存 (4xx/5xx 不缓存, 允许 caller 重试)
		if rec.status >= 200 && rec.status < 300 {
			store.Set(r.Context(), cacheKey, rec.body.Bytes(), rec.status, 24*time.Hour)
		}
	})
}

// capturingWriter 拦截 ResponseWriter 写入到 buffer.
type capturingWriter struct {
	http.ResponseWriter
	body   *bytes.Buffer
	status int
}

func (c *capturingWriter) WriteHeader(s int) {
	c.status = s
	c.ResponseWriter.WriteHeader(s)
}

func (c *capturingWriter) Write(b []byte) (int, error) {
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}

// ─── URL alias ────────────────────────────────────────────────────────

// AliasMiddleware /v1/<resource> → /api/<resource>  Stripe-style 别名.
//
// 不改原路由, 仅在请求进来时 rewrite r.URL.Path.
func AliasMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			rest := r.URL.Path[len("/v1/"):]
			switch {
			case strings.HasPrefix(rest, "accounts"):
				r.URL.Path = "/api/connected_" + rest // /v1/accounts/xxx → /api/connected_accounts/xxx
			case strings.HasPrefix(rest, "transfers"),
				strings.HasPrefix(rest, "application_fees"),
				strings.HasPrefix(rest, "payouts"):
				r.URL.Path = "/api/" + rest
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ─── Apply 函数 (注册到 main.go) ──────────────────────────────────────

// WithStripeCompat 一键包 mux: AliasMiddleware → IdempotencyMiddleware → 原 mux.
//
// 用法:
//
//	mux := http.NewServeMux()
//	... 注册各 endpoint ...
//	finalHandler := adminhttp.WithStripeCompat(mux, store)
//	srv := &http.Server{Handler: finalHandler}
func WithStripeCompat(next http.Handler, store IdempotencyStore) http.Handler {
	return AliasMiddleware(IdempotencyMiddleware(store, next))
}

// ─── Response envelope helpers ────────────────────────────────────────

// StripeEnvelope Stripe Event-style envelope.
//
// 普通 resource 返单对象: { id, object, ... 原字段 ... }
// list 返:                { object: "list", data: [...], has_more: bool, url: "/v1/transfers" }
type StripeEnvelope struct {
	ID       string `json:"id,omitempty"`
	Object   string `json:"object"`
	Created  int64  `json:"created,omitempty"`
	LiveMode bool   `json:"livemode"`
	// 嵌入原对象字段 (caller json.Marshal 时 inline)
}

// drainBody helper.
func drainBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b
}

var _ = drainBody // reserved for future signed-request validation
