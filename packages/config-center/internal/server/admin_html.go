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
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Config Center</title>
<style>
  /* ── 跟 payment-admin-web / accounting-admin-web (Ant Design v5) 视觉对齐 ── */
  :root {
    --ant-primary: #1677ff;
    --ant-primary-hover: #4096ff;
    --ant-primary-active: #0958d9;
    --ant-success: #52c41a;
    --ant-warning: #faad14;
    --ant-error: #ff4d4f;
    --ant-text: rgba(0,0,0,0.88);
    --ant-text-secondary: rgba(0,0,0,0.65);
    --ant-text-tertiary: rgba(0,0,0,0.45);
    --ant-border: #d9d9d9;
    --ant-border-secondary: #f0f0f0;
    --ant-bg-layout: #f5f5f5;
    --ant-bg-container: #fff;
  }
  * { box-sizing: border-box; }
  html, body {
    margin: 0; padding: 0;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "Helvetica Neue",
                 Helvetica, Arial, "PingFang SC", "Hiragino Sans GB",
                 "Microsoft YaHei", sans-serif;
    font-size: 14px;
    color: var(--ant-text);
    background: var(--ant-bg-layout);
    line-height: 1.5715;
  }
  /* ── App layout：左侧 sidebar + 顶部 header + 内容区 ── */
  .app { display: flex; min-height: 100vh; }
  .sider {
    width: 220px; background: #001529; color: rgba(255,255,255,0.85);
    flex-shrink: 0; padding: 0;
  }
  .sider .logo {
    height: 56px; line-height: 56px; padding: 0 24px;
    color: #fff; font-size: 16px; font-weight: 600;
    border-bottom: 1px solid rgba(255,255,255,0.08);
  }
  .sider .logo .badge-cc {
    display: inline-block; margin-left: 8px; padding: 1px 6px;
    background: var(--ant-primary); border-radius: 3px;
    font-size: 11px; font-weight: 400;
  }
  .sider nav { padding: 8px 0; }
  .sider nav a {
    display: block; padding: 10px 24px;
    color: rgba(255,255,255,0.75); text-decoration: none;
    transition: background 0.2s;
  }
  .sider nav a:hover { background: rgba(255,255,255,0.08); color: #fff; }
  .sider nav a.active { background: var(--ant-primary); color: #fff; }
  .sider nav .group {
    padding: 16px 24px 8px; font-size: 12px;
    color: rgba(255,255,255,0.4); text-transform: uppercase;
    letter-spacing: 0.5px;
  }

  .main { flex: 1; min-width: 0; }
  .header {
    height: 56px; background: #fff; padding: 0 24px;
    border-bottom: 1px solid var(--ant-border-secondary);
    display: flex; align-items: center;
  }
  .header .crumb { color: var(--ant-text-secondary); }
  .header .crumb a { color: var(--ant-primary); text-decoration: none; }
  .header .crumb a:hover { color: var(--ant-primary-hover); }
  .header .crumb .sep { margin: 0 8px; color: var(--ant-text-tertiary); }

  .content { padding: 24px; max-width: 1280px; }
  .page-title { font-size: 20px; font-weight: 600; margin: 0 0 16px; color: var(--ant-text); }
  .page-desc { color: var(--ant-text-secondary); margin: 0 0 24px; }

  /* ── Card ── */
  .card {
    background: var(--ant-bg-container);
    border-radius: 8px;
    box-shadow: 0 1px 2px rgba(0,0,0,0.03);
    border: 1px solid var(--ant-border-secondary);
    margin-bottom: 16px;
    overflow: hidden;
  }
  .card-head {
    padding: 12px 24px; border-bottom: 1px solid var(--ant-border-secondary);
    font-weight: 500; font-size: 16px;
    display: flex; justify-content: space-between; align-items: center;
  }
  .card-head .extra { font-size: 14px; font-weight: normal; }
  .card-body { padding: 24px; }

  /* ── Alert ── */
  .alert {
    padding: 12px 16px; border-radius: 8px; margin-bottom: 16px;
    border: 1px solid; line-height: 1.6;
  }
  .alert-info {
    background: #e6f4ff; border-color: #91caff; color: #002c8c;
  }
  .alert-info::before { content: "ⓘ "; font-weight: bold; }

  /* ── Table ── */
  table {
    width: 100%; border-collapse: collapse; background: var(--ant-bg-container);
  }
  table thead th {
    background: #fafafa; padding: 12px 16px;
    text-align: left; font-weight: 500;
    color: var(--ant-text); border-bottom: 1px solid var(--ant-border-secondary);
    font-size: 14px;
  }
  table tbody td {
    padding: 12px 16px;
    border-bottom: 1px solid var(--ant-border-secondary);
    vertical-align: top;
  }
  table tbody tr:hover { background: #fafafa; }
  table tbody tr:last-child td { border-bottom: none; }
  table a { color: var(--ant-primary); text-decoration: none; }
  table a:hover { color: var(--ant-primary-hover); text-decoration: underline; }

  /* ── Tag / Badge ── */
  .tag, .badge {
    display: inline-block; padding: 0 7px; line-height: 20px;
    font-size: 12px; border-radius: 4px;
    border: 1px solid; white-space: nowrap;
  }
  .b-full, .b-FULL, .b-full      { background:#f6ffed; color:#389e0d; border-color:#b7eb8f; }
  .b-canary, .b-CANARY           { background:#fffbe6; color:#d48806; border-color:#ffe58f; }
  .b-targeted, .b-TARGETED       { background:#e6f4ff; color:#0958d9; border-color:#91caff; }
  .b-scheduled, .b-SCHEDULED     { background:#f9f0ff; color:#531dab; border-color:#d3adf7; }
  .b-add { background:#f6ffed; color:#389e0d; border-color:#b7eb8f; }
  .b-del { background:#fff1f0; color:#cf1322; border-color:#ffa39e; }
  .b-ctx { background:#fafafa; color:#595959; border-color:#d9d9d9; }

  /* ── Buttons ── */
  button, .btn {
    background: var(--ant-primary); color: #fff;
    border: 1px solid var(--ant-primary); padding: 6px 16px;
    border-radius: 6px; cursor: pointer; font-size: 14px;
    transition: all 0.2s; line-height: 1.5;
  }
  button:hover, .btn:hover { background: var(--ant-primary-hover); border-color: var(--ant-primary-hover); }
  button:active, .btn:active { background: var(--ant-primary-active); }
  .btn-default {
    background: #fff; color: var(--ant-text); border-color: var(--ant-border);
  }
  .btn-default:hover { color: var(--ant-primary); border-color: var(--ant-primary); }
  .btn-danger { background: var(--ant-error); border-color: var(--ant-error); }

  /* ── Form ── */
  form .row { margin-bottom: 16px; }
  form label {
    display: block; margin-bottom: 6px;
    color: var(--ant-text); font-size: 14px;
  }
  input, textarea, select {
    width: 100%; padding: 6px 11px; font-size: 14px;
    border: 1px solid var(--ant-border); border-radius: 6px;
    transition: all 0.2s; line-height: 1.5715;
    color: var(--ant-text); background: #fff;
    font-family: inherit;
  }
  textarea { font-family: ui-monospace, "SF Mono", Consolas, monospace; min-height: 80px; }
  input:focus, textarea:focus, select:focus {
    border-color: var(--ant-primary);
    box-shadow: 0 0 0 2px rgba(5,145,255,0.1);
    outline: none;
  }

  /* ── Misc ── */
  pre {
    background: #f5f5f5; padding: 12px; border-radius: 6px;
    overflow-x: auto; font-family: ui-monospace, "SF Mono", Consolas, monospace;
    font-size: 13px; margin: 0;
  }
  .muted { color: var(--ant-text-tertiary); font-size: 13px; }
  .strong { font-weight: 500; }
  h2 { font-size: 18px; font-weight: 600; margin: 24px 0 12px; }
  h3 { font-size: 16px; font-weight: 600; margin: 20px 0 8px; }
  hr { border: none; border-top: 1px solid var(--ant-border-secondary); margin: 16px 0; }
  .toolbar { margin-bottom: 16px; display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
  .toolbar input, .toolbar select { width: auto; flex: 0 1 auto; }
</style>
</head>
<body>
<div class="app">
  <aside class="sider">
    <div class="logo">Config Center<span class="badge-cc">v1</span></div>
    <nav>
      <div class="group">概览</div>
      <a href="/admin/" {{if .Groups}}class="active"{{end}}>首页</a>
      <a href="/admin/items">全平台 key 检索</a>
      <a href="/admin/audit">审计日志</a>
      <div class="group">业务命名空间</div>
      <a href="/admin/ns/order-core">order-core</a>
      <a href="/admin/ns/payment-core">payment-core</a>
      <a href="/admin/ns/payment-channel">payment-channel</a>
      <a href="/admin/ns/card-center">card-center</a>
      <a href="/admin/ns/card-payment">card-payment</a>
      <a href="/admin/ns/user-merchant-core">user-merchant-core</a>
      <a href="/admin/ns/risk-manage">risk-manage</a>
      <a href="/admin/ns/api-gateway">api-gateway</a>
      <a href="/admin/ns/accounting-system">accounting-system</a>
      <a href="/admin/ns/clearing-settlement">clearing-settlement</a>
      <a href="/admin/ns/reconplatform">reconplatform</a>
      <a href="/admin/ns/kms-manage">kms-manage</a>
    </nav>
  </aside>
  <div class="main">
    <header class="header">
      <div class="crumb">
        <a href="/admin/">Config Center</a>
        {{if .Namespace}}<span class="sep">/</span><a href="/admin/ns/{{.Namespace}}">{{.Namespace}}</a>{{end}}
        {{if .Key}}<span class="sep">/</span>{{.Key}}{{end}}
      </div>
    </header>
    <main class="content">
      <h1 class="page-title">{{.Title}}</h1>
      {{template "body" .}}
    </main>
  </div>
</div>
</body></html>
{{end}}

{{define "index"}}{{template "layout" .}}{{end}}
{{define "list_keys"}}{{template "layout" .}}{{end}}
{{define "key_detail"}}{{template "layout" .}}{{end}}
{{define "edit"}}{{template "layout" .}}{{end}}
{{define "diff"}}{{template "layout" .}}{{end}}
{{define "list_items"}}{{template "layout" .}}{{end}}
{{define "new_item"}}{{template "layout" .}}{{end}}
{{define "audit"}}{{template "layout" .}}{{end}}

{{define "body"}}
{{- /* body 按页面类型 dispatch；layout 调本块时根据 .Title 关键词分支 */ -}}

{{if .Groups}}
{{- /* 首页：全平台 12 namespace 按业务角色分组 */ -}}
<div class="alert alert-info">
  本系统是全平台所有服务的<strong>动态配置统一入口</strong>。
  改任一 key → SDK watch → 集群所有副本秒级 OnChange 热更新。
  各业务服务原 <code>/admin/config</code> 端点已 410 Gone，请改用本页。
</div>
{{range .Groups}}
<div class="card">
  <div class="card-head">{{.Title}}</div>
  <table>
    <thead><tr><th>Namespace</th><th>主要配置项</th><th style="width:200px">动作</th></tr></thead>
    <tbody>
    {{range .Items}}
    <tr>
      <td><strong>{{.Name}}</strong></td>
      <td class="muted">{{.Note}}</td>
      <td>
        <a href="/admin/ns/{{.Name}}">查看 keys</a> ·
        <a href="/admin/items/new?ns={{.Name}}">新增 key</a>
      </td>
    </tr>
    {{end}}
    </tbody>
  </table>
</div>
{{end}}

{{else if .Items}}
{{- /* list_keys：单 namespace 下所有 key */ -}}
{{if .Namespace}}<p class="muted">Namespace: <b>{{.Namespace}}</b></p>{{end}}
<p>
  <a href="/admin/items/new?ns={{.Namespace}}">+ 新增 key</a> |
  <a href="/admin/">返回首页</a>
</p>
<table>
  <tr><th>Key</th><th>Active Version</th><th>Latest Version</th><th>Updated</th><th>动作</th></tr>
  {{range .Items}}
  <tr>
    <td><a href="/admin/ns/{{$.Namespace}}/{{.KeyName}}">{{.KeyName}}</a></td>
    <td>v{{.ActiveVersion}}</td>
    <td>v{{.LatestVersion}}</td>
    <td class="muted">{{formatTime .UpdatedAt}}</td>
    <td>
      <a href="/admin/ns/{{$.Namespace}}/{{.KeyName}}/edit">编辑</a>
    </td>
  </tr>
  {{end}}
</table>
{{if eq (len .Items) 0}}
<p class="muted">本 namespace 暂无 key。点 <a href="/admin/items/new?ns={{.Namespace}}">+ 新增 key</a> 写第一条。</p>
{{end}}

{{else if .Versions}}
{{- /* key_detail：单 key 历史版本 + 当前 active */ -}}
<p class="muted">
  <a href="/admin/ns/{{.Namespace}}">← {{.Namespace}}</a> /
  <b>{{.Key}}</b>
</p>
{{if .Current}}
<h2>当前生效</h2>
<table>
  <tr><th>Version</th><td>v{{.Current.Version}}</td></tr>
  <tr><th>Strategy</th><td><span class="badge b-{{.Current.Strategy | printf "%s" | toLower}}">{{.Current.Strategy}}</span></td></tr>
  <tr><th>Format</th><td>{{.Current.Format}}</td></tr>
  <tr><th>Effective</th><td>{{formatTime .Current.EffectiveAt}}</td></tr>
  <tr><th>Expire</th><td>{{formatTime .Current.ExpireAt}}</td></tr>
  <tr><th>Updated By</th><td>{{.Current.CreatedBy}}</td></tr>
  <tr><th>Reason</th><td>{{.Current.ChangeReason}}</td></tr>
  <tr><th>Value</th><td><pre>{{truncate .Current.Value 1024}}</pre></td></tr>
</table>
<p>
  <a href="/admin/ns/{{.Namespace}}/{{.Key}}/edit">编辑（产新版本）</a>
</p>
{{end}}

{{if .Subscribers}}
<h3>订阅服务</h3>
<p>{{range .Subscribers}}<span class="badge b-full">{{.}}</span> {{end}}</p>
{{end}}

<h2>历史版本</h2>
<table>
  <tr><th>Version</th><th>Strategy</th><th>Created By</th><th>Created At</th><th>Reason</th><th>动作</th></tr>
  {{range .Versions}}
  <tr>
    <td>v{{.Version}}</td>
    <td><span class="badge b-{{.Strategy | printf "%s" | toLower}}">{{.Strategy}}</span></td>
    <td>{{.CreatedBy}}</td>
    <td class="muted">{{formatTime .CreatedAt}}</td>
    <td class="muted">{{truncate .ChangeReason 64}}</td>
    <td>
      {{if $.Current}}{{if ne .Version $.Current.Version}}
      <form method="POST" action="/admin/ns/{{$.Namespace}}/{{$.Key}}/rollback" style="display:inline">
        <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
        <input type="hidden" name="to" value="{{.Version}}">
        <input type="text" name="reason" placeholder="rollback 原因" style="width:120px;display:inline">
        <button type="submit" onclick="return confirm('确认回滚到 v{{.Version}}？')">回滚</button>
      </form>
      <a href="/admin/ns/{{$.Namespace}}/{{$.Key}}/diff?from={{.Version}}&to={{$.Current.Version}}">diff</a>
      {{end}}{{end}}
    </td>
  </tr>
  {{end}}
</table>

{{else if .Lines}}
{{- /* diff 页 */ -}}
<p class="muted">
  <a href="/admin/ns/{{.Namespace}}/{{.Key}}">← {{.Namespace}}/{{.Key}}</a> diff
  v{{.From.Version}} → v{{.To.Version}}
</p>
<table>
  <tr><th>old</th><th>new</th><th>kind</th><th>line</th></tr>
  {{range .Lines}}
  <tr>
    <td class="muted">{{if .OldNum}}{{.OldNum}}{{end}}</td>
    <td class="muted">{{if .NewNum}}{{.NewNum}}{{end}}</td>
    <td><span class="badge b-{{.Kind}}">{{.Kind}}</span></td>
    <td><pre>{{.Text}}</pre></td>
  </tr>
  {{end}}
</table>

{{else if .Services}}
{{- /* new_item：新增配置 */ -}}
<form method="POST" action="/admin/items/new">
  <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
  <div class="row"><label>Namespace</label>
    <select name="namespace">
      {{range .Services}}<option value="{{.}}">{{.}}</option>{{end}}
    </select>
  </div>
  <div class="row"><label>Key</label><input name="key" required placeholder="e.g. rate_limit.rps"></div>
  <div class="row"><label>Format</label>
    <select name="format">
      <option value="json">json</option>
      <option value="plain">plain</option>
      <option value="yaml">yaml</option>
    </select>
  </div>
  <div class="row"><label>Strategy</label>
    <select name="strategy">
      <option value="FULL">FULL（全量）</option>
      <option value="CANARY">CANARY（灰度）</option>
      <option value="TARGETED">TARGETED（白名单）</option>
      <option value="SCHEDULED">SCHEDULED（按时生效）</option>
    </select>
  </div>
  <div class="row"><label>Value</label><textarea name="value" rows="6" required></textarea></div>
  <div class="row"><label>订阅服务（多选）</label>
    {{range .Services}}<label style="display:inline;margin-right:12px"><input type="checkbox" name="subscribers" value="{{.}}" style="width:auto"> {{.}}</label>{{end}}
  </div>
  <div class="row"><label>Reason</label><input name="reason" placeholder="变更说明"></div>
  <div class="row"><button type="submit">创建</button></div>
</form>

{{else if .Current}}
{{- /* edit 页：单 key 的编辑表单（产新 version） */ -}}
<p class="muted">
  <a href="/admin/ns/{{.Namespace}}/{{.Key}}">← {{.Namespace}}/{{.Key}}</a> 编辑
</p>
<form method="POST" action="/admin/ns/{{.Namespace}}/{{.Key}}/put">
  <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
  <div class="row"><label>Format</label>
    <select name="format">
      <option value="json"{{if eq .Current.Format "json"}} selected{{end}}>json</option>
      <option value="plain"{{if eq .Current.Format "plain"}} selected{{end}}>plain</option>
      <option value="yaml"{{if eq .Current.Format "yaml"}} selected{{end}}>yaml</option>
    </select>
  </div>
  <div class="row"><label>Strategy</label>
    <select name="strategy">
      <option value="FULL"{{if eq .Current.Strategy "FULL"}} selected{{end}}>FULL</option>
      <option value="CANARY"{{if eq .Current.Strategy "CANARY"}} selected{{end}}>CANARY</option>
      <option value="TARGETED"{{if eq .Current.Strategy "TARGETED"}} selected{{end}}>TARGETED</option>
      <option value="SCHEDULED"{{if eq .Current.Strategy "SCHEDULED"}} selected{{end}}>SCHEDULED</option>
    </select>
  </div>
  <div class="row"><label>Strategy Spec (JSON)</label><textarea name="strategy_spec" rows="3">{{.Current.StrategySpec}}</textarea></div>
  <div class="row"><label>Effective at (RFC3339, 留空立即)</label><input name="effective_at" type="datetime-local"></div>
  <div class="row"><label>Expire at (留空永不过期)</label><input name="expire_at" type="datetime-local"></div>
  <div class="row"><label>Value</label><textarea name="value" rows="8" required>{{.Current.Value}}</textarea></div>
  <div class="row"><label>Reason</label><input name="reason" required placeholder="变更说明（必填，进 audit log）"></div>
  <div class="row"><button type="submit">提交（产生新版本）</button></div>
</form>

{{else if .Rows}}
{{- /* list_items 全平台 + audit 共用 */ -}}
{{if .Q}}<p class="muted">搜索: <b>{{.Q}}</b>{{if .Subscriber}} (sub={{.Subscriber}}){{end}}</p>{{end}}
<form method="GET" style="margin-bottom:12px">
  <input name="q" value="{{.Q}}" placeholder="按 key/namespace 模糊搜索" style="width:280px;display:inline">
  <input name="sub" value="{{.Subscriber}}" placeholder="订阅服务过滤" style="width:200px;display:inline">
  <button type="submit">搜</button>
</form>
<table>
  {{- /* list_items rows are *ConfigItemView, audit rows are *ConfigAuditEntry — 字段不同分支 */ -}}
  {{range .Rows}}
    {{if .KeyName}}
      {{- /* ConfigItemView */ -}}
      <tr>
        <td><a href="/admin/ns/{{.Namespace}}/{{.KeyName}}">{{.Namespace}} / {{.KeyName}}</a></td>
        <td>v{{.ActiveVersion}}</td>
        <td class="muted">{{formatTime .UpdatedAt}}</td>
      </tr>
    {{else}}
      {{- /* ConfigAuditEntry */ -}}
      <tr>
        <td class="muted">{{formatTime .CreatedAt}}</td>
        <td><span class="badge b-full">{{.Op}}</span></td>
        <td><a href="/admin/ns/{{.Namespace}}/{{.KeyName}}">{{.Namespace}}/{{.KeyName}}</a></td>
        <td>{{.Actor}}</td>
        <td class="muted">{{truncate .ChangeReason 80}}</td>
      </tr>
    {{end}}
  {{end}}
</table>
{{if eq (len .Rows) 0}}<p class="muted">无数据</p>{{end}}

{{else}}
<p class="muted">空页（路由没匹配到模板分支）</p>
{{end}}
{{end}}
`
