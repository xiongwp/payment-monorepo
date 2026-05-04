package handler

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"time"
)

// Session/cookie-based auth + CSRF.
//
// Why this exists: the legacy flow stored the bearer token in
// localStorage('admin_token') and sent it in the Authorization header. Any
// XSS in the SPA (a malicious dependency, a 3rd-party script, an unescaped
// merchant name in a tooltip…) immediately exfiltrates the admin token. With
// 资金安全 actions on the table that's catastrophic.
//
// New flow:
//   - POST /api/auth/login   { username, password } → Set-Cookie admin_session
//                                                     (HttpOnly, Secure,
//                                                     SameSite=Strict)
//                                                   + Set-Cookie admin_csrf
//                                                     (NOT HttpOnly so the
//                                                     SPA can read it and
//                                                     echo it in
//                                                     X-CSRF-Token).
//   - POST /api/auth/logout  → both cookies cleared.
//   - All mutating /api/* requests are checked by CSRFMiddleware: the
//     X-CSRF-Token header MUST equal the admin_csrf cookie (double-submit
//     pattern). XSS still cannot mount this attack because httpOnly admin_
//     session can't be exfiltrated and a different-origin tab can't read the
//     cookie either.
//
// The actual user/password verification uses the same ADMIN_TOKENS table from
// helpers.go: each row is (token, user_id, role) where the token doubles as
// the password. This keeps the tokens.yaml model + simplifies the prod
// deployment story (one secret store).
//
// In future a JWT-backed session is straightforward: replace the cookie value
// with a signed JWT, validate signature in AuthMiddleware. The cookie
// indirection alone solves the XSS-exfil leg, which is the urgent risk.

// SessionHandler owns /auth/login and /auth/logout.
type SessionHandler struct{}

// NewSessionHandler constructs the handler.
func NewSessionHandler() *SessionHandler { return &SessionHandler{} }

// generateCSRFToken produces an unpredictable 32-byte hex string. Failures to
// read /dev/urandom are extraordinarily rare; we panic so we never serve a
// predictable token.
func generateCSRFToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read returning an error is essentially "OS broken"; we
		// must not silently fall back to a weaker generator.
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// cookieSecure returns true if cookies should be marked Secure. Always true in
// prod (HTTPS-only); in dev we leave it off so http://localhost works.
func cookieSecure() bool { return IsProdEnv() }

// Login POST /api/auth/login.
//
// Body: { token: "<bearer token>" } — for now we accept the same opaque token
// that AuthMiddleware honours, looked up in the ADMIN_TOKENS table. The
// response sets two cookies (session + csrf) and returns the principal.
func (h *SessionHandler) Login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Token = strings.TrimSpace(body.Token)
	if body.Token == "" {
		writeError(w, http.StatusBadRequest, "token required")
		return
	}
	rec, ok := staticTokenTable()[body.Token]
	if !ok {
		// also accept the legacy single token so existing scripts keep working
		legacy := os.Getenv("ADMIN_BEARER_TOKEN")
		if legacy == "" || body.Token != legacy {
			writeError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		rec = principalRecord{UserID: "legacy-token", Role: "admin"}
	}

	csrf := generateCSRFToken()
	// session cookie value = the actual bearer token. AuthMiddleware reads
	// either Authorization header or this cookie (see cookieToken below).
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    body.Token,
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure(),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   12 * 3600, // 12h working session
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_csrf",
		Value:    csrf,
		Path:     "/",
		HttpOnly: false, // SPA must read it to echo in X-CSRF-Token
		Secure:   cookieSecure(),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   12 * 3600,
	})
	writeJSON(w, map[string]any{
		"user_id": rec.UserID,
		"role":    rec.Role,
	})
}

// Logout POST /api/auth/logout — best-effort cookie clear.
func (h *SessionHandler) Logout(w http.ResponseWriter, _ *http.Request) {
	for _, name := range []string{"admin_session", "admin_csrf"} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			HttpOnly: name == "admin_session",
			Secure:   cookieSecure(),
			SameSite: http.SameSiteStrictMode,
			Expires:  time.Unix(0, 0),
			MaxAge:   -1,
		})
	}
	writeJSON(w, map[string]any{"ok": true})
}

// CSRFMiddleware enforces the double-submit cookie pattern on mutating
// requests authenticated by cookie. If the request didn't carry an
// admin_session cookie (i.e. it's still using the Authorization header), CSRF
// is skipped — the cookie attack vector doesn't apply.
func CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMutating(r.Method) || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		// Only enforce when the auth came from the cookie.
		sess, _ := r.Cookie("admin_session")
		if sess == nil || sess.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		csrfCookie, _ := r.Cookie("admin_csrf")
		csrfHeader := r.Header.Get("X-CSRF-Token")
		if csrfCookie == nil || csrfCookie.Value == "" || csrfHeader == "" || csrfCookie.Value != csrfHeader {
			writeError(w, http.StatusForbidden, "csrf token mismatch")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cookieToken returns the bearer token for the request — preferring the
// HttpOnly cookie when present, else the legacy Authorization header. Allows
// AuthMiddleware to support both flows during the migration window.
func cookieToken(r *http.Request) string {
	if c, _ := r.Cookie("admin_session"); c != nil && c.Value != "" {
		return c.Value
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}
