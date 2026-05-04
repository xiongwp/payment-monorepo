package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
)

func mkMLRule(t *testing.T, cfg MLThresholdConfig) engine.Rule {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	r, err := MLThresholdFactory()("rid", "ml", true, raw)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return r
}

func TestMLThreshold_HitAboveThreshold(t *testing.T) {
	r := mkMLRule(t, MLThresholdConfig{Threshold: 0.7})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{MLScore: 0.85, MLModelVer: "v1"})
	if hit == nil {
		t.Fatal("0.85 > 0.7; expected hit")
	}
}

func TestMLThreshold_NoHitBelowOrEqualThreshold(t *testing.T) {
	r := mkMLRule(t, MLThresholdConfig{Threshold: 0.7})
	if hit := r.Evaluate(context.Background(), &engine.TxnContext{MLScore: 0.5}); hit != nil {
		t.Fatal("0.5 < 0.7; expected no hit")
	}
	if hit := r.Evaluate(context.Background(), &engine.TxnContext{MLScore: 0.7}); hit != nil {
		t.Fatal("0.7 == 0.7 (strict >); expected no hit")
	}
}

func TestMLThreshold_FailOpenDefault_NoModelNoHit(t *testing.T) {
	r := mkMLRule(t, MLThresholdConfig{Threshold: 0.7})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{MLScore: 0})
	if hit != nil {
		t.Fatal("MLScore=0 + fail_open default → no hit")
	}
}

func TestMLThreshold_FailCloseExplicit_NoModelHit(t *testing.T) {
	failClose := false
	r := mkMLRule(t, MLThresholdConfig{Threshold: 0.7, FailOpen: &failClose})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{MLScore: 0})
	if hit == nil {
		t.Fatal("MLScore=0 + fail_open=false → expected hit (fail-close)")
	}
}

func TestMLThreshold_Deny(t *testing.T) {
	r := mkMLRule(t, MLThresholdConfig{Threshold: 0.5, Decision: "deny"})
	hit := r.Evaluate(context.Background(), &engine.TxnContext{MLScore: 0.9})
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatalf("expected deny, got %+v", hit)
	}
}

func TestMLThreshold_FactoryRejectsBadThreshold(t *testing.T) {
	for _, th := range []float64{0, -0.1, 1.5} {
		raw, _ := json.Marshal(MLThresholdConfig{Threshold: th})
		if _, err := MLThresholdFactory()("r", "n", true, raw); err == nil {
			t.Errorf("expected error for threshold %v", th)
		}
	}
}
