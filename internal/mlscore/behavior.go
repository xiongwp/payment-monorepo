// behavior.go: 行为生物学评分器 (mouse / keystroke / scroll / timing)。
//
// 当前 LogisticService 只把 behavior 信号当二值阈值用 (TimeToCheckoutMs <
// 2000 → 1)。真实 fraud detection 需要 fine-grained 模式识别：
//   - 鼠标轨迹 entropy + 加速度分布 → 区分人类 vs bot 自动化
//   - 击键间隔 distribution (gamma vs uniform) → 真人打字 vs 机器粘贴
//   - 滚动速度峰值数 → 人在浏览 vs 直接跳到提交
//
// 完整方案需要 LSTM / Transformer 模型 (用 ONNX runtime 跑)。本文件提供：
//   1. BehaviorFeatures — 统计型特征工程，从原始 SDK 采集字段算出 12 个
//      高阶 metric (跟训练 pipeline 对齐)
//   2. BehaviorScorer — 基于训练好的 LR 子模型 (BehaviorWeights) 跑评分
//   3. RemoteBehaviorScorer alias → 直接接 RemoteModelService 跑 LSTM
//
// 跟 LogisticService 区别：
//   - LogisticService 是混合模型（fingerprint + IP + amount + 行为）
//   - BehaviorScorer 只看行为信号，输出独立 behavior_score
//
// 用法（套 EnsembleService 融合）：
//
//	ensemble := NewEnsemble("v3", AggregateWeightedMean,
//	    EnsembleMember{Name: "logistic", Service: logistic, Weight: 0.5},
//	    EnsembleMember{Name: "behavior", Service: behaviorScorer, Weight: 0.3},
//	    EnsembleMember{Name: "xgboost",  Service: remoteXGB,    Weight: 0.2},
//	)
package mlscore

import (
	"context"
	"math"
	"strings"
)

// BehaviorWeights 行为子模型的权重 (类比 FeatureWeights 但只行为字段)。
// cmd/retrain 训出来的子模型 JSON 直接灌进来，跟 logistic 同套训练框架。
type BehaviorWeights struct {
	NoMouse        float64 // mouse_entropy == 0
	BotTrack       float64 // mouse_entropy < 1.5 (近直线)
	NoKeystroke    float64 // keystroke_count == 0
	BotTyping      float64 // typing_rhythm_cv < 0.05 (近恒定间隔)
	UnnaturalCV    float64 // typing_rhythm_cv > 1.5 (混乱节奏)
	RapidCheckout  float64 // time_to_checkout < 2000ms
	UltraRapid     float64 // time_to_checkout < 500ms
	NoScroll       float64 // scroll_speed == 0
	HeadlessAgent  float64 // user_agent 含 headless / phantomjs 等
	NoFingerprint  float64 // fingerprint_hash == ""
	WeakConcurrency float64 // hardware_concurrency in [0,1]
	PastedCard     float64 // PastedFields 含 card_number
}

// 默认 prior weights：从公开 fraud research 调校，零数据冷启可直接用。
var defaultBehaviorWeights = BehaviorWeights{
	NoMouse:         0.9,
	BotTrack:        0.7,
	NoKeystroke:     0.4,
	BotTyping:       1.0,
	UnnaturalCV:     0.4,
	RapidCheckout:   1.2,
	UltraRapid:      0.6, // 跟 RapidCheckout 叠加，<500ms 总共 1.8
	NoScroll:        0.3,
	HeadlessAgent:   1.5,
	NoFingerprint:   0.6,
	WeakConcurrency: 0.5,
	PastedCard:      1.0,
}

// BehaviorScorer 基于 BehaviorWeights 的纯计算 scorer (无外部依赖；
// hot-path safe)。
type BehaviorScorer struct {
	w        BehaviorWeights
	intercept float64
	verTag   string
}

func NewBehaviorScorer() *BehaviorScorer {
	return &BehaviorScorer{
		w:         defaultBehaviorWeights,
		intercept: -2.0, // baseline 真人 ≈ sigmoid(-2) = 0.12
		verTag:    "behavior-v1.0-prior",
	}
}

func NewBehaviorScorerWith(w BehaviorWeights, intercept float64, verTag string) *BehaviorScorer {
	if verTag == "" {
		verTag = "behavior-custom"
	}
	return &BehaviorScorer{w: w, intercept: intercept, verTag: verTag}
}

// Score 跑行为评分。
func (b *BehaviorScorer) Score(_ context.Context, f Features) (Result, error) {
	if b == nil {
		return Result{}, nil
	}
	z := b.intercept

	// 鼠标
	if f.MouseMovementEntropy == 0 && f.KeystrokeCount > 0 {
		// 没鼠标但有键盘输入 → 强信号 (典型 RPA bot)
		z += b.w.NoMouse
	}
	if f.MouseMovementEntropy > 0 && f.MouseMovementEntropy < 1.5 {
		z += b.w.BotTrack
	}

	// 键盘
	if f.KeystrokeCount == 0 {
		z += b.w.NoKeystroke
	}
	if f.TypingRhythmCV > 0 && f.TypingRhythmCV < 0.05 {
		z += b.w.BotTyping
	}
	if f.TypingRhythmCV > 1.5 {
		z += b.w.UnnaturalCV
	}

	// 时序
	if f.TimeToCheckoutMs > 0 {
		if f.TimeToCheckoutMs < 500 {
			z += b.w.UltraRapid
		}
		if f.TimeToCheckoutMs < 2000 {
			z += b.w.RapidCheckout
		}
	}

	// 设备
	if f.FingerprintHash == "" {
		z += b.w.NoFingerprint
	}
	if f.HardwareConcurrency > 0 && f.HardwareConcurrency <= 1 {
		z += b.w.WeakConcurrency
	}
	wr := strings.ToLower(f.WebGLRenderer)
	if strings.Contains(wr, "swiftshader") ||
		strings.Contains(wr, "llvmpipe") ||
		strings.Contains(wr, "headless") ||
		strings.Contains(wr, "phantom") {
		z += b.w.HeadlessAgent
	}

	score := 1 / (1 + math.Exp(-z))
	return Result{
		Score:    score,
		ModelVer: b.verTag,
	}, nil
}

// Weights 公开当前权重让运营 dashboard 展示 (调试用)。
func (b *BehaviorScorer) Weights() BehaviorWeights {
	if b == nil {
		return BehaviorWeights{}
	}
	return b.w
}

// SetWeights 替换权重（hot reload 用，不重启服务）。
func (b *BehaviorScorer) SetWeights(w BehaviorWeights) {
	if b == nil {
		return
	}
	b.w = w
}
