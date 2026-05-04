package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// helpers

func mustJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

// ── link_fanout_multihop ─────────────────────────────────────────

func TestLinkFanoutMultihop_2HopRingDetected(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	// 1 hop from device:A: customer:1 only
	// 2 hop from device:A: customer:1, device:B, customer:2, device:C, customer:3, device:D
	for i := 1; i <= 4; i++ {
		c := "customer:c" + itos(i)
		d := "device:d" + itos(i)
		ls.Link(ctx, "device:A", c)
		ls.Link(ctx, c, d)
	}
	r, err := LinkFanoutMultihopFactory(ls)("ring", "ring", true,
		mustJSON(t, `{"pivot":"device","peer":"customer","hops":2,"threshold":3,"decision":"review"}`))
	if err != nil {
		t.Fatal(err)
	}
	hit := r.Evaluate(ctx, &engine.TxnContext{DeviceID: "A"})
	if hit == nil {
		t.Fatal("expected ring detection at 2 hops")
	}
	if hit.Decision != engine.Review {
		t.Fatalf("default review, got %s", hit.Decision)
	}
}

func TestLinkFanoutMultihop_BelowThreshold(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	ls.Link(ctx, "device:A", "customer:c1")
	ls.Link(ctx, "customer:c1", "device:B")
	r, _ := LinkFanoutMultihopFactory(ls)("r1", "", true,
		mustJSON(t, `{"pivot":"device","peer":"customer","hops":2,"threshold":5}`))
	if hit := r.Evaluate(ctx, &engine.TxnContext{DeviceID: "A"}); hit != nil {
		t.Fatalf("below threshold should not hit, got %+v", hit)
	}
}

// ── graph_reputation ─────────────────────────────────────────────

func TestGraphReputation_FraudNeighbor1Hop(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	ls.Link(ctx, "customer:victim", "device:shared")
	ls.Tag(ctx, "device:shared", "fraud")

	r, err := GraphReputationFactory(ls)("rep", "rep", true, mustJSON(t,
		`{"hops":2,"hop_decay":0.5,"pivots":["customer"],
		   "tag_weights":{"fraud":40},"review_min":20,"deny_min":50}`))
	if err != nil {
		t.Fatal(err)
	}
	// victim 1 跳就看到 device:shared 的 fraud tag → score = 1*40*1.0 = 40
	hit := r.Evaluate(ctx, &engine.TxnContext{CustomerID: "victim"})
	if hit == nil || hit.Decision != engine.Review {
		t.Fatalf("expected REVIEW with fraud at 1-hop, got %+v", hit)
	}
}

func TestGraphReputation_NoTagsBenign(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	ls.Link(ctx, "customer:alice", "device:phone")
	r, _ := GraphReputationFactory(ls)("rep", "", true, mustJSON(t,
		`{"hops":2,"tag_weights":{"fraud":40}}`))
	if hit := r.Evaluate(ctx, &engine.TxnContext{CustomerID: "alice"}); hit != nil {
		t.Fatalf("clean graph should not hit, got %+v", hit)
	}
}

// ── cross_merchant_link ──────────────────────────────────────────

func TestCrossMerchantLink_DeviceAcrossManyMerchants(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		ls.Link(ctx, "device:roaming", "merchant:m"+itos(i))
	}
	r, err := CrossMerchantLinkFactory(ls)("xm", "xm", true, mustJSON(t,
		`{"pivots":["device"],"threshold":3,"decision":"review"}`))
	if err != nil {
		t.Fatal(err)
	}
	hit := r.Evaluate(ctx, &engine.TxnContext{DeviceID: "roaming", MerchantID: "m1"})
	if hit == nil || hit.Decision != engine.Review {
		t.Fatalf("3 merchants ≥ threshold 3 should hit review, got %+v", hit)
	}
}

func TestCrossMerchantLink_SingleMerchant(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	ls.Link(ctx, "device:home", "merchant:m1")
	r, _ := CrossMerchantLinkFactory(ls)("xm", "", true,
		mustJSON(t, `{"pivots":["device"],"threshold":3}`))
	if hit := r.Evaluate(ctx, &engine.TxnContext{DeviceID: "home", MerchantID: "m1"}); hit != nil {
		t.Fatalf("single merchant should not hit, got %+v", hit)
	}
}

// ── card_testing ─────────────────────────────────────────────────

func TestCardTesting_SameCustomerManyCards(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	for i := 1; i <= 4; i++ {
		ls.Link(ctx, "customer:tester", "card:fp"+itos(i))
	}
	r, err := CardTestingFactory(ls)("ct", "card testing", true, mustJSON(t,
		`{"pivots":["customer"],"threshold":3,"decision":"deny"}`))
	if err != nil {
		t.Fatal(err)
	}
	hit := r.Evaluate(ctx, &engine.TxnContext{CustomerID: "tester"})
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatalf("4 cards ≥ threshold 3 should DENY, got %+v", hit)
	}
}

func TestCardTesting_NormalUser(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	ls.Link(ctx, "customer:alice", "card:fp1")
	r, _ := CardTestingFactory(ls)("ct", "", true,
		mustJSON(t, `{"pivots":["customer"],"threshold":3}`))
	if hit := r.Evaluate(ctx, &engine.TxnContext{CustomerID: "alice"}); hit != nil {
		t.Fatalf("normal 1-card user should not hit, got %+v", hit)
	}
}

func itos(i int) string {
	if i < 0 {
		return "-"
	}
	const digits = "0123456789"
	if i < 10 {
		return string(digits[i])
	}
	return itos(i/10) + string(digits[i%10])
}
