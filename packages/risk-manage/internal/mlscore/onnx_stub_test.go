//go:build !onnx

// onnx_stub_test.go：默认 build 下 OnnxService stub 行为的测试。
// 验证：
//   - NewOnnxService 返 ErrOnnxNotEnabled
//   - admin handler 在 stub mode 下返合理 status code

package mlscore

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewOnnxService_StubAlwaysErrors(t *testing.T) {
	svc, err := NewOnnxService("/tmp/fake.onnx", []string{"amount"})
	if svc != nil {
		t.Errorf("stub should return nil svc, got %v", svc)
	}
	if !errors.Is(err, ErrOnnxNotEnabled) {
		t.Errorf("err=%v want ErrOnnxNotEnabled", err)
	}
}

func TestStubOnnxService_AllMethodsSafe(t *testing.T) {
	// 即使 svc=nil 上述也是 stub 设计；演示直接用 zero-value 类型也不 panic
	var s OnnxService
	if v := s.ModelVersion(); v != "" {
		t.Errorf("ModelVersion stub=%q want empty", v)
	}
	if p := s.ModelPath(); p != "" {
		t.Errorf("ModelPath stub=%q want empty", p)
	}
	if fo := s.FeatureOrder(); fo != nil {
		t.Errorf("FeatureOrder stub=%v want nil", fo)
	}
	if err := s.Reload("/foo", []string{"a"}); !errors.Is(err, ErrOnnxNotEnabled) {
		t.Errorf("Reload stub err=%v", err)
	}
	_, err := s.Score(nil, Features{})
	if !errors.Is(err, ErrOnnxNotEnabled) {
		t.Errorf("Score stub err=%v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close stub err=%v want nil", err)
	}
}

func TestAdminHandler_InfoNilSvc(t *testing.T) {
	mux := http.NewServeMux()
	RegisterOnnxAdminHandlers(mux, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/ml/onnx/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", resp.StatusCode)
	}
	var info OnnxInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Enabled {
		t.Error("nil svc should report enabled=false")
	}
	if !strings.Contains(info.Note, "not configured") {
		t.Errorf("note=%q expected 'not configured'", info.Note)
	}
}

func TestAdminHandler_ReloadNilSvc503(t *testing.T) {
	mux := http.NewServeMux()
	RegisterOnnxAdminHandlers(mux, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(OnnxReloadRequest{
		ModelPath:    "/tmp/x.onnx",
		FeatureOrder: []string{"amount"},
	})
	resp, _ := http.Post(srv.URL+"/admin/ml/onnx/reload", "application/json", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", resp.StatusCode)
	}
}

func TestAdminHandler_ReloadBadRequest(t *testing.T) {
	// 给个非 nil（stub）service；reload 调用先做 body 校验返 400；
	// 通过 body 校验后会到 svc.Reload，stub 返 ErrOnnxNotEnabled → 503（见单独测试）。
	mux := http.NewServeMux()
	RegisterOnnxAdminHandlers(mux, &OnnxService{})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 空 body → 400
	resp, _ := http.Post(srv.URL+"/admin/ml/onnx/reload", "application/json", strings.NewReader("{}"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty body status=%d want 400", resp.StatusCode)
	}

	// model_path 有但 feature_order 缺 → 400
	body, _ := json.Marshal(map[string]string{"model_path": "/x.onnx"})
	resp2, _ := http.Post(srv.URL+"/admin/ml/onnx/reload", "application/json", bytes.NewReader(body))
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("missing feature_order status=%d want 400", resp2.StatusCode)
	}
}

func TestAdminHandler_ReloadStubBuild503(t *testing.T) {
	// stub build 下，OnnxService{} 的 Reload 返 ErrOnnxNotEnabled → 503
	mux := http.NewServeMux()
	RegisterOnnxAdminHandlers(mux, &OnnxService{})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	body, _ := json.Marshal(OnnxReloadRequest{
		ModelPath:    "/tmp/x.onnx",
		FeatureOrder: []string{"amount"},
	})
	resp, _ := http.Post(srv.URL+"/admin/ml/onnx/reload", "application/json", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503 (stub build can't load)", resp.StatusCode)
	}
}
