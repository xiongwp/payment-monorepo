// Package maya 实现 Maya Checkout v2 adapter（Basic Auth + hosted page）。
//
// 参考：https://developers.maya.ph/
// Checkout v2 规格：https://s3-us-west-2.amazonaws.com/developers.paymaya.com.pg/checkout/v2/Checkout+API.html
package maya

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseSandbox = "https://pg-sandbox.paymaya.com"
	baseProd    = "https://pg.paymaya.com"

	pathCreateCheckout = "/checkout/v1/checkouts"
	pathGetCheckout    = "/checkout/v1/checkouts/%s"
	pathRefund         = "/payments/v1/payments/%s/refunds"
	pathVoid           = "/payments/v1/payments/%s/voids"
)

type Config struct {
	Env           string
	PublicKey     string
	SecretKey     string
	RedirectOK    string
	RedirectFail  string
	RedirectCxl   string
	BaseURL string // 非空时覆盖 sandbox/prod，用于 mockserver
}

type Adapter struct {
	cfg Config
	h   *http.Client
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, h: channel.NewHTTPClient(15 * time.Second)}
}

func (a *Adapter) Name() string { return "maya" }

func (a *Adapter) base() string {
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	if a.cfg.Env == "prod" {
		return baseProd
	}
	return baseSandbox
}

// ---- Charge --------------------------------------------------------------

// minorAmount 用 int64 的"分单位"承载金额，序列化时直接构造精确的两位小数
// JSON number 字面量。**避免 float64(amt)/100 的二进制精度丢失**：
//
//	float64(10001) / 100 = 100.00999999999999 (而非 100.01)
//
// JSON 编码器再把它写出去时部分实现会保留一长串 9，被 Maya 严格校验拒绝；
// 即使没拒，也会给后续对账造成 1 cent 抖动。
//
// 单位语义：等于 ISO minor units（cents），与 channel.ChargeRequest.Amount
// / RefundRequest.Amount 一致。负值原样支持（保留符号）。
type minorAmount int64

// MarshalJSON 把 cents 写成"主单位.分单位"形式，例如 10001 → 100.01。
// 不走 float 中转。
func (m minorAmount) MarshalJSON() ([]byte, error) {
	n := int64(m)
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	major := n / 100
	cents := n % 100
	return []byte(fmt.Sprintf("%s%d.%02d", sign, major, cents)), nil
}

type mayaAmount struct {
	Value    minorAmount    `json:"value"`
	Currency string         `json:"currency"`
	Details  map[string]any `json:"details,omitempty"`
}

type mayaCheckoutReq struct {
	TotalAmount            mayaAmount       `json:"totalAmount"`
	Buyer                  map[string]any   `json:"buyer,omitempty"`
	Items                  []map[string]any `json:"items,omitempty"`
	RedirectURL            map[string]string `json:"redirectUrl"`
	RequestReferenceNumber string           `json:"requestReferenceNumber"`
	Metadata               map[string]any   `json:"metadata,omitempty"`
}

type mayaCheckoutResp struct {
	CheckoutID  string `json:"checkoutId"`
	RedirectURL string `json:"redirectUrl"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	body := mayaCheckoutReq{
		TotalAmount: mayaAmount{
			Value:    minorAmount(req.Amount),
			Currency: req.Currency,
		},
		RedirectURL: map[string]string{
			"success": orDefault(req.ReturnURL, a.cfg.RedirectOK),
			"failure": orDefault(req.ReturnURL, a.cfg.RedirectFail),
			"cancel":  orDefault(req.ReturnURL, a.cfg.RedirectCxl),
		},
		RequestReferenceNumber: req.IdempotencyKey,
		Metadata:               map[string]any{"pi_id": req.PiID},
	}
	if req.Buyer.FirstName != "" || req.Buyer.Email != "" {
		body.Buyer = map[string]any{
			"firstName": req.Buyer.FirstName,
			"lastName":  req.Buyer.LastName,
			"contact": map[string]string{
				"email": req.Buyer.Email,
				"phone": req.Buyer.Phone,
			},
		}
	}
	var resp mayaCheckoutResp
	if err := a.doBasic(ctx, a.cfg.PublicKey, http.MethodPost, pathCreateCheckout, body, &resp); err != nil {
		return nil, err
	}
	if resp.CheckoutID == "" {
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			FailureCode:    channel.FailUnknown,
			RawFailureCode: "no_checkout_id",
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

func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	path := fmt.Sprintf(pathVoid, req.ExternalRefNo)
	var resp map[string]any
	if err := a.doBasic(ctx, a.cfg.SecretKey, http.MethodPost, path, map[string]string{
		"reason":                 "requested_by_customer",
		"requestReferenceNumber": req.IdempotencyKey,
	}, &resp); err != nil {
		return nil, err
	}
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	path := fmt.Sprintf(pathRefund, req.ExternalRefNo)
	body := map[string]any{
		"totalAmount": mayaAmount{
			Value:    minorAmount(req.Amount),
			Currency: "PHP",
		},
		"requestReferenceNumber": req.IdempotencyKey,
		"reason":                 req.Reason,
	}
	var resp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := a.doBasic(ctx, a.cfg.SecretKey, http.MethodPost, path, body, &resp); err != nil {
		return nil, err
	}
	if resp.Status == "SUCCESS" || resp.Status == "SUCCEEDED" {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.ID}, nil
	}
	return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.ID}, nil
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	path := fmt.Sprintf(pathGetCheckout, req.ExternalRefNo)
	var resp struct {
		Status string     `json:"status"` // CREATED / PAYMENT_SUCCESS / PAYMENT_FAILED / CHECKOUT_EXPIRED
		Amount mayaAmount `json:"totalAmount"`
	}
	if err := a.doBasic(ctx, a.cfg.SecretKey, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch resp.Status {
	case "PAYMENT_SUCCESS", "PAID":
		rt = channel.ResultSucceeded
	case "PAYMENT_FAILED", "CHECKOUT_EXPIRED":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{
		Result:         rt,
		ExternalRefNo:  req.ExternalRefNo,
		AmountCaptured: int64(resp.Amount.Value * 100),
	}, nil
}

// ---- Webhook -------------------------------------------------------------

// Maya v2 不签回调 —— 靠 IP 白名单 + requestReferenceNumber 对账兜底。
func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	var nb struct {
		ID                     string     `json:"id"`
		RequestReferenceNumber string     `json:"requestReferenceNumber"`
		PaymentStatus          string     `json:"paymentStatus"`
		Status                 string     `json:"status"`
		Amount                 mayaAmount `json:"amount"`
		CreatedAt              string     `json:"createdAt"`
	}
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}
	evt := &channel.WebhookEvent{
		EventID:       firstNonEmpty(nb.ID, nb.RequestReferenceNumber),
		PiID:          nb.RequestReferenceNumber,
		ExternalRefNo: nb.ID,
		Amount:        int64(nb.Amount.Value * 100),
		SignatureOK:   true, // 依赖外部 IP 白名单
		Raw: map[string]string{
			"status":        nb.Status,
			"paymentStatus": nb.PaymentStatus,
		},
	}
	state := firstNonEmpty(nb.PaymentStatus, nb.Status)
	switch state {
	case "PAYMENT_SUCCESS", "CHECKOUT_SUCCESS", "PAID", "SUCCESS":
		evt.EventType = "charge.succeeded"
	case "PAYMENT_FAILED", "CHECKOUT_FAILURE", "FAILED":
		evt.EventType = "charge.failed"
	case "CHECKOUT_EXPIRED":
		evt.EventType = "charge.failed"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	if t, err := time.Parse(time.RFC3339, nb.CreatedAt); err == nil {
		evt.Timestamp = t
	}
	return evt, nil
}

// ---- helpers ------------------------------------------------------------

func (a *Adapter) doBasic(ctx context.Context, key, verb, path string, in, out any) error {
	var rdr *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	url := a.base() + path
	var reqBody *bytes.Reader = rdr
	var req *http.Request
	var err error
	if reqBody == nil {
		req, err = http.NewRequestWithContext(ctx, verb, url, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, verb, url, reqBody)
	}
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(key+":")))
	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("maya: http %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func orDefault(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
