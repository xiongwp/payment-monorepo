// email_reputation.go: 接外部 email 信誉服务（emailrep.io / Hunter.io / 自建）的规则。
//
// 调用约定：
//   POST/GET <endpoint>?email=<addr>
//   返 JSON: {"score":0-100,"suspicious":bool,"disposable":bool,"new_domain":bool,...}
//
// 规则按 score / suspicious / disposable 任一字段判定。
//
// 集成模式：
//   - 不强制接外部服务（commercial 服务有月费 / 配额）
//   - endpoint 空 = 规则永远 no-op（dev / 没接入时 fail-open）
//   - 缓存 5 分钟（每邮箱）；防一笔交易里同邮箱多次 evaluate 重复打外部 API
//   - 超时 1s + 熔断 fail-open；外部 API 挂掉不阻塞 Screen 主路径
//
// 配置：
//
//	type: email_reputation
//	config:
//	  endpoint:    "https://api.example.com/email/lookup"
//	  api_key:     "..."
//	  min_score:   30      # 信誉分 < 此值 → 命中
//	  fail_open:   true    # 外部失败时 → ALLOW（默认）vs REVIEW
//	  decision:    review  # 命中后判 review / deny
//
// caller 在 metadata.email 或 RegisterInput.Email 里塞邮箱地址（caller 责任 PII 脱敏：
// 本规则只看 fully-qualified email 做 lookup，其他不留）。
package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xiongwp/risk-manage/internal/engine"
)

type EmailReputationConfig struct {
	Endpoint   string `json:"endpoint"`
	APIKey     string `json:"api_key"`
	MinScore   int    `json:"min_score"`
	FailOpen   bool   `json:"fail_open"`
	Decision   string `json:"decision"`
	TimeoutMs  int    `json:"timeout_ms"`  // 默认 1000
	CacheSec   int    `json:"cache_sec"`   // 默认 300（5min）
}

type emailRepResult struct {
	Score      int  `json:"score"`
	Suspicious bool `json:"suspicious"`
	Disposable bool `json:"disposable"`
	NewDomain  bool `json:"new_domain"`
}

type emailRepCacheEntry struct {
	res     *emailRepResult
	err     bool // err=true 表示 lookup 失败（短缓存避免重复打）
	expires time.Time
}

type emailReputationRule struct {
	id, name string
	enabled  bool
	cfg      EmailReputationConfig
	verdict  engine.Decision

	client *http.Client

	mu    sync.Mutex
	cache map[string]emailRepCacheEntry
}

func EmailReputationFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg EmailReputationConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("email_reputation: %w", err)
			}
		}
		if cfg.MinScore <= 0 {
			cfg.MinScore = 30
		}
		if cfg.TimeoutMs <= 0 {
			cfg.TimeoutMs = 1000
		}
		if cfg.CacheSec <= 0 {
			cfg.CacheSec = 300
		}
		v := engine.Review
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "deny") {
			v = engine.Deny
		}
		return &emailReputationRule{
			id: id, name: name, enabled: enabled, cfg: cfg, verdict: v,
			client: &http.Client{Timeout: time.Duration(cfg.TimeoutMs) * time.Millisecond},
			cache:  make(map[string]emailRepCacheEntry, 256),
		}, nil
	}
}

func (r *emailReputationRule) ID() string    { return r.id }
func (r *emailReputationRule) Name() string  { return r.name }
func (r *emailReputationRule) Type() string  { return "email_reputation" }
func (r *emailReputationRule) Enabled() bool { return r.enabled }

func (r *emailReputationRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.cfg.Endpoint == "" || txn == nil || txn.Metadata == nil {
		return nil
	}
	email := strings.ToLower(strings.TrimSpace(txn.Metadata["email"]))
	if email == "" || !strings.Contains(email, "@") {
		return nil
	}
	res, lookupErr := r.lookup(ctx, email)
	if lookupErr {
		if r.cfg.FailOpen {
			return nil
		}
		return &engine.Hit{
			RuleID: r.id, RuleName: r.name, Decision: engine.Review,
			Detail: fmt.Sprintf("email reputation lookup failed (fail_open=false) for %q", email),
		}
	}
	// 任意可疑信号触发：score<min OR disposable OR suspicious
	if res.Score < r.cfg.MinScore || res.Suspicious || res.Disposable {
		return &engine.Hit{
			RuleID: r.id, RuleName: r.name, Decision: r.verdict,
			Detail: fmt.Sprintf("email %q reputation score=%d suspicious=%v disposable=%v",
				email, res.Score, res.Suspicious, res.Disposable),
		}
	}
	return nil
}

// lookup 拿外部 reputation；带 5min 缓存（成功 + 失败都缓存，失败缓存短一些避免
// 拖死外部 API quota）。返回 (result, isError)；isError=true 时 result 可空。
func (r *emailReputationRule) lookup(ctx context.Context, email string) (*emailRepResult, bool) {
	r.mu.Lock()
	if e, ok := r.cache[email]; ok && time.Now().Before(e.expires) {
		r.mu.Unlock()
		return e.res, e.err
	}
	r.mu.Unlock()

	q := url.Values{}
	q.Set("email", email)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.cfg.Endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return r.cacheError(email)
	}
	if r.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return r.cacheError(email)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return r.cacheError(email)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	if err != nil {
		return r.cacheError(email)
	}
	var out emailRepResult
	if err := json.Unmarshal(body, &out); err != nil {
		return r.cacheError(email)
	}
	r.mu.Lock()
	r.cache[email] = emailRepCacheEntry{
		res: &out, expires: time.Now().Add(time.Duration(r.cfg.CacheSec) * time.Second),
	}
	r.evictIfTooBigLocked()
	r.mu.Unlock()
	return &out, false
}

func (r *emailReputationRule) cacheError(email string) (*emailRepResult, bool) {
	r.mu.Lock()
	r.cache[email] = emailRepCacheEntry{
		err:     true,
		expires: time.Now().Add(60 * time.Second), // 错误短缓存：1min
	}
	r.evictIfTooBigLocked()
	r.mu.Unlock()
	return nil, true
}

// evictIfTooBigLocked 防 cache 无限增长：超 4096 条清最早 1024 条（粗粒度，
// 不维护严格 LRU；commercial 部署应换 Redis 共享缓存）。
func (r *emailReputationRule) evictIfTooBigLocked() {
	if len(r.cache) <= 4096 {
		return
	}
	i := 0
	for k := range r.cache {
		delete(r.cache, k)
		i++
		if i >= 1024 {
			break
		}
	}
}
