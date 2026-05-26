// onnx_features_test.go：Features → []float32 转换的单元测试。
//
// 没有 build tag — 默认 build / -tags onnx 都跑。
// 不依赖 onnxruntime so 库（纯 Go 转换逻辑）。

package mlscore

import (
	"math"
	"testing"
)

func TestFeaturesToFloat32_BasicOrder(t *testing.T) {
	f := Features{
		Amount:              50_000,
		IPProxy:             true,
		IPVPN:               false,
		HardwareConcurrency: 4,
	}
	order := []string{"amount", "ip_proxy", "ip_vpn", "hardware_concurrency"}
	vec := featuresToFloat32(f, order, nil)
	if len(vec) != 4 {
		t.Fatalf("len=%d want 4", len(vec))
	}
	if vec[0] != 50_000 {
		t.Errorf("amount=%v want 50000", vec[0])
	}
	if vec[1] != 1 {
		t.Errorf("ip_proxy=%v want 1", vec[1])
	}
	if vec[2] != 0 {
		t.Errorf("ip_vpn=%v want 0", vec[2])
	}
	if vec[3] != 4 {
		t.Errorf("hardware_concurrency=%v want 4", vec[3])
	}
}

func TestFeaturesToFloat32_DerivedFeatures(t *testing.T) {
	f := Features{
		IPCountry:       "CN",
		Country:         "US",
		FingerprintHash: "",
		WebGLRenderer:   "Google SwiftShader",
		TimeToCheckoutMs: 1500,
		MouseMovementEntropy: 0,
		KeystrokeCount:  5,
		TypingRhythmCV:  0.01,
	}
	order := []string{
		"ip_country_mismatch", "no_fingerprint",
		"headless_renderer", "rapid_checkout",
		"no_mouse_entropy", "bot_typing_rhythm",
	}
	vec := featuresToFloat32(f, order, nil)
	for i, name := range order {
		if vec[i] != 1 {
			t.Errorf("%s=%v want 1", name, vec[i])
		}
	}
}

func TestFeaturesToFloat32_UnknownFeatureZero(t *testing.T) {
	f := Features{Amount: 100}
	order := []string{"amount", "totally_made_up_feature"}
	vec := featuresToFloat32(f, order, nil)
	if vec[0] != 100 {
		t.Errorf("amount=%v", vec[0])
	}
	if vec[1] != 0 {
		t.Errorf("unknown should be 0, got %v", vec[1])
	}
}

func TestFeaturesToFloat32_ExtraPassthrough(t *testing.T) {
	f := Features{
		Extra: map[string]string{"card_bin": "424242", "missing_key": ""},
	}
	order := []string{"extra.card_bin", "extra.missing_key", "extra.never_set"}
	vec := featuresToFloat32(f, order, nil)
	if vec[0] != 1 {
		t.Errorf("extra.card_bin = %v want 1", vec[0])
	}
	if vec[1] != 0 {
		t.Errorf("extra.missing_key empty value = %v want 0", vec[1])
	}
	if vec[2] != 0 {
		t.Errorf("extra.never_set absent = %v want 0", vec[2])
	}
}

func TestSanitizeFloat32_NaNInf(t *testing.T) {
	if got := sanitizeFloat32(float32(math.NaN())); got != 0 {
		t.Errorf("NaN -> %v want 0", got)
	}
	if got := sanitizeFloat32(float32(math.Inf(1))); got != 0 {
		t.Errorf("+Inf -> %v want 0", got)
	}
	if got := sanitizeFloat32(float32(math.Inf(-1))); got != 0 {
		t.Errorf("-Inf -> %v want 0", got)
	}
	if got := sanitizeFloat32(1.5); got != 1.5 {
		t.Errorf("1.5 -> %v want 1.5", got)
	}
}

func TestFeaturesToFloat32_PrebuiltIndex(t *testing.T) {
	// idx 预计算路径
	order := []string{"amount", "ip_proxy"}
	idx := map[string]int{"amount": 0, "ip_proxy": 1}
	f := Features{Amount: 7, IPProxy: true}
	vec := featuresToFloat32(f, order, idx)
	if vec[0] != 7 || vec[1] != 1 {
		t.Errorf("idx path: vec=%v", vec)
	}
}

func TestFeaturesToFloat32_HighRiskCountry(t *testing.T) {
	cases := []struct {
		country string
		want    float32
	}{
		{"NG", 1}, {"RU", 1}, {"US", 0}, {"", 0},
	}
	for _, tc := range cases {
		f := Features{Country: tc.country}
		vec := featuresToFloat32(f, []string{"high_risk_country"}, nil)
		if vec[0] != tc.want {
			t.Errorf("country=%q got=%v want=%v", tc.country, vec[0], tc.want)
		}
	}
}
