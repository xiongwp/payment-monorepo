package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
)

func mkBehaviorRule(t *testing.T, cfg BehaviorAnomalyConfig) engine.Rule {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	r, err := BehaviorAnomalyFactory()("rid", "name", true, raw)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return r
}

func TestBehavior_TooFastCheckout(t *testing.T) {
	r := mkBehaviorRule(t, BehaviorAnomalyConfig{MinTimeToCheckoutMs: 3000})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{TimeToCheckoutMs: 500})
	if hit == nil {
		t.Fatal("500ms < 3000ms; expected hit")
	}
}

func TestBehavior_NormalCheckoutNoHit(t *testing.T) {
	r := mkBehaviorRule(t, BehaviorAnomalyConfig{MinTimeToCheckoutMs: 3000})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{TimeToCheckoutMs: 5000})
	if hit != nil {
		t.Fatalf("5000ms >= 3000ms; expected no hit; got %+v", hit)
	}
}

func TestBehavior_LowMouseEntropy(t *testing.T) {
	r := mkBehaviorRule(t, BehaviorAnomalyConfig{MinMouseEntropy: 0.5})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{MouseMovementEntropy: 0.1})
	if hit == nil {
		t.Fatal("entropy 0.1 < 0.5; expected hit")
	}
}

func TestBehavior_BotClickInterval(t *testing.T) {
	r := mkBehaviorRule(t, BehaviorAnomalyConfig{BotClickIntervalMs: 50})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{ClickIntervalMs: 30})
	if hit == nil {
		t.Fatal("30ms clicks < 50ms threshold; expected hit")
	}
}

func TestBehavior_TypingRhythmConstant(t *testing.T) {
	r := mkBehaviorRule(t, BehaviorAnomalyConfig{MaxTypingRhythmCV: 0.05})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{TypingRhythmCV: 0.01})
	if hit == nil {
		t.Fatal("CV 0.01 < 0.05; expected hit")
	}
}

func TestBehavior_PastedCardNumber(t *testing.T) {
	r := mkBehaviorRule(t, BehaviorAnomalyConfig{CardPasteBlacklist: true, Decision: "deny"})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		PastedFields: []string{"card_number", "cvc"},
	})
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatalf("expected deny on pasted card+cvc, got %+v", hit)
	}
}

func TestBehavior_PastedNonSensitiveOK(t *testing.T) {
	r := mkBehaviorRule(t, BehaviorAnomalyConfig{CardPasteBlacklist: true})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		PastedFields: []string{"first_name", "email"},
	})
	if hit != nil {
		t.Fatalf("non-sensitive paste should not hit; got %+v", hit)
	}
}

func TestBehavior_AllOffNoHit(t *testing.T) {
	r := mkBehaviorRule(t, BehaviorAnomalyConfig{})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		TimeToCheckoutMs:     1, // 极快但 cfg 没启用
		MouseMovementEntropy: 0.001,
		ClickIntervalMs:      1,
		TypingRhythmCV:       0.001,
		PastedFields:         []string{"card_number"},
	})
	if hit != nil {
		t.Fatalf("all triggers off → no hit; got %+v", hit)
	}
}

func TestBehavior_MultipleSignalsConcatDetail(t *testing.T) {
	r := mkBehaviorRule(t, BehaviorAnomalyConfig{
		MinTimeToCheckoutMs: 3000,
		MinMouseEntropy:     0.5,
	})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		TimeToCheckoutMs:     500,
		MouseMovementEntropy: 0.1,
	})
	if hit == nil {
		t.Fatal("expected hit")
	}
	if len(hit.Detail) < 30 { // 至少有两条原因拼接
		t.Fatalf("expected verbose detail, got %q", hit.Detail)
	}
}
