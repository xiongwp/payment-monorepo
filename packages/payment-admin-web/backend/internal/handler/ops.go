// handler/ops.go —— 运营监控 API（交易监控 / 风控规则 / 熔断器 / Webhook 投递）
package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	riskv1 "github.com/xiongwp/risk-manage/kitex_gen/risk/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

type OpsHandler struct{ deps clients.Deps }

func NewOpsHandler(d clients.Deps) *OpsHandler { return &OpsHandler{deps: d} }

// ── 风控规则管理 ──────────────────────────────────────────────────

// GET /api/ops/risk/rules — 列出所有风控规则（只读监控）。
//
// 数据来源优先级：
//  1. risk-manage gRPC ListRules（最权威，含 runtime mode / enabled 状态）
//  2. config-center risk-manage/rules.definitions（规则定义源）— 在 (1) 失败
//     或返空时兜底，让运营至少能看到「规则集应该是什么样的」
//  3. 都拿不到 → 友好降级 hint，UI 弹横幅而不是红错
//
// 这样 risk-manage 容器没起 / etcd 没注册 / rules.definitions 还没 seed
// 这三种 case 都不会让运营看到一个"什么都没有"的无解空白页。
func (h *OpsHandler) ListRiskRules(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	resp, grpcErr := h.deps.Risk.ListRules(ctx, &riskv1.ListRulesRequest{})

	// gRPC 成功且非空 → 直接返
	if grpcErr == nil && len(resp.GetRules()) > 0 {
		items := make([]map[string]interface{}, 0, len(resp.GetRules()))
		for _, rule := range resp.GetRules() {
			items = append(items, map[string]interface{}{
				"id":          rule.GetId(),
				"name":        rule.GetName(),
				"type":        rule.GetType(),
				"enabled":     rule.GetEnabled(),
				"description": rule.GetDescription(),
				"config":      rule.GetConfigJson(),
			})
		}
		writeJSON(w, map[string]interface{}{
			"rules":          items,
			"total":          len(items),
			"service_status": "ok",
			"source":         "risk-manage gRPC",
		})
		return
	}

	// gRPC 挂了 / 空 → 兜底从 config-center 读 rules.definitions
	ccRules, ccErr := fetchRulesFromConfigCenter(r.Context())
	if ccErr == nil && len(ccRules) > 0 {
		status := "config_center_fallback"
		hint := "risk-manage runtime 不可用 / 规则集为空；从 config-center " +
			"namespace=risk-manage key=rules.definitions 兜底展示。改规则请在 " +
			"Config Center 管理（http://localhost:9691/admin/ns/risk-manage）。"
		if grpcErr == nil {
			// risk-manage 在线但规则空（一般是 rules.definitions 还没 seed 到运行时）
			hint = "risk-manage 在线但 runtime 规则集为空；展示来自 config-center 的规则定义；" +
				"重启 risk-manage 让 SDK 拉到 rules.definitions 即可同步运行时状态。"
		}
		writeJSON(w, map[string]interface{}{
			"rules":          ccRules,
			"total":          len(ccRules),
			"service_status": status,
			"source":         "config-center risk-manage/rules.definitions",
			"hint":           hint,
		})
		return
	}

	// 两边都拿不到 → 友好降级
	errMsg := ""
	if grpcErr != nil {
		errMsg = grpcErr.Error()
	}
	ccErrMsg := ""
	if ccErr != nil {
		ccErrMsg = ccErr.Error()
	}
	writeJSON(w, map[string]interface{}{
		"rules":              []any{},
		"total":              0,
		"service_status":     "unavailable",
		"service_error":      errMsg,
		"config_center_error": ccErrMsg,
		"hint": "risk-manage 与 config-center 都拿不到规则。先 seed config-center：" +
			"bash packages/config-center/deploy.sh seed；再确认 risk-manage 容器健康。",
	})
}

// fetchRulesFromConfigCenter 从 config-center 读 rules.definitions（JSON 数组）
// 解析后转成 OpsCenter UI 用的字段集。失败返 (nil, err) 让 caller 降级。
//
// 走 HTTP API: GET /api/v1/configs/risk-manage/rules.definitions
// 响应里 .value 是字符串（rules.definitions 整个 JSON 数组的 raw 文本）。
//
// 端点来自 CONFIG_CENTER_HTTP env，默认 http://config-center:9691（docker
// 网内别名）/ http://localhost:9691（本地 dev）。
func fetchRulesFromConfigCenter(ctx context.Context) ([]map[string]interface{}, error) {
	base := os.Getenv("CONFIG_CENTER_HTTP")
	if base == "" {
		base = "http://config-center:9691"
	}
	url := base + "/api/v1/configs/risk-manage/rules.definitions"

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	cli := &http.Client{Timeout: 2 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, &configCenterError{status: resp.StatusCode}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// config-center GET 响应：{"value": "<string>", "format": ..., ...}
	var envelope struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if envelope.Value == "" {
		return nil, nil
	}
	// envelope.Value 是 rules.definitions 的 raw JSON 数组字符串
	var rules []map[string]interface{}
	if err := json.Unmarshal([]byte(envelope.Value), &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

type configCenterError struct{ status int }

func (e *configCenterError) Error() string {
	return "config-center HTTP " + http.StatusText(e.status)
}

// POST /api/ops/risk/reload — 热重载风控规则。
// risk-manage 不可用时降级返友好提示，让前端不弹红错。
func (h *OpsHandler) ReloadRiskRules(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	resp, err := h.deps.Risk.ReloadRules(ctx, &riskv1.ReloadRulesRequest{})
	if err != nil {
		writeJSON(w, map[string]interface{}{
			"loaded":         0,
			"service_status": "unavailable",
			"service_error":  err.Error(),
			"hint":           "走 Config Center 改 namespace=reconplatform/rules 后 SDK OnChange 自动热更新，不需要本端点",
		})
		return
	}
	writeJSON(w, map[string]interface{}{"loaded": resp.GetLoaded(), "service_status": "ok"})
}

// ── 各服务健康 + metrics 聚合 ─────────────────────────────────────

type serviceHealth struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Healthy bool   `json:"healthy"`
	Latency string `json:"latency,omitempty"`
	Error   string `json:"error,omitempty"`
}

// GET /api/ops/health — 聚合所有下游服务健康状态。并发 probe，总超时 2s。
func (h *OpsHandler) HealthOverview(w http.ResponseWriter, r *http.Request) {
	services := []struct {
		name string
		url  string
	}{
		// **config-center 排第一**：业务服务都依赖它，它挂了页面所有热更新都失效
		{"config-center", "http://config-center:9691/healthz"},
		// 业务服务 metrics port /healthz（DB ping fail 时 503）
		{"order-core", "http://order-core:9090/healthz"},
		{"payment-core", "http://payment-core:9190/healthz"},
		{"payment-channel", "http://payment-channel:9093/healthz"},
		{"kms-manage", "http://kms-manage:9390/healthz"},
		{"risk-manage", "http://risk-manage:9590/healthz"},
		{"user-merchant-core", "http://user-merchant-core:9291/healthz"},
	}

	client := &http.Client{Timeout: 2 * time.Second}
	results := make([]serviceHealth, len(services))
	var wg sync.WaitGroup
	// 改并发 probe：单实例抖动不会把总时延抬到 N×timeout；等同于 2s 上限。
	for i, svc := range services {
		wg.Add(1)
		go func(i int, svc struct{ name, url string }) {
			defer wg.Done()
			sh := serviceHealth{Name: svc.name, URL: svc.url}
			start := time.Now()
			resp, err := client.Get(svc.url)
			sh.Latency = time.Since(start).String()
			if err != nil {
				sh.Error = err.Error()
			} else {
				resp.Body.Close()
				sh.Healthy = resp.StatusCode == 200
				if !sh.Healthy {
					sh.Error = resp.Status
				}
			}
			results[i] = sh
		}(i, svc)
	}
	wg.Wait()

	allHealthy := true
	for _, r := range results {
		if !r.Healthy {
			allHealthy = false
			break
		}
	}
	writeJSON(w, map[string]interface{}{
		"overall": allHealthy,
		"services": results,
	})
}

// ── Webhook 投递统计（直接查 MySQL） ─────────────────────────────

// GET /api/ops/webhooks/stats — webhook 投递概况
func (h *OpsHandler) WebhookStats(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "webhook stats requires order-core admin gRPC (not yet wired)")
}

// ── 系统配置概览 ──────────────────────────────────────────────────

// GET /api/ops/config — 当前系统配置概览（脱敏）+ config-center 跳转入口。
//
// **统一配置入口**：所有动态配置（rate_limit / 风控阈值 / 熔断 / TTL 等）
// 收口在 config-center；本接口只返指针页面，让前端 banner 跳转过去。
func (h *OpsHandler) ConfigOverview(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]interface{}{
		"config_center_url": "http://localhost:9691/admin/",
		"config_center_note": "全平台 12 namespace 动态配置统一入口。改任一 key → SDK watch → " +
			"集群所有副本秒级 OnChange 热更新。",
		"services": []string{
			"config-center", "order-core", "payment-core", "payment-channel",
			"kms-manage", "risk-manage", "user-merchant-core", "accounting-system",
			"card-center", "card-payment", "clearing-settlement", "reconplatform",
			"api-gateway",
		},
		"ports": map[string]interface{}{
			"config-center":   "HTTP:9691 (admin) gRPC:9690 metrics:9692",
			"order-core":      "gRPC:9091",
			"payment-core":    "gRPC:9090 metrics:9190",
			"payment-channel": "gRPC:9092 webhook:9192 metrics:9093",
			"kms-manage":      "gRPC:9290 metrics:9390",
			"risk-manage":     "gRPC:9490 metrics:9590",
			"admin-backend":   "HTTP:9190",
		},
		"features": map[string]bool{
			"risk_screening":      true,
			"circuit_breaker":     true,
			"webhook_delivery":    true,
			"kms_encryption":      true,
			"trace_id":            true,
			"config_center":       true,
			"hot_reload_onchange": true,
		},
	})
}

// ─── 用来解析 risk Deps 的占位 ──────────────────────────────────
var _ = json.Marshal
