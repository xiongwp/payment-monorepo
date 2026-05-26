//go:build !onnx

// behavior_lstm_stub.go：默认 build 下 BehaviorLSTMService 的 stub 实现。
//
// 跟 onnx_stub.go 设计一致：让 main.go / service.RiskService 无条件引用
// BehaviorLSTMService 类型 + Score 调用编译过，但默认 build 不绑 onnxruntime。
//
// stub 行为：
//   - NewBehaviorLSTMService 返 ErrBehaviorLSTMNotEnabled
//   - Score 永远返中性 0.5（跟 onnx build 下 fallback 一致）
//   - Reload 返 ErrBehaviorLSTMNotEnabled
//
// 启用：go build -tags onnx ./cmd/server
package mlscore

import (
	"context"
	"errors"
	"sync"
)

// BehaviorLSTMService stub。字段 minimum；Score 永远走 fallback。
type BehaviorLSTMService struct {
	mu        sync.Mutex
	modelPath string
}

// 复制自 onnx 版本的常量；测试 / 外部代码引用不依赖 build tag。
const (
	MouseSeqLen      = 200
	MouseFeatDim     = 3
	KeystrokeSeqLen  = 50
	KeystrokeFeatDim = 3
)

// MouseEvent 单个鼠标事件。stub 跟 onnx 版本 struct 字段一致。
type MouseEvent struct {
	X int   `json:"x"`
	Y int   `json:"y"`
	T int64 `json:"t"`
}

// KeystrokeEvent 单个按键事件。
type KeystrokeEvent struct {
	KeyCode int     `json:"keycode"`
	Dwell   float64 `json:"dwell"`
	Flight  float64 `json:"flight"`
}

// BehaviorInput LSTM 推理输入。
type BehaviorInput struct {
	Mouse        []MouseEvent
	Keystrokes   []KeystrokeEvent
	ScreenW      int
	ScreenH      int
	SessionDurMs int64
}

// BehaviorResult 推理结果。
type BehaviorResult struct {
	AnomalyScore float64
	ModelVer     string
}

var (
	// ErrBehaviorLSTMNotEnabled stub build 标记
	ErrBehaviorLSTMNotEnabled = errors.New("behavior_lstm not enabled: rebuild with -tags onnx")
	// ErrBehaviorModelPathEmpty 配置空（stub 也用，admin 路径区分配置错 vs 没编 onnx）
	ErrBehaviorModelPathEmpty = errors.New("behavior_lstm: model path is empty")
	// ErrBehaviorModelLoad 加载错（stub 用不到，对齐 onnx 版本错误集合）
	ErrBehaviorModelLoad = errors.New("behavior_lstm: model load failed")
)

// neutralScore stub 走的中性 anomaly 分（0.5 = "不确定"）。
// 跟 onnx 版本的 fallback 行为一致：service.Screen 不区分 stub vs runtime
// fallback —— 两者都走 "rule.behavior_lstm_threshold > 0.7 默认不命中" 路径。
const neutralScore = 0.5

// NewBehaviorLSTMService stub 永远返 ErrBehaviorLSTMNotEnabled。
// main.go 收到错应 log warn + 用 nil-svc（Score nil-receiver 仍返 neutralScore）。
func NewBehaviorLSTMService(modelPath string) (*BehaviorLSTMService, error) {
	return nil, ErrBehaviorLSTMNotEnabled
}

// Score stub：无论 svc nil 还是非 nil，永远返 neutralScore。
// 调用方典型路径：service.Screen 拿 BehaviorInput → svc.Score(ctx, in) → 不管
// build tag 都拿到一个合理分数；不需要 if svc != nil 分支。
func (s *BehaviorLSTMService) Score(_ context.Context, _ BehaviorInput) (BehaviorResult, error) {
	return BehaviorResult{AnomalyScore: neutralScore, ModelVer: "stub"}, nil
}

// Reload stub
func (s *BehaviorLSTMService) Reload(_ string) error {
	return ErrBehaviorLSTMNotEnabled
}

// ModelVersion stub
func (s *BehaviorLSTMService) ModelVersion() string { return "stub" }

// ModelPath stub
func (s *BehaviorLSTMService) ModelPath() string { return "" }

// Close stub
func (s *BehaviorLSTMService) Close() error { return nil }
