package vault

import (
	"context"
	"testing"

	"reconcile-system/packages/tokenization-vault/internal/domain"
	"reconcile-system/packages/tokenization-vault/internal/providers"
	"reconcile-system/packages/tokenization-vault/internal/providers/inhouse"
	"reconcile-system/packages/tokenization-vault/internal/providers/vts"
	"reconcile-system/packages/tokenization-vault/internal/store"
)

func newTestVault() *Vault {
	return &Vault{
		Store: store.NewMemStore(),
		Providers: map[domain.TokenProvider]providers.Provider{
			domain.ProviderVTS:     vts.New([]byte("test-vts-key")),
			domain.ProviderInhouse: inhouse.New(),
		},
		DEK:      []byte("01234567890123456789012345678901"),
		TokenEnv: "test",
	}
}

// PAN 用 Luhn 合法的测试卡号 (visa)
const testVisaPAN = "4242424242424242"

func TestExchange_NewToken(t *testing.T) {
	v := newTestVault()
	res, err := v.Exchange(context.Background(), domain.ExchangeRequest{
		PAN:        testVisaPAN,
		ExpMonth:   12,
		ExpYear:    2028,
		Cardholder: "John Doe",
		MerchantID: "m_1",
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if !startsWith(res.Token, "tk_test_") {
		t.Errorf("token prefix wrong: %s", res.Token)
	}
	if res.Brand != domain.BrandVisa {
		t.Errorf("brand = %s, want visa", res.Brand)
	}
	if res.PANLast4 != "4242" {
		t.Errorf("last4 = %s", res.PANLast4)
	}
	if res.BIN != "424242" {
		t.Errorf("bin = %s", res.BIN)
	}
}

func TestExchange_DedupSameMerchantSamePAN(t *testing.T) {
	v := newTestVault()
	r1, _ := v.Exchange(context.Background(), domain.ExchangeRequest{
		PAN: testVisaPAN, ExpMonth: 12, ExpYear: 2028, MerchantID: "m_1",
	})
	r2, _ := v.Exchange(context.Background(), domain.ExchangeRequest{
		PAN: testVisaPAN, ExpMonth: 12, ExpYear: 2028, MerchantID: "m_1",
	})
	if r1.Token != r2.Token {
		t.Errorf("dedup failed: %s vs %s", r1.Token, r2.Token)
	}
}

func TestExchange_DifferentMerchantsDifferentTokens(t *testing.T) {
	v := newTestVault()
	r1, _ := v.Exchange(context.Background(), domain.ExchangeRequest{
		PAN: testVisaPAN, MerchantID: "m_1",
	})
	r2, _ := v.Exchange(context.Background(), domain.ExchangeRequest{
		PAN: testVisaPAN, MerchantID: "m_2",
	})
	if r1.Token == r2.Token {
		t.Error("same token across merchants violates merchant-scoped tokenization")
	}
}

func TestExchange_RejectInvalidLuhn(t *testing.T) {
	v := newTestVault()
	_, err := v.Exchange(context.Background(), domain.ExchangeRequest{
		PAN: "4242424242424243", // Luhn check 错位
		MerchantID: "m_1",
	})
	if err != ErrInvalidPAN {
		t.Errorf("expected ErrInvalidPAN, got %v", err)
	}
}

func TestChargeIntent_GeneratesCryptogram(t *testing.T) {
	v := newTestVault()
	exch, _ := v.Exchange(context.Background(), domain.ExchangeRequest{
		PAN: testVisaPAN, ExpMonth: 12, ExpYear: 2028, MerchantID: "m_1",
	})
	// 先 provision (async 后兜底显式调一次保证 sync test)
	_, _ = v.Provision(context.Background(), exch.Token)

	out, err := v.ChargeIntent(context.Background(), domain.ChargeIntentRequest{
		Token:      exch.Token,
		MerchantID: "m_1",
		Amount:     1999,
		Currency:   "USD",
		IntentID:   "intent_1",
	})
	if err != nil {
		t.Fatalf("charge intent: %v", err)
	}
	if out.NetworkToken == "" || out.Cryptogram == "" {
		t.Errorf("missing network_token / cryptogram: %+v", out)
	}
	if out.Provider != domain.ProviderVTS {
		t.Errorf("provider = %s, want vts (visa BIN)", out.Provider)
	}
}

func TestChargeIntent_RecurringChangesECI(t *testing.T) {
	v := newTestVault()
	ex, _ := v.Exchange(context.Background(), domain.ExchangeRequest{
		PAN: testVisaPAN, MerchantID: "m_1",
	})
	_, _ = v.Provision(context.Background(), ex.Token)
	cit, _ := v.ChargeIntent(context.Background(), domain.ChargeIntentRequest{
		Token: ex.Token, Amount: 100, Currency: "USD",
	})
	mit, _ := v.ChargeIntent(context.Background(), domain.ChargeIntentRequest{
		Token: ex.Token, Amount: 100, Currency: "USD", Recurring: true,
	})
	if cit.ECI == mit.ECI {
		t.Errorf("CIT and MIT should have different ECI, both = %s", cit.ECI)
	}
}

func TestSuspend_BlocksCharge(t *testing.T) {
	v := newTestVault()
	ex, _ := v.Exchange(context.Background(), domain.ExchangeRequest{
		PAN: testVisaPAN, MerchantID: "m_1",
	})
	_, _ = v.Provision(context.Background(), ex.Token)

	if err := v.SuspendToken(context.Background(), ex.Token, "fraud_review"); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	_, err := v.ChargeIntent(context.Background(), domain.ChargeIntentRequest{
		Token: ex.Token, Amount: 100, Currency: "USD",
	})
	if err != ErrInactive {
		t.Errorf("expected ErrInactive, got %v", err)
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
