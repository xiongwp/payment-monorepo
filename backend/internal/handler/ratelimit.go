package handler

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// BodyLimitMiddleware caps request body size per content-type heuristic:
//   - JSON / form / text → 64 KB
//   - multipart upload (KYC doc metadata POST) → 2 MB
// Headers are capped separately by http.Server.MaxHeaderBytes.
//
// Exceeding the cap returns 413 Payload Too Large before any handler runs.
func BodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodDelete {
			next.ServeHTTP(w, r)
			return
		}
		ct := r.Header.Get("Content-Type")
		var max int64 = 64 * 1024
		if strings.HasPrefix(ct, "multipart/") {
			max = 2 * 1024 * 1024
		}
		r.Body = http.MaxBytesReader(w, r.Body, max)
		next.ServeHTTP(w, r)
	})
}

// RateLimitMiddleware enforces a global + per-client-IP rate limit. The global
// limiter protects the BFF from retry storms; the per-IP limiter prevents a
// single admin session from monopolizing capacity.
//
// Defaults (tuned for admin traffic, not payment hot path):
//   - Global: 200 req/s, burst 400
//   - Per-IP: 20 req/s, burst 40
// Override via env ADMIN_RATE_RPS / ADMIN_RATE_BURST / ADMIN_RATE_PER_IP_RPS.
//
// Never a "production" rate limiter — real payment APIs route through
// order-core/payment-core which have their own limits. This is admin-only.
func RateLimitMiddleware(globalRPS, globalBurst, perIPRPS, perIPBurst int) func(http.Handler) http.Handler {
	if globalRPS <= 0 {
		globalRPS = 200
	}
	if globalBurst <= 0 {
		globalBurst = 400
	}
	if perIPRPS <= 0 {
		perIPRPS = 20
	}
	if perIPBurst <= 0 {
		perIPBurst = 40
	}
	global := rate.NewLimiter(rate.Limit(globalRPS), globalBurst)
	ips := &ipLimiterStore{
		m:       map[string]*ipEntry{},
		rps:     perIPRPS,
		burst:   perIPBurst,
		maxIdle: 10 * time.Minute,
	}
	go ips.gcLoop()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !global.Allow() {
				http.Error(w, "global rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			ip := clientIPFromReq(r)
			if !ips.allow(ip) {
				http.Error(w, "per-IP rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type ipEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

type ipLimiterStore struct {
	mu      sync.Mutex
	m       map[string]*ipEntry
	rps     int
	burst   int
	maxIdle time.Duration
}

func (s *ipLimiterStore) allow(ip string) bool {
	s.mu.Lock()
	e, ok := s.m[ip]
	if !ok {
		e = &ipEntry{lim: rate.NewLimiter(rate.Limit(s.rps), s.burst)}
		s.m[ip] = e
	}
	e.seen = time.Now()
	s.mu.Unlock()
	return e.lim.Allow()
}

// gcLoop evicts per-IP limiters that haven't been touched for maxIdle so the
// map doesn't grow unbounded under scan traffic.
func (s *ipLimiterStore) gcLoop() {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-s.maxIdle)
		s.mu.Lock()
		for ip, e := range s.m {
			if e.seen.Before(cutoff) {
				delete(s.m, ip)
			}
		}
		n := len(s.m)
		s.mu.Unlock()
		if n > 10000 {
			log.Printf("warn: per-IP limiter map size=%d (not pruned aggressively enough?)", n)
		}
	}
}

func clientIPFromReq(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	return r.RemoteAddr
}
