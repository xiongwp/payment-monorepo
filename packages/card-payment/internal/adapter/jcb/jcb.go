// Package jcb 实现 processor.Network，调 JCB Net (J/Smart) REST API。
//
// 协议依据：JCB Authorization API v1（多数 PSP 实际通过 JCB acquirer 中转，但
// 本端的 wire shape 一致）
//   - mTLS：JCB 项目证书
//   - 鉴权：HMAC-SHA256 over (timestamp + method + path + body_sha256_hex)
//     Header: X-JCB-MERCHANT-ID / X-JCB-TIMESTAMP / X-JCB-SIGNATURE
//   - Endpoints：
//       Authorize: POST /api/v1/payments
//       Capture:   POST /api/v1/payments/{id}/capture
//       Refund:    POST /api/v1/payments/{id}/refund
//       Void:      POST /api/v1/payments/{id}/void
//       Inquiry:   GET  /api/v1/payments/{id}
//
// 注意：JCB 多数交易在日本市场，币种 JPY 0 小数；其它币种 2 小数。J/Secure 是
// JCB 自家 3DS。
package jcb

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
	MerchantID      string
	APISecret       string // HMAC key
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
		BaseURL: cfg.Endpoint, ClientCertPath: cfg.ClientCert,
		ClientKeyPath: cfg.ClientKey, ServerCAPath: cfg.ServerCA,
		InsecureSkipVerify: cfg.InsecureSandbox, Timeout: cfg.Timeout,
		MaxRetries: 3, Logger: logger,
	})
	if err != nil {
		logger.Error("jcb adapter init failed", zap.Error(err))
		a.mock = true
		return a
	}
	a.hc = hc
	return a
}

func (a *Adapter) Name() string { return "jcb" }

type authReq struct {
	MerchantTransactionID string `json:"merchant_transaction_id"`
	TransactionType       string `json:"transaction_type"` // SALE / AUTH
	Amount                int64  `json:"amount"`           // 整数（minor unit）
	Currency              string `json:"currency"`
	Card                  struct {
		Number     string `json:"number"`
		Expiry     string `json:"expiry"` // MMYY
		HolderName string `json:"holder_name,omitempty"`
	} `json:"card"`
	Authentication *struct {
		Cavv string `json:"cavv,omitempty"`
		Eci  string `json:"eci,omitempty"`
	} `json:"authentication,omitempty"`
	MerchantDescriptor string `json:"merchant_descriptor,omitempty"`
}

type authResp struct {
	TransactionID  string `json:"transaction_id"`
	Status         string `json:"status"`
	ApprovalCode   string `json:"approval_code"`
	DeclineCode    string `json:"decline_code,omitempty"`
	DeclineMessage string `json:"decline_message,omitempty"`
	AVSResult      string `json:"avs_result,omitempty"`
	CVVResult      string `json:"cvv_result,omitempty"`
	FraudScore     int    `json:"fraud_score,omitempty"`
}

func (a *Adapter) Authorize(ctx context.Context, req *processor.NetworkAuthRequest) (*processor.NetworkAuthResponse, error) {
	if req == nil || req.PAN == "" {
		return nil, errors.New("jcb: pan required")
	}
	if a.mock {
		return mockAuthorize(req), nil
	}
	body := authReq{
		MerchantTransactionID: req.IdempotencyKey,
		TransactionType:       "SALE",
		Amount:                req.Amount,
		Currency:              req.Currency,
		MerchantDescriptor:    req.MerchantDescriptor,
	}
	body.Card.Number = req.PAN
	body.Card.Expiry = fmt.Sprintf("%02d%02d", req.ExpMonth, req.ExpYear%100)
	body.Card.HolderName = req.HolderName
	if req.ThreeDS != nil {
		body.Authentication = &struct {
			Cavv string `json:"cavv,omitempty"`
			Eci  string `json:"eci,omitempty"`
		}{Cavv: req.ThreeDS.CAVV, Eci: req.ThreeDS.ECI}
	}

	path := "/api/v1/payments"
	headers, payload, err := a.signedHeaders("POST", path, body)
	if err != nil {
		return nil, fmt.Errorf("jcb sign: %w", err)
	}
	resp, err := a.hc.PostJSON(ctx, path, headers, json.RawMessage(payload))
	if err != nil {
		return nil, fmt.Errorf("jcb http: %w", err)
	}
	var rb authResp
	if err := json.Unmarshal(resp.Body, &rb); err != nil {
		return nil, fmt.Errorf("jcb decode: %w", err)
	}
	out := &processor.NetworkAuthResponse{
		NetworkRefNo: rb.TransactionID,
		ARN:          rb.ApprovalCode,
		MaskedPAN:    maskPAN(req.PAN),
		Network:      "jcb",
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
	body := map[string]any{"amount": req.Amount, "currency": req.Currency}
	path := "/api/v1/payments/" + req.NetworkRefNo + "/capture"
	headers, _, err := a.signedHeaders("POST", path, body)
	if err != nil {
		return nil, err
	}
	resp, err := a.hc.PostJSON(ctx, path, headers, body)
	if err != nil {
		return nil, fmt.Errorf("jcb capture: %w", err)
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
		return &processor.NetworkRefundResponse{RefundRefNo: "jcbrf_" + req.NetworkRefNo, Status: "refunded", RefundedAmount: req.Amount}, nil
	}
	body := map[string]any{"amount": req.Amount, "currency": req.Currency, "reason": req.Reason}
	path := "/api/v1/payments/" + req.NetworkRefNo + "/refund"
	headers, _, err := a.signedHeaders("POST", path, body)
	if err != nil {
		return nil, err
	}
	resp, err := a.hc.PostJSON(ctx, path, headers, body)
	if err != nil {
		return nil, fmt.Errorf("jcb refund: %w", err)
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
	path := "/api/v1/payments/" + req.NetworkRefNo + "/void"
	headers, _, err := a.signedHeaders("POST", path, body)
	if err != nil {
		return nil, err
	}
	resp, err := a.hc.PostJSON(ctx, path, headers, body)
	if err != nil {
		return nil, fmt.Errorf("jcb void: %w", err)
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
	path := "/api/v1/payments/" + req.NetworkRefNo
	headers, _, err := a.signedHeaders("GET", path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.hc.GetJSON(ctx, path, headers)
	if err != nil {
		return nil, fmt.Errorf("jcb query: %w", err)
	}
	var rb struct {
		TransactionID string `json:"transaction_id"`
		Status        string `json:"status"`
		Amount        int64  `json:"amount"`
		Currency      string `json:"currency"`
	}
	_ = json.Unmarshal(resp.Body, &rb)
	out := &processor.NetworkQueryResponse{NetworkRefNo: rb.TransactionID, Amount: rb.Amount, Currency: rb.Currency}
	switch strings.ToUpper(rb.Status) {
	case "APPROVED", "CAPTURED":
		out.Status = "approved"
	case "DECLINED":
		out.Status = "declined"
	case "VOIDED":
		out.Status = "voided"
	case "PENDING":
		out.Status = "pending"
	default:
		out.Status = strings.ToLower(rb.Status)
	}
	return out, nil
}

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
		"X-JCB-MERCHANT-ID": a.cfg.MerchantID,
		"X-JCB-TIMESTAMP":   ts,
		"X-JCB-SIGNATURE":   sig,
		"Accept":            "application/json",
	}, payload, nil
}

// mockAuthorize:
//   3528 / 3589 (经典 JCB BIN): approved
//   3500:                          declined SOFT
//   3501:                          declined HARD
func mockAuthorize(req *processor.NetworkAuthRequest) *processor.NetworkAuthResponse {
	bin := req.PAN
	if len(bin) > 4 {
		bin = bin[:4]
	}
	out := &processor.NetworkAuthResponse{
		NetworkRefNo: "jcbmock_" + req.IdempotencyKey,
		ARN:          "ARN_" + req.IdempotencyKey,
		MaskedPAN:    maskPAN(req.PAN),
		Network:      "jcb",
	}
	switch bin {
	case "3528", "3589":
		out.Status = "approved"
		out.AVSResult = "Y"
		out.CVVResult = "M"
		out.ThreeDSEci = "05"
		out.ThreeDSStatus = "Y"
		out.FraudScore = 10
	case "3500":
		out.Status = "declined"
		out.DeclineCode = "INSUFFICIENT_FUNDS"
		out.DeclineCategory = "SOFT"
	case "3501":
		out.Status = "declined"
		out.DeclineCode = "FRAUDULENT_TRANSACTION"
		out.DeclineCategory = "HARD"
		out.FraudScore = 90
	default:
		out.Status = "approved"
		out.AVSResult = "U"
		out.CVVResult = "U"
		out.FraudScore = 25
	}
	return out
}

func mapEciToStatus(eci string) string {
	switch strings.TrimSpace(eci) {
	case "02", "05":
		return "Y"
	case "01", "06":
		return "A"
	case "07":
		return "N"
	}
	return ""
}

func maskPAN(pan string) string {
	if len(pan) < 12 {
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
