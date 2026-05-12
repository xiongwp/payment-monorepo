// Package adminhttp — data-rights HTTP API.
//
// 用户 (商户后台 / 终端 SDK):
//   POST /v1/requests                  — 提交 DSAR / RTBF
//   GET  /v1/requests/{id}             — 看自己的工单状态
//
// Admin (X-Admin-Token):
//   GET  /admin/requests               — list (filter by state / type / overdue)
//   POST /admin/requests/{id}/verify   — 验证身份
//   POST /admin/requests/{id}/approve  — 批准 (触发 fan-out)
//   POST /admin/requests/{id}/reject   — 拒绝
//   POST /admin/requests/{id}/fulfill  — 发邮件 + 标 fulfilled

package adminhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/data-rights/internal/audit"
	"reconcile-system/packages/data-rights/internal/domain"
	"reconcile-system/packages/data-rights/internal/metrics"
	"reconcile-system/packages/data-rights/internal/orchestrator"
	"reconcile-system/packages/data-rights/internal/store"
)

type Server struct {
	Store      store.Store
	Orch       *orchestrator.Orchestrator
	Audit      audit.Sink
	AdminToken string
	Log        *zap.Logger
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	// 用户面
	mux.HandleFunc("/v1/requests", s.handleSubmit)
	mux.HandleFunc("/v1/requests/", s.handleGetRequest)

	// admin
	mux.HandleFunc("/admin/requests", s.requireAdmin(s.handleListRequests))
	mux.HandleFunc("/admin/requests/", s.requireAdmin(s.handleAdminAction))
	return mux
}

// ── 用户面 ──

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	var body struct {
		Type         domain.RequestType  `json:"type"`
		Subject      domain.Subject      `json:"subject"`
		Jurisdiction domain.Jurisdiction `json:"jurisdiction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Subject.ID == "" || body.Subject.Type == "" {
		writeErr(w, http.StatusBadRequest, "bad_subject", "subject.id and subject.type required")
		return
	}
	if body.Type == "" {
		body.Type = domain.RequestAccess
	}
	if body.Jurisdiction == "" {
		body.Jurisdiction = guessJurisdiction(body.Subject.Country)
	}
	req := domain.Request{
		RequestID:    "dsar_" + randHex(16),
		Type:         body.Type,
		Subject:      body.Subject,
		Jurisdiction: body.Jurisdiction,
		State:        domain.StateReceived,
		SubmittedAt:  time.Now().UTC(),
		DeadlineAt:   time.Now().UTC().Add(30 * 24 * time.Hour),
	}
	if err := s.Store.SaveRequest(req); err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	metrics.RequestsTotal.WithLabelValues(string(req.Type), string(req.Jurisdiction)).Inc()
	_ = s.Audit.Emit(r.Context(), audit.Event{
		OccurredAt: req.SubmittedAt,
		Actor:      string(req.Subject.Type) + ":" + req.Subject.ID,
		Action:     "submit",
		RequestID:  req.RequestID,
		Details: map[string]interface{}{
			"type":         string(req.Type),
			"jurisdiction": string(req.Jurisdiction),
		},
		SourceIP: r.RemoteAddr,
	})
	writeJSON(w, http.StatusCreated, req)
}

func (s *Server) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/requests/")
	req, err := s.Store.GetRequest(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// ── admin ──

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.ListFilter{
		State:   domain.State(q.Get("state")),
		Type:    domain.RequestType(q.Get("type")),
		Overdue: q.Get("overdue") == "1",
		Limit:   atoiOr(q.Get("limit"), 50),
		Offset:  atoiOr(q.Get("offset"), 0),
	}
	list, err := s.Store.ListRequests(filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"requests": list,
		"count":    len(list),
	})
}

// /admin/requests/{id}/{action}
func (s *Server) handleAdminAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/requests/"), "/")
	if len(parts) != 2 {
		writeErr(w, http.StatusBadRequest, "bad_path", "expect /admin/requests/{id}/{action}")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	id := parts[0]
	action := parts[1]
	req, err := s.Store.GetRequest(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	var body struct {
		Reviewer string `json:"reviewer"`
		Reason   string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	switch action {
	case "verify":
		req.Verification.Method = "ops_manual"
		req.Verification.VerifiedAt = time.Now().UTC()
		req.Verification.VerifierID = body.Reviewer
		_ = s.Store.SaveRequest(req)
		_ = s.Store.UpdateState(id, domain.StateVerifying, body.Reviewer)
		s.emitAudit(r, body.Reviewer, "verify", id, nil)
	case "approve":
		_ = s.Store.UpdateState(id, domain.StateCollecting, body.Reviewer)
		s.emitAudit(r, body.Reviewer, "approve", id, nil)
		// 异步 fan-out
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if req.Type == domain.RequestErasure {
				_ = s.Orch.CollectErasure(ctx, req)
			} else {
				_ = s.Orch.CollectAccess(ctx, req)
			}
			_ = s.Store.UpdateState(id, domain.StateReview, "system")
		}()
	case "reject":
		if body.Reason == "" {
			writeErr(w, http.StatusBadRequest, "missing_reason", "reason required for reject")
			return
		}
		_ = s.Store.UpdateState(id, domain.StateRejected, body.Reason)
		s.emitAudit(r, body.Reviewer, "reject", id, map[string]interface{}{"reason": body.Reason})
	case "fulfill":
		// 真生产: 拼 zip + 加密 + S3 + 邮件发链接 → 这里 stub
		latest, _ := s.Store.GetRequest(id)
		combinedSHA := orchestrator.CombineExports(latest.ServiceStatuses)
		_ = s.Store.SetExport(id, "s3://exports/"+id+".zip", combinedSHA)
		_ = s.Store.UpdateState(id, domain.StateFulfilled, body.Reviewer)
		s.emitAudit(r, body.Reviewer, "fulfill", id, map[string]interface{}{
			"export_sha256": combinedSHA,
		})
	default:
		writeErr(w, http.StatusBadRequest, "bad_action", "verify|approve|reject|fulfill")
		return
	}

	final, _ := s.Store.GetRequest(id)
	writeJSON(w, http.StatusOK, final)
}

// ── helpers ──

func (s *Server) emitAudit(r *http.Request, actor, action, reqID string, details map[string]interface{}) {
	_ = s.Audit.Emit(r.Context(), audit.Event{
		OccurredAt: time.Now().UTC(),
		Actor:      actor,
		Action:     action,
		RequestID:  reqID,
		Details:    details,
		SourceIP:   r.RemoteAddr,
	})
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

func guessJurisdiction(country string) domain.Jurisdiction {
	// 简化映射 — 真实生产更细
	switch strings.ToUpper(country) {
	case "US", "CA": // California 用 US-CA 区分太烦, 这里都按 US 归 CCPA
		return domain.JurCA
	case "GB", "UK":
		return domain.JurUK
	case "BR":
		return domain.JurBR
	case "CN":
		return domain.JurCN
	case "DE", "FR", "IT", "ES", "NL", "BE", "PL", "SE", "DK", "FI", "AT", "IE":
		return domain.JurEU
	}
	return domain.JurOther
}

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
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func atoiOr(s string, def int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return def
	}
	return n
}
