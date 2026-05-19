package handler

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	orderv1 "reconcile-system/packages/order-core/kitex_gen/order/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

// AuditHandler exposes the audit log list to the admin UI.
type AuditHandler struct{ deps clients.Deps }

// NewAuditHandler constructs the handler.
func NewAuditHandler(d clients.Deps) *AuditHandler { return &AuditHandler{deps: d} }

// List GET /api/audit — filterable server-side list of admin actions.
func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	var sinceMs, untilMs int64
	if s := q.Get("since_ms"); s != "" {
		sinceMs, _ = strconv.ParseInt(s, 10, 64)
	}
	if s := q.Get("until_ms"); s != "" {
		untilMs, _ = strconv.ParseInt(s, 10, 64)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.Audit.List(ctx, &orderv1.ListAuditRequest{
		Actor:      q.Get("actor"),
		Action:     q.Get("action"),
		TargetType: q.Get("target_type"),
		TargetId:   q.Get("target_id"),
		SinceMs:    sinceMs,
		UntilMs:    untilMs,
		Limit:      int32(limit),
		Offset:     int32(offset),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"items": resp.GetItems(), "total": resp.GetTotal()})
}

// highSensitivityActions are dotted action ids whose audit MUST be written
// **synchronously** before the response is returned. If the audit store is
// unavailable, the BFF returns 503 and **rolls back** the mutation as far as
// it can (we cannot un-do a downstream gRPC call, but we can stop returning
// success to the operator so they don't trust an action that lacks a
// non-repudiable trail). For these actions a missing audit row is a
// regulatory incident; risk of a few extra latency ms is the trade-off.
//
// Everything not in this set is fire-and-forget (the legacy behaviour).
var highSensitivityActions = map[string]struct{}{
	"disputes.post":          {}, // concede / simulate / submit-evidence (path under /disputes/...)
	"merchants.post":         {}, // create
	"merchant.create":        {},
	"merchant.kyc.approve":   {},
	"merchant.kyc.reject":    {},
	"merchant.kyc.terminate": {},
	"merchant.kyc.suspend":   {},
	"merchant.rotate_key":    {},
	"refunds.post":           {},
	"refund.manual":          {},
}

// isHighSensitivity returns whether the (resolved) action requires sync audit.
// We also probe the URL path manually so we catch the simulate/concede/evidence
// sub-paths even before they're folded into a coarser top-level action name by
// actionFromPath.
func isHighSensitivity(action, path string) bool {
	if _, ok := highSensitivityActions[action]; ok {
		return true
	}
	if strings.Contains(path, "/disputes/") {
		// /disputes/{pi}/{id}/{simulate|concede|evidence|cancel}
		if strings.HasSuffix(path, "/simulate") ||
			strings.HasSuffix(path, "/concede") ||
			strings.HasSuffix(path, "/evidence") {
			return true
		}
	}
	if strings.HasSuffix(path, "/rotate-key") {
		return true
	}
	return false
}

// AuditMiddleware records every mutating admin request (POST / PATCH / DELETE /
// PUT) into order-core's admin_audit_log table via gRPC. Reads pass through
// untouched.
//
// Two write modes co-exist:
//
//   - High-sensitivity actions (concede, kyc.approve/terminate, rotate-key,
//     refund.manual, ...) → SYNCHRONOUS. Failure to record returns 503 to the
//     caller; the upstream gRPC mutation already happened, but the operator is
//     told the action is "unknown result" so they don't accidentally repeat it
//     thinking it failed.
//   - Everything else → fire-and-forget goroutine, logged on error. Avoids
//     coupling typical admin GET/PATCH latency to the audit store.
//
// Actor comes from AuthMiddleware (token-derived) via actorFromRequest, no
// longer from a client-supplied X-Admin-User header.
func AuditMiddleware(audit orderauditservice.Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if audit == nil || !isMutating(r.Method) || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()

			// Capture request body so we can ship a (redacted) copy to audit.
			var reqBody []byte
			if r.Body != nil {
				buf, _ := io.ReadAll(io.LimitReader(r.Body, 16*1024))
				reqBody = buf
				r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
			}

			action := actionFromPath(r.Method, r.URL.Path)
			highSens := isHighSensitivity(action, r.URL.Path)

			if highSens {
				// SYNC PATH: buffer the response and only flush after the audit
				// row is persisted. If audit fails we return 503 to the caller
				// instead of leaking a confirmed-but-unaudited admin action.
				bw := &bufferedResponseWriter{header: http.Header{}, body: bytes.NewBuffer(nil), status: http.StatusOK}
				next.ServeHTTP(bw, r)

				// 4xx upstream errors: still write audit (operator may want to
				// see the rejection) but never block on success codes only.
				redacted := redactSecrets(reqBody)
				entry := &orderv1.WriteAuditRequest{
					Actor:       actorFromRequest(r),
					ActorIp:     clientIP(r),
					Action:      action,
					TargetType:  targetTypeFromPath(r.URL.Path),
					TargetId:    targetIDFromPath(r.URL.Path),
					HttpMethod:  r.Method,
					HttpPath:    r.URL.Path,
					HttpStatus:  int32(bw.status),
					RequestBody: string(redacted),
					DurationMs:  int32(time.Since(start).Milliseconds()),
				}
				if bw.status >= 400 {
					trimmed := strings.TrimSpace(bw.body.String())
					if len(trimmed) > 512 {
						trimmed = trimmed[:512]
					}
					entry.ResponseMsg = trimmed
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := audit.Write(ctx, entry); err != nil {
					log.Printf("audit write FAILED (sync, blocking response): %v (action=%s path=%s)", err, entry.Action, entry.HttpPath)
					// Replace whatever the handler said with 503 — operator
					// MUST not believe the action succeeded if audit didn't
					// land. The downstream mutation (e.g. concede) may already
					// have happened; the UI will surface "unknown result".
					writeError(w, http.StatusServiceUnavailable, "audit write failed; refusing to commit response")
					return
				}
				// Audit ok — flush buffered response.
				bw.flushTo(w)
				return
			}

			// LEGACY PATH: fire and forget; non-critical mutations.
			lrw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK, body: bytes.NewBuffer(nil)}
			next.ServeHTTP(lrw, r)

			redacted := redactSecrets(reqBody)
			entry := &orderv1.WriteAuditRequest{
				Actor:       actorFromRequest(r),
				ActorIp:     clientIP(r),
				Action:      action,
				TargetType:  targetTypeFromPath(r.URL.Path),
				TargetId:    targetIDFromPath(r.URL.Path),
				HttpMethod:  r.Method,
				HttpPath:    r.URL.Path,
				HttpStatus:  int32(lrw.status),
				RequestBody: string(redacted),
				DurationMs:  int32(time.Since(start).Milliseconds()),
			}
			if lrw.status >= 400 {
				entry.ResponseMsg = strings.TrimSpace(lrw.body.String())
				if len(entry.ResponseMsg) > 512 {
					entry.ResponseMsg = entry.ResponseMsg[:512]
				}
			}
			go func(e *orderv1.WriteAuditRequest) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if _, err := audit.Write(ctx, e); err != nil {
					log.Printf("audit write failed: %v (action=%s path=%s)", err, e.Action, e.HttpPath)
				}
			}(entry)
		})
	}
}

// bufferedResponseWriter is a minimal http.ResponseWriter that captures the
// status, headers, and body so AuditMiddleware can decide post-hoc whether to
// flush them (audit ok) or replace them with a 503 (audit failed).
type bufferedResponseWriter struct {
	header http.Header
	body   *bytes.Buffer
	status int
	// wroteHeader is sticky — net/http calls WriteHeader at most once per
	// response.
	wroteHeader bool
}

func (b *bufferedResponseWriter) Header() http.Header { return b.header }
func (b *bufferedResponseWriter) WriteHeader(code int) {
	if b.wroteHeader {
		return
	}
	b.status = code
	b.wroteHeader = true
}
func (b *bufferedResponseWriter) Write(p []byte) (int, error) {
	if !b.wroteHeader {
		b.WriteHeader(http.StatusOK)
	}
	return b.body.Write(p)
}

// flushTo replays the buffered response into the real ResponseWriter, in the
// same order the inner handler emitted it.
func (b *bufferedResponseWriter) flushTo(w http.ResponseWriter) {
	for k, vs := range b.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// actionFromPath returns a dotted taxonomy like "merchant.approve" derived
// from the HTTP method + path. Unknown paths become "http.<method>".
func actionFromPath(method, path string) string {
	parts := splitPath(path)
	// Drop leading "api"
	if len(parts) > 0 && parts[0] == "api" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return "http." + strings.ToLower(method)
	}
	resource := parts[0] // merchants / orders / channels / kms / ops / audit

	// Specialize known mutations with a verb.
	if resource == "merchants" {
		// /merchants → create; /merchants/{id} → update;
		// /merchants/{id}/kyc/{action} → merchant.kyc.<action>;
		// /merchants/{id}/rotate-key → merchant.rotate_key
		if len(parts) == 1 {
			return "merchant.create"
		}
		if len(parts) >= 4 && parts[2] == "kyc" {
			return "merchant.kyc." + parts[3]
		}
		if len(parts) >= 3 && parts[2] == "rotate-key" {
			return "merchant.rotate_key"
		}
		if len(parts) >= 3 && parts[2] == "documents" {
			return "merchant.document.add"
		}
		if len(parts) >= 4 && parts[2] == "documents" && parts[3] != "" {
			return "merchant.document.review"
		}
		if len(parts) == 2 {
			return "merchant.update"
		}
	}
	if resource == "channels" {
		if len(parts) >= 2 {
			return "channel." + strings.Join(parts[1:], ".")
		}
	}
	return resource + "." + strings.ToLower(method)
}

func targetTypeFromPath(path string) string {
	parts := splitPath(path)
	if len(parts) > 0 && parts[0] == "api" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return ""
	}
	// merchants → merchant, orders → order, channels → channel
	res := parts[0]
	if strings.HasSuffix(res, "s") {
		return strings.TrimSuffix(res, "s")
	}
	return res
}

func targetIDFromPath(path string) string {
	parts := splitPath(path)
	if len(parts) > 0 && parts[0] == "api" {
		parts = parts[1:]
	}
	// /merchants/{id}/... → id at index 1
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

func splitPath(p string) []string {
	out := make([]string, 0, 6)
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	return r.RemoteAddr
}

// actorFromRequest returns the audit actor for r. Source of truth is the
// request context (populated by AuthMiddleware from the validated bearer
// token). The legacy X-Admin-User header is **ignored** — letting clients name
// themselves freely defeated audit non-repudiation since any browser tab could
// claim to be a different operator.
func actorFromRequest(r *http.Request) string {
	if v := r.Context().Value(ctxKeyActor); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return "unknown"
}

// ctxKey for carrying the actor identity forward from auth middleware.
type ctxKey int

const (
	ctxKeyActor ctxKey = iota
	// ctxKeyRole carries the principal's RBAC role (admin / operator /
	// viewer) — used by RequireRoleMiddleware to decide allow/deny.
	ctxKeyRole
)

// redactSecrets replaces well-known sensitive JSON field values with a
// placeholder so the audit log doesn't store plaintext keys/secrets.
// Intentionally simple (substring replace); for production add a proper
// JSON-aware redactor. Keys listed are what the BFF endpoints pass through.
func redactSecrets(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	out := string(b)
	for _, pat := range []string{
		`"live_secret_key"`, `"test_secret_key"`, `"webhook_secret"`,
		`"plaintext"`, `"ciphertext"`, `"secret_key"`, `"api_key"`, `"password"`,
	} {
		// Replace "field":"anything" → "field":"<redacted>"
		start := 0
		for {
			idx := strings.Index(out[start:], pat)
			if idx < 0 {
				break
			}
			pos := start + idx
			colon := strings.IndexByte(out[pos:], ':')
			if colon < 0 {
				break
			}
			valStart := pos + colon + 1
			// Skip whitespace then expect quote
			for valStart < len(out) && (out[valStart] == ' ' || out[valStart] == '\t') {
				valStart++
			}
			if valStart >= len(out) || out[valStart] != '"' {
				start = pos + len(pat)
				continue
			}
			valEnd := valStart + 1
			for valEnd < len(out) && out[valEnd] != '"' {
				if out[valEnd] == '\\' {
					valEnd++
				}
				valEnd++
			}
			if valEnd >= len(out) {
				break
			}
			out = out[:valStart] + `"<redacted>"` + out[valEnd+1:]
			start = valStart + len(`"<redacted>"`)
		}
	}
	return []byte(out)
}
