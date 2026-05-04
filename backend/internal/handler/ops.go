// handler/ops.go —— 运营监控 API（交易监控 / 风控规则 / 熔断器 / Webhook 投递）
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	riskv1 "github.com/xiongwp/risk-manage/api/proto/risk/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

type OpsHandler struct{ deps clients.Deps }

func NewOpsHandler(d clients.Deps) *OpsHandler { return &OpsHandler{deps: d} }

// ── 风控规则管理 ──────────────────────────────────────────────────

// GET /api/ops/risk/rules — 列出所有风控规则
func (h *OpsHandler) ListRiskRules(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	resp, err := h.deps.Risk.ListRules(ctx, &riskv1.ListRulesRequest{})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
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
	writeJSON(w, map[string]interface{}{"rules": items, "total": len(items)})
}

// POST /api/ops/risk/reload — 热重载风控规则
func (h *OpsHandler) ReloadRiskRules(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	resp, err := h.deps.Risk.ReloadRules(ctx, &riskv1.ReloadRulesRequest{})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"loaded": resp.GetLoaded()})
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
		// 容器 DNS + metrics port 上的 /healthz（含 DB ping 的服务会 503 表达"未就绪"）。
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

// GET /api/ops/config — 当前系统配置概览（脱敏）
func (h *OpsHandler) ConfigOverview(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]interface{}{
		"services": []string{"order-core", "payment-core", "payment-channel", "kms-manage", "risk-manage"},
		"ports": map[string]interface{}{
			"order-core":      "gRPC:9091",
			"payment-core":    "gRPC:9090 metrics:9190",
			"payment-channel": "gRPC:9092 webhook:9192 metrics:9093",
			"kms-manage":      "gRPC:9290 metrics:9390",
			"risk-manage":     "gRPC:9490 metrics:9590",
			"admin-backend":   "HTTP:9190",
		},
		"features": map[string]bool{
			"risk_screening":   true,
			"circuit_breaker":  true,
			"webhook_delivery": true,
			"kms_encryption":   true,
			"trace_id":         true,
		},
	})
}

// ─── 用来解析 risk Deps 的占位 ──────────────────────────────────
var _ = json.Marshal
