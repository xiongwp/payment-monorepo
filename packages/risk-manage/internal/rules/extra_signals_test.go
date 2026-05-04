package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
)

func newRule2(t *testing.T, factory engine.RuleFactory, body string) engine.Rule {
	t.Helper()
	r, err := factory("rid", "name", true, json.RawMessage(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// ── impossible_travel ─────

func TestImpossibleTravel_TooFastHits(t *testing.T) {
	r := newRule2(t, ImpossibleTravelFactory(), `{"window_min":360,"speed_kmh":1000}`)
	// 1000km in 30min = 2000 km/h → impossible
	h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"last_seen_min": "30", "geo_distance_km": "1000"},
	})
	if h == nil || h.Decision != engine.Review {
		t.Fatalf("expected impossible-travel REVIEW, got %+v", h)
	}
}

func TestImpossibleTravel_NormalSpeedNoHit(t *testing.T) {
	r := newRule2(t, ImpossibleTravelFactory(), `{}`)
	// 50km in 60min = 50 km/h → normal commute
	if h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"last_seen_min": "60", "geo_distance_km": "50"},
	}); h != nil {
		t.Fatalf("normal speed should not hit, got %+v", h)
	}
}

func TestImpossibleTravel_OutsideWindowSkipped(t *testing.T) {
	r := newRule2(t, ImpossibleTravelFactory(), `{"window_min":60}`)
	// 跨 24h 一定不算 impossible
	if h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"last_seen_min": "1440", "geo_distance_km": "5000"},
	}); h != nil {
		t.Fatalf("outside window should skip, got %+v", h)
	}
}

// ── returning_customer ─────

func TestReturningCustomer_TrustedForceAllow(t *testing.T) {
	r := newRule2(t, ReturningCustomerFactory(), `{"min_paid_count":3}`)
	h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{
			"customer_paid_count_90d":      "10",
			"customer_chargeback_count_90d": "0",
		},
	})
	if h == nil || h.Decision != engine.Allow || !h.Force {
		t.Fatalf("trusted customer should Force-Allow, got %+v", h)
	}
}

func TestReturningCustomer_AnyChargebackBlocks(t *testing.T) {
	r := newRule2(t, ReturningCustomerFactory(), `{"min_paid_count":3,"max_chargeback":0}`)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{
			"customer_paid_count_90d":      "10",
			"customer_chargeback_count_90d": "1",
		},
	}); h != nil {
		t.Fatalf("with chargeback should not trigger trust, got %+v", h)
	}
}

// ── avs_check ─────

func TestAVS_NMismatchDeny(t *testing.T) {
	r := newRule2(t, AVSCheckFactory(), `{}`)
	h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"avs_response": "N"},
	})
	if h == nil || h.Decision != engine.Deny || !h.Force {
		t.Fatalf("AVS=N should Force-Deny, got %+v", h)
	}
}

func TestAVS_PartialReview(t *testing.T) {
	r := newRule2(t, AVSCheckFactory(), `{}`)
	for _, code := range []string{"A", "Z"} {
		h := r.Evaluate(context.Background(), &engine.TxnContext{
			Metadata: map[string]string{"avs_response": code},
		})
		if h == nil || h.Decision != engine.Review {
			t.Fatalf("AVS=%s should review, got %+v", code, h)
		}
	}
}

func TestAVS_FullMatchSkip(t *testing.T) {
	r := newRule2(t, AVSCheckFactory(), `{}`)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"avs_response": "Y"},
	}); h != nil {
		t.Fatalf("AVS=Y should skip, got %+v", h)
	}
}

// ── bin_country ─────

func TestBINCountry_Mismatch(t *testing.T) {
	r := newRule2(t, BINCountryFactory(), `{}`)
	h := r.Evaluate(context.Background(), &engine.TxnContext{
		Country:  "US",
		Metadata: map[string]string{"bin_country": "RU"},
	})
	if h == nil || h.Decision != engine.Review {
		t.Fatalf("RU BIN on US merchant should review, got %+v", h)
	}
}

func TestBINCountry_Match(t *testing.T) {
	r := newRule2(t, BINCountryFactory(), `{}`)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{
		Country:  "US",
		Metadata: map[string]string{"bin_country": "US"},
	}); h != nil {
		t.Fatalf("matching countries should skip, got %+v", h)
	}
}

// ── email_validation ─────

func TestEmailValidation_Disposable(t *testing.T) {
	r := newRule2(t, EmailValidationFactory(), `{"block_disposable":true}`)
	h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"email_disposable": "true"},
	})
	if h == nil || h.Decision != engine.Deny || !h.Force {
		t.Fatalf("disposable=true with block_disposable should Force-Deny, got %+v", h)
	}
}

func TestEmailValidation_NewEmail(t *testing.T) {
	r := newRule2(t, EmailValidationFactory(), `{"min_age_days":30}`)
	h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"email_age_days": "5"},
	})
	if h == nil || h.Decision != engine.Review {
		t.Fatalf("5-day email vs min 30 should review, got %+v", h)
	}
}

func TestEmailValidation_OldClean(t *testing.T) {
	r := newRule2(t, EmailValidationFactory(), `{}`)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"email_age_days": "365"},
	}); h != nil {
		t.Fatalf("old email should skip, got %+v", h)
	}
}
