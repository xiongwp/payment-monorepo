// moneyflow.go — MF-2: Money Flow Designer 嵌入 admin BFF.
//
// 三件事:
//   1. Reverse proxy /api/moneyflow/*  →  split-payment 服务 (/api/moneyflow/*)
//      (admin BFF 不直接管 graph 业务, 透传给 split-payment cmd/server)
//   2. 静态 serve frontend/moneyflow-designer.html → /moneyflow
//   3. /moneyflow/health 简易探活, 反映 split-payment 是否可达
//
// 设计取舍:
//   - 走代理而不是 gRPC client: split-payment 提供的是 JSON HTTP, 直接代理省得再写
//     一遍 schema 转换; 也便于 dev 在 admin 端开关.
//   - admin 的 auth / audit / rate-limit middleware 仍生效 (mux Subrouter 注册).
package handler

import (
	_ "embed"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"
)

// designerHTML — MF-2 frontend designer 直接 embed 进二进制 (避免外挂文件/卷依赖).
//
// 单一源在 packages/payment-admin-web/frontend/moneyflow-designer.html;
// 本 assets/ 目录是它的 build-time 副本 (go:embed 不支持 ../).
// 每次改 frontend HTML 后用以下命令同步:
//
//	cp ../frontend/moneyflow-designer.html assets/moneyflow-designer.html
//
// 或 backend Makefile / CI step 自动 sync.
//
//go:embed assets/moneyflow-designer.html
var designerHTML []byte

// resourcesHTML — SP-13 Stripe-style 资源管理页 (Accounts / Transfers / Fees / Payouts).
//
//go:embed assets/moneyflow-resources.html
var resourcesHTML []byte

// designerV2HTML — SP-3D React Flow 重写的 designer.
//
//go:embed assets/moneyflow-designer-v2.html
var designerV2HTML []byte

// rulesHTML — SP-AC-5 TransactionRule 管理页.
//
//go:embed assets/moneyflow-rules.html
var rulesHTML []byte

// MoneyflowHandler 注入 split-payment upstream.
type MoneyflowHandler struct {
	proxy    *httputil.ReverseProxy
	upstream string
}

// NewMoneyflowHandler 构造.
//
// upstream 走 env SPLIT_PAYMENT_HTTP_ADDR (默认 http://split-payment:8098).
// designer HTML 走 go:embed (designerHTML 字节切片), 无需外挂.
func NewMoneyflowHandler() *MoneyflowHandler {
	up := envOr("SPLIT_PAYMENT_HTTP_ADDR", "http://split-payment:8098")
	u, err := url.Parse(up)
	if err != nil || u.Scheme == "" {
		u, _ = url.Parse("http://localhost:8098")
		up = u.String()
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "split-payment upstream unreachable: "+err.Error(),
			http.StatusBadGateway)
	}
	rp.FlushInterval = 100 * time.Millisecond // dry-run / runs/search 响应可能慢
	return &MoneyflowHandler{proxy: rp, upstream: up}
}

// Proxy mux 注册: /api/moneyflow/* → split-payment.
//
// 已挂的 admin auth / audit / rate-limit middleware 仍然先跑过, 代理在最末端.
func (h *MoneyflowHandler) Proxy(w http.ResponseWriter, r *http.Request) {
	// 透传 path: gorilla mux 把 /api/moneyflow/xxx 整段交给 handler,
	// ReverseProxy 默认会 rewrite Host, 这里不动 path.
	h.proxy.ServeHTTP(w, r)
}

// Designer GET /moneyflow — serve embedded designer HTML.
//
// 二进制内嵌, 零文件系统依赖. 改 HTML 要同步 frontend/ → assets/ 后重 build.
func (h *MoneyflowHandler) Designer(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	_, _ = w.Write(designerHTML)
}

// Resources GET /moneyflow/resources — SP-13 资源管理页.
func (h *MoneyflowHandler) Resources(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	_, _ = w.Write(resourcesHTML)
}

// DesignerV2 GET /moneyflow/v2 — SP-3D React Flow designer.
func (h *MoneyflowHandler) DesignerV2(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	_, _ = w.Write(designerV2HTML)
}

// Rules GET /moneyflow/rules — SP-AC-5 TransactionRule 管理页.
func (h *MoneyflowHandler) Rules(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	_, _ = w.Write(rulesHTML)
}

// Health GET /api/moneyflow/_health — admin BFF 探 split-payment 是否在线.
func (h *MoneyflowHandler) Health(w http.ResponseWriter, _ *http.Request) {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(h.upstream + "/healthz")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "upstream down: "+err.Error())
		return
	}
	defer resp.Body.Close()
	writeJSON(w, map[string]any{
		"upstream": h.upstream,
		"status":   "ok",
		"code":     resp.StatusCode,
	})
}

// envOr local helper.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
