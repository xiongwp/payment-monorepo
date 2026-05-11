// Package clients — refund-engine 出站 HTTP client。
//
// WebhookClient: refund 状态变化 → POST merchant-webhook /api/v1/events
// AuditClient:   ops 操作 → POST audit-log /api/v1/audit/log (留痕)
//
// 这是第一个真实跨服务的样板 — 其它服务可以照样接：
//   billing-system / clearing-settlement / dispute-service / kyc-service
//   都应该有自己的 clients/ 包接 webhook / audit。

package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"reconcile-system/packages/refund-engine/internal/domain"
)

// WebhookClient 调 merchant-webhook 发事件。
type WebhookClient struct {
	BaseURL string
	HC      *http.Client
}

// NewWebhookClient — base_url 默认 http://merchant-webhook:8080。
func NewWebhookClient(baseURL string) *WebhookClient {
	if baseURL == "" {
		baseURL = "http://merchant-webhook:8080"
	}
	return &WebhookClient{
		BaseURL: baseURL,
		HC:      &http.Client{Timeout: 5 * time.Second},
	}
}

// NotifyMerchant 实现 workflow.Notifier 接口。
func (c *WebhookClient) NotifyMerchant(ctx context.Context, merchantID, eventType string, payload any) error {
	body, _ := json.Marshal(payload)
	req := map[string]any{
		"merchant_id": merchantID,
		"event_type":  eventType,
		"event_id":    fmt.Sprintf("evt_refund_%d", time.Now().UnixNano()),
		"payload":     json.RawMessage(body),
	}
	reqBody, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+"/api/v1/events", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HC.Do(httpReq)
	if err != nil {
		return fmt.Errorf("webhook call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

// NotifyBilling 触发 billing-system 写 fee_event(refund)。
func (c *WebhookClient) NotifyBilling(ctx context.Context, r *domain.Refund) error {
	// 简化：直接调 billing /api/v1/fee/events
	billingURL := "http://billing-system:8080/api/v1/fee/events"
	req := map[string]any{
		"merchant_id":      r.MerchantID,
		"ref_id":           r.RefundID,
		"ref_service":      "refund-engine",
		"event_type":       "refund",
		"amount_minor":     r.AmountMinor,
		"currency":         r.Currency,
		"product":          "card_charge",
		"channel_adapter":  "stub",
		"region":           "PH",
		"trace_id":         r.TraceID,
		"occurred_at":      time.Now().UTC().Format(time.RFC3339),
	}
	reqBody, _ := json.Marshal(req)
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", billingURL, bytes.NewReader(reqBody))
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HC.Do(httpReq)
	if err != nil {
		return fmt.Errorf("billing call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("billing returned %d", resp.StatusCode)
	}
	return nil
}

// AuditClient 调 audit-log 留痕。
type AuditClient struct {
	BaseURL string
	HC      *http.Client
	Service string // 'refund-engine'
}

func NewAuditClient(baseURL, service string) *AuditClient {
	if baseURL == "" {
		baseURL = "http://audit-log:8080"
	}
	return &AuditClient{
		BaseURL: baseURL,
		HC:      &http.Client{Timeout: 3 * time.Second},
		Service: service,
	}
}

// Log 异步写一条审计 — 失败不阻塞业务（log warn 即可）。
func (a *AuditClient) Log(ctx context.Context, actor, action, resourceType, resourceID, note string) error {
	entry := map[string]any{
		"service":       a.Service,
		"actor_email":   actor,
		"action":        action,
		"resource_type": resourceType,
		"resource_id":   resourceID,
		"note":          note,
	}
	body, _ := json.Marshal(entry)
	req, _ := http.NewRequestWithContext(ctx, "POST",
		a.BaseURL+"/api/v1/audit/log", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HC.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
