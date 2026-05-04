// Package bpi 实现 BPI Open Finance Portal adapter（BancNet InstaPay + BPI 账户转账）。
//
// 参考：https://developer.bpi.com.ph/
// 鉴权：OAuth2 client credentials + 请求行签名：
//   base = "{method}\n{path}\n{timestamp}\n{sha256(body)}"
//   header X-BPI-Signature = base64(HMAC-SHA256(client_secret, base))
package bpi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	baseSandbox = "https://sandbox.api.bpi.com.ph"
	baseProd    = "https://api.bpi.com.ph"

	pathToken    = "/oauth/v1/token"
	pathTransfer = "/open/v1/fund-transfer"
	pathQuery    = "/open/v1/fund-transfer/%s"
	pathRefund   = "/open/v1/fund-transfer/refund"

	scope = "fund-transfer"
)

type Config struct {
	Env          string
	ClientID     string
	ClientSecret string
	PartnerID    string
	SenderAcct   string
	SenderName   string
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

func (a *Adapter) Name() string { return "bpi" }

func (a *Adapter) base() string {
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	if a.cfg.Env == "prod" {
		return baseProd
	}
	return baseSandbox
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
		return "", fmt.Errorf("bpi: empty token")
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

type party struct {
	AccountNumber string `json:"accountNumber"`
	Name          string `json:"name"`
	BankCode      string `json:"bankCode,omitempty"`
}

type transferReq struct {
	TransactionID string `json:"transactionId"`
	Amount        amount `json:"amount"`
	Sender        party  `json:"sender"`
	Beneficiary   party  `json:"beneficiary"`
	Remarks       string `json:"remarks,omitempty"`
	CallbackURL   string `json:"callbackUrl,omitempty"`
}

type transferResp struct {
	TransactionID   string `json:"transactionId"`
	ReferenceNumber string `json:"referenceNumber"`
	Status          string `json:"status"` // SUCCESS / PENDING / FAILED
	Amount          amount `json:"amount"`
	ErrorCode       string `json:"errorCode"`
	ErrorMessage    string `json:"errorMessage"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	body := transferReq{
		TransactionID: req.IdempotencyKey,
		Amount:        amount{Currency: req.Currency, Value: float64(req.Amount) / 100.0},
		Sender: party{
			AccountNumber: a.cfg.SenderAcct,
			Name:          a.cfg.SenderName,
		},
		Beneficiary: party{
			AccountNumber: req.Metadata["beneficiary_account_number"],
			Name:          req.Metadata["beneficiary_account_name"],
			BankCode:      req.Metadata["beneficiary_bank_code"],
		},
		Remarks:     req.Description,
		CallbackURL: req.NotifyURL,
	}
	var resp transferResp
	if err := a.doSigned(ctx, http.MethodPost, pathTransfer, body, &resp); err != nil {
		return nil, err
	}
	switch strings.ToUpper(resp.Status) {
	case "SUCCESS", "SUCCEEDED":
		return &channel.ChargeResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.ReferenceNumber}, nil
	case "PENDING", "PROCESSING":
		return &channel.ChargeResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.ReferenceNumber}, nil
	default:
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			ExternalRefNo:  resp.ReferenceNumber,
			FailureCode:    channel.MapFailure("bpi", resp.ErrorCode+" "+resp.Status),
			RawFailureCode: resp.ErrorCode,
			FailureMessage: resp.ErrorMessage,
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
	TransactionID string `json:"transactionId"`
	OriginTxnID   string `json:"originTxnId"`
	Amount        amount `json:"amount"`
	Reason        string `json:"reason,omitempty"`
}

type refundResp struct {
	TransactionID   string `json:"transactionId"`
	ReferenceNumber string `json:"referenceNumber"`
	Status          string `json:"status"`
	ErrorCode       string `json:"errorCode"`
	ErrorMessage    string `json:"errorMessage"`
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	body := refundReq{
		TransactionID: req.IdempotencyKey,
		OriginTxnID:   req.ExternalRefNo,
		Amount:        amount{Currency: "PHP", Value: float64(req.Amount) / 100.0},
		Reason:        req.Reason,
	}
	var resp refundResp
	if err := a.doSigned(ctx, http.MethodPost, pathRefund, body, &resp); err != nil {
		return nil, err
	}
	switch strings.ToUpper(resp.Status) {
	case "SUCCESS", "SUCCEEDED":
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.ReferenceNumber}, nil
	case "PENDING", "PROCESSING":
		return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.ReferenceNumber}, nil
	default:
		return &channel.OpResponse{
			Result:         channel.ResultFailed,
			ExternalRefNo:  resp.ReferenceNumber,
			FailureCode:    channel.MapFailure("bpi", resp.ErrorCode+" "+resp.Status),
			RawFailureCode: resp.ErrorCode,
			FailureMessage: resp.ErrorMessage,
		}, nil
	}
}

// ---- Query -------------------------------------------------------------

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	path := fmt.Sprintf(pathQuery, req.ExternalRefNo)
	var resp transferResp
	if err := a.doSigned(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch strings.ToUpper(resp.Status) {
	case "SUCCESS", "SUCCEEDED":
		rt = channel.ResultSucceeded
	case "FAILED", "REJECTED", "RETURNED":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{
		Result:         rt,
		ExternalRefNo:  resp.ReferenceNumber,
		AmountCaptured: int64(resp.Amount.Value * 100),
		Raw:            map[string]string{"status": resp.Status},
	}, nil
}

// ---- Webhook -----------------------------------------------------------

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	sig := headers["X-BPI-Signature"]
	if sig == "" {
		sig = headers["X-Bpi-Signature"]
	}
	// BPI callback signs the raw body with client_secret (HMAC-SHA256 base64).
	sigOK := sig != "" && sig == channel.HMACSHA256Base64([]byte(a.cfg.ClientSecret), body)
	var nb struct {
		TransactionID   string `json:"transactionId"`
		ReferenceNumber string `json:"referenceNumber"`
		Status          string `json:"status"`
		Amount          amount `json:"amount"`
		Timestamp       string `json:"timestamp"`
	}
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}
	evt := &channel.WebhookEvent{
		EventID:       nb.ReferenceNumber,
		PiID:          nb.TransactionID,
		ExternalRefNo: nb.ReferenceNumber,
		Amount:        int64(nb.Amount.Value * 100),
		SignatureOK:   sigOK,
		Raw:           map[string]string{"status": nb.Status},
	}
	switch strings.ToUpper(nb.Status) {
	case "SUCCESS", "SUCCEEDED":
		evt.EventType = "charge.succeeded"
	case "FAILED", "REJECTED", "RETURNED":
		evt.EventType = "charge.failed"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	if t, err := time.Parse(time.RFC3339, nb.Timestamp); err == nil {
		evt.Timestamp = t
	}
	return evt, nil
}

// ---- signing helpers ---------------------------------------------------

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
	ts := time.Now().UTC().Format(time.RFC3339)
	base := verb + "\n" + path + "\n" + ts + "\n" + sha256Hex(bodyBytes)
	sig := channel.HMACSHA256Base64([]byte(a.cfg.ClientSecret), []byte(base))

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
	req.Header.Set("X-BPI-PartnerID", a.cfg.PartnerID)
	req.Header.Set("X-BPI-Timestamp", ts)
	req.Header.Set("X-BPI-Signature", sig)

	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("bpi: http %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
