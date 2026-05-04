package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

func TestAmountLimit_PerTxn(t *testing.T) {
	counter := store.NewMemCounter()
	factory := AmountLimitFactory(counter)
	r, err := factory("r1", "max 50k", true, json.RawMessage(`{"max_per_txn": 5000000, "scope_by": "merchant"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Under limit
	hit := r.Evaluate(context.Background(), &engine.TxnContext{Amount: 4000000, MerchantID: "m1"})
	if hit != nil {
		t.Fatal("4M should be under 5M limit")
	}
	// Over limit
	hit = r.Evaluate(context.Background(), &engine.TxnContext{Amount: 6000000, MerchantID: "m1"})
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatal("6M should exceed 5M per-txn limit")
	}
}

func TestAmountLimit_DailyExceeded(t *testing.T) {
	counter := store.NewMemCounter()
	factory := AmountLimitFactory(counter)
	r, _ := factory("r2", "daily 1M", true, json.RawMessage(`{"max_daily": 1000000, "scope_by": "customer"}`))

	ctx := context.Background()
	// Accumulate 800k
	counter.Incr(ctx, "customer:c1", 800000)

	// 800k + 100k = 900k → allow
	hit := r.Evaluate(ctx, &engine.TxnContext{Amount: 100000, CustomerID: "c1"})
	if hit != nil {
		t.Fatal("900k total should be under 1M")
	}

	// 800k + 300k = 1.1M → deny
	hit = r.Evaluate(ctx, &engine.TxnContext{Amount: 300000, CustomerID: "c1"})
	if hit == nil {
		t.Fatal("1.1M total should exceed daily limit")
	}
	if hit.Decision != engine.Deny {
		t.Fatalf("want Deny got %s", hit.Decision)
	}
}

func TestVelocity(t *testing.T) {
	counter := store.NewMemCounter()
	factory := VelocityFactory(counter)
	r, _ := factory("v1", "3 per 5 min", true, json.RawMessage(`{"max_count": 3, "window_min": 5, "scope_by": "customer"}`))

	ctx := context.Background()
	txn := &engine.TxnContext{Amount: 1000, CustomerID: "c1"}

	// 3 transactions
	for i := 0; i < 3; i++ {
		counter.Incr(ctx, "customer:c1", 1000)
	}

	// 4th should be denied
	hit := r.Evaluate(ctx, txn)
	if hit == nil {
		t.Fatal("4th txn in 5min window should be denied")
	}
}

func TestBlacklist(t *testing.T) {
	bl := store.NewMemBlacklist()
	factory := BlacklistFactory(bl)
	r, _ := factory("bl1", "ip blacklist", true, json.RawMessage(`{"dimension": "ip"}`))

	ctx := context.Background()
	txn := &engine.TxnContext{IPAddress: "1.2.3.4"}

	// Not blocked
	hit := r.Evaluate(ctx, txn)
	if hit != nil {
		t.Fatal("should not be blocked")
	}

	// Add to blacklist
	bl.Add(ctx, "ip", "1.2.3.4", "fraud")

	// Now blocked
	hit = r.Evaluate(ctx, txn)
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatal("blacklisted IP should be denied")
	}

	// Remove
	bl.Remove(ctx, "ip", "1.2.3.4")
	hit = r.Evaluate(ctx, txn)
	if hit != nil {
		t.Fatal("removed from blacklist should not be blocked")
	}
}

func TestCountryBlock_Whitelist(t *testing.T) {
	factory := CountryBlockFactory()
	r, _ := factory("cb1", "PH only", true, json.RawMessage(`{"mode": "allow", "countries": ["PH"]}`))

	ctx := context.Background()
	// PH → allow
	hit := r.Evaluate(ctx, &engine.TxnContext{Country: "PH"})
	if hit != nil {
		t.Fatal("PH should be allowed")
	}
	// US → deny
	hit = r.Evaluate(ctx, &engine.TxnContext{Country: "US"})
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatal("US should be denied by PH whitelist")
	}
}

func TestCountryBlock_Blacklist(t *testing.T) {
	factory := CountryBlockFactory()
	r, _ := factory("cb2", "block CN", true, json.RawMessage(`{"mode": "deny", "countries": ["CN"]}`))

	ctx := context.Background()
	hit := r.Evaluate(ctx, &engine.TxnContext{Country: "PH"})
	if hit != nil {
		t.Fatal("PH should be allowed")
	}
	hit = r.Evaluate(ctx, &engine.TxnContext{Country: "CN"})
	if hit == nil {
		t.Fatal("CN should be blocked")
	}
}
