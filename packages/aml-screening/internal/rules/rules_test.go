package rules

import (
	"context"
	"testing"
	"time"
)

func mkInput() Input {
	now := time.Now()
	return Input{
		CustomerID:    "c1",
		MerchantID:    "m1",
		Amount:        1000,
		Currency:      "USD",
		CountryOrigin: "US",
		CountryDest:   "US",
		Now:           now,
		Watchlists: Watchlists{
			BlockedCustomers: map[string]bool{},
			BlockedMerchants: map[string]bool{},
			BlockedIBANs:     map[string]bool{},
			BlockedCountries: map[string]bool{"IR": true, "KP": true, "SY": true},
		},
	}
}

func TestEngine_AllowDefault(t *testing.T) {
	r := NewEngine().Eval(context.Background(), mkInput())
	if r.Verdict != VerdictAllow {
		t.Errorf("default should allow, got %s", r.Verdict)
	}
}

func TestEngine_StructuringTrigger(t *testing.T) {
	in := mkInput()
	for i := 0; i < 12; i++ {
		in.Recent24h = append(in.Recent24h, ChargeFact{
			At: in.Now.Add(-time.Duration(i) * time.Hour), Amount: 15000, Country: "US",
		})
	}
	r := NewEngine().Eval(context.Background(), in)
	if r.Verdict != VerdictReview {
		t.Errorf("expect review, got %s", r.Verdict)
	}
	if len(r.Hits) == 0 || r.Hits[0].Rule != "structuring" {
		t.Errorf("expect structuring hit, got %+v", r.Hits)
	}
}

func TestEngine_SanctionsCountryBlock(t *testing.T) {
	in := mkInput()
	in.CountryOrigin = "IR"
	r := NewEngine().Eval(context.Background(), in)
	if r.Verdict != VerdictBlock {
		t.Errorf("expect block, got %s", r.Verdict)
	}
}

func TestEngine_SpendBurstReview(t *testing.T) {
	in := mkInput()
	in.Avg30d = 10000
	in.Today = 200000   // 20x 平均
	r := NewEngine().Eval(context.Background(), in)
	if r.Verdict != VerdictReview {
		t.Errorf("expect review, got %s", r.Verdict)
	}
}

func TestEngine_CrossBorderHighFreq(t *testing.T) {
	in := mkInput()
	in.Recent1h = []ChargeFact{
		{Country: "us"}, {Country: "gb"}, {Country: "de"}, {Country: "fr"}, {Country: "jp"},
	}
	r := NewEngine().Eval(context.Background(), in)
	if r.Verdict != VerdictReview {
		t.Errorf("expect review, got %s", r.Verdict)
	}
}

func TestEngine_WatchlistBlock(t *testing.T) {
	in := mkInput()
	in.Watchlists.BlockedCustomers = map[string]bool{"c1": true}
	r := NewEngine().Eval(context.Background(), in)
	if r.Verdict != VerdictBlock {
		t.Errorf("expect block, got %s", r.Verdict)
	}
}

func TestEngine_BlockShortCircuitsReview(t *testing.T) {
	in := mkInput()
	// 同时触发 review 和 block,预期 block
	in.Avg30d = 10000
	in.Today = 1000000
	in.CountryOrigin = "KP"
	r := NewEngine().Eval(context.Background(), in)
	if r.Verdict != VerdictBlock {
		t.Errorf("expect block (short-circuit), got %s", r.Verdict)
	}
}
