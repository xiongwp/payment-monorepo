//go:build !onnx

// onnx_stub.go：默认 build 下 OnnxService 的 stub 实现。
//
// 为什么需要 stub：
//   - main.go / admin handler 代码无条件引用 NewOnnxService（不能 build-tag 包裹所有 call site）
//   - 默认 build 不链接 onnxruntime_go（避免系统装 libonnxruntime.so）
//   - 配置 model_path != "" 但没用 -tags onnx 编译时，启动 clear error
//
// stub 行为：
//   - NewOnnxService 始终返 ErrOnnxNotEnabled
//   - 类型 OnnxService 仍存在（让 main.go 编过）；Score 永远返 noop
//   - admin 端点 /admin/ml/onnx/* 仍注册，但返 503 + "rebuild with -tags onnx"
//
// 启用 ONNX：
//
//	go build -tags onnx -o risk-server ./cmd/server
//	# 需要装：apt install libonnxruntime-dev / brew install onnxruntime
//	# 或下载 binary 后 ONNXRUNTIME_LIB_PATH=/path/to/libonnxruntime.so
package mlscore

import (
	"context"
	"errors"
	"sync"
)

// OnnxService stub 类型，默认 build 下的占位。
// 字段保持 minimum 让 main.go 引用编译通过；Score 永远返错。
type OnnxService struct {
	mu        sync.Mutex
	modelPath string
}

var (
	// ErrOnnxNotEnabled 默认 build 下尝试创建 / Reload OnnxService 都返这个。
	// 让 caller（admin handler / main.go startup）能区分"配置错"和"binary 没编 onnx"。
	ErrOnnxNotEnabled = errors.New("onnx runtime not enabled: rebuild with -tags onnx")

	// ErrModelPathEmpty model_path 配置为空（caller 应该 fallback 到 logistic）
	ErrModelPathEmpty = errors.New("onnx: model path is empty")

	// ErrFeatureOrderEmpty feature_order 不能为空
	ErrFeatureOrderEmpty = errors.New("onnx: feature_order is empty")

	// ErrInputDimMismatch 模型 input shape 跟 feature_order 长度不一致
	// （stub 里用不到，仅为了 onnx tag / 无 tag 版本错误集合对齐）
	ErrInputDimMismatch = errors.New("onnx: model input dim mismatch feature_order")
)

// NewOnnxService stub 实现：永远返 ErrOnnxNotEnabled。
//
// 调用方典型路径：main.go newMLScore 看 model_path != "" → NewOnnxService
// → 拿到 err 后 log warn + fallback 走 NewLogisticService。
func NewOnnxService(modelPath string, featureOrder []string) (*OnnxService, error) {
	return nil, ErrOnnxNotEnabled
}

// Score stub：永远返 ErrOnnxNotEnabled。生产 build 不应该走到这（NewOnnxService
// 已经 fail 了）；defensive 兜底以防 nil 检查漏。
func (s *OnnxService) Score(_ context.Context, _ Features) (Result, error) {
	return Result{}, ErrOnnxNotEnabled
}

// Reload stub：永远返 ErrOnnxNotEnabled。admin 端点用，让运维能从响应区分
// "binary 没编 onnx" vs "模型文件错"。
func (s *OnnxService) Reload(_ string, _ []string) error {
	return ErrOnnxNotEnabled
}

// ModelVersion stub：返空字符串。
func (s *OnnxService) ModelVersion() string { return "" }

// ModelPath stub：返空字符串。
func (s *OnnxService) ModelPath() string { return "" }

// FeatureOrder stub：返 nil。
func (s *OnnxService) FeatureOrder() []string { return nil }

// Close stub：no-op。
func (s *OnnxService) Close() error { return nil }
