// Package adminhttp — vault HTTP API.
//
// 内部 (mTLS only, PCI 边界内):
//   POST /v1/tokens/exchange         — PAN → internal_token (PCI)
//   POST /v1/tokens/{token}/charge   — 生成 (network_token, cryptogram)
//   POST /v1/tokens/{token}/provision — 触发/重试 VTS/MDES provision
//   GET  /v1/tokens/{token}          — meta (last4 / brand / status, 不返 PAN)
//   POST /v1/tokens/{token}/suspend  — 冻结
//   DELETE /v1/tokens/{token}        — 软删
//
// Admin (内网, X-Admin-Token):
//   GET  /admin/health/providers     — provider 状态
//   GET  /admin/metrics/merchants/{id}/count

package adminhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/tokenization-vault/internal/audit"
	"reconcile-system/packages/tokenization-vault/internal/domain"
	"reconcile-system/packages/tokenization-vault/internal/metrics"
	"reconcile-system/packages/tokenization-vault/internal/store"
	"reconcile-system/packages/tokenization-vault/internal/vault"
)

type Server struct {
	Vault      *vault.Vault
	Store      store.Store
	Audit      audit.Sink
	AdminToken string
	Log        *zap.Logger
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/tokens/exchange", s.handleExchange)
	mux.HandleFunc("/v1/tokens/", s.handleTokenRoutes)
	mux.HandleFunc("/admin/", s.requireAdmin(s.handleAdmin))
	return mux
}

// ── /v1/tokens/exchange ──

func (s *Server) handleExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST")
		return
	}
	var req domain.ExchangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if req.MerchantID == "" || req.PAN == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "merchant_id and pan required")
		return
	}
	res, err := s.Vault.Exchange(r.Context(), req)
	if err != nil {
		switch err {
		case vault.ErrInvalidPAN:
			writeErr(w, http.StatusBadRequest, "invalid_pan", err.Error())
		default:
			s.Log.Error("exchange", zap.Error(err))
			writeErr(w, http.StatusInternalServerError, "exchange_failed", err.Error())
		}
		metrics.ExchangeTotal.WithLabelValues("error", "").Inc()
		return
	}
	result := "created"
	if res.Provisioned {
		result = "dedup"
	}
	metrics.ExchangeTotal.WithLabelValues(result, string(res.Brand)).Inc()
	_ = s.Audit.Emit(r.Context(), audit.Event{
		OccurredAt: time.Now().UTC(),
		Actor:      r.Header.Get("X-Client-Id"),
		Action:     "exchange",
		Token:      res.Token,
		Details: map[string]interface{}{
			"merchant_id": req.MerchantID,
			"brand":       string(res.Brand),
			"last4":       res.PANLast4,
			"result":      result,
		},
		SourceIP: r.RemoteAddr,
	})
	writeJSON(w, http.StatusOK, res)
}

// /v1/tokens/{token}[/charge|/provision|/suspend]
func (s *Server) handleTokenRoutes(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/tokens/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeErr(w, http.StatusBadRequest, "bad_path", "missing token")
		return
	}
	token := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		s.getMeta(w, r, token)
	case action == "" && r.Method == http.MethodDelete:
		s.deleteToken(w, r, token)
	case action == "charge" && r.Method == http.MethodPost:
		s.charge(w, r, token)
	case action == "provision" && r.Method == http.MethodPost:
		s.provision(w, r, token)
	case action == "suspend" && r.Method == http.MethodPost:
		s.suspend(w, r, token)
	default:
		writeErr(w, http.StatusBadRequest, "bad_path", "unknown action: "+action+" method "+r.Method)
	}
}

func (s *Server) getMeta(w http.ResponseWriter, r *http.Request, token string) {
	t, err := s.Store.Get(token)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	// 不返 PANHash 等敏感字段
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":       t.Token,
		"merchant_id": t.MerchantID,
		"pan_last4":   t.PANLast4,
		"bin":         t.BIN,
		"brand":       t.Brand,
		"exp_month":   t.ExpMonth,
		"exp_year":    t.ExpYear,
		"status":      t.Status,
		"provisioned": t.NetworkRef != nil,
		"provider": func() string {
			if t.NetworkRef != nil {
				return string(t.NetworkRef.Provider)
			}
			return ""
		}(),
		"created_at": t.Created_at,
	})
}

func (s *Server) charge(w http.ResponseWriter, r *http.Request, token string) {
	var req domain.ChargeIntentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	req.Token = token
	t0 := time.Now()
	res, err := s.Vault.ChargeIntent(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "charge_intent_failed", err.Error())
		return
	}
	rec := "false"
	if req.Recurring {
		rec = "true"
	}
	metrics.ChargeIntentTotal.WithLabelValues(string(res.Provider), rec).Inc()
	_ = s.Audit.Emit(r.Context(), audit.Event{
		OccurredAt: time.Now().UTC(),
		Actor:      r.Header.Get("X-Client-Id"),
		Action:     "charge_intent",
		Token:      token,
		Details: map[string]interface{}{
			"amount":     req.Amount,
			"currency":   req.Currency,
			"recurring":  req.Recurring,
			"intent_id":  req.IntentID,
			"latency_ms": time.Since(t0).Milliseconds(),
		},
		SourceIP: r.RemoteAddr,
	})
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) provision(w http.ResponseWriter, r *http.Request, token string) {
	t0 := time.Now()
	res, err := s.Vault.Provision(r.Context(), token)
	if err != nil {
		metrics.ProvisionTotal.WithLabelValues("", "error").Inc()
		writeErr(w, http.StatusInternalServerError, "provision_failed", err.Error())
		return
	}
	metrics.ProvisionTotal.WithLabelValues(string(res.Provider), "ok").Inc()
	metrics.ProvisionLatency.WithLabelValues(string(res.Provider)).Observe(time.Since(t0).Seconds())
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) suspend(w http.ResponseWriter, r *http.Request, token string) {
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.Vault.SuspendToken(r.Context(), token, body.Reason); err != nil {
		writeErr(w, http.StatusBadRequest, "suspend_failed", err.Error())
		return
	}
	metrics.TokenSuspendTotal.WithLabelValues(body.Reason).Inc()
	_ = s.Audit.Emit(r.Context(), audit.Event{
		OccurredAt: time.Now().UTC(),
		Actor:      r.Header.Get("X-Client-Id"),
		Action:     "suspend",
		Token:      token,
		Details:    map[string]interface{}{"reason": body.Reason},
		SourceIP:   r.RemoteAddr,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "suspended"})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request, token string) {
	if err := s.Vault.DeleteToken(r.Context(), token); err != nil {
		writeErr(w, http.StatusBadRequest, "delete_failed", err.Error())
		return
	}
	_ = s.Audit.Emit(r.Context(), audit.Event{
		OccurredAt: time.Now().UTC(),
		Actor:      r.Header.Get("X-Client-Id"),
		Action:     "delete",
		Token:      token,
		SourceIP:   r.RemoteAddr,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ── admin ──

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	switch {
	case path == "health/providers":
		// 简单版: 列已配置 provider 名 (生产 ping VTS/MDES 拿真状态)
		out := map[string]string{}
		for _, p := range []domain.TokenProvider{domain.ProviderVTS, domain.ProviderMDES, domain.ProviderInhouse} {
			out[string(p)] = "configured"
		}
		writeJSON(w, http.StatusOK, out)
	case strings.HasPrefix(path, "metrics/merchants/"):
		// /admin/metrics/merchants/{id}/count
		rest := strings.TrimPrefix(path, "metrics/merchants/")
		segs := strings.Split(rest, "/")
		if len(segs) < 2 || segs[1] != "count" {
			writeErr(w, http.StatusBadRequest, "bad_path", "")
			return
		}
		mid := segs[0]
		n, err := s.Store.CountByMerchant(mid)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"merchant_id": mid, "active_token_count": n})
	default:
		writeErr(w, http.StatusNotFound, "no_route", path)
	}
}

func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AdminToken == "" {
			h(w, r)
			return
		}
		if r.Header.Get("X-Admin-Token") != s.AdminToken {
			writeErr(w, http.StatusForbidden, "forbidden", "admin token required")
			return
		}
		h(w, r)
	}
}

// ── helpers ──

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]string{"error": errCode, "message": msg})
}

var _ = context.Background
