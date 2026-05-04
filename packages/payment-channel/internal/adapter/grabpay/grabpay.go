// Package grabpay 实现 GrabPay OTC adapter（Merchant Partner API）。
//
// 参考：https://developer.grab.com/
// 鉴权：partnerSecret + HMAC-SHA256（X-GID-AUX-Signature）。
package grabpay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseSandbox = "https://partner-api.stg-myteksi.com"
	baseProd    = "https://partner-api.grab.com"

	pathCharge = "/grabpay/partner/v2/charge/init"
	pathRefund = "/grabpay/partner/v2/refund"
	pathQuery  = "/grabpay/partner/v2/charge/status"
)

type Config struct {
	Env           string
	PartnerID     string
	PartnerSecret string
	MerchantID    string
	NotifyURL     string
	BaseURL string // 非空时覆盖 sandbox/prod，用于 mockserver
}

type Adapter struct {
	cfg Config
	h   *http.Client
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, h: channel.NewHTTPClient(15 * time.Second)}
}

func (a *Adapter) Name() string { return "grabpay" }

func (a *Adapter) base() string {
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	if a.cfg.Env == "prod" {
		return baseProd
	}
	return baseSandbox
}

// ---- Charge ---------------------------------------------------------------

type chargeInit struct {
	PartnerTxID      string `json:"partnerTxID"`
	PartnerGroupTxID string `json:"partnerGroupTxID"`
	Amount           int64  `json:"amount"`
	Currency         string `json:"currency"`
	Description      string `json:"description"`
	MerchantID       string `json:"merchantID"`
	ReturnURL        string `json:"returnUrl,omitempty"`
}

type chargeInitResp struct {
	TxID       string `json:"txID"`
	State      string `json:"state"`
	Request    string `json:"request"`
	DeepLink   string `json:"deepLink"`
	RedirectURL string `json:"redirectUrl"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	body := chargeInit{
		PartnerTxID:      req.IdempotencyKey,
		PartnerGroupTxID: req.PiID,
		Amount:           req.Amount,
		Currency:         req.Currency,
		Description:      req.Description,
		MerchantID:       a.cfg.MerchantID,
		ReturnURL:        req.ReturnURL,
	}
	var resp chargeInitResp
	if err := a.doSigned(ctx, http.MethodPost, pathCharge, body, &resp); err != nil {
		return nil, err
	}
	if resp.TxID == "" {
		return &channel.ChargeResponse{Result: channel.ResultFailed, FailureCode: channel.FailUnknown}, nil
	}
	if resp.State == "SUCCESS" {
		return &channel.ChargeResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.TxID}, nil
	}
	return &channel.ChargeResponse{
		Result:        channel.ResultRequiresAction,
		ExternalRefNo: resp.TxID,
		RequiredAction: &channel.RequiredAction{
			Type:        "app_redirect",
			RedirectURL: firstNonEmpty(resp.DeepLink, resp.RedirectURL),
			Scheme:      "universal",
			ReturnURL:   req.ReturnURL,
			ExpiresAt:   time.Now().Add(15 * time.Minute),
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

type refundReq struct {
	PartnerTxID     string `json:"partnerTxID"`
	OriginTxID      string `json:"originTxID"`
	Amount          int64  `json:"amount"`
	Currency        string `json:"currency"`
	Description     string `json:"description,omitempty"`
	MerchantID      string `json:"merchantID"`
}

type refundResp struct {
	TxID  string `json:"txID"`
	State string `json:"state"`
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	body := refundReq{
		PartnerTxID: req.IdempotencyKey,
		OriginTxID:  req.ExternalRefNo,
		Amount:      req.Amount,
		Currency:    "PHP",
		Description: req.Reason,
		MerchantID:  a.cfg.MerchantID,
	}
	var resp refundResp
	if err := a.doSigned(ctx, http.MethodPost, pathRefund, body, &resp); err != nil {
		return nil, err
	}
	if resp.State == "SUCCESS" {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.TxID}, nil
	}
	return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.TxID}, nil
}

// ---- Query --------------------------------------------------------------

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	var resp struct {
		TxID   string `json:"txID"`
		State  string `json:"state"`
		Amount int64  `json:"amount"`
	}
	if err := a.doSigned(ctx, http.MethodGet, pathQuery+"/"+req.ExternalRefNo, nil, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch resp.State {
	case "SUCCESS":
		rt = channel.ResultSucceeded
	case "FAIL", "FAILED":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{Result: rt, ExternalRefNo: resp.TxID, AmountCaptured: resp.Amount}, nil
}

// ---- Webhook -----------------------------------------------------------

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	sig := headers["X-Signature"]
	sigOK := channel.ConstantTimeEqHex(sig, channel.HMACSHA256Hex([]byte(a.cfg.PartnerSecret), body))
	var nb struct {
		Type        string `json:"type"` // CHARGE_SUCCESS / CHARGE_FAIL / REFUND_SUCCESS
		TxID        string `json:"txID"`
		PartnerTxID string `json:"partnerTxID"`
		Amount      int64  `json:"amount"`
	}
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}
	evt := &channel.WebhookEvent{
		EventID:       nb.TxID,
		PiID:          nb.PartnerTxID,
		ExternalRefNo: nb.TxID,
		Amount:        nb.Amount,
		SignatureOK:   sigOK,
		Timestamp:     time.Now(),
		Raw:           map[string]string{"type": nb.Type},
	}
	switch nb.Type {
	case "CHARGE_SUCCESS":
		evt.EventType = "charge.succeeded"
	case "CHARGE_FAIL":
		evt.EventType = "charge.failed"
	case "REFUND_SUCCESS":
		evt.EventType = "refund.succeeded"
	case "REFUND_FAIL":
		evt.EventType = "refund.failed"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	return evt, nil
}

// ---- signing ------------------------------------------------------------

func (a *Adapter) doSigned(ctx context.Context, verb, path string, in, out any) error {
	var bodyBytes []byte
	if in != nil {
		var err error
		bodyBytes, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	bodyHash := sha256.Sum256(bodyBytes)
	date := time.Now().UTC().Format(http.TimeFormat)
	canonical := fmt.Sprintf("%s\napplication/json\n%s\n%s\n%s",
		verb, date, path, base64.StdEncoding.EncodeToString(bodyHash[:]))
	sig := channel.HMACSHA256Base64([]byte(a.cfg.PartnerSecret), []byte(canonical))

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
	req.Header.Set("X-GID-AUX-Date", date)
	req.Header.Set("X-GID-AUX-Signature", sig)
	req.Header.Set("X-GID-AUX-PartnerID", a.cfg.PartnerID)

	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("grabpay: http %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
