// Package auth — TOTP enroll HTTP endpoint。
//
// POST /admin/auth/totp/enable
//
//	Auth: OIDC bearer (无需 MFA 头本身，enroll 不能要求已 enroll)
//	Body: 无（user_id 从 OIDC claim 拿）
//	返：{ "secret": "...", "otpauth_uri": "otpauth://..." }
//
// 前端拿 otpauth_uri 渲 QR；用户在 Authenticator 添加后，下次写操作请求带
// X-Risk-MFA-Code: <6digit> 即可通过。
//
// 安全考虑：
//   - enroll 接口不要求 MFA（first-time bootstrap）；但需要 OIDC bearer，
//     仅本人能 enroll 自己。
//   - 已 enroll 用户再调 → 强制刷新 secret（rotate 场景：手机丢失）。生产可
//     加 confirmation flow（输入两次 code 才覆盖）。
//   - issuer 字符串走 config（"Risk Admin (prod)" / "Risk Admin (staging)"
//     便于用户区分多环境的 entry）。
package auth

import (
	"encoding/json"
	"net/http"
)

// RegisterTOTPEnableHandler 把 POST /admin/auth/totp/enable 挂到 mux。
// 调用方应保证本路径已经被 OIDCMiddleware 覆盖（拿 OIDC sub）。
func RegisterTOTPEnableHandler(mux *http.ServeMux, v *TOTPVerifier, issuer string) {
	if v == nil {
		return
	}
	if issuer == "" {
		issuer = "RiskAdmin"
	}
	mux.HandleFunc("/admin/auth/totp/enable", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		actor := ActorFromContext(r.Context())
		if actor == nil || actor.UserID == "" {
			// 拒绝无 OIDC ctx 的 enroll（dev mode 也禁，避免空 user_id 污染 mem store）
			http.Error(w, `{"error":"no oidc context; enable admin.oidc"}`, http.StatusForbidden)
			return
		}
		accountName := actor.Email
		if accountName == "" {
			accountName = actor.UserID
		}
		secret, otpauth, err := v.Enroll(actor.UserID, issuer, accountName)
		if err != nil {
			http.Error(w, `{"error":"enroll failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"secret":      secret,
			"otpauth_uri": otpauth,
		})
	})
}
