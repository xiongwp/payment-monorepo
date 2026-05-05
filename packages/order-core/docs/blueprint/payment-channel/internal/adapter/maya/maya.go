//go:build ignore
// +build ignore

// Package maya 实现 Maya Checkout v2 adapter。
//
// 规格：https://s3-us-west-2.amazonaws.com/developers.paymaya.com.pg/checkout/v2/Checkout+API.html
//
// 鉴权：HTTP Basic Auth  Base64(apiKey + ":")   password 空
//   - Create 用 public key
//   - Retrieve / Refund / Void 用 secret key
//
// Base URL:
//   Sandbox https://pg-sandbox.paymaya.com
//   Prod    https://pg.paymaya.com
package maya

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseSandbox = "https://pg-sandbox.paymaya.com"
	baseProd    = "https://pg.paymaya.com"

	pathCreateCheckout   = "/checkout/v1/checkouts"
	pathRetrieveCheckout = "/checkout/v1/checkouts/%s"
	pathRefundPayment    = "/payments/v1/payments/%s/refunds"
	pathVoidPayment      = "/payments/v1/payments/%s/voids"
)

type Config struct {
	Env       string // sandbox / prod
	PublicKey string // Create 用
	SecretKey string // Retrieve / Refund / Void 用
	NotifyURL string // 自己收 webhook 的入口
}

type Adapter struct {
	cfg Config
	h   *http.Client
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, h: &http.Client{Timeout: 10 * time.Second}}
}

func (a *Adapter) Name() string { return "maya" }

func (a *Adapter) base() string {
	if a.cfg.Env == "prod" {
		return baseProd
	}
	return baseSandbox
}

// ---- Charge (create checkout) ---------------------------------------------

type createCheckoutReq struct {
	TotalAmount            amount      `json:"totalAmount"`
	Buyer                  buyer       `json:"buyer"`
	Items                  []itemEntry `json:"items"`
	RedirectURL            redirects   `json:"redirectUrl"`
	RequestReferenceNumber string      `json:"requestReferenceNumber"`
	Metadata               map[string]string `json:"metadata,omitempty"`
}

type amount struct {
	Value    float64 `json:"value"`
	Currency string  `json:"currency"`
	Details  map[string]float64 `json:"details,omitempty"`
}

type buyer struct {
	FirstName string  `json:"firstName"`
	LastName  string  `json:"lastName"`
	Contact   contact `json:"contact"`
}

type contact struct {
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
}

type itemEntry struct {
	Name        string `json:"name"`
	Quantity    int    `json:"quantity"`
	TotalAmount amount `json:"totalAmount"`
}

type redirects struct {
	Success string `json:"success"`
	Failure string `json:"failure"`
	Cancel  string `json:"cancel"`
}

type createCheckoutResp struct {
	CheckoutID  string `json:"checkoutId"`
	RedirectURL string `json:"redirectUrl"`
	// 错误响应
	Error            string `json:"error,omitempty"`
	Code             string `json:"code,omitempty"`
	Message          string `json:"message,omitempty"`
	ParameterErrors  []struct {
		Parameter string `json:"parameter"`
		Reason    string `json:"reason"`
	} `json:"parameters,omitempty"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	body := createCheckoutReq{
		TotalAmount: amount{
			Value:    float64(req.Amount) / 100,
			Currency: req.Currency,
			Details:  map[string]float64{"subtotal": float64(req.Amount) / 100},
		},
		Buyer: buyer{
			FirstName: req.Buyer.FirstName,
			LastName:  req.Buyer.LastName,
			Contact:   contact{Email: req.Buyer.Email, Phone: req.Buyer.Phone},
		},
		Items: []itemEntry{{
			Name: ifEmpty(req.Description, "Order"), Quantity: 1,
			TotalAmount: amount{Value: float64(req.Amount) / 100, Currency: req.Currency},
		}},
		RedirectURL: redirects{
			Success: req.ReturnURL + "?r=ok",
			Failure: req.ReturnURL + "?r=fail",
			Cancel:  req.ReturnURL + "?r=cancel",
		},
		RequestReferenceNumber: req.IdempotencyKey,
		Metadata:               req.Metadata,
	}
	var resp createCheckoutResp
	status, err := a.doJSON(ctx, http.MethodPost, pathCreateCheckout, a.cfg.PublicKey, body, &resp)
	if err != nil {
		return nil, err
	}
	if status >= 400 || resp.RedirectURL == "" {
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			FailureCode:    channel.MapFailure("maya", resp.Code),
			RawFailureCode: resp.Code,
			FailureMessage: resp.Message,
		}, nil
	}
	return &channel.ChargeResponse{
		Result:        channel.ResultRequiresAction,
		ExternalRefNo: resp.CheckoutID,
		RequiredAction: &channel.RequiredAction{
			Type:        "app_redirect",
			RedirectURL: resp.RedirectURL,
			Scheme:      "universal",
			ReturnURL:   req.ReturnURL,
			ExpiresAt:   time.Now().Add(30 * time.Minute),
		},
	}, nil
}

// ---- Capture（Maya 是自动 capture，此处仅做兼容） -------------------------

func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	// Checkout v2 的 Checkout 默认即时 capture；如需 manual capture 用 Payments Vault API。
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

// ---- Void -----------------------------------------------------------------

func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	var resp map[string]any
	status, err := a.doJSON(ctx, http.MethodPost,
		fmt.Sprintf(pathVoidPayment, req.ExternalRefNo), a.cfg.SecretKey,
		map[string]string{"reason": "requested_by_customer"}, &resp)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return &channel.OpResponse{Result: channel.ResultFailed,
			FailureCode: channel.FailUnknown}, nil
	}
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

// ---- Refund ---------------------------------------------------------------

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	body := map[string]any{
		"totalAmount":            map[string]any{"amount": float64(req.Amount) / 100, "currency": "PHP"},
		"requestReferenceNumber": req.IdempotencyKey,
		"reason":                 req.Reason,
	}
	var resp struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	status, err := a.doJSON(ctx, http.MethodPost,
		fmt.Sprintf(pathRefundPayment, req.ExternalRefNo), a.cfg.SecretKey, body, &resp)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return &channel.OpResponse{
			Result:         channel.ResultFailed,
			FailureCode:    channel.MapFailure("maya", resp.Code),
			RawFailureCode: resp.Code,
			FailureMessage: resp.Message,
		}, nil
	}
	// Maya 退款异步，成功状态从 webhook 再来。
	return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.ID}, nil
}

// ---- Query ----------------------------------------------------------------

type retrieveResp struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	PaymentStatus  string `json:"paymentStatus"`
	TotalAmount    amount `json:"totalAmount"`
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	var resp retrieveResp
	status, err := a.doJSON(ctx, http.MethodGet,
		fmt.Sprintf(pathRetrieveCheckout, req.ExternalRefNo), a.cfg.SecretKey, nil, &resp)
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return &channel.QueryResponse{Result: channel.ResultFailed}, nil
	}
	return &channel.QueryResponse{
		Result:         mapCheckoutStatus(resp.Status),
		ExternalRefNo:  resp.ID,
		AmountCaptured: int64(resp.TotalAmount.Value * 100),
	}, nil
}

func mapCheckoutStatus(s string) channel.ResultType {
	switch s {
	case "COMPLETED", "PAYMENT_SUCCESS", "CHECKOUT_SUCCESS":
		return channel.ResultSucceeded
	case "CREATED", "PENDING_PAYMENT":
		return channel.ResultProcessing
	case "EXPIRED", "CANCELLED", "CHECKOUT_DROPOUT":
		return channel.ResultFailed
	case "FAILED", "CHECKOUT_FAILURE":
		return channel.ResultFailed
	}
	return channel.ResultProcessing
}

// ---- Webhook --------------------------------------------------------------

type webhookBody struct {
	ID                     string `json:"id"`
	Status                 string `json:"status"`
	PaymentStatus          string `json:"paymentStatus"`
	RequestReferenceNumber string `json:"requestReferenceNumber"`
	TotalAmount            amount `json:"totalAmount"`
	CreatedAt              string `json:"createdAt"`
}

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	var wb webhookBody
	if err := json.Unmarshal(body, &wb); err != nil {
		return nil, err
	}
	// Maya v2 文档未规范签名头，使用 source IP allowlist + requestReferenceNumber 做
	// 二次校验（见 internal/service/webhook_service.go）。signatureOK 先置 true，
	// 由上层基于 IP 决定是否采信。
	evt := &channel.WebhookEvent{
		EventID:       wb.ID,
		PiID:          refToPiID(wb.RequestReferenceNumber),
		ExternalRefNo: wb.ID,
		Amount:        int64(wb.TotalAmount.Value * 100),
		Timestamp:     parseTime(wb.CreatedAt),
		SignatureOK:   true,
		Raw:           map[string]string{"status": wb.Status, "paymentStatus": wb.PaymentStatus},
	}
	switch wb.Status {
	case "COMPLETED", "PAYMENT_SUCCESS", "CHECKOUT_SUCCESS":
		evt.EventType = "charge.succeeded"
	case "FAILED", "CHECKOUT_FAILURE":
		evt.EventType = "charge.failed"
	case "EXPIRED", "CHECKOUT_DROPOUT", "CANCELLED":
		evt.EventType = "charge.failed"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	return evt, nil
}

// ---- helpers --------------------------------------------------------------

func (a *Adapter) doJSON(ctx context.Context, method, path, apiKey string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base()+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(apiKey+":")))
	resp, err := a.h.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func refToPiID(ref string) string {
	// requestReferenceNumber = sha256(pi_id + ":" + action)[:64]。
	// 这里无法反推 pi_id —— 真实实现中在落 acquirer_tx 时做 ref→pi_id 映射。
	return ref
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func ifEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}