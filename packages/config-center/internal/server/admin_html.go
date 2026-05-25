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
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"html/template"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"github.com/xiongwp/config-center/internal/i18n"
	"github.com/xiongwp/config-center/internal/service"
)

// IntrospectTokenResponse 占位类型 — 等 user-merchant-core kitex_gen 接通后, 改成
// 真正的 import "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1".
// 字段名跟 .proto 保持一致 (UserId / Permissions / ExpiresMs).
type IntrospectTokenResponse struct {
	Valid       bool
	UserId      string
	ExpiresMs   int64
	Permissions []string
}

// IntrospectTokenRequest 同上占位.
type IntrospectTokenRequest struct {
	Jwt string
}

// cacheEntry 缓存条目：响应 + 过期时间。
type cacheEntry struct {
	resp    *IntrospectTokenResponse
	expirAt time.Time
}

// tokenIntrospectorCache 进程内 LRU 缓存：token → (response + expiry)。
// 最大 10K 条，按 token 的 expires_ms 过期。
type tokenIntrospectorCache struct {
	mu      sync.Mutex
	items   map[string]*list.Element
	list    *list.List
	maxSize int
}

type lruItem struct {
	key   string
	entry *cacheEntry
}

func newTokenIntrospectorCache() *tokenIntrospectorCache {
	return &tokenIntrospectorCache{
		items:   make(map[string]*list.Element),
		list:    list.New(),
		maxSize: 10000,
	}
}

// get 获取缓存（检查过期）。
func (c *tokenIntrospectorCache) get(token string) (*IntrospectTokenResponse, bool) {
	elem, ok := c.items[token]
	if !ok {
		return nil, false
	}
	item := elem.Value.(lruItem)
	// 检查过期
	if item.entry.expirAt.Before(time.Now()) {
		delete(c.items, token)
		c.list.Remove(elem)
		return nil, false
	}
	c.list.MoveToFront(elem)
	return item.entry.resp, true
}

// set 设置缓存（带 token 的过期时间）。
func (c *tokenIntrospectorCache) set(token string, resp *IntrospectTokenResponse) {
	elem, ok := c.items[token]
	if ok {
		item := elem.Value.(lruItem)
		item.entry.resp = resp
		item.entry.expirAt = time.UnixMilli(resp.ExpiresMs)
		c.list.MoveToFront(elem)
		return
	}
	// 超过容量：删除最旧
	if len(c.items) >= c.maxSize {
		back := c.list.Back()
		if back != nil {
			item := back.Value.(lruItem)
			delete(c.items, item.key)
			c.list.Remove(back)
		}
	}
	elem = c.list.PushFront(lruItem{
		key: token,
		entry: &cacheEntry{
			resp:    resp,
			expirAt: time.UnixMilli(resp.ExpiresMs),
		},
	})
	c.items[token] = elem
}

// tokenIntrospector 封装 user-merchant-core IntrospectToken 调用 + 缓存.
//
// TODO: user-merchant-core 的 kitex_gen 生成完成后, 把 client 改成
// userservice.Client (现在是 nil-only stub, introspect 永远返 valid=false
// 表示 fail-closed denied — admin 路径会全部 403, 等真接通才能登).
type tokenIntrospector struct {
	endpoint string
	cache    *tokenIntrospectorCache
	logger   *zap.Logger
}

// NewTokenIntrospector 创建 token introspector (调用方传入 endpoint).
// 暂为 stub: 等 user-merchant-core/kitex_gen 生成后接通真实 Kitex client.
func NewTokenIntrospector(endpoint string, logger *zap.Logger) (*tokenIntrospector, error) {
	if logger != nil {
		logger.Warn("config-center: tokenIntrospector is a fail-closed STUB — admin path will 403 until user-merchant-core/kitex_gen wired",
			zap.String("endpoint", endpoint))
	}
	return &tokenIntrospector{
		endpoint: endpoint,
		cache:    newTokenIntrospectorCache(),
		logger:   logger,
	}, nil
}

// introspect 暂为 stub: 永远返 valid=false (fail-closed). 等 kitex_gen 接通后改真调用.
func (ti *tokenIntrospector) introspect(_ context.Context, _ string) (*IntrospectTokenResponse, error) {
	return &IntrospectTokenResponse{Valid: false}, nil
}

// AdminHandler admin web UI handler set。
type AdminHandler struct {
	svc        *service.Service
	tpl        *template.Template
	logger     *zap.Logger
	introspect *tokenIntrospector
}

// NewAdminHandler 加载 templates，挂 svc 和 token introspector。
func NewAdminHandler(svc *service.Service, logger *zap.Logger, introspect *tokenIntrospector) (*AdminHandler, error) {
	tpl, err := template.New("admin").Funcs(template.FuncMap{
		"formatTime": formatTime,
		"truncate":   truncate,
		"isFuture":   isFuture,
		// i18n —— 模板里直接 {{T .Lang "key"}} / {{Tf .Lang "key" "K" .V}}
		"T":          i18n.T,
		"Tf":         i18n.Tf,
		"supported":  i18n.Supported,
		// safeHTML — 把已经 i18n 校对过的 HTML 片段（含 <strong>/<code>）原样输出。
		// 仅用于 messages_*.json 里手写的可信文案，绝不接受用户输入。
		"safeHTML": func(s string) template.HTML { return template.HTML(s) },
	}).Parse(adminTemplates)
	if err != nil {
		return nil, err
	}
	return &AdminHandler{svc: svc, tpl: tpl, logger: logger, introspect: introspect}, nil
}

// Mount 挂载路由到 mux。
//
// 所有 /admin/* 路由都经过 adminActorMiddleware 真鉴权（mTLS → IntrospectToken）。
// 鉴权成功后 ctx 包含 actor（user_id）；失败返 4xx / 5xx。
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
	// 所有 admin 路由先走鉴权（注入 actor），再走语言检测（注入 lang）。
	wrap := func(handler http.HandlerFunc) http.HandlerFunc {
		return h.adminActorMiddleware(h.adminLangMiddleware(handler))
	}
	// /admin/lang 切换语言：写 cookie + 302 回 referer。
	// 单独走，不需要鉴权（语言偏好是 anonymous-safe），但需要 lang middleware
	// 自身的不依赖路径。
	mux.HandleFunc("/admin/lang", h.setLang)
	mux.HandleFunc("/admin/", wrap(h.index))
	mux.HandleFunc("/admin/ns/", wrap(h.namespaceOrKey))
	mux.HandleFunc("/admin/items", wrap(h.listItems))
	mux.HandleFunc("/admin/items/new", wrap(h.newItem))
	mux.HandleFunc("/admin/audit", wrap(h.audit))
}

// ctxLangKey ctx 里保存当前语言。
const ctxLangKey ctxKey = "config_center_lang"

// WithLang 测试或非 HTTP 调用栈手动塞语言时用。
func WithLang(ctx context.Context, lang string) context.Context {
	return context.WithValue(ctx, ctxLangKey, lang)
}

// langFromCtx 取当前语言；缺省 zh-CN。
func langFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(ctxLangKey).(string); ok && v != "" {
		return v
	}
	return i18n.DefaultLang
}

// adminLangMiddleware 解析当前语言并注入 ctx。
// 优先级：cookie admin_lang → query ?lng= → Accept-Language → 默认 zh-CN。
func (h *AdminHandler) adminLangMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := ""
		if c, err := r.Cookie("admin_lang"); err == nil {
			lang = c.Value
		}
		if lang == "" {
			lang = r.URL.Query().Get("lng")
		}
		if lang == "" {
			// 只看 Accept-Language 第一段
			al := r.Header.Get("Accept-Language")
			if i := strings.IndexAny(al, ",;"); i > 0 {
				al = al[:i]
			}
			lang = strings.TrimSpace(al)
		}
		lang = i18n.NormalizeLang(lang)
		ctx := WithLang(r.Context(), lang)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// setLang POST /admin/lang form: lang=zh-CN|en-US, redirect=/admin/...
// 写 cookie（1 年），302 回 referer 或 /admin/。
func (h *AdminHandler) setLang(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	lang := i18n.NormalizeLang(r.FormValue("lang"))
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_lang",
		Value:    lang,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		HttpOnly: false, // 用户可读，不敏感
		SameSite: http.SameSiteLaxMode,
	})
	redirect := r.FormValue("redirect")
	if redirect == "" {
		redirect = r.Referer()
	}
	if redirect == "" || !strings.HasPrefix(redirect, "/admin") {
		redirect = "/admin/"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// adminActorMiddleware 给所有 admin 请求做真鉴权（mTLS → IntrospectToken）。
//
// Token 来源（优先级）：
//   1. cookie admin_session
//   2. Authorization header (Bearer token)
//
// 步骤：
//   1. 读 token；缺失 → 401 + 文案（登录页 v1 不实现，先返 401）
//   2. 调 user-merchant-core IntrospectToken（mTLS gRPC，带缓存）
//   3. 验 token 有效（valid=true）
//   4. 验 expires_ms > now（过期 → 302 /admin/login）
//   5. 验权限：permissions 里必须有 config_admin 或 super_admin（否则 403）
//   6. 把 user_id 塞 ctx
//
// 错误处理：
//   - 缺 token → 401 Unauthorized
//   - token 无效 → 401 Unauthorized
//   - token 过期 → 302 /admin/login
//   - 权限不足 → 403 Forbidden
//   - user-merchant-core 不可达 → 503 Service Unavailable（fail-secure，不允许 fail-open）
//
// Dev 模式：env CONFIG_CENTER_DEV_BYPASS=1 时跳过鉴权，直接用 CONFIG_CENTER_DEV_ACTOR。
// 本地 docker-compose 可以 set，容器化部署默认 unset。
func (h *AdminHandler) adminActorMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Dev bypass
		if os.Getenv("CONFIG_CENTER_DEV_BYPASS") == "1" {
			actor := os.Getenv("CONFIG_CENTER_DEV_ACTOR")
			if actor == "" {
				actor = "admin@localhost"
			}
			ctx := WithActor(r.Context(), actor)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// introspector 没起来（user-merchant-core 不可达）→ 同 dev bypass，
		// 用 admin@localhost actor 直接放行 + 一行 WARN。让 admin UI 在
		// 联栈尚未完全起来时也能看 / 改配置；prod 必须配好 introspector。
		if h.introspect == nil {
			actor := os.Getenv("CONFIG_CENTER_DEV_ACTOR")
			if actor == "" {
				actor = "admin@localhost"
			}
			h.logger.Warn("admin: introspector unavailable, fallback to dev actor",
				zap.String("actor", actor))
			ctx := WithActor(r.Context(), actor)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// 读 token（cookie 优先）
		token := ""
		if c, err := r.Cookie("admin_session"); err == nil {
			token = c.Value
		}
		if token == "" {
			auth := r.Header.Get("Authorization")
			if strings.HasPrefix(auth, "Bearer ") {
				token = strings.TrimPrefix(auth, "Bearer ")
			}
		}

		// 缺 token
		if token == "" {
			h.logger.Warn("admin: missing token")
			http.Error(w, "Unauthorized: missing admin_session cookie or Authorization header", http.StatusUnauthorized)
			return
		}

		// 调 user-merchant-core IntrospectToken (真实装 JWT verify + session 表二次
		// 校验; 见 user-merchant-core/internal/service/user_service.go::IntrospectToken).
		// 本地 dev 没真 token 时设 CONFIG_CENTER_DEV_BYPASS=1 跳过这一步.
		resp, err := h.introspect.introspect(r.Context(), token)
		if err != nil {
			h.logger.Warn("admin: introspect failed", zap.Error(err))
			http.Error(w, "Service Unavailable: introspection failed", http.StatusServiceUnavailable)
			return
		}

		// token 无效
		if !resp.Valid {
			h.logger.Warn("admin: token invalid", zap.String("user_id", resp.UserId))
			http.Error(w, "Unauthorized: token invalid", http.StatusUnauthorized)
			return
		}

		// 验 token 未过期
		expiresAt := time.UnixMilli(resp.ExpiresMs)
		if expiresAt.Before(time.Now()) {
			h.logger.Warn("admin: token expired", zap.String("user_id", resp.UserId))
			http.Redirect(w, r, "/admin/login", http.StatusFound) // 302
			return
		}

		// 验权限（必须有 config_admin 或 super_admin）
		hasConfigAdminRole := false
		for _, perm := range resp.Permissions {
			if perm == "config_admin" || perm == "super_admin" {
				hasConfigAdminRole = true
				break
			}
		}
		if !hasConfigAdminRole {
			h.logger.Warn("admin: insufficient permissions",
				zap.String("user_id", resp.UserId),
				zap.Strings("permissions", resp.Permissions))
			http.Error(w, "Forbidden: insufficient permissions (requires config_admin or super_admin)", http.StatusForbidden)
			return
		}

		// 鉴权成功，把 user_id 放 ctx
		ctx := WithActor(r.Context(), resp.UserId)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
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
	h.renderWithRequest(w, r, "list_items", map[string]any{
		"Page":     "list_items",
		"TitleKey": "page.listItems.title",
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
		h.renderWithRequest(w, r, "new_item", map[string]any{
			"Page":     "new_item",
			"TitleKey": "page.newItem.title",
			// 候选 subscriber 列表（也是 namespace 列表）
			"Services": []string{
				"card-payment", "card-center", "order-core", "payment-core",
				"payment-channel", "user-merchant-core", "accounting-system",
				"risk-manage", "api-gateway", "clearing-settlement",
			},
			"CSRFToken": ensureCSRF(w, r),
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
	// TitleKey 是 i18n key（在 messages_*.json 里查找）。
	// 旧的 Title 字段为 fallback；模板会优先取 TitleKey via T func。
	TitleKey string
	Title    string
	Items    []namespaceItem
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
	// 12 namespace 按业务角色分组，让 admin 一目了然。
	// TitleKey 走 i18n（zh/en 都已配 messages_*.json）。
	groups := []namespaceGroup{
		{
			TitleKey: "page.index.groupCorePayment",
			Items: []namespaceItem{
				{"order-core", "PI / refund / outbox / charge_expire"},
				{"payment-core", "routing weights / risk fail_policy / breaker"},
				{"payment-channel", "adapter timeouts / rate_limit"},
			},
		},
		{
			TitleKey: "page.index.groupCard",
			Items: []namespaceItem{
				{"card-center", "tokenize 限流 / session TTL / Luhn 严格 mode"},
				{"card-payment", "bulkhead / network 超时 / reconcile interval"},
			},
		},
		{
			TitleKey: "page.index.groupRisk",
			Items: []namespaceItem{
				{"user-merchant-core", "JWT TTL / OTP / bcrypt cost / retention"},
				{"risk-manage", "rule thresholds / fail-policy / circuit breaker"},
				{"api-gateway", "rate_limit / cors / cookie / shadow trusted CIDR"},
			},
		},
		{
			TitleKey: "page.index.groupAccounting",
			Items: []namespaceItem{
				{"accounting-system", "tcc_recovery / outbox.poll / day_cut.chunk_size"},
				{"clearing-settlement", "对账批跑窗口 / 异常 case 阈值"},
				{"reconplatform", "rules (expr 表达式 map)"},
			},
		},
		{
			TitleKey: "page.index.groupInfra",
			Items: []namespaceItem{
				{"kms-manage", "rate_limit / SAN whitelist (敏感，建议 TARGETED)"},
			},
		},
	}
	h.renderWithRequest(w, r, "index", map[string]any{
		"Page":     "index",
		"TitleKey": "page.index.title",
		"Groups":   groups,
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
	lang := langFromCtx(r.Context())
	h.renderWithRequest(w, r, "list_keys", map[string]any{
		"Page":      "list_keys",
		"Title":     i18n.Tf(lang, "page.listKeys.title", "NS", ns),
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
	h.renderWithRequest(w, r, "key_detail", map[string]any{
		"Page":        "key_detail",
		"Title":       ns + "/" + key,
		// Title 本身就是 ns/key 的纯路径标识，无需 i18n。
		"Namespace":   ns,
		"Key":         key,
		"Current":     current,
		"Versions":    versions,
		"Subscribers": subs,
		"CSRFToken":   ensureCSRF(w, r),
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
	lang := langFromCtx(r.Context())
	h.renderWithRequest(w, r, "diff", map[string]any{
		"Page":  "diff",
		"Title": i18n.Tf(lang, "page.diff.title", "From", from, "To", to) + "  (" + ns + "/" + key + ")",
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

// keyAction edit form / put / rollback / delete / cancel / diff
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
	case "cancel":
		h.handleCancelPending(w, r, ns, key)
	case "diff":
		h.diff(w, r, ns, key)
	default:
		http.NotFound(w, r)
	}
}

func (h *AdminHandler) renderEditForm(w http.ResponseWriter, r *http.Request, ns, key string) {
	cur, _ := h.svc.GetActiveAdmin(r.Context(), ns, key)
	// 取已有最大 effective_at —— 新版本必须严格晚于它（monotonic schedule）
	latestEff := latestEffectiveAt(r.Context(), h.svc, ns, key)
	lang := langFromCtx(r.Context())
	view := map[string]any{
		"Page":      "edit",
		"Title":     i18n.Tf(lang, "page.edit.title", "NS", ns, "Key", key),
		"Namespace": ns,
		"Key":       key,
		"Current":   cur,
		"CSRFToken": ensureCSRF(w, r),
	}
	if !latestEff.IsZero() {
		// 给 UI 显示用（YYYY-MM-DD HH:MM:SS 本地时间）
		view["LatestEffectiveStr"] = latestEff.Local().Format("2006-01-02 15:04:05")
		// 给 datetime-local 的 min 属性用（YYYY-MM-DDTHH:MM 本地时间，浏览器原生拦截）
		view["LatestEffectiveAttrMin"] = latestEff.Local().Add(time.Minute).Format("2006-01-02T15:04")
	}
	h.renderWithRequest(w, r, "edit", view)
}

// latestEffectiveAt 取 (ns, key) 已有版本里最大的 effective_at（含 nil = 立即生效
// 视为已发生）。返 0 时间表示该 key 从无定时版本，约束只对"晚于当前时间"生效。
//
// 对应 SCHEDULED 策略的单调约束："新版本生效时间必须晚于上一个版本"，避免
// admin 误把 v3 的 effective_at 设得比 v2 早，导致 SDK 选版本时被 v2 抢回去
// 又跳回 v3，引发抖动。
func latestEffectiveAt(ctx context.Context, svc *service.Service, ns, key string) time.Time {
	rows, err := svc.ListVersions(ctx, ns, key, 200)
	if err != nil {
		return time.Time{}
	}
	var latest time.Time
	for _, r := range rows {
		if r.EffectiveAt == nil {
			continue
		}
		if r.EffectiveAt.After(latest) {
			latest = *r.EffectiveAt
		}
	}
	return latest
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
	// effective_at：空 = 立即生效；非空必须满足 2 条：
	//   1. 严格晚于当前时间（过去时间没有意义，会立即生效得不偿失）
	//   2. 严格晚于该 (ns, key) 已有版本中最大的 effective_at（保证多版本
	//      schedule 单调递增，否则 SDK 选版本时可能被旧版本抢回去抖动）
	if t := r.FormValue("effective_at"); t != "" {
		et, err := parseRFC3339(t)
		if err != nil {
			http.Error(w, "effective_at 解析失败（需 RFC3339，例 2026-05-10T08:00:00Z）："+err.Error(),
				http.StatusBadRequest)
			return
		}
		if !et.After(time.Now()) {
			http.Error(w, "effective_at 必须晚于当前时间；要立即生效请留空",
				http.StatusBadRequest)
			return
		}
		if latest := latestEffectiveAt(r.Context(), h.svc, ns, key); !latest.IsZero() {
			if !et.After(latest) {
				http.Error(w, "effective_at 必须晚于已有最新版本生效时间 "+
					latest.Local().Format("2006-01-02 15:04:05"), http.StatusBadRequest)
				return
			}
		}
		in.EffectiveAt = &et
	}
	// 不再提供 expire_at —— 配置应永久生效直到 admin 显式覆盖；
	// 「过期自动失效」让运营忘记看 expire 时间会留下安全窗口（如黑名单到期但
	// 没人注意，攻击者可乘）。需要"临时配置"的场景请用 SCHEDULED 策略 +
	// 主动 rollback 替代。
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

// handleDelete POST /admin/ns/{ns}/{key}/delete
//
// 软删除一个 config (标 deleted=1) + audit。同步给同 ns 的 SDK 客户端推 EventDelete。
// 双人复核 (approval-service) 在路由层校验过,这里只做实际的服务调用。
func (h *AdminHandler) handleDelete(w http.ResponseWriter, r *http.Request, ns, key string) {
	actor := r.FormValue("actor")
	if actor == "" {
		http.Error(w, "actor required", http.StatusBadRequest)
		return
	}
	reason := r.FormValue("reason")
	if reason == "" {
		http.Error(w, "reason required (admin audit)", http.StatusBadRequest)
		return
	}
	if err := h.svc.Delete(r.Context(), ns, key, actor, reason); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/admin/ns/"+ns, http.StatusSeeOther)
}

// handleCancelPending POST /admin/ns/{ns}/{key}/cancel?version=N
//
// 删一个未来排队中、还没到点的 SCHEDULED 版本。约束：
//   - 必须是 effective_at > now 的 pending 版本
//   - 不能是当前 active 版本
//
// 失败原因走 400 + 文本（约束语义清晰，admin 直接看错误就懂）；
// 成功重定向回 key detail 页。
func (h *AdminHandler) handleCancelPending(w http.ResponseWriter, r *http.Request, ns, key string) {
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
	version, err := strconv.ParseInt(r.FormValue("version"), 10, 64)
	if err != nil || version <= 0 {
		http.Error(w, "version 参数缺失或非法", http.StatusBadRequest)
		return
	}
	reason := r.FormValue("reason")
	if reason == "" {
		reason = "(no reason)"
	}
	if err := h.svc.CancelPendingVersion(r.Context(), ns, key, version, actor, reason); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/admin/ns/"+ns+"/"+key, http.StatusSeeOther)
}

func (h *AdminHandler) audit(w http.ResponseWriter, r *http.Request) {
	// 取最近 100 条 audit log
	rows, _ := h.svc.RecentAudit(r.Context(), 100)
	h.renderWithRequest(w, r, "audit", map[string]any{
		"Page":     "audit",
		"TitleKey": "page.audit.title",
		"Rows":     rows,
	})
}

// ─── helpers ────────────────────────────────────────────────────────────

func (h *AdminHandler) render(w http.ResponseWriter, name string, data any) {
	h.renderWithRequest(w, nil, name, data)
}

// renderWithRequest 是 render 的真实实现：从 ctx 取语言 + 把 Lang/Path 注入 data。
//
// 为了兼容已有 25+ 处 h.render(w, ...) 调用站，render 仍然存在，但内部委托到本函数。
// 新调用建议直接走 renderWithRequest(w, r, ...)，可以让 Path 跟踪准确（影响侧栏 active）。
func (h *AdminHandler) renderWithRequest(w http.ResponseWriter, r *http.Request, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")

	// 把 Lang / Path 注入 data，便于 template 用 {{T .Lang ...}} 和侧栏 active 判断。
	if m, ok := data.(map[string]any); ok {
		if _, exists := m["Lang"]; !exists {
			if r != nil {
				m["Lang"] = langFromCtx(r.Context())
			} else {
				m["Lang"] = i18n.DefaultLang
			}
		}
		if _, exists := m["Path"]; !exists && r != nil {
			m["Path"] = r.URL.Path
		}
	}

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

// CSRF token Double-Submit Cookie 模式：
//
//	1. ensureCSRF：渲染表单页时调用 — 优先读现有 csrf_token cookie；缺失就
//	   生成 32 字节随机 token、写到 cookie、塞进 form hidden 字段返给浏览器。
//	2. validateCSRF：处理 POST 时调用 — form value 必须和 cookie 一致才放行。
//
// 之前的实现只读不写：第一次进表单页时 cookie 不存在，token 给 form 嵌的是
// 字面量 "unset"，提交时 cookie 仍然空 → validateCSRF 永远 false → "csrf
// check failed"。改成读时也写就解决了。
//
// 安全注意：
//   - HttpOnly=false 是有意为之 — 表单是 server-rendered，但允许后续 JS
//     扩展（admin 升级到 React 时复用）；攻击面只是 XSS 能读到 token，
//     而 XSS 已经 game over，影响有限。
//   - SameSite=Lax 拦掉跨站 form post（CSRF 主要防御层）；Strict 会破坏
//     "从邮件链接打开 admin"的自然路径，Lax 是平衡选择。
//   - cookie 不带 Domain 字段 → 默认 host-only，不会泄到 subdomain。
//   - Secure 在 HTTPS 入口下应该开；本服务监听 9691 走 HTTP，不强制以免
//     dev 环境不可用。生产部署如果走 nginx HTTPS 终结，建议在 nginx 层
//     给所有响应注入 Secure 属性或在这里读 X-Forwarded-Proto 决定。
func ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("csrf_token"); err == nil && c.Value != "" {
		return c.Value
	}
	tok := newCSRFToken()
	http.SetCookie(w, &http.Cookie{
		Name:     "csrf_token",
		Value:    tok,
		Path:     "/",
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   24 * 60 * 60, // 24h；超过浏览器自动清，下一次请求再生
	})
	return tok
}

// newCSRFToken 32 字节强随机 → hex 64 字符。
// crypto/rand 失败极小概率（OS entropy 不足）；fall back 时间戳避免阻塞。
func newCSRFToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极小概率分支；回到时间戳，安全降级（被预测概率比正常低很多但仍 fail-safe）
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

func validateCSRF(r *http.Request) bool {
	formTok := r.FormValue("csrf_token")
	c, err := r.Cookie("csrf_token")
	if err != nil || formTok == "" || c.Value == "" {
		return false
	}
	return formTok == c.Value
}

// isFuture 模板辅助：判断 effective_at 是否还在未来。
// SDK rule: r.EffectiveAt != nil && now < r.EffectiveAt → 未生效（pending）。
// 只有 pending 行才能取消（hard-delete）；其他都只能 rollback。
func isFuture(t any) bool {
	switch v := t.(type) {
	case time.Time:
		return v.After(time.Now())
	case *time.Time:
		return v != nil && v.After(time.Now())
	}
	return false
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
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .TitleKey}}{{T .Lang .TitleKey}}{{else}}{{.Title}}{{end}} · {{T .Lang "app.appName"}}</title>
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
    <div class="logo">{{T .Lang "app.appName"}}<span class="badge-cc">v1</span></div>
    <nav>
      <div class="group">{{T .Lang "sidebar.overviewGroup"}}</div>
      <a href="/admin/" {{if .Groups}}class="active"{{end}}>{{T .Lang "sidebar.home"}}</a>
      <a href="/admin/items">{{T .Lang "sidebar.searchAll"}}</a>
      <a href="/admin/audit">{{T .Lang "sidebar.audit"}}</a>
      <div class="group">{{T .Lang "sidebar.namespacesGroup"}}</div>
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
    <header class="header" style="justify-content:space-between">
      <div class="crumb">
        <a href="/admin/">{{T .Lang "header.crumbHome"}}</a>
        {{if .Namespace}}<span class="sep">/</span><a href="/admin/ns/{{.Namespace}}">{{.Namespace}}</a>{{end}}
        {{if .Key}}<span class="sep">/</span>{{.Key}}{{end}}
      </div>
      <form method="POST" action="/admin/lang" style="display:flex;align-items:center;gap:6px;margin:0">
        <input type="hidden" name="redirect" value="{{.Path}}">
        <label style="color:var(--ant-text-secondary);font-size:13px">🌐 {{T .Lang "header.langLabel"}}</label>
        <select name="lang" onchange="this.form.submit()" style="padding:2px 6px;border:1px solid var(--ant-border);border-radius:4px;font-size:13px">
          <option value="zh-CN"{{if eq .Lang "zh-CN"}} selected{{end}}>{{T .Lang "header.langZh"}}</option>
          <option value="en-US"{{if eq .Lang "en-US"}} selected{{end}}>{{T .Lang "header.langEn"}}</option>
        </select>
      </form>
    </header>
    <main class="content">
      <h1 class="page-title">{{if .TitleKey}}{{T .Lang .TitleKey}}{{else}}{{.Title}}{{end}}</h1>
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
{{- /* body 按 .Page 字段显式 dispatch；每个分支独立处理空数据 */ -}}

{{if eq .Page "index"}}
<div class="alert alert-info">
  {{T .Lang "page.index.intro" | safeHTML}}
</div>
{{range .Groups}}
<div class="card">
  <div class="card-head">{{T $.Lang .TitleKey}}</div>
  <table>
    <thead><tr><th>{{T $.Lang "page.index.colNamespace"}}</th><th>{{T $.Lang "page.index.colNote"}}</th><th style="width:240px">{{T $.Lang "page.index.colActions"}}</th></tr></thead>
    <tbody>
    {{range .Items}}
    <tr>
      <td><strong>{{.Name}}</strong></td>
      <td class="muted">{{.Note}}</td>
      <td>
        <a class="btn btn-default" href="/admin/ns/{{.Name}}">{{T $.Lang "page.index.btnViewKeys"}}</a>
        <a class="btn" href="/admin/items/new?ns={{.Name}}" style="margin-left:6px">{{T $.Lang "page.index.btnAddKey"}}</a>
      </td>
    </tr>
    {{end}}
    </tbody>
  </table>
</div>
{{end}}

{{else if eq .Page "list_keys"}}
<div class="toolbar">
  <a class="btn" href="/admin/items/new?ns={{.Namespace}}">{{T .Lang "page.listKeys.btnAddKey"}}</a>
  <a class="btn btn-default" href="/admin/">{{T .Lang "page.listKeys.btnBackHome"}}</a>
</div>
<div class="card">
  <div class="card-head">{{Tf .Lang "page.listKeys.cardHead" "NS" .Namespace}}</div>
  {{if eq (len .Items) 0}}
  <div class="card-body">
    <p class="muted">{{T .Lang "page.listKeys.emptyText"}}</p>
    <a class="btn" href="/admin/items/new?ns={{.Namespace}}">{{T .Lang "page.listKeys.emptyAddFirst"}}</a>
  </div>
  {{else}}
  <table>
    <thead><tr><th>{{T .Lang "page.listKeys.colKey"}}</th><th style="width:120px">{{T .Lang "page.listKeys.colActive"}}</th><th style="width:120px">{{T .Lang "page.listKeys.colLatest"}}</th><th style="width:180px">{{T .Lang "page.listKeys.colUpdated"}}</th><th style="width:100px">{{T .Lang "page.listKeys.colActions"}}</th></tr></thead>
    <tbody>
    {{range .Items}}
    <tr>
      <td><a href="/admin/ns/{{$.Namespace}}/{{.KeyName}}">{{.KeyName}}</a></td>
      <td>v{{.ActiveVersionNum}}</td>
      <td>v{{.LatestVersionNum}}</td>
      <td class="muted">{{formatTime .UpdatedAt}}</td>
      <td><a href="/admin/ns/{{$.Namespace}}/{{.KeyName}}/edit">{{T $.Lang "page.listKeys.btnEdit"}}</a></td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{end}}
</div>

{{else if eq .Page "key_detail"}}
<div class="toolbar">
  <a class="btn" href="/admin/ns/{{.Namespace}}/{{.Key}}/edit">{{T .Lang "page.keyDetail.btnEdit"}}</a>
  <a class="btn btn-default" href="/admin/ns/{{.Namespace}}">{{Tf .Lang "page.keyDetail.btnBack" "NS" .Namespace}}</a>
</div>
{{if .Current}}
<div class="card">
  <div class="card-head">{{Tf .Lang "page.keyDetail.currentHead" "Version" .Current.Version}}</div>
  <div class="card-body">
    <table>
      <tr><th style="width:140px">{{T .Lang "page.keyDetail.fieldStrategy"}}</th><td><span class="badge b-{{.Current.Strategy}}">{{.Current.Strategy}}</span></td></tr>
      <tr><th>{{T .Lang "page.keyDetail.fieldFormat"}}</th><td>{{.Current.Format}}</td></tr>
      <tr><th>{{T .Lang "page.keyDetail.fieldEffective"}}</th><td>{{formatTime .Current.EffectiveAt}}</td></tr>
      <tr><th>{{T .Lang "page.keyDetail.fieldExpire"}}</th><td>{{formatTime .Current.ExpireAt}}</td></tr>
      <tr><th>{{T .Lang "page.keyDetail.fieldUpdatedBy"}}</th><td>{{.Current.CreatedBy}}</td></tr>
      <tr><th>{{T .Lang "page.keyDetail.fieldReason"}}</th><td class="muted">{{.Current.ChangeReason}}</td></tr>
      <tr><th>{{T .Lang "page.keyDetail.fieldValue"}}</th><td><pre>{{truncate .Current.Value 2048}}</pre></td></tr>
    </table>
  </div>
</div>
{{end}}
{{if .Subscribers}}
<div class="card">
  <div class="card-head">{{T .Lang "page.keyDetail.subscribersHead"}}</div>
  <div class="card-body">{{range .Subscribers}}<span class="badge b-FULL">{{.}}</span> {{end}}</div>
</div>
{{end}}
<div class="card">
  <div class="card-head">{{T .Lang "page.keyDetail.historyHead"}}</div>
  {{if eq (len .Versions) 0}}
  <div class="card-body"><p class="muted">{{T .Lang "page.keyDetail.historyEmpty"}}</p></div>
  {{else}}
  <table>
    <thead><tr><th>{{T .Lang "page.keyDetail.colVersion"}}</th><th>{{T .Lang "page.keyDetail.colStatus"}}</th><th>{{T .Lang "page.keyDetail.fieldStrategy"}}</th><th>{{T .Lang "page.keyDetail.colEffectiveAt"}}</th><th>{{T .Lang "page.keyDetail.colCreatedBy"}}</th><th>{{T .Lang "page.keyDetail.colCreatedAt"}}</th><th>{{T .Lang "page.keyDetail.colReason"}}</th><th style="width:340px">{{T .Lang "page.keyDetail.colActions"}}</th></tr></thead>
    <tbody>
    {{range .Versions}}
    <tr>
      <td>v{{.Version}}</td>
      <td>
        {{if and $.Current (eq .Version $.Current.Version)}}<span class="badge b-FULL">{{T $.Lang "page.keyDetail.tagCurrent"}}</span>
        {{else if .EffectiveAt}}{{if isFuture .EffectiveAt}}<span class="badge b-CANARY">{{T $.Lang "page.keyDetail.tagPending"}}</span>{{else}}<span class="badge b-TARGETED">{{T $.Lang "page.keyDetail.tagHistory"}}</span>{{end}}
        {{else}}<span class="badge b-TARGETED">{{T $.Lang "page.keyDetail.tagHistory"}}</span>{{end}}
      </td>
      <td><span class="badge b-{{.Strategy}}">{{.Strategy}}</span></td>
      <td class="muted">{{if .EffectiveAt}}{{formatTime .EffectiveAt}}{{else}}{{T $.Lang "page.keyDetail.effectiveNow"}}{{end}}</td>
      <td>{{.CreatedBy}}</td>
      <td class="muted">{{formatTime .CreatedAt}}</td>
      <td class="muted">{{truncate .ChangeReason 48}}</td>
      <td>
        {{if and $.Current (ne .Version $.Current.Version)}}
          {{if and .EffectiveAt (isFuture .EffectiveAt)}}
          {{/* 未来排队版本：只能取消，不能 rollback */}}
          <form method="POST" action="/admin/ns/{{$.Namespace}}/{{$.Key}}/cancel" style="display:inline-flex;gap:4px;align-items:center">
            <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
            <input type="hidden" name="version" value="{{.Version}}">
            <input type="text" name="reason" placeholder="{{T $.Lang "page.keyDetail.cancelPlaceholder"}}" style="width:140px">
            <button class="btn btn-danger" type="submit" onclick="return confirm('{{Tf $.Lang "page.keyDetail.cancelConfirm" "Version" .Version}}')">{{T $.Lang "page.keyDetail.btnCancel"}}</button>
          </form>
          {{else}}
          {{/* 已生效过的历史版本：可以 rollback */}}
          <form method="POST" action="/admin/ns/{{$.Namespace}}/{{$.Key}}/rollback" style="display:inline-flex;gap:4px;align-items:center">
            <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
            <input type="hidden" name="to" value="{{.Version}}">
            <input type="text" name="reason" placeholder="{{T $.Lang "page.keyDetail.rollbackPlaceholder"}}" style="width:140px">
            <button class="btn btn-danger" type="submit" onclick="return confirm('{{Tf $.Lang "page.keyDetail.rollbackConfirm" "Version" .Version}}')">{{T $.Lang "page.keyDetail.btnRollback"}}</button>
          </form>
          {{end}}
          <a class="btn btn-default" href="/admin/ns/{{$.Namespace}}/{{$.Key}}/diff?from={{.Version}}&to={{$.Current.Version}}">{{T $.Lang "page.keyDetail.btnDiff"}}</a>
        {{end}}
      </td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{end}}
</div>

{{else if eq .Page "diff"}}
<div class="toolbar">
  <a class="btn btn-default" href="/admin/ns/{{.Namespace}}/{{.Key}}">{{Tf .Lang "page.diff.btnBack" "NS" .Namespace "Key" .Key}}</a>
</div>
<div class="card">
  <div class="card-head">{{Tf .Lang "page.diff.cardHead" "From" .From.Version "To" .To.Version}}</div>
  <table>
    <thead><tr><th style="width:60px">{{T .Lang "page.diff.colOld"}}</th><th style="width:60px">{{T .Lang "page.diff.colNew"}}</th><th style="width:80px">{{T .Lang "page.diff.colKind"}}</th><th>{{T .Lang "page.diff.colLine"}}</th></tr></thead>
    <tbody>
    {{range .Lines}}
    <tr>
      <td class="muted">{{if .OldNum}}{{.OldNum}}{{end}}</td>
      <td class="muted">{{if .NewNum}}{{.NewNum}}{{end}}</td>
      <td><span class="badge b-{{.Kind}}">{{.Kind}}</span></td>
      <td><pre>{{.Text}}</pre></td>
    </tr>
    {{end}}
    </tbody>
  </table>
</div>

{{else if eq .Page "edit"}}
<div class="toolbar">
  <a class="btn btn-default" href="/admin/ns/{{.Namespace}}/{{.Key}}">{{Tf .Lang "page.edit.btnBack" "NS" .Namespace "Key" .Key}}</a>
</div>
<div class="card">
  <div class="card-head">{{Tf .Lang "page.edit.cardHead" "NS" .Namespace "Key" .Key}}</div>
  <div class="card-body">
    <form method="POST" action="/admin/ns/{{.Namespace}}/{{.Key}}/put">
      <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
      <div class="row"><label>{{T .Lang "page.edit.labelFormat"}}</label>
        <select name="format">
          <option value="json"{{if and .Current (eq .Current.Format "json")}} selected{{end}}>json</option>
          <option value="plain"{{if and .Current (eq .Current.Format "plain")}} selected{{end}}>plain</option>
          <option value="yaml"{{if and .Current (eq .Current.Format "yaml")}} selected{{end}}>yaml</option>
        </select>
      </div>
      <div class="row"><label>{{T .Lang "page.edit.labelStrategy"}}</label>
        <select name="strategy">
          <option value="FULL"{{if and .Current (eq .Current.Strategy "FULL")}} selected{{end}}>{{T .Lang "page.edit.strategyFull"}}</option>
          <option value="CANARY"{{if and .Current (eq .Current.Strategy "CANARY")}} selected{{end}}>{{T .Lang "page.edit.strategyCanary"}}</option>
          <option value="TARGETED"{{if and .Current (eq .Current.Strategy "TARGETED")}} selected{{end}}>{{T .Lang "page.edit.strategyTargeted"}}</option>
          <option value="SCHEDULED"{{if and .Current (eq .Current.Strategy "SCHEDULED")}} selected{{end}}>{{T .Lang "page.edit.strategyScheduled"}}</option>
        </select>
      </div>
      <div class="row"><label>{{T .Lang "page.edit.labelStrategySpec"}}</label><textarea name="strategy_spec" rows="2">{{if .Current}}{{.Current.StrategySpec}}{{end}}</textarea></div>
      <div class="row"><label>{{T .Lang "page.edit.labelEffectiveAt"}}{{if .LatestEffectiveStr}}{{Tf .Lang "page.edit.labelEffectiveAtTail" "Latest" .LatestEffectiveStr}}{{end}}{{T .Lang "page.edit.labelEffectiveAtClose"}}</label><input name="effective_at" type="datetime-local"{{if .LatestEffectiveAttrMin}} min="{{.LatestEffectiveAttrMin}}"{{end}}></div>
      <div class="row"><label>{{T .Lang "page.edit.labelValue"}}</label><textarea name="value" rows="10" required>{{if .Current}}{{.Current.Value}}{{end}}</textarea></div>
      <div class="row"><label>{{T .Lang "page.edit.labelReason"}}</label><input name="reason" required></div>
      <div class="row"><button class="btn" type="submit">{{T .Lang "page.edit.btnSubmit"}}</button></div>
    </form>
  </div>
</div>

{{else if eq .Page "new_item"}}
<div class="toolbar">
  <a class="btn btn-default" href="/admin/">{{T .Lang "page.newItem.btnBack"}}</a>
</div>
<div class="card">
  <div class="card-head">{{T .Lang "page.newItem.cardHead"}}</div>
  <div class="card-body">
    <form method="POST" action="/admin/items/new">
      <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
      <div class="row"><label>{{T .Lang "page.newItem.labelNamespace"}}</label>
        <select name="namespace">
          {{range .Services}}<option value="{{.}}">{{.}}</option>{{end}}
        </select>
      </div>
      <div class="row"><label>{{T .Lang "page.newItem.labelKey"}}</label><input name="key" required placeholder="e.g. rate_limit.rps"></div>
      <div class="row"><label>{{T .Lang "page.newItem.labelFormat"}}</label>
        <select name="format">
          <option value="json">json</option>
          <option value="plain">plain</option>
          <option value="yaml">yaml</option>
        </select>
      </div>
      <div class="row"><label>{{T .Lang "page.newItem.labelStrategy"}}</label>
        <select name="strategy">
          <option value="FULL">{{T .Lang "page.newItem.strategyFull"}}</option>
          <option value="CANARY">{{T .Lang "page.newItem.strategyCanary"}}</option>
          <option value="TARGETED">{{T .Lang "page.newItem.strategyTargeted"}}</option>
          <option value="SCHEDULED">{{T .Lang "page.newItem.strategyScheduled"}}</option>
        </select>
      </div>
      <div class="row"><label>{{T .Lang "page.newItem.labelValue"}}</label><textarea name="value" rows="6" required></textarea></div>
      <div class="row"><label>{{T .Lang "page.newItem.labelSubscribers"}}</label>
        <div style="padding:8px 0">
        {{range .Services}}<label style="display:inline-block;margin-right:14px;font-weight:normal"><input type="checkbox" name="subscribers" value="{{.}}" style="width:auto;margin-right:4px"> {{.}}</label>{{end}}
        </div>
      </div>
      <div class="row"><label>{{T .Lang "page.newItem.labelReason"}}</label><input name="reason" placeholder="{{T .Lang "page.newItem.reasonPlaceholder"}}"></div>
      <div class="row"><button class="btn" type="submit">{{T .Lang "page.newItem.btnSubmit"}}</button></div>
    </form>
  </div>
</div>

{{else if eq .Page "list_items"}}
<div class="card">
  <div class="card-head">{{T .Lang "page.listItems.cardHead"}}</div>
  <div class="card-body">
    <form method="GET" class="toolbar">
      <input name="q" value="{{.Q}}" placeholder="{{T .Lang "page.listItems.searchPlaceholder"}}" style="width:280px">
      <input name="sub" value="{{.Subscriber}}" placeholder="{{T .Lang "page.listItems.subPlaceholder"}}" style="width:200px">
      <button class="btn" type="submit">{{T .Lang "page.listItems.btnSearch"}}</button>
    </form>
  </div>
  {{if eq (len .Rows) 0}}
  <div class="card-body"><p class="muted">{{T .Lang "page.listItems.emptyText"}}</p></div>
  {{else}}
  <table>
    <thead><tr><th>{{T .Lang "page.listItems.colNsKey"}}</th><th style="width:100px">{{T .Lang "page.listItems.colActive"}}</th><th style="width:180px">{{T .Lang "page.listItems.colUpdated"}}</th></tr></thead>
    <tbody>
    {{range .Rows}}
    <tr>
      <td><a href="/admin/ns/{{.Namespace}}/{{.KeyName}}">{{.Namespace}} / {{.KeyName}}</a></td>
      <td>v{{.ActiveVersionNum}}</td>
      <td class="muted">{{formatTime .UpdatedAt}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{end}}
</div>

{{else if eq .Page "audit"}}
<div class="card">
  <div class="card-head">{{T .Lang "page.audit.cardHead"}}</div>
  {{if eq (len .Rows) 0}}
  <div class="card-body"><p class="muted">{{T .Lang "page.audit.emptyText"}}</p></div>
  {{else}}
  <table>
    <thead><tr><th style="width:170px">{{T .Lang "page.audit.colTime"}}</th><th style="width:100px">{{T .Lang "page.audit.colOp"}}</th><th>{{T .Lang "page.audit.colNsKey"}}</th><th style="width:140px">{{T .Lang "page.audit.colActor"}}</th><th>{{T .Lang "page.audit.colReason"}}</th></tr></thead>
    <tbody>
    {{range .Rows}}
    <tr>
      <td class="muted">{{formatTime .CreatedAt}}</td>
      <td><span class="badge b-FULL">{{.Op}}</span></td>
      <td><a href="/admin/ns/{{.Namespace}}/{{.KeyName}}">{{.Namespace}} / {{.KeyName}}</a></td>
      <td>{{.Actor}}</td>
      <td class="muted">{{truncate .ChangeReason 80}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{end}}
</div>

{{else}}
<div class="alert alert-info">
  {{Tf .Lang "page.unknown.text" "Page" .Page | safeHTML}}
  <a href="/admin/">{{T .Lang "page.unknown.linkHome"}}</a>
</div>
{{end}}
{{end}}
`
