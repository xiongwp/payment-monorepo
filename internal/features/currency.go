package features

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// CurrencyExtractor 把 Amount × FX_rate → AmountUSD（minor unit）。
//
// 数据源：rates map，启动时由 cron job 从 Open Exchange Rates / ECB / 央行
// API 拉一次刷一次。当前 stub 用一个内置 default rates 表（先验粗值）；
// 生产换 RateProvider 接口拿实时汇率。
//
// 失败语义：FX 拿不到 / 币种未知 → AmountUSD = Amount（不换算，跨币种规则
// 在该 case 上少算一笔，比 fail-close 更安全）。
type CurrencyExtractor struct {
	rates RateProvider
}

// RateProvider USD-per-unit-of-currency。USD/USD = 1。
type RateProvider interface {
	// Rate 返回 1 单位 currency 兑 USD 的汇率（USD per unit）。0 = 不支持。
	Rate(currency string) float64
}

// NewCurrencyExtractor rates 接口；nil → 用内置 default。
func NewCurrencyExtractor(p RateProvider) *CurrencyExtractor {
	if p == nil {
		p = defaultRates
	}
	return &CurrencyExtractor{rates: p}
}

func (e *CurrencyExtractor) Name() string { return "currency" }

func (e *CurrencyExtractor) Enrich(_ context.Context, txn *engine.TxnContext) {
	if txn == nil || txn.Amount == 0 {
		return
	}
	cur := strings.ToUpper(strings.TrimSpace(txn.Currency))
	if cur == "" {
		return
	}
	r := e.rates.Rate(cur)
	if r <= 0 {
		txn.AmountUSD = txn.Amount // 兜底
		return
	}
	// minor unit 假设全是 100 (cents)；JPY / KRW 是 1 minor=1 unit，
	// 折算用粗算（生产用专门 currency-precision 表）。这里先简化：
	// 直接 amount × rate，保留 minor unit 概念。
	txn.AmountUSD = int64(float64(txn.Amount) * r)
}

// ── default static rates（2025Q4 粗略值，启动时 log "stale"）─────────

var defaultRates = &staticRates{
	rates: map[string]float64{
		"USD": 1.0,
		"EUR": 1.10,
		"GBP": 1.30,
		"JPY": 0.0067,  // 1 yen ≈ 0.67 cents
		"CNY": 0.14,
		"HKD": 0.13,
		"SGD": 0.74,
		"AUD": 0.65,
		"CAD": 0.74,
		"PHP": 0.018,
		"IDR": 0.000063,
		"THB": 0.029,
		"VND": 0.000040,
		"MYR": 0.21,
		"INR": 0.012,
		"BRL": 0.18,
		"MXN": 0.058,
		"ZAR": 0.054,
		"RUB": 0.011,
	},
	loadedAt: time.Now(),
}

type staticRates struct {
	mu       sync.RWMutex
	rates    map[string]float64
	loadedAt time.Time
}

func (s *staticRates) Rate(currency string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rates[strings.ToUpper(currency)]
}

// Update 替换汇率表（cron job 拉新数据后调）。原子。
func (s *staticRates) Update(rates map[string]float64) {
	cp := make(map[string]float64, len(rates))
	for k, v := range rates {
		cp[strings.ToUpper(k)] = v
	}
	s.mu.Lock()
	s.rates = cp
	s.loadedAt = time.Now()
	s.mu.Unlock()
}

// LoadedAt 上次更新时间（监控 stale rate 用）。
func (s *staticRates) LoadedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadedAt
}
