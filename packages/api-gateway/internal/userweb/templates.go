// Package userweb 用户注册 / 登录 / OTP 校验的 HTML 页面 + handler。
//
// 模板嵌入 binary（embed）；CSS/JS 极简内联，零外部依赖。
//
// 请求流：
//
//	GET  /signup       → signup.html
//	POST /signup       → call user-merchant-core UserService.Register → 设置 jwt cookie → 302 /me
//	GET  /login        → login.html
//	POST /login        → UserService.Login → needs_otp ? 302 /verify-otp : 设置 cookie + 302 /me
//	GET  /verify-otp   → otp.html (challenge 在 query string 或 cookie)
//	POST /verify-otp   → UserService.VerifyOTP → cookie + 302 /me
//	POST /logout       → 清 cookie + 302 /login
//	GET  /me           → 当前用户信息（cookie 验 + UserService.GetUser）
//
// 风控参数从 form / cookie / header 拿：ip / device_id / user_agent /
// risk_session_id / fingerprint_hash。空就空，user-merchant-core 会兜底。
package userweb

import (
	"embed"
	"fmt"
	"html/template"
)

//go:embed templates/*.html
var templatesFS embed.FS

// loadTemplates 给每个页面建独立的 *template.Template（layout + 该页）。
//
// 历史 bug：之前一次性 ParseFS("templates/*.html") 进同一棵树，每个页面都
// {{define "content"}}…{{end}}（同名），后解析的文件覆盖前面的，导致
// /signup 和 /login 都渲染最后一个 page 的 content block。
//
// 拆 per-page 树后每个 page 自己 define "content"，互不污染。
func loadTemplates() (map[string]*template.Template, error) {
	// 文件名 → handler render 时用的 key。otp.html 有个 page 就叫 "otp"，
	// handler 里就传 "otp.html"，对得上。
	pages := []string{
		"signup", "login", "otp", "me",
		// 卡支付前端：cards.go 用
		"cards", "cards_new", "pay", "pay_result",
	}
	out := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t, err := template.New(p+".html").ParseFS(templatesFS,
			"templates/layout.html",
			"templates/"+p+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		out[p+".html"] = t
	}
	return out, nil
}
