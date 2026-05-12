// Package paymentsdk — Official Go SDK for Payment Platform API.
//
// Usage:
//
//   import "github.com/payment-platform/sdk-go"
//
//   c := paymentsdk.New("pk_test_xxx")
//   charge, err := c.Charges.Create(ctx, &paymentsdk.ChargeParams{
//       Amount: 1999, Currency: "USD", Source: "tok_xxx",
//   })
package paymentsdk

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const APIVersion = "2026-05-01"
const SDKVersion = "0.1.0"

// Client SDK 主入口.
type Client struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
	MaxRetries int

	// 资源 API
	Charges   *ChargesAPI
	Refunds   *RefundsAPI
	Payouts   *PayoutsAPI
	Customers *CustomersAPI
	Webhooks  *WebhooksAPI
}

// New 创建 client. apiKey 必填; baseUrl 不传按 key 前缀自动选 sandbox/prod.
func New(apiKey string, opts ...Opt) *Client {
	if apiKey == "" {
		panic("paymentsdk: apiKey required (pk_test_... or pk_live_...)")
	}
	c := &Client{
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		MaxRetries: 3,
	}
	if strings.HasPrefix(apiKey, "pk_test_") {
		c.BaseURL = "https://api-sandbox.payment.example.com"
	} else {
		c.BaseURL = "https://api.payment.example.com"
	}
	for _, opt := range opts {
		opt(c)
	}
	c.Charges = &ChargesAPI{c: c}
	c.Refunds = &RefundsAPI{c: c}
	c.Payouts = &PayoutsAPI{c: c}
	c.Customers = &CustomersAPI{c: c}
	c.Webhooks = &WebhooksAPI{c: c}
	return c
}

// Opt 可选配置
type Opt func(*Client)

func WithBaseURL(u string) Opt        { return func(c *Client) { c.BaseURL = u } }
func WithTimeout(d time.Duration) Opt { return func(c *Client) { c.HTTPClient.Timeout = d } }
func WithMaxRetries(n int) Opt        { return func(c *Client) { c.MaxRetries = n } }

// Mode 自动按 key 前缀判.
func (c *Client) Mode() string {
	if strings.HasPrefix(c.APIKey, "pk_test_") {
		return "test"
	}
	return "live"
}

// APIError HTTP 错误.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Body       map[string]interface{}
}

func (e *APIError) Error() string {
	return fmt.Sprintf("paymentsdk: HTTP %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// Request 内部 HTTP 调用.
func (c *Client) Request(ctx context.Context, method, path string, in, out interface{}, idempotencyKey string) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	urlStr := c.BaseURL + path
	headers := map[string]string{
		"Authorization": "Bearer " + c.APIKey,
		"User-Agent":    "paymentsdk-go/" + SDKVersion,
		"Accept":        "application/json",
		"X-API-Version": APIVersion,
	}
	if in != nil {
		headers["Content-Type"] = "application/json"
	}
	if method != http.MethodGet && method != http.MethodHead {
		if idempotencyKey == "" {
			idempotencyKey = "idem_" + randHex(16)
		}
		headers["Idempotency-Key"] = idempotencyKey
	}

	var lastErr error
	for attempt := 0; attempt < c.MaxRetries; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, method, urlStr, body)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(100*(1<<attempt)) * time.Millisecond)
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode < 300 {
			if out != nil && len(respBody) > 0 {
				return json.Unmarshal(respBody, out)
			}
			return nil
		}
		// 4xx (除 429) 不重试
		var errBody map[string]interface{}
		_ = json.Unmarshal(respBody, &errBody)
		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			Body:       errBody,
		}
		if s, ok := errBody["error"].(string); ok {
			apiErr.Code = s
		}
		if s, ok := errBody["message"].(string); ok {
			apiErr.Message = s
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
			return apiErr
		}
		lastErr = apiErr
		time.Sleep(time.Duration(100*(1<<attempt)) * time.Millisecond)
	}
	return lastErr
}

// ── 资源 API ──

type ChargesAPI struct{ c *Client }

type ChargeParams struct {
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Source      string `json:"source"` // tokenized PAN reference
	Description string `json:"description,omitempty"`
	CustomerID  string `json:"customer_id,omitempty"`
}

type Charge struct {
	ID       string    `json:"id"`
	Amount   int64     `json:"amount"`
	Currency string    `json:"currency"`
	Status   string    `json:"status"`
	Created  time.Time `json:"created"`
}

func (a *ChargesAPI) Create(ctx context.Context, p *ChargeParams, idem ...string) (*Charge, error) {
	out := &Charge{}
	k := ""
	if len(idem) > 0 {
		k = idem[0]
	}
	if err := a.c.Request(ctx, "POST", "/v1/charges", p, out, k); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *ChargesAPI) Retrieve(ctx context.Context, id string) (*Charge, error) {
	out := &Charge{}
	if err := a.c.Request(ctx, "GET", "/v1/charges/"+id, nil, out, ""); err != nil {
		return nil, err
	}
	return out, nil
}

type RefundsAPI struct{ c *Client }

type RefundParams struct {
	ChargeID string `json:"charge_id"`
	Amount   int64  `json:"amount,omitempty"` // 不传 = 全额
	Reason   string `json:"reason,omitempty"`
}

func (a *RefundsAPI) Create(ctx context.Context, p *RefundParams, idem ...string) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	k := ""
	if len(idem) > 0 {
		k = idem[0]
	}
	if err := a.c.Request(ctx, "POST", "/v1/refunds", p, &out, k); err != nil {
		return nil, err
	}
	return out, nil
}

type PayoutsAPI struct{ c *Client }

func (a *PayoutsAPI) Retrieve(ctx context.Context, id string) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	if err := a.c.Request(ctx, "GET", "/v1/payouts/"+id, nil, &out, ""); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *PayoutsAPI) List(ctx context.Context, query map[string]string) (map[string]interface{}, error) {
	q := url.Values{}
	for k, v := range query {
		q.Set(k, v)
	}
	p := "/v1/payouts"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	out := map[string]interface{}{}
	if err := a.c.Request(ctx, "GET", p, nil, &out, ""); err != nil {
		return nil, err
	}
	return out, nil
}

type CustomersAPI struct{ c *Client }

func (a *CustomersAPI) Create(ctx context.Context, p map[string]interface{}, idem ...string) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	k := ""
	if len(idem) > 0 {
		k = idem[0]
	}
	if err := a.c.Request(ctx, "POST", "/v1/customers", p, &out, k); err != nil {
		return nil, err
	}
	return out, nil
}

// WebhooksAPI 验签 + 解 event.
type WebhooksAPI struct{ c *Client }

// ConstructEvent 同 stripe.Webhook.ConstructEvent.
func (a *WebhooksAPI) ConstructEvent(rawBody []byte, sigHeader, endpointSecret string, toleranceSec int) (map[string]interface{}, error) {
	if toleranceSec <= 0 {
		toleranceSec = 300
	}
	var timestamp int64
	var sigs []string
	for _, part := range strings.Split(sigHeader, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "t=") {
			_, err := fmt.Sscanf(part[2:], "%d", &timestamp)
			if err != nil {
				return nil, errors.New("paymentsdk: bad timestamp")
			}
		} else if strings.HasPrefix(part, "v1=") {
			sigs = append(sigs, part[3:])
		}
	}
	if timestamp == 0 || len(sigs) == 0 {
		return nil, errors.New("paymentsdk: malformed signature header")
	}
	delta := time.Now().Unix() - timestamp
	if delta > int64(toleranceSec) || delta < -int64(toleranceSec) {
		return nil, errors.New("paymentsdk: timestamp out of tolerance")
	}
	mac := hmac.New(sha256.New, []byte(endpointSecret))
	mac.Write([]byte(fmt.Sprintf("%d.%s", timestamp, rawBody)))
	expected := hex.EncodeToString(mac.Sum(nil))
	ok := false
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(expected)) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, errors.New("paymentsdk: signature mismatch")
	}
	var event map[string]interface{}
	if err := json.Unmarshal(rawBody, &event); err != nil {
		return nil, fmt.Errorf("paymentsdk: parse event: %w", err)
	}
	return event, nil
}

// ── helpers ──

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
