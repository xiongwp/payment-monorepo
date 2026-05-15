// accounting_meta.go — SP-AC-4 BFF reverse proxy 到 accounting-system admin API.
//
// 路径:
//   /api/accounting/account_types       → accounting/admin/account_types
//   /api/accounting/transaction_rules   → accounting/admin/transaction_rules
//   /api/accounting/transactions        → accounting/admin/transactions
//
// Designer 节点 picker 调本端点拉账户类型 / 规则列表.
package handler

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// AccountingMetaHandler.
type AccountingMetaHandler struct {
	proxy *httputil.ReverseProxy
}

// NewAccountingMetaHandler.
func NewAccountingMetaHandler() *AccountingMetaHandler {
	up := envOrDef("ACCOUNTING_HTTP_URL", "http://accounting-system:8080")
	u, err := url.Parse(up)
	if err != nil || u.Scheme == "" {
		u, _ = url.Parse("http://localhost:8080")
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.FlushInterval = 100 * time.Millisecond
	// rewrite path: /api/accounting/xxx → /admin/xxx
	defaultDirector := rp.Director
	rp.Director = func(r *http.Request) {
		defaultDirector(r)
		r.URL.Path = strings.Replace(r.URL.Path, "/api/accounting/", "/admin/", 1)
	}
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "accounting upstream unreachable: "+err.Error(),
			http.StatusBadGateway)
	}
	return &AccountingMetaHandler{proxy: rp}
}

// Proxy.
func (h *AccountingMetaHandler) Proxy(w http.ResponseWriter, r *http.Request) {
	h.proxy.ServeHTTP(w, r)
}
