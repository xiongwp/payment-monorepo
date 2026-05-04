// Package coinsph 实现 Coins.ph Merchant Checkout adapter。
//
// 参考：https://docs.coins.ph/rest-api/
//       https://docs.coins.asia/reference/accepting-payments-with-the-merchant-api
//
// 鉴权：SHA1 shared-secret digest。Merchant 注册后拿 merchantId + secretKey，
// 每次请求的 digest = SHA1(secret + txnid + amount + currency) 放 X-Coins-Signature。
package coinsph

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseSandbox = "https://sandbox.coins.ph"
	baseProd    = "https://coins.ph"

	pathCreateInvoice = "/pay/api/v3/invoices/"
	pathGetInvoice    = "/pay/api/v3/invoices/%s/"
	pathRefund        = "/pay/api/v3/invoices/%s/refund/"
)

type Config struct {
	Env         string
	MerchantID  string
	SecretKey   string
	NotifyURL   string
}

type Adapter struct {
	cfg Config
	h   *http.Client
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, h: &http.Client{Timeout: 10 * time.Second}}
}

func (a *Adapter) Name() string { return "coinsph" }

func (a *Adapter) base() string {
	if a.cfg.Env == "prod" {
		return baseProd
	}
	return baseSandbox
}

// ---- Charge ---------------------------------------------------------------

type invoiceReq struct {
	Merchant              string            `json:"merchant"`
	ExternalTransactionID string            `json:"external_transaction_id"`
	Currency              string            `json:"currency"`
	Amount                string            `json:"amount"`
	Description           string            `json:"description"`
	CallbackURL           string            `json:"callback_url"`
	SuccessRedirectURL    string            `json:"success_redirect_url,omitempty"`
	FailureRedirectURL    string            `json:"failure_redirect_url,omitempty"`
	Notes                 map[string]string `json:"notes,omitempty"`
}

type invoiceResp struct {
	Invoice struct {
		ID           string `json:"id"`
		CheckoutURL  string `json:"checkout_url"`
		Status       string `json:"status"`
		CreatedAt    string `json:"created_at"`
		ExpiresAt    string `json:"expires_at"`
	} `json:"invoice"`
	Errors []struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"errors,omitempty"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	amount := fmt.Sprintf("%.2f", float64(req.Amount)/100)
	body := invoiceReq{
		Merchant:              a.cfg.MerchantID,
		ExternalTransactionID: req.IdempotencyKey,
		Currency:              req.Currency,
		Amount:                amount,
		Description:           req.Description,
		CallbackURL:           a.cfg.NotifyURL,
		SuccessRedirectURL:    req.ReturnURL + "?r=ok",
		FailureRedirectURL:    req.ReturnURL + "?r=fail",
		Notes:                 map[string]string{"pi_id": req.PiID},
	}
	var resp invoiceResp
	status, err := a.doSigned(ctx, http.MethodPost, pathCreateInvoice,
		req.IdempotencyKey, amount, req.Currency, body, &resp)
	if err != nil {
		return nil, err
	}
	if status >= 400 || resp.Invoice.CheckoutURL == "" {
		var code, msg string
		if len(resp.Errors) > 0 {
			code = resp.Errors[0].Code
			msg = resp.Errors[0].Detail
		}
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			FailureCode:    channel.MapFailure("coinsph", code),
			RawFailureCode: code,
			FailureMessage: msg,
		}, nil
	}
	return &channel.ChargeResponse{
		Result:        channel.ResultRequiresAction,
		ExternalRefNo: resp.Invoice.ID,
		RequiredAction: &channel.RequiredAction{
			Type:        "app_redirect",
			RedirectURL: resp.Invoice.CheckoutURL,
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

// ---- Refund ---------------------------------------------------------------

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	amount := fmt.Sprintf("%.2f", float64(req.Amount)/100)
	body := map[string]string{
		"amount":                  amount,
		"currency":                "PHP",
		"reason":                  req.Reason,
		"external_transaction_id": req.IdempotencyKey,
	}
	var resp struct {
		Refund struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"refund"`
		Errors []struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"errors,omitempty"`
	}
	path := fmt.Sprintf(pathRefund, req.ExternalRefNo)
	status, err := a.doSigned(ctx, http.MethodPost, path, req.IdempotencyKey, amount, "PHP", body, &resp)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		var code string
		if len(resp.Errors) > 0 {
			code = resp.Errors[0].Code
		}
		return &channel.OpResponse{Result: channel.ResultFailed,
			FailureCode: channel.MapFailure("coinsph", code), RawFailureCode: code}, nil
	}
	if resp.Refund.Status == "completed" {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.Refund.ID}, nil
	}
	return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.Refund.ID}, nil
}

// ---- Query ----------------------------------------------------------------

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	var resp struct {
		Invoice struct {
			ID     string `json:"id"`
			Status string `json:"status"` // pending / paid / expired / cancelled
			Amount string `json:"amount"`
		} `json:"invoice"`
	}
	path := fmt.Sprintf(pathGetInvoice, url.PathEscape(req.ExternalRefNo))
	_, err := a.doSigned(ctx, http.MethodGet, path, req.ExternalRefNo, "", "", nil, &resp)
	if err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch strings.ToLower(resp.Invoice.Status) {
	case "paid", "completed":
		rt = channel.ResultSucceeded
	case "expired", "cancelled", "failed":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{Result: rt, ExternalRefNo: resp.Invoice.ID}, nil
}

// ---- Webhook --------------------------------------------------------------

type callbackBody struct {
	Invoice struct {
		ID                    string `json:"id"`
		Status                string `json:"status"`
		ExternalTransactionID string `json:"external_transaction_id"`
		Amount                string `json:"amount"`
		Currency              string `json:"currency"`
		PaidAt                string `json:"paid_at"`
	} `json:"invoice"`
}

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	// Coins 回调验签头：X-Coins-Signature = SHA1(secret + body)
	sig := headers["X-Coins-Signature"]
	ok := verifyDigest(a.cfg.SecretKey, body, sig)
	var cb callbackBody
	if err := json.Unmarshal(body, &cb); err != nil {
		return nil, err
	}
	evt := &channel.WebhookEvent{
		EventID:       cb.Invoice.ID,
		PiID:          cb.Invoice.ExternalTransactionID, // 由上层 ref → pi_id 映射
		ExternalRefNo: cb.Invoice.ID,
		Timestamp:     parseTime(cb.Invoice.PaidAt),
		SignatureOK:   ok,
		Raw:           map[string]string{"status": cb.Invoice.Status},
	}
	switch strings.ToLower(cb.Invoice.Status) {
	case "paid", "completed":
		evt.EventType = "charge.succeeded"
	case "expired", "cancelled", "failed":
		evt.EventType = "charge.failed"
	case "refunded":
		evt.EventType = "refund.succeeded"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	return evt, nil
}

// ---- digest ---------------------------------------------------------------

func (a *Adapter) doSigned(ctx context.Context, method, path, txnID, amount, currency string, in, out any) (int, error) {
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
	req.Header.Set("X-Coins-Merchant", a.cfg.MerchantID)
	req.Header.Set("X-Coins-Signature", digestString(a.cfg.SecretKey+txnID+amount+currency))
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

func digestString(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func verifyDigest(secret string, body []byte, sig string) bool {
	h := sha1.New()
	_, _ = h.Write([]byte(secret))
	_, _ = h.Write(body)
	expect := hex.EncodeToString(h.Sum(nil))
	return strings.EqualFold(expect, sig)
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}
