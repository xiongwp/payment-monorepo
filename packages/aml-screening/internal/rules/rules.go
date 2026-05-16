// Package rules — AML (Anti-Money Laundering) 规则引擎.
//
// 当前规则:
//   1. 高频小额 (Structuring): 24h 内 > 10 笔且每笔 < $200 → REVIEW
//   2. 黑名单国家: 受制裁国家发起 → BLOCK
//   3. 突增异常: 单日交易额 / 历史 30d 均值 > 10x → REVIEW
//   4. 跨境高频: 跨 5 个国家在 1h 内 → REVIEW
//   5. 黑名单 hit (OFAC/EU): 客户 / 商户 / IBAN match → BLOCK
//
// 规则通过 RuleEngine.Eval 顺序执行,任一 BLOCK 立刻中止;REVIEW 累积.
package rules

import (
	"context"
	"strings"
	"time"
)

// Verdict 规则裁定结果.
type Verdict string

const (
	VerdictAllow  Verdict = "allow"
	VerdictReview Verdict = "review"
	VerdictBlock  Verdict = "block"
)

// Input AML 输入.
type Input struct {
	CustomerID    string
	MerchantID    string
	Amount        int64    // cents
	Currency      string
	CountryOrigin string   // ISO2 客户所在国
	CountryDest   string   // ISO2 商户所在国
	IBAN          string   // 转账目的 IBAN (跨境)
	Recent24h     []ChargeFact
	Recent1h      []ChargeFact
	Avg30d        int64    // 客户过去 30d 日均交易额 (cents)
	Today         int64    // 客户今日累计 (cents)
	Watchlists    Watchlists
	Now           time.Time
}

// ChargeFact 一笔过往交易摘要 (历史窗口).
type ChargeFact struct {
	At     time.Time
	Amount int64
	Country string
}

// Watchlists 全局制裁名单 (定期 refresh,见 aml-refresh-ofac cron).
type Watchlists struct {
	BlockedCustomers map[string]bool
	BlockedMerchants map[string]bool
	BlockedIBANs     map[string]bool
	BlockedCountries map[string]bool // ISO2 受制裁国家
}

// Hit 单条规则命中.
type Hit struct {
	Rule    string
	Verdict Verdict
	Reason  string
}

// Result 整体输出.
type Result struct {
	Verdict Verdict
	Hits    []Hit
}

// Rule 接口,每条规则一个.
type Rule interface {
	Name() string
	Eval(ctx context.Context, in Input) (Verdict, string)
}

// Engine 串行跑规则.
type Engine struct {
	rules []Rule
}

// NewEngine 内置 5 条 production rules.
func NewEngine() *Engine {
	return &Engine{
		rules: []Rule{
			ruleStructuring{},
			ruleSanctionsCountry{},
			ruleSpendBurst{},
			ruleCrossBorderHighFreq{},
			ruleWatchlistHit{},
		},
	}
}

// AddRule 注入额外规则 (custom).
func (e *Engine) AddRule(r Rule) { e.rules = append(e.rules, r) }

// Eval 跑全部规则.
func (e *Engine) Eval(ctx context.Context, in Input) *Result {
	out := &Result{Verdict: VerdictAllow}
	for _, r := range e.rules {
		v, reason := r.Eval(ctx, in)
		if v == VerdictAllow {
			continue
		}
		out.Hits = append(out.Hits, Hit{Rule: r.Name(), Verdict: v, Reason: reason})
		if v == VerdictBlock {
			out.Verdict = VerdictBlock
			return out  // BLOCK 立即停
		}
		if v == VerdictReview && out.Verdict != VerdictBlock {
			out.Verdict = VerdictReview
		}
	}
	return out
}

// ─── 规则实现 ─────────────────────────────────────────────────

// 规则 1: Structuring (high-freq small-amount).
type ruleStructuring struct{}

func (ruleStructuring) Name() string { return "structuring" }
func (ruleStructuring) Eval(_ context.Context, in Input) (Verdict, string) {
	if len(in.Recent24h) < 10 {
		return VerdictAllow, ""
	}
	smallCount := 0
	for _, c := range in.Recent24h {
		if c.Amount < 20000 { // < $200
			smallCount++
		}
	}
	if smallCount >= 10 {
		return VerdictReview, "10+ small-amount txns in last 24h"
	}
	return VerdictAllow, ""
}

// 规则 2: 受制裁国家.
type ruleSanctionsCountry struct{}

func (ruleSanctionsCountry) Name() string { return "sanctions_country" }
func (ruleSanctionsCountry) Eval(_ context.Context, in Input) (Verdict, string) {
	if in.Watchlists.BlockedCountries == nil {
		return VerdictAllow, ""
	}
	if in.Watchlists.BlockedCountries[strings.ToUpper(in.CountryOrigin)] {
		return VerdictBlock, "country_origin sanctioned"
	}
	if in.Watchlists.BlockedCountries[strings.ToUpper(in.CountryDest)] {
		return VerdictBlock, "country_dest sanctioned"
	}
	return VerdictAllow, ""
}

// 规则 3: 日额突增.
type ruleSpendBurst struct{}

func (ruleSpendBurst) Name() string { return "spend_burst" }
func (ruleSpendBurst) Eval(_ context.Context, in Input) (Verdict, string) {
	if in.Avg30d == 0 {
		return VerdictAllow, ""
	}
	if in.Today > in.Avg30d*10 {
		return VerdictReview, "today > 10x 30d-average"
	}
	return VerdictAllow, ""
}

// 规则 4: 跨境高频 (1h 内 5 国).
type ruleCrossBorderHighFreq struct{}

func (ruleCrossBorderHighFreq) Name() string { return "cross_border_high_freq" }
func (ruleCrossBorderHighFreq) Eval(_ context.Context, in Input) (Verdict, string) {
	countries := map[string]bool{}
	for _, c := range in.Recent1h {
		if c.Country != "" {
			countries[strings.ToUpper(c.Country)] = true
		}
	}
	if len(countries) >= 5 {
		return VerdictReview, "5+ countries in 1h"
	}
	return VerdictAllow, ""
}

// 规则 5: Watchlist 直接命中.
type ruleWatchlistHit struct{}

func (ruleWatchlistHit) Name() string { return "watchlist_hit" }
func (ruleWatchlistHit) Eval(_ context.Context, in Input) (Verdict, string) {
	if in.Watchlists.BlockedCustomers[in.CustomerID] {
		return VerdictBlock, "customer on watchlist"
	}
	if in.Watchlists.BlockedMerchants[in.MerchantID] {
		return VerdictBlock, "merchant on watchlist"
	}
	if in.IBAN != "" && in.Watchlists.BlockedIBANs[in.IBAN] {
		return VerdictBlock, "IBAN on watchlist"
	}
	return VerdictAllow, ""
}
