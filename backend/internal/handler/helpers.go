// Package handler HTTP handlers for the admin BFF. Response shape matches
// accounting-admin-web 的惯例：`{code, message, data}`.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type apiResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(apiResponse{Code: 0, Message: "ok", Data: data})
}

func writeError(w http.ResponseWriter, httpCode int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpCode)
	_ = json.NewEncoder(w).Encode(apiResponse{Code: httpCode, Message: msg})
}

// writeProxyResult 把上游原始 JSON 字节包成 admin BFF 标准 {code, message,
// data} 形式：
//   - 2xx: 解析 upstream JSON → writeJSON(parsed)，确保 code=0 让前端
//     axios 拦截器不报 "API error"
//   - >= 400: 解析 upstream {"error":"..."} 当 message → writeError
//
// 之前 proxyPostThrough 类直接 w.Write(body) 把原始 upstream JSON 写出去，
// 前端 client.ts 拦截器看到 code=undefined 即把所有响应当失败 ("API error")，
// 即便 status 是 200。本 helper 修这个 bug。
func writeProxyResult(w http.ResponseWriter, body []byte, status int) {
	if status >= 400 {
		// 上游错误 body 形如 {"error":"..."} 或纯文本。优先解 JSON。
		var errResp struct {
			Error string `json:"error"`
		}
		msg := string(body)
		if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error != "" {
			msg = errResp.Error
		}
		writeError(w, status, msg)
		return
	}
	// 2xx: 解析为通用 any 结构再走 writeJSON
	var data interface{}
	if len(body) == 0 {
		writeJSON(w, nil)
		return
	}
	if err := json.Unmarshal(body, &data); err != nil {
		// 上游返了非 JSON（极少；通常是文本回包）—— 直接当 string data
		writeJSON(w, string(body))
		return
	}
	writeJSON(w, data)
}

func readJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// allowedOrigins is the comma-separated whitelist consumed by CORSMiddleware.
// In prod this MUST be non-empty; the boot path (cmd/server/main.go) calls
// MustValidateCORSConfig to fatal-exit when it isn't.
//
// Reading the env var lazily (per-request) lets test harnesses tweak the value
// without restarting the server. Cost is negligible (~1 string copy / req).
func allowedOrigins() []string {
	raw := os.Getenv("ADMIN_CORS_ALLOWED_ORIGINS")
	if raw == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, p := range strings.Split(raw, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// MustValidateCORSConfig fails fast if APP_ENV=prod but no CORS whitelist was
// supplied. Wide-open CORS in prod lets any third-party page replay an
// admin's bearer token to /api/* — unacceptable for an admin that can move
// money.
func MustValidateCORSConfig() {
	if !IsProdEnv() {
		return
	}
	if len(allowedOrigins()) == 0 {
		log.Fatalf("APP_ENV=prod requires ADMIN_CORS_ALLOWED_ORIGINS to be a non-empty whitelist (comma-separated)")
	}
}

// CORSMiddleware enforces a configured Origin whitelist (env:
// ADMIN_CORS_ALLOWED_ORIGINS, comma-separated). When the request Origin is in
// the list it is echoed back; otherwise no CORS headers are emitted (the
// browser will block the response). In dev/empty-config mode the middleware
// transparently opens up to "*" so vite proxy still works.
//
// Credentials: if cookie auth is enabled (CSRF flow), Allow-Credentials is set
// to "true" and "*" is rejected by the spec — the whitelist must list literal
// origins.
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		whitelist := allowedOrigins()
		switch {
		case len(whitelist) == 0 && !IsProdEnv():
			// Local dev: open up so vite (5173) and nginx-proxied (3100) both work.
			w.Header().Set("Access-Control-Allow-Origin", "*")
		case origin != "" && containsOrigin(whitelist, origin):
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		default:
			// no header → browser blocks. Still serve the request so we have a
			// healthcheck path; the response body just won't be readable cross-
			// origin.
		}
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-CSRF-Token, X-Idempotency-Key")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func containsOrigin(list []string, want string) bool {
	for _, o := range list {
		if o == want {
			return true
		}
	}
	return false
}

// LoggingMiddleware 在每次 HTTP 请求进出打印 method + path + 完整 body + 状态码 +
// 耗时。Body 体积截到 4 KB 防止 webhook 类大请求把日志冲爆。
const maxLoggedBodyBytes = 4 << 10 // 4 KiB

// loggingResponseWriter 包装 http.ResponseWriter 以拦截 status 和 body
type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	body   *bytes.Buffer
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *loggingResponseWriter) Write(b []byte) (int, error) {
	// 只采样 maxLoggedBodyBytes 进 buffer，其它直接透传
	if w.body.Len() < maxLoggedBodyBytes {
		room := maxLoggedBodyBytes - w.body.Len()
		if room > len(b) {
			room = len(b)
		}
		w.body.Write(b[:room])
	}
	return w.ResponseWriter.Write(b)
}

func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()

		// 读 request body 并 putback（handler 还要读）
		var reqBody []byte
		if r.Body != nil {
			lr := io.LimitReader(r.Body, maxLoggedBodyBytes+1)
			reqBody, _ = io.ReadAll(lr)
			// rest body 拼回去（剩余部分继续可读）
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(reqBody), r.Body))
		}
		log.Printf("HTTP IN  %s %s body=%s",
			r.Method, r.URL.RequestURI(), truncate(reqBody))

		lrw := &loggingResponseWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
			body:           bytes.NewBuffer(nil),
		}
		next.ServeHTTP(lrw, r)

		log.Printf("HTTP OUT %s %s status=%d dur=%s body=%s",
			r.Method, r.URL.RequestURI(), lrw.status, time.Since(start), truncate(lrw.body.Bytes()))
	})
}

func truncate(b []byte) string {
	if len(b) > maxLoggedBodyBytes {
		return string(b[:maxLoggedBodyBytes]) + "...(truncated)"
	}
	return string(b)
}

// principalRecord describes one authenticated admin: their user-id (used as
// the audit actor) plus their RBAC role. Loaded from the optional tokens
// config, keyed by token string. Multiple tokens per user are supported so
// rotation can happen without downtime.
type principalRecord struct {
	UserID string
	Role   string
}

// staticTokenTable is the parsed ADMIN_TOKENS env var. Format:
//
//	"<token1>:<user_id1>:<role1>,<token2>:<user_id2>:<role2>"
//
// Empty (or invalid lines) silently skipped; an empty table means: fall back
// to the legacy single-token mode (ADMIN_BEARER_TOKEN) where all callers map
// to a synthetic principal with role=admin.
func staticTokenTable() map[string]principalRecord {
	raw := os.Getenv("ADMIN_TOKENS")
	if raw == "" {
		return nil
	}
	out := map[string]principalRecord{}
	for _, ln := range strings.Split(raw, ",") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		parts := strings.SplitN(ln, ":", 3)
		if len(parts) < 2 {
			continue
		}
		role := "viewer"
		if len(parts) == 3 {
			role = parts[2]
		}
		out[parts[0]] = principalRecord{UserID: parts[1], Role: role}
	}
	return out
}

// AuthMiddleware verifies the request bearer token and decodes a principal
// (user-id + role) into the request context for downstream handlers (audit,
// RBAC). Behaviour:
//
//   - APP_ENV=prod with empty legacy token AND empty ADMIN_TOKENS → fatal at
//     boot (caller must check via MustValidateAuthConfig).
//   - non-prod with empty token → middleware passes through but tags actor as
//     "dev-anon".
//   - tokens table populated → request token is looked up, principal injected.
//   - legacy single token → role assumed admin, user-id derived from the
//     token's first 8 sha-like chars (kept for backward compat — see
//     fingerprint comment in audit.go).
func AuthMiddleware(legacyToken string) func(http.Handler) http.Handler {
	table := staticTokenTable()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			// Empty config in non-prod = open mode (dev-friendly). Inject a
			// throwaway principal so AuditMiddleware still has an actor.
			if legacyToken == "" && len(table) == 0 {
				ctx := context.WithValue(r.Context(), ctxKeyActor, "dev-anon")
				ctx = context.WithValue(ctx, ctxKeyRole, "admin")
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			tok := cookieToken(r)
			if tok == "" {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			// Prefer the structured tokens table; legacy token is the fallback.
			if rec, ok := table[tok]; ok {
				ctx := context.WithValue(r.Context(), ctxKeyActor, rec.UserID)
				ctx = context.WithValue(ctx, ctxKeyRole, rec.Role)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if legacyToken != "" && tok == legacyToken {
				// Legacy single-token mode: derive a stable actor id from the
				// token fingerprint so audit rows are still attributable. Role
				// assumed admin (the legacy token unlocked everything before).
				ctx := context.WithValue(r.Context(), ctxKeyActor, "legacy-token")
				ctx = context.WithValue(ctx, ctxKeyRole, "admin")
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			writeError(w, http.StatusUnauthorized, "unauthorized")
		})
	}
}

// MustValidateAuthConfig is called by main.go before starting the server; in
// prod a missing token configuration is a fatal misconfiguration. We refuse to
// boot with auth disabled in prod even if someone "forgets" to set the env.
func MustValidateAuthConfig(legacyToken string) {
	if !IsProdEnv() {
		return
	}
	if legacyToken == "" && len(staticTokenTable()) == 0 {
		log.Fatalf("APP_ENV=prod requires ADMIN_BEARER_TOKEN or ADMIN_TOKENS to be configured")
	}
}

// ─── RBAC ──────────────────────────────────────────────────────────────────
//
// roleAllows maps role → permitted actions. Actions are the dotted action
// taxonomy emitted by actionFromPath in audit.go (see also
// highSensitivityActions). A request whose action is in this set is allowed
// for that role; everything else gets 403.
//
// "admin"   — full access (all mutating actions + read).
// "operator" — read + most mutations except destructive money movers.
// "viewer"  — read-only.
var roleAllows = map[string]map[string]struct{}{
	"admin": nil, // nil = wildcard
	"operator": toSet(
		"merchant.update", "merchant.document.add", "merchant.document.review",
		"merchant.kyc.submit", "merchant.kyc.review", "merchant.kyc.request_more_info",
		"webhooks.post", "channels.post",
		"app.post",
	),
	"viewer": toSet(),
}

func toSet(items ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, s := range items {
		out[s] = struct{}{}
	}
	return out
}

// RequireRoleMiddleware enforces the action allow-list for the principal's
// role. It piggybacks on actionFromPath / actorFromRequest for naming.
//
// GETs are always allowed (read-only). For mutations:
//   - admin: pass through
//   - operator/viewer: action must be in roleAllows[role]
//   - missing role: treat as viewer
func RequireRoleMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMutating(r.Method) || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		role, _ := r.Context().Value(ctxKeyRole).(string)
		if role == "" {
			role = "viewer"
		}
		allow, ok := roleAllows[role]
		if !ok {
			writeError(w, http.StatusForbidden, "role not authorised")
			return
		}
		// admin: nil set means wildcard.
		if allow == nil {
			next.ServeHTTP(w, r)
			return
		}
		action := actionFromPath(r.Method, r.URL.Path)
		if _, ok := allow[action]; !ok {
			writeError(w, http.StatusForbidden, "role "+role+" cannot perform "+action)
			return
		}
		next.ServeHTTP(w, r)
	})
}
