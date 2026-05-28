// explain.go: 决策可解释性 — 把 ML score 拆成 per-feature contribution。
//
// 背景：现 audit 只记"哪些 rule 命中 + ml_score"。投诉申诉时运营答不出
// "为什么 ML 给 0.4 分"。本包提供 SHAP-style 简化版 feature attribution，
// 让 admin /admin/decisions/{id}/explain 能列出 top-K 贡献特征。
//
// 简化版 SHAP（**非**严格 Shapley 值）：
//
//	contribution_i = w_i × (x_i - baseline_mean_i)
//
//   - LogisticExplainer：w_i 来自 LogisticConfig.Weights（手工先验或训练后系数）；
//     baseline_mean_i = 0（特征都是二值 indicator，baseline=不命中）；
//     即 contribution_i = w_i × indicator_i —— logit 空间的真实贡献。
//   - OnnxShapExplainer：w_i + baseline_mean_i 从同名 .shap.json 加载
//     （retrain CLI / Python 训练侧产）；用同公式算 deterministic attribution。
//     真 TreeSHAP / LinearSHAP 需要 Python shap 库；产线接 ML 团队的
//     shap 输出即可，本简化版保证 base_value + Σcontribution ≈ logit(final_score)。
//
// 排序：top_contributors 按 |contribution| 降序（不分正负，"最有解释力"在前）；
// Direction 字段告诉前端是推风险还是减风险。
//
// audit 集成：service.recordAuditWith 后**同步**调一次 Explain（< 100µs），
// 失败 → log warn 不阻塞主路径。落到 DecisionAudit.Explain 块；admin 端点
// /admin/decisions/{id}/explain 优先读这个块，没存的话 fallback 重新算。

package mlscore

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"sort"
	"strings"
)

// ExplainResult 一笔决策的 ML 解释。JSON 兼容（admin endpoint 直接 marshal）。
type ExplainResult struct {
	BaseValue        float64               `json:"base_value"`        // 训练集均值 logit（无信号 baseline）
	FinalScore       float64               `json:"final_score"`       // 当前样本 score（sigmoid 后 0-1）
	TopContributors  []FeatureContribution `json:"top_contributors"`  // 按 |contribution| 排序的 top-K
	AllContributions map[string]float64    `json:"all_contributions"` // 全量字典（key=feature_name）
	ModelVersion     string                `json:"model_version"`
}

// FeatureContribution 单个特征的贡献明细。
type FeatureContribution struct {
	Feature      string  `json:"feature"`
	Value        float64 `json:"value"`        // 原始特征值（二值 0/1 或数值）
	Contribution float64 `json:"contribution"` // SHAP-style 贡献（正 = 推风险，负 = 减风险）
	Direction    string  `json:"direction"`    // "increase_risk" / "decrease_risk" / "neutral"
}

// Explainer 解释一笔特征向量为什么得到这个 score。
//
// LogisticExplainer 忽略 model_path（系数已在构造时灌入）；
// OnnxShapExplainer 在构造时从同名 .shap.json 加载元数据。
type Explainer interface {
	Explain(ctx context.Context, f Features, topK int) (*ExplainResult, error)
}

// ── LogisticExplainer ──────────────────────────────────────────────────

// LogisticExplainer 给 LogisticService 配套的解释器。
// w_i × indicator_i = logit-space contribution；intercept = base_value。
type LogisticExplainer struct {
	cfg LogisticConfig
}

// NewLogisticExplainer 默认权重的解释器（跟 NewLogisticService 同步）。
func NewLogisticExplainer() *LogisticExplainer {
	return &LogisticExplainer{cfg: defaultWeights}
}

// NewLogisticExplainerWith 自定义系数（跟 NewLogisticServiceWith 配对）。
func NewLogisticExplainerWith(cfg LogisticConfig) *LogisticExplainer {
	if cfg.ModelVer == "" {
		cfg.ModelVer = "logistic-custom"
	}
	return &LogisticExplainer{cfg: cfg}
}

// Explain 复用 LogisticService.Score 的指标二值化条件算每条信号的 indicator，
// 然后 contribution = weight × indicator。Score 用 sigmoid(intercept + Σ) 还原。
func (e *LogisticExplainer) Explain(_ context.Context, f Features, topK int) (*ExplainResult, error) {
	w := e.cfg.Weights
	// 复用 Score 的指示器条件 — 跟 logistic.go 保持同步。
	indicators := map[string]float64{
		"high_amount":          ind(f.Amount > 100000),
		"ip_proxy":             ind(f.IPProxy),
		"ip_vpn":               ind(f.IPVPN),
		"ip_datacenter":        ind(f.IPDataCenter),
		"ip_country_mismatch":  ind(f.IPCountry != "" && f.Country != "" && !strings.EqualFold(f.IPCountry, f.Country)),
		"no_fingerprint":       ind(f.FingerprintHash == ""),
		"headless_renderer":    ind(isHeadlessRenderer(f.WebGLRenderer)),
		"low_concurrency":      ind(f.HardwareConcurrency > 0 && f.HardwareConcurrency <= 1),
		"rapid_checkout":       ind(f.TimeToCheckoutMs > 0 && f.TimeToCheckoutMs < 2000),
		"no_mouse_entropy":     ind(f.MouseMovementEntropy == 0 && f.KeystrokeCount > 0),
		"bot_typing_rhythm":    ind(f.TypingRhythmCV > 0 && f.TypingRhythmCV < 0.05),
		"no_keystrokes":        ind(f.KeystrokeCount == 0),
		"high_risk_country":    ind(highRiskCountries[strings.ToUpper(f.Country)]),
		"bot_behavior_combo":   ind(f.MousePauseCount == 0 && f.KeystrokeCount > 0),
		"bot_straight_line":    ind(f.MouseStraightnessRatio > 0.95 && f.MouseAvgSpeedPxPerMs > 0),
		"low_mouse_variance":   ind(f.MouseSpeedVariance > 0 && f.MouseSpeedVariance < 0.001 && f.MouseAvgSpeedPxPerMs > 0),
		"bot_keystroke_rhythm": ind(f.KeystrokeDwellCV > 0 && f.KeystrokeDwellCV < 0.05 && f.KeystrokeFlightCV > 0 && f.KeystrokeFlightCV < 0.05),
	}
	weights := map[string]float64{
		"high_amount":          w.HighAmount,
		"ip_proxy":             w.IPProxy,
		"ip_vpn":               w.IPVPN,
		"ip_datacenter":        w.IPDataCenter,
		"ip_country_mismatch":  w.IPCountryMismatch,
		"no_fingerprint":       w.NoFingerprint,
		"headless_renderer":    w.HeadlessRenderer,
		"low_concurrency":      w.LowConcurrency,
		"rapid_checkout":       w.RapidCheckout,
		"no_mouse_entropy":     w.NoMouseEntropy,
		"bot_typing_rhythm":    w.BotTypingRhythm,
		"no_keystrokes":        w.NoKeystrokes,
		"high_risk_country":    w.HighRiskCountry,
		"bot_behavior_combo":   w.BotBehaviorCombo,
		"bot_straight_line":    w.BotStraightLine,
		"low_mouse_variance":   w.LowMouseVariance,
		"bot_keystroke_rhythm": w.BotKeystrokeRhythm,
	}
	contribs := make(map[string]float64, len(indicators))
	z := e.cfg.Intercept
	for k, v := range indicators {
		c := weights[k] * v
		contribs[k] = c
		z += c
	}
	var score float64
	if e.cfg.PlattA != 0 {
		score = sigmoid(e.cfg.PlattA*z + e.cfg.PlattB)
	} else {
		score = sigmoid(z)
	}
	return assembleResult(e.cfg.Intercept, score, contribs, indicators, e.cfg.ModelVer, topK), nil
}

// ── OnnxShapExplainer ──────────────────────────────────────────────────

// shapMetadata .shap.json 的 schema。Python 训练侧 explain_export.py 产。
type shapMetadata struct {
	BaseValue               float64            `json:"base_value"`
	ModelVersion            string             `json:"model_version"`
	FeatureBaselineMean     map[string]float64 `json:"feature_baseline_mean"`
	FeatureGlobalImportance map[string]float64 `json:"feature_global_importance"`
}

// OnnxShapExplainer 加载训练侧产的 .shap.json，对 GBDT/XGBoost 模型做简化 SHAP。
// 算法：contribution_i = importance_i × (value_i - baseline_mean_i)
// — 不严格 Shapley，但可解释方向 + 量级，足够答疑场景。
type OnnxShapExplainer struct {
	meta shapMetadata
}

// NewOnnxShapExplainer 从 modelPath 同名 .shap.json 加载元数据。
// 文件不存在 / 解析失败 → 返 error；caller 应 fallback 到 LogisticExplainer。
func NewOnnxShapExplainer(modelPath string) (*OnnxShapExplainer, error) {
	if modelPath == "" {
		return nil, errors.New("explain: model path empty")
	}
	jsonPath := strings.TrimSuffix(modelPath, ".onnx") + ".shap.json"
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, err
	}
	var meta shapMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	if len(meta.FeatureGlobalImportance) == 0 {
		return nil, errors.New("explain: shap metadata missing feature_global_importance")
	}
	if meta.FeatureBaselineMean == nil {
		meta.FeatureBaselineMean = map[string]float64{}
	}
	return &OnnxShapExplainer{meta: meta}, nil
}

func (e *OnnxShapExplainer) Explain(_ context.Context, f Features, topK int) (*ExplainResult, error) {
	values := featureValueMap(f)
	contribs := make(map[string]float64, len(e.meta.FeatureGlobalImportance))
	z := e.meta.BaseValue
	for name, imp := range e.meta.FeatureGlobalImportance {
		v := values[name]
		mean := e.meta.FeatureBaselineMean[name]
		c := imp * (v - mean)
		contribs[name] = c
		z += c
	}
	score := sigmoid(z)
	return assembleResult(e.meta.BaseValue, score, contribs, values, e.meta.ModelVersion, topK), nil
}

// ── 公共辅助 ───────────────────────────────────────────────────────────

func assembleResult(baseValue, finalScore float64, contribs, values map[string]float64, modelVer string, topK int) *ExplainResult {
	type kv struct {
		k string
		v float64
	}
	flat := make([]kv, 0, len(contribs))
	for k, v := range contribs {
		flat = append(flat, kv{k, v})
	}
	sort.Slice(flat, func(i, j int) bool {
		ai, aj := math.Abs(flat[i].v), math.Abs(flat[j].v)
		if ai == aj {
			return flat[i].k < flat[j].k // 稳定输出：相同贡献按名字字典序
		}
		return ai > aj
	})
	if topK <= 0 || topK > len(flat) {
		topK = len(flat)
	}
	top := make([]FeatureContribution, 0, topK)
	for i := 0; i < topK; i++ {
		c := flat[i].v
		if c == 0 {
			break // 后面都是 0 信号，截断（不污染 top 列表）
		}
		dir := "neutral"
		switch {
		case c > 0:
			dir = "increase_risk"
		case c < 0:
			dir = "decrease_risk"
		}
		top = append(top, FeatureContribution{
			Feature:      flat[i].k,
			Value:        values[flat[i].k],
			Contribution: c,
			Direction:    dir,
		})
	}
	return &ExplainResult{
		BaseValue:        baseValue,
		FinalScore:       finalScore,
		TopContributors:  top,
		AllContributions: contribs,
		ModelVersion:     modelVer,
	}
}

func ind(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func isHeadlessRenderer(r string) bool {
	wr := strings.ToLower(r)
	return wr != "" && (strings.Contains(wr, "swiftshader") ||
		strings.Contains(wr, "llvmpipe") ||
		strings.Contains(wr, "mesa offscreen"))
}

// featureValueMap 把 Features 拍平成 map[name]float64，给 OnnxShapExplainer 用。
// key 命名跟 driftFeaturesFrom / Python 训练侧保持一致；bool → 0/1，string 不入。
func featureValueMap(f Features) map[string]float64 {
	return map[string]float64{
		"amount":                      float64(f.Amount),
		"ip_proxy":                    ind(f.IPProxy),
		"ip_vpn":                      ind(f.IPVPN),
		"ip_datacenter":               ind(f.IPDataCenter),
		"hardware_concurrency":        float64(f.HardwareConcurrency),
		"time_to_checkout_ms":         float64(f.TimeToCheckoutMs),
		"mouse_movement_entropy":      f.MouseMovementEntropy,
		"click_interval_ms":           float64(f.ClickIntervalMs),
		"typing_rhythm_cv":            f.TypingRhythmCV,
		"keystroke_count":             float64(f.KeystrokeCount),
		"mouse_avg_speed_px_per_ms":   f.MouseAvgSpeedPxPerMs,
		"mouse_speed_variance":        f.MouseSpeedVariance,
		"mouse_acceleration_kurtosis": f.MouseAccelerationKurtosis,
		"mouse_straightness_ratio":    f.MouseStraightnessRatio,
		"mouse_pause_count":           float64(f.MousePauseCount),
		"keystroke_dwell_mean":        f.KeystrokeDwellMean,
		"keystroke_dwell_cv":          f.KeystrokeDwellCV,
		"keystroke_flight_mean":       f.KeystrokeFlightMean,
		"keystroke_flight_cv":         f.KeystrokeFlightCV,
	}
}
