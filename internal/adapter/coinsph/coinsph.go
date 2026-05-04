// Package coinsph 实现 Coins.ph Merchant Checkout adapter。
//
// 参考：https://docs.coins.asia/v2/docs/merchant-checkouts
// 鉴权：SHA1 shared secret digest。
package coinsph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseSandbox = "https://sandbox.coins.ph"
	baseProd    = "https://coins.ph"

	pathInvoice = "/pay/api/v3/invoices/"
	pathQuery   = "/pay/api/v3/invoices/%s"
	pathRefund  = "/pay/api/v3/invoices/%s/refunds"
)

type Config struct {
	Env        string
	MerchantID string
	SecretKey  string
	NotifyURL  string
	BaseURL string // 非空时覆盖 sandbox/prod，用于 mockserver
}

type Adapter struct {
	cfg Config
	h   *http.Client
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, h: channel.NewHTTPClient(15 * time.Second)}
}

func (a *Adapter) Name() string { return "coinsph" }

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

type invoiceReq struct {
	Merchant              string            `json:"merchant"`
	ExternalTransactionID string            `json:"external_transaction_id"`
	Currency              string            `json:"currency"`
	Amount                string            `json:"amount"`
	Description           string            `json:"description,omitempty"`
	CallbackURL           string            `json:"callback_url,omitempty"`
	Notes                 map[string]string `json:"notes,omitempty"`
}

type invoiceResp struct {
	InvoiceID   string `json:"invoice_id"`
	CheckoutURL string `json:"checkout_url"`
	Status      string `json:"status"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	body := invoiceReq{
		Merchant:              a.cfg.MerchantID,
		ExternalTransactionID: req.IdempotencyKey,
		Currency:              req.Currency,
		Amount:                formatAmount(req.Amount),
		Description:           req.Description,
		CallbackURL:           a.cfg.NotifyURL,
		Notes:                 map[string]string{"pi_id": req.PiID},
	}
	var resp invoiceResp
	if err := a.do(ctx, http.MethodPost, pathInvoice, body, &resp); err != nil {
		return nil, err
	}
	if resp.InvoiceID == "" {
		return &channel.ChargeResponse{Result: channel.ResultFailed, FailureCode: channel.FailUnknown}, nil
	}
	return &channel.ChargeResponse{
		Result:        channel.ResultRequiresAction,
		ExternalRefNo: resp.InvoiceID,
		RequiredAction: &channel.RequiredAction{
			Type:        "app_redirect",
			RedirectURL: resp.CheckoutURL,
			Scheme:      "universal",
			ReturnURL:   req.ReturnURL,
			ExpiresAt:   time.Now().Add(20 * time.Minute),
		},
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
	path := fmt.Sprintf(pathRefund, req.ExternalRefNo)
	body := map[string]any{
		"external_transaction_id": req.IdempotencyKey,
		"amount":                  formatAmount(req.Amount),
		"reason":                  req.Reason,
	}
	var resp struct {
		RefundID string `json:"refund_id"`
		Status   string `json:"status"`
	}
	if err := a.do(ctx, http.MethodPost, path, body, &resp); err != nil {
		return nil, err
	}
	if resp.Status == "succeeded" || resp.Status == "completed" {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.RefundID}, nil
	}
	return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.RefundID}, nil
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	path := fmt.Sprintf(pathQuery, req.ExternalRefNo)
	var resp struct {
		Status string `json:"status"`
		Amount string `json:"amount"`
	}
	if err := a.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch resp.Status {
	case "paid", "succeeded":
		rt = channel.ResultSucceeded
	case "failed", "expired", "canceled":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	amt, _ := strconv.ParseFloat(resp.Amount, 64)
	return &channel.QueryResponse{
		Result:         rt,
		ExternalRefNo:  req.ExternalRefNo,
		AmountCaptured: int64(amt * 100),
	}, nil
}

// ---- Webhook -----------------------------------------------------------

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	sig := headers["X-Coins-Signature"]
	sigOK := channel.ConstantTimeEqHex(sig, channel.SHA1Hex([]byte(a.cfg.SecretKey), body))
	var nb struct {
		InvoiceID             string `json:"invoice_id"`
		ExternalTransactionID string `json:"external_transaction_id"`
		Status                string `json:"status"`
		Amount                string `json:"amount"`
		PaidAt                string `json:"paid_at"`
	}
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}
	amt, _ := strconv.ParseFloat(nb.Amount, 64)
	evt := &channel.WebhookEvent{
		EventID:       nb.InvoiceID,
		PiID:          nb.ExternalTransactionID,
		ExternalRefNo: nb.InvoiceID,
		Amount:        int64(amt * 100),
		SignatureOK:   sigOK,
		Raw:           map[string]string{"status": nb.Status},
	}
	switch nb.Status {
	case "paid", "succeeded":
		evt.EventType = "charge.succeeded"
	case "failed", "canceled", "expired":
		evt.EventType = "charge.failed"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	if t, err := time.Parse(time.RFC3339, nb.PaidAt); err == nil {
		evt.Timestamp = t
	}
	return evt, nil
}

// ---- helpers ------------------------------------------------------------

func (a *Adapter) do(ctx context.Context, verb, path string, in, out any) error {
	var bodyBytes []byte
	if in != nil {
		var err error
		bodyBytes, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	sig := channel.SHA1Hex([]byte(a.cfg.SecretKey), bodyBytes)

	var reqBody *bytes.Reader
	if bodyBytes != nil {
		reqBody = bytes.NewReader(bodyBytes)
	}
	var req *http.Request
	var err error
	if reqBody == nil {
		req, err = http.NewRequestWithContext(ctx, verb, a.base()+path, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, verb, a.base()+path, reqBody)
	}
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Coins-Signature", sig)
	req.Header.Set("X-Coins-Merchant", a.cfg.MerchantID)

	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("coinsph: http %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func formatAmount(cents int64) string {
	return strconv.FormatFloat(float64(cents)/100.0, 'f', 2, 64)
}
