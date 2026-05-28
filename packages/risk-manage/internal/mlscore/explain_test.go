package mlscore

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestLogisticExplainer_HighRiskFeaturesSorted(t *testing.T) {
	e := NewLogisticExplainer()
	r, err := e.Explain(context.Background(), Features{
		Amount:           500000, // > 100k → high_amount (w=0.5)
		IPVPN:            true,   // w=0.8
		IPProxy:          true,   // w=0.7
		FingerprintHash:  "",     // no_fingerprint w=0.6
		TimeToCheckoutMs: 500,    // rapid_checkout w=1.2 (强信号)
		KeystrokeCount:   5,      // 防 no_keystrokes 触发
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.TopContributors) != 3 {
		t.Fatalf("topK=3 expected, got %d", len(r.TopContributors))
	}
	// 期望 rapid_checkout (1.2) > ip_vpn (0.8) > ip_proxy (0.7) — 按 |contribution| 降序
	if r.TopContributors[0].Feature != "rapid_checkout" {
		t.Errorf("top[0] expected rapid_checkout, got %s", r.TopContributors[0].Feature)
	}
	if r.TopContributors[1].Feature != "ip_vpn" {
		t.Errorf("top[1] expected ip_vpn, got %s", r.TopContributors[1].Feature)
	}
	if r.TopContributors[2].Feature != "ip_proxy" {
		t.Errorf("top[2] expected ip_proxy, got %s", r.TopContributors[2].Feature)
	}
	// 所有 top 都应该 increase_risk（正向）
	for _, c := range r.TopContributors {
		if c.Direction != "increase_risk" {
			t.Errorf("expected increase_risk for %s, got %s", c.Feature, c.Direction)
		}
	}
	// base_value 应该是 intercept = -2.5
	if math.Abs(r.BaseValue-(-2.5)) > 1e-9 {
		t.Errorf("base_value expected -2.5, got %f", r.BaseValue)
	}
	// final_score in (0, 1)
	if r.FinalScore <= 0 || r.FinalScore >= 1 {
		t.Errorf("final_score out of (0,1): %f", r.FinalScore)
	}
}

func TestLogisticExplainer_AllZeroFeatures_NoTopContributors(t *testing.T) {
	e := NewLogisticExplainer()
	// 极简空 Features — 但 KeystrokeCount=0 会触发 no_keystrokes 信号；
	// 设 KeystrokeCount > 0 + FingerprintHash 非空 → 真正全 0 信号
	r, err := e.Explain(context.Background(), Features{
		KeystrokeCount:       10,
		FingerprintHash:      "abc",
		HardwareConcurrency:  8,
		MouseMovementEntropy: 2.0,
		MousePauseCount:      3,
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.TopContributors) != 0 {
		t.Errorf("expected no top contributors for all-clean features, got %d: %+v",
			len(r.TopContributors), r.TopContributors)
	}
	// base_value 还在；final_score = sigmoid(-2.5) ≈ 0.075
	if r.BaseValue != -2.5 {
		t.Errorf("base_value expected -2.5, got %f", r.BaseValue)
	}
	if r.FinalScore >= 0.1 {
		t.Errorf("clean-baseline final_score should be < 0.1, got %f", r.FinalScore)
	}
}

func TestLogisticExplainer_TopKLimit(t *testing.T) {
	e := NewLogisticExplainer()
	// 多个信号都触发，topK=2 应该只返 2 条
	r, _ := e.Explain(context.Background(), Features{
		IPVPN:           true,
		IPProxy:         true,
		IPDataCenter:    true,
		Amount:          500000,
		FingerprintHash: "",
		KeystrokeCount:  5,
	}, 2)
	if len(r.TopContributors) != 2 {
		t.Fatalf("topK=2 expected, got %d", len(r.TopContributors))
	}
	// AllContributions 应该是全量（17 个 indicator）
	if len(r.AllContributions) < 10 {
		t.Errorf("AllContributions should be full dict, got %d", len(r.AllContributions))
	}
}

func TestLogisticExplainer_ScoreConsistency(t *testing.T) {
	// LogisticExplainer.FinalScore 应该跟 LogisticService.Score 一致。
	f := Features{
		IPVPN:           true,
		Amount:          500000,
		KeystrokeCount:  5,
		FingerprintHash: "abc",
	}
	svc := NewLogisticService()
	scored, _ := svc.Score(context.Background(), f)
	exp := NewLogisticExplainer()
	r, _ := exp.Explain(context.Background(), f, 5)
	if math.Abs(scored.Score-r.FinalScore) > 1e-9 {
		t.Errorf("score mismatch: service=%f explainer=%f", scored.Score, r.FinalScore)
	}
}

func TestOnnxShapExplainer_LoadAndExplain(t *testing.T) {
	// 模拟训练侧产的 .shap.json
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model_v1.onnx")
	meta := shapMetadata{
		BaseValue:    -1.5,
		ModelVersion: "gbdt_test_v1",
		FeatureBaselineMean: map[string]float64{
			"amount":   1500,
			"ip_proxy": 0.05,
		},
		FeatureGlobalImportance: map[string]float64{
			"amount":   0.0001,
			"ip_proxy": 1.5,
		},
	}
	data, _ := json.Marshal(meta)
	jsonPath := filepath.Join(dir, "model_v1.shap.json")
	if err := os.WriteFile(jsonPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := NewOnnxShapExplainer(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Explain(context.Background(), Features{
		Amount:  5000, // contribution = 0.0001 × (5000 - 1500) = 0.35
		IPProxy: true, // contribution = 1.5 × (1 - 0.05) = 1.425
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if r.ModelVersion != "gbdt_test_v1" {
		t.Errorf("model_version mismatch: %s", r.ModelVersion)
	}
	if r.TopContributors[0].Feature != "ip_proxy" {
		t.Errorf("top[0] expected ip_proxy, got %s", r.TopContributors[0].Feature)
	}
}

func TestOnnxShapExplainer_MissingFile(t *testing.T) {
	_, err := NewOnnxShapExplainer("/nonexistent/path/model.onnx")
	if err == nil {
		t.Fatal("expected error for missing .shap.json")
	}
}
