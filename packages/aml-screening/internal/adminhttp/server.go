// Package adminhttp — AML HTTP API.
//
// 公共 (服务间, 走 oauth2 bearer):
//   POST /v1/screen                          — 同步筛查 (KYB / payout / high-value tx)
//   GET  /v1/screen/{request_id}             — 查询历史结果
//
// Admin (内网, 走 X-Admin-Token):
//   GET  /admin/hits/pending                 — 待复核命中队列
//   POST /admin/hits/{hit_id}/resolve        — ops 复核决策
//   GET  /admin/lists/{source}/count         — 名单条目数
//   POST /admin/lists/{source}/refresh       — 立即刷一次
//
// 不带 admin token 的 /v1/* 由 oauth2 server 颁的 client_credentials JWT 鉴权.

package adminhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/aml-screening/internal/audit"
	"reconcile-system/packages/aml-screening/internal/domain"
	"reconcile-system/packages/aml-screening/internal/metrics"
	"reconcile-system/packages/aml-screening/internal/screening"
	"reconcile-system/packages/aml-screening/internal/sources"
	"reconcile-system/packages/aml-screening/internal/store"
)

type Server struct {
	Store      store.Store
	Cfg        screening.Config
	Audit      audit.Sink
	AdminToken string
	Log        *zap.Logger
	// 用 source name → refresher 注册表, 给 /admin/lists/{source}/refresh 调用.
	Refreshers map[domain.ListSource]*sources.Refresher
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/screen", s.handleScreen)
	mux.HandleFunc("/v1/screen/", s.handleScreenGet)
	mux.HandleFunc("/admin/hits/pending", s.requireAdmin(s.handleHitsPending))
	mux.HandleFunc("/admin/hits/", s.requireAdmin(s.handleHitResolve))
	mux.HandleFunc("/admin/lists/", s.requireAdmin(s.handleLists))
	return mux
}

// ───────────────── /v1/screen ─────────────────

func (s *Server) handleScreen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST")
		return
	}
	var req domain.ScreenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name required")
		return
	}
	if req.RequestID == "" {
		req.RequestID = "req_" + randHex(12)
	}
	if req.Subject == "" {
		req.Subject = domain.SubjectIndividual
	}

	t0 := time.Now()
	// idempotency — 1h 内同 request_id 直接返已有结果
	if prev, err := s.Store.GetResult(req.RequestID); err == nil {
		writeJSON(w, http.StatusOK, prev)
		return
	}

	// 取候选 — first-letter prefix
	prefix := ""
	if req.Name != "" {
		prefix = strings.ToLower(string([]rune(req.Name)[0]))
	}
	allSources := domain.AllExternalSources
	allSources = append(allSources, domain.SourceInternalBlock)
	candidates, err := s.Store.Candidates(prefix, allSources, screening.NormalizeCountry(req.Nationality))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}

	hits := screening.Match(req, candidates, s.Cfg)
	// 给每个 hit 分 hit_id
	for i := range hits {
		hits[i].HitID = "hit_" + randHex(10)
	}
	action, top := screening.Decide(hits, s.Cfg)
	res := domain.ScreenResult{
		RequestID:  req.RequestID,
		Action:     action,
		HighestHit: top,
		Hits:       hits,
		ScreenedAt: time.Now().UTC(),
		Sources:    allSources,
		Latency_ms: time.Since(t0).Milliseconds(),
	}
	_ = s.Store.SaveResult(res, req)

	// metrics
	metrics.ScreenTotal.WithLabelValues(string(action), req.Trigger).Inc()
	metrics.ScreenLatency.WithLabelValues(req.Trigger).Observe(time.Since(t0).Seconds())
	for _, h := range hits {
		metrics.HitsByAction.WithLabelValues(string(h.ListEntry.Source)).Inc()
	}

	// audit
	_ = s.Audit.Emit(r.Context(), audit.Event{
		OccurredAt: res.ScreenedAt,
		Actor:      clientFromCtx(r),
		Action:     "screen",
		Subject:    req.MerchantID,
		Details: map[string]interface{}{
			"trigger":    req.Trigger,
			"action":     string(action),
			"hit_count":  len(hits),
			"top_score":  top,
			"request_id": req.RequestID,
		},
		SourceIP: r.RemoteAddr,
	})

	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleScreenGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/screen/")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "missing request_id")
		return
	}
	res, err := s.Store.GetResult(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ───────────────── admin /admin/hits ─────────────────

func (s *Server) handleHitsPending(w http.ResponseWriter, r *http.Request) {
	limit := 50
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	hits, err := s.Store.ListPendingHits(limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"hits":   hits,
		"limit":  limit,
		"offset": offset,
	})
}

// /admin/hits/{hit_id}/resolve
func (s *Server) handleHitResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST")
		return
	}
	// 路径: /admin/hits/{hit_id}/resolve
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/hits/"), "/")
	if len(parts) != 2 || parts[1] != "resolve" {
		writeErr(w, http.StatusBadRequest, "bad_path", "expect /admin/hits/{hit_id}/resolve")
		return
	}
	hitID := parts[0]
	var dec domain.HitResolution
	if err := json.NewDecoder(r.Body).Decode(&dec); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	dec.HitID = hitID
	dec.ResolvedAt = time.Now().UTC()
	switch dec.Decision {
	case domain.HitCleared, domain.HitFrozen, domain.HitEscalated:
	default:
		writeErr(w, http.StatusBadRequest, "bad_decision", "must be cleared|frozen|escalated")
		return
	}
	if err := s.Store.UpdateHitState(hitID, dec); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	_ = s.Audit.Emit(r.Context(), audit.Event{
		OccurredAt: dec.ResolvedAt,
		Actor:      dec.Reviewer,
		Action:     "hit_resolve",
		Subject:    hitID,
		Details: map[string]interface{}{
			"decision": string(dec.Decision),
			"reason":   dec.Reason,
		},
		SourceIP: r.RemoteAddr,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ───────────────── admin /admin/lists ─────────────────

func (s *Server) handleLists(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/lists/"), "/")
	if len(parts) < 2 {
		writeErr(w, http.StatusBadRequest, "bad_path", "expect /admin/lists/{source}/(count|refresh)")
		return
	}
	src := domain.ListSource(parts[0])
	action := parts[1]
	switch action {
	case "count":
		n, err := s.Store.EntryCount(src)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		metrics.ListEntries.WithLabelValues(string(src)).Set(float64(n))
		writeJSON(w, http.StatusOK, map[string]interface{}{"source": src, "count": n})
	case "refresh":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST")
			return
		}
		ref, ok := s.Refreshers[src]
		if !ok {
			writeErr(w, http.StatusNotFound, "no_refresher", "no refresher for "+string(src))
			return
		}
		t0 := time.Now()
		n, err := ref.RefreshOnce()
		if err != nil {
			metrics.RefreshSuccess.WithLabelValues(string(src), "error").Inc()
			writeErr(w, http.StatusInternalServerError, "refresh_failed", err.Error())
			return
		}
		metrics.RefreshSuccess.WithLabelValues(string(src), "ok").Inc()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"source":     src,
			"refreshed":  n,
			"duration":   time.Since(t0).Milliseconds(),
		})
	default:
		writeErr(w, http.StatusBadRequest, "bad_action", "expect count|refresh")
	}
}

// ───────────────── middleware ─────────────────

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

// ───────────────── helpers ─────────────────

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]string{"error": errCode, "message": msg})
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// 从 ctx 取 client_id (oauth2 中间件注入); 没有就空
func clientFromCtx(r *http.Request) string {
	if v := r.Header.Get("X-Client-Id"); v != "" {
		return v
	}
	return ""
}

// 兜底
var _ = errors.New
var _ = context.Background
