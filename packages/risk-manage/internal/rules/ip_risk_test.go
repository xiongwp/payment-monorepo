package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
)

func mkIPRule(t *testing.T, cfg IPRiskConfig) engine.Rule {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	r, err := IPRiskFactory()("rid", "name", true, raw)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return r
}

func TestIPRisk_BlocksProxy(t *testing.T) {
	r := mkIPRule(t, IPRiskConfig{BlockProxy: true})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{IPProxy: true})
	if hit == nil || hit.Decision != engine.Review {
		t.Fatalf("expected proxy hit, got %+v", hit)
	}
}

func TestIPRisk_BlocksVPNDeny(t *testing.T) {
	r := mkIPRule(t, IPRiskConfig{BlockVPN: true, Decision: "deny"})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{IPVPN: true})
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatalf("expected VPN deny, got %+v", hit)
	}
}

func TestIPRisk_CountryMismatch(t *testing.T) {
	r := mkIPRule(t, IPRiskConfig{CountryMismatch: true})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{Country: "PH", IPCountry: "US"})
	if hit == nil {
		t.Fatal("PH txn from US ip should hit")
	}
}

func TestIPRisk_NoMismatchWhenCountriesAgree(t *testing.T) {
	r := mkIPRule(t, IPRiskConfig{CountryMismatch: true})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{Country: "PH", IPCountry: "PH"})
	if hit != nil {
		t.Fatal("matching countries should not hit")
	}
}

func TestIPRisk_NoTriggersAllOff(t *testing.T) {
	r := mkIPRule(t, IPRiskConfig{}) // 全关
	hit := r.Evaluate(context.Background(), &engine.TxnContext{IPProxy: true, IPVPN: true})
	if hit != nil {
		t.Fatalf("all triggers off → no hit; got %+v", hit)
	}
}
