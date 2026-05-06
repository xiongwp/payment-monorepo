// admin_html.go：服务端渲染的管理页面。
//
// 跟 api-gateway/userweb 同形：纯 server-side templates，无前端框架。
// 路由：
//
//	GET  /admin/                       首页 — namespace 列表 + 跳转
//	GET  /admin/ns/{namespace}         namespace 下所有 key 列表 + active version
//	GET  /admin/ns/{namespace}/{key}   单 key 详情 — 当前 + 历史 version 表 + diff
//	GET  /admin/ns/{namespace}/{key}/edit  编辑表单 — 选 strategy 4 选 1 + value + reason
//	POST /admin/ns/{namespace}/{key}/put   提交（CSRF token + actor 来自 session）
//	POST /admin/ns/{namespace}/{key}/rollback?to=N  回滚到指定 version
//	GET  /admin/audit                  审计 log 时间序
//
// 鉴权：复用 user-merchant-core 的 admin role（admin RBAC role 才能进）。
// 跟 payment-admin-web 一致：先 mTLS 到 user-merchant-core IntrospectToken
// 验 admin claim，再放行。
//
// CSRF：每个 form 嵌一次性 token，cookie 同名校验（Double Submit Cookie）。
package server

import (
	"context"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/config-center/internal/service"
)

// AdminHandler admin web UI handler set。
type AdminHandler struct {
	svc    *service.Service
	tpl    *template.Template
	logger *zap.Logger
}

// NewAdminHandler 加载 templates，挂 svc。
func NewAdminHandler(svc *service.Service, logger *zap.Logger) (*AdminHandler, error) {
	tpl, err := template.New("admin").Funcs(template.FuncMap{
		"formatTime": formatTime,
		"truncate":   truncate,
	}).Parse(adminTemplates)
	if err != nil {
		return nil, err
	}
	return &AdminHandler{svc: svc, tpl: tpl, logger: logger}, nil
}

// Mount 挂载路由到 mux。
//
// caller 应在外层 wrap admin auth middleware（验 admin role）；本 handler
// 只信 ctx 里 actor 字段（typed key），不信 form input。
//
// 路由清单：
//
//	GET  /admin/                        首页 — 订阅图（service ↔ item）
//	GET  /admin/ns/{namespace}          按服务名展示该服务的所有 key
//	GET  /admin/ns/{namespace}/{key}    单 key 详情 + 历史
//	GET  /admin/ns/{namespace}/{key}/edit  编辑
//	POST /admin/ns/{namespace}/{key}/{put|rollback|delete}
//	GET  /admin/items                   全平台 item 列表（按 key 名搜索 / 排序）
//	GET  /admin/items/new               新建 item 表单（含 "应用到哪些系统" 多选）
//	POST /admin/items/new               提交新建
//	GET  /admin/audit                   审计 log
func (h *AdminHandler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/admin/", h.index)
	mux.HandleFunc("/admin/ns/", h.namespaceOrKey)
	mux.HandleFunc("/admin/items", h.listItems)
	mux.HandleFunc("/admin/items/new", h.newItem)
	mux.HandleFunc("/admin/audit", h.audit)
}

// listItems 全平台 config_item 检索：
//
//	q=substring   按 key 名模糊匹配
//	sub=service   只展示某 service 订阅的
//	sort=key|updated  排序
//
// 用例：运维要看哪些 key 含 "rate_limit"、哪些 service 共用某 key 等。
func (h *AdminHandler) listItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	sub := r.URL.Query().Get("sub")
	rows, err := h.svc.SearchItems(r.Context(), q, sub, 200)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	h.render(w, "list_items", map[string]any{
		"Title":      "All Config Items",
		"Q":          q,
		"Subscriber": sub,
		"Rows":       rows,
	})
}

// newItem GET 渲染表单 / POST 创建。
// 表单字段：namespace, key, value, format, strategy, subscribers[]（多选哪些 service）。
func (h *AdminHandler) newItem(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.render(w, "new_item", map[string]any{
			"Title": "New Config Item",
			// 候选 subscriber 列表（也是 namespace 列表）
			"Services": []string{
				"card-payment", "card-center", "order-core", "payment-core",
				"payment-channel", "user-merchant-core", "accounting-system",
				"risk-manage", "api-gateway", "clearing-settlement",
			},
			"CSRFToken": csrfTokenFor(r),
		})
	case http.MethodPost:
		if !validateCSRF(r) {
			http.Error(w, "csrf check failed", http.StatusForbidden)
			return
		}
		actor := actorFromCtx(r.Context())
		if actor == "" {
			http.Error(w, "no actor", http.StatusUnauthorized)
			return
		}
		_ = r.ParseForm()
		ns := r.FormValue("namespace")
		key := r.FormValue("key")
		// service.PutConfig 走正常入库路径
		_, err := h.svc.PutConfig(r.Context(), service.PutVersionInput{
			Namespace:    ns,
			Key:          key,
			Value:        r.FormValue("value"),
			Format:       valueOrDefault(r.FormValue("format"), "json"),
			Strategy:     valueOrDefault(r.FormValue("strategy"), "FULL"),
			Actor:        actor,
			ChangeReason: r.FormValue("reason"),
		})
		if err != nil {
			http.Error(w, "put: "+err.Error(), 400)
			return
		}
		// 同时挂订阅关系
		subs := r.Form["subscribers"]
		if err := h.svc.SetSubscribers(r.Context(), ns, key, subs); err != nil {
			h.logger.Warn("set subscribers failed", zap.Error(err))
		}
		http.Redirect(w, r, "/admin/ns/"+ns+"/"+key, http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// index 全平台配置统一入口 —— 按角色分组展示 12 个 namespace。
//
// 把所有业务服务的「配置管理页面」收口到这里：
//   - 业务侧服务（accounting-system / order-core / payment-core 等）原本各自
//     的 admin /config endpoint 已 410 Gone 改 redirect → config-center
//   - 全平台动态配置只在本页编辑、版本化、审计、灰度推送
//   - 改一次 → SDK watch → 集群所有副本秒级 OnChange 热更新
type namespaceGroup struct {
	Title string
	Items []namespaceItem
}
type namespaceItem struct {
	Name string
	Note string
}

func (h *AdminHandler) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/" && r.URL.Path != "/admin" {
		http.NotFound(w, r)
		return
	}
	// 12 namespace 按业务角色分组，让 admin 一目了然
	groups := []namespaceGroup{
		{
			Title: "支付核心",
			Items: []namespaceItem{
				{"order-core", "PI / refund / outbox / charge_expire"},
				{"payment-core", "routing weights / risk fail_policy / breaker"},
				{"payment-channel", "adapter timeouts / rate_limit"},
			},
		},
		{
			Title: "卡支付（PCI 隔离）",
			Items: []namespaceItem{
				{"card-center", "tokenize 限流 / session TTL / Luhn 严格 mode"},
				{"card-payment", "bulkhead / network 超时 / reconcile interval"},
			},
		},
		{
			Title: "用户 / 商户 / 风控",
			Items: []namespaceItem{
				{"user-merchant-core", "JWT TTL / OTP / bcrypt cost / retention"},
				{"risk-manage", "rule thresholds / fail-policy / circuit breaker"},
				{"api-gateway", "rate_limit / cors / cookie / shadow trusted CIDR"},
			},
		},
		{
			Title: "记账 / 清算 / 对账",
			Items: []namespaceItem{
				{"accounting-system", "tcc_recovery / outbox.poll / day_cut.chunk_size"},
				{"clearing-settlement", "对账批跑窗口 / 异常 case 阈值"},
				{"reconplatform", "rules (expr 表达式 map)"},
			},
		},
		{
			Title: "基础设施",
			Items: []namespaceItem{
				{"kms-manage", "rate_limit / SAN whitelist (敏感，建议 TARGETED)"},
			},
		},
	}
	h.render(w, "index", map[string]any{
		"Title":  "Config Center — 全平台动态配置统一入口",
		"Groups": groups,
	})
}

// namespaceOrKey dispatch /admin/ns/{ns}, /admin/ns/{ns}/{key}, /admin/ns/{ns}/{key}/edit
// 等子路径。
func (h *AdminHandler) namespaceOrKey(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/ns/")
	parts := strings.SplitN(rest, "/", 3)
	switch len(parts) {
	case 1:
		h.listKeys(w, r, parts[0])
	case 2:
		h.keyDetail(w, r, parts[0], parts[1])
	case 3:
		h.keyAction(w, r, parts[0], parts[1], parts[2])
	default:
		http.NotFound(w, r)
	}
}

// listKeys 列 namespace 下所有 key。
func (h *AdminHandler) listKeys(w http.ResponseWriter, r *http.Request, ns string) {
	rows, err := h.svc.ListNamespace(r.Context(), ns)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	h.render(w, "list_keys", map[string]any{
		"Title":     "Namespace: " + ns,
		"Namespace": ns,
		"Items":     rows,
	})
}

// keyDetail 详情 + 历史 version 表。
func (h *AdminHandler) keyDetail(w http.ResponseWriter, r *http.Request, ns, key string) {
	versions, err := h.svc.ListVersions(r.Context(), ns, key, 50)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	current, _ := h.svc.GetActiveAdmin(r.Context(), ns, key)
	subs, _ := h.svc.GetSubscribers(r.Context(), ns, key)
	h.render(w, "key_detail", map[string]any{
		"Title":       ns + "/" + key,
		"Namespace":   ns,
		"Key":         key,
		"Current":     current,
		"Versions":    versions,
		"Subscribers": subs,
		"CSRFToken":   csrfTokenFor(r),
	})
}

// diff 两个 version 的内容差异。
//
//	GET /admin/ns/{ns}/{key}/diff?from=N&to=M
//
// 服务端用 simple LCS（standard go diff 不进 stdlib，自己写）；输出 unified-diff
// 风格的 HTML，加色标 + 行号。生产可换成 https://github.com/sergi/go-diff
// 但 v0.1 自己来。
//
// 用例：admin 看版本 5 改了什么 → 在版本列表点 "diff vs 4" 跳到本路径。
func (h *AdminHandler) diff(w http.ResponseWriter, r *http.Request, ns, key string) {
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if from <= 0 || to <= 0 {
		http.Error(w, "from/to required", 400)
		return
	}
	a, _ := h.svc.GetVersion(r.Context(), ns, key, from)
	b, _ := h.svc.GetVersion(r.Context(), ns, key, to)
	if a == nil || b == nil {
		http.Error(w, "version not found", 404)
		return
	}
	lines := unifiedDiff(a.Value, b.Value)
	h.render(w, "diff", map[string]any{
		"Title":     "Diff " + ns + "/" + key,
		"Namespace": ns,
		"Key":       key,
		"From":      a, "To": b,
		"Lines":     lines,
	})
}

// unifiedDiff 极简 line-by-line LCS diff。
// 返 []DiffLine：{Kind: "ctx"|"add"|"del", Text}
type DiffLine struct {
	Kind string
	Text string
	OldNum, NewNum int
}

func unifiedDiff(a, b string) []DiffLine {
	la := strings.Split(a, "\n")
	lb := strings.Split(b, "\n")
	// 经典 LCS DP
	m, n := len(la), len(lb)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if la[i-1] == lb[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] > dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	// backtrack
	out := []DiffLine{}
	i, j := m, n
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && la[i-1] == lb[j-1]:
			out = append([]DiffLine{{Kind: "ctx", Text: la[i-1], OldNum: i, NewNum: j}}, out...)
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			out = append([]DiffLine{{Kind: "add", Text: lb[j-1], NewNum: j}}, out...)
			j--
		default:
			out = append([]DiffLine{{Kind: "del", Text: la[i-1], OldNum: i}}, out...)
			i--
		}
	}
	return out
}

// keyAction edit form / put / rollback / delete / diff
func (h *AdminHandler) keyAction(w http.ResponseWriter, r *http.Request, ns, key, action string) {
	switch action {
	case "edit":
		h.renderEditForm(w, r, ns, key)
	case "put":
		h.handlePut(w, r, ns, key)
	case "rollback":
		h.handleRollback(w, r, ns, key)
	case "delete":
		h.handleDelete(w, r, ns, key)
	case "diff":
		h.diff(w, r, ns, key)
	default:
		http.NotFound(w, r)
	}
}

func (h *AdminHandler) renderEditForm(w http.ResponseWriter, r *http.Request, ns, key string) {
	cur, _ := h.svc.GetActiveAdmin(r.Context(), ns, key)
	h.render(w, "edit", map[string]any{
		"Title":     "Edit " + ns + "/" + key,
		"Namespace": ns,
		"Key":       key,
		"Current":   cur,
		"CSRFToken": csrfTokenFor(r),
	})
}

func (h *AdminHandler) handlePut(w http.ResponseWriter, r *http.Request, ns, key string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !validateCSRF(r) {
		http.Error(w, "csrf check failed", http.StatusForbidden)
		return
	}
	actor := actorFromCtx(r.Context())
	if actor == "" {
		http.Error(w, "no actor in context (need admin login)", http.StatusUnauthorized)
		return
	}
	in := service.PutVersionInput{
		Namespace:    ns,
		Key:          key,
		Value:        r.FormValue("value"),
		Format:       valueOrDefault(r.FormValue("format"), "json"),
		Strategy:     valueOrDefault(r.FormValue("strategy"), "FULL"),
		StrategySpec: r.FormValue("strategy_spec"),
		Actor:        actor,
		ChangeReason: r.FormValue("reason"),
	}
	if t := r.FormValue("effective_at"); t != "" {
		if et, err := parseRFC3339(t); err == nil {
			in.EffectiveAt = &et
		}
	}
	if t := r.FormValue("expire_at"); t != "" {
		if et, err := parseRFC3339(t); err == nil {
			in.ExpireAt = &et
		}
	}
	if _, err := h.svc.PutConfig(r.Context(), in); err != nil {
		http.Error(w, "put failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/admin/ns/"+ns+"/"+key, http.StatusSeeOther)
}

func (h *AdminHandler) handleRollback(w http.ResponseWriter, r *http.Request, ns, key string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !validateCSRF(r) {
		http.Error(w, "csrf check failed", http.StatusForbidden)
		return
	}
	actor := actorFromCtx(r.Context())
	if actor == "" {
		http.Error(w, "no actor", http.StatusUnauthorized)
		return
	}
	to, _ := strconv.ParseInt(r.FormValue("to"), 10, 64)
	if _, err := h.svc.Rollback(r.Context(), ns, key, to, actor, r.FormValue("reason")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/admin/ns/"+ns+"/"+key, http.StatusSeeOther)
}

func (h *AdminHandler) handleDelete(w http.ResponseWriter, r *http.Request, ns, key string) {
	// TODO: 实现 service.DeleteConfig。当前 stub。
	http.Error(w, "delete not implemented", http.StatusNotImplemented)
}

func (h *AdminHandler) audit(w http.ResponseWriter, r *http.Request) {
	// 取最近 100 条 audit log
	rows, _ := h.svc.RecentAudit(r.Context(), 100)
	h.render(w, "audit", map[string]any{
		"Title": "Audit Log",
		"Rows":  rows,
	})
}

// ─── helpers ────────────────────────────────────────────────────────────

func (h *AdminHandler) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if err := h.tpl.ExecuteTemplate(w, name, data); err != nil {
		h.logger.Error("template render failed", zap.String("name", name), zap.Error(err))
	}
}

type ctxKey string

const ctxActorKey ctxKey = "config_center_actor"

func actorFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(ctxActorKey).(string)
	return v
}

// WithActor caller（admin auth middleware）拿到 admin user_id 后塞 ctx。
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, ctxActorKey, actor)
}

func valueOrDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// CSRF token 极简：从 cookie 读，跟 form 里的 hidden 字段比对（Double Submit）。
// 真实实现要在 admin auth middleware 派发；这里 stub。
func csrfTokenFor(r *http.Request) string {
	c, err := r.Cookie("csrf_token")
	if err != nil {
		return "unset"
	}
	return c.Value
}

func validateCSRF(r *http.Request) bool {
	formTok := r.FormValue("csrf_token")
	c, err := r.Cookie("csrf_token")
	if err != nil || formTok == "" || c.Value == "" {
		return false
	}
	return formTok == c.Value
}

func formatTime(t any) string {
	switch v := t.(type) {
	case time.Time:
		if v.IsZero() {
			return "—"
		}
		return v.Format("2006-01-02 15:04:05")
	case *time.Time:
		if v == nil || v.IsZero() {
			return "—"
		}
		return v.Format("2006-01-02 15:04:05")
	}
	return ""
}
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// parseRFC3339 admin form 里 datetime-local 提交的格式（YYYY-MM-DDThh:mm 或完整 RFC3339）。
func parseRFC3339(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// HTML datetime-local 格式
	if t, err := time.ParseInLocation("2006-01-02T15:04", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, &time.ParseError{Layout: time.RFC3339, Value: s}
}

// adminTemplates 所有 HTML 模板拼一起。生产建议拆 separate files + embed.FS。
const adminTemplates = `
{{define "layout"}}
<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>{{.Title}}</title>
<style>
  body { font-family: -apple-system, sans-serif; max-width: 1100px; margin: 24px auto; padding: 0 12px; color: #222; }
  table { border-collapse: collapse; width: 100%; margin: 12px 0; }
  th, td { border: 1px solid #ddd; padding: 6px 8px; text-align: left; vertical-align: top; }
  th { background: #f5f5f5; }
  pre { background: #f9f9f9; padding: 8px; border-radius: 3px; overflow-x: auto; }
  .badge { display: inline-block; padding: 2px 8px; border-radius: 10px; font-size: 12px; }
  .b-full { background:#e6f4ea; color:#1e7e34; }
  .b-canary { background:#fff3cd; color:#856404; }
  .b-targeted { background:#cce5ff; color:#004085; }
  .b-scheduled { background:#e2e3e5; color:#383d41; }
  nav a { margin-right: 12px; }
  form .row { margin: 8px 0; }
  input, textarea, select { width: 100%; box-sizing: border-box; padding: 6px; }
  button { background: #1f6feb; color: white; padding: 8px 16px; border: 0; border-radius: 4px; cursor: pointer; }
  .muted { color: #888; font-size: 12px; }
</style>
</head>
<body>
<nav>
  <a href="/admin/">Namespaces</a>
  <a href="/admin/audit">Audit Log</a>
</nav>
<h1>{{.Title}}</h1>
{{template "body" .}}
</body></html>
{{end}}

{{define "index"}}{{template "layout" .}}{{end}}
{{define "body"}}
{{- /* 全平台 12 namespace 按业务角色分组 */ -}}
<p class="muted">
  本系统是全平台所有服务的<b>动态配置统一入口</b>。
  改任一 key → SDK watch → 集群所有副本秒级 OnChange 热更新。
  各业务服务原 /admin/config 端点已 410 Gone，请改用本页。
</p>
{{range .Groups}}
<h2>{{.Title}}</h2>
<table>
  <tr><th>Namespace</th><th>主要配置项</th><th>动作</th></tr>
  {{range .Items}}
  <tr>
    <td><b>{{.Name}}</b></td>
    <td class="muted">{{.Note}}</td>
    <td>
      <a href="/admin/ns/{{.Name}}">查看 keys</a> |
      <a href="/admin/items/new?ns={{.Name}}">新增 key</a>
    </td>
  </tr>
  {{end}}
</table>
{{end}}
<p class="muted" style="margin-top:24px">
  <a href="/admin/items">全平台 key 检索</a> |
  <a href="/admin/audit">审计日志</a> |
  <a href="/healthz">健康</a>
</p>
{{end}}
`
