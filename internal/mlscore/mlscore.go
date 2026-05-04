// Package mlscore 提供机器学习推理客户端，给风控引擎注入 ML 风险分。
//
// 形状对齐架构图的"Risk Scoring Engine (Rules + ML)"中的 ML 部分：
//
//	端 SDK + service 收集特征 → mlscore.Service.Score(features) → 0.0-1.0 分
//	→ 注入 TxnContext.MLScore → ml_threshold 规则按阈值判定
//
// 接口可适配多种实现：本地 ONNX runtime / 远程 TF Serving / Sift / 自训
// XGBoost gRPC 服务等。当前 stub 是 NoopService（永远返回 0）+ 简单
// FixedService（dev / 单测）。生产替换为 RemoteService（gRPC client）。
package mlscore

import (
	"context"
	"sync/atomic"
)

// Features 推理输入。复制自 TxnContext 的关键子集（不暴露内部 struct，
// 让推理服务能独立演进；新增特征通过 Extra 透传）。
type Features struct {
	MerchantID    string
	CustomerID    string
	Amount        int64
	Currency      string
	PaymentMethod string
	Country       string
	IPCountry     string
	IPProxy       bool
	IPVPN         bool
	IPDataCenter  bool

	FingerprintHash      string
	CanvasFingerprint    string
	WebGLRenderer        string
	HardwareConcurrency  int

	TimeToCheckoutMs     int64
	MouseMovementEntropy float64
	ClickIntervalMs      int
	TypingRhythmCV       float64
	KeystrokeCount       int

	// Extra 业务侧自定义信号；推理服务按 key 取（如 "card_bin"、"card_country"）。
	Extra map[string]string
}

// Result 推理结果。Score 范围 [0, 1]，越高越可疑。
type Result struct {
	Score    float64
	ModelVer string // 训练版本号；用于审计可重现
}

// Service 推理服务接口。Score 在 ms 级返回；超时由调用方控制 ctx。
type Service interface {
	Score(ctx context.Context, f Features) (Result, error)
}

// NoopService 永远返回 score=0。生产 ML 服务未配置时 fail-open。
type NoopService struct{}

func (NoopService) Score(_ context.Context, _ Features) (Result, error) {
	return Result{Score: 0, ModelVer: "noop"}, nil
}

// FixedService 始终返回固定分数。dev / 单测 / 演示用。
type FixedService struct {
	score    float64
	modelVer string
	calls    atomic.Int64
}

func NewFixedService(score float64, modelVer string) *FixedService {
	return &FixedService{score: score, modelVer: modelVer}
}

func (f *FixedService) Score(_ context.Context, _ Features) (Result, error) {
	f.calls.Add(1)
	return Result{Score: f.score, ModelVer: f.modelVer}, nil
}

// Calls 测试 / metrics 用：累计调用数。
func (f *FixedService) Calls() int64 { return f.calls.Load() }
