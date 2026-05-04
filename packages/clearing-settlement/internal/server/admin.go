// Package server: admin HTTP（探针 + 触发结算）。
//
// 设计：与 accounting-system / api-gateway 同形 — X-Admin-Token 校验 +
// 健康路径豁免 + body size 上限 + slowloris 防御超时。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/xiongwp/payment-util/healthx"
	"go.uber.org/zap"

	"github.com/xiongwp/clearing-settlement/internal/service"
)

// AdminConfig admin HTTP 配置。
type AdminConfig struct {
	Port  int
	Token string // 空 = warn-only
}

// AdminServer 内部管理端口。
type AdminServer struct {
	cfg     AdminConfig
	srv     *http.Server
	svc     service.SettlementService
	logger  *zap.Logger
}

// NewAdminServer 构造。
func NewAdminServer(cfg AdminConfig, svc service.SettlementService, logger *zap.Logger) *AdminServer {
	mux := http.NewServeMux()
	a := &AdminServer{cfg: cfg, svc: svc, logger: logger}

	mux.HandleFunc("/admin/health", healthx.Liveness)
	// /admin/health/readiness 后续 PR 接入 DB + accounting client 后再加 probes；
	// 当前是 skeleton 阶段没有依赖 → 退到 always-OK readiness（payment-util/
	// healthx.Readiness() 不带 probe 即等同 always-OK）。
	mux.HandleFunc("/admin/health/readiness", healthx.Readiness())
	mux.HandleFunc("/admin/settle/trigger", a.handleTrigger)   // POST {settle_date, currency}
	mux.HandleFunc("/admin/settle/status", a.handleStatus)     // GET ?settle_date=...&run_id=...
	mux.HandleFunc("/admin/settle/resume", a.handleResume)     // POST {settle_date, run_id, threshold_seconds}

	if cfg.Token == "" {
		logger.Error("admin http: AUTH DISABLED — set ADMIN_HTTP_TOKEN env or admin.token in config for production")
	} else {
		logger.Info("admin http: token auth enabled")
	}

	const maxBody = 1 << 20 // 1 MB
	handler := http.MaxBytesHandler(a.tokenMiddleware(mux), maxBody)

	a.srv = &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       5 * time.Second,
	}
	return a
}

// Start 异步启动 admin HTTP。
func (a *AdminServer) Start() error {
	a.logger.Info("clearing-settlement admin listening", zap.String("addr", a.srv.Addr))
	go func() {
		if err := a.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			a.logger.Error("admin http error", zap.Error(err))
		}
	}()
	return nil
}

// Stop 优雅关停。
func (a *AdminServer) Stop(ctx context.Context) error {
	return a.srv.Shutdown(ctx)
}

func (a *AdminServer) tokenMiddleware(next http.Handler) http.Handler {
	if a.cfg.Token == "" {
		return next
	}
	expected := []byte(a.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/health", "/admin/health/readiness":
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(r.Header.Get("X-Admin-Token"))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized: missing or invalid X-Admin-Token"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleTrigger POST { "settle_date":"YYYY-MM-DD", "currency":"PHP" }
func (a *AdminServer) handleTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SettleDate string `json:"settle_date"`
		Currency   string `json:"currency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if req.Currency == "" {
		req.Currency = "PHP"
	}
	runID, err := a.svc.TriggerSettlement(r.Context(), req.SettleDate, req.Currency)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "settle_date": req.SettleDate, "currency": req.Currency})
}

// handleStatus GET ?settle_date=YYYY-MM-DD&run_id=N
func (a *AdminServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	settleDate := r.URL.Query().Get("settle_date")
	runID := 0
	if v := r.URL.Query().Get("run_id"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &runID)
	}
	st, err := a.svc.GetStatus(r.Context(), settleDate, runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleResume POST { "settle_date":"YYYY-MM-DD", "run_id":N, "threshold_seconds":300 }
func (a *AdminServer) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SettleDate       string `json:"settle_date"`
		RunID            int    `json:"run_id"`
		ThresholdSeconds int    `json:"threshold_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	resumed, err := a.svc.ResumeStuck(r.Context(), req.SettleDate, req.RunID, time.Duration(req.ThresholdSeconds)*time.Second)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resumed": resumed})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
