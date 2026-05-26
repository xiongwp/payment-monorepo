//go:build onnx

// behavior_lstm.go：行为生物 LSTM 推理服务（onnx build tag 启用）。
//
// 跟 onnx_service.go 的区别：
//   - onnx_service.go 跑 GBDT / LR：tabular features → 单条 fraud probability
//   - 本服务跑 LSTM：mouse / keystroke 时序 → behavior_anomaly_score
//
// 模型不在仓库，**Python 团队负责训练**。本文件只搭 Go 侧接入骨架：
//   - 模型签名约定（input/output tensor 名 + shape）
//   - 序列预处理（pad / truncate）
//   - 原子热切换（atomic.Pointer，跟 OnnxService 一样）
//   - 模型不存在 / 加载失败时 → fallback 返中性 0.5，不阻断主路径
//
// Tensor shape 契约（与 BEHAVIOR_LSTM.md 对齐）：
//   - mouse:     (B=1, T=200, F=3)  F=[x_norm, y_norm, t_norm]
//   - keystroke: (B=1, T=50,  F=3)  F=[keycode_norm, dwell_norm, flight_norm]
//   - output:    (B=1, 1)            value ∈ [0,1] anomaly score
//
// Python 训练流程见 BEHAVIOR_LSTM.md。
package mlscore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ort "github.com/yalue/onnxruntime_go"
)

// 序列长度上限（不足补 0，超长 truncate 保留尾部）。
// 跟 Python 训练侧 padded_sequence(max_len=...) 必须严格一致；改值要双方同步。
const (
	MouseSeqLen     = 200
	MouseFeatDim    = 3
	KeystrokeSeqLen = 50
	KeystrokeFeatDim = 3
)

// MouseEvent 单个鼠标事件。
// X, Y：像素坐标；T：相对 session start 的毫秒时戳。
// 预处理时归一化到 [0,1]：x/screenW, y/screenH, t/sessionDurMs。
type MouseEvent struct {
	X int     `json:"x"`
	Y int     `json:"y"`
	T int64   `json:"t"` // ms since session start
}

// KeystrokeEvent 单个按键事件。
// Dwell：按下持续时间 (ms)；Flight：与前一次按键间隔 (ms)；KeyCode：键码（归一化用）。
type KeystrokeEvent struct {
	KeyCode int     `json:"keycode"`
	Dwell   float64 `json:"dwell"`  // ms
	Flight  float64 `json:"flight"` // ms
}

// BehaviorInput LSTM 推理输入：两路时序。
type BehaviorInput struct {
	Mouse      []MouseEvent
	Keystrokes []KeystrokeEvent
	// 归一化用的 baseline；不传走默认（screen 1920x1080，session 60s）。
	ScreenW       int
	ScreenH       int
	SessionDurMs  int64
}

// BehaviorResult 推理输出。
type BehaviorResult struct {
	AnomalyScore float64 // ∈ [0, 1]，越高越可疑（bot / 自动化）
	ModelVer     string
}

// BehaviorLSTMService 包装一个 ONNX LSTM session，专门跑行为序列模型。
//
// 跟 OnnxService 完全独立：
//   - 不同模型架构（LSTM vs GBDT），不同 input shape
//   - 不同 admin 端点（避免 reload 把 GBDT 模型当 LSTM 加载）
//   - 不同 fallback 策略（LSTM 不可用返 0.5 中性值，不像 GBDT 返 0）
type BehaviorLSTMService struct {
	current atomic.Pointer[behaviorSession]

	mu        sync.Mutex
	modelPath string
	modelVer  string

	inflight sync.WaitGroup
}

type behaviorSession struct {
	sess              *ort.AdvancedSession
	mouseInputName    string
	keystrokeInputName string
	outName           string
}

var (
	// ErrBehaviorLSTMNotEnabled stub build 标记
	ErrBehaviorLSTMNotEnabled = errors.New("behavior_lstm not enabled: rebuild with -tags onnx")
	// ErrBehaviorModelPathEmpty 配置空
	ErrBehaviorModelPathEmpty = errors.New("behavior_lstm: model path is empty")
	// ErrBehaviorModelLoad 加载错（文件不存在 / 不是合法 onnx / shape 不匹配）
	ErrBehaviorModelLoad = errors.New("behavior_lstm: model load failed")
)

// neutralScore fallback 时返的中性值。
// 为什么是 0.5 不是 0：
//   - GBDT fraud_prob=0 表示"无可疑"，rule 直接放行（fail-open 合理）
//   - LSTM behavior_anomaly=0 表示"非常像真人"，假阳极少（fail-close 风险）
//   - 0.5 = "不确定"，下游 rule（behavior_lstm_threshold > 0.7）默认不命中
//   - 同时让 dashboard 看 distribution 时能区分 "fallback" vs "真低分"
const neutralScore = 0.5

// NewBehaviorLSTMService 加载 modelPath 处的 ONNX LSTM 模型。
// 失败返 wrap(ErrBehaviorModelLoad)；caller 应当 fallback 到 nil-service
// （Score 路径自然返中性分）。
func NewBehaviorLSTMService(modelPath string) (*BehaviorLSTMService, error) {
	if modelPath == "" {
		return nil, ErrBehaviorModelPathEmpty
	}
	if err := initOnnxRuntime(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBehaviorModelLoad, err)
	}
	sess, err := loadBehaviorSession(modelPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBehaviorModelLoad, err)
	}
	svc := &BehaviorLSTMService{
		modelPath: modelPath,
		modelVer:  deriveModelVer(modelPath),
	}
	svc.current.Store(sess)
	return svc, nil
}

func loadBehaviorSession(modelPath string) (*behaviorSession, error) {
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("model file: %w", err)
	}
	inputs, outputs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("onnx introspect: %w", err)
	}
	if len(inputs) < 2 || len(outputs) < 1 {
		return nil, errors.New("behavior_lstm: expected 2 inputs (mouse, keystroke) and 1 output")
	}
	// 约定 input[0]=mouse  input[1]=keystroke；若 Python 端 export 顺序反了
	// 也通过 name 字段 "mouse_seq" / "keystroke_seq" 命名匹配。
	mouseName := inputs[0].Name
	keyName := inputs[1].Name
	for _, in := range inputs {
		nl := strings.ToLower(in.Name)
		if strings.Contains(nl, "mouse") {
			mouseName = in.Name
		}
		if strings.Contains(nl, "key") {
			keyName = in.Name
		}
	}
	outName := outputs[0].Name

	mouseShape := ort.NewShape(1, MouseSeqLen, MouseFeatDim)
	mouseT, err := ort.NewEmptyTensor[float32](mouseShape)
	if err != nil {
		return nil, fmt.Errorf("mouse tensor: %w", err)
	}
	keyShape := ort.NewShape(1, KeystrokeSeqLen, KeystrokeFeatDim)
	keyT, err := ort.NewEmptyTensor[float32](keyShape)
	if err != nil {
		_ = mouseT.Destroy()
		return nil, fmt.Errorf("keystroke tensor: %w", err)
	}
	outShape := ort.NewShape(1, 1)
	outT, err := ort.NewEmptyTensor[float32](outShape)
	if err != nil {
		_ = mouseT.Destroy()
		_ = keyT.Destroy()
		return nil, fmt.Errorf("output tensor: %w", err)
	}

	sess, err := ort.NewAdvancedSession(
		modelPath,
		[]string{mouseName, keyName},
		[]string{outName},
		[]ort.Value{mouseT, keyT},
		[]ort.Value{outT},
		nil,
	)
	if err != nil {
		_ = mouseT.Destroy()
		_ = keyT.Destroy()
		_ = outT.Destroy()
		return nil, fmt.Errorf("new session: %w", err)
	}
	return &behaviorSession{
		sess:               sess,
		mouseInputName:     mouseName,
		keystrokeInputName: keyName,
		outName:            outName,
	}, nil
}

// Score 推理。流程：
//  1. 原子读 session（无 session → 返中性 0.5，不算 error）
//  2. 预处理 input（pad / truncate + 归一化）
//  3. 写 input tensor → Run → 读 output[0]
//  4. clamp [0,1] + 返 BehaviorResult
//
// nil-safe: svc == nil 直接返 neutralScore（main.go 没初始化时 caller 不用判 nil）。
func (s *BehaviorLSTMService) Score(ctx context.Context, in BehaviorInput) (BehaviorResult, error) {
	if s == nil {
		return BehaviorResult{AnomalyScore: neutralScore, ModelVer: "no-service"}, nil
	}
	sess := s.current.Load()
	if sess == nil {
		return BehaviorResult{AnomalyScore: neutralScore, ModelVer: "no-session"}, nil
	}
	s.inflight.Add(1)
	defer s.inflight.Done()
	if ctx != nil {
		select {
		case <-ctx.Done():
			return BehaviorResult{}, ctx.Err()
		default:
		}
	}

	// 串行 Run（tensor buffer 共享；并发要 per-goroutine session pool）
	s.mu.Lock()
	defer s.mu.Unlock()
	sess = s.current.Load()
	if sess == nil {
		return BehaviorResult{AnomalyScore: neutralScore, ModelVer: "raced-out"}, nil
	}

	mouseVec := preprocessMouse(in)
	keyVec := preprocessKeystrokes(in)

	tensors := sess.sess.GetInputs()
	if len(tensors) < 2 {
		return BehaviorResult{}, errors.New("behavior_lstm: session inputs missing")
	}
	mouseT, ok := tensors[0].(*ort.Tensor[float32])
	if !ok {
		return BehaviorResult{}, errors.New("behavior_lstm: mouse input not float32")
	}
	keyT, ok := tensors[1].(*ort.Tensor[float32])
	if !ok {
		return BehaviorResult{}, errors.New("behavior_lstm: keystroke input not float32")
	}
	copy(mouseT.GetData(), mouseVec)
	copy(keyT.GetData(), keyVec)

	if err := sess.sess.Run(); err != nil {
		return BehaviorResult{}, fmt.Errorf("behavior_lstm run: %w", err)
	}
	outs := sess.sess.GetOutputs()
	if len(outs) == 0 {
		return BehaviorResult{}, errors.New("behavior_lstm: no outputs")
	}
	outT, ok := outs[0].(*ort.Tensor[float32])
	if !ok {
		return BehaviorResult{}, errors.New("behavior_lstm: output not float32")
	}
	data := outT.GetData()
	if len(data) == 0 {
		return BehaviorResult{}, errors.New("behavior_lstm: empty output")
	}
	raw := float64(data[0])
	if math.IsNaN(raw) || math.IsInf(raw, 0) {
		raw = neutralScore
	}
	if raw < 0 {
		raw = 0
	} else if raw > 1 {
		raw = 1
	}
	return BehaviorResult{AnomalyScore: raw, ModelVer: s.modelVer}, nil
}

// preprocessMouse pad/truncate 到固定长度 + (x,y,t) 归一化到 [0,1]。
// 输出 []float32 长度 = MouseSeqLen * MouseFeatDim = 600。
// 不足补 0（左 padding，保留最近的事件在右端给 LSTM 看）。
func preprocessMouse(in BehaviorInput) []float32 {
	out := make([]float32, MouseSeqLen*MouseFeatDim)
	w := in.ScreenW
	if w <= 0 {
		w = 1920
	}
	h := in.ScreenH
	if h <= 0 {
		h = 1080
	}
	dur := in.SessionDurMs
	if dur <= 0 {
		dur = 60000
	}
	src := in.Mouse
	// 超长 truncate：保留最后 MouseSeqLen 个（最近活动最有信号）
	if len(src) > MouseSeqLen {
		src = src[len(src)-MouseSeqLen:]
	}
	// 左 padding：把数据右对齐
	offset := MouseSeqLen - len(src)
	for i, e := range src {
		base := (offset + i) * MouseFeatDim
		out[base+0] = clamp01(float32(e.X) / float32(w))
		out[base+1] = clamp01(float32(e.Y) / float32(h))
		out[base+2] = clamp01(float32(e.T) / float32(dur))
	}
	return out
}

// preprocessKeystrokes 同理；KeyCode 用 / 256 归一化（覆盖 ASCII + 常见键码）。
// dwell / flight 用 / 1000 归一化（按 1s 为 baseline；多数 dwell < 200ms）。
func preprocessKeystrokes(in BehaviorInput) []float32 {
	out := make([]float32, KeystrokeSeqLen*KeystrokeFeatDim)
	src := in.Keystrokes
	if len(src) > KeystrokeSeqLen {
		src = src[len(src)-KeystrokeSeqLen:]
	}
	offset := KeystrokeSeqLen - len(src)
	for i, k := range src {
		base := (offset + i) * KeystrokeFeatDim
		out[base+0] = clamp01(float32(k.KeyCode) / 256.0)
		out[base+1] = clamp01(float32(k.Dwell) / 1000.0)
		out[base+2] = clamp01(float32(k.Flight) / 1000.0)
	}
	return out
}

func clamp01(v float32) float32 {
	if v != v { // NaN
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// Reload 跟 OnnxService.Reload 一样的原子热切换流程（mu 串行化 + 延迟释放）。
func (s *BehaviorLSTMService) Reload(modelPath string) error {
	if modelPath == "" {
		return ErrBehaviorModelPathEmpty
	}
	if err := initOnnxRuntime(); err != nil {
		return fmt.Errorf("%w: %v", ErrBehaviorModelLoad, err)
	}
	newSess, err := loadBehaviorSession(modelPath)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBehaviorModelLoad, err)
	}
	s.mu.Lock()
	old := s.current.Swap(newSess)
	s.modelPath = modelPath
	s.modelVer = deriveModelVer(modelPath)
	s.mu.Unlock()

	if old != nil {
		go func(sess *behaviorSession) {
			done := make(chan struct{})
			go func() { s.inflight.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			_ = sess.sess.Destroy()
		}(old)
	}
	return nil
}

// ModelVersion 当前模型版本（admin / audit 用）。
func (s *BehaviorLSTMService) ModelVersion() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelVer
}

// ModelPath 当前模型路径。
func (s *BehaviorLSTMService) ModelPath() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelPath
}

// Close 释放 onnx session（进程退出时调）。
func (s *BehaviorLSTMService) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.current.Swap(nil)
	if sess != nil {
		return sess.sess.Destroy()
	}
	return nil
}

// 防 unused import "filepath" 警告（deriveModelVer 在 onnx_service.go 已定义但
// 仅在 onnx build 下，本文件也是 onnx build → 共用就行；这行只是给 reader 提
// 醒同包共享 helper）。
var _ = filepath.Base
