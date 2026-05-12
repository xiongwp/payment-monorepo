// Package aggregator — 把 payout_events 滚成年度 Aggregate.
//
// 主路径: 收到 PayoutEvent → 直接 incremental 累加到 (merchant, year, jurisdiction) 的 aggregate.
// 兜底路径: 每日 cron 跑全量 backfill — 补丢的事件 / 修正 aggregate 错位.
//
// gross_amount 单位都是分; 跨币种 aggregate 按 jurisdiction 拆 (一个商户在 EU 和 US 各一份).

package aggregator

import (
	"sync"

	"reconcile-system/packages/tax-reporting/internal/domain"
	"reconcile-system/packages/tax-reporting/internal/store"
)

type Aggregator struct {
	mu    sync.Mutex // 增量累加用; 真生产 RDB 用 UPDATE
	Store store.Store
}

func New(s store.Store) *Aggregator { return &Aggregator{Store: s} }

// Incremental 一条 PayoutEvent → 更新 aggregate.
// 同步; 调用方建议丢入 worker pool / Kafka consumer 异步跑.
func (a *Aggregator) Incremental(e domain.PayoutEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	year := e.OccurredAt.Year()
	month := int(e.OccurredAt.Month()) - 1 // 0..11

	agg, err := a.Store.GetAggregate(e.MerchantID, year, e.Jurisdiction)
	if err != nil {
		// 第一次, 初始化
		agg = domain.Aggregate{
			MerchantID:   e.MerchantID,
			Year:         year,
			Jurisdiction: e.Jurisdiction,
			Currency:     e.Currency,
			ByChannel:    map[string]int64{},
		}
	}
	if agg.ByChannel == nil {
		agg.ByChannel = map[string]int64{}
	}
	agg.MonthlyGross[month] += e.GrossAmount
	agg.MonthlyCount[month] += e.TxnCount
	agg.TotalGross += e.GrossAmount
	agg.TotalCount += e.TxnCount
	if e.Channel != "" {
		agg.ByChannel[e.Channel] += e.GrossAmount
	}
	return a.Store.UpsertAggregate(agg)
}

// Recompute 全量重算一个 merchant 的年度 aggregate (修正错位用).
// 调用前先 Truncate aggregate.
func (a *Aggregator) Recompute(merchantID string, year int) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	events, err := a.Store.ListPayouts(merchantID, year)
	if err != nil {
		return err
	}
	// 按 jurisdiction 分桶
	bucket := make(map[string]*domain.Aggregate)
	for _, e := range events {
		key := e.Jurisdiction
		agg, ok := bucket[key]
		if !ok {
			agg = &domain.Aggregate{
				MerchantID:   merchantID,
				Year:         year,
				Jurisdiction: key,
				Currency:     e.Currency,
				ByChannel:    map[string]int64{},
			}
			bucket[key] = agg
		}
		m := int(e.OccurredAt.Month()) - 1
		agg.MonthlyGross[m] += e.GrossAmount
		agg.MonthlyCount[m] += e.TxnCount
		agg.TotalGross += e.GrossAmount
		agg.TotalCount += e.TxnCount
		if e.Channel != "" {
			agg.ByChannel[e.Channel] += e.GrossAmount
		}
	}
	for _, agg := range bucket {
		if err := a.Store.UpsertAggregate(*agg); err != nil {
			return err
		}
	}
	return nil
}

// EligibleFor 是否触发申报 (跟阈值比对).
func EligibleFor(agg domain.Aggregate, formType domain.FormType, thresholds []domain.FilingThreshold) bool {
	for _, t := range thresholds {
		if t.Jurisdiction != agg.Jurisdiction || t.Year != agg.Year || t.FormType != formType {
			continue
		}
		if t.MinGross > 0 && agg.TotalGross < t.MinGross {
			return false
		}
		if t.MinCount > 0 && agg.TotalCount < t.MinCount {
			return false
		}
		return true
	}
	return false
}
