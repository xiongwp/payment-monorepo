//go:build onnx

// onnx_service.go：基于 ONNX Runtime 的推理服务（build tag `onnx` 启用）。
//
// 为什么 build tag：
//   - github.com/yalue/onnxruntime_go 依赖系统 libonnxruntime.so/.dylib
//   - 默认 build 不应该绑这个依赖（CI / Docker base image 不一定装 onnxruntime）
//   - 客户要用 GBDT 模型时显式 `go build -tags onnx ./cmd/server`
//
// 工作流（详见 ONNX_PIPELINE.md）：
//
//	1. ML 团队用 Python (lightgbm/xgboost) 训练模型
//	2. 用 onnxmltools / skl2onnx 转 .onnx
//	3. 上传到 S3/OSS（命名 model_<version>_<gitsha>.onnx）
//	4. 运维 admin POST /admin/ml/onnx/reload {model_path, feature_order}
//	5. OnnxService.Reload 原子切换 session（in-flight 请求不受影响）
//
// 特征顺序契约（feature_order）：
//   - ML 训练时 X 矩阵的列顺序就是 feature_order
//   - 推理时 Features → []float32 严格按这个顺序填
//   - 不在 feature_order 中的 Features 字段被忽略
//   - feature_order 中找不到对应字段的 → 用 0 填（log warn 一次）
//   - 长度跟模型 input shape 必须对齐，不对就拒绝加载
//
// Reload 原子性：
//   - sessionHolder 用 atomic.Pointer 切换
//   - 旧 session 不立刻 Destroy（in-flight 推理可能还在跑）
//   - 改用 sync.WaitGroup 兜底 + 延时 5s 后释放（实现见 Reload）
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

// OnnxService 用 ONNX Runtime 推理的 ScoreService 实现。
//
// 并发模型：
//   - Score 是 hot path，session 通过 atomic.Pointer 无锁读
//   - Reload 是冷路径，用 mu 串行化避免两个 admin 请求同时切
//   - 旧 session 在新 session 上线后延迟释放，等 in-flight 完成
type OnnxService struct {
	// 通过 atomic.Pointer 实现 session 原子热切换
	current atomic.Pointer[onnxSession]

	mu        sync.Mutex // 串行化 Reload；只有 Reload / NewOnnxService 加
	modelPath string     // 当前模型路径（mu 保护写，原子读不需要锁因为字符串赋值原子）
	modelVer  string     // 派生自 modelPath（文件名）

	// inflight 给 Score 计数；Reload 等 inflight=0 再 Destroy 旧 session
	// （超时 5s 兜底，防慢请求拖死 reload）
	inflight sync.WaitGroup
}

// onnxSession 单个模型快照。Reload 时整体替换。
//
// Tensor 复用：本实现每次 Score new 一个 input tensor（小开销），不复用——
// 因为复用要 per-goroutine pool 或加锁，跟原子 hot-swap 不太协调。如果剖
// 性能发现 tensor alloc 是瓶颈，再上 sync.Pool。
type onnxSession struct {
	sess      *ort.AdvancedSession
	featOrder []string          // 特征顺序（驱动 Features → []float32）
	featIndex map[string]int    // featOrder 反查 O(1)
	inputDim  int               // = len(featOrder)，模型期望的输入维度
	inputName string            // ONNX 模型 input tensor 名
	outName   string            // ONNX 模型 output tensor 名
}

var (
	// ErrOnnxNotEnabled 默认 build 下尝试创建 OnnxService 会返这个。
	// onnx build tag 版本不会用到（这里只是为了让 stub 共享同名 sentinel）。
	ErrOnnxNotEnabled = errors.New("onnx runtime not enabled: rebuild with -tags onnx")

	// ErrModelPathEmpty model_path 配置为空时返回（caller 应该走 logistic fallback）
	ErrModelPathEmpty = errors.New("onnx: model path is empty")

	// ErrFeatureOrderEmpty feature_order 不能为空（无法构造输入向量）
	ErrFeatureOrderEmpty = errors.New("onnx: feature_order is empty")

	// ErrInputDimMismatch 模型 input shape 跟 feature_order 长度不一致
	ErrInputDimMismatch = errors.New("onnx: model input dim mismatch feature_order")
)

// 全局 onnxruntime 环境初始化（lazy + once）。
// onnxruntime_go 要求进程级别 InitializeEnvironment 之后才能 NewAdvancedSession。
var (
	ortInitOnce sync.Once
	ortInitErr  error
)

func initOnnxRuntime() error {
	ortInitOnce.Do(func() {
		// 优先环境变量 ONNXRUNTIME_LIB_PATH 指向 libonnxruntime.so/.dylib
		// 没配的话依赖系统 ld.so 找到（推荐 docker base image 装一份）。
		if p := os.Getenv("ONNXRUNTIME_LIB_PATH"); p != "" {
			ort.SetSharedLibraryPath(p)
		}
		if err := ort.InitializeEnvironment(); err != nil {
			ortInitErr = fmt.Errorf("onnxruntime init: %w", err)
		}
	})
	return ortInitErr
}

// NewOnnxService 加载 modelPath，按 featureOrder 构造输入向量。
//
// 失败原因：
//   - modelPath 不存在或不可读
//   - onnxruntime 初始化失败（so 库没装 / SetSharedLibraryPath 错）
//   - 模型 input dim 跟 featureOrder 长度不一致
//
// 成功后 OnnxService 即可处理 Score 调用；Reload 可热切换到别的模型。
func NewOnnxService(modelPath string, featureOrder []string) (*OnnxService, error) {
	if modelPath == "" {
		return nil, ErrModelPathEmpty
	}
	if len(featureOrder) == 0 {
		return nil, ErrFeatureOrderEmpty
	}
	if err := initOnnxRuntime(); err != nil {
		return nil, err
	}
	sess, err := loadOnnxSession(modelPath, featureOrder)
	if err != nil {
		return nil, err
	}
	svc := &OnnxService{
		modelPath: modelPath,
		modelVer:  deriveModelVer(modelPath),
	}
	svc.current.Store(sess)
	return svc, nil
}

// loadOnnxSession 干活函数：建 ort.AdvancedSession + 校验 input shape。
//
// 假设：
//   - 模型只有 1 个 input tensor，shape = [N, F] 或 [F]（N=batch）
//   - 1 个 output tensor，shape = [N] 或 [N, 1]（fraud probability）
//   - 多 input / multi-output 模型暂不支持（出现再扩展 Score signature）
func loadOnnxSession(modelPath string, featureOrder []string) (*onnxSession, error) {
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("onnx model file: %w", err)
	}

	// 通过 GetInputOutputInfo 读 metadata（input/output name + shape）
	inputs, outputs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("onnx introspect: %w", err)
	}
	if len(inputs) == 0 || len(outputs) == 0 {
		return nil, errors.New("onnx: model has no inputs or outputs")
	}
	inputName := inputs[0].Name
	outName := outputs[0].Name

	// 校验 input shape：最后一维必须等于 len(featureOrder)
	// （第一维可能是 -1 表示动态 batch）
	inDims := inputs[0].Dimensions
	expectedF := int64(len(featureOrder))
	lastDim := inDims[len(inDims)-1]
	if lastDim > 0 && lastDim != expectedF {
		return nil, fmt.Errorf("%w: model expects %d, got %d",
			ErrInputDimMismatch, lastDim, expectedF)
	}

	// 准备 batch=1 input tensor（Score 单条调用；批量将来再加 ScoreBatch）
	inputShape := ort.NewShape(1, expectedF)
	inputTensor, err := ort.NewEmptyTensor[float32](inputShape)
	if err != nil {
		return nil, fmt.Errorf("onnx new input tensor: %w", err)
	}
	outputShape := ort.NewShape(1, 1)
	outputTensor, err := ort.NewEmptyTensor[float32](outputShape)
	if err != nil {
		_ = inputTensor.Destroy()
		return nil, fmt.Errorf("onnx new output tensor: %w", err)
	}
	sess, err := ort.NewAdvancedSession(
		modelPath,
		[]string{inputName},
		[]string{outName},
		[]ort.Value{inputTensor},
		[]ort.Value{outputTensor},
		nil, // SessionOptions: 默认；要 GPU / thread pool 调优再传
	)
	if err != nil {
		_ = inputTensor.Destroy()
		_ = outputTensor.Destroy()
		return nil, fmt.Errorf("onnx new session: %w", err)
	}

	idx := make(map[string]int, len(featureOrder))
	for i, name := range featureOrder {
		idx[name] = i
	}

	return &onnxSession{
		sess:      sess,
		featOrder: append([]string(nil), featureOrder...), // 防 caller 后续修改
		featIndex: idx,
		inputDim:  len(featureOrder),
		inputName: inputName,
		outName:   outName,
	}, nil
}

// Score 推理单条样本。
//
// 流程：
//  1. atomic.Pointer 读当前 session（无锁）
//  2. Features → []float32（按 featOrder，缺失填 0）
//  3. ort.AdvancedSession.Run → 取 output[0]
//  4. clamp 到 [0, 1]
//
// inflight WaitGroup 让 Reload 知道有多少调用还在跑；释放旧 session 前等。
func (s *OnnxService) Score(ctx context.Context, f Features) (Result, error) {
	sess := s.current.Load()
	if sess == nil {
		return Result{}, errors.New("onnx: no session loaded")
	}
	s.inflight.Add(1)
	defer s.inflight.Done()

	// ctx 超时检查（推理本身没法中断 native call，但 caller 已经放弃就别浪费）
	if ctx != nil {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		default:
		}
	}

	// 当前实现共享 tensor buffer，必须串行化 Run 调用；用 mu 保证。
	// 真生产要并发：每个 goroutine 用 ort.NewSession + 自己的 input tensor。
	// 这里 keep simple，单 session 模式（QPS < 5k 够用）。
	s.mu.Lock()
	defer s.mu.Unlock()
	sess = s.current.Load() // double-check（Reload 可能切了）
	if sess == nil {
		return Result{}, errors.New("onnx: session became nil")
	}

	vec := featuresToFloat32(f, sess.featOrder, sess.featIndex)
	// 写入 input tensor（sess.sess 的 input value 是 NewEmptyTensor 创建的，
	// 我们这里需要拿到 underlying data slice 写。onnxruntime_go API：
	// AdvancedSession 在 New 时绑 input value，要重新设要走别的 API。
	// 简化版：每次 Score 拿 tensor.GetData() 直接覆盖底层 slice）
	inputs := sess.sess.GetInputs()
	if len(inputs) == 0 {
		return Result{}, errors.New("onnx: session has no inputs")
	}
	inT, ok := inputs[0].(*ort.Tensor[float32])
	if !ok {
		return Result{}, errors.New("onnx: input tensor type mismatch (expected float32)")
	}
	data := inT.GetData()
	if len(data) != len(vec) {
		return Result{}, fmt.Errorf("onnx: input buffer size %d != feature vec %d",
			len(data), len(vec))
	}
	copy(data, vec)

	if err := sess.sess.Run(); err != nil {
		return Result{}, fmt.Errorf("onnx run: %w", err)
	}
	outputs := sess.sess.GetOutputs()
	if len(outputs) == 0 {
		return Result{}, errors.New("onnx: no outputs")
	}
	outT, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return Result{}, errors.New("onnx: output tensor type mismatch (expected float32)")
	}
	outData := outT.GetData()
	if len(outData) == 0 {
		return Result{}, errors.New("onnx: empty output")
	}
	raw := float64(outData[0])
	// clamp [0,1]：GBDT / LR 通常输出已是 sigmoid 后概率；保险 clamp。
	if math.IsNaN(raw) || math.IsInf(raw, 0) {
		raw = 0
	}
	if raw < 0 {
		raw = 0
	} else if raw > 1 {
		raw = 1
	}
	return Result{Score: raw, ModelVer: s.modelVer}, nil
}

// Reload 原子热切换模型。
//
// 流程：
//  1. 串行化（mu 防止两个 reload 同时跑）
//  2. 加载新 session（失败不影响旧）
//  3. atomic.Pointer.Store 切换
//  4. 异步等 inflight 归零（或 5s 超时）后 Destroy 旧 session
//
// 为什么不直接 Destroy 旧 session：Score 此刻可能正在跑（持有旧指针），
// native 调用没法中断；提前释放 → segfault。
func (s *OnnxService) Reload(modelPath string, featureOrder []string) error {
	if modelPath == "" {
		return ErrModelPathEmpty
	}
	if len(featureOrder) == 0 {
		return ErrFeatureOrderEmpty
	}
	if err := initOnnxRuntime(); err != nil {
		return err
	}
	newSess, err := loadOnnxSession(modelPath, featureOrder)
	if err != nil {
		return err
	}
	// 这里加 mu 避免 reload 跟 Score 的 mu 抢；Score 也持 mu 串行。
	s.mu.Lock()
	old := s.current.Swap(newSess)
	s.modelPath = modelPath
	s.modelVer = deriveModelVer(modelPath)
	s.mu.Unlock()

	if old != nil {
		go func(sess *onnxSession) {
			// 等 inflight 归零或超时 5s
			done := make(chan struct{})
			go func() {
				s.inflight.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				// 超时：仍然 Destroy；in-flight 极少数遭遇 segfault 概率
				// （生产里 5s 内一定够，单条推理 <10ms）
			}
			_ = sess.sess.Destroy()
		}(old)
	}
	return nil
}

// ModelVersion 当前 modelVer（文件名 sans 扩展名，给审计 / dashboard）。
func (s *OnnxService) ModelVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelVer
}

// ModelPath 当前模型文件路径（admin /info 端点用）。
func (s *OnnxService) ModelPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelPath
}

// FeatureOrder 当前 feature_order 副本（admin /info 端点用）。
func (s *OnnxService) FeatureOrder() []string {
	sess := s.current.Load()
	if sess == nil {
		return nil
	}
	return append([]string(nil), sess.featOrder...)
}

// Close 关闭 service；释放 onnx session。
// 仅在进程退出时调用；运行期间用 Reload 切换。
func (s *OnnxService) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.current.Swap(nil)
	if sess != nil {
		return sess.sess.Destroy()
	}
	return nil
}

// deriveModelVer 从路径推导 ModelVer：
//
//	/var/models/risk_v2.3_a1b2c3d.onnx → "risk_v2.3_a1b2c3d"
//
// 约定模型文件名包含 version + git sha；详见 ONNX_PIPELINE.md "模型版本管理"。
func deriveModelVer(modelPath string) string {
	base := filepath.Base(modelPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
