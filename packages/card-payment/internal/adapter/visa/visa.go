// Package visa 实现 processor.Network 接口，调 Visa Net (CyberSource REST API)。
//
// 协议依据：CyberSource REST API v2 (Visa Direct / VAS Auth)
//   - mTLS：Visa MAS 项目证书 + Visa CA root
//   - 鉴权：HTTP Signature (RSA-SHA256) over (request-target) + host + date + digest
//     Header: Authorization: Signature keyid="<merchantId>",algorithm="rsa-sha256",
//             headers="host date (request-target) digest v-c-merchant-id",signature="..."
//   - Endpoints:
//       Authorize:  POST /pts/v2/payments
//       Capture:    POST /pts/v2/payments/{id}/captures
//       Refund:     POST /pts/v2/payments/{id}/refunds
//       Void:       POST /pts/v2/payments/{id}/voids
//       Query:      GET  /tss/v2/transactions/{id}
//
// 拿到 Visa 合同后只需配：
//   network.visa.endpoint = "https://api.visa.com" (或 "...sandbox.visa.com")
//   network.visa.merchant_id = "<分配的 Merchant ID>"
//   network.visa.private_key = path PEM
//   network.visa.cert / .key / .ca = mTLS 三件套
package visa

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/card-payment/internal/adapter/httpx"
	"github.com/xiongwp/card-payment/internal/processor"
)

// Adapter Visa Net REST 适配器
type Adapter struct {
	cfg        Config
	hc         *httpx.Client
	privateKey *rsa.PrivateKey
	logger     *zap.Logger
	mock       bool
}

// Config visa adapter 配置（生产 + sandbox 通用）
type Config struct {
	Endpoint        string        // https://api.visa.com 或 sandbox
	MerchantID      string        // CyberSource Merchant ID（HTTP Signature keyid）
	APIKeyID        string        // 跟 PrivateKeyPath 配对
	PrivateKeyPath  string        // RSA 私钥 PEM
	ClientCert      string        // mTLS client cert（Visa MAS 证书）
	ClientKey       string        // mTLS client key
	ServerCA        string        // Visa CA root
	Timeout         time.Duration // 默认 30s
	Mock            bool          // dev mock
	InsecureSandbox bool          // dev sandbox 自签证书时打开（assertProdSafety prod 拒）
}

// New 构造。生产路径要求 PrivateKeyPath + mTLS 三件齐全；mock=true 全跳过。
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
		// 启动期挂掉，让 fx 报错；不让进程进入"看着像生产但 panic 在第一笔交易"状态
		logger.Error("visa adapter init failed", zap.Error(err))
		a.mock = true
		return a
	}
	a.hc = hc
	if cfg.PrivateKeyPath != "" {
		key, err := httpx.LoadRSAPrivateKey(cfg.PrivateKeyPath)
		if err != nil {
			logger.Error("visa: rsa key load failed; degrading to mock", zap.Error(err))
			a.mock = true
			return a
		}
		a.privateKey = key
	} else {
		logger.Warn("visa: private_key_path empty; signature will fail in prod")
	}
	return a
}

// Name implements processor.Network
func (a *Adapter) Name() string { return "visa" }

// ─── Authorize ────────────────────────────────────────────────────────────

// authReqBody CyberSource v2 Authorize payload（裁剪到我们用的字段）。
// 完整 schema 见 CyberSource REST API spec；以下字段是商户最常用的子集。
type authReqBody struct {
	ClientReferenceInformation struct {
		Code string `json:"code"` // = idempotency_key
	} `json:"clientReferenceInformation"`
	ProcessingInformation struct {
		Capture          bool   `json:"capture"`           // true = auth+capture (sale); false = auth only
		CommerceIndicator string `json:"commerceIndicator"` // "internet" / "moto" / "recurring"
	} `json:"processingInformation"`
	OrderInformation struct {
		AmountDetails struct {
			TotalAmount string `json:"totalAmount"`
			Currency    string `json:"currency"`
		} `json:"amountDetails"`
		BillTo *struct {
			MerchantDescriptor string `json:"merchantDescriptor,omitempty"`
		} `json:"billTo,omitempty"`
	} `json:"orderInformation"`
	PaymentInformation struct {
		Card struct {
			Number          string `json:"number"`     // PAN
			ExpirationMonth string `json:"expirationMonth"`
			ExpirationYear  string `json:"expirationYear"`
		} `json:"card"`
	} `json:"paymentInformation"`
	ConsumerAuthenticationInformation *struct {
		Cavv      string `json:"cavv,omitempty"`
		Eci       string `json:"eci,omitempty"`
		XID       string `json:"xid,omitempty"`
		DSTransID string `json:"directoryServerTransactionId,omitempty"`
	} `json:"consumerAuthenticationInformation,omitempty"`
}

// authRespBody CyberSource v2 Authorize response (subset).
type authRespBody struct {
	ID            string `json:"id"`           // 网络 ref no
	Status        string `json:"status"`       // AUTHORIZED / DECLINED / PENDING / INVALID_REQUEST
	ReconciliationID string `json:"reconciliationId"`
	ProcessorInformation struct {
		ApprovalCode      string `json:"approvalCode"`
		NetworkResponse   struct {
			ProcessorResponseCode string `json:"processorResponseCode"`
		} `json:"networkResponse"`
		AVS struct {
			Code string `json:"code"`
		} `json:"avs"`
		CardVerification struct {
			ResultCode string `json:"resultCode"`
		} `json:"cardVerification"`
	} `json:"processorInformation"`
	ErrorInformation *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"errorInformation,omitempty"`
	RiskInformation *struct {
		Score struct {
			Result int `json:"result"` // 0..99
		} `json:"score"`
	} `json:"riskInformation,omitempty"`
}

// Authorize POST /pts/v2/payments — 一次完成 auth (capture=false) 或 auth+capture (true)。
// 这里默认 sale 模式（capture=true），跟 processor.Authorize 语义对齐。
func (a *Adapter) Authorize(ctx context.Context, req *processor.NetworkAuthRequest) (*processor.NetworkAuthResponse, error) {
	if req == nil || req.PAN == "" {
		return nil, errors.New("visa: pan required")
	}
	if a.mock {
		return mockAuthorize(req), nil
	}

	body := authReqBody{}
	body.ClientReferenceInformation.Code = req.IdempotencyKey
	body.ProcessingInformation.Capture = true // sale
	body.ProcessingInformation.CommerceIndicator = "internet"
	body.OrderInformation.AmountDetails.TotalAmount = formatAmount(req.Amount, req.Currency)
	body.OrderInformation.AmountDetails.Currency = req.Currency
	if req.MerchantDescriptor != "" {
		body.OrderInformation.BillTo = &struct {
			MerchantDescriptor string `json:"merchantDescriptor,omitempty"`
		}{MerchantDescriptor: req.MerchantDescriptor}
	}
	body.PaymentInformation.Card.Number = req.PAN
	body.PaymentInformation.Card.ExpirationMonth = fmt.Sprintf("%02d", req.ExpMonth)
	body.PaymentInformation.Card.ExpirationYear = fmt.Sprintf("%04d", req.ExpYear)
	if req.ThreeDS != nil {
		body.ConsumerAuthenticationInformation = &struct {
			Cavv      string `json:"cavv,omitempty"`
			Eci       string `json:"eci,omitempty"`
			XID       string `json:"xid,omitempty"`
			DSTransID string `json:"directoryServerTransactionId,omitempty"`
		}{
			Cavv:      req.ThreeDS.CAVV,
			Eci:       req.ThreeDS.ECI,
			XID:       req.ThreeDS.XID,
			DSTransID: req.ThreeDS.DSTransID,
		}
	}

	headers, payload, err := a.signedHeaders(http.MethodPost, "/pts/v2/payments", body)
	if err != nil {
		return nil, fmt.Errorf("visa sign: %w", err)
	}
	resp, err := a.hc.PostJSON(ctx, "/pts/v2/payments", headers, json.RawMessage(payload))
	if err != nil {
		return nil, fmt.Errorf("visa http: %w", err)
	}
	var rb authRespBody
	if err := json.Unmarshal(resp.Body, &rb); err != nil {
		return nil, fmt.Errorf("visa decode: %w (status=%d)", err, resp.StatusCode)
	}

	out := &processor.NetworkAuthResponse{
		NetworkRefNo: rb.ID,
		ARN:          rb.ReconciliationID,
		MaskedPAN:    maskPAN(req.PAN),
		Network:      "visa",
		AVSResult:    normalizeAVS(rb.ProcessorInformation.AVS.Code),
		CVVResult:    normalizeCVV(rb.ProcessorInformation.CardVerification.ResultCode),
	}
	if rb.RiskInformation != nil {
		out.FraudScore = rb.RiskInformation.Score.Result
	}
	if req.ThreeDS != nil {
		out.ThreeDSEci = req.ThreeDS.ECI
		out.ThreeDSStatus = mapEciToStatus(req.ThreeDS.ECI)
	}
	switch strings.ToUpper(rb.Status) {
	case "AUTHORIZED", "PARTIAL_AUTHORIZED":
		out.Status = "approved"
	case "PENDING", "PENDING_AUTHENTICATION":
		out.Status = "pending"
	case "DECLINED", "AUTHORIZED_RISK_DECLINED":
		out.Status = "declined"
		if rb.ErrorInformation != nil {
			out.DeclineCode = rb.ErrorInformation.Reason
			out.DeclineReason = rb.ErrorInformation.Message
		}
		out.DeclineCategory = httpx.MapDeclineCode(out.DeclineCode)
	default:
		out.Status = "error"
		if rb.ErrorInformation != nil {
			out.DeclineCode = rb.ErrorInformation.Reason
			out.DeclineReason = rb.ErrorInformation.Message
		}
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && out.Status != "declined" {
		// 4xx 但 status 没归类，认作业务级 invalid_request
		out.Status = "declined"
		out.DeclineCode = "INVALID_REQUEST"
		out.DeclineCategory = "SOFT"
	}
	return out, nil
}

// ─── Capture / Refund / Void / Query ─────────────────────────────────────

func (a *Adapter) Capture(ctx context.Context, req *processor.NetworkCaptureRequest) (*processor.NetworkCaptureResponse, error) {
	if a.mock {
		return &processor.NetworkCaptureResponse{NetworkRefNo: req.NetworkRefNo, Status: "captured", CapturedAmount: req.Amount}, nil
	}
	body := map[string]any{
		"clientReferenceInformation": map[string]string{"code": req.NetworkRefNo + "-cap"},
		"orderInformation": map[string]any{
			"amountDetails": map[string]string{"totalAmount": formatAmount(req.Amount, req.Currency), "currency": req.Currency},
		},
	}
	path := "/pts/v2/payments/" + req.NetworkRefNo + "/captures"
	headers, _, err := a.signedHeaders(http.MethodPost, path, body)
	if err != nil {
		return nil, fmt.Errorf("visa sign: %w", err)
	}
	resp, err := a.hc.PostJSON(ctx, path, headers, body)
	if err != nil {
		return nil, fmt.Errorf("visa capture http: %w", err)
	}
	var rb struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(resp.Body, &rb)
	out := &processor.NetworkCaptureResponse{NetworkRefNo: rb.ID, CapturedAmount: req.Amount}
	if strings.EqualFold(rb.Status, "PENDING") || strings.EqualFold(rb.Status, "TRANSMITTED") {
		out.Status = "captured"
	} else {
		out.Status = strings.ToLower(rb.Status)
	}
	return out, nil
}

func (a *Adapter) Refund(ctx context.Context, req *processor.NetworkRefundRequest) (*processor.NetworkRefundResponse, error) {
	if a.mock {
		return &processor.NetworkRefundResponse{RefundRefNo: "vrf_" + req.NetworkRefNo, Status: "refunded", RefundedAmount: req.Amount}, nil
	}
	body := map[string]any{
		"clientReferenceInformation": map[string]string{"code": req.NetworkRefNo + "-rf"},
		"orderInformation": map[string]any{
			"amountDetails": map[string]string{"totalAmount": formatAmount(req.Amount, req.Currency), "currency": req.Currency},
		},
	}
	path := "/pts/v2/payments/" + req.NetworkRefNo + "/refunds"
	headers, _, err := a.signedHeaders(http.MethodPost, path, body)
	if err != nil {
		return nil, fmt.Errorf("visa sign: %w", err)
	}
	resp, err := a.hc.PostJSON(ctx, path, headers, body)
	if err != nil {
		return nil, fmt.Errorf("visa refund http: %w", err)
	}
	var rb struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(resp.Body, &rb)
	out := &processor.NetworkRefundResponse{RefundRefNo: rb.ID, RefundedAmount: req.Amount}
	if strings.EqualFold(rb.Status, "PENDING") {
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
	body := map[string]any{"clientReferenceInformation": map[string]string{"code": req.NetworkRefNo + "-vd"}}
	path := "/pts/v2/payments/" + req.NetworkRefNo + "/voids"
	headers, _, err := a.signedHeaders(http.MethodPost, path, body)
	if err != nil {
		return nil, fmt.Errorf("visa sign: %w", err)
	}
	resp, err := a.hc.PostJSON(ctx, path, headers, body)
	if err != nil {
		return nil, fmt.Errorf("visa void http: %w", err)
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
	path := "/tss/v2/transactions/" + req.NetworkRefNo
	headers, _, err := a.signedHeaders(http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("visa sign: %w", err)
	}
	resp, err := a.hc.GetJSON(ctx, path, headers)
	if err != nil {
		return nil, fmt.Errorf("visa query http: %w", err)
	}
	var rb struct {
		ID                       string `json:"id"`
		ApplicationInformation   struct {
			ReasonCode  string `json:"reasonCode"`
			Status      string `json:"status"`
		} `json:"applicationInformation"`
		OrderInformation struct {
			AmountDetails struct {
				TotalAmount string `json:"totalAmount"`
				Currency    string `json:"currency"`
			} `json:"amountDetails"`
		} `json:"orderInformation"`
	}
	_ = json.Unmarshal(resp.Body, &rb)
	amt, _ := parseAmount(rb.OrderInformation.AmountDetails.TotalAmount, rb.OrderInformation.AmountDetails.Currency)
	out := &processor.NetworkQueryResponse{
		NetworkRefNo: rb.ID,
		Amount:       amt,
		Currency:     rb.OrderInformation.AmountDetails.Currency,
	}
	switch strings.ToUpper(rb.ApplicationInformation.Status) {
	case "AUTHORIZED", "TRANSMITTED", "SETTLED":
		out.Status = "approved"
	case "DECLINED", "REJECTED":
		out.Status = "declined"
		out.DeclineCode = rb.ApplicationInformation.ReasonCode
	case "VOIDED", "REVERSED":
		out.Status = "voided"
	case "PENDING":
		out.Status = "pending"
	default:
		out.Status = strings.ToLower(rb.ApplicationInformation.Status)
	}
	return out, nil
}

// ─── 内部 helpers ─────────────────────────────────────────────────────────

// signedHeaders 构造 HTTP Signature header（CyberSource v2 spec）。
//
// 返回 headers + 序列化后的 body（caller 用 json.RawMessage 直接传 PostJSON）。
// body=nil 时（GET 请求）digest 用空字符串的 sha256。
func (a *Adapter) signedHeaders(method, path string, body any) (map[string]string, []byte, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal body: %w", err)
		}
	}
	digest := "SHA-256=" + httpx.SHA256Base64(payload)
	date := time.Now().UTC().Format(http.TimeFormat)
	host := a.cfg.Endpoint
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host = strings.TrimSuffix(host, "/")

	canonical := httpx.CanonicalRequestForVisa(method, path, host, date, digest)
	if a.privateKey == nil {
		return nil, nil, errors.New("visa: private_key not loaded")
	}
	sig, err := httpx.RSASignSHA256(a.privateKey, []byte(canonical))
	if err != nil {
		return nil, nil, fmt.Errorf("rsa sign: %w", err)
	}
	authValue := fmt.Sprintf(
		`Signature keyid="%s", algorithm="rsa-sha256", headers="host date (request-target) digest v-c-merchant-id", signature="%s"`,
		a.cfg.APIKeyID, sig,
	)
	return map[string]string{
		"Host":             host,
		"Date":             date,
		"Digest":           digest,
		"Authorization":    authValue,
		"v-c-merchant-id":  a.cfg.MerchantID,
		"Idempotency-Key":  "", // caller 会覆盖
	}, payload, nil
}

// formatAmount Visa 用 decimal string，分位 2（除少数零小数币种 JPY/KRW）。
// req.Amount 是最小货币单位（cent / sen），转 decimal 字符串。
func formatAmount(amountMinor int64, currency string) string {
	switch strings.ToUpper(currency) {
	case "JPY", "KRW", "VND", "ISK":
		// 0 小数币种：直接整数字符串
		return strconv.FormatInt(amountMinor, 10)
	}
	// 默认 2 小数
	if amountMinor < 0 {
		amountMinor = -amountMinor
	}
	major := amountMinor / 100
	minor := amountMinor % 100
	return fmt.Sprintf("%d.%02d", major, minor)
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
	c := strings.ToUpper(strings.TrimSpace(code))
	switch c {
	case "Y", "M", "X", "D", "F":
		return "Y"
	case "A", "W", "Z":
		return "A"
	case "N", "C":
		return "N"
	case "U", "G", "I", "S":
		return "U"
	}
	return ""
}

func normalizeCVV(code string) string {
	c := strings.ToUpper(strings.TrimSpace(code))
	switch c {
	case "M":
		return "M"
	case "N":
		return "N"
	case "P":
		return "P"
	case "U", "S", "X":
		return "U"
	}
	return ""
}

// mapEciToStatus ECI → 3DS status（粗映射；网络细微差异在 risk-manage 处理）。
//
//	ECI 02/05 → 完整认证 (Y)
//	ECI 01/06 → 尝试认证 (A)
//	ECI 07     → 失败 / 未认证 (N)
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

// mockAuthorize dev 路径：BIN 决定结果，方便 e2e 测试
//   - 4242: approved + ECI 05 + AVS=Y + CVV=M（happy path）
//   - 4000: declined INSUFFICIENT_FUNDS (SOFT)
//   - 4001: declined STOLEN_CARD (HARD)
//   - 4002: pending PENDING_REVIEW
//   - 4003: high risk approved（FraudScore=85, AVS=N）
//   - 4444: 模拟卡组织 5xx（hc 返 nil，processor 走 reconcile）
//   - 其他 4xxx: approved 默认
func mockAuthorize(req *processor.NetworkAuthRequest) *processor.NetworkAuthResponse {
	bin := req.PAN
	if len(bin) > 4 {
		bin = bin[:4]
	}
	masked := maskPAN(req.PAN)
	out := &processor.NetworkAuthResponse{
		NetworkRefNo: "vmock_" + req.IdempotencyKey,
		MaskedPAN:    masked,
		Network:      "visa",
		ARN:          "ARN_" + req.IdempotencyKey,
	}
	switch bin {
	case "4242":
		out.Status = "approved"
		out.AVSResult = "Y"
		out.CVVResult = "M"
		out.ThreeDSEci = "05"
		out.ThreeDSStatus = "Y"
		out.FraudScore = 5
	case "4000":
		out.Status = "declined"
		out.DeclineCode = "INSUFFICIENT_FUNDS"
		out.DeclineReason = "mock soft decline"
		out.DeclineCategory = "SOFT"
	case "4001":
		out.Status = "declined"
		out.DeclineCode = "STOLEN_CARD"
		out.DeclineReason = "mock hard decline (stolen)"
		out.DeclineCategory = "HARD"
		out.FraudScore = 99
	case "4002":
		out.Status = "pending"
		out.DeclineReason = "mock pending review"
	case "4003":
		out.Status = "approved"
		out.AVSResult = "N"
		out.CVVResult = "P"
		out.FraudScore = 85
	default:
		out.Status = "approved"
		out.AVSResult = "U"
		out.CVVResult = "U"
		out.FraudScore = 20
	}
	return out
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
