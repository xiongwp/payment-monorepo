// Package sources — 多源汇率拉取。每个 source 实现 Fetcher 接口。
//
// 内置:
//   - ecb: 欧洲央行 (免费, EUR 为 base, 周末没数据)
//   - oanda: OANDA API (商业)
//   - stripe_fx: 从 Stripe 拿 (走 partner 账号)
//   - manual: 商户后台手填 (overrides)
//
// fetcher 输出统一 *domain.Rate; 调用方 (puller) 聚合多源 + 入库。

package sources

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"reconcile-system/packages/fx-service/internal/domain"
)

// Fetcher 一个数据源。
type Fetcher interface {
	Name() string
	Fetch(ctx context.Context, pairs []Pair) ([]*domain.Rate, error)
}

// Pair 待拉的币种对。
type Pair struct {
	From, To string
}

// ─── ECB (欧央行) ─────────────────────────────────────────────────────
// https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml — base = EUR

type ECBFetcher struct {
	HTTP *http.Client
}

func (e *ECBFetcher) Name() string { return "ecb" }

func (e *ECBFetcher) Fetch(ctx context.Context, _ []Pair) ([]*domain.Rate, error) {
	if e.HTTP == nil {
		e.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml", nil)
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ecb http %d", resp.StatusCode)
	}
	var doc ecbXML
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	expires := now.Add(5 * time.Minute)
	out := []*domain.Rate{}
	if len(doc.Cube.Date) == 0 {
		return nil, fmt.Errorf("ecb: no data")
	}
	day := doc.Cube.Date[0]
	for _, c := range day.Rate {
		// 1 EUR = X CCY
		v, err := strconv.ParseFloat(c.Rate, 64)
		if err != nil {
			continue
		}
		e8 := int64(v * float64(domain.RateScale))
		out = append(out, &domain.Rate{
			FromCurrency: "EUR", ToCurrency: strings.ToUpper(c.Currency),
			MidRateE8: e8, BidRateE8: e8, AskRateE8: e8, // ECB 不区分 bid/ask
			Source: "ecb", FetchedAt: now, ExpiresAt: expires,
		})
	}
	return out, nil
}

type ecbXML struct {
	Cube struct {
		Date []struct {
			Time string `xml:"time,attr"`
			Rate []struct {
				Currency string `xml:"currency,attr"`
				Rate     string `xml:"rate,attr"`
			} `xml:"Cube"`
		} `xml:"Cube"`
	} `xml:"Cube"`
}

// ─── Manual (admin 手填, override 自动源) ─────────────────────────────

type ManualFetcher struct {
	Store map[string]*domain.Rate // key: "FROM_TO"
}

func (m *ManualFetcher) Name() string { return "manual" }

func (m *ManualFetcher) Fetch(_ context.Context, pairs []Pair) ([]*domain.Rate, error) {
	out := []*domain.Rate{}
	for _, p := range pairs {
		if r, ok := m.Store[p.From+"_"+p.To]; ok {
			cp := *r // shallow copy
			cp.Source = "manual"
			cp.FetchedAt = time.Now().UTC()
			cp.ExpiresAt = cp.FetchedAt.Add(1 * time.Hour)
			out = append(out, &cp)
		}
	}
	return out, nil
}
