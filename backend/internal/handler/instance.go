package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// adminHTTPToken 从环境变量读 accounting-system admin 的 shared-secret token。
// 部署时通过 K8s Secret → env 注入；为空时不带 header（accounting-system 端鉴权关闭）。
var adminHTTPToken = os.Getenv("ACCOUNTING_ADMIN_TOKEN")

// addAuth 为每个发往 accounting-system /admin/* 的请求加上鉴权头（如已配置）。
// 单点 helper 避免每个 handler 各自记得带 header → 漏的话 401 回到 admin-web。
func addAuth(req *http.Request) {
	if adminHTTPToken != "" {
		req.Header.Set("X-Admin-Token", adminHTTPToken)
	}
}

// instanceInfo mirrors the JSON returned by accounting-system's GET /admin/instances.
type instanceInfo struct {
	InstanceID    string    `json:"instance_id"`
	Host          string    `json:"host"`
	HTTPAdminPort int       `json:"http_admin_port"`
	GRPCPort      int       `json:"grpc_port"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	StartedAt     time.Time `json:"started_at"`
}

// reloadResult holds the per-instance reload outcome.
type reloadResult struct {
	InstanceID string `json:"instance_id"`
	Count      int    `json:"count"`
	Error      string `json:"error,omitempty"`
}

// InstanceHandler discovers accounting-system instances via a seed instance's
// HTTP admin API and fans out reload requests to all live instances.
type InstanceHandler struct {
	seedAddr   string       // e.g. "http://host:8888"
	httpClient *http.Client
}

func NewInstanceHandler(seedAddr string) *InstanceHandler {
	return &InstanceHandler{
		seedAddr: seedAddr,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// listAliveInstances fetches the live instance list from the seed.
func (h *InstanceHandler) listAliveInstances(r *http.Request) ([]instanceInfo, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, h.seedAddr+"/admin/instances", nil)
	if err != nil {
		return nil, err
	}
	addAuth(req)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call seed /admin/instances: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("seed /admin/instances returned %d: %s", resp.StatusCode, body)
	}
	var instances []instanceInfo
	if err := json.NewDecoder(resp.Body).Decode(&instances); err != nil {
		return nil, fmt.Errorf("decode instances: %w", err)
	}
	return instances, nil
}

// ListInstances GET /v1/service-instances
// Proxies the seed instance's /admin/instances and returns the list.
func (h *InstanceHandler) ListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := h.listAliveInstances(r)
	if err != nil {
		writeError(w, 503, "无法连接到 accounting-system，请检查服务是否正在运行（seed: "+h.seedAddr+"）")
		return
	}
	writeJSON(w, instances)
}

// ReloadHotAccounts POST /v1/hot-accounts/reload
// Fans out POST /admin/reload/hot-accounts to all alive instances (or a specific one
// if the query param instance_id is set).
func (h *InstanceHandler) ReloadHotAccounts(w http.ResponseWriter, r *http.Request) {
	h.fanout(w, r, "/admin/reload/hot-accounts")
}

// ReloadBufferAccounts POST /v1/buffer-accounts/reload
// Fans out POST /admin/reload/buffer-accounts to all alive instances (or a specific one).
func (h *InstanceHandler) ReloadBufferAccounts(w http.ResponseWriter, r *http.Request) {
	h.fanout(w, r, "/admin/reload/buffer-accounts")
}

// ReloadTransactionRules POST /v1/transaction-rules/reload
// Fans out POST /admin/reload/transaction-rules to all alive instances. Triggered
// after editing transaction_rule / account_type_info, instead of waiting for
// the 60s background ticker.
func (h *InstanceHandler) ReloadTransactionRules(w http.ResponseWriter, r *http.Request) {
	h.fanout(w, r, "/admin/reload/transaction-rules")
}

// ReloadBusinessTypes POST /v1/business-types/reload
// Fans out POST /admin/reload/business-types to all alive instances. Normally triggered
// automatically by RegisterBusinessType; exposed here as a manual recovery button.
func (h *InstanceHandler) ReloadBusinessTypes(w http.ResponseWriter, r *http.Request) {
	h.fanout(w, r, "/admin/reload/business-types")
}

// fanout calls the given admin path on all (or one specific) alive instances concurrently.
func (h *InstanceHandler) fanout(w http.ResponseWriter, r *http.Request, path string) {
	instances, err := h.listAliveInstances(r)
	if err != nil {
		writeError(w, 503, "无法连接到 accounting-system，请检查服务是否正在运行（seed: "+h.seedAddr+"）")
		return
	}

	// Optional: target a single instance.
	targetID := r.URL.Query().Get("instance_id")
	if targetID != "" {
		var filtered []instanceInfo
		for _, inst := range instances {
			if inst.InstanceID == targetID {
				filtered = append(filtered, inst)
				break
			}
		}
		if len(filtered) == 0 {
			writeError(w, 404, "instance not found: "+targetID)
			return
		}
		instances = filtered
	}

	var mu sync.Mutex
	results := make([]reloadResult, 0, len(instances))
	hasError := false

	var wg sync.WaitGroup
	for _, inst := range instances {
		inst := inst
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := h.callInstance(r, inst, path)
			mu.Lock()
			results = append(results, res)
			if res.Error != "" {
				hasError = true
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Aggregate total count.
	total := 0
	for _, res := range results {
		total += res.Count
	}

	if hasError {
		// Still return 200/code=0 with partial results; callers check per-instance errors.
		writeJSON(w, map[string]interface{}{
			"message":   "reloaded (with errors)",
			"count":     total,
			"instances": results,
		})
	} else {
		writeJSON(w, map[string]interface{}{
			"message":   "reloaded",
			"count":     total,
			"instances": results,
		})
	}
}

// ─── Hot account CRUD (proxied to seed via HTTP admin) ───────────────────────

// ListHotAccounts GET /v1/hot-accounts → GET seed/admin/hot-accounts
func (h *InstanceHandler) ListHotAccounts(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, h.seedAddr+"/admin/hot-accounts", nil)
	h.proxyTo(w, req)
}

// CreateHotAccount POST /v1/hot-accounts → POST seed/admin/hot-accounts
func (h *InstanceHandler) CreateHotAccount(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, h.seedAddr+"/admin/hot-accounts", r.Body)
	req.Header.Set("Content-Type", "application/json")
	h.proxyTo(w, req)
}

// CreatePlatformAccount POST /v1/platform-accounts → POST seed/admin/platform-accounts
// Body: {reserved_id, account_type, currency}
func (h *InstanceHandler) CreatePlatformAccount(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, h.seedAddr+"/admin/platform-accounts", r.Body)
	req.Header.Set("Content-Type", "application/json")
	h.proxyTo(w, req)
}

// CreatePlatformAccountFleet POST /v1/platform-accounts/fleet → POST seed/admin/platform-accounts/fleet
// Body: {account_type, business_type_code, description, channel_code, channel_business_type, currency}
// Creates 100 shard-distributed accounts for one channel AND registers it in account_business_type_info.
func (h *InstanceHandler) CreatePlatformAccountFleet(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, h.seedAddr+"/admin/platform-accounts/fleet", r.Body)
	req.Header.Set("Content-Type", "application/json")
	h.proxyTo(w, req)
}

// ListBusinessTypes GET /v1/business-types → GET seed/admin/business-types
func (h *InstanceHandler) ListBusinessTypes(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, h.seedAddr+"/admin/business-types", nil)
	h.proxyTo(w, req)
}

// RegisterBusinessType POST /v1/business-types
// 两步操作（保证集群内所有实例即时一致）：
//  1. POST seed/admin/business-types 写 meta DB
//  2. 成功后 fanout POST /admin/reload/business-types 到所有活实例，让每台
//     accounting-system 把新 business_type 加载到本地内存缓存中（无需等 60s ConfigSync tick）。
//
// 使用两步而不是单点 proxy 的理由：业务类型只在"新增"时变化（低频），若依赖 60s
// 定时拉取，新账户创建请求在窗口期内可能打到尚未 reload 的实例 → 报 "business_type not registered"。
func (h *InstanceHandler) RegisterBusinessType(w http.ResponseWriter, r *http.Request) {
	// Step 1: 读取请求体一次，因为既要转发给 seed 也可能在失败时返回给前端
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	// 写 meta DB（会 upsert 到 account_business_type_info）
	regReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, h.seedAddr+"/admin/business-types", bytes.NewReader(body))
	regReq.Header.Set("Content-Type", "application/json")
	addAuth(regReq)
	regResp, err := h.httpClient.Do(regReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "register business_type: "+err.Error())
		return
	}
	defer regResp.Body.Close()

	var registerBody interface{}
	if err := json.NewDecoder(regResp.Body).Decode(&registerBody); err != nil {
		writeError(w, http.StatusBadGateway, "decode register response: "+err.Error())
		return
	}
	if regResp.StatusCode >= 400 {
		if m, ok := registerBody.(map[string]interface{}); ok {
			if errMsg, ok := m["error"].(string); ok {
				writeError(w, regResp.StatusCode, errMsg)
				return
			}
		}
		writeError(w, regResp.StatusCode, fmt.Sprintf("register business_type upstream HTTP %d", regResp.StatusCode))
		return
	}

	// Step 2: fanout reload 到全集群（尽力而为，即使部分失败仍返回 registry 记录，
	// 因为 60s ConfigSync tick 会兜底；但结果里会带 reload_instances 诊断信息）
	instances, listErr := h.listAliveInstances(r)
	fanoutResults := make([]reloadResult, 0)
	if listErr == nil {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, inst := range instances {
			inst := inst
			wg.Add(1)
			go func() {
				defer wg.Done()
				res := h.callInstance(r, inst, "/admin/reload/business-types")
				mu.Lock()
				fanoutResults = append(fanoutResults, res)
				mu.Unlock()
			}()
		}
		wg.Wait()
	}

	writeJSON(w, map[string]interface{}{
		"business_type":    registerBody,
		"reload_instances": fanoutResults,
		"reload_error":     errString(listErr),
	})
}

// errString returns err.Error() or empty string for nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// PlatformAccountBalances GET /v1/platform-accounts/balances?business_type=N
// 返回该 business_type 对应的 100 个平台账户及当前余额。
func (h *InstanceHandler) PlatformAccountBalances(w http.ResponseWriter, r *http.Request) {
	url := h.seedAddr + "/admin/platform-accounts/balances?" + r.URL.RawQuery
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	h.proxyTo(w, req)
}

// PlatformAccountSnapshots GET /v1/platform-accounts/snapshots?business_type=N&date=YYYY-MM-DD
func (h *InstanceHandler) PlatformAccountSnapshots(w http.ResponseWriter, r *http.Request) {
	url := h.seedAddr + "/admin/platform-accounts/snapshots?" + r.URL.RawQuery
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	h.proxyTo(w, req)
}

// ListAccountTypes GET /v1/account-types → seed /admin/account-types
// 返回 account_type_info 全表（含 is_platform），前端据此判断某 account_type 属于平台内部 or 业务账户。
func (h *InstanceHandler) ListAccountTypes(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, h.seedAddr+"/admin/account-types", nil)
	h.proxyTo(w, req)
}

// TccArchiveConfig GET /v1/tcc-archive/config → seed /admin/tcc-archive/config
func (h *InstanceHandler) TccArchiveConfig(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, h.seedAddr+"/admin/tcc-archive/config", nil)
	h.proxyTo(w, req)
}

// TccArchiveRun POST /v1/tcc-archive/run → seed /admin/tcc-archive/run
// 触发立即归档（异步，立即返回 202）。
func (h *InstanceHandler) TccArchiveRun(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, h.seedAddr+"/admin/tcc-archive/run", nil)
	h.proxyTo(w, req)
}

// DayCutResume POST /v1/day-cut/resume → seed /admin/day-cut/resume
// Body: {cut_date, run_id, stuck_threshold_seconds?}
// 重新派发指定 (cut_date, run_id) 中卡死的 PROCESSING day-cut 分片。
// 用于 DB 断网 / 进程重启后 day-cut 卡住无法继续的场景。
func (h *InstanceHandler) DayCutResume(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, h.seedAddr+"/admin/day-cut/resume", r.Body)
	req.Header.Set("Content-Type", "application/json")
	h.proxyTo(w, req)
}

// TccRetryConfirmNow POST /v1/tcc/retry-confirm → seed /admin/tcc/retry-confirm
// Body: {"threshold_seconds": 0}（默认 0 = 不管多新的 CONFIRMING 立即重试）
// loadtest 后不想等 5min stuck 阈值 → 调此端点立即恢复，再跑 day-cut。
func (h *InstanceHandler) TccRetryConfirmNow(w http.ResponseWriter, r *http.Request) {
	body := r.Body
	if body == nil {
		body = io.NopCloser(bytes.NewReader([]byte(`{"threshold_seconds":0}`)))
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, h.seedAddr+"/admin/tcc/retry-confirm", body)
	req.Header.Set("Content-Type", "application/json")
	h.proxyTo(w, req)
}

// ListSystemConfig GET /v1/config → seed /admin/config
func (h *InstanceHandler) ListSystemConfig(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, h.seedAddr+"/admin/config", nil)
	h.proxyTo(w, req)
}

// (RedisRebuild moved to redis_rebuild.go — uses gRPC instead of admin HTTP
// proxy, matching the rest of admin-web's account/snapshot/tcc handlers.)

// UpsertSystemConfig POST /v1/config → seed POST /admin/config + fanout reload
// 两步：写 DB（seed），然后扇出 reload 到所有 alive instances。
func (h *InstanceHandler) UpsertSystemConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	// Step 1: 写 DB（任一存活实例的 admin/config 都行；用 seed）
	upReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, h.seedAddr+"/admin/config", bytes.NewReader(body))
	upReq.Header.Set("Content-Type", "application/json")
	addAuth(upReq)
	upResp, err := h.httpClient.Do(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upsert config: "+err.Error())
		return
	}
	defer upResp.Body.Close()
	var upBody interface{}
	if err := json.NewDecoder(upResp.Body).Decode(&upBody); err != nil {
		writeError(w, http.StatusBadGateway, "decode upsert response: "+err.Error())
		return
	}
	if upResp.StatusCode >= 400 {
		if m, ok := upBody.(map[string]interface{}); ok {
			if e, ok := m["error"].(string); ok {
				writeError(w, upResp.StatusCode, e)
				return
			}
		}
		writeError(w, upResp.StatusCode, fmt.Sprintf("upstream HTTP %d", upResp.StatusCode))
		return
	}
	// Step 2: fanout reload 到所有活实例（best-effort，部分失败仍继续 — 60s 兜底兜底）
	instances, lerr := h.listAliveInstances(r)
	fanoutResults := make([]reloadResult, 0)
	if lerr == nil {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, inst := range instances {
			inst := inst
			wg.Add(1)
			go func() {
				defer wg.Done()
				res := h.callInstance(r, inst, "/admin/reload/config")
				mu.Lock()
				fanoutResults = append(fanoutResults, res)
				mu.Unlock()
			}()
		}
		wg.Wait()
	}
	writeJSON(w, map[string]interface{}{
		"upsert":           upBody,
		"reload_instances": fanoutResults,
		"reload_error":     errString(lerr),
	})
}

// DeleteSystemConfig DELETE /v1/config/{key} → seed DELETE /admin/config/{key} + fanout reload
func (h *InstanceHandler) DeleteSystemConfig(w http.ResponseWriter, r *http.Request) {
	key := extractLastPathSegment(r.URL.Path)
	if key == "" {
		writeError(w, http.StatusBadRequest, "config key required")
		return
	}
	delReq, _ := http.NewRequestWithContext(r.Context(), http.MethodDelete, h.seedAddr+"/admin/config/"+key, nil)
	addAuth(delReq)
	delResp, err := h.httpClient.Do(delReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer delResp.Body.Close()
	var delBody interface{}
	_ = json.NewDecoder(delResp.Body).Decode(&delBody)
	if delResp.StatusCode >= 400 {
		writeError(w, delResp.StatusCode, fmt.Sprintf("delete upstream HTTP %d", delResp.StatusCode))
		return
	}
	// fanout reload
	instances, lerr := h.listAliveInstances(r)
	fanoutResults := make([]reloadResult, 0)
	if lerr == nil {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, inst := range instances {
			inst := inst
			wg.Add(1)
			go func() {
				defer wg.Done()
				res := h.callInstance(r, inst, "/admin/reload/config")
				mu.Lock()
				fanoutResults = append(fanoutResults, res)
				mu.Unlock()
			}()
		}
		wg.Wait()
	}
	writeJSON(w, map[string]interface{}{
		"deleted":          delBody,
		"reload_instances": fanoutResults,
	})
}

// ReloadSystemConfig POST /v1/config/reload → fanout /admin/reload/config (manual button).
func (h *InstanceHandler) ReloadSystemConfig(w http.ResponseWriter, r *http.Request) {
	h.fanout(w, r, "/admin/reload/config")
}

// UpdateHotAccount PUT /v1/hot-accounts/{id} → PUT seed/admin/hot-accounts/{id}
func (h *InstanceHandler) UpdateHotAccount(w http.ResponseWriter, r *http.Request) {
	id := extractLastPathSegment(r.URL.Path)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPut, h.seedAddr+"/admin/hot-accounts/"+id, r.Body)
	req.Header.Set("Content-Type", "application/json")
	h.proxyTo(w, req)
}

// DeleteHotAccount DELETE /v1/hot-accounts/{id} → DELETE seed/admin/hot-accounts/{id}
func (h *InstanceHandler) DeleteHotAccount(w http.ResponseWriter, r *http.Request) {
	id := extractLastPathSegment(r.URL.Path)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodDelete, h.seedAddr+"/admin/hot-accounts/"+id, nil)
	h.proxyTo(w, req)
}

// proxyTo forwards a prepared request to accounting-system's HTTP admin and
// relays the response wrapped in the standard {code, message, data} envelope.
// 单点 helper：所有 admin proxy 必走这里 → 集中加 auth header，避免每个调用点漏配。
func (h *InstanceHandler) proxyTo(w http.ResponseWriter, req *http.Request) {
	addAuth(req)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer resp.Body.Close()

	var body interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		writeError(w, 500, "decode upstream response: "+err.Error())
		return
	}
	if resp.StatusCode >= 400 {
		// Extract error message if present
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

// extractLastPathSegment returns the last non-empty segment of a URL path.
// e.g. "/v1/hot-accounts/42" → "42"
func extractLastPathSegment(path string) string {
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// callInstance sends a POST to a single accounting-system instance and parses the response.
func (h *InstanceHandler) callInstance(r *http.Request, inst instanceInfo, path string) reloadResult {
	url := fmt.Sprintf("http://%s:%d%s", inst.Host, inst.HTTPAdminPort, path)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return reloadResult{InstanceID: inst.InstanceID, Error: err.Error()}
	}
	addAuth(req)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return reloadResult{InstanceID: inst.InstanceID, Error: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return reloadResult{InstanceID: inst.InstanceID, Error: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, body)}
	}
	var body struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return reloadResult{InstanceID: inst.InstanceID, Error: "decode response: " + err.Error()}
	}
	return reloadResult{InstanceID: inst.InstanceID, Count: body.Count}
}
