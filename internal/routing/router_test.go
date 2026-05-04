package routing

import "testing"

func TestRouter_Basic(t *testing.T) {
	r := NewRouter([]Rule{
		{Priority: 100, Country: "PH", PaymentMethod: "GCASH", Adapter: "gcash"},
		{Priority: 100, Country: "PH", PaymentMethod: "MAYA", Adapter: "maya"},
		{Priority: 100, Country: "PH", PaymentMethod: "GRABPAY", Adapter: "grabpay"},
		{Priority: 100, Country: "PH", PaymentMethod: "INSTAPAY", Adapter: "instapay"},
		{Priority: 100, Country: "PH", PaymentMethod: "PESONET", AmountMax: 30000000, Adapter: "pesonet"},
		// 通配兜底
		{Priority: 999, Adapter: "gcash"},
	})
	cases := []struct {
		name string
		in   MatchInput
		want string
	}{
		{"gcash", MatchInput{Country: "PH", PaymentMethod: "GCASH", Amount: 10000}, "gcash"},
		{"maya", MatchInput{Country: "PH", PaymentMethod: "MAYA", Amount: 10000}, "maya"},
		{"grabpay lowercase", MatchInput{Country: "ph", PaymentMethod: "grabpay", Amount: 10000}, "grabpay"},
		{"fallback", MatchInput{Country: "PH", PaymentMethod: "UNKNOWN", Amount: 10000}, "gcash"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := r.Route(c.in)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != c.want {
				t.Fatalf("want %s got %s", c.want, got)
			}
		})
	}
}

func TestRouter_MerchantOverride(t *testing.T) {
	r := NewRouter([]Rule{
		{Priority: 100, Country: "PH", PaymentMethod: "GCASH", Adapter: "gcash"},
		{Priority: 50, Merchant: "vip-mch", Country: "PH", PaymentMethod: "GCASH", Adapter: "gcash_vip"},
	})
	got, err := r.Route(MatchInput{Merchant: "vip-mch", Country: "PH", PaymentMethod: "GCASH"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "gcash_vip" {
		t.Fatalf("want gcash_vip got %s", got)
	}
}

func TestRouter_NoMatch(t *testing.T) {
	r := NewRouter([]Rule{{Priority: 100, Country: "PH", PaymentMethod: "GCASH", Adapter: "gcash"}})
	_, err := r.Route(MatchInput{Country: "SG", PaymentMethod: "GCASH"})
	if err == nil {
		t.Fatal("expected ErrNoMatch")
	}
}

func TestRouter_AmountBands(t *testing.T) {
	r := NewRouter([]Rule{
		{Priority: 50, Country: "PH", PaymentMethod: "BANK", AmountMax: 5000000, Adapter: "instapay"},   // ≤₱50k
		{Priority: 60, Country: "PH", PaymentMethod: "BANK", AmountMin: 5000001, Adapter: "pesonet"},    // >₱50k
	})
	got, _ := r.Route(MatchInput{Country: "PH", PaymentMethod: "BANK", Amount: 100000})
	if got != "instapay" {
		t.Fatalf("want instapay, got %s", got)
	}
	got, _ = r.Route(MatchInput{Country: "PH", PaymentMethod: "BANK", Amount: 6000000})
	if got != "pesonet" {
		t.Fatalf("want pesonet, got %s", got)
	}
}
