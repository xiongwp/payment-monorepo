// Package billease 实现 BillEase BNPL adapter（First Digital Finance Corp Merchant API）。
//
// 参考：https://developers.billease.ph/
// 鉴权：OAuth2 client credentials（client_id + client_secret），短期 bearer。
// 流程：创建 checkout -> 返回 redirect_url -> 用户做信用审批 -> 回调。
// 因 BNPL 需要信用审核，Charge 一律返回 RequiresAction。
package billease

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseSandbox = "https://api-sandbox.billease.ph"
	baseProd    = "https://api.billease.ph"

	pathToken    = "/api/v1/oauth/token"
	pathCheckout = "/api/v2/checkout/create"
	pathGet      = "/api/v2/checkout/%s"
	pathRefund   = "/api/v2/checkout/%s/refund"
)

type Config struct {
	Env          string
	ClientID     string
	ClientSecret string
	CallbackURL  string
	SuccessURL   string
	FailureURL   string
	CancelURL    string
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

func (a *Adapter) Name() string { return "billease" }

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

type tokenReq struct {
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

type tokenResp struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

func (a *Adapter) getToken(ctx context.Context) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if a.cachedToken != "" && time.Now().Before(a.tokenExpiry) {
		return a.cachedToken, nil
	}
	body, err := json.Marshal(tokenReq{
		GrantType:    "client_credentials",
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.ClientSecret,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+pathToken, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.h.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("billease: token http %d", resp.StatusCode)
	}
	var out tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("billease: empty token")
	}
	a.cachedToken = out.AccessToken
	ttl := out.ExpiresIn - 30
	if ttl < 30 {
		ttl = 30
	}
	a.tokenExpiry = time.Now().Add(time.Duration(ttl) * time.Second)
	return a.cachedToken, nil
}

// ---- Charge -----------------------------------------------------------

type customer struct {
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Email     string `json:"email,omitempty"`
	Phone     string `json:"phone,omitempty"`
}

type item struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
	Price    int64  `json:"price"`
}

type returnURLs struct {
	Success string `json:"success"`
	Failure string `json:"failure"`
	Cancel  string `json:"cancel"`
}

type checkoutReq struct {
	ReferenceID string     `json:"reference_id"`
	Amount      int64      `json:"amount"`
	Currency    string     `json:"currency"`
	Customer    customer   `json:"customer"`
	Items       []item     `json:"items"`
	CallbackURL string     `json:"callback_url,omitempty"`
	ReturnURL   returnURLs `json:"return_url"`
}

type checkoutResp struct {
	CheckoutID   string `json:"checkout_id"`
	Status       string `json:"status"`
	RedirectURL  string `json:"redirect_url"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	successURL := orDefault(req.ReturnURL, a.cfg.SuccessURL)
	body := checkoutReq{
		ReferenceID: req.IdempotencyKey,
		Amount:      req.Amount,
		Currency:    req.Currency,
		Customer: customer{
			FirstName: req.Buyer.FirstName,
			LastName:  req.Buyer.LastName,
			Email:     req.Buyer.Email,
			Phone:     req.Buyer.Phone,
		},
		Items: []item{{
			Name:     orDefault(req.Description, "Order "+req.PiID),
			Quantity: 1,
			Price:    req.Amount,
		}},
		CallbackURL: orDefault(req.NotifyURL, a.cfg.CallbackURL),
		ReturnURL: returnURLs{
			Success: successURL,
			Failure: orDefault(a.cfg.FailureURL, successURL),
			Cancel:  orDefault(a.cfg.CancelURL, successURL),
		},
	}
	var resp checkoutResp
	if err := a.doBearer(ctx, http.MethodPost, pathCheckout, body, &resp); err != nil {
		return nil, err
	}
	if resp.CheckoutID == "" || resp.RedirectURL == "" {
		raw := firstNonEmpty(resp.ErrorCode, resp.Status)
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			FailureCode:    channel.MapFailure(a.Name(), raw),
			RawFailureCode: raw,
			FailureMessage: resp.ErrorMessage,
		}, nil
	}
	// BNPL 永远需要跳转做信用审批。
	return &channel.ChargeResponse{
		Result:        channel.ResultRequiresAction,
		ExternalRefNo: resp.CheckoutID,
		RequiredAction: &channel.RequiredAction{
			Type:        "app_redirect",
			RedirectURL: resp.RedirectURL,
			Scheme:      "universal",
			ReturnURL:   req.ReturnURL,
			ExpiresAt:   time.Now().Add(30 * time.Minute),
		},
	}, nil
}

// BNPL 没有 pre-auth；Capture/Void 做 no-op。
func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

// ---- Refund -----------------------------------------------------------

type refundReq struct {
	ReferenceID string `json:"reference_id"`
	Amount      int64  `json:"amount"`
	Reason      string `json:"reason,omitempty"`
}

type refundResp struct {
	RefundID     string `json:"refund_id"`
	Status       string `json:"status"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	path := fmt.Sprintf(pathRefund, req.ExternalRefNo)
	body := refundReq{
		ReferenceID: req.IdempotencyKey,
		Amount:      req.Amount,
		Reason:      req.Reason,
	}
	var resp refundResp
	if err := a.doBearer(ctx, http.MethodPost, path, body, &resp); err != nil {
		return nil, err
	}
	switch strings.ToLower(resp.Status) {
	case "succeeded", "success", "refunded":
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.RefundID}, nil
	case "failed", "rejected":
		raw := firstNonEmpty(resp.ErrorCode, resp.Status)
		return &channel.OpResponse{
			Result:         channel.ResultFailed,
			ExternalRefNo:  resp.RefundID,
			FailureCode:    channel.MapFailure(a.Name(), raw),
			RawFailureCode: raw,
			FailureMessage: resp.ErrorMessage,
		}, nil
	default:
		return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: resp.RefundID}, nil
	}
}

// ---- Query -----------------------------------------------------------

type queryResp struct {
	CheckoutID     string `json:"checkout_id"`
	Status         string `json:"status"`
	Amount         int64  `json:"amount"`
	AmountRefunded int64  `json:"amount_refunded"`
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	path := fmt.Sprintf(pathGet, req.ExternalRefNo)
	var resp queryResp
	if err := a.doBearer(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch strings.ToLower(resp.Status) {
	case "paid", "approved":
		rt = channel.ResultSucceeded
	case "rejected", "expired", "cancelled", "canceled":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{
		Result:         rt,
		ExternalRefNo:  firstNonEmpty(resp.CheckoutID, req.ExternalRefNo),
		AmountCaptured: resp.Amount,
		AmountRefunded: resp.AmountRefunded,
		Raw:            map[string]string{"status": resp.Status},
	}, nil
}

// ---- Webhook --------------------------------------------------------

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	sig := firstHeader(headers, "X-BillEase-Signature", "X-Billease-Signature", "x-billease-signature")
	sigOK := channel.ConstantTimeEqHex(sig, channel.HMACSHA256Hex([]byte(a.cfg.ClientSecret), body))
	var nb struct {
		Event       string `json:"event"`
		CheckoutID  string `json:"checkout_id"`
		ReferenceID string `json:"reference_id"`
		Status      string `json:"status"`
		Amount      int64  `json:"amount"`
		Timestamp   string `json:"timestamp"`
	}
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}
	evt := &channel.WebhookEvent{
		EventID:       nb.CheckoutID,
		PiID:          nb.ReferenceID,
		ExternalRefNo: nb.CheckoutID,
		Amount:        nb.Amount,
		SignatureOK:   sigOK,
		Raw: map[string]string{
			"event":  nb.Event,
			"status": nb.Status,
		},
	}
	switch strings.ToLower(nb.Event) {
	case "checkout.approved", "checkout.paid":
		evt.EventType = "charge.succeeded"
	case "checkout.rejected", "checkout.expired", "checkout.cancelled":
		evt.EventType = "charge.failed"
	case "refund.succeeded", "refund.completed":
		evt.EventType = "refund.succeeded"
	case "refund.failed":
		evt.EventType = "refund.failed"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	if t, err := time.Parse(time.RFC3339, nb.Timestamp); err == nil {
		evt.Timestamp = t
	} else {
		evt.Timestamp = time.Now()
	}
	return evt, nil
}

// ---- helpers -------------------------------------------------------

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

	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("billease: http %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
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
