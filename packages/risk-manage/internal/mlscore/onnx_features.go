// onnx_features.go：Features → []float32 转换（onnx 推理输入构造）。
//
// 独立于 onnx build tag：测试不需要 onnxruntime 库也能跑。
//
// 转换规则：
//   - 按 featOrder 顺序生成定长 []float32
//   - 已知特征名（见 featureExtractor map）→ 取对应字段值
//   - 未知特征名 → 0 + log warn（一次性 sync.Once，避免刷屏）
//   - bool → 0/1；string 非空 → 1（适用于 indicator 特征）
//   - 数值字段 NaN/Inf/nil → 0
//
// 特征名约定（snake_case，跟 ML 团队 Feast feature definition 对齐）：
//
//   amount             → Features.Amount (int64 → float32)
//   ip_proxy           → Features.IPProxy (bool)
//   ip_vpn             → Features.IPVPN (bool)
//   ip_datacenter      → Features.IPDataCenter (bool)
//   ip_country_mismatch→ derive: IPCountry != Country
//   no_fingerprint     → derive: FingerprintHash == ""
//   headless_renderer  → derive: WebGLRenderer ~= swiftshader/llvmpipe
//   hardware_concurrency → int → float32
//   time_to_checkout_ms→ int64 → float32
//   mouse_entropy      → float64 → float32
//   click_interval_ms  → int → float32
//   typing_rhythm_cv   → float64 → float32
//   keystroke_count    → int → float32
//   high_risk_country  → derive
//
// 离线训练 + 在线推理特征一致性是 ML 工程最大坑；推荐用 Feast feature store
// 做单一来源，详见 ONNX_PIPELINE.md "特征一致性"。

package mlscore

import (
	"math"
	"strings"
	"sync"
)

// onnxUnknownFeatLog 一次性 warn unknown features（按 name 去重）
var onnxUnknownFeatLog sync.Map // map[string]struct{}

// featuresToFloat32 按 featOrder 把 Features 转成模型输入向量。
//
// idx 是 featOrder 的反查 map（caller 预算 O(1)）；nil idx fallback 用 featOrder 重建。
//
// 不在已知 extractor 列表里的特征名：返 0 + warn（生产里 ML 团队 schema-driven
// 增量加特征，工程侧来不及加 extractor 时降级跑，比 panic 友好）。
func featuresToFloat32(f Features, featOrder []string, idx map[string]int) []float32 {
	if idx == nil {
		idx = make(map[string]int, len(featOrder))
		for i, n := range featOrder {
			idx[n] = i
		}
	}
	vec := make([]float32, len(featOrder))
	for name, i := range idx {
		v, known := extractFeature(name, f)
		if !known {
			// 仅 warn 一次（同名第二次 LoadOrStore 找到就 skip）
			onnxUnknownFeatLog.LoadOrStore(name, struct{}{})
			vec[i] = 0
			continue
		}
		vec[i] = sanitizeFloat32(v)
	}
	return vec
}

// extractFeature 单字段提取。返 (value, known)；known=false 表示 name 不在
// 已知特征名集合。
//
// 列表跟 ONNX_PIPELINE.md 的 feature dictionary 对齐；新增特征两边同步。
func extractFeature(name string, f Features) (float32, bool) {
	switch name {
	case "amount":
		return float32(f.Amount), true
	case "ip_proxy":
		return boolToFloat32(f.IPProxy), true
	case "ip_vpn":
		return boolToFloat32(f.IPVPN), true
	case "ip_datacenter":
		return boolToFloat32(f.IPDataCenter), true
	case "ip_country_mismatch":
		return boolToFloat32(f.IPCountry != "" && f.Country != "" &&
			!strings.EqualFold(f.IPCountry, f.Country)), true
	case "no_fingerprint":
		return boolToFloat32(f.FingerprintHash == ""), true
	case "headless_renderer":
		wr := strings.ToLower(f.WebGLRenderer)
		return boolToFloat32(wr != "" && (strings.Contains(wr, "swiftshader") ||
			strings.Contains(wr, "llvmpipe") ||
			strings.Contains(wr, "mesa offscreen"))), true
	case "hardware_concurrency":
		return float32(f.HardwareConcurrency), true
	case "low_concurrency":
		return boolToFloat32(f.HardwareConcurrency > 0 && f.HardwareConcurrency <= 1), true
	case "time_to_checkout_ms":
		return float32(f.TimeToCheckoutMs), true
	case "rapid_checkout":
		return boolToFloat32(f.TimeToCheckoutMs > 0 && f.TimeToCheckoutMs < 2000), true
	case "mouse_entropy":
		return float32(f.MouseMovementEntropy), true
	case "no_mouse_entropy":
		return boolToFloat32(f.MouseMovementEntropy == 0 && f.KeystrokeCount > 0), true
	case "click_interval_ms":
		return float32(f.ClickIntervalMs), true
	case "typing_rhythm_cv":
		return float32(f.TypingRhythmCV), true
	case "bot_typing_rhythm":
		return boolToFloat32(f.TypingRhythmCV > 0 && f.TypingRhythmCV < 0.05), true
	case "keystroke_count":
		return float32(f.KeystrokeCount), true
	case "no_keystrokes":
		return boolToFloat32(f.KeystrokeCount == 0), true
	case "high_risk_country":
		return boolToFloat32(highRiskCountries[strings.ToUpper(f.Country)]), true
	}
	// Extra 透传：feature name 形如 "extra.card_bin"
	if strings.HasPrefix(name, "extra.") {
		key := strings.TrimPrefix(name, "extra.")
		if f.Extra != nil {
			if v, ok := f.Extra[key]; ok {
				return boolToFloat32(v != ""), true
			}
		}
		return 0, true // extra.* 缺失视为已知特征 = 0（不 warn）
	}
	return 0, false
}

func boolToFloat32(b bool) float32 {
	if b {
		return 1
	}
	return 0
}

// sanitizeFloat32 NaN/Inf → 0；防 onnxruntime native 层 panic。
func sanitizeFloat32(v float32) float32 {
	if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
		return 0
	}
	return v
}
