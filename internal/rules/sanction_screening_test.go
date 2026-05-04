package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/sanction"
)

func newSanctionSvc(t *testing.T) sanction.Service {
	t.Helper()
	s := sanction.NewMemService()
	_ = s.Reload(context.Background(), []*sanction.Entry{
		{Source: sanction.SourceOFAC, UID: "1", Name: "OSAMA BIN LADEN", Country: "SA"},
		{Source: sanction.SourceEU, UID: "2", Name: "BAD ENTITY LLC", Type: "entity"},
	})
	return s
}

func newSanctionRule(t *testing.T, cfg string, svc sanction.Service) engine.Rule {
	t.Helper()
	r, err := SanctionScreeningFactory(svc)("s1", "sanction", true, json.RawMessage(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSanction_HitDeniesWithForce(t *testing.T) {
	r := newSanctionRule(t, `{"required":false,"decision":"deny"}`, newSanctionSvc(t))
	h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"full_name": "Osama Bin Laden", "country": "SA"},
	})
	if h == nil || h.Decision != engine.Deny || !h.Force {
		t.Fatalf("expected force deny, got %+v", h)
	}
}

func TestSanction_NoNameRequiredReview(t *testing.T) {
	r := newSanctionRule(t, `{"required":true}`, newSanctionSvc(t))
	h := r.Evaluate(context.Background(), &engine.TxnContext{})
	if h == nil || h.Decision != engine.Review || !h.Force {
		t.Fatalf("missing full_name in required mode should force review, got %+v", h)
	}
}

func TestSanction_NoNameSkipMode(t *testing.T) {
	r := newSanctionRule(t, `{"required":false}`, newSanctionSvc(t))
	if h := r.Evaluate(context.Background(), &engine.TxnContext{}); h != nil {
		t.Fatalf("missing full_name in skip mode should not hit, got %+v", h)
	}
}

func TestSanction_NormalNameNoHit(t *testing.T) {
	r := newSanctionRule(t, `{"required":false}`, newSanctionSvc(t))
	h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"full_name": "Alice Smith"},
	})
	if h != nil {
		t.Fatalf("normal name should pass, got %+v", h)
	}
}
