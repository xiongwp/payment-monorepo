// logistic.go: 内置 logistic regression 推理。
//
// 取代 NoopService 当作 risk-manage 的默认 ML 模型 — 客户问"你们 ML 怎么实现的"
// 不再是空答。商业部署可以保留它做 fallback（远程 ML gRPC 不可达时）。
//
// 模型形态：
//
//	score = sigmoid(b + Σ w_i × x_i)
//	x_i = 标准化后的特征值（z-score 或 0/1）
//	返回 Score ∈ (0, 1)
//
// 系数来源：本实现是手工"先验校准"的版本，权重基于公开 fraud research +
// Stripe Radar 早期文档 + 经典 IEEE 论文 (Bahnsen 2016 Chap 3 / Stripe Engineering Blog)。
// 不能跟训练好的 XGBoost 比，但好过 NoopService — 客户在生产数据上跑 1
// 个月即可重训替换。
//
// 重训流程：
//   1. 离线导出 risk_decision_audit + risk_outcome 全 join → CSV
//   2. sklearn LogisticRegression(class_weight='balanced').fit(X, y)
//   3. 拿训练好的 (intercept_, coef_) 替换下面的 defaultWeights
//   4. modelVer 升级（"v1.0" → "v1.1"）让 audit 能区分模型版本

package mlscore

import (
	"context"
	"math"
	"strings"
)

// LogisticConfig 模型配置（系数 + 截距）。零值字段会用 default。
//
// PlattA / PlattB 是 Platt scaling 的两个参数，给训练完之后做概率校准
// 用：calibrated_p = sigmoid(PlattA * raw_z + PlattB)。当 class_weight
// 训练时模型输出的"概率"不再是真实概率而是个 ranking score，必须 Platt
// 校准回真实频率才能用 0.5 之类的固定阈值切。
//
// 缺省 PlattA=0（视作未校准）→ Score 用 sigmoid(z) 不变；非零时才走
// 校准路径。json marshal 缺省 omitempty 让旧 LogisticConfig 文件也能 load。
type LogisticConfig struct {
	ModelVer  string         `json:"ModelVer,omitempty"`
	Intercept float64        `json:"Intercept"`
	Weights   FeatureWeights `json:"Weights"`
	PlattA    float64        `json:"PlattA,omitempty"`
	PlattB    float64        `json:"PlattB,omitempty"`
}

// FeatureWeights 每个 (binarized) 特征的权重。
//
// 命名约定 *_w 后缀；所有特征都先二值化或 z-score 化再乘 weight。
// 二值化：HighAmount 阈值 100k；NoFingerprint 缺 fingerprint_hash。
type FeatureWeights struct {
	HighAmount        float64 // amount > 100k → x=1
	IPProxy           float64 // 0/1
	IPVPN             float64
	IPDataCenter      float64
	IPCountryMismatch float64 // ip_country != country → 1
	NoFingerprint     float64 // fingerprint_hash 缺失 → 1
	HeadlessRenderer  float64 // WebGL 含 SwiftShader/llvmpipe → 1
	LowConcurrency    float64 // hardware_concurrency in [0,1] → 1
	RapidCheckout     float64 // time_to_checkout_ms < 2000 → 1
	NoMouseEntropy    float64 // mouse_entropy == 0 → 1
	BotTypingRhythm   float64 // typing_rhythm_cv < 0.05 → 1（接近恒定）
	NoKeystrokes      float64 // keystroke_count == 0 → 1
	HighRiskCountry   float64 // country in {top fraud-rate iso} → 1
}

// 默认权重：先验校准。逻辑：
//   - IP proxy / VPN / data-center 都是中等偏强信号，0.6-0.8
//   - IP-country mismatch + no fingerprint 中等
//   - 设备伪造特征（headless, low concurrency）较强
//   - 行为信号（rapid_checkout / no_mouse_entropy / bot_typing）强
//   - 大金额 + 高风险国 中等
//   - 截距 -2.5 让无信号 baseline ≈ sigmoid(-2.5) = 0.075（"低风险背景"）
var defaultWeights = LogisticConfig{
	ModelVer:  "logistic-v1.0-prior",
	Intercept: -2.5,
	Weights: FeatureWeights{
		HighAmount:        0.5,
		IPProxy:           0.7,
		IPVPN:             0.8,
		IPDataCenter:      0.6,
		IPCountryMismatch: 0.4,
		NoFingerprint:     0.6,
		HeadlessRenderer:  1.0,
		LowConcurrency:    0.5,
		RapidCheckout:     1.2,
		NoMouseEntropy:    0.9,
		BotTypingRhythm:   1.0,
		NoKeystrokes:      0.4,
		HighRiskCountry:   0.6,
	},
}

// HighRiskCountries 公开 fraud rate 高的国家代码（FATF 高风险 + chargeback rate）。
// 实际部署应根据自身数据每月更新；这里是先验默认。
var highRiskCountries = map[string]bool{
	"NG": true, "PK": true, "VE": true, "RU": true, "BD": true,
	"PH": false, // PH 整体 ok；具体看商户行业
}

// LogisticService 默认 ML scorer。零依赖、纯计算、< 1µs/call。
type LogisticService struct {
	cfg LogisticConfig
}

// NewLogisticService 默认配置（先验权重）。生产把训练好的系数 NewLogisticServiceWith。
func NewLogisticService() *LogisticService {
	return &LogisticService{cfg: defaultWeights}
}

// NewLogisticServiceWith 指定 cfg。Intercept / Weights 零值就是 0（合法）；
// 调用方完全负责系数。仅 ModelVer 缺省 fallback（避免审计丢版本）。
func NewLogisticServiceWith(cfg LogisticConfig) *LogisticService {
	if cfg.ModelVer == "" {
		cfg.ModelVer = "logistic-custom"
	}
	return &LogisticService{cfg: cfg}
}

func (s *LogisticService) Score(_ context.Context, f Features) (Result, error) {
	w := s.cfg.Weights
	z := s.cfg.Intercept
	if f.Amount > 100000 { // > $1000 阈值（minor unit, 100k = 1000）
		z += w.HighAmount
	}
	if f.IPProxy {
		z += w.IPProxy
	}
	if f.IPVPN {
		z += w.IPVPN
	}
	if f.IPDataCenter {
		z += w.IPDataCenter
	}
	if f.IPCountry != "" && f.Country != "" &&
		!strings.EqualFold(f.IPCountry, f.Country) {
		z += w.IPCountryMismatch
	}
	if f.FingerprintHash == "" {
		z += w.NoFingerprint
	}
	wr := strings.ToLower(f.WebGLRenderer)
	if wr != "" && (strings.Contains(wr, "swiftshader") ||
		strings.Contains(wr, "llvmpipe") ||
		strings.Contains(wr, "mesa offscreen")) {
		z += w.HeadlessRenderer
	}
	if f.HardwareConcurrency > 0 && f.HardwareConcurrency <= 1 {
		z += w.LowConcurrency
	}
	if f.TimeToCheckoutMs > 0 && f.TimeToCheckoutMs < 2000 {
		z += w.RapidCheckout
	}
	if f.MouseMovementEntropy == 0 && f.KeystrokeCount > 0 {
		z += w.NoMouseEntropy
	}
	if f.TypingRhythmCV > 0 && f.TypingRhythmCV < 0.05 {
		z += w.BotTypingRhythm
	}
	if f.KeystrokeCount == 0 {
		z += w.NoKeystrokes
	}
	if highRiskCountries[strings.ToUpper(f.Country)] {
		z += w.HighRiskCountry
	}

	// Platt scaling：训练阶段拟合的 (A, B) 把 raw decision 映射回真实概率。
	// A=0 视为未校准（兼容老配置），走原 sigmoid(z) 路径。
	var score float64
	if s.cfg.PlattA != 0 {
		score = sigmoid(s.cfg.PlattA*z + s.cfg.PlattB)
	} else {
		score = sigmoid(z)
	}
	return Result{Score: score, ModelVer: s.cfg.ModelVer}, nil
}

func sigmoid(z float64) float64 {
	return 1.0 / (1.0 + math.Exp(-z))
}
