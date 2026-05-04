package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// ─── account_age ─────────────────────────────────────────────────

func newAccountAgeRule(t *testing.T, raw string) engine.Rule {
	t.Helper()
	r, err := AccountAgeFactory()("aa", "account_age", true, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAccountAge_NewAccountWithdrawHits(t *testing.T) {
	r := newAccountAgeRule(t, `{"min_days":1,"apply_events":["withdraw"],"decision":"deny"}`)
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		EventType: "withdraw",
		Metadata:  map[string]string{"account_age_days": "0"},
	})
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatalf("want Deny hit, got %+v", hit)
	}
}

func TestAccountAge_OldAccountPasses(t *testing.T) {
	r := newAccountAgeRule(t, `{"min_days":1,"apply_events":["withdraw"]}`)
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		EventType: "withdraw",
		Metadata:  map[string]string{"account_age_days": "30"},
	})
	if hit != nil {
		t.Fatalf("30d old should not hit, got %+v", hit)
	}
}

func TestAccountAge_OnlyAppliesToConfiguredEvents(t *testing.T) {
	r := newAccountAgeRule(t, `{"min_days":1,"apply_events":["withdraw"]}`)
	// register 事件不该被这条规则拦
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		EventType: "register",
		Metadata:  map[string]string{"account_age_days": "0"},
	})
	if hit != nil {
		t.Fatalf("register event should not be evaluated, got %+v", hit)
	}
}

func TestAccountAge_MissingMetadataDoesNotHit(t *testing.T) {
	r := newAccountAgeRule(t, `{"min_days":1,"apply_events":["withdraw"]}`)
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		EventType: "withdraw",
		Metadata:  map[string]string{},
	})
	if hit != nil {
		t.Fatalf("missing metadata should not hit (caller may not be wired), got %+v", hit)
	}
}

// ─── disposable_email ────────────────────────────────────────────

func newDisposableRule(t *testing.T, raw string) engine.Rule {
	t.Helper()
	r, err := DisposableEmailFactory()("de", "disposable_email", true, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDisposableEmail_BuiltinHit(t *testing.T) {
	r := newDisposableRule(t, `{"apply_events":["register"]}`)
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		EventType: "register",
		Metadata:  map[string]string{"email_domain": "Mailinator.com"}, // 大小写不敏感
	})
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatalf("want Deny hit on builtin domain, got %+v", hit)
	}
}

func TestDisposableEmail_LegitDomainNoHit(t *testing.T) {
	r := newDisposableRule(t, `{"apply_events":["register"]}`)
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		EventType: "register",
		Metadata:  map[string]string{"email_domain": "gmail.com"},
	})
	if hit != nil {
		t.Fatalf("gmail.com should not hit, got %+v", hit)
	}
}

func TestDisposableEmail_ExtraDomain(t *testing.T) {
	r := newDisposableRule(t, `{"apply_events":["register"],"extra_domains":["custom-temp.io"]}`)
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		EventType: "register",
		Metadata:  map[string]string{"email_domain": "custom-temp.io"},
	})
	if hit == nil {
		t.Fatalf("extra_domain should hit")
	}
}

func TestDisposableEmail_EventScoping(t *testing.T) {
	r := newDisposableRule(t, `{"apply_events":["register"]}`)
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		EventType: "login", // 不在 apply_events 里
		Metadata:  map[string]string{"email_domain": "mailinator.com"},
	})
	if hit != nil {
		t.Fatalf("login event should be skipped, got %+v", hit)
	}
}
