// Package amex 实现 processor.Network，调 American Express API (Direct / OptBlue)。
//
// 协议依据：AmEx Authorization Service API v2 (REST/JSON)
//   - mTLS：AmEx 项目证书 + AmEx CA root
//   - 鉴权：HMAC-SHA256 over (timestamp + method + path + body_sha256_hex)
//     Header: X-AMEX-API-KEY: <api_key>
//             X-AMEX-CLIENT-ID: <client_id>
//             X-AMEX-TIMESTAMP: <unix_ms>
//             X-AMEX-SIGNATURE: hex(hmac_sha256(api_secret, canonical))
//   - Endpoints (subset, OptBlue 模式实际由 acquirer 转 AmEx)：
//       Authorize: POST /payments/digital/v2/payments
//       Capture:   POST /payments/digital/v2/payments/{id}/capture
//       Refund:    POST /payments/digital/v2/payments/{id}/refund
//       Void:      POST /payments/digital/v2/payments/{id}/void
//       Inquiry:   GET  /payments/digital/v2/payments/{id}
//
// 注意：AmEx PAN 可能是 15 位（37/34 BIN），mask 函数要兼容。SafeKey 是 AmEx 自家
// 3DS（ECI 06 = 完整认证；ECI 07 = 失败）。
package amex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/card-payment/internal/adapter/httpx"
	"github.com/xiongwp/card-payment/internal/processor"
)

type Adapter struct {
	cfg    Config
	hc     *httpx.Client
	logger *zap.Logger
	mock   bool
}

type Config struct {
	Endpoint        string
	APIKey          string
	ClientID        string
	APISecret       string // HMAC key（来自 KMS）
	ClientCert      string
	ClientKey       string
	ServerCA        string
	Timeout         time.Duration
	Mock            bool
	InsecureSandbox bool
}

func New(cfg Config, logger *zap.Logger) *Adapter {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	mock := cfg.Mock || cfg.Endpoint == ""
	a := &Adapter{cfg: cfg, logger: logger, mock: mock}
	if mock {
		return a
	}
	hc, err := httpx.New(httpx.Config{
		BaseURL:            cfg.Endpoint,
		ClientCertPath:     cfg.ClientCert,
		ClientKeyPath:      cfg.ClientKey,
		ServerCAPath:       cfg.ServerCA,
		InsecureSkipVerify: cfg.InsecureSandbox,
		Timeout:            cfg.Timeout,
		MaxRetries:         3,
		Logger:             logger,
	})
	if err != nil {
		logger.Error("amex adapter init failed", zap.Error(err))
		a.mock = true
		return a
	}
	a.hc = hc
	return a
}

func (a *Adapter) Name() string { return "amex" }

type authReq struct {
	MerchantTransactionID string `json:"merchant_transaction_id"` // = idempotency_key
	TransactionType       string `json:"transaction_type"`        // SALE / AUTH
	Amount                struct {
		Value    string `json:"value"`
		Currency string `json:"currency"`
	} `json:"amount"`
	Card struct {
		Number      string `json:"number"`
		Expiry      string `json:"expiry"` // MM/YY
		HolderName  string `json:"holder_name,omitempty"`
	} `json:"card"`
	Authentication *struct {
		Cavv string `json:"cavv,omitempty"`
		Eci  string `json:"eci,omitempty"`
	} `json:"authentication,omitempty"`
	MerchantDescriptor string `json:"merchant_descriptor,omitempty"`
}

type authResp struct {
	TransactionID  string `json:"transaction_id"`  // = network_ref_no
	Status         string `json:"status"`          // APPROVED / DECLINED / PENDING
	ApprovalCode   string `json:"approval_code"`
	DeclineCode    string `json:"decline_code,omitempty"`
	DeclineMessage string `json:"decline_message,omitempty"`
	AVSResult      string `json:"avs_result,omitempty"`
	CVVResult      string `json:"cvv_result,omitempty"`
	FraudScore     int    `json:"fraud_score,omitempty"`
}

func (a *Adapter) Authorize(ctx context.Context, req *processor.NetworkAuthRequest) (*processor.NetworkAuthResponse, error) {
	if req == nil || req.PAN == "" {
		return nil, errors.New("amex: pan required")
	}
	if a.mock {
		return mockAuthorize(req), nil
	}
	body := authReq{
		MerchantTransactionID: req.IdempotencyKey,
		TransactionType:       "SALE",
		MerchantDescriptor:    req.MerchantDescriptor,
	}
	body.Amount.Value = formatAmount(req.Amount, req.Currency)
	body.Amount.Currency = req.Currency
	body.Card.Number = req.PAN
	body.Card.Expiry = fmt.Sprintf("%02d/%02d", req.ExpMonth, req.ExpYear%100)
	body.Card.HolderName = req.HolderName
	if req.ThreeDS != nil {
		body.Authentication = &struct {
			Cavv string `json:"cavv,omitempty"`
			Eci  string `json:"eci,omitempty"`
		}{Cavv: req.ThreeDS.CAVV, Eci: req.ThreeDS.ECI}
	}

	path := "/payments/digital/v2/payments"
	headers, payload, err := a.signedHeaders("POST", path, body)
	if err != nil {
		return nil, fmt.Errorf("amex sign: %w", err)
	}
	resp, err := a.hc.PostJSON(ctx, path, headers, json.RawMessage(payload))
	if err != nil {
		return nil, fmt.Errorf("amex http: %w", err)
	}
	var rb authResp
	if err := json.Unmarshal(resp.Body, &rb); err != nil {
		return nil, fmt.Errorf("amex decode: %w", err)
	}
	out := &processor.NetworkAuthResponse{
		NetworkRefNo: rb.TransactionID,
		ARN:          rb.ApprovalCode,
		MaskedPAN:    maskPAN(req.PAN),
		Network:      "amex",
		AVSResult:    strings.ToUpper(strings.TrimSpace(rb.AVSResult)),
		CVVResult:    strings.ToUpper(strings.TrimSpace(rb.CVVResult)),
		FraudScore:   rb.FraudScore,
	}
	if req.ThreeDS != nil {
		out.ThreeDSEci = req.ThreeDS.ECI
		out.ThreeDSStatus = mapEciToStatus(req.ThreeDS.ECI)
	}
	switch strings.ToUpper(rb.Status) {
	case "APPROVED":
		out.Status = "approved"
	case "PENDING":
		out.Status = "pending"
	case "DECLINED":
		out.Status = "declined"
		out.DeclineCode = rb.DeclineCode
		out.DeclineReason = rb.DeclineMessage
		out.DeclineCategory = httpx.MapDeclineCode(out.DeclineCode)
	default:
		out.Status = "error"
		out.DeclineReason = rb.DeclineMessage
	}
	return out, nil
}

func (a *Adapter) Capture(ctx context.Context, req *processor.NetworkCaptureRequest) (*processor.NetworkCaptureResponse, error) {
	if a.mock {
		return &processor.NetworkCaptureResponse{NetworkRefNo: req.NetworkRefNo, Status: "captured", CapturedAmount: req.Amount}, nil
	}
	body := map[string]any{"amount": map[string]string{"value": formatAmount(req.Amount, req.Currency), "currency": req.Currency}}
	path := "/payments/digital/v2/payments/" + req.NetworkRefNo + "/capture"
	headers, _, err := a.signedHeaders("POST", path, body)
	if err != nil {
		return nil, err
	}
	resp, err := a.hc.PostJSON(ctx, path, headers, body)
	if err != nil {
		return nil, fmt.Errorf("amex capture: %w", err)
	}
	var rb struct {
		TransactionID string `json:"transaction_id"`
		Status        string `json:"status"`
	}
	_ = json.Unmarshal(resp.Body, &rb)
	out := &processor.NetworkCaptureResponse{NetworkRefNo: rb.TransactionID, CapturedAmount: req.Amount}
	if strings.EqualFold(rb.Status, "CAPTURED") {
		out.Status = "captured"
	} else {
		out.Status = strings.ToLower(rb.Status)
	}
	return out, nil
}

func (a *Adapter) Refund(ctx context.Context, req *processor.NetworkRefundRequest) (*processor.NetworkRefundResponse, error) {
	if a.mock {
		return &processor.NetworkRefundResponse{RefundRefNo: "amexrf_" + req.NetworkRefNo, Status: "refunded", RefundedAmount: req.Amount}, nil
	}
	body := map[string]any{"amount": map[string]string{"value": formatAmount(req.Amount, req.Currency), "currency": req.Currency}, "reason": req.Reason}
	path := "/payments/digital/v2/payments/" + req.NetworkRefNo + "/refund"
	headers, _, err := a.signedHeaders("POST", path, body)
	if err != nil {
		return nil, err
	}
	resp, err := a.hc.PostJSON(ctx, path, headers, body)
	if err != nil {
		return nil, fmt.Errorf("amex refund: %w", err)
	}
	var rb struct {
		RefundID string `json:"refund_id"`
		Status   string `json:"status"`
	}
	_ = json.Unmarshal(resp.Body, &rb)
	out := &processor.NetworkRefundResponse{RefundRefNo: rb.RefundID, RefundedAmount: req.Amount}
	if strings.EqualFold(rb.Status, "REFUNDED") {
		out.Status = "refunded"
	} else {
		out.Status = strings.ToLower(rb.Status)
	}
	return out, nil
}

func (a *Adapter) Void(ctx context.Context, req *processor.NetworkVoidRequest) (*processor.NetworkVoidResponse, error) {
	if a.mock {
		return &processor.NetworkVoidResponse{Status: "voided"}, nil
	}
	body := map[string]any{"reason": req.Reason}
	path := "/payments/digital/v2/payments/" + req.NetworkRefNo + "/void"
	headers, _, err := a.signedHeaders("POST", path, body)
	if err != nil {
		return nil, err
	}
	resp, err := a.hc.PostJSON(ctx, path, headers, body)
	if err != nil {
		return nil, fmt.Errorf("amex void: %w", err)
	}
	var rb struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(resp.Body, &rb)
	if strings.EqualFold(rb.Status, "VOIDED") {
		return &processor.NetworkVoidResponse{Status: "voided"}, nil
	}
	return &processor.NetworkVoidResponse{Status: strings.ToLower(rb.Status)}, nil
}

func (a *Adapter) Query(ctx context.Context, req *processor.NetworkQueryRequest) (*processor.NetworkQueryResponse, error) {
	if a.mock {
		return &processor.NetworkQueryResponse{NetworkRefNo: req.NetworkRefNo, Status: "approved"}, nil
	}
	path := "/payments/digital/v2/payments/" + req.NetworkRefNo
	headers, _, err := a.signedHeaders("GET", path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.hc.GetJSON(ctx, path, headers)
	if err != nil {
		return nil, fmt.Errorf("amex query: %w", err)
	}
	var rb struct {
		TransactionID string `json:"transaction_id"`
		Status        string `json:"status"`
		Amount        struct {
			Value    string `json:"value"`
			Currency string `json:"currency"`
		} `json:"amount"`
	}
	_ = json.Unmarshal(resp.Body, &rb)
	amt, _ := parseAmount(rb.Amount.Value, rb.Amount.Currency)
	out := &processor.NetworkQueryResponse{NetworkRefNo: rb.TransactionID, Amount: amt, Currency: rb.Amount.Currency}
	switch strings.ToUpper(rb.Status) {
	case "APPROVED", "CAPTURED":
		out.Status = "approved"
	case "DECLINED":
		out.Status = "declined"
	case "VOIDED", "REVERSED":
		out.Status = "voided"
	case "PENDING":
		out.Status = "pending"
	default:
		out.Status = strings.ToLower(rb.Status)
	}
	return out, nil
}

// signedHeaders AmEx HMAC：canonical = ts + "\n" + method + "\n" + path + "\n" + sha256_hex(body)
func (a *Adapter) signedHeaders(method, path string, body any) (map[string]string, []byte, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	bodyHash := httpx.SHA256Base64(payload)
	canonical := ts + "\n" + strings.ToUpper(method) + "\n" + path + "\n" + bodyHash
	sig := httpx.HMACSHA256Hex([]byte(a.cfg.APISecret), []byte(canonical))
	return map[string]string{
		"X-AMEX-API-KEY":    a.cfg.APIKey,
		"X-AMEX-CLIENT-ID":  a.cfg.ClientID,
		"X-AMEX-TIMESTAMP":  ts,
		"X-AMEX-SIGNATURE":  sig,
		"Accept":            "application/json",
	}, payload, nil
}

// mockAuthorize:
//   3782 / 3714 (经典 AmEx BIN): approved
//   3700:                          declined SOFT
//   3701:                          declined HARD (suspected_fraud)
func mockAuthorize(req *processor.NetworkAuthRequest) *processor.NetworkAuthResponse {
	bin := req.PAN
	if len(bin) > 4 {
		bin = bin[:4]
	}
	out := &processor.NetworkAuthResponse{
		NetworkRefNo: "amexmock_" + req.IdempotencyKey,
		ARN:          "ARN_" + req.IdempotencyKey,
		MaskedPAN:    maskPAN(req.PAN),
		Network:      "amex",
	}
	switch bin {
	case "3782", "3714":
		out.Status = "approved"
		out.AVSResult = "Y"
		out.CVVResult = "M"
		out.ThreeDSEci = "06"
		out.ThreeDSStatus = "Y"
		out.FraudScore = 12
	case "3700":
		out.Status = "declined"
		out.DeclineCode = "INSUFFICIENT_FUNDS"
		out.DeclineCategory = "SOFT"
	case "3701":
		out.Status = "declined"
		out.DeclineCode = "SUSPECTED_FRAUD"
		out.DeclineCategory = "HARD"
		out.FraudScore = 92
	default:
		out.Status = "approved"
		out.AVSResult = "U"
		out.CVVResult = "U"
		out.FraudScore = 30
	}
	return out
}

// formatAmount AmEx 用 decimal string；JPY/KRW 0 小数。
func formatAmount(amountMinor int64, currency string) string {
	switch strings.ToUpper(currency) {
	case "JPY", "KRW", "VND", "ISK":
		return strconv.FormatInt(amountMinor, 10)
	}
	if amountMinor < 0 {
		amountMinor = -amountMinor
	}
	return fmt.Sprintf("%d.%02d", amountMinor/100, amountMinor%100)
}

func parseAmount(s, currency string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	switch strings.ToUpper(currency) {
	case "JPY", "KRW", "VND", "ISK":
		return strconv.ParseInt(s, 10, 64)
	}
	parts := strings.Split(s, ".")
	major, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, err
	}
	var minor int64
	if len(parts) > 1 {
		f := parts[1]
		if len(f) > 2 {
			f = f[:2]
		}
		for len(f) < 2 {
			f += "0"
		}
		minor, _ = strconv.ParseInt(f, 10, 64)
	}
	return major*100 + minor, nil
}

func mapEciToStatus(eci string) string {
	// AmEx SafeKey: 06 = full auth, 07 = failed
	switch strings.TrimSpace(eci) {
	case "05", "06":
		return "Y"
	case "07":
		return "N"
	}
	return ""
}

// maskPAN AmEx 兼容 15 位
func maskPAN(pan string) string {
	if len(pan) < 11 {
		return pan
	}
	out := make([]byte, len(pan))
	copy(out, pan[:6])
	for i := 6; i < len(pan)-4; i++ {
		out[i] = '*'
	}
	copy(out[len(pan)-4:], pan[len(pan)-4:])
	return string(out)
}
