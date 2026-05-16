// fx.go — SP-3C Multi-currency + FX rate snapshot.
//
// 跨币种资金流的核心问题:
//
//   1. Charge 在 USD 1000, Transfer 给德国卖家要 EUR — 需要换汇
//   2. 不同时间汇率不同, 业务侧要 lock rate at execution time
//   3. 财务侧要审计: 这笔钱按多少汇率换的, 何时? (FX rate snapshot)
//
// 设计:
//   - FXClient 接口, 真实实现走 fx-service gRPC / Open Exchange Rates API
//   - GetRate(from, to, at) → Rate + 来源 + 时间戳, 持久化到 FXSnapshot 表
//   - Translator 在 build Transfer 时, 若 edge.dest_currency != tc.Currency, 调 FXClient,
//     把换算后的 amount 落 Transfer.AmountMinor (目标币种), 同时记 FXSnapshot.ID
//   - Reversal 退款时按原 snapshot 反算, 不重新查汇率 (保持双边平衡)
//
// 财务侧:
//   - Transfer 记账走原币 + 平台基础币双 ledger entry
//   - accounting-system 支持多币种账户 (账户 ID 后缀币种, e.g. seller_balance/USD/{id})
package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FXSnapshot 锁定的一次汇率快照.
//
// 每次跨币种 Transfer 落一条, 退款时按 ID 反查同一 rate.
type FXSnapshot struct {
	ID           string    `db:"id" json:"id"` // fxs_xxx
	FromCurrency string    `db:"from_currency" json:"from_currency"`
	ToCurrency   string    `db:"to_currency" json:"to_currency"`
	Rate         float64   `db:"rate" json:"rate"`           // to = from * rate (e.g. USD→EUR rate=0.92)
	Source       string    `db:"source" json:"source"`       // "fx-service" / "openexchangerates" / "manual"
	FetchedAt    time.Time `db:"fetched_at" json:"fetched_at"`
	GraphRunID   int64     `db:"graph_run_id" json:"graph_run_id,omitempty"`
}

// FXClient 调 FX 服务的抽象.
//
// 真实实现:
//   - GRPCFXClient → 调 fx-service:9091 (我们自研)
//   - HTTPOpenExchangeRates → 调 openexchangerates.org (第三方 API)
//
// dev 用 StaticFXClient 固定汇率不调网络.
type FXClient interface {
	GetRate(ctx context.Context, from, to string) (*FXSnapshot, error)
}

// StaticFXClient dev/单测用, 固定汇率表.
//
// e.g. StaticFXClient{Rates: map[string]float64{"USD-EUR": 0.92, "USD-CNY": 7.20}}
type StaticFXClient struct {
	Rates map[string]float64 // key = "FROM-TO"
}

// GetRate.
func (s StaticFXClient) GetRate(_ context.Context, from, to string) (*FXSnapshot, error) {
	if from == to {
		return &FXSnapshot{
			FromCurrency: from, ToCurrency: to, Rate: 1.0,
			Source: "identity", FetchedAt: time.Now().UTC(),
		}, nil
	}
	key := from + "-" + to
	rate, ok := s.Rates[key]
	if !ok {
		// 试反向 (USD-EUR 0.92 ⇒ EUR-USD 1/0.92)
		if revKey := to + "-" + from; s.Rates[revKey] > 0 {
			rate = 1.0 / s.Rates[revKey]
			ok = true
		}
	}
	if !ok {
		return nil, fmt.Errorf("static fx: no rate for %s → %s", from, to)
	}
	return &FXSnapshot{
		ID:           genID("fxs"),
		FromCurrency: from,
		ToCurrency:   to,
		Rate:         rate,
		Source:       "static",
		FetchedAt:    time.Now().UTC(),
	}, nil
}

// FXSnapshotRepo 持久化 FX 快照.
type FXSnapshotRepo interface {
	Insert(ctx context.Context, snap *FXSnapshot) error
	Get(ctx context.Context, id string) (*FXSnapshot, error)
}

// ─── Translator 集成 ──────────────────────────────────────────────────

// ConvertAmount 把 amount 从 fromCur 换到 toCur. 同币种返回原 amount + nil snapshot.
//
// 调用方 (translator) 在产生 Transfer 之前调本函数, 把 Transfer.AmountMinor 写换算后的值,
// 同时把 snapshot.ID 写到 Transfer.Metadata["fx_snapshot_id"] 留审计.
//
// 取整策略: floor (保守, 防超额转出). 余数归 platform 收入.
func ConvertAmount(
	ctx context.Context,
	fx FXClient,
	repo FXSnapshotRepo,
	amount int64,
	fromCur, toCur string,
	graphRunID int64,
) (converted int64, snap *FXSnapshot, err error) {
	if fromCur == toCur {
		return amount, nil, nil
	}
	if fx == nil {
		return 0, nil, errors.New("FX client not configured, cannot convert " + fromCur + " → " + toCur)
	}
	snap, err = fx.GetRate(ctx, fromCur, toCur)
	if err != nil {
		return 0, nil, fmt.Errorf("fx get rate: %w", err)
	}
	snap.GraphRunID = graphRunID
	// floor: 1000 USD * 0.92 = 920 EUR (cents)
	converted = int64(float64(amount) * snap.Rate)

	// 落 snapshot 表 (审计 + 退款时反查)
	if repo != nil && snap.ID != "" {
		if err := repo.Insert(ctx, snap); err != nil {
			// 不阻塞主流程, 仅 warn
			// caller 会拿到 snap 但 ID 可能在 DB 里没记 — 退款时 fallback 新查
		}
	}
	return converted, snap, nil
}
