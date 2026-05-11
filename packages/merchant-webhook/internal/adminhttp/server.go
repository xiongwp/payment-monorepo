// merchant-webhook HTTP server.
//
//   POST /api/v1/endpoints      商户注册 webhook endpoint
//   GET  /api/v1/endpoints?merchant_id=
//   POST /api/v1/events         业务侧发事件 (内部调用)

package adminhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/merchant-webhook/internal/dispatcher"
	"reconcile-system/packages/merchant-webhook/internal/domain"
	"reconcile-system/packages/merchant-webhook/internal/repository"
)

type Server struct {
	repo *repository.MemoryRepo
	disp *dispatcher.Dispatcher
	log  *zap.Logger
}

func New(repo *repository.MemoryRepo, disp *dispatcher.Dispatcher, log *zap.Logger) *Server {
	return &Server{repo: repo, disp: disp, log: log}
}

func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/endpoints", s.endpointsHandler)
	mux.HandleFunc("/api/v1/events", s.eventsHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
}

// endpointsHandler GET/POST /api/v1/endpoints
func (s *Server) endpointsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var body struct {
			MerchantID  string `json:"merchant_id"`
			URL         string `json:"url"`
			EventTypes  string `json:"event_types"`   // CSV
			Description string `json:"description"`
			MaxRetries  int    `json:"max_retries"`
			TimeoutMS   int    `json:"timeout_ms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if body.MerchantID == "" || body.URL == "" || body.EventTypes == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("merchant_id, url, event_types required"))
			return
		}
		secret := randHex(32)
		now := time.Now().UTC()
		ep := &domain.Endpoint{
			MerchantID: body.MerchantID, URL: body.URL,
			EventTypes: body.EventTypes, Description: body.Description,
			Active: true, TLSVerify: true,
			MaxRetries: body.MaxRetries, TimeoutMS: body.TimeoutMS,
			CreatedAt: now, UpdatedAt: now,
		}
		id, err := s.repo.RegisterEndpoint(ep, secret)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"endpoint":      ep,
			"id":            id,
			"signing_secret": secret, // 只这一次返完整 secret，后续只能看 last4
		})
	case http.MethodGet:
		mid := r.URL.Query().Get("merchant_id")
		if mid == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("merchant_id required"))
			return
		}
		// 简化：取所有 event_types 下的 endpoint 去重
		eps, _ := s.repo.ListEndpointsForEvent(r.Context(), mid, "")
		writeJSON(w, http.StatusOK, map[string]any{"endpoints": eps})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// eventsHandler POST /api/v1/events — 业务侧发事件入口。
func (s *Server) eventsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		MerchantID string          `json:"merchant_id"`
		EventType  string          `json:"event_type"`
		EventID    string          `json:"event_id"`
		Payload    json.RawMessage `json:"payload"`
		TraceID    string          `json:"trace_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ev := &domain.Event{
		MerchantID: body.MerchantID, EventType: body.EventType,
		EventID: body.EventID, Payload: body.Payload, TraceID: body.TraceID,
	}
	if err := s.disp.Enqueue(ctx, ev); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "event_id": ev.ID})
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}
