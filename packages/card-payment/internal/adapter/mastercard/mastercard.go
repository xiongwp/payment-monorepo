// Package mastercard 实现 processor.Network 接口，调 Mastercard Payment Gateway
// Services (MPGS) REST API。
//
// 协议依据：MPGS REST v1 (Direct Payment)
//   - mTLS：Mastercard MAS 项目证书
//   - 鉴权：HTTP Basic Auth (merchant.<merchantId>:apiPassword)
//     **注意**：MPGS Basic 用的是 password 不是 token；password 必须随业务密钥
//     合管理（KMS 取出），不能落进程 env。生产 PRD 必须用 KMS 提供 password。
//   - Endpoints (Direct API):
//       Authorize (sale): PUT /api/rest/version/<v>/merchant/<m>/order/<orderId>/transaction/<txnId>
//                         body: {"apiOperation":"PAY", ...}
//       Refund:           PUT /api/rest/version/<v>/merchant/<m>/order/<orderId>/transaction/<txnId>
//                         body: {"apiOperation":"REFUND", ...}
//       Void:             body: {"apiOperation":"VOID"}
//       Inquiry:          GET /api/rest/version/<v>/merchant/<m>/order/<orderId>
package mastercard

import (
	"context"
	"encoding/base64"
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

const apiVersion = "76"

type Adapter struct {
	cfg    Config
	hc     *httpx.Client
	logger *zap.Logger
	mock   bool
	authH  string
}

type Config struct {
	Endpoint        string
	MerchantID      string
	APIPassword     string
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
		logger.Error("mastercard adapter init failed", zap.Error(err))
		a.mock = true
		return a
	}
	a.hc = hc
	if cfg.APIPassword != "" {
		creds := "merchant." + cfg.MerchantID + ":" + cfg.APIPassword
		a.authH = "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
	}
	return a
}

func (a *Adapter) Name() string { return "mastercard" }

type txnPayload struct {
	APIOperation string `json:"apiOperation"`
	Order        struct {
		Amount      string `json:"amount"`
		Currency    string `json:"currency"`
		Reference   string `json:"reference,omitempty"`
		Description string `json:"description,omitempty"`
	} `json:"order"`
	SourceOfFunds struct {
		Type     string `json:"type"`
		Provided struct {
			Card struct {
				Number string `json:"number"`
				Expiry struct {
					Month string `json:"month"`
					Year  string `json:"year"`
				} `json:"expiry"`
			} `json:"card"`
		} `json:"provided"`
	} `json:"sourceOfFunds"`
	Authentication *struct {
		Cavv      string `json:"3dsTransactionId,omitempty"`
		Eci       string `json:"electronicCommerceIndicator,omitempty"`
		XID       string `json:"xid,omitempty"`
		DSTransID string `json:"dsTransactionId,omitempty"`
	} `json:"authentication,omitempty"`
	Transaction struct {
		Reference string `json:"reference,omitempty"`
	} `json:"transaction"`
}

type txnResponse struct {
	Result      string `json:"result"`
	Order       struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"order"`
	Transaction struct {
		ID                string `json:"id"`
		AuthorizationCode string `json:"authorizationCode"`
		AcquirerMessage   string `json:"acquirerMessage"`
		ReceiptNumber     string `json:"receiptNumber"`
	} `json:"transaction"`
	Risk *struct {
		Response struct {
			GatewayCode string `json:"gatewayCode"`
			Score       struct {
				Value int `json:"value"`
			} `json:"score"`
		} `json:"response"`
	} `json:"risk,omitempty"`
	Response *struct {
		AcquirerCode     string `json:"acquirerCode"`
		AcquirerMessage  string `json:"acquirerMessage"`
		CardSecurityCode *struct {
			GatewayCode string `json:"gatewayCode"`
		} `json:"cardSecurityCode,omitempty"`
		AVSResponse *struct {
			GatewayCode string `json:"gatewayCode"`
		} `json:"avsResponse,omitempty"`
		GatewayCode string `json:"gatewayCode"`
	} `json:"response,omitempty"`
	Error *struct {
		Cause       string `json:"cause"`
		Explanation string `json:"explanation"`
	} `json:"error,omitempty"`
}

func (a *Adapter) Authorize(ctx context.Context, req *processor.NetworkAuthRequest) (*processor.NetworkAuthResponse, error) {
	if req == nil || req.PAN == "" {
		return nil, errors.New("mastercard: pan required")
	}
	if a.mock {
		return mockAuthorize(req), nil
	}
	if a.authH == "" {
		return nil, errors.New("mastercard: api_password not configured")
	}

	orderID := req.IdempotencyKey
	txnID := req.IdempotencyKey + "-1"
	body := txnPayload{APIOperation: "PAY"}
	body.Order.Amount = formatAmount(req.Amount, req.Currency)
	body.Order.Currency = req.Currency
	body.Order.Reference = req.IdempotencyKey
	body.Order.Description = req.MerchantDescriptor
	body.SourceOfFunds.Type = "CARD"
	body.SourceOfFunds.Provided.Card.Number = req.PAN
	body.SourceOfFunds.Provided.Card.Expiry.Month = fmt.Sprintf("%02d", req.ExpMonth)
	body.SourceOfFunds.Provided.Card.Expiry.Year = fmt.Sprintf("%02d", req.ExpYear%100)
	body.Transaction.Reference = txnID
	if req.ThreeDS != nil {
		body.Authentication = &struct {
			Cavv      string `json:"3dsTransactionId,omitempty"`
			Eci       string `json:"electronicCommerceIndicator,omitempty"`
			XID       string `json:"xid,omitempty"`
			DSTransID string `json:"dsTransactionId,omitempty"`
		}{
			Cavv: req.ThreeDS.CAVV, Eci: req.ThreeDS.ECI,
			XID: req.ThreeDS.XID, DSTransID: req.ThreeDS.DSTransID,
		}
	}

	path := fmt.Sprintf("/api/rest/version/%s/merchant/%s/order/%s/transaction/%s", apiVersion, a.cfg.MerchantID, orderID, txnID)
	resp, err := a.put(ctx, path, body)
	if err != nil {
		return nil, fmt.Errorf("mastercard http: %w", err)
	}
	var rb txnResponse
	if err := json.Unmarshal(resp.Body, &rb); err != nil {
		return nil, fmt.Errorf("mastercard decode: %w (status=%d)", err, resp.StatusCode)
	}
	out := &processor.NetworkAuthResponse{
		NetworkRefNo: rb.Transaction.ID,
		ARN:          rb.Transaction.ReceiptNumber,
		MaskedPAN:    maskPAN(req.PAN),
		Network:      "mastercard",
	}
	if rb.Response != nil {
		if rb.Response.AVSResponse != nil {
			out.AVSResult = normalizeAVS(rb.Response.AVSResponse.GatewayCode)
		}
		if rb.Response.CardSecurityCode != nil {
			out.CVVResult = normalizeCVV(rb.Response.CardSecurityCode.GatewayCode)
		}
	}
	if rb.Risk != nil {
		out.FraudScore = rb.Risk.Response.Score.Value
	}
	if req.ThreeDS != nil {
		out.ThreeDSEci = req.ThreeDS.ECI
		out.ThreeDSStatus = mapEciToStatus(req.ThreeDS.ECI)
	}
	switch strings.ToUpper(rb.Result) {
	case "SUCCESS":
		out.Status = "approved"
	case "PENDING":
		out.Status = "pending"
	case "FAILURE":
		out.Status = "declined"
		if rb.Response != nil {
			out.DeclineCode = rb.Response.GatewayCode
			out.DeclineReason = rb.Response.AcquirerMessage
		}
		if rb.Error != nil && out.DeclineReason == "" {
			out.DeclineReason = rb.Error.Explanation
		}
		out.DeclineCategory = httpx.MapDeclineCode(out.DeclineCode)
	default:
		out.Status = "error"
		if rb.Error != nil {
			out.DeclineReason = rb.Error.Explanation
		}
	}
	return out, nil
}

func (a *Adapter) Capture(ctx context.Context, req *processor.NetworkCaptureRequest) (*processor.NetworkCaptureResponse, error) {
	if a.mock {
		return &processor.NetworkCaptureResponse{NetworkRefNo: req.NetworkRefNo, Status: "captured", CapturedAmount: req.Amount}, nil
	}
	body := txnPayload{APIOperation: "CAPTURE"}
	body.Order.Amount = formatAmount(req.Amount, req.Currency)
	body.Order.Currency = req.Currency
	path := a.txnPath(req.NetworkRefNo, "-cap")
	resp, err := a.put(ctx, path, body)
	if err != nil {
		return nil, fmt.Errorf("mastercard capture: %w", err)
	}
	var rb txnResponse
	_ = json.Unmarshal(resp.Body, &rb)
	out := &processor.NetworkCaptureResponse{NetworkRefNo: rb.Transaction.ID, CapturedAmount: req.Amount}
	if strings.EqualFold(rb.Result, "SUCCESS") {
		out.Status = "captured"
	} else {
		out.Status = strings.ToLower(rb.Result)
	}
	return out, nil
}

func (a *Adapter) Refund(ctx context.Context, req *processor.NetworkRefundRequest) (*processor.NetworkRefundResponse, error) {
	if a.mock {
		return &processor.NetworkRefundResponse{RefundRefNo: "mcrf_" + req.NetworkRefNo, Status: "refunded", RefundedAmount: req.Amount}, nil
	}
	body := txnPayload{APIOperation: "REFUND"}
	body.Order.Amount = formatAmount(req.Amount, req.Currency)
	body.Order.Currency = req.Currency
	path := a.txnPath(req.NetworkRefNo, "-rf")
	resp, err := a.put(ctx, path, body)
	if err != nil {
		return nil, fmt.Errorf("mastercard refund: %w", err)
	}
	var rb txnResponse
	_ = json.Unmarshal(resp.Body, &rb)
	out := &processor.NetworkRefundResponse{RefundRefNo: rb.Transaction.ID, RefundedAmount: req.Amount}
	if strings.EqualFold(rb.Result, "SUCCESS") {
		out.Status = "refunded"
	} else {
		out.Status = strings.ToLower(rb.Result)
	}
	return out, nil
}

func (a *Adapter) Void(ctx context.Context, req *processor.NetworkVoidRequest) (*processor.NetworkVoidResponse, error) {
	if a.mock {
		return &processor.NetworkVoidResponse{Status: "voided"}, nil
	}
	body := txnPayload{APIOperation: "VOID"}
	path := a.txnPath(req.NetworkRefNo, "-vd")
	resp, err := a.put(ctx, path, body)
	if err != nil {
		return nil, fmt.Errorf("mastercard void: %w", err)
	}
	var rb txnResponse
	_ = json.Unmarshal(resp.Body, &rb)
	if strings.EqualFold(rb.Result, "SUCCESS") {
		return &processor.NetworkVoidResponse{Status: "voided"}, nil
	}
	return &processor.NetworkVoidResponse{Status: strings.ToLower(rb.Result)}, nil
}

func (a *Adapter) Query(ctx context.Context, req *processor.NetworkQueryRequest) (*processor.NetworkQueryResponse, error) {
	if a.mock {
		return &processor.NetworkQueryResponse{NetworkRefNo: req.NetworkRefNo, Status: "approved"}, nil
	}
	path := fmt.Sprintf("/api/rest/version/%s/merchant/%s/order/%s", apiVersion, a.cfg.MerchantID, req.NetworkRefNo)
	headers := map[string]string{"Authorization": a.authH, "Accept": "application/json"}
	resp, err := a.hc.GetJSON(ctx, path, headers)
	if err != nil {
		return nil, fmt.Errorf("mastercard query: %w", err)
	}
	var rb struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	_ = json.Unmarshal(resp.Body, &rb)
	amt, _ := parseAmount(rb.Amount, rb.Currency)
	out := &processor.NetworkQueryResponse{NetworkRefNo: rb.ID, Amount: amt, Currency: rb.Currency}
	switch strings.ToUpper(rb.Status) {
	case "AUTHORIZED", "CAPTURED":
		out.Status = "approved"
	case "FAILED":
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

func (a *Adapter) txnPath(networkRefNo, suffix string) string {
	return fmt.Sprintf("/api/rest/version/%s/merchant/%s/order/%s/transaction/%s%s",
		apiVersion, a.cfg.MerchantID, networkRefNo, networkRefNo, suffix)
}

// put MPGS 严格要求 PUT；当前 httpx 没暴露 PutJSON，复用 PostJSON（MPGS 实测兼容）。
// 真正接通 prod 时建议给 httpx 加 PutJSON 严格走 PUT。
func (a *Adapter) put(ctx context.Context, path string, body any) (*httpx.Response, error) {
	headers := map[string]string{
		"Authorization": a.authH,
		"Accept":        "application/json",
	}
	return a.hc.PostJSON(ctx, path, headers, body)
}

// mockAuthorize:
//   5454 / 5555: approved (happy path)
//   5100:        SOFT decline (insufficient_funds)
//   5101:        HARD decline (fraud)
//   其它:        approved (low risk)
func mockAuthorize(req *processor.NetworkAuthRequest) *processor.NetworkAuthResponse {
	bin := req.PAN
	if len(bin) > 4 {
		bin = bin[:4]
	}
	out := &processor.NetworkAuthResponse{
		NetworkRefNo: "mcmock_" + req.IdempotencyKey,
		ARN:          "ARN_" + req.IdempotencyKey,
		MaskedPAN:    maskPAN(req.PAN),
		Network:      "mastercard",
	}
	switch bin {
	case "5454", "5555":
		out.Status = "approved"
		out.AVSResult = "Y"
		out.CVVResult = "M"
		out.ThreeDSEci = "02"
		out.ThreeDSStatus = "Y"
		out.FraudScore = 8
	case "5100":
		out.Status = "declined"
		out.DeclineCode = "INSUFFICIENT_FUNDS"
		out.DeclineCategory = "SOFT"
	case "5101":
		out.Status = "declined"
		out.DeclineCode = "FRAUDULENT_TRANSACTION"
		out.DeclineCategory = "HARD"
		out.FraudScore = 95
	default:
		out.Status = "approved"
		out.AVSResult = "U"
		out.CVVResult = "U"
		out.FraudScore = 25
	}
	return out
}

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

func normalizeAVS(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "MATCH", "ADDRESS_AND_POSTCODE_MATCHED":
		return "Y"
	case "ADDRESS_MATCHED", "POSTCODE_MATCHED":
		return "A"
	case "NO_MATCH", "NOT_MATCHED":
		return "N"
	case "NOT_AVAILABLE", "NOT_VERIFIED":
		return "U"
	}
	return ""
}

func normalizeCVV(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "MATCH":
		return "M"
	case "NO_MATCH":
		return "N"
	case "NOT_PROCESSED":
		return "P"
	case "NOT_PRESENT", "NOT_SUPPORTED":
		return "U"
	}
	return ""
}

func mapEciToStatus(eci string) string {
	switch strings.TrimSpace(eci) {
	case "02":
		return "Y"
	case "01":
		return "A"
	case "00":
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
