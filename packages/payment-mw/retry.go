// retry.go — 出站 HTTP client 重试 + 退避 + bulkhead helper。
//
// 给跨服务调用用（refund-engine → merchant-webhook, billing → audit-log 等）。
// 包含:
//   - DoWithRetry: 重试 + 指数退避 + jitter
//   - BulkheadClient: 每个 upstream 一个独立 *http.Client 池，
//                     防止一个慢上游耗尽全局连接池
//
// 重试规则:
//   - 5xx + 429: 重试
//   - 408 Request Timeout: 重试
//   - 网络错: 重试
//   - 4xx (非 408/429): 不重试（业务错）
//   - context 取消: 不重试
//
// 重试间隔: 100ms / 250ms / 600ms / 1.5s / 3s (5 次)，每次 +- 20% jitter

package mw

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"
)

// RetryConfig 重试配置。
type RetryConfig struct {
	MaxAttempts int           // 默认 5
	BaseDelay   time.Duration // 默认 100ms
	MaxDelay    time.Duration // 默认 5s
	Jitter      float64       // 默认 0.2 (±20%)
}

// DefaultRetryConfig 安全默认。
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{MaxAttempts: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: 5 * time.Second, Jitter: 0.2}
}

// DoWithRetry 调用 client.Do 带自动重试。
//
// 注意：body 必须可重读 (用 bytes.NewReader 之类的 ReadSeeker)。
// 一次性 Reader 重试会失败（无法 seek 回 0）。
func DoWithRetry(ctx context.Context, client *http.Client, req *http.Request, cfg RetryConfig) (*http.Response, error) {
	if cfg.MaxAttempts == 0 {
		cfg = DefaultRetryConfig()
	}
	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		// 每次重试用同样的 req，但重置 body 位置（如果是 ReadSeeker）
		if req.Body != nil {
			if seeker, ok := req.Body.(io.Seeker); ok {
				seeker.Seek(0, io.SeekStart)
			}
		}
		resp, err := client.Do(req.WithContext(ctx))
		if err == nil {
			if !shouldRetryStatus(resp.StatusCode) {
				return resp, nil
			}
			// 5xx / 429 / 408 — 关 body 准备重试
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = errFromStatus(resp.StatusCode)
		} else {
			lastErr = err
			if !shouldRetryErr(err) {
				return nil, err
			}
		}
		// 最后一次失败就退出
		if attempt == cfg.MaxAttempts {
			break
		}
		// 算 backoff
		delay := computeBackoff(attempt, cfg)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

func shouldRetryStatus(code int) bool {
	switch code {
	case 408, 429:
		return true
	}
	return code >= 500
}

func shouldRetryErr(err error) bool {
	if err == nil {
		return false
	}
	// context 取消不重试
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// 网络错（dial / timeout）→ 重试
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false
}

type httpErr struct{ status int }

func (e httpErr) Error() string { return http.StatusText(e.status) }

func errFromStatus(code int) error { return httpErr{status: code} }

// computeBackoff 指数退避 + jitter: base × 2^(attempt-1) ± jitter%
func computeBackoff(attempt int, cfg RetryConfig) time.Duration {
	base := float64(cfg.BaseDelay) * float64(int(1)<<uint(attempt-1))
	if cfg.MaxDelay > 0 && time.Duration(base) > cfg.MaxDelay {
		base = float64(cfg.MaxDelay)
	}
	jit := cfg.Jitter
	if jit > 0 {
		base *= 1 + (rand.Float64()*2-1)*jit
	}
	return time.Duration(base)
}

// ─── Bulkhead — 每 upstream 独立 client pool ─────────────────────────

// BulkheadPool 给 upstream 配独立 http.Client（独立 connection pool）。
//
// 防止某个慢上游耗尽 default *http.Transport 的连接 → 拖死整个服务。
type BulkheadPool struct {
	mu      sync.RWMutex
	clients map[string]*http.Client
	defaults BulkheadDefaults
}

// BulkheadDefaults 默认 pool 参数。
type BulkheadDefaults struct {
	MaxIdleConns        int
	MaxConnsPerHost     int
	IdleConnTimeout     time.Duration
	ResponseHeaderTimeout time.Duration
	Timeout             time.Duration
}

// NewBulkhead 构造。
func NewBulkhead(defaults BulkheadDefaults) *BulkheadPool {
	if defaults.MaxIdleConns == 0 {
		defaults.MaxIdleConns = 100
	}
	if defaults.MaxConnsPerHost == 0 {
		defaults.MaxConnsPerHost = 50
	}
	if defaults.IdleConnTimeout == 0 {
		defaults.IdleConnTimeout = 90 * time.Second
	}
	if defaults.ResponseHeaderTimeout == 0 {
		defaults.ResponseHeaderTimeout = 10 * time.Second
	}
	if defaults.Timeout == 0 {
		defaults.Timeout = 30 * time.Second
	}
	return &BulkheadPool{
		clients:  map[string]*http.Client{},
		defaults: defaults,
	}
}

// For 拿某个 upstream 的专用 client（第一次访问时建）。
func (b *BulkheadPool) For(upstream string) *http.Client {
	b.mu.RLock()
	if c, ok := b.clients[upstream]; ok {
		b.mu.RUnlock()
		return c
	}
	b.mu.RUnlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	// double-check
	if c, ok := b.clients[upstream]; ok {
		return c
	}
	tr := &http.Transport{
		MaxIdleConns:          b.defaults.MaxIdleConns,
		MaxConnsPerHost:       b.defaults.MaxConnsPerHost,
		IdleConnTimeout:       b.defaults.IdleConnTimeout,
		ResponseHeaderTimeout: b.defaults.ResponseHeaderTimeout,
	}
	c := &http.Client{Transport: tr, Timeout: b.defaults.Timeout}
	b.clients[upstream] = c
	return c
}

// Close 关闭所有 idle conn — graceful shutdown 时调。
func (b *BulkheadPool) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.clients {
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
}
