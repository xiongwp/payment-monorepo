package mlscore

import (
	"context"
	"testing"
)

func TestLogistic_BaselineLowScore(t *testing.T) {
	s := NewLogisticService()
	r, err := s.Score(context.Background(), Features{
		Amount:               5000, // 50 minor unit
		Country:              "PH",
		FingerprintHash:      "abc",
		HardwareConcurrency:  8,
		MouseMovementEntropy: 3.2,
		KeystrokeCount:       42,
		TimeToCheckoutMs:     12000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Score >= 0.2 {
		t.Fatalf("clean baseline should be < 0.2, got %.3f", r.Score)
	}
}

func TestLogistic_ManyFraudSignalsHighScore(t *testing.T) {
	s := NewLogisticService()
	r, _ := s.Score(context.Background(), Features{
		Amount:              500000, // > 100k → HighAmount
		Country:             "RU",   // HighRiskCountry
		IPCountry:           "US",   // mismatch
		IPProxy:             true,
		IPVPN:               true,
		IPDataCenter:        true,
		FingerprintHash:     "", // missing
		WebGLRenderer:       "Google SwiftShader",
		HardwareConcurrency: 1,
		TimeToCheckoutMs:    500,
		MouseMovementEntropy: 0,
		KeystrokeCount:      0,
		TypingRhythmCV:      0.01,
	})
	if r.Score < 0.95 {
		t.Fatalf("max-signals fraud should be ≥ 0.95, got %.3f", r.Score)
	}
}

func TestLogistic_ModerateRisk(t *testing.T) {
	// IP VPN 单独 → 中等风险
	s := NewLogisticService()
	r, _ := s.Score(context.Background(), Features{
		IPVPN:               true,
		FingerprintHash:     "abc",
		HardwareConcurrency: 8,
		MouseMovementEntropy: 2.0,
		KeystrokeCount:      30,
	})
	if r.Score < 0.10 || r.Score > 0.45 {
		t.Fatalf("VPN-only should be moderate, got %.3f", r.Score)
	}
}

func TestLogistic_ModelVerStable(t *testing.T) {
	s := NewLogisticService()
	r, _ := s.Score(context.Background(), Features{})
	if r.ModelVer == "" {
		t.Fatal("ModelVer must be set for audit")
	}
}

func TestLogistic_CustomConfig(t *testing.T) {
	cfg := LogisticConfig{
		ModelVer:  "test-v0.1",
		Intercept: 0,
		Weights:   FeatureWeights{IPProxy: 5.0}, // 极强 IP proxy 权重
	}
	s := NewLogisticServiceWith(cfg)
	r, _ := s.Score(context.Background(), Features{IPProxy: true})
	// sigmoid(0 + 5) = 0.993
	if r.Score < 0.99 {
		t.Fatalf("custom huge weight not applied? got %.3f", r.Score)
	}
	if r.ModelVer != "test-v0.1" {
		t.Fatalf("ModelVer wrong: %q", r.ModelVer)
	}
}

// PlattA=0 视为未校准，Score 应等价 sigmoid(z)；做 backward compat 保护：
// 旧 LogisticConfig JSON 文件不带 PlattA/B → 行为不变。
func TestLogistic_NoPlattScalingDefaultsToSigmoid(t *testing.T) {
	cfg := LogisticConfig{Intercept: 1, Weights: FeatureWeights{IPProxy: 2}}
	s := NewLogisticServiceWith(cfg)
	r, _ := s.Score(context.Background(), Features{IPProxy: true})
	// sigmoid(1+2) = 0.9526
	if r.Score < 0.94 || r.Score > 0.97 {
		t.Fatalf("expected ~0.953; got %.4f", r.Score)
	}
}

// Platt scaling A/B 接进 Score 路径：A=2, B=-3 → sigmoid(2*z-3) 而不是
// sigmoid(z)。验证管道接通。
func TestLogistic_PlattScalingAppliedWhenSet(t *testing.T) {
	cfg := LogisticConfig{
		Intercept: 1,
		Weights:   FeatureWeights{IPProxy: 2},
		PlattA:    2,
		PlattB:    -3,
	}
	s := NewLogisticServiceWith(cfg)
	r, _ := s.Score(context.Background(), Features{IPProxy: true})
	// raw z=3，calibrated = sigmoid(2*3-3) = sigmoid(3) = 0.9526
	if r.Score < 0.94 || r.Score > 0.97 {
		t.Fatalf("expected ~0.953; got %.4f", r.Score)
	}
}
