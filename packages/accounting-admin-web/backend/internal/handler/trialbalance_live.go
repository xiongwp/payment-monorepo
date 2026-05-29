package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// TrialBalanceLiveHandler proxies the trial-balance endpoints that live on
// accounting-system's adminhttp server (HTTP, not gRPC):
//
//	GET /v1/trial-balance/live       → GET {base}/admin/trial-balance/live
//	GET /v1/trial-balance/drilldown  → GET {base}/admin/trial-balance/drilldown
//	GET /v1/trial-balance/export     → GET {base}/admin/trial-balance/export   (CSV stream)
//
// base 来自 env ACCOUNTING_ADMIN_URL（main.go 注入，默认 accounting-service:8888）。
// 这几个端点是跨全部 100 分片的只读汇总，任一活实例都返回相同结果，所以单点 proxy 即可，
// 无需像 reload 那样 fanout 到全集群。
type TrialBalanceLiveHandler struct {
	baseURL    string
	httpClient *http.Client
}

func NewTrialBalanceLiveHandler(baseURL string) *TrialBalanceLiveHandler {
	return &TrialBalanceLiveHandler{
		baseURL: baseURL,
		// 试算平衡跨 100 分片聚合，可能较慢；给足超时。
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// addAdminAuth attaches the shared-secret token if configured (mirrors instance.go addAuth).
func (h *TrialBalanceLiveHandler) addAdminAuth(req *http.Request) {
	if tok := os.Getenv("ACCOUNTING_ADMIN_TOKEN"); tok != "" {
		req.Header.Set("X-Admin-Token", tok)
	}
}

// Live GET /v1/trial-balance/live?currency=PHP
func (h *TrialBalanceLiveHandler) Live(w http.ResponseWriter, r *http.Request) {
	h.proxyJSON(w, r, "/admin/trial-balance/live")
}

// Snapshot GET /v1/trial-balance/snapshot?currency=&snapshot_date=&run_id=
// 走 admin HTTP 而非 gRPC,让 business_type / Level 字段直接 JSON 透传(proto 没定义)。
func (h *TrialBalanceLiveHandler) Snapshot(w http.ResponseWriter, r *http.Request) {
	h.proxyJSON(w, r, "/admin/trial-balance/snapshot")
}

// Dates GET /v1/trial-balance/dates
func (h *TrialBalanceLiveHandler) Dates(w http.ResponseWriter, r *http.Request) {
	h.proxyJSON(w, r, "/admin/trial-balance/dates")
}

// Drilldown GET /v1/trial-balance/drilldown?currency=&category=&account_type=&business_type=&snapshot_date=&run_id=
// 透传全部 query params。
func (h *TrialBalanceLiveHandler) Drilldown(w http.ResponseWriter, r *http.Request) {
	h.proxyJSON(w, r, "/admin/trial-balance/drilldown")
}

// proxyJSON forwards the incoming GET (query string included) to the accounting
// adminhttp path, then re-wraps the upstream JSON in the admin-web envelope
// {code,message,data} so the frontend request() client can unwrap `data`.
// Upstream / network failures return 502 (not a bare 500) with the error text.
func (h *TrialBalanceLiveHandler) proxyJSON(w http.ResponseWriter, r *http.Request, path string) {
	url := h.baseURL + path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "build upstream request: "+err.Error())
		return
	}
	h.addAdminAuth(req)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "accounting-system 不可达（"+h.baseURL+"）："+err.Error())
		return
	}
	defer resp.Body.Close()

	var body interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadGateway, "decode upstream response: "+err.Error())
		return
	}
	if resp.StatusCode >= 400 {
		// accounting adminhttp 错误体形如 {"error": "..."}
		if m, ok := body.(map[string]interface{}); ok {
			if errMsg, ok := m["error"].(string); ok {
				writeError(w, resp.StatusCode, errMsg)
				return
			}
		}
		writeError(w, resp.StatusCode, fmt.Sprintf("upstream HTTP %d", resp.StatusCode))
		return
	}
	writeJSON(w, body)
}

// Export GET /v1/trial-balance/export?currency=&snapshot_date=&run_id=
// CSV 流式透传：保留上游 Content-Type + Content-Disposition（含 filename），
// 让浏览器直接触发下载。proxy 失败时退回 JSON 错误（502），不裸 500。
func (h *TrialBalanceLiveHandler) Export(w http.ResponseWriter, r *http.Request) {
	url := h.baseURL + "/admin/trial-balance/export"
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "build upstream request: "+err.Error())
		return
	}
	h.addAdminAuth(req)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "accounting-system 不可达（"+h.baseURL+"）："+err.Error())
		return
	}
	defer resp.Body.Close()

	// 上游非 2xx：上游返回 JSON 错误体，原样作为 JSON 错误回前端（不当作 CSV 下载）。
	if resp.StatusCode >= 400 {
		var body interface{}
		if derr := json.NewDecoder(resp.Body).Decode(&body); derr == nil {
			if m, ok := body.(map[string]interface{}); ok {
				if errMsg, ok := m["error"].(string); ok {
					writeError(w, resp.StatusCode, errMsg)
					return
				}
			}
		}
		writeError(w, resp.StatusCode, fmt.Sprintf("export upstream HTTP %d", resp.StatusCode))
		return
	}

	// 透传 CSV header，保留 filename，触发浏览器下载。
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
