// Package adminhttp — OAuth2 HTTP endpoints。
//
// Routes:
//   POST /oauth2/token             — RFC 6749 §4.4 client_credentials grant
//   POST /oauth2/introspect        — RFC 7662 token introspection
//   POST /oauth2/revoke            — RFC 7009 token revocation
//   GET  /.well-known/jwks.json    — RFC 7517 JWKS (公钥分发)
//   GET  /.well-known/openid-configuration — Discovery (issuer / endpoints)
//
// Admin (内网，需 X-Admin-Token):
//   POST /admin/clients            — 创建客户端
//   GET  /admin/clients            — 列客户端
//   POST /admin/clients/{id}/rotate-secret  — 滚 secret
//   POST /admin/clients/{id}/suspend
//   POST /admin/keys/rotate        — RSA key rotation
//
// 所有 /oauth2/* 不需要 admin token，由 client_credentials 自身鉴权。

package adminhttp

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"reconcile-system/packages/oauth2-server/internal/domain"
	"reconcile-system/packages/oauth2-server/internal/jwks"
	"reconcile-system/packages/oauth2-server/internal/store"

	"go.uber.org/zap"
)

// Server HTTP handlers.
type Server struct {
	Log            *zap.Logger
	Keys           *jwks.KeyStore
	Store          store.Store
	Issuer         string
	Audience       string
	TokenTTL       time.Duration
	AdminToken     string
}

// NewServer 构造。
func NewServer(log *zap.Logger, ks *jwks.KeyStore, s store.Store, issuer, audience, adminToken string, ttl time.Duration) *Server {
	if ttl == 0 {
		ttl = time.Hour
	}
	return &Server{
		Log:        log,
		Keys:       ks,
		Store:      s,
		Issuer:     issuer,
		Audience:   audience,
		TokenTTL:   ttl,
		AdminToken: adminToken,
	}
}

// Register 挂到 mux。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/oauth2/token", s.handleToken)
	mux.HandleFunc("/oauth2/introspect", s.handleIntrospect)
	mux.HandleFunc("/oauth2/revoke", s.handleRevoke)
	mux.HandleFunc("/.well-known/jwks.json", s.handleJWKS)
	mux.HandleFunc("/.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("/admin/clients", s.handleAdminClients)
	mux.HandleFunc("/admin/clients/", s.handleAdminClientByID)
	mux.HandleFunc("/admin/keys/rotate", s.handleAdminKeyRotate)
	mux.HandleFunc("/healthz", s.handleHealth)
}

// ─── /oauth2/token ─────────────────────────────────────────────────────

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOAuthErr(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_request", "cannot parse form")
		return
	}

	req := domain.TokenRequest{
		GrantType:    r.PostForm.Get("grant_type"),
		ClientID:     r.PostForm.Get("client_id"),
		ClientSecret: r.PostForm.Get("client_secret"),
		Scope:        r.PostForm.Get("scope"),
	}
	// Basic Auth fallback (RFC 6749 §2.3.1)
	if req.ClientID == "" {
		if cid, csec, ok := r.BasicAuth(); ok {
			req.ClientID = cid
			req.ClientSecret = csec
		}
	}

	if req.GrantType != "client_credentials" {
		writeOAuthErr(w, http.StatusBadRequest, "unsupported_grant_type",
			"only client_credentials supported")
		return
	}
	if req.ClientID == "" || req.ClientSecret == "" {
		writeOAuthErr(w, http.StatusUnauthorized, "invalid_client", "client_id/client_secret required")
		return
	}

	sourceIP := clientIP(r)
	cli, grantedScope, err := store.AuthenticateClient(s.Store, req.ClientID, req.ClientSecret, req.Scope, sourceIP)
	if err != nil {
		s.Log.Warn("client auth failed",
			zap.String("client_id", req.ClientID),
			zap.String("ip", sourceIP),
			zap.Error(err))
		status, code := mapAuthErr(err)
		writeOAuthErr(w, status, code, err.Error())
		return
	}

	now := time.Now().UTC()
	exp := now.Add(s.TokenTTL)
	jti := randomJTI()
	claims := map[string]any{
		"iss":        s.Issuer,
		"aud":        s.Audience,
		"sub":        cli.ClientID,
		"client_id":  cli.ClientID,
		"owner_type": string(cli.OwnerType),
		"owner_id":   cli.OwnerID,
		"scope":      grantedScope,
		"iat":        now.Unix(),
		"exp":        exp.Unix(),
		"jti":        jti,
	}
	token, err := s.Keys.Sign(claims)
	if err != nil {
		s.Log.Error("sign token failed", zap.Error(err))
		writeOAuthErr(w, http.StatusInternalServerError, "server_error", "sign failed")
		return
	}

	go func() {
		_ = s.Store.UpdateLastUsed(cli.ClientID, now)
	}()

	resp := domain.TokenResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   int(s.TokenTTL.Seconds()),
		Scope:       grantedScope,
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── /oauth2/introspect ────────────────────────────────────────────────

func (s *Server) handleIntrospect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOAuthErr(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusOK, domain.IntrospectResponse{Active: false})
		return
	}
	token := r.PostForm.Get("token")
	if token == "" {
		writeJSON(w, http.StatusOK, domain.IntrospectResponse{Active: false})
		return
	}
	// 调用方 (resource server) 也得是 valid client (RFC 7662 §2.1)
	cid := r.PostForm.Get("client_id")
	csec := r.PostForm.Get("client_secret")
	if cid == "" {
		if c, sec, ok := r.BasicAuth(); ok {
			cid, csec = c, sec
		}
	}
	if cid != "" {
		if _, _, err := store.AuthenticateClient(s.Store, cid, csec, "", clientIP(r)); err != nil {
			writeOAuthErr(w, http.StatusUnauthorized, "invalid_client", err.Error())
			return
		}
	}

	claims, err := s.Keys.Verify(token)
	if err != nil {
		writeJSON(w, http.StatusOK, domain.IntrospectResponse{Active: false})
		return
	}
	jti, _ := claims["jti"].(string)
	if jti != "" && s.Store.IsRevoked(jti) {
		writeJSON(w, http.StatusOK, domain.IntrospectResponse{Active: false})
		return
	}
	resp := domain.IntrospectResponse{
		Active:    true,
		Scope:     stringClaim(claims, "scope"),
		ClientID:  stringClaim(claims, "client_id"),
		OwnerType: stringClaim(claims, "owner_type"),
		OwnerID:   stringClaim(claims, "owner_id"),
		TokenType: "Bearer",
		ExpiresAt: int64Claim(claims, "exp"),
		IssuedAt:  int64Claim(claims, "iat"),
		Subject:   stringClaim(claims, "sub"),
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── /oauth2/revoke ────────────────────────────────────────────────────

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOAuthErr(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	token := r.PostForm.Get("token")
	if token == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	claims, err := s.Keys.Verify(token)
	if err != nil {
		// 按 RFC 7009: 即使 token 无效也返 200
		w.WriteHeader(http.StatusOK)
		return
	}
	jti, _ := claims["jti"].(string)
	exp := int64Claim(claims, "exp")
	if jti == "" || exp == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	_ = s.Store.Revoke(jti, time.Unix(exp, 0))
	s.Log.Info("token revoked", zap.String("jti", jti))
	w.WriteHeader(http.StatusOK)
}

// ─── /.well-known/jwks.json ────────────────────────────────────────────

func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	s.Keys.ServeJWKS(w, r)
}

// ─── /.well-known/openid-configuration ─────────────────────────────────

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	cfg := map[string]any{
		"issuer":                                s.Issuer,
		"token_endpoint":                        s.Issuer + "/oauth2/token",
		"introspection_endpoint":                s.Issuer + "/oauth2/introspect",
		"revocation_endpoint":                   s.Issuer + "/oauth2/revoke",
		"jwks_uri":                              s.Issuer + "/.well-known/jwks.json",
		"grant_types_supported":                 []string{"client_credentials"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		"introspection_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported": []string{
			"charge:write", "charge:read",
			"refund:write", "refund:read",
			"dispute:write", "dispute:read",
			"kyc:read", "kyc:write",
			"merchant:read", "merchant:write",
			"ops:read", "ops:write",
		},
	}
	writeJSON(w, http.StatusOK, cfg)
}

// ─── /admin/* ──────────────────────────────────────────────────────────

func (s *Server) checkAdmin(r *http.Request) error {
	if s.AdminToken == "" {
		return errors.New("admin disabled")
	}
	got := r.Header.Get("X-Admin-Token")
	if got == "" {
		// 也支持 bearer
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			got = strings.TrimPrefix(h, "Bearer ")
		}
	}
	if got != s.AdminToken {
		return errors.New("admin token mismatch")
	}
	return nil
}

func (s *Server) handleAdminClients(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAdmin(r); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := s.Store.ListClients()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var in struct {
			Name          string `json:"name"`
			OwnerType     string `json:"owner_type"`
			OwnerID       string `json:"owner_id"`
			AllowedScopes string `json:"allowed_scopes"`
			AllowedIPs    string `json:"allowed_ips"`
			RateLimitRPS  int    `json:"rate_limit_rps"`
			ExpiresIn     int    `json:"expires_in_days"` // 可选
		}
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		clientID := genClientID(in.OwnerType, in.OwnerID)
		secret := genSecret()
		hash, err := store.HashSecret(secret)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c := &domain.Client{
			ClientID:      clientID,
			SecretHash:    hash,
			SecretLast4:   secret[len(secret)-4:],
			Name:          in.Name,
			OwnerType:     domain.ClientType(in.OwnerType),
			OwnerID:       in.OwnerID,
			AllowedScopes: in.AllowedScopes,
			AllowedIPs:    in.AllowedIPs,
			RateLimitRPS:  in.RateLimitRPS,
			Status:        "active",
		}
		if in.ExpiresIn > 0 {
			exp := time.Now().Add(time.Duration(in.ExpiresIn) * 24 * time.Hour)
			c.ExpiresAt = &exp
		}
		if err := s.Store.PutClient(c); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 返 secret — 仅此一次！
		writeJSON(w, http.StatusCreated, map[string]any{
			"client_id":     clientID,
			"client_secret": secret,
			"warning":       "save this secret now — it will never be shown again",
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAdminClientByID(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAdmin(r); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	// /admin/clients/{id}/{action}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/clients/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		http.Error(w, "client id required", http.StatusBadRequest)
		return
	}
	cid := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	c, err := s.Store.GetClient(cid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	switch action {
	case "":
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, c)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case "rotate-secret":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		newSec := genSecret()
		hash, err := store.HashSecret(newSec)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c.SecretHash = hash
		c.SecretLast4 = newSec[len(newSec)-4:]
		if err := s.Store.PutClient(c); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"client_id":     c.ClientID,
			"client_secret": newSec,
			"warning":       "save this secret now",
		})
	case "suspend":
		c.Status = "suspended"
		_ = s.Store.PutClient(c)
		writeJSON(w, http.StatusOK, c)
	case "activate":
		c.Status = "active"
		_ = s.Store.PutClient(c)
		writeJSON(w, http.StatusOK, c)
	case "revoke":
		c.Status = "revoked"
		_ = s.Store.PutClient(c)
		writeJSON(w, http.StatusOK, c)
	default:
		http.Error(w, "unknown action: "+action, http.StatusBadRequest)
	}
}

func (s *Server) handleAdminKeyRotate(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAdmin(r); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := s.Keys.Rotate(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Log.Info("RSA key rotated", zap.String("new_kid", s.Keys.Active().KID))
	writeJSON(w, http.StatusOK, map[string]any{
		"new_kid": s.Keys.Active().KID,
		"message": "active key rotated; old key kept 30d for token verification",
	})
}

// ─── /healthz ──────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "oauth2-server"})
}

// ─── helpers ───────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeOAuthErr(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, domain.ErrorResponse{Error: code, ErrorDescription: desc})
}

func clientIP(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		parts := strings.Split(h, ",")
		return strings.TrimSpace(parts[0])
	}
	if h := r.Header.Get("X-Real-IP"); h != "" {
		return h
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func stringClaim(c map[string]any, k string) string {
	v, _ := c[k].(string)
	return v
}

func int64Claim(c map[string]any, k string) int64 {
	if f, ok := c[k].(float64); ok {
		return int64(f)
	}
	if i, ok := c[k].(int64); ok {
		return i
	}
	return 0
}

func mapAuthErr(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrClientNotFound), errors.Is(err, store.ErrInvalidSecret):
		return http.StatusUnauthorized, "invalid_client"
	case errors.Is(err, store.ErrClientSuspended), errors.Is(err, store.ErrClientExpired):
		return http.StatusUnauthorized, "unauthorized_client"
	case errors.Is(err, store.ErrIPNotAllowed):
		return http.StatusUnauthorized, "unauthorized_client"
	case errors.Is(err, store.ErrScopeNotAllowed):
		return http.StatusBadRequest, "invalid_scope"
	default:
		return http.StatusBadRequest, "invalid_request"
	}
}

func genClientID(ownerType, ownerID string) string {
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	prefix := "cli"
	switch ownerType {
	case "merchant":
		prefix = "mer"
	case "service":
		prefix = "svc"
	case "ops":
		prefix = "ops"
	}
	cleaned := strings.ReplaceAll(strings.ToLower(ownerID), " ", "_")
	if cleaned == "" {
		cleaned = "anon"
	}
	if len(cleaned) > 16 {
		cleaned = cleaned[:16]
	}
	return fmt.Sprintf("%s_%s_%s", prefix, cleaned, hex.EncodeToString(suffix))
}

func genSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
