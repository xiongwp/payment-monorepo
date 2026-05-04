// Package metrobank 实现 Metrobank M Developer Portal adapter（Direct Debit / QR Ph / Bills Payment）。
//
// 参考：https://mdeveloper.metrobank.com.ph/
// 鉴权：OAuth2 client credentials + IBM API Gateway headers：
//   X-IBM-Client-Id / X-IBM-Client-Secret / X-Correlation-ID / X-Metrobank-Signature
// 其中 X-Metrobank-Signature = HMAC-SHA256 hex(partner_secret, body)
// 银行侧同步返回，没有 pre-auth，Capture/Void 做 no-op。
package metrobank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseSandbox = "https://sandbox-api.metrobank.com.ph"
	baseProd    = "https://api.metrobank.com.ph"

	pathToken   = "/oauth2/v1/token"
	pathDebit   = "/partners/v1/payments/debit"
	pathRefund  = "/partners/v1/payments/refund"
	pathQuery   = "/partners/v1/payments/%s"

	scope = "payments"
)

type Config struct {
	Env          string
	ClientID     string
	ClientSecret string
	PartnerID    string
	PartnerSec   string // HMAC 签名使用的 partner secret（可与 ClientSecret 不同）
	BaseURL string // 非空时覆盖 sandbox/prod，用于 mockserver
}

type Adapter struct {
	cfg         Config
	h           *http.Client
	tokenMu     sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, h: channel.NewHTTPClient(20 * time.Second)}
}

func (a *Adapter) Name() string { return "metrobank" }

func (a *Adapter) base() string {
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	if a.cfg.Env == "prod" {
		return baseProd
	}
	return baseSandbox
}

func (a *Adapter) signingSecret() string {
	if a.cfg.PartnerSec != "" {
		return a.cfg.PartnerSec
	}
	return a.cfg.ClientSecret
}

// ---- token cache -------------------------------------------------------

func (a *Adapter) getToken(ctx context.Context) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if a.cachedToken != "" && time.Now().Before(a.tokenExpiry) {
		return a.cachedToken, nil
	}
	form := url.Values{}
	form.Set("client_id", a.cfg.ClientID)
	form.Set("client_secret", a.cfg.ClientSecret)
	form.Set("grant_type", "client_credentials")
	form.Set("scope", scope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+pathToken,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-IBM-Client-Id", a.cfg.ClientID)
	req.Header.Set("X-IBM-Client-Secret", a.cfg.ClientSecret)
	resp, err := a.h.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("metrobank: empty token")
	}
	a.cachedToken = out.AccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(out.ExpiresIn-30) * time.Second)
	return a.cachedToken, nil
}

// ---- Charge ------------------------------------------------------------

type amount struct {
	Currency string  `json:"currency"`
	Value    float64 `json:"value"`
}

type debitAccount struct {
	AccountNo   string `json:"accountNo"`
	AccountName string `json:"accountName"`
}

type debitReq struct {
	PartnerRefNo string       `json:"partnerRefNo"`
	Amount       amount       `json:"amount"`
	DebitAccount debitAccount `json:"debitAccount"`
	Purpose      string       `json:"purpose,omitempty"`
	NotifyURL    string       `json:"notifyUrl,omitempty"`
}

type debitResp struct {
	PartnerRefNo  string `json:"partnerRefNo"`
	BankRefNo     string `json:"bankRefNo"`
	Status        string `json:"status"` // SUCCEEDED / PROCESSING / REJECTED
	Amount        amount `json:"amount"`
	ReasonCode    string `json:"reasonCode"`
	ReasonMessage string `json:"reasonMessage"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	body := debitReq{
		PartnerRefNo: req.IdempotencyKey,
		Amount:       amount{Currency: req.Currency, Value: float64(req.Amount) / 100.0},
		DebitAccount: debitAccount{
			AccountNo:   req.Metadata["debit_account_no"],
			AccountName: req.Metadata["debit_account_name"],
		},
		Purpose:   req.Description,
		NotifyURL: req.NotifyURL,
	}
	var resp debitResp
	if err := a.doSigned(ctx, http.MethodPost, pathDebit, body, &resp); err != nil {
		return nil, err
	}
	switch strings.ToUpper(resp.Status) {
	case "SUCCEEDED", "SUCCESS":
		return &channel.ChargeResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.BankRefNo}, nil
	case "PROCESSING", "PENDING":
		return &channel.ChargeResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.BankRefNo}, nil
	default:
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			ExternalRefNo:  resp.BankRefNo,
			FailureCode:    channel.MapFailure("metrobank", resp.ReasonCode+" "+resp.Status),
			RawFailureCode: resp.ReasonCode,
			FailureMessage: resp.ReasonMessage,
		}, nil
	}
}

func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

// ---- Refund ------------------------------------------------------------

type refundReq struct {
	PartnerRefNo string `json:"partnerRefNo"`
	OriginRefNo  string `json:"originRefNo"`
	Amount       amount `json:"amount"`
	Reason       string `json:"reason,omitempty"`
}

type refundResp struct {
	PartnerRefNo  string `json:"partnerRefNo"`
	BankRefNo     string `json:"bankRefNo"`
	Status        string `json:"status"`
	ReasonCode    string `json:"reasonCode"`
	ReasonMessage string `json:"reasonMessage"`
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	body := refundReq{
		PartnerRefNo: req.IdempotencyKey,
		OriginRefNo:  req.ExternalRefNo,
		Amount:       amount{Currency: "PHP", Value: float64(req.Amount) / 100.0},
		Reason:       req.Reason,
	}
	var resp refundResp
	if err := a.doSigned(ctx, http.MethodPost, pathRefund, body, &resp); err != nil {
		return nil, err
	}
	switch strings.ToUpper(resp.Status) {
	case "SUCCEEDED", "SUCCESS":
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.BankRefNo}, nil
	case "PROCESSING", "PENDING":
		return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.BankRefNo}, nil
	default:
		return &channel.OpResponse{
			Result:         channel.ResultFailed,
			ExternalRefNo:  resp.BankRefNo,
			FailureCode:    channel.MapFailure("metrobank", resp.ReasonCode+" "+resp.Status),
			RawFailureCode: resp.ReasonCode,
			FailureMessage: resp.ReasonMessage,
		}, nil
	}
}

// ---- Query -------------------------------------------------------------

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	path := fmt.Sprintf(pathQuery, req.ExternalRefNo)
	var resp debitResp
	if err := a.doSigned(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch strings.ToUpper(resp.Status) {
	case "SUCCEEDED", "SUCCESS":
		rt = channel.ResultSucceeded
	case "REJECTED", "FAILED", "RETURNED":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{
		Result:         rt,
		ExternalRefNo:  resp.BankRefNo,
		AmountCaptured: int64(resp.Amount.Value * 100),
		Raw:            map[string]string{"status": resp.Status},
	}, nil
}

// ---- Webhook -----------------------------------------------------------

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	sig := headers["X-Metrobank-Signature"]
	if sig == "" {
		sig = headers["X-MetroBank-Signature"]
	}
	sigOK := channel.ConstantTimeEqHex(sig, channel.HMACSHA256Hex([]byte(a.signingSecret()), body))
	var nb struct {
		PartnerRefNo string `json:"partnerRefNo"`
		BankRefNo    string `json:"bankRefNo"`
		Status       string `json:"status"`
		Amount       amount `json:"amount"`
		Timestamp    string `json:"timestamp"`
	}
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}
	evt := &channel.WebhookEvent{
		EventID:       nb.BankRefNo,
		PiID:          nb.PartnerRefNo,
		ExternalRefNo: nb.BankRefNo,
		Amount:        int64(nb.Amount.Value * 100),
		SignatureOK:   sigOK,
		Raw:           map[string]string{"status": nb.Status},
	}
	switch strings.ToUpper(nb.Status) {
	case "SUCCEEDED", "SUCCESS":
		evt.EventType = "charge.succeeded"
	case "REJECTED", "FAILED", "RETURNED":
		evt.EventType = "charge.failed"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	if t, err := time.Parse(time.RFC3339, nb.Timestamp); err == nil {
		evt.Timestamp = t
	}
	return evt, nil
}

// ---- helpers -----------------------------------------------------------

func newCorrelationID() string {
	return fmt.Sprintf("mbt-%d", time.Now().UnixNano())
}

func (a *Adapter) doSigned(ctx context.Context, verb, path string, in, out any) error {
	tok, err := a.getToken(ctx)
	if err != nil {
		return err
	}
	var bodyBytes []byte
	if in != nil {
		bodyBytes, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	var reqBody *bytes.Reader
	if bodyBytes != nil {
		reqBody = bytes.NewReader(bodyBytes)
	}
	var req *http.Request
	if reqBody == nil {
		req, err = http.NewRequestWithContext(ctx, verb, a.base()+path, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, verb, a.base()+path, reqBody)
	}
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-IBM-Client-Id", a.cfg.ClientID)
	req.Header.Set("X-IBM-Client-Secret", a.cfg.ClientSecret)
	req.Header.Set("X-Correlation-ID", newCorrelationID())
	if a.cfg.PartnerID != "" {
		req.Header.Set("X-Metrobank-PartnerID", a.cfg.PartnerID)
	}
	// HMAC-SHA256 hex of raw body with partner secret
	sig := channel.HMACSHA256Hex([]byte(a.signingSecret()), bodyBytes)
	req.Header.Set("X-Metrobank-Signature", sig)

	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("metrobank: http %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
