// introspect_cache.go — 资源服务端可选 introspect 缓存。
//
// 用途: 让本地 JWT 验签 + 远端 revocation 检查能并存:
//   1. 本地 RS256 验签 (JWTVerifier) — 99% 请求只走这一步
//   2. 每 ~30s 异步查 /oauth2/introspect, 把 active=false 加本地黑名单
//   3. 黑名单命中 → 401 (revoked)
//
// 比每个请求 introspect 快得多 (~1µs vs ~5ms)，但牺牲 ~30s revocation 传播延迟。
//
// 用法:
//
//   ic := mw.NewIntrospectCache(mw.IntrospectCacheConfig{
//       Endpoint: "http://oauth2-server:8087/oauth2/introspect",
//       Refresh:  30 * time.Second,
//       TTL:      5 * time.Minute,
//   })
//
//   // 接进 Auth handler:
//   cfg := mw.AuthConfig{
//       BearerJWT: jwtVerifier,
//       RevocationCheck: ic.IsRevoked,
//   }

package mw

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// IntrospectCacheConfig 配置。
type IntrospectCacheConfig struct {
	Endpoint string        // /oauth2/introspect
	ClientID string        // 调 introspect 自己也是 OAuth client
	ClientSecret string
	HTTP     *http.Client
	TTL      time.Duration // 黑名单 entry 多久后过期 (默认 5min)
	Log      *zap.Logger
}

// IntrospectCache 异步 revocation 检查器。
type IntrospectCache struct {
	cfg     IntrospectCacheConfig
	mu      sync.RWMutex
	revoked map[string]time.Time // jti → expire-at
}

// NewIntrospectCache 构造。
func NewIntrospectCache(cfg IntrospectCacheConfig) *IntrospectCache {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 3 * time.Second}
	}
	if cfg.TTL == 0 {
		cfg.TTL = 5 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = zap.NewNop()
	}
	ic := &IntrospectCache{cfg: cfg, revoked: map[string]time.Time{}}
	go ic.gcLoop()
	return ic
}

// CheckAndCache 异步查 introspect — 如果 active=false 加黑名单。
//
// 一般做法: 业务里的 Auth middleware 先 JWT 验签拿到 claims，
// 调 go ic.CheckAndCache(jti, token) (异步, 不阻塞), 同时 ic.IsRevoked(jti) 同步查。
func (ic *IntrospectCache) CheckAndCache(jti, token string) {
	if jti == "" {
		return
	}
	form := url.Values{}
	form.Set("token", token)
	if ic.cfg.ClientID != "" {
		form.Set("client_id", ic.cfg.ClientID)
		form.Set("client_secret", ic.cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ic.cfg.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := ic.cfg.HTTP.Do(req)
	if err != nil {
		ic.cfg.Log.Debug("introspect call failed", zap.Error(err))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var ir struct{ Active bool `json:"active"` }
	if err := json.Unmarshal(body, &ir); err != nil {
		return
	}
	if !ir.Active {
		ic.mu.Lock()
		ic.revoked[jti] = time.Now().Add(ic.cfg.TTL)
		ic.mu.Unlock()
	}
}

// IsRevoked 查 jti 是否在本地黑名单 (revoked 不一定包含全部 revoke 事件;
// 它是 introspect 延迟传播过来的黑名单)。
func (ic *IntrospectCache) IsRevoked(jti string) bool {
	if jti == "" {
		return false
	}
	ic.mu.RLock()
	exp, ok := ic.revoked[jti]
	ic.mu.RUnlock()
	if !ok {
		return false
	}
	return time.Now().Before(exp)
}

func (ic *IntrospectCache) gcLoop() {
	t := time.NewTicker(1 * time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		ic.mu.Lock()
		for jti, exp := range ic.revoked {
			if now.After(exp) {
				delete(ic.revoked, jti)
			}
		}
		ic.mu.Unlock()
	}
}

// Size 当前黑名单 entry 数 (监控用)。
func (ic *IntrospectCache) Size() int {
	ic.mu.RLock()
	defer ic.mu.RUnlock()
	return len(ic.revoked)
}
