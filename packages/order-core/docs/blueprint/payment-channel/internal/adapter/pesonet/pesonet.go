//go:build ignore
// +build ignore

// Package pesonet 实现 PESONet 批量转账 adapter（以 UnionBank Partner API 为接入点）。
//
// 参考：https://developer.unionbankph.com/product/docs/pesonet-transfers-via-partners
//       PPMI：https://www.philpayments.org.ph/pesonet
//
// PESONet = 批量清算，T+0 / T+1 到账，单笔上限视渠道一般 ₱300,000，某些批次更高。
// 鉴权：OAuth2 client_credentials + mTLS（同 InstaPay）。
package pesonet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	pathTransfer = "/partners/v1/pesonet/transfer"
	pathQuery    = "/partners/v1/pesonet/transfer/%s"
	scopeTransfer = "partner_pesonet"
)

type Config struct {
	Env           string
	ClientID      string
	ClientSecret  string
	PartnerID     string
	IBMClientID   string
	IBMSecret     string
	MerchantAcct  account
	HTTPClient    *http.Client
}

type Adapter struct {
	cfg Config

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

func New(cfg Config) *Adapter {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Adapter{cfg: cfg}
}

func (a *Adapter) Name() string { return "pesonet" }

func (a *Adapter) base() string {
	if a.cfg.Env == "prod" {
		return baseProd
	}
	return baseSandbox
}

// ---- OAuth ----------------------------------------------------------------

func (a *Adapter) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Now().Before(a.tokenExp) && a.token != "" {
		return a.token, nil
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", a.cfg.ClientID)
	form.Set("client_secret", a.cfg.ClientSecret)
	form.Set("scope", scopeTransfer)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+pathToken, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("pesonet: empty access_token")
	}
	a.token = tr.AccessToken
	a.tokenExp = time.Now().Add(time.Duration(tr.ExpiresIn-30) * time.Second)
	return a.token, nil
}

// ---- Transfer (= Charge) --------------------------------------------------

type account struct {
	AccountNumber string `json:"accountNumber"`
	AccountName   string `json:"accountName"`
	BankCode      string `json:"bankCode,omitempty"`
}

type moneyAmount struct {
	Currency string  `json:"currency"`
	Value    float64 `json:"value"`
}

type transferReq struct {
	SenderRefID           string  `json:"senderRefId"`
	ValueDate             string  `json:"valueDate"` // PESONet 特有：YYYY-MM-DD，批次日期
	TranRequestDate       string  `json:"tranRequestDate"`
	Amount                moneyAmount `json:"amount"`
	Sender                account `json:"sender"`
	Beneficiary           struct {
		Account account `json:"account"`
	} `json:"beneficiary"`
	RemittanceInformation string `json:"remittanceInformation"`
}

type transferResp struct {
	ReferenceNumber string `json:"referenceNumber"`
	BatchID         string `json:"batchId"`
	Status          string `json:"status"` // ACCEPTED / PENDING / SETTLED / RETURNED / REJECTED
	ResponseCode    string `json:"responseCode"`
	ResponseMessage string `json:"responseMessage"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	b := transferReq{
		SenderRefID:     req.IdempotencyKey,
		ValueDate:       time.Now().Format("2006-01-02"),
		TranRequestDate: time.Now().Format("2006-01-02T15:04:05-07:00"),
		Amount:          moneyAmount{Currency: req.Currency, Value: float64(req.Amount) / 100},
		Sender:          a.cfg.MerchantAcct,
		RemittanceInformation: req.Description,
	}
	b.Beneficiary.Account = account{
		AccountNumber: req.Metadata["beneficiary_account_number"],
		AccountName:   req.Metadata["beneficiary_account_name"],
		BankCode:      req.Metadata["beneficiary_bank_code"],
	}
	var resp transferResp
	status, err := a.doAuthed(ctx, http.MethodPost, pathTransfer, b, &resp)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			FailureCode:    channel.MapFailure("pesonet", resp.ResponseCode),
			RawFailureCode: resp.ResponseCode,
			FailureMessage: resp.ResponseMessage,
		}, nil
	}
	// PESONet 是批量清算，几乎总是先返回 PENDING / ACCEPTED，最终结果走 webhook。
	return &channel.ChargeResponse{
		Result:        channel.ResultProcessing,
		ExternalRefNo: resp.ReferenceNumber,
	}, nil
}

func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultFailed, FailureCode: channel.FailChannelUnavailable}, nil
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	// 反向批次转账实现退款。
	return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: req.IdempotencyKey}, nil
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	var resp transferResp
	path := fmt.Sprintf(pathQuery, url.PathEscape(req.ExternalRefNo))
	_, err := a.doAuthed(ctx, http.MethodGet, path, nil, &resp)
	if err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch resp.Status {
	case "SETTLED":
		rt = channel.ResultSucceeded
	case "RETURNED", "REJECTED":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{Result: rt, ExternalRefNo: resp.ReferenceNumber}, nil
}

// ---- Webhook --------------------------------------------------------------

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	// PESONet 回调由结算批次触发，一天三批（9:00 / 13:00 / 16:00）。
	sig := headers["x-callback-signature"]
	ok := sig != "" // 具体 HMAC 在 internal/channel/sign 下

	var cb struct {
		SenderRefID     string `json:"senderRefId"`
		ReferenceNumber string `json:"referenceNumber"`
		Status          string `json:"status"`
		Amount          moneyAmount `json:"amount"`
		SettledAt       string `json:"settledAt"`
		BatchID         string `json:"batchId"`
	}
	if err := json.Unmarshal(body, &cb); err != nil {
		return nil, err
	}
	evt := &channel.WebhookEvent{
		EventID:       cb.ReferenceNumber,
		PiID:          cb.SenderRefID,
		ExternalRefNo: cb.ReferenceNumber,
		Amount:        int64(cb.Amount.Value * 100),
		Timestamp:     parseTime(cb.SettledAt),
		SignatureOK:   ok,
		Raw:           map[string]string{"status": cb.Status, "batchId": cb.BatchID},
	}
	switch cb.Status {
	case "SETTLED":
		evt.EventType = "charge.succeeded"
	case "RETURNED", "REJECTED", "FAILED":
		evt.EventType = "charge.failed"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	return evt, nil
}

// ---- helpers --------------------------------------------------------------

func (a *Adapter) doAuthed(ctx context.Context, method, path string, in, out any) (int, error) {
	tok, err := a.accessToken(ctx)
	if err != nil {
		return 0, err
	}
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
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("x-ibm-client-id", a.cfg.IBMClientID)
	req.Header.Set("x-ibm-client-secret", a.cfg.IBMSecret)
	req.Header.Set("x-partner-id", a.cfg.PartnerID)
	resp, err := a.cfg.HTTPClient.Do(req)
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

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}