// Package paymongo 实现 PayMongo adapter。
//
// PayMongo 是 PH 最大的 fintech PSP，单 API 覆盖 Cards / GCash / GrabPay /
// Maya / Dob（online banking）。
//
// 参考：https://developers.paymongo.com/reference
// 鉴权：HTTP Basic Auth，username = secret key (sk_test_... / sk_live_...)，
// password 为空。
package paymongo

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseURL = "https://api.paymongo.com"

	pathCreateSource = "/v1/sources"
	pathGetSource    = "/v1/sources/%s"
	pathRefund       = "/v1/refunds"
)

type Config struct {
	SecretKey     string
	WebhookSecret string
	// Env 仅用于日志/排错，PayMongo 用同一域名，通过 key 前缀区分。
	Env string
	// BaseURL 非空时覆盖 baseURL，用于 mockserver / 代理。
	BaseURL string
}

type Adapter struct {
	cfg Config
	h   *http.Client
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, h: channel.NewHTTPClient(20 * time.Second)}
}

func (a *Adapter) base() string {
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	return baseURL
}

func (a *Adapter) Name() string { return "paymongo" }

// ---- Charge --------------------------------------------------------------

type sourceReq struct {
	Data struct {
		Attributes sourceAttrs `json:"attributes"`
	} `json:"data"`
}

type sourceAttrs struct {
	Type     string            `json:"type"`
	Amount   int64             `json:"amount"`
	Currency string            `json:"currency"`
	Redirect map[string]string `json:"redirect"`
	Billing  map[string]any    `json:"billing,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type sourceResp struct {
	Data struct {
		ID         string `json:"id"`
		Attributes struct {
			Status   string `json:"status"`
			Redirect struct {
				CheckoutURL string `json:"checkout_url"`
			} `json:"redirect"`
			LastPaymentError struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"last_payment_error"`
		} `json:"attributes"`
	} `json:"data"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	srcType := "gcash"
	if req.Metadata != nil {
		if t := req.Metadata["paymongo_source_type"]; t != "" {
			srcType = t
		}
	}

	body := sourceReq{}
	body.Data.Attributes = sourceAttrs{
		Type:     srcType,
		Amount:   req.Amount,
		Currency: firstNonEmpty(req.Currency, "PHP"),
		Redirect: map[string]string{
			"success": req.ReturnURL,
			"failed":  req.ReturnURL,
		},
		Metadata: map[string]string{"pi_id": req.PiID},
	}
	if req.Buyer.FirstName != "" || req.Buyer.Email != "" {
		body.Data.Attributes.Billing = map[string]any{
			"name":  strings.TrimSpace(req.Buyer.FirstName + " " + req.Buyer.LastName),
			"email": req.Buyer.Email,
			"phone": req.Buyer.Phone,
		}
	}

	var resp sourceResp
	if err := a.doBasic(ctx, http.MethodPost, pathCreateSource, req.IdempotencyKey, body, &resp); err != nil {
		return nil, err
	}
	if resp.Data.ID == "" {
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			FailureCode:    channel.FailUnknown,
			RawFailureCode: "no_source_id",
		}, nil
	}
	if resp.Data.Attributes.Status == "failed" {
		raw := resp.Data.Attributes.LastPaymentError.Code
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			ExternalRefNo:  resp.Data.ID,
			FailureCode:    channel.MapFailure(a.Name(), raw),
			RawFailureCode: raw,
			FailureMessage: resp.Data.Attributes.LastPaymentError.Message,
		}, nil
	}
	return &channel.ChargeResponse{
		Result:        channel.ResultRequiresAction,
		ExternalRefNo: resp.Data.ID,
		RequiredAction: &channel.RequiredAction{
			Type:        "app_redirect",
			RedirectURL: resp.Data.Attributes.Redirect.CheckoutURL,
			Scheme:      "universal",
			ReturnURL:   req.ReturnURL,
			ExpiresAt:   time.Now().Add(30 * time.Minute),
		},
		Raw: map[string]string{"status": resp.Data.Attributes.Status, "source_type": srcType},
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
	Data struct {
		Attributes refundAttrs `json:"attributes"`
	} `json:"data"`
}

type refundAttrs struct {
	Amount    int64  `json:"amount"`
	PaymentID string `json:"payment_id"`
	Reason    string `json:"reason,omitempty"`
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	body := refundReq{}
	body.Data.Attributes = refundAttrs{
		Amount:    req.Amount,
		PaymentID: req.ExternalRefNo,
		Reason:    firstNonEmpty(req.Reason, "requested_by_customer"),
	}
	var resp struct {
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				Status string `json:"status"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := a.doBasic(ctx, http.MethodPost, pathRefund, req.IdempotencyKey, body, &resp); err != nil {
		return nil, err
	}
	if resp.Data.Attributes.Status == "succeeded" {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.Data.ID}, nil
	}
	return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.Data.ID}, nil
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	path := fmt.Sprintf(pathGetSource, req.ExternalRefNo)
	var resp struct {
		Data struct {
			Attributes struct {
				Status string `json:"status"`
				Amount int64  `json:"amount"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := a.doBasic(ctx, http.MethodGet, path, "", nil, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch resp.Data.Attributes.Status {
	case "chargeable", "paid":
		rt = channel.ResultSucceeded
	case "failed", "expired":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{
		Result:         rt,
		ExternalRefNo:  req.ExternalRefNo,
		AmountCaptured: resp.Data.Attributes.Amount,
		Raw:            map[string]string{"status": resp.Data.Attributes.Status},
	}, nil
}

// ---- Webhook -------------------------------------------------------------
//
// PayMongo 头：`Paymongo-Signature: t=<timestamp>,te=<test_sig>,li=<live_sig>`
// 签名 = HMAC-SHA256( `<t>.<payload>`, webhook_secret )，hex 编码。
func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	sigHeader := firstHeader(headers, "Paymongo-Signature", "paymongo-signature")
	ts, testSig, liveSig := parsePaymongoSig(sigHeader)

	sigOK := false
	if a.cfg.WebhookSecret != "" && ts != "" {
		payload := ts + "." + string(body)
		mac := hmac.New(sha256.New, []byte(a.cfg.WebhookSecret))
		mac.Write([]byte(payload))
		want := hex.EncodeToString(mac.Sum(nil))
		if testSig != "" && channel.ConstantTimeEqHex(want, testSig) {
			sigOK = true
		}
		if liveSig != "" && channel.ConstantTimeEqHex(want, liveSig) {
			sigOK = true
		}
	}

	var nb struct {
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				Type string `json:"type"`
				Data struct {
					ID         string `json:"id"`
					Attributes struct {
						Amount   int64             `json:"amount"`
						Status   string            `json:"status"`
						Metadata map[string]string `json:"metadata"`
					} `json:"attributes"`
				} `json:"data"`
				CreatedAt int64 `json:"created_at"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}

	evt := &channel.WebhookEvent{
		EventID:       nb.Data.ID,
		EventType:     mapPaymongoEvent(nb.Data.Attributes.Type),
		PiID:          nb.Data.Attributes.Data.Attributes.Metadata["pi_id"],
		ExternalRefNo: nb.Data.Attributes.Data.ID,
		Amount:        nb.Data.Attributes.Data.Attributes.Amount,
		SignatureOK:   sigOK,
		Raw: map[string]string{
			"type":   nb.Data.Attributes.Type,
			"status": nb.Data.Attributes.Data.Attributes.Status,
		},
	}
	if nb.Data.Attributes.CreatedAt > 0 {
		evt.Timestamp = time.Unix(nb.Data.Attributes.CreatedAt, 0)
	}
	return evt, nil
}

func mapPaymongoEvent(t string) string {
	switch t {
	case "source.chargeable", "payment.paid":
		return "charge.succeeded"
	case "payment.failed":
		return "charge.failed"
	case "payment.refunded", "payment.refund.updated":
		return "refund.succeeded"
	default:
		return "payment_intent.requires_action"
	}
}

func parsePaymongoSig(h string) (ts, testSig, liveSig string) {
	if h == "" {
		return "", "", ""
	}
	for _, part := range strings.Split(h, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "te":
			testSig = kv[1]
		case "li":
			liveSig = kv[1]
		}
	}
	return
}

// ---- helpers -------------------------------------------------------------

func (a *Adapter) doBasic(ctx context.Context, verb, path, idem string, in, out any) error {
	var reqBody *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	url := a.base() + path
	var req *http.Request
	var err error
	if reqBody == nil {
		req, err = http.NewRequestWithContext(ctx, verb, url, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, verb, url, reqBody)
	}
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(a.cfg.SecretKey+":")))
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("paymongo: http %d", resp.StatusCode)
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

func firstHeader(h map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := h[k]; ok && v != "" {
			return v
		}
	}
	return ""
}
