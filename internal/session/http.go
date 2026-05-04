package session

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"go.uber.org/zap"
)

// RegisterHandlers 把 SDK endpoint 注册到 mux：
//
//	POST /v1/risk/session            创建（接受 fingerprint），返回 {session_id}
//	POST /v1/risk/session/finalize  覆盖 behavior（接受 session_id + 行为字段）
//
// 这两个端点是对**公网开放**的（SDK 发出来），没有 auth。设计上不返回任何
// 敏感数据，纯写入；session_id 是不可枚举的 16-byte 随机值。
//
// 生产部署应在 api-gateway 层加 per-IP rate limit 防刷（已经有 IPRPS / IPBurst
// 配置）。
func RegisterHandlers(mux *http.ServeMux, store Store, logger *zap.Logger) {
	mux.HandleFunc("/v1/risk/session", makeCreateHandler(store, logger))
	mux.HandleFunc("/v1/risk/session/finalize", makeFinalizeHandler(store, logger))
}

func makeCreateHandler(store Store, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body Snapshot
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		body.IPAddress = clientIP(r)
		id, err := store.Create(body)
		if err != nil {
			logger.Warn("session create failed", zap.Error(err))
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"session_id": id})
	}
}

func makeFinalizeHandler(store Store, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID            string   `json:"session_id"`
			TimeToCheckoutMs     int64    `json:"timeToCheckoutMs"`
			MouseMovementEntropy float64  `json:"mouseMovementEntropy"`
			ClickIntervalMs      int      `json:"clickIntervalMs"`
			ScrollSpeedPxPerSec  float64  `json:"scrollSpeedPxPerSec"`
			TypingRhythmCV       float64  `json:"typingRhythmCV"`
			KeystrokeCount       int      `json:"keystrokeCount"`
			MouseMoves           int      `json:"mouseMoves"`
			PastedFields         []string `json:"pastedFields"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if body.SessionID == "" {
			http.Error(w, `{"error":"session_id required"}`, http.StatusBadRequest)
			return
		}
		err := store.Finalize(body.SessionID, BehaviorPatch{
			TimeToCheckoutMs:     body.TimeToCheckoutMs,
			MouseMovementEntropy: body.MouseMovementEntropy,
			ClickIntervalMs:      body.ClickIntervalMs,
			ScrollSpeedPxPerSec:  body.ScrollSpeedPxPerSec,
			TypingRhythmCV:       body.TypingRhythmCV,
			KeystrokeCount:       body.KeystrokeCount,
			MouseMoves:           body.MouseMoves,
			PastedFields:         body.PastedFields,
		})
		if err != nil {
			var nf ErrNotFound
			if errors.As(err, &nf) {
				http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
				return
			}
			logger.Warn("session finalize failed", zap.Error(err))
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// clientIP 提取最近一跳 IP；信任 LB / CDN 的 X-Forwarded-For（生产应在 LB
// 层做信任清洗）。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return strings.TrimSpace(xrip)
	}
	if i := strings.LastIndexByte(r.RemoteAddr, ':'); i >= 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}
