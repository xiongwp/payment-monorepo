// risk.go — SP-FIN-1 RiskClient / AMLClient HTTP-JSON 真实实现.
//
// 设计:
//   - 走 HTTP-JSON 而不是 gRPC, 因为 risk-manage / aml-screening 的 proto 还在演进,
//     HTTP-JSON 跨版本兼容更宽松; 性能足够 (qps < 1000 这个场景).
//   - Service URL 走 env (RISK_HTTP_URL / AML_HTTP_URL), 默认走容器名解析.
//   - 失败超时 2s 不阻塞主路径, 由上层 RiskGate 按 fail-safe 策略决定 allow/deny.
//
// 协议 (post JSON):
//
//   POST {risk_url}/v1/evaluate
//   { "event": "charge.succeeded", "charge_id":..., "merchant_id":..., "amount_minor":..., ... }
//   →
//   { "decision": "allow|review|deny", "score": 65, "rule_id": "marketplace_high_amount", "reason": "..." }
//
// 适配 split-payment/internal/RiskClient / AMLClient 接口.
package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

)

// HTTPRiskClient 通过 HTTP-JSON 调 risk-manage 服务.
type HTTPRiskClient struct {
	BaseURL    string
	HTTPClient *http.Client
	AuthToken  string // 可选 bearer
}

// NewHTTPRiskClient 默认 2s timeout.
func NewHTTPRiskClient(baseURL, token string) *HTTPRiskClient {
	return &HTTPRiskClient{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
		AuthToken:  token,
	}
}

// Evaluate impl RiskClient.
func (c *HTTPRiskClient) Evaluate(ctx context.Context, req RiskRequest) (*RiskResult, error) {
	return c.call(ctx, "/v1/evaluate", req)
}

// HTTPAMLClient 同 RiskClient 但调 aml-screening.
type HTTPAMLClient struct {
	BaseURL    string
	HTTPClient *http.Client
	AuthToken  string
}

// NewHTTPAMLClient.
func NewHTTPAMLClient(baseURL, token string) *HTTPAMLClient {
	return &HTTPAMLClient{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
		AuthToken:  token,
	}
}

// Screen impl AMLClient.
func (c *HTTPAMLClient) Screen(ctx context.Context, req RiskRequest) (*RiskResult, error) {
	hr := &HTTPRiskClient{BaseURL: c.BaseURL, HTTPClient: c.HTTPClient, AuthToken: c.AuthToken}
	return hr.call(ctx, "/v1/screen", req)
}

// call 共用逻辑.
func (c *HTTPRiskClient) call(ctx context.Context, path string, req RiskRequest) (*RiskResult, error) {
	if c.BaseURL == "" {
		return nil, errors.New("risk client: base url empty")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal req: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("upstream %d", resp.StatusCode)
	}
	var out struct {
		Decision string `json:"decision"`
		Score    int    `json:"score"`
		RuleID   string `json:"rule_id"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	d := RiskDecision(out.Decision)
	if d == "" {
		d = RiskAllow
	}
	return &RiskResult{
		Decision: d, Score: out.Score, RuleID: out.RuleID, Reason: out.Reason,
	}, nil
}
