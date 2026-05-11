// Package workflow — FX 报价 + 兑换执行。
//
// 流程:
//   1. quote(from, to, amount) → 查最新 rate + 加 spread + 计算 fee → 锁价 5min → 返 quote_id
//   2. confirm(quote_id) → 调 accounting AtomicBatch 落账 (双币种分录)
//   3. 过期未 confirm → 自动 canceled (cron 清理)

package workflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"reconcile-system/packages/fx-service/internal/domain"

	accountingv1 "github.com/xiongwp/accounting-system/api/proto/accounting/v1"
)

// Service ...
type Service struct {
	Rates       RateRepo
	Quotes      QuoteRepo
	Conversions ConversionRepo
	Accounting  AccountingClient
	DefaultSpreadBp int  // 默认 spread, 50bp = 0.5%
	FeeFlatMinor    int64 // 平台手续费固定 (e.g. 50 cents)
	QuoteTTL    time.Duration
	Now         func() time.Time
}

// RateRepo ...
type RateRepo interface {
	Latest(ctx context.Context, from, to string) (*domain.Rate, error)
	Save(ctx context.Context, r *domain.Rate) error
}
// QuoteRepo ...
type QuoteRepo interface {
	Get(ctx context.Context, id string) (*domain.Quote, error)
	Save(ctx context.Context, q *domain.Quote) error
}
// ConversionRepo ...
type ConversionRepo interface {
	Save(ctx context.Context, c *domain.Conversion) error
}

// AccountingClient ...
type AccountingClient interface {
	AtomicBatchBooking(ctx context.Context, in *accountingv1.AtomicBatchBookingRequest) (*accountingv1.AtomicBatchBookingResponse, error)
}

// ─── Quote ────────────────────────────────────────────────────────────

// QuoteRequest ...
type QuoteRequest struct {
	FromCurrency string
	ToCurrency   string
	FromAmountMinor int64
	MerchantID   string
	CustomerID   string
}

// Quote 报价 + 锁 5min。
func (s *Service) Quote(ctx context.Context, req QuoteRequest) (*domain.Quote, error) {
	if req.FromAmountMinor <= 0 {
		return nil, errors.New("amount must be positive")
	}
	if req.FromCurrency == req.ToCurrency {
		return nil, errors.New("from == to")
	}
	rate, err := s.Rates.Latest(ctx, req.FromCurrency, req.ToCurrency)
	if err != nil {
		// 尝试通过 USD 做 cross-rate (FROM→USD→TO)
		rate, err = s.crossRate(ctx, req.FromCurrency, req.ToCurrency)
		if err != nil {
			return nil, fmt.Errorf("no rate: %w", err)
		}
	}
	// 加 spread (用户卖出 from 买 to, 平台拿 ask)
	rateWithSpread := domain.ApplySpread(rate.MidRateE8, s.DefaultSpreadBp, true)
	toAmount := domain.ApplyRate(req.FromAmountMinor, rateWithSpread)

	now := s.now()
	q := &domain.Quote{
		QuoteID:         genID("q_"),
		FromCurrency:    req.FromCurrency,
		ToCurrency:      req.ToCurrency,
		FromAmountMinor: req.FromAmountMinor,
		ToAmountMinor:   toAmount,
		RateE8:          rateWithSpread,
		MidRateE8:       rate.MidRateE8,
		SpreadBp:        s.DefaultSpreadBp,
		FeeMinor:        s.FeeFlatMinor,
		FeeCurrency:     req.FromCurrency,
		MerchantID:      req.MerchantID,
		CustomerID:      req.CustomerID,
		Status:          "open",
		ExpiresAt:       now.Add(s.ttl()),
		CreatedAt:       now,
	}
	if err := s.Quotes.Save(ctx, q); err != nil {
		return nil, err
	}
	return q, nil
}

// crossRate 用 USD 做桥 (e.g. JPY → USD → EUR)。
func (s *Service) crossRate(ctx context.Context, from, to string) (*domain.Rate, error) {
	if from == "USD" || to == "USD" {
		return nil, errors.New("no rate found")
	}
	r1, err := s.Rates.Latest(ctx, from, "USD")
	if err != nil {
		return nil, fmt.Errorf("%s→USD: %w", from, err)
	}
	r2, err := s.Rates.Latest(ctx, "USD", to)
	if err != nil {
		return nil, fmt.Errorf("USD→%s: %w", to, err)
	}
	// cross rate = r1 * r2 (单位匹配)
	// 1 FROM = r1.mid * 1e-8 USD ; 1 USD = r2.mid * 1e-8 TO
	// → 1 FROM = (r1.mid * r2.mid) * 1e-16 TO; 再 *1e8 得 e8
	crossE8 := (r1.MidRateE8 * r2.MidRateE8) / domain.RateScale
	return &domain.Rate{
		FromCurrency: from, ToCurrency: to,
		MidRateE8: crossE8, BidRateE8: crossE8, AskRateE8: crossE8,
		Source: "cross/" + r1.Source + "+" + r2.Source,
		FetchedAt: s.now(), ExpiresAt: s.now().Add(5 * time.Minute),
	}, nil
}

// ─── Confirm — 调 accounting 双币种落账 ─────────────────────────────────

// ConfirmRequest ...
type ConfirmRequest struct {
	QuoteID     string
	FromAccount string // e.g. "cust_usd_wallet/u123"
	ToAccount   string // e.g. "cust_jpy_wallet/u123"
	FeeAccount  string // e.g. "platform_fx_revenue"
}

// Confirm 执行 — 不可重入 (同 quote_id 第二次 confirm 报错)。
func (s *Service) Confirm(ctx context.Context, req ConfirmRequest) (*domain.Conversion, error) {
	q, err := s.Quotes.Get(ctx, req.QuoteID)
	if err != nil {
		return nil, err
	}
	if q.Status != "open" {
		return nil, fmt.Errorf("quote status=%s, cannot confirm", q.Status)
	}
	if s.now().After(q.ExpiresAt) {
		q.Status = "expired"
		_ = s.Quotes.Save(ctx, q)
		return nil, errors.New("quote expired")
	}

	conv := &domain.Conversion{
		ConversionID:    genID("conv_"),
		QuoteID:         q.QuoteID,
		FromAccount:     req.FromAccount,
		ToAccount:       req.ToAccount,
		FromAmountMinor: q.FromAmountMinor,
		ToAmountMinor:   q.ToAmountMinor,
		FromCurrency:    q.FromCurrency,
		ToCurrency:      q.ToCurrency,
		RateE8:          q.RateE8,
		Status:          "pending",
		CreatedAt:       s.now(),
	}
	_ = s.Conversions.Save(ctx, conv)

	// 双币种 + 手续费, 3 笔分录, 全部原子
	// 1. 扣 from 账户 q.FromAmountMinor (FROM 币种)
	// 2. 加 to 账户 q.ToAmountMinor (TO 币种)
	// 3. 扣 from 账户 q.FeeMinor → 平台手续费收入 (FROM 币种)
	requests := []*accountingv1.AtomicBatchBookingEntry{
		{
			RequestId: conv.ConversionID + "-main",
			BusinessNo: conv.ConversionID,
			Currency: q.FromCurrency,
			Description: fmt.Sprintf("FX %s→%s amount %d", q.FromCurrency, q.ToCurrency, q.FromAmountMinor),
			Entries: []*accountingv1.AccountingEntry{
				{AccountId: req.FromAccount,
					Amount: strconv.FormatInt(q.FromAmountMinor, 10),
					Direction: accountingv1.Direction_DIRECTION_DEBIT},
				{AccountId: "fx_clearing/" + q.FromCurrency,
					Amount: strconv.FormatInt(q.FromAmountMinor, 10),
					Direction: accountingv1.Direction_DIRECTION_CREDIT},
			},
		},
		{
			RequestId: conv.ConversionID + "-target",
			BusinessNo: conv.ConversionID,
			Currency: q.ToCurrency,
			Description: fmt.Sprintf("FX %s→%s converted %d", q.FromCurrency, q.ToCurrency, q.ToAmountMinor),
			Entries: []*accountingv1.AccountingEntry{
				{AccountId: "fx_clearing/" + q.ToCurrency,
					Amount: strconv.FormatInt(q.ToAmountMinor, 10),
					Direction: accountingv1.Direction_DIRECTION_DEBIT},
				{AccountId: req.ToAccount,
					Amount: strconv.FormatInt(q.ToAmountMinor, 10),
					Direction: accountingv1.Direction_DIRECTION_CREDIT},
			},
		},
	}
	if q.FeeMinor > 0 {
		requests = append(requests, &accountingv1.AtomicBatchBookingEntry{
			RequestId: conv.ConversionID + "-fee",
			BusinessNo: conv.ConversionID,
			Currency: q.FeeCurrency,
			Description: fmt.Sprintf("FX fee %s %d", q.FeeCurrency, q.FeeMinor),
			Entries: []*accountingv1.AccountingEntry{
				{AccountId: req.FromAccount,
					Amount: strconv.FormatInt(q.FeeMinor, 10),
					Direction: accountingv1.Direction_DIRECTION_DEBIT},
				{AccountId: req.FeeAccount,
					Amount: strconv.FormatInt(q.FeeMinor, 10),
					Direction: accountingv1.Direction_DIRECTION_CREDIT},
			},
		})
	}

	resp, err := s.Accounting.AtomicBatchBooking(ctx, &accountingv1.AtomicBatchBookingRequest{
		BatchRequestId:  conv.ConversionID,
		BatchBusinessNo: conv.ConversionID,
		Requests:        requests,
		Description:     fmt.Sprintf("FX conversion %s", conv.ConversionID),
	})
	if err != nil {
		conv.Status = "failed"
		_ = s.Conversions.Save(ctx, conv)
		return nil, err
	}
	if !resp.AllSuccess {
		conv.Status = "failed"
		_ = s.Conversions.Save(ctx, conv)
		return nil, fmt.Errorf("partial fail: %s", resp.Message)
	}
	if len(resp.Results) > 0 {
		conv.VoucherNo = resp.Results[0].VoucherNo
	}
	conv.Status = "posted"
	_ = s.Conversions.Save(ctx, conv)

	q.Status = "confirmed"
	_ = s.Quotes.Save(ctx, q)
	return conv, nil
}

// ─── helpers ──────────────────────────────────────────────────────────

func genID(prefix string) string { return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()) }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Service) ttl() time.Duration {
	if s.QuoteTTL > 0 {
		return s.QuoteTTL
	}
	return 5 * time.Minute
}
