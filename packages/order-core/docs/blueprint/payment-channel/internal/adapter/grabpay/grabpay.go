//go:build ignore
// +build ignore

// Package grabpay 实现 GrabPay OTC（One-Time Charge）adapter。
//
// 参考：https://developer.grab.com/
//       https://github.com/grab/grabpay-merchant-sdk
//
// 鉴权：OAuth2 Bearer + 每请求 HMAC-SHA256 签名。
// 签名内容：HTTP-Method + "\n" + Content-Type + "\n" + Date(RFC1123) + "\n" +
//          Path + "\n" + Base64(SHA256(Body))
// 签名 header：X-GID-AUX-Signature / X-GID-AUX-Date
package grabpay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseSandbox = "https://partner-api.stg-myteksi.com"
	baseProd    = "https://partner-api.grab.com"

	pathChargeInit   = "/grabpay/partner/v2/charge/init"
	pathChargeCancel = "/grabpay/partner/v2/charge/cancel"
	pathRefund       = "/grabpay/partner/v2/refund"
	pathQuery        = "/grabpay/partner/v2/charge/status"
)

type Config struct {
	Env           string
	PartnerID     string
	PartnerSecret string
	MerchantID    string
	AccessToken   string // OAuth token，由外层定时刷新
	NotifyURL     string
}

type Adapter struct {
	cfg Config
	h   *http.Client
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, h: &http.Client{Timeout: 10 * time.Second}}
}

func (a *Adapter) Name() string { return "grabpay" }

func (a *Adapter) base() string {
	if a.cfg.Env == "prod" {
		return baseProd
	}
	return baseSandbox
}

// ---- Charge ---------------------------------------------------------------

type initReq struct {
	PartnerTxID      string `json:"partnerTxID"`
	PartnerGroupTxID string `json:"partnerGroupTxID"`
	Amount           int64  `json:"amount"`
	Currency         string `json:"currency"`
	Description      string `json:"description"`
	MerchantID       string `json:"merchantID"`
}

type initResp struct {
	TxID        string `json:"txID"`
	State       string `json:"state"` // PENDING / SUCCESS / FAIL
	RedirectURL string `json:"redirectURL"`
	Code        string `json:"code"`
	Reason      string `json:"reason"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	body := initReq{
		PartnerTxID:      req.IdempotencyKey,
		PartnerGroupTxID: req.PiID,
		Amount:           req.Amount,
		Currency:         req.Currency,
		Description:      req.Description,
		MerchantID:       a.cfg.MerchantID,
	}
	var resp initResp
	status, err := a.doSigned(ctx, http.MethodPost, pathChargeInit, body, &resp)
	if err != nil {
		return nil, err
	}
	if status >= 400 || resp.State == "FAIL" {
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			FailureCode:    channel.MapFailure("grabpay", resp.Code),
			RawFailureCode: resp.Code,
			FailureMessage: resp.Reason,
		}, nil
	}
	return &channel.ChargeResponse{
		Result:        channel.ResultRequiresAction,
		ExternalRefNo: resp.TxID,
		RequiredAction: &channel.RequiredAction{
			Type:        "app_redirect",
			RedirectURL: resp.RedirectURL,
			Scheme:      "universal",
			ReturnURL:   req.ReturnURL,
			ExpiresAt:   time.Now().Add(10 * time.Minute),
		},
	}, nil
}

func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	var resp struct {
		State string `json:"state"`
		Code  string `json:"code"`
	}
	status, err := a.doSigned(ctx, http.MethodPost, pathChargeCancel,
		map[string]string{"txID": req.ExternalRefNo, "partnerTxID": req.IdempotencyKey}, &resp)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return &channel.OpResponse{Result: channel.ResultFailed, FailureCode: channel.FailUnknown}, nil
	}
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

// ---- Refund ---------------------------------------------------------------

type refundReq struct {
	OrigPartnerTxID string `json:"origPartnerTxID"`
	PartnerTxID     string `json:"partnerTxID"`
	Amount          int64  `json:"amount"`
	Currency        string `json:"currency"`
	Description     string `json:"description"`
	MerchantID      string `json:"merchantID"`
}

type refundResp struct {
	TxID  string `json:"txID"`
	State string `json:"state"`
	Code  string `json:"code"`
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	body := refundReq{
		OrigPartnerTxID: req.ExternalRefNo,
		PartnerTxID:     req.IdempotencyKey,
		Amount:          req.Amount,
		Currency:        "PHP",
		Description:     req.Reason,
		MerchantID:      a.cfg.MerchantID,
	}
	var resp refundResp
	if _, err := a.doSigned(ctx, http.MethodPost, pathRefund, body, &resp); err != nil {
		return nil, err
	}
	if resp.State == "SUCCESS" {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.TxID}, nil
	}
	if resp.State == "PENDING" {
		return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.TxID}, nil
	}
	return &channel.OpResponse{Result: channel.ResultFailed, FailureCode: channel.MapFailure("grabpay", resp.Code), RawFailureCode: resp.Code}, nil
}

// ---- Query ----------------------------------------------------------------

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	var resp struct {
		TxID  string `json:"txID"`
		State string `json:"state"`
		Amount int64 `json:"amount"`
	}
	if _, err := a.doSigned(ctx, http.MethodPost, pathQuery,
		map[string]string{"txID": req.ExternalRefNo}, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch resp.State {
	case "SUCCESS":
		rt = channel.ResultSucceeded
	case "FAIL":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{Result: rt, ExternalRefNo: resp.TxID, AmountCaptured: resp.Amount}, nil
}

// ---- Webhook --------------------------------------------------------------

type notifyBody struct {
	TxID        string `json:"txID"`
	PartnerTxID string `json:"partnerTxID"`
	State       string `json:"state"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Type        string `json:"type"` // CHARGE / REFUND
	Code        string `json:"code"`
	Timestamp   int64  `json:"timestamp"`
}

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	sigOK := a.verifySign(body, headers["X-Signature"])
	var nb notifyBody
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}
	evt := &channel.WebhookEvent{
		EventID:       nb.TxID,
		PiID:          nb.PartnerTxID,
		ExternalRefNo: nb.TxID,
		Amount:        nb.Amount,
		Timestamp:     time.Unix(nb.Timestamp, 0),
		SignatureOK:   sigOK,
		Raw:           map[string]string{"state": nb.State, "code": nb.Code},
	}
	ok := nb.State == "SUCCESS"
	switch {
	case nb.Type == "REFUND" && ok:
		evt.EventType = "refund.succeeded"
	case nb.Type == "REFUND":
		evt.EventType = "refund.failed"
	case ok:
		evt.EventType = "charge.succeeded"
	default:
		evt.EventType = "charge.failed"
	}
	return evt, nil
}

// ---- HTTP + signing -------------------------------------------------------

func (a *Adapter) doSigned(ctx context.Context, method, path string, in, out any) (int, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	date := time.Now().UTC().Format(http.TimeFormat)
	bodyHash := sha256.Sum256(b)
	canonical := fmt.Sprintf("%s\napplication/json\n%s\n%s\n%s",
		method, date, path, base64.StdEncoding.EncodeToString(bodyHash[:]))
	mac := hmac.New(sha256.New, []byte(a.cfg.PartnerSecret))
	_, _ = mac.Write([]byte(canonical))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, method, a.base()+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.cfg.AccessToken)
	req.Header.Set("X-GID-AUX-Date", date)
	req.Header.Set("X-GID-AUX-Signature", sig)
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

func (a *Adapter) verifySign(body []byte, hdr string) bool {
	mac := hmac.New(sha256.New, []byte(a.cfg.PartnerSecret))
	_, _ = mac.Write(body)
	expect := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expect), []byte(hdr))
}