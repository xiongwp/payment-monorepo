// Package dragonpay 实现 Dragonpay Collect API adapter。
//
// Dragonpay 是 PH 的老牌 aggregator，主打 OTC（7-Eleven / Cebuana Lhuillier /
// ECPay / M. Lhuillier / Palawan）、online banking（BPI/BDO/Metrobank/
// UnionBank）以及 e-wallet（GCash/GrabPay/Maya）。
//
// 参考：https://www.dragonpay.ph/integration-guide/
// 鉴权：每个请求带 digest = SHA1(merchantid:txnid:amount:ccy:description:merchantkey)。
package dragonpay

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseSandbox = "https://test.dragonpay.ph"
	baseProd    = "https://gw.dragonpay.ph"

	pathPost   = "/api/collect/v1/%s/post"
	pathQuery  = "/api/collect/v1/%s"
	pathRefund = "/api/refund/v1/post"

	defaultProcID = "GCSH" // GCash
)

type Config struct {
	Env         string
	MerchantID  string
	MerchantKey string
	NotifyURL   string
	BaseURL string // 非空时覆盖 sandbox/prod，用于 mockserver
}

type Adapter struct {
	cfg Config
	h   *http.Client
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, h: channel.NewHTTPClient(20 * time.Second)}
}

func (a *Adapter) Name() string { return "dragonpay" }

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

type postReq struct {
	Amount      string `json:"Amount"`
	Currency    string `json:"Currency"`
	Description string `json:"Description"`
	Email       string `json:"Email"`
	ProcID      string `json:"ProcId,omitempty"`
	Param1      string `json:"Param1,omitempty"`
	Param2      string `json:"Param2,omitempty"`
	Digest      string `json:"Digest"`
}

type postResp struct {
	Status  string `json:"Status"`
	RefNo   string `json:"RefNo"`
	URL     string `json:"Url"`
	Message string `json:"Message"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	txnid := firstNonEmpty(req.IdempotencyKey, req.PiID)
	amount := formatAmount(req.Amount)
	currency := firstNonEmpty(req.Currency, "PHP")
	desc := firstNonEmpty(req.Description, "Payment "+req.PiID)

	procID := defaultProcID
	if req.Metadata != nil {
		if p := req.Metadata["dragonpay_procid"]; p != "" {
			procID = p
		}
	}

	digest := sha1Hex(strings.Join([]string{
		a.cfg.MerchantID, txnid, amount, currency, desc, a.cfg.MerchantKey,
	}, ":"))

	body := postReq{
		Amount:      amount,
		Currency:    currency,
		Description: desc,
		Email:       req.Buyer.Email,
		ProcID:      procID,
		Param1:      req.PiID,
		Param2:      req.NotifyURL,
		Digest:      digest,
	}

	path := fmt.Sprintf(pathPost, url.PathEscape(txnid))
	var resp postResp
	if err := a.do(ctx, http.MethodPost, path, body, &resp); err != nil {
		return nil, err
	}

	raw := map[string]string{
		"status":  resp.Status,
		"proc_id": procID,
		"message": resp.Message,
	}

	switch resp.Status {
	case "S":
		return &channel.ChargeResponse{
			Result:        channel.ResultSucceeded,
			ExternalRefNo: firstNonEmpty(resp.RefNo, txnid),
			Raw:           raw,
		}, nil
	case "F":
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			ExternalRefNo:  firstNonEmpty(resp.RefNo, txnid),
			FailureCode:    channel.MapFailure(a.Name(), resp.Message),
			RawFailureCode: resp.Status,
			FailureMessage: resp.Message,
			Raw:            raw,
		}, nil
	}

	if resp.URL != "" {
		return &channel.ChargeResponse{
			Result:        channel.ResultRequiresAction,
			ExternalRefNo: firstNonEmpty(resp.RefNo, txnid),
			RequiredAction: &channel.RequiredAction{
				Type:        "app_redirect",
				RedirectURL: resp.URL,
				Scheme:      "universal",
				ReturnURL:   req.ReturnURL,
				ExpiresAt:   time.Now().Add(30 * time.Minute),
			},
			Raw: raw,
		}, nil
	}
	// P/U/其他：处理中
	return &channel.ChargeResponse{
		Result:        channel.ResultProcessing,
		ExternalRefNo: firstNonEmpty(resp.RefNo, txnid),
		Raw:           raw,
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
	TxnID  string `json:"TxnId"`
	Amount string `json:"Amount"`
	Reason string `json:"Reason"`
	Digest string `json:"Digest"`
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	amount := formatAmount(req.Amount)
	reason := firstNonEmpty(req.Reason, "requested_by_customer")
	digest := sha1Hex(strings.Join([]string{
		a.cfg.MerchantID, req.ExternalRefNo, amount, reason, a.cfg.MerchantKey,
	}, ":"))
	body := refundReq{
		TxnID:  req.ExternalRefNo,
		Amount: amount,
		Reason: reason,
		Digest: digest,
	}
	var resp struct {
		Status  string `json:"Status"`
		RefNo   string `json:"RefNo"`
		Message string `json:"Message"`
	}
	if err := a.do(ctx, http.MethodPost, pathRefund, body, &resp); err != nil {
		return nil, err
	}
	switch resp.Status {
	case "S", "R":
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: firstNonEmpty(resp.RefNo, req.ExternalRefNo)}, nil
	case "F":
		return &channel.OpResponse{
			Result:         channel.ResultFailed,
			ExternalRefNo:  firstNonEmpty(resp.RefNo, req.ExternalRefNo),
			FailureCode:    channel.MapFailure(a.Name(), resp.Message),
			RawFailureCode: resp.Status,
			FailureMessage: resp.Message,
		}, nil
	default:
		return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: firstNonEmpty(resp.RefNo, req.ExternalRefNo)}, nil
	}
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	path := fmt.Sprintf(pathQuery, url.PathEscape(req.ExternalRefNo))
	var resp struct {
		Status  string `json:"Status"`
		RefNo   string `json:"RefNo"`
		Message string `json:"Message"`
		Amount  string `json:"Amount"`
	}
	if err := a.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	rt := mapDragonpayStatus(resp.Status)
	amt, _ := strconv.ParseFloat(resp.Amount, 64)
	return &channel.QueryResponse{
		Result:         rt,
		ExternalRefNo:  firstNonEmpty(resp.RefNo, req.ExternalRefNo),
		AmountCaptured: int64(amt * 100),
		Raw:            map[string]string{"status": resp.Status, "message": resp.Message},
	}, nil
}

// ---- Webhook -------------------------------------------------------------
//
// Dragonpay 回调是 form-encoded，字段：
//   txnid, refno, status, message, amount, ccy, digest
// digest = SHA1(txnid:refno:status:message:merchantkey)。
func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	txnid := form.Get("txnid")
	refno := form.Get("refno")
	status := form.Get("status")
	message := form.Get("message")
	amountStr := form.Get("amount")
	ccy := form.Get("ccy")
	gotDigest := strings.ToLower(form.Get("digest"))

	wantDigest := sha1Hex(strings.Join([]string{
		txnid, refno, status, message, a.cfg.MerchantKey,
	}, ":"))
	sigOK := gotDigest != "" && channel.ConstantTimeEqHex(gotDigest, wantDigest)

	amt, _ := strconv.ParseFloat(amountStr, 64)
	evt := &channel.WebhookEvent{
		EventID:       refno,
		PiID:          txnid,
		ExternalRefNo: firstNonEmpty(refno, txnid),
		Amount:        int64(amt * 100),
		SignatureOK:   sigOK,
		Timestamp:     time.Now(),
		Raw: map[string]string{
			"status":  status,
			"message": message,
			"ccy":     ccy,
		},
	}
	evt.EventType = mapDragonpayEvent(status)
	return evt, nil
}

// ---- helpers -------------------------------------------------------------

func (a *Adapter) do(ctx context.Context, verb, path string, in, out any) error {
	var reqBody *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	fullURL := a.base() + path
	var req *http.Request
	var err error
	if reqBody == nil {
		req, err = http.NewRequestWithContext(ctx, verb, fullURL, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, verb, fullURL, reqBody)
	}
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Merchant-Id", a.cfg.MerchantID)
	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("dragonpay: http %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// mapDragonpayStatus: S=Success F=Failure P=Pending U=Unknown R=Refund
// K=Chargeback V=Voided A=Authorized G=Await Pay E=Expired.
func mapDragonpayStatus(s string) channel.ResultType {
	switch s {
	case "S", "A":
		return channel.ResultSucceeded
	case "F", "E", "V", "K":
		return channel.ResultFailed
	case "P", "U", "G", "R":
		return channel.ResultProcessing
	default:
		return channel.ResultProcessing
	}
}

func mapDragonpayEvent(s string) string {
	switch s {
	case "S", "A":
		return "charge.succeeded"
	case "F", "E":
		return "charge.failed"
	case "V":
		return "charge.failed"
	case "R":
		return "refund.succeeded"
	case "K":
		return "refund.succeeded"
	default:
		return "payment_intent.requires_action"
	}
}

func sha1Hex(s string) string {
	h := sha1.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func formatAmount(cents int64) string {
	return strconv.FormatFloat(float64(cents)/100.0, 'f', 2, 64)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
