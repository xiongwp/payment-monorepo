package sandbox

import (
	"testing"

	"github.com/xiongwp/risk-manage/internal/auth"
	"github.com/xiongwp/risk-manage/internal/engine"
)

func TestDetect_TestKeyAllow(t *testing.T) {
	p := &auth.Principal{Scope: auth.ScopeMerchant, MerchantID: "m1", IsTest: true}
	r := Detect(p, &engine.TxnContext{
		MerchantID: "m1",
		Metadata:   map[string]string{"risk_test_scenario": string(ScenarioAllow)},
	})
	if r == nil || r.Decision != engine.Allow {
		t.Fatalf("expected ALLOW, got %+v", r)
	}
}

func TestDetect_TestKeyDeny(t *testing.T) {
	p := &auth.Principal{Scope: auth.ScopeMerchant, IsTest: true}
	r := Detect(p, &engine.TxnContext{CustomerID: "risk_test_deny"})
	if r == nil || r.Decision != engine.Deny || r.RecommendedAction != "block" {
		t.Fatalf("expected DENY+block, got %+v", r)
	}
}

func TestDetect_LiveKeyIgnoresSandbox(t *testing.T) {
	// 生产商户 key 即使带 risk_test_ 也不触发（防滥用）
	p := &auth.Principal{Scope: auth.ScopeMerchant, MerchantID: "m1", IsTest: false}
	r := Detect(p, &engine.TxnContext{CustomerID: "risk_test_deny"})
	if r != nil {
		t.Fatalf("live key should never trigger sandbox, got %+v", r)
	}
}

func TestDetect_NoPrincipalDevMode(t *testing.T) {
	// 没 principal（dev / 单测）→ metadata 仍可触发，方便本地开发
	r := Detect(nil, &engine.TxnContext{
		Metadata: map[string]string{"risk_test_scenario": string(ScenarioReview)},
	})
	if r == nil || r.Decision != engine.Review {
		t.Fatalf("dev mode should honor sandbox metadata, got %+v", r)
	}
}

func TestDetect_HighScoreCustomValue(t *testing.T) {
	r := Detect(nil, &engine.TxnContext{
		CustomerID: "risk_test_high_score:65",
	})
	if r == nil || r.RiskScore != 65 {
		t.Fatalf("custom score 65 expected, got %+v", r)
	}
	if r.Decision != engine.Deny { // 65 >= 50 → deny per default thresholds
		t.Fatalf("score 65 should map to DENY in sandbox helper, got %s", r.Decision)
	}
}

func TestDetect_NoScenario(t *testing.T) {
	if r := Detect(nil, &engine.TxnContext{CustomerID: "regular_user"}); r != nil {
		t.Fatalf("regular customer should not trigger sandbox: %+v", r)
	}
}

func TestDetect_CardFingerprint(t *testing.T) {
	r := Detect(nil, &engine.TxnContext{
		Metadata: map[string]string{"card_fingerprint": "risk_test_sanction_hit"},
	})
	if r == nil || r.Decision != engine.Deny {
		t.Fatalf("sanction sandbox should DENY, got %+v", r)
	}
}

func TestDetect_AdminKeyNotAffected(t *testing.T) {
	// admin scope 不是 ScopeMerchant，sandbox 还是开放（dev 模式语义）
	p := &auth.Principal{Scope: auth.ScopeAdmin}
	r := Detect(p, &engine.TxnContext{CustomerID: "risk_test_review"})
	if r == nil {
		t.Fatal("admin key should still allow sandbox triggers (dev/integration)")
	}
}
