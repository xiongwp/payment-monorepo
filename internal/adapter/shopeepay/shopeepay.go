// Package shopeepay 实现 ShopeePay adapter（SeaMoney Partner Platform）。
//
// 参考：https://docs.shopeepay.com/
// 鉴权：partner_id + partner_key，HMAC-SHA256 hex of "partner_id|timestamp|path|body"。
// 走 Authorization: SHA256 Credential=<partner_id>,Signature=<hex> 头。
// 流程：创建订单 -> 返回 deeplink / payment_link -> 用户在 Shopee App 授权 -> 回调。
package shopeepay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseSandbox = "https://uat-partner.shopeepay.com"
	baseProd    = "https://partner.shopeepay.com"

	pathCharge = "/v3/merchant-host/order/create"
	pathRefund = "/v3/merchant-host/refund/create"
	pathQuery  = "/v3/merchant-host/order/query"
)

type Config struct {
	Env          string
	PartnerID    string
	PartnerKey   string
	MerchantExtID string
	StoreExtID    string
	NotifyURL    string
	BaseURL string // 非空时覆盖 sandbox/prod，用于 mockserver
}

type Adapter struct {
	cfg Config
	h   *http.Client
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, h: channel.NewHTTPClient(20 * time.Second)}
}

func (a *Adapter) Name() string { return "shopeepay" }

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

type chargeReq struct {
	RequestID             string `json:"request_id"`
	MerchantExtID         string `json:"merchant_ext_id"`
	StoreExtID            string `json:"store_ext_id,omitempty"`
	PaymentReferenceID    string `json:"payment_reference_id"`
	Amount                int64  `json:"amount"`
	Currency              string `json:"currency"`
	PaymentExpiryDuration int    `json:"payment_expiry_duration"`
	NotifyURL             string `json:"notify_url,omitempty"`
	RedirectURL           string `json:"redirect_url,omitempty"`
	Description           string `json:"description,omitempty"`
}

type chargeResp struct {
	OrderSN            string `json:"order_sn"`
	PaymentLink        string `json:"payment_link"`
	DeepLink           string `json:"deeplink"`
	State              string `json:"state"`
	PaymentReferenceID string `json:"payment_reference_id"`
	ErrorCode          string `json:"error_code"`
	ErrorMessage       string `json:"error_message"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	body := chargeReq{
		RequestID:             req.IdempotencyKey,
		MerchantExtID:         a.cfg.MerchantExtID,
		StoreExtID:            a.cfg.StoreExtID,
		PaymentReferenceID:    req.PiID,
		Amount:                req.Amount,
		Currency:              req.Currency,
		PaymentExpiryDuration: 900,
		NotifyURL:             orDefault(req.NotifyURL, a.cfg.NotifyURL),
		RedirectURL:           req.ReturnURL,
		Description:           req.Description,
	}
	var resp chargeResp
	if err := a.doSigned(ctx, http.MethodPost, pathCharge, body, &resp); err != nil {
		return nil, err
	}
	if resp.OrderSN == "" {
		raw := firstNonEmpty(resp.ErrorCode, resp.State)
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			FailureCode:    channel.MapFailure(a.Name(), raw),
			RawFailureCode: raw,
			FailureMessage: resp.ErrorMessage,
		}, nil
	}
	if strings.EqualFold(resp.State, "SUCCESS") {
		return &channel.ChargeResponse{
			Result:        channel.ResultSucceeded,
			ExternalRefNo: resp.OrderSN,
		}, nil
	}
	return &channel.ChargeResponse{
		Result:        channel.ResultRequiresAction,
		ExternalRefNo: resp.OrderSN,
		RequiredAction: &channel.RequiredAction{
			Type:        "app_redirect",
			RedirectURL: firstNonEmpty(resp.DeepLink, resp.PaymentLink),
			Scheme:      "universal",
			ReturnURL:   req.ReturnURL,
			ExpiresAt:   time.Now().Add(15 * time.Minute),
		},
	}, nil
}

// ShopeePay wallet 没有 pre-auth 概念，Capture/Void 返回 no-op。
func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

// ---- Refund --------------------------------------------------------------

type refundReq struct {
	RequestID    string `json:"request_id"`
	OrderSN      string `json:"order_sn"`
	RefundAmount int64  `json:"refund_amount"`
	Reason       string `json:"reason,omitempty"`
}

type refundResp struct {
	RefundSN     string `json:"refund_sn"`
	State        string `json:"state"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	body := refundReq{
		RequestID:    req.IdempotencyKey,
		OrderSN:      req.ExternalRefNo,
		RefundAmount: req.Amount,
		Reason:       req.Reason,
	}
	var resp refundResp
	if err := a.doSigned(ctx, http.MethodPost, pathRefund, body, &resp); err != nil {
		return nil, err
	}
	if resp.RefundSN == "" {
		raw := firstNonEmpty(resp.ErrorCode, resp.State)
		return &channel.OpResponse{
			Result:         channel.ResultFailed,
			FailureCode:    channel.MapFailure(a.Name(), raw),
			RawFailureCode: raw,
			FailureMessage: resp.ErrorMessage,
		}, nil
	}
	if strings.EqualFold(resp.State, "SUCCESS") {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.RefundSN}, nil
	}
	return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.RefundSN}, nil
}

// ---- Query --------------------------------------------------------------

type queryReq struct {
	MerchantExtID      string `json:"merchant_ext_id"`
	PaymentReferenceID string `json:"payment_reference_id"`
}

type queryResp struct {
	OrderSN        string `json:"order_sn"`
	State          string `json:"state"`
	Amount         int64  `json:"amount"`
	RefundedAmount int64  `json:"refunded_amount"`
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	body := queryReq{
		MerchantExtID:      a.cfg.MerchantExtID,
		PaymentReferenceID: req.PiID,
	}
	var resp queryResp
	if err := a.doSigned(ctx, http.MethodPost, pathQuery, body, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch strings.ToUpper(resp.State) {
	case "SUCCESS", "PAID":
		rt = channel.ResultSucceeded
	case "FAIL", "FAILED", "EXPIRED", "CANCELLED":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{
		Result:         rt,
		ExternalRefNo:  firstNonEmpty(resp.OrderSN, req.ExternalRefNo),
		AmountCaptured: resp.Amount,
		AmountRefunded: resp.RefundedAmount,
		Raw:            map[string]string{"state": resp.State},
	}, nil
}

// ---- Webhook -----------------------------------------------------------

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	sig := extractSignature(firstHeader(headers, "Authorization", "authorization"))
	sigOK := channel.ConstantTimeEqHex(sig, channel.HMACSHA256Hex([]byte(a.cfg.PartnerKey), body))
	var nb struct {
		Code               string `json:"code"`
		OrderSN            string `json:"order_sn"`
		RefundSN           string `json:"refund_sn"`
		PaymentReferenceID string `json:"payment_reference_id"`
		Amount             int64  `json:"amount"`
		Timestamp          int64  `json:"timestamp"`
		ErrorCode          string `json:"error_code"`
		ErrorMessage       string `json:"error_message"`
	}
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}
	evt := &channel.WebhookEvent{
		EventID:       firstNonEmpty(nb.RefundSN, nb.OrderSN),
		PiID:          nb.PaymentReferenceID,
		ExternalRefNo: firstNonEmpty(nb.RefundSN, nb.OrderSN),
		Amount:        nb.Amount,
		SignatureOK:   sigOK,
		Raw: map[string]string{
			"code":          nb.Code,
			"error_code":    nb.ErrorCode,
			"error_message": nb.ErrorMessage,
		},
	}
	switch strings.ToUpper(nb.Code) {
	case "PAYMENT_SUCCESS":
		evt.EventType = "charge.succeeded"
	case "PAYMENT_FAILED":
		evt.EventType = "charge.failed"
	case "REFUND_SUCCESS":
		evt.EventType = "refund.succeeded"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	if nb.Timestamp > 0 {
		evt.Timestamp = time.Unix(nb.Timestamp, 0)
	} else {
		evt.Timestamp = time.Now()
	}
	return evt, nil
}

// ---- signing -----------------------------------------------------------

func (a *Adapter) doSigned(ctx context.Context, verb, path string, in, out any) error {
	var bodyBytes []byte
	if in != nil {
		var err error
		bodyBytes, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	canonical := fmt.Sprintf("%s|%s|%s|%s", a.cfg.PartnerID, ts, path, string(bodyBytes))
	sig := channel.HMACSHA256Hex([]byte(a.cfg.PartnerKey), []byte(canonical))

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
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("Authorization", fmt.Sprintf("SHA256 Credential=%s,Signature=%s", a.cfg.PartnerID, sig))

	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("shopeepay: http %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- helpers -----------------------------------------------------------

// extractSignature 从 "SHA256 Credential=xxx,Signature=<hex>" 头里拿 Signature。
func extractSignature(auth string) string {
	if auth == "" {
		return ""
	}
	for _, part := range strings.Split(auth, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "Signature") {
			return strings.TrimSpace(kv[1])
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

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func orDefault(v, d string) string {
	if v != "" {
		return v
	}
	return d
}
