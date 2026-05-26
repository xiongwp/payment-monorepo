//go:build onnx

// onnx_service_test.go：OnnxService 完整推理测试。
//
// 仅 -tags onnx build 运行（需要 onnxruntime so）。CI 配两套 build：
// 默认 build 跑大多数测试；nightly job 跑 -tags onnx + libonnxruntime 镜像。
//
// Fixture：environment var ONNX_TEST_MODEL 指向一个 .onnx 文件
// （ML 团队预生成；测试目录不 check in 二进制 fixture）。没设就 t.Skip。
//
// 推荐 fixture：sklearn 训一个 2 维 LR 当 onnx 模型，input shape [1,2]
// → output shape [1,1]。Python：
//
//	from sklearn.linear_model import LogisticRegression
//	from skl2onnx import to_onnx
//	import numpy as np
//	X = np.array([[0,0],[1,1],[0,1],[1,0]], dtype=np.float32)
//	y = np.array([0,1,0,1])
//	clf = LogisticRegression().fit(X, y)
//	onx = to_onnx(clf, X[:1], target_opset=12)
//	open("test_model.onnx","wb").write(onx.SerializeToString())

package mlscore

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixturePath(t *testing.T) string {
	t.Helper()
	p := os.Getenv("ONNX_TEST_MODEL")
	if p == "" {
		t.Skip("ONNX_TEST_MODEL not set; skipping; see test header for fixture recipe")
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("ONNX_TEST_MODEL=%s not readable: %v", p, err)
	}
	return p
}

func TestOnnxService_LoadAndScore(t *testing.T) {
	path := fixturePath(t)
	// fixture 假设 2 个特征；调整成实际模型的 feature_order
	order := []string{"amount", "ip_proxy"}
	svc, err := NewOnnxService(path, order)
	if err != nil {
		t.Fatalf("NewOnnxService: %v", err)
	}
	defer svc.Close()

	res, err := svc.Score(context.Background(), Features{Amount: 1, IPProxy: true})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if res.Score < 0 || res.Score > 1 {
		t.Errorf("score out of range: %v", res.Score)
	}
	if res.ModelVer == "" {
		t.Error("ModelVer empty")
	}
}

func TestOnnxService_ModelVersion(t *testing.T) {
	path := fixturePath(t)
	svc, err := NewOnnxService(path, []string{"amount", "ip_proxy"})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ver := svc.ModelVersion()
	if ver == "" {
		t.Error("ModelVersion empty after load")
	}
}

func TestOnnxService_InputDimMismatch(t *testing.T) {
	path := fixturePath(t)
	// 故意传超大 feature order 触发 dim mismatch
	tooLong := []string{"amount", "ip_proxy", "ip_vpn", "fake1", "fake2", "fake3", "fake4"}
	_, err := NewOnnxService(path, tooLong)
	if err == nil {
		t.Error("expected dim mismatch err")
	}
}

func TestOnnxService_ReloadAtomic(t *testing.T) {
	// 并发 Score + Reload；不应 panic，Score 总有可读结果或 error（不能 segfault）。
	path := fixturePath(t)
	order := []string{"amount", "ip_proxy"}
	svc, err := NewOnnxService(path, order)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var scoreCount, errCount atomic.Int64

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := svc.Score(context.Background(), Features{Amount: 100})
				if err != nil {
					errCount.Add(1)
				} else {
					scoreCount.Add(1)
				}
			}
		}()
	}

	// 同时跑 reload 几次
	for i := 0; i < 3; i++ {
		time.Sleep(50 * time.Millisecond)
		if err := svc.Reload(path, order); err != nil {
			t.Errorf("Reload[%d]: %v", i, err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	if scoreCount.Load() == 0 {
		t.Error("no successful scores during concurrent reload")
	}
	t.Logf("reload concurrency: %d scores OK, %d errors", scoreCount.Load(), errCount.Load())
}

func TestNewOnnxService_EmptyArgs(t *testing.T) {
	if _, err := NewOnnxService("", []string{"amount"}); err != ErrModelPathEmpty {
		t.Errorf("empty path err=%v want ErrModelPathEmpty", err)
	}
	if _, err := NewOnnxService("/tmp/x.onnx", nil); err != ErrFeatureOrderEmpty {
		t.Errorf("empty order err=%v want ErrFeatureOrderEmpty", err)
	}
}
