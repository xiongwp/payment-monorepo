package provider

import (
	"context"
	"testing"
	"time"
)

func TestStatic_LatestRate(t *testing.T) {
	p := NewStaticProvider("test", 0.01)
	p.SetRate("USD", "EUR", 0.92)
	r, err := p.LatestRate(context.Background(), "USD", "EUR")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r.Mid != 0.92 || r.Provider != "test" {
		t.Errorf("rate wrong: %+v", r)
	}
	if r.Bid >= r.Ask {
		t.Errorf("bid >= ask: %f >= %f", r.Bid, r.Ask)
	}
}

func TestStatic_NoRate(t *testing.T) {
	p := NewStaticProvider("test", 0.01)
	_, err := p.LatestRate(context.Background(), "USD", "XYZ")
	if err == nil {
		t.Fatal("expect err for unknown rate")
	}
}

func TestStatic_QuoteLock(t *testing.T) {
	p := NewStaticProvider("test", 0.01)
	p.SetRate("USD", "EUR", 0.92)
	q, err := p.QuoteLock(context.Background(), &QuoteRequest{
		From: "USD", To: "EUR", Amount: 10000, TTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if q.AmountOut == 0 {
		t.Errorf("amount_out 0")
	}
	if q.ExpiresAt.Before(time.Now()) {
		t.Errorf("quote already expired")
	}
}

func TestReutersStub_RequiresAPIKey(t *testing.T) {
	if _, err := NewReutersProvider(ReutersConfig{}); err == nil {
		t.Fatal("expect err without APIKey")
	}
	p, err := NewReutersProvider(ReutersConfig{APIKey: "fake"})
	if err != nil || p == nil {
		t.Errorf("expect provider, got err=%v p=%v", err, p)
	}
}

func TestOandaStub_RequiresAccountAndToken(t *testing.T) {
	if _, err := NewOandaProvider(OandaConfig{}); err == nil {
		t.Fatal("expect err without account/token")
	}
}
