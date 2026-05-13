// Package provider — FX rate provider interfaces + adapters (Reuters / OANDA / static).
//
// 实现策略:
//   - 真实 Reuters / OANDA API 需要付费 contract,这里定义接口 + Stub/Static 实现
//   - 上线前由运维替换 Static 为 ReutersProvider (impl 调用方按 contract 写)
//   - 接口稳定,业务侧 (fx-service handlers) 不感知 provider 切换
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Rate 一个汇率快照.
type Rate struct {
	From      string  // ISO 4217 e.g. USD
	To        string  // ISO 4217 e.g. EUR
	Mid       float64 // mid rate (e.g. 0.92 = 1 USD -> 0.92 EUR)
	Bid       float64 // 我们买进 From 给 To 的价 (mid - spread)
	Ask       float64 // 我们卖 From 收 To 的价 (mid + spread)
	Spread    float64 // bid/ask 差占 mid 比例 (e.g. 0.005 = 0.5%)
	Timestamp time.Time
	Provider  string
}

// QuoteRequest 商户 FX 报价请求.
type QuoteRequest struct {
	From       string
	To         string
	Amount     int64    // cents in From
	TTL        time.Duration  // 报价有效期,默认 30s
	MerchantID string
}

// QuoteResponse 锁汇报价 (TTL 内可执行).
type QuoteResponse struct {
	QuoteID   string
	Rate      float64
	From      string
	To        string
	AmountIn  int64
	AmountOut int64
	ExpiresAt time.Time
	Provider  string
}

// Provider FX 数据源接口.
type Provider interface {
	Name() string

	// LatestRate 拿当前 mid + bid/ask.
	LatestRate(ctx context.Context, from, to string) (*Rate, error)

	// QuoteLock 创建一个 TTL 内可执行的锁汇报价.
	QuoteLock(ctx context.Context, req *QuoteRequest) (*QuoteResponse, error)
}

// ─── Static / Stub 实现 (dev + 单测用) ─────────────────────────

// StaticProvider 用预先注入的 rate map 应答.
type StaticProvider struct {
	mu        sync.RWMutex
	name      string
	rates     map[string]float64 // key = "USD/EUR"
	spread    float64
	now       func() time.Time
	quoteSeq  uint64
}

// NewStaticProvider 创建.
func NewStaticProvider(name string, spread float64) *StaticProvider {
	if spread <= 0 {
		spread = 0.005 // 0.5% 默认 spread
	}
	return &StaticProvider{
		name:   name,
		rates:  map[string]float64{},
		spread: spread,
		now:    time.Now,
	}
}

// SetRate 注入测试 rate.
func (p *StaticProvider) SetRate(from, to string, mid float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rates[strings.ToUpper(from)+"/"+strings.ToUpper(to)] = mid
}

// Name impl.
func (p *StaticProvider) Name() string { return p.name }

// LatestRate impl.
func (p *StaticProvider) LatestRate(_ context.Context, from, to string) (*Rate, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	key := strings.ToUpper(from) + "/" + strings.ToUpper(to)
	mid, ok := p.rates[key]
	if !ok {
		return nil, fmt.Errorf("no rate for %s", key)
	}
	return &Rate{
		From: from, To: to, Mid: mid,
		Bid: mid * (1 - p.spread/2), Ask: mid * (1 + p.spread/2),
		Spread:    p.spread,
		Timestamp: p.now(),
		Provider:  p.name,
	}, nil
}

// QuoteLock impl. 静态实现不真锁,只回响 quote.
func (p *StaticProvider) QuoteLock(ctx context.Context, req *QuoteRequest) (*QuoteResponse, error) {
	if req == nil {
		return nil, errors.New("nil request")
	}
	r, err := p.LatestRate(ctx, req.From, req.To)
	if err != nil {
		return nil, err
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	p.quoteSeq++
	rate := r.Bid // 商户卖出方向用 Bid
	return &QuoteResponse{
		QuoteID:   fmt.Sprintf("q_%s_%d", p.name, p.quoteSeq),
		Rate:      rate,
		From:      req.From,
		To:        req.To,
		AmountIn:  req.Amount,
		AmountOut: int64(float64(req.Amount) * rate),
		ExpiresAt: p.now().Add(ttl),
		Provider:  p.name,
	}, nil
}

// ─── Reuters / OANDA provider (skeleton) ───────────────────────

// ReutersConfig Reuters Datalink API 配置.
type ReutersConfig struct {
	BaseURL    string        // 默认 https://api.refinitiv.com
	APIKey     string
	HTTPTimeout time.Duration
}

// NewReutersProvider 返回真实 Provider. 商务谈下来 API 后接入 (本骨架内仅校验 config).
func NewReutersProvider(cfg ReutersConfig) (Provider, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("reuters: APIKey required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.refinitiv.com"
	}
	// 真实实现走 net/http 拉 /v1/quotes;响应 JSON parse 后填 Rate.
	// 当前返回 stub static provider,接 API 后替换 HTTP 调用即可。
	return NewStaticProvider("reuters-stub", 0.003), nil
}

// OandaConfig OANDA REST v20 API 配置.
type OandaConfig struct {
	BaseURL      string
	AccountID    string
	Token        string
	HTTPTimeout  time.Duration
}

// NewOandaProvider 返回 OANDA Provider.
func NewOandaProvider(cfg OandaConfig) (Provider, error) {
	if cfg.AccountID == "" || cfg.Token == "" {
		return nil, errors.New("oanda: AccountID + Token required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api-fxtrade.oanda.com"
	}
	return NewStaticProvider("oanda-stub", 0.004), nil
}
