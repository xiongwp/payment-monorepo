// Package landbank 实现 Landbank ePay adapter（Bills Payment / Government Transfers / QR Ph）。
//
// 参考：https://epay.landbank.com/
// 鉴权：HTTP Basic Auth (merchant_id / secret_key) + body-level MD5 digest。
//   digest = md5(merchant_id | txn_id | amount | secret_key)
// 支付采用 hosted-page redirect，返回 Result=RequiresAction + app_redirect。
package landbank

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseSandbox = "https://epay-test.landbank.com/api"
	baseProd    = "https://epay.landbank.com/api"

	pathCreate = "/payment/v2/create"
	pathRefund = "/payment/v2/refund"
	pathStatus = "/payment/v2/status/%s"
)

type Config struct {
	Env        string
	MerchantID string
	SecretKey  string
	ReturnURL  string // 默认前端回跳
	BaseURL string // 非空时覆盖 sandbox/prod，用于 mockserver
}

type Adapter struct {
	cfg Config
	h   *http.Client
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, h: channel.NewHTTPClient(15 * time.Second)}
}

func (a *Adapter) Name() string { return "landbank" }

func (a *Adapter) base() string {
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	if a.cfg.Env == "prod" {
		return baseProd
	}
	return baseSandbox
}

// ---- helpers -----------------------------------------------------------

func formatAmount(cents int64) string {
	return strconv.FormatFloat(float64(cents)/100.0, 'f', 2, 64)
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// digest = md5(merchant_id | txn_id | amount | secret_key)
func (a *Adapter) digest(txnID, amount string) string {
	return md5Hex(a.cfg.MerchantID + "|" + txnID + "|" + amount + "|" + a.cfg.SecretKey)
}

// ---- Charge ------------------------------------------------------------

type createReq struct {
	MerchantID  string `json:"merchantId"`
	TxnID       string `json:"txnId"`
	Amount      string `json:"amount"`
	Currency    string `json:"currency"`
	Description string `json:"description,omitempty"`
	CallbackURL string `json:"callbackUrl,omitempty"`
	ReturnURL   string `json:"returnUrl,omitempty"`
	Digest      string `json:"digest"`
}

type createResp struct {
	PaymentURL    string `json:"paymentUrl"`
	BankRefNo     string `json:"bankRefNo"`
	Status        string `json:"status"`
	ReasonCode    string `json:"reasonCode"`
	ReasonMessage string `json:"reasonMessage"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	amt := formatAmount(req.Amount)
	returnURL := req.ReturnURL
	if returnURL == "" {
		returnURL = a.cfg.ReturnURL
	}
	body := createReq{
		MerchantID:  a.cfg.MerchantID,
		TxnID:       req.IdempotencyKey,
		Amount:      amt,
		Currency:    "PHP",
		Description: req.Description,
		CallbackURL: req.NotifyURL,
		ReturnURL:   returnURL,
		Digest:      a.digest(req.IdempotencyKey, amt),
	}
	var resp createResp
	if err := a.do(ctx, http.MethodPost, pathCreate, body, &resp); err != nil {
		return nil, err
	}
	if resp.PaymentURL == "" {
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			ExternalRefNo:  resp.BankRefNo,
			FailureCode:    channel.MapFailure("landbank", resp.ReasonCode+" "+resp.Status),
			RawFailureCode: resp.ReasonCode,
			FailureMessage: resp.ReasonMessage,
		}, nil
	}
	return &channel.ChargeResponse{
		Result:        channel.ResultRequiresAction,
		ExternalRefNo: resp.BankRefNo,
		RequiredAction: &channel.RequiredAction{
			Type:        "app_redirect",
			RedirectURL: resp.PaymentURL,
			Scheme:      "universal",
			ReturnURL:   returnURL,
			ExpiresAt:   time.Now().Add(20 * time.Minute),
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

// ---- Refund ------------------------------------------------------------

type refundReq struct {
	MerchantID  string `json:"merchantId"`
	TxnID       string `json:"txnId"`        // 原单 txnId
	RefundTxnID string `json:"refundTxnId"`  // 本次退款 idempotency
	Amount      string `json:"amount"`
	Digest      string `json:"digest"`
}

type refundResp struct {
	RefundRefNo   string `json:"refundRefNo"`
	Status        string `json:"status"`
	ReasonCode    string `json:"reasonCode"`
	ReasonMessage string `json:"reasonMessage"`
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	amt := formatAmount(req.Amount)
	body := refundReq{
		MerchantID:  a.cfg.MerchantID,
		TxnID:       req.ExternalRefNo,
		RefundTxnID: req.IdempotencyKey,
		Amount:      amt,
		// 退款 digest 用本次 refundTxnId 作为 txnId 参与摘要，与新建保持同构。
		Digest: a.digest(req.IdempotencyKey, amt),
	}
	var resp refundResp
	if err := a.do(ctx, http.MethodPost, pathRefund, body, &resp); err != nil {
		return nil, err
	}
	switch strings.ToUpper(resp.Status) {
	case "SUCCESS", "SUCCEEDED", "COMPLETED":
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.RefundRefNo}, nil
	case "PENDING", "PROCESSING":
		return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.RefundRefNo}, nil
	default:
		return &channel.OpResponse{
			Result:         channel.ResultFailed,
			ExternalRefNo:  resp.RefundRefNo,
			FailureCode:    channel.MapFailure("landbank", resp.ReasonCode+" "+resp.Status),
			RawFailureCode: resp.ReasonCode,
			FailureMessage: resp.ReasonMessage,
		}, nil
	}
}

// ---- Query -------------------------------------------------------------

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	path := fmt.Sprintf(pathStatus, req.ExternalRefNo)
	var resp struct {
		TxnID     string `json:"txnId"`
		BankRefNo string `json:"bankRefNo"`
		Status    string `json:"status"`
		Amount    string `json:"amount"`
		Refunded  string `json:"refundedAmount"`
	}
	if err := a.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch strings.ToUpper(resp.Status) {
	case "PAID", "SUCCESS", "SUCCEEDED", "COMPLETED":
		rt = channel.ResultSucceeded
	case "FAILED", "EXPIRED", "CANCELED", "CANCELLED", "REJECTED":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	captured, _ := strconv.ParseFloat(resp.Amount, 64)
	refunded, _ := strconv.ParseFloat(resp.Refunded, 64)
	return &channel.QueryResponse{
		Result:         rt,
		ExternalRefNo:  resp.BankRefNo,
		AmountCaptured: int64(captured * 100),
		AmountRefunded: int64(refunded * 100),
		Raw:            map[string]string{"status": resp.Status},
	}, nil
}

// ---- Webhook -----------------------------------------------------------

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	var nb struct {
		MerchantID string `json:"merchantId"`
		TxnID      string `json:"txnId"`
		BankRefNo  string `json:"bankRefNo"`
		Status     string `json:"status"`
		Amount     string `json:"amount"`
		Digest     string `json:"digest"`
		Timestamp  string `json:"timestamp"`
	}
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}
	expected := md5Hex(nb.MerchantID + "|" + nb.TxnID + "|" + nb.Amount + "|" + a.cfg.SecretKey)
	sigOK := channel.ConstantTimeEqHex(strings.ToLower(nb.Digest), expected)

	amt, _ := strconv.ParseFloat(nb.Amount, 64)
	evt := &channel.WebhookEvent{
		EventID:       nb.BankRefNo,
		PiID:          nb.TxnID,
		ExternalRefNo: nb.BankRefNo,
		Amount:        int64(amt * 100),
		SignatureOK:   sigOK,
		Raw:           map[string]string{"status": nb.Status},
	}
	switch strings.ToUpper(nb.Status) {
	case "PAID", "SUCCESS", "SUCCEEDED", "COMPLETED":
		evt.EventType = "charge.succeeded"
	case "FAILED", "EXPIRED", "CANCELED", "CANCELLED", "REJECTED":
		evt.EventType = "charge.failed"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	if t, err := time.Parse(time.RFC3339, nb.Timestamp); err == nil {
		evt.Timestamp = t
	}
	return evt, nil
}

// ---- HTTP --------------------------------------------------------------

func (a *Adapter) do(ctx context.Context, verb, path string, in, out any) error {
	var bodyBytes []byte
	if in != nil {
		var err error
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
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(a.cfg.MerchantID, a.cfg.SecretKey)

	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("landbank: http %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
