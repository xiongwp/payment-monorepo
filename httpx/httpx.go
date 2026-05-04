// Package httpx 提供跨服务共用的 admin HTTP 响应工具。
//
// 原则：
//   - 成功：writeJSON(w, 200, body)
//   - 错误：writeErr(w, status, "invalid_config_key", "config_key required")
//   - 405:  methodNotAllowed(w)
//
// ErrorEnvelope 格式固定：{"code": status, "message": human_readable, "details": code_string}
// 前端 admin-web 可据 code 判重试策略；message 展示给用户；details 机读分类。
package httpx

import (
	"encoding/json"
	"net/http"
)

// ErrorEnvelope 统一错误 body。
type ErrorEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// WriteJSON 写 JSON 响应。错误忽略（http 连接已无效写了也没用）。
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteErr 写统一错误响应。
//   WriteErr(w, 400, "bad_request", "config_key required")
func WriteErr(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorEnvelope{Code: status, Message: message, Details: code})
}

// MethodNotAllowed 常见 405。
func MethodNotAllowed(w http.ResponseWriter) {
	WriteErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}
