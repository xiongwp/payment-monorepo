// fx.go — SP-FIN-1 真 FXClient HTTP-JSON 实现.
//
// 调 fx-service (自研) 或 Open Exchange Rates (第三方) 的统一接口.
//
//   GET {url}/v1/rates?from=USD&to=EUR
//   →
//   { "from":"USD", "to":"EUR", "rate":0.9234, "source":"fx-service", "fetched_at": "2026-05-15T01:23:45Z" }
package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"reconcile-system/packages/split-payment/internal/workflow"
)

// HTTPFXClient FX 服务客户端.
type HTTPFXClient struct {
	BaseURL    string
	HTTPClient *http.Client
	AuthToken  string
	// CachedRates 内存缓存, 5min TTL — 多副本各自独立缓存 (足够 dev/staging).
	cache *fxCache
}

type fxCache struct {
	rates map[string]fxCacheEntry
}

type fxCacheEntry struct {
	snap *workflow.FXSnapshot
	exp  time.Time
}

// NewHTTPFXClient.
func NewHTTPFXClient(baseURL, token string) *HTTPFXClient {
	return &HTTPFXClient{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
		AuthToken:  token,
		cache:      &fxCache{rates: map[string]fxCacheEntry{}},
	}
}

// GetRate impl workflow.FXClient.
//
// 5min cache + 2s timeout. 失败返 error (caller 决定是否 fallback).
func (c *HTTPFXClient) GetRate(ctx context.Context, from, to string) (*workflow.FXSnapshot, error) {
	if from == to {
		return &workflow.FXSnapshot{FromCurrency: from, ToCurrency: to, Rate: 1, Source: "identity", FetchedAt: time.Now().UTC()}, nil
	}
	key := from + "-" + to
	if ent, ok := c.cache.rates[key]; ok && time.Now().Before(ent.exp) {
		return ent.snap, nil
	}
	if c.BaseURL == "" {
		return nil, errors.New("fx: base url empty")
	}
	u := c.BaseURL + "/v1/rates?from=" + url.QueryEscape(from) + "&to=" + url.QueryEscape(to)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fx http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fx upstream %d", resp.StatusCode)
	}
	var out struct {
		From      string  `json:"from"`
		To        string  `json:"to"`
		Rate      float64 `json:"rate"`
		Source    string  `json:"source"`
		FetchedAt string  `json:"fetched_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("fx decode: %w", err)
	}
	if out.Rate <= 0 {
		return nil, fmt.Errorf("fx: rate <= 0")
	}
	t, _ := time.Parse(time.RFC3339, out.FetchedAt)
	if t.IsZero() {
		t = time.Now().UTC()
	}
	snap := &workflow.FXSnapshot{
		ID:           "fxs_" + fmt.Sprintf("%d", time.Now().UnixNano()),
		FromCurrency: out.From, ToCurrency: out.To,
		Rate: out.Rate, Source: out.Source, FetchedAt: t,
	}
	c.cache.rates[key] = fxCacheEntry{snap: snap, exp: time.Now().Add(5 * time.Minute)}
	return snap, nil
}
