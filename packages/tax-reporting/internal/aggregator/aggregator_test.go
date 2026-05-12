package aggregator

import (
	"testing"
	"time"

	"reconcile-system/packages/tax-reporting/internal/domain"
	"reconcile-system/packages/tax-reporting/internal/store"
)

func TestIncremental_AccumulatesByMonth(t *testing.T) {
	s := store.NewMemStore()
	a := New(s)

	jan := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	mar := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	for _, ev := range []domain.PayoutEvent{
		{EventID: "e1", MerchantID: "m1", OccurredAt: jan, GrossAmount: 100_00, NetAmount: 97_00, FeesAmount: 3_00, Currency: "USD", Jurisdiction: "US", TxnCount: 1, Channel: "visa"},
		{EventID: "e2", MerchantID: "m1", OccurredAt: jan, GrossAmount: 200_00, NetAmount: 194_00, FeesAmount: 6_00, Currency: "USD", Jurisdiction: "US", TxnCount: 1, Channel: "mc"},
		{EventID: "e3", MerchantID: "m1", OccurredAt: mar, GrossAmount: 500_00, NetAmount: 485_00, FeesAmount: 15_00, Currency: "USD", Jurisdiction: "US", TxnCount: 1, Channel: "visa"},
	} {
		if err := s.AppendPayout(ev); err != nil {
			t.Fatal(err)
		}
		if err := a.Incremental(ev); err != nil {
			t.Fatal(err)
		}
	}

	agg, err := s.GetAggregate("m1", 2026, "US")
	if err != nil {
		t.Fatal(err)
	}
	if agg.TotalGross != 800_00 {
		t.Errorf("total gross = %d, want 80000", agg.TotalGross)
	}
	if agg.MonthlyGross[0] != 300_00 { // Jan
		t.Errorf("Jan = %d, want 30000", agg.MonthlyGross[0])
	}
	if agg.MonthlyGross[2] != 500_00 { // Mar
		t.Errorf("Mar = %d, want 50000", agg.MonthlyGross[2])
	}
	if agg.ByChannel["visa"] != 600_00 || agg.ByChannel["mc"] != 200_00 {
		t.Errorf("channel split wrong: %+v", agg.ByChannel)
	}
}

func TestEligibleFor_2026Threshold(t *testing.T) {
	thresholds := domain.DefaultThresholds()
	// 2026 US 阈值 \$600
	below := domain.Aggregate{Year: 2026, Jurisdiction: "US", TotalGross: 500_00}
	above := domain.Aggregate{Year: 2026, Jurisdiction: "US", TotalGross: 700_00}

	if EligibleFor(below, domain.Form1099K, thresholds) {
		t.Error("\$500 below \$600 threshold, should NOT be eligible")
	}
	if !EligibleFor(above, domain.Form1099K, thresholds) {
		t.Error("\$700 above \$600 threshold, should be eligible")
	}
}

func TestRecompute_RebuildsFromEvents(t *testing.T) {
	s := store.NewMemStore()
	a := New(s)

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		_ = s.AppendPayout(domain.PayoutEvent{
			EventID:    "e_" + string(rune('a'+i)),
			MerchantID: "m1",
			OccurredAt: now,
			GrossAmount: 100_00,
			Currency:   "USD",
			Jurisdiction: "US",
			TxnCount:    1,
		})
	}
	if err := a.Recompute("m1", 2026); err != nil {
		t.Fatal(err)
	}
	agg, _ := s.GetAggregate("m1", 2026, "US")
	if agg.TotalGross != 500_00 {
		t.Errorf("after recompute total = %d, want 50000", agg.TotalGross)
	}
}
