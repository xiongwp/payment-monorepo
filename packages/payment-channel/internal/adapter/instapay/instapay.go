// Package instapay 实现 UnionBank Partner InstaPay adapter（OAuth2 + mTLS）。
//
// 参考：https://developer.unionbankph.com/
// InstaPay = 24/7 实时，单笔上限 ₱50,000。
package instapay

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
	baseSandbox = "https://api-uat.unionbankph.com"
	baseProd    = "https://api.unionbankph.com"

	pathToken    = "/partners/sb/v1/oauth2/token"
	pathTransfer = "/partners/v1/instapay/transfer"
	pathQuery    = "/partners/v1/instapay/transfer/%s"
	pathRefund   = "/partners/v1/instapay/refund"

	scope = "partner_instapay"
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

func (a *Adapter) Name() string { return "instapay" }

func (a *Adapter) base() string {
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	if a.cfg.Env == "prod" {
		return baseProd
	}
	return baseSandbox
}

// ---- Token 缓存 ---------------------------------------------------------

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
		return "", fmt.Errorf("instapay: empty token")
	}
	a.cachedToken = out.AccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(out.ExpiresIn-30) * time.Second)
	return a.cachedToken, nil
}

// ---- Charge -----------------------------------------------------------

type transferReq struct {
	SenderRefID           string          `json:"senderRefId"`
	TranRequestDate       string          `json:"tranRequestDate"`
	Amount                amount          `json:"amount"`
	Beneficiary           beneficiary     `json:"beneficiary"`
	Sender                senderInfo      `json:"sender"`
	RemittanceInformation string          `json:"remittanceInformation,omitempty"`
}

type amount struct {
	Currency string  `json:"currency"`
	Value    float64 `json:"value"`
}

type beneficiary struct {
	AccountNumber string `json:"accountNumber"`
	AccountName   string `json:"accountName"`
	BankCode      string `json:"bankCode"`
}

type senderInfo struct {
	AccountNumber string `json:"accountNumber"`
	AccountName   string `json:"accountName"`
}

type transferResp struct {
	ReferenceNumber string  `json:"referenceNumber"`
	Status          string  `json:"status"`
	Amount          amount  `json:"amount"`
	Message         string  `json:"message"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	beneAcct := req.Metadata["beneficiary_account_number"]
	beneName := req.Metadata["beneficiary_account_name"]
	beneBank := req.Metadata["beneficiary_bank_code"]
	body := transferReq{
		SenderRefID:     req.IdempotencyKey,
		TranRequestDate: time.Now().Format(time.RFC3339),
		Amount:          amount{Currency: req.Currency, Value: float64(req.Amount) / 100.0},
		Beneficiary: beneficiary{
			AccountNumber: beneAcct,
			AccountName:   beneName,
			BankCode:      beneBank,
		},
		Sender: senderInfo{
			AccountName:   a.cfg.SenderName,
			AccountNumber: a.cfg.SenderAcct,
		},
		RemittanceInformation: req.Description,
	}
	var resp transferResp
	if err := a.doBearer(ctx, http.MethodPost, pathTransfer, body, &resp); err != nil {
		return nil, err
	}
	switch resp.Status {
	case "SUCCESS", "SUCCEEDED":
		return &channel.ChargeResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.ReferenceNumber}, nil
	case "PENDING", "PROCESSING":
		return &channel.ChargeResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.ReferenceNumber}, nil
	default:
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			ExternalRefNo:  resp.ReferenceNumber,
			FailureCode:    channel.MapFailure("instapay", resp.Status),
			RawFailureCode: resp.Status,
			FailureMessage: resp.Message,
		}, nil
	}
}

func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}
func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

// Refund: InstaPay 不支持直接"撤销"，只能走反向转账。
func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	body := map[string]any{
		"senderRefId":  req.IdempotencyKey,
		"originRefNo":  req.ExternalRefNo,
		"amount":       amount{Currency: "PHP", Value: float64(req.Amount) / 100.0},
		"reason":       req.Reason,
	}
	var resp struct {
		ReferenceNumber string `json:"referenceNumber"`
		Status          string `json:"status"`
	}
	if err := a.doBearer(ctx, http.MethodPost, pathRefund, body, &resp); err != nil {
		return nil, err
	}
	if resp.Status == "SUCCESS" {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.ReferenceNumber}, nil
	}
	return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.ReferenceNumber}, nil
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	path := fmt.Sprintf(pathQuery, req.ExternalRefNo)
	var resp transferResp
	if err := a.doBearer(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch resp.Status {
	case "SUCCESS", "SUCCEEDED":
		rt = channel.ResultSucceeded
	case "FAILED", "RETURNED":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{
		Result:         rt,
		ExternalRefNo:  resp.ReferenceNumber,
		AmountCaptured: int64(resp.Amount.Value * 100),
	}, nil
}

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	sig := headers["X-Callback-Signature"]
	sigOK := channel.ConstantTimeEqHex(sig, channel.HMACSHA256Hex([]byte(a.cfg.ClientSecret), body))
	var nb struct {
		SenderRefID     string  `json:"senderRefId"`
		ReferenceNumber string  `json:"referenceNumber"`
		Status          string  `json:"status"`
		Amount          amount  `json:"amount"`
		Timestamp       string  `json:"timestamp"`
	}
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}
	evt := &channel.WebhookEvent{
		EventID:       nb.ReferenceNumber,
		PiID:          nb.SenderRefID,
		ExternalRefNo: nb.ReferenceNumber,
		Amount:        int64(nb.Amount.Value * 100),
		SignatureOK:   sigOK,
		Raw:           map[string]string{"status": nb.Status},
	}
	switch nb.Status {
	case "SUCCESS", "SUCCEEDED":
		evt.EventType = "charge.succeeded"
	case "FAILED", "RETURNED":
		evt.EventType = "charge.failed"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	if t, err := time.Parse(time.RFC3339, nb.Timestamp); err == nil {
		evt.Timestamp = t
	}
	return evt, nil
}

// ---- helpers ------------------------------------------------------------

func (a *Adapter) doBearer(ctx context.Context, verb, path string, in, out any) error {
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
	req.Header.Set("x-ibm-client-id", a.cfg.ClientID)
	req.Header.Set("x-ibm-client-secret", a.cfg.ClientSecret)
	req.Header.Set("x-partner-id", a.cfg.PartnerID)

	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("instapay: http %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
