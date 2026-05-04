// Package xendit 实现 Xendit Invoice API adapter。
//
// Xendit 在 PH 覆盖 Virtual Account / E-Wallet (GCash/PayMaya/GrabPay) /
// Retail Outlets / QR Ph / Cards。
//
// 参考：https://developers.xendit.co/api-reference
// 鉴权：HTTP Basic Auth，username = secret key (xnd_development_... /
// xnd_production_...)，password 为空。
package xendit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseURL = "https://api.xendit.co"

	pathCreateInvoice = "/v2/invoices"
	pathGetInvoice    = "/v2/invoices/%s"
	pathRefund        = "/refunds"
)

type Config struct {
	SecretKey         string
	VerificationToken string // x-callback-token
	Env               string
	BaseURL           string // 非空时覆盖 baseURL，用于 mockserver / 代理
}

type Adapter struct {
	cfg Config
	h   *http.Client
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, h: channel.NewHTTPClient(20 * time.Second)}
}

func (a *Adapter) base() string {
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	return baseURL
}

func (a *Adapter) Name() string { return "xendit" }

// ---- Charge --------------------------------------------------------------

type invoiceReq struct {
	ExternalID         string   `json:"external_id"`
	Amount             int64    `json:"amount"`
	PayerEmail         string   `json:"payer_email,omitempty"`
	Description        string   `json:"description,omitempty"`
	SuccessRedirectURL string   `json:"success_redirect_url,omitempty"`
	FailureRedirectURL string   `json:"failure_redirect_url,omitempty"`
	Currency           string   `json:"currency"`
	PaymentMethods     []string `json:"payment_methods,omitempty"`
	InvoiceDuration    int      `json:"invoice_duration,omitempty"`
	Customer           *struct {
		GivenNames  string `json:"given_names,omitempty"`
		Surname     string `json:"surname,omitempty"`
		Email       string `json:"email,omitempty"`
		MobileNumber string `json:"mobile_number,omitempty"`
	} `json:"customer,omitempty"`
}

type invoiceResp struct {
	ID         string `json:"id"`
	InvoiceURL string `json:"invoice_url"`
	Status     string `json:"status"`
	Amount     int64  `json:"amount"`
	ExternalID string `json:"external_id"`
	ErrorCode  string `json:"error_code"`
	Message    string `json:"message"`
}

var defaultMethods = []string{"GCASH", "GRABPAY", "PAYMAYA", "QRPH", "CARDS"}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	methods := defaultMethods
	if req.Metadata != nil {
		if m := req.Metadata["xendit_payment_method"]; m != "" {
			methods = splitAndUpper(m)
		}
	}
	body := invoiceReq{
		ExternalID:         req.IdempotencyKey,
		Amount:             req.Amount / 100, // Xendit 用整数主币（PHP），非分
		PayerEmail:         req.Buyer.Email,
		Description:        req.Description,
		SuccessRedirectURL: req.ReturnURL,
		FailureRedirectURL: req.ReturnURL,
		Currency:           firstNonEmpty(req.Currency, "PHP"),
		PaymentMethods:     methods,
		InvoiceDuration:    900,
	}
	if req.Buyer.FirstName != "" || req.Buyer.Email != "" {
		body.Customer = &struct {
			GivenNames   string `json:"given_names,omitempty"`
			Surname      string `json:"surname,omitempty"`
			Email        string `json:"email,omitempty"`
			MobileNumber string `json:"mobile_number,omitempty"`
		}{
			GivenNames:   req.Buyer.FirstName,
			Surname:      req.Buyer.LastName,
			Email:        req.Buyer.Email,
			MobileNumber: req.Buyer.Phone,
		}
	}

	var resp invoiceResp
	if err := a.doBasic(ctx, http.MethodPost, pathCreateInvoice, req.IdempotencyKey, body, &resp); err != nil {
		return nil, err
	}
	if resp.ID == "" {
		raw := resp.ErrorCode
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			FailureCode:    channel.MapFailure(a.Name(), raw),
			RawFailureCode: raw,
			FailureMessage: resp.Message,
		}, nil
	}
	return &channel.ChargeResponse{
		Result:        channel.ResultRequiresAction,
		ExternalRefNo: resp.ID,
		RequiredAction: &channel.RequiredAction{
			Type:        "app_redirect",
			RedirectURL: resp.InvoiceURL,
			Scheme:      "universal",
			ReturnURL:   req.ReturnURL,
			ExpiresAt:   time.Now().Add(15 * time.Minute),
		},
		Raw: map[string]string{"status": resp.Status},
	}, nil
}

func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

// ---- Refund --------------------------------------------------------------

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	body := map[string]any{
		"payment_id":  req.ExternalRefNo,
		"amount":      req.Amount / 100,
		"reason":      firstNonEmpty(req.Reason, "REQUESTED_BY_CUSTOMER"),
		"external_id": req.IdempotencyKey,
	}
	var resp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := a.doBasic(ctx, http.MethodPost, pathRefund, req.IdempotencyKey, body, &resp); err != nil {
		return nil, err
	}
	switch strings.ToUpper(resp.Status) {
	case "SUCCEEDED", "SUCCESS":
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.ID}, nil
	case "FAILED":
		return &channel.OpResponse{Result: channel.ResultFailed, ExternalRefNo: resp.ID}, nil
	default:
		return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.ID}, nil
	}
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	path := fmt.Sprintf(pathGetInvoice, req.ExternalRefNo)
	var resp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Amount int64  `json:"amount"`
	}
	if err := a.doBasic(ctx, http.MethodGet, path, "", nil, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch strings.ToUpper(resp.Status) {
	case "PAID", "SETTLED":
		rt = channel.ResultSucceeded
	case "EXPIRED", "FAILED":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{
		Result:         rt,
		ExternalRefNo:  resp.ID,
		AmountCaptured: resp.Amount * 100, // 再转回分
		Raw:            map[string]string{"status": resp.Status},
	}, nil
}

// ---- Webhook -------------------------------------------------------------
//
// Xendit 用不透明 verification token 校验：
//   x-callback-token: <token>
// 不是 HMAC，直接做常数时间等值比较。
func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	token := firstHeader(headers, "x-callback-token", "X-Callback-Token")
	sigOK := a.cfg.VerificationToken != "" &&
		len(token) == len(a.cfg.VerificationToken) &&
		channel.ConstantTimeEqHex(
			encHex(token), encHex(a.cfg.VerificationToken),
		)

	var nb struct {
		ID         string `json:"id"`
		ExternalID string `json:"external_id"`
		Status     string `json:"status"`
		Amount     int64  `json:"amount"`
		PaidAt     string `json:"paid_at"`
		Updated    string `json:"updated"`
	}
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}

	evt := &channel.WebhookEvent{
		EventID:       nb.ID,
		PiID:          nb.ExternalID,
		ExternalRefNo: nb.ID,
		Amount:        nb.Amount * 100,
		SignatureOK:   sigOK,
		Raw:           map[string]string{"status": nb.Status},
	}
	switch strings.ToUpper(nb.Status) {
	case "PAID", "SETTLED":
		evt.EventType = "charge.succeeded"
	case "EXPIRED", "FAILED":
		evt.EventType = "charge.failed"
	case "REFUNDED":
		evt.EventType = "refund.succeeded"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	if t, err := time.Parse(time.RFC3339, firstNonEmpty(nb.PaidAt, nb.Updated)); err == nil {
		evt.Timestamp = t
	}
	return evt, nil
}

// ---- helpers -------------------------------------------------------------

func (a *Adapter) doBasic(ctx context.Context, verb, path, idem string, in, out any) error {
	var reqBody *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	url := a.base() + path
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
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(a.cfg.SecretKey+":")))
	if idem != "" {
		req.Header.Set("X-Idempotency-Key", idem)
	}
	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("xendit: http %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func splitAndUpper(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToUpper(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func firstHeader(h map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := h[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

// encHex 把任意字符串做 hex 编码，供 ConstantTimeEqHex 使用。
func encHex(s string) string {
	const hexdig = "0123456789abcdef"
	b := []byte(s)
	out := make([]byte, 2*len(b))
	for i, c := range b {
		out[2*i] = hexdig[c>>4]
		out[2*i+1] = hexdig[c&0x0f]
	}
	return string(out)
}
