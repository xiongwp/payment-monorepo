// handler.go: HTTP handlers for /signup /login /verify-otp /me /logout。
//
// 设计要点：
//   - 用 httpOnly + secure cookie 持久化 JWT；前端不直接拿 token
//   - 风控参数（ip / device_id / user_agent / risk_session_id / fp）从 request
//     header / cookie 自动提取，业务侧不需要手填
//   - 错误用 flash message 展示在表单上方，不暴露内部错误
package userweb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"

	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/status"
)

// CookieName JWT cookie 名。
const CookieName = "uauth"

// Handler holds 模板 + UserService gRPC client。
type Handler struct {
	// pages 每页一棵独立模板树（layout + 该 page）；别名 see loadTemplates。
	pages  map[string]*template.Template
	uc     usermerchantv1.UserServiceClient
	logger *zap.Logger
	// CookieDomain / CookieSecure 由 main.go 按部署环境配置。
	CookieDomain string
	CookieSecure bool
}

// NewHandler 装配模板 + gRPC client。
func NewHandler(uc usermerchantv1.UserServiceClient, logger *zap.Logger) (*Handler, error) {
	pages, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	return &Handler{pages: pages, uc: uc, logger: logger}, nil
}

// Register 把所有路由挂到 mux。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/signup", h.handleSignup)
	mux.HandleFunc("/login", h.handleLogin)
	mux.HandleFunc("/verify-otp", h.handleVerifyOTP)
	mux.HandleFunc("/logout", h.handleLogout)
	mux.HandleFunc("/me", h.handleMe)
	mux.HandleFunc("/", h.handleRoot)
}

func (h *Handler) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if _, ok := readJWTCookie(r); ok {
		http.Redirect(w, r, "/me", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ── /signup ─────────────────────────────────────────────────────────

func (h *Handler) handleSignup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.render(w, "signup.html", map[string]any{"Title": "Sign up"})
	case http.MethodPost:
		_ = r.ParseForm()
		req := &usermerchantv1.RegisterRequest{
			Username:        r.FormValue("login_id"),
			Password:        r.FormValue("password"),
			Email:           strings.ToLower(strings.TrimSpace(r.FormValue("email"))),
			Phone:           strings.TrimSpace(r.FormValue("phone")),
			IpAddress:       clientIP(r),
			DeviceId:        h.deviceID(r),
			UserAgent:       r.UserAgent(),
			RiskSessionId:   r.FormValue("risk_session_id"),
			FingerprintHash: cookieValue(r, "fp"),
			UaHash:          uaHashOf(r.UserAgent()),
			Metadata: map[string]string{
				"channel":      "web",
				"email_domain": emailDomainOf(r.FormValue("email")),
			},
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := h.uc.Register(ctx, req)
		if err != nil {
			h.flash(w, r, "signup.html", "Sign up", grpcMsg(err))
			return
		}
		// 风控触发 step-up → 跳到 /verify-otp 让用户填邮件验证码
		if resp.GetNeedsOtp() {
			http.SetCookie(w, &http.Cookie{
				Name:   "otp_challenge", Value: resp.GetOtpChallenge(),
				Path:   "/", HttpOnly: true,
				Secure: h.CookieSecure, SameSite: http.SameSiteLaxMode,
				MaxAge: 300, // 5min
			})
			http.Redirect(w, r, "/verify-otp", http.StatusFound)
			return
		}
		h.setJWTCookie(w, resp.GetJwt())
		http.Redirect(w, r, "/me", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── /login ──────────────────────────────────────────────────────────

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.render(w, "login.html", map[string]any{"Title": "Log in"})
	case http.MethodPost:
		_ = r.ParseForm()
		req := &usermerchantv1.LoginRequest{
			LoginId:         r.FormValue("login_id"),
			Password:        r.FormValue("password"),
			IpAddress:       clientIP(r),
			DeviceId:        h.deviceID(r),
			UserAgent:       r.UserAgent(),
			RiskSessionId:   r.FormValue("risk_session_id"),
			FingerprintHash: cookieValue(r, "fp"),
			Metadata:        map[string]string{"channel": "web"},
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := h.uc.Login(ctx, req)
		if err != nil {
			h.flash(w, r, "login.html", "Log in", grpcMsg(err))
			return
		}
		if resp.GetNeedsOtp() {
			http.SetCookie(w, &http.Cookie{
				Name:     "otp_challenge",
				Value:    resp.GetOtpChallenge(),
				Path:     "/", HttpOnly: true,
				Secure: h.CookieSecure, SameSite: http.SameSiteLaxMode,
				MaxAge: 300, // 5min
			})
			http.Redirect(w, r, "/verify-otp", http.StatusFound)
			return
		}
		h.setJWTCookie(w, resp.GetJwt())
		http.Redirect(w, r, "/me", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── /verify-otp ─────────────────────────────────────────────────────

func (h *Handler) handleVerifyOTP(w http.ResponseWriter, r *http.Request) {
	challenge := cookieValue(r, "otp_challenge")
	if challenge == "" {
		challenge = r.URL.Query().Get("challenge")
	}
	if challenge == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.render(w, "otp.html", map[string]any{
			"Title":     "Verify",
			"Challenge": challenge,
			"OTPSource": "邮件 / 验证器",
		})
	case http.MethodPost:
		_ = r.ParseForm()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := h.uc.VerifyOTP(ctx, &usermerchantv1.VerifyOTPRequest{
			OtpChallenge: r.FormValue("challenge"),
			Code:         strings.TrimSpace(r.FormValue("code")),
		})
		if err != nil {
			h.render(w, "otp.html", map[string]any{
				"Title": "Verify", "Challenge": challenge,
				"OTPSource": "邮件 / 验证器", "Flash": grpcMsg(err),
			})
			return
		}
		h.clearCookie(w, "otp_challenge")
		h.setJWTCookie(w, resp.GetJwt())
		http.Redirect(w, r, "/me", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── /me ─────────────────────────────────────────────────────────────

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	jwt, ok := readJWTCookie(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	intro, err := h.uc.IntrospectToken(ctx, &usermerchantv1.IntrospectTokenRequest{Jwt: jwt})
	if err != nil || !intro.GetValid() {
		h.clearCookie(w, CookieName)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	user, err := h.uc.GetUser(ctx, &usermerchantv1.GetUserRequest{UserId: intro.GetUserId()})
	if err != nil {
		http.Error(w, "fetch user failed: "+grpcMsg(err), http.StatusInternalServerError)
		return
	}
	// 多币种账户：accounting-system 才是余额真账，本仓只展示已开户的币种 +
	// account_id；运营同事/SDK 拿 account_id 再调 accounting-system 查实时余额。
	accs, _ := h.uc.ListUserAccounts(ctx, &usermerchantv1.ListUserAccountsRequest{UserId: intro.GetUserId()})

	type acctRow struct{ Currency, AccountID, Status string }
	rows := make([]acctRow, 0)
	if accs != nil {
		for _, a := range accs.GetAccounts() {
			rows = append(rows, acctRow{
				Currency:  a.GetCurrency(),
				AccountID: a.GetAccountId(),
				Status:    a.GetStatus(),
			})
		}
	}
	h.render(w, "me.html", map[string]any{
		"Title":    "Account",
		"User":     user,
		"Accounts": rows,
	})
}

// ── /logout ─────────────────────────────────────────────────────────

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	h.clearCookie(w, CookieName)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ── helpers ─────────────────────────────────────────────────────────

func (h *Handler) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t, ok := h.pages[name]
	if !ok {
		h.logger.Warn("unknown template", zap.String("name", name))
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	// 每棵树根 = layout，所以执行 "layout" 让外壳调 page 的 {{define "content"}}。
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		h.logger.Warn("template render failed", zap.String("name", name), zap.Error(err))
		http.Error(w, "internal", http.StatusInternalServerError)
	}
}

func (h *Handler) flash(w http.ResponseWriter, r *http.Request, name, title, msg string) {
	h.render(w, name, map[string]any{"Title": title, "Flash": msg})
}

func (h *Handler) setJWTCookie(w http.ResponseWriter, jwt string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    jwt,
		Path:     "/",
		Domain:   h.CookieDomain,
		HttpOnly: true,
		Secure:   h.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   24 * 60 * 60,
	})
}

func (h *Handler) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", Domain: h.CookieDomain,
		MaxAge: -1, HttpOnly: true, Secure: h.CookieSecure,
	})
}

func readJWTCookie(r *http.Request) (string, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// 取第一个（最远端 client）
		if i := strings.IndexByte(v, ','); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if host != "" {
		return host
	}
	return r.RemoteAddr
}

// deviceID 从 cookie 取持久化 device id；没有就 hash(UA + Accept-Language) 兜底。
// 真实集成应该让前端 SDK 注入。
func (h *Handler) deviceID(r *http.Request) string {
	if v := cookieValue(r, "did"); v != "" {
		return v
	}
	hh := sha256.Sum256([]byte(r.UserAgent() + "|" + r.Header.Get("Accept-Language")))
	return "anon_" + hex.EncodeToString(hh[:6])
}

func uaHashOf(ua string) string {
	if ua == "" {
		return ""
	}
	h := sha256.Sum256([]byte(ua))
	return hex.EncodeToString(h[:8])
}

func emailDomainOf(email string) string {
	if i := strings.IndexByte(email, '@'); i >= 0 {
		return strings.ToLower(email[i+1:])
	}
	return ""
}

func grpcMsg(err error) string {
	if err == nil {
		return ""
	}
	if st, ok := status.FromError(err); ok {
		// 不暴露内部 detail；只返回 message
		return st.Message()
	}
	var ge interface{ GRPCStatus() *status.Status }
	if errors.As(err, &ge) {
		return ge.GRPCStatus().Message()
	}
	return err.Error()
}
