// onnx_admin.go：ONNX 模型 admin endpoint handlers。
//
// 跟 cmd/server/main.go 的 registerDriftHandlers 风格一致；caller 在 main.go
// 拿到 *OnnxService 后调 RegisterOnnxAdminHandlers(mux, svc, logger)。
//
// 端点：
//
//	POST /admin/ml/onnx/reload   Body: {"model_path":"...", "feature_order":[...]}
//	                             → 200 {"model_ver":"...", "feature_order":[...]}
//	                             → 503 {"error":"rebuild with -tags onnx"}（默认 build）
//	                             → 400 {"error":"input dim mismatch ..."}
//
//	GET  /admin/ml/onnx/info     → 200 {"model_path", "model_ver", "feature_order"}
//	                             → 200 {"enabled":false} （stub build）
//
// 设计选择：放在 mlscore 包而不是 cmd/server，让 admin endpoint 单元可以独
// 立测试；main.go 只负责 mux.HandleFunc 绑定。
//
// 不放在 build tag 后面：handler 本身没引 onnxruntime；OnnxService 方法
// (Reload/ModelVersion/...) 在 stub build 也存在，只是返 ErrOnnxNotEnabled。

package mlscore

import (
	"encoding/json"
	"errors"
	"net/http"
)

// OnnxReloadRequest admin reload payload。
//
// feature_order 必传：哪怕跟当前一样也要 explicit 传，避免 ML 团队改了模型
// schema 但忘了同步给工程，加载了 dim 不对的模型。
type OnnxReloadRequest struct {
	ModelPath    string   `json:"model_path"`
	FeatureOrder []string `json:"feature_order"`
}

// OnnxInfoResponse GET /admin/ml/onnx/info 响应。
type OnnxInfoResponse struct {
	Enabled      bool     `json:"enabled"`
	ModelPath    string   `json:"model_path,omitempty"`
	ModelVer     string   `json:"model_ver,omitempty"`
	FeatureOrder []string `json:"feature_order,omitempty"`
	Note         string   `json:"note,omitempty"` // stub build 用："rebuild with -tags onnx"
}

// RegisterOnnxAdminHandlers 在 mux 上注册 /admin/ml/onnx/{reload,info}。
//
// svc 可为 nil（配置没启用 onnx）→ /info 返 enabled=false；/reload 返 503。
// 这样 main.go 总是无条件 register，不需要 if svc != nil 包裹路由。
func RegisterOnnxAdminHandlers(mux *http.ServeMux, svc *OnnxService) {
	mux.HandleFunc("/admin/ml/onnx/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if svc == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "onnx service not configured; check mlscore.onnx.* in config",
			})
			return
		}
		var req OnnxReloadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
			return
		}
		if req.ModelPath == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model_path required"})
			return
		}
		if len(req.FeatureOrder) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "feature_order required"})
			return
		}
		if err := svc.Reload(req.ModelPath, req.FeatureOrder); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, ErrOnnxNotEnabled) {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, OnnxInfoResponse{
			Enabled:      true,
			ModelPath:    svc.ModelPath(),
			ModelVer:     svc.ModelVersion(),
			FeatureOrder: svc.FeatureOrder(),
		})
	})

	mux.HandleFunc("/admin/ml/onnx/info", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if svc == nil {
			writeJSON(w, http.StatusOK, OnnxInfoResponse{
				Enabled: false,
				Note:    "onnx service not configured (mlscore.onnx.model_path empty)",
			})
			return
		}
		ver := svc.ModelVersion()
		// stub build 下 ver 为空说明 binary 没编 -tags onnx
		if ver == "" {
			writeJSON(w, http.StatusOK, OnnxInfoResponse{
				Enabled: false,
				Note:    "onnx runtime not enabled in this binary; rebuild with -tags onnx",
			})
			return
		}
		writeJSON(w, http.StatusOK, OnnxInfoResponse{
			Enabled:      true,
			ModelPath:    svc.ModelPath(),
			ModelVer:     ver,
			FeatureOrder: svc.FeatureOrder(),
		})
	})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
