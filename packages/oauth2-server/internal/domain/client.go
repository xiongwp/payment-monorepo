// Package domain — OAuth 2.0 客户端 + token 实体。
//
// 实现 OAuth 2.0 RFC 6749 — Client Credentials Grant (RFC 6749 §4.4):
//
//   1. 商户/服务后台拿到 client_id + client_secret (出 dashboard 复制 1 次)
//   2. POST /oauth2/token
//      grant_type=client_credentials
//      client_id=...
//      client_secret=...
//      scope=charge:write refund:write   (可选)
//      → 返 access_token (RS256 JWT, 1h 有效)
//   3. 调 API 带 Authorization: Bearer <access_token>
//   4. 服务端用 /.well-known/jwks.json 校验签名 + claim
//   5. token 过期前 5min 自动 refresh (商户 client SDK 做)
//
// 不实现的（精简化）:
//   - Authorization Code Flow (用户授权) — 支付系统不需要用户授权
//   - Refresh Token — client_credentials 直接重新拿就行
//   - PKCE — 同上无 user
//
// 标准:
//   RFC 6749 §4.4 Client Credentials
//   RFC 7519 JWT
//   RFC 7517 JWKS
//   RFC 7662 Token Introspection

package domain

import "time"

// Client OAuth 客户端 (商户 / 内部服务)。
type Client struct {
	ID           int64     `db:"id" json:"id"`
	ClientID     string    `db:"client_id" json:"client_id"`            // 公开 (mer_xxx_oauth)
	SecretHash   string    `db:"secret_hash" json:"-"`                  // bcrypt(secret)
	SecretLast4  string    `db:"secret_last4" json:"secret_last4"`      // UI 显示
	Name         string    `db:"name" json:"name"`                      // human-readable
	OwnerType    ClientType `db:"owner_type" json:"owner_type"`         // 'merchant'|'service'|'ops'
	OwnerID      string    `db:"owner_id" json:"owner_id"`              // merchant_id 或 service name
	AllowedScopes string   `db:"allowed_scopes" json:"allowed_scopes"`  // CSV: "charge:write refund:read ..."
	AllowedIPs   string    `db:"allowed_ips" json:"allowed_ips,omitempty"` // 可选 IP 白名单 CSV
	RateLimitRPS int       `db:"rate_limit_rps" json:"rate_limit_rps"`  // 0 = 走 default
	Status       string    `db:"status" json:"status"`                  // 'active'|'suspended'|'revoked'
	ExpiresAt    *time.Time `db:"expires_at" json:"expires_at,omitempty"` // 客户端凭据本身的过期
	LastUsedAt   *time.Time `db:"last_used_at" json:"last_used_at,omitempty"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

// ClientType 客户端类型。
type ClientType string

const (
	ClientMerchant ClientType = "merchant"  // 商户 SDK 用
	ClientService  ClientType = "service"   // 内部服务间（mTLS + OAuth 双重）
	ClientOps      ClientType = "ops"       // ops 后台脚本
)

// TokenRequest /token 端点 form 字段。
type TokenRequest struct {
	GrantType    string `json:"grant_type"`    // 必须 "client_credentials"
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Scope        string `json:"scope,omitempty"`
}

// TokenResponse RFC 6749 §5.1 标准响应。
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`   // "Bearer"
	ExpiresIn   int    `json:"expires_in"`   // 秒数
	Scope       string `json:"scope,omitempty"`
}

// ErrorResponse RFC 6749 §5.2。
type ErrorResponse struct {
	Error            string `json:"error"`              // invalid_request / invalid_client / invalid_grant / unauthorized_client / unsupported_grant_type / invalid_scope
	ErrorDescription string `json:"error_description"`
}

// IntrospectResponse RFC 7662。
type IntrospectResponse struct {
	Active     bool   `json:"active"`
	Scope      string `json:"scope,omitempty"`
	ClientID   string `json:"client_id,omitempty"`
	OwnerType  string `json:"owner_type,omitempty"`
	OwnerID    string `json:"owner_id,omitempty"`
	TokenType  string `json:"token_type,omitempty"`
	ExpiresAt  int64  `json:"exp,omitempty"`
	IssuedAt   int64  `json:"iat,omitempty"`
	Subject    string `json:"sub,omitempty"`
}

// Claims JWT payload 自定义 claims。
type Claims struct {
	Sub       string `json:"sub"`         // client_id
	ClientID  string `json:"client_id"`
	OwnerType string `json:"owner_type"`
	OwnerID   string `json:"owner_id"`    // merchant_id 等
	Scope     string `json:"scope"`
	Iss       string `json:"iss"`         // "https://oauth.payment.example.com"
	Aud       string `json:"aud"`         // "payment-api"
	Iat       int64  `json:"iat"`
	Exp       int64  `json:"exp"`
	Jti       string `json:"jti"`         // unique token ID (revocation 用)
}
