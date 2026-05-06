// Package unionpay 实现 processor.Network，调中国银联（CUP）UnionPay
// International API (UPOP / UPI Cross-border)。
//
// 协议依据：UPI 收单接口 v5.1（form-encoded + RSA-SHA256 签名）
//   - mTLS：可选（部分银联接口走 server-auth + 应用层 RSA 签名替代）
//   - 鉴权：表单字段按 key 升序拼 → RSA-SHA256 → base64 → 写入 signature 字段
//     verify 端用银联颁发的公钥校验
//   - Endpoints：
//       Authorize: POST /gateway/api/backTransReq.do
//                  txnType=01 (consume) / 02 (preAuth)
//       Capture:   POST /gateway/api/backTransReq.do  txnType=03 (preAuth complete)
//       Refund:    POST /gateway/api/backTransReq.do  txnType=04
//       Void:      POST /gateway/api/backTransReq.do  txnType=31 (consume reverse)
//       Inquiry:   POST /gateway/api/queryTrans.do
//
// 注意：人民币 (CNY) 用分（minor unit），跨境美元 (USD) 也用分。订单号长度
// 8-40 位，必须商户内唯一。orderId 可以复用我们的 idempotency_key。
package unionpay

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/card-payment/internal/adapter/httpx"
	"github.com/xiongwp/card-payment/internal/processor"
)

type Adapter struct {
	cfg        Config
	hc         *httpx.Client
	logger     *zap.Logger
	mock       bool
	privateKey *rsa.PrivateKey
}

type Config struct {
	Endpoint        string
	MerchantID      string // 银联分配，15 位
	CertID          string // 商户签名证书序列号
	PrivateKeyPath  string
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
		logger.Error("unionpay adapter init failed", zap.Error(err))
		a.mock = true
		return a
	}
	a.hc = hc
	if cfg.PrivateKeyPath != "" {
		key, err := httpx.LoadRSAPrivateKey(cfg.PrivateKeyPath)
		if err != nil {
			logger.Error("unionpay rsa key load failed", zap.Error(err))
			a.mock = true
			return a
		}
		a.privateKey = key
	}
	return a
}

func (a *Adapter) Name() string { return "unionpay" }

// 银联固定字段（部分）：
//   version=5.1.0  encoding=UTF-8  signMethod=01 (RSA)
//   bizType=000201 (B2C 网关) / 000301 (TOKEN)
//   accessType=0 (商户直连)
//   txnType txnSubType txnTime orderId txnAmount currencyCode merId certId
//   accNo (PAN, 加密 / 明文取决于业务)  signature
const (
	upVersion    = "5.1.0"
	upEncoding   = "UTF-8"
	upSignMethod = "01"
	upBizType    = "000201"
	upAccessType = "0"
)

func (a *Adapter) Authorize(ctx context.Context, req *processor.NetworkAuthRequest) (*processor.NetworkAuthResponse, error) {
	if req == nil || req.PAN == "" {
		return nil, errors.New("unionpay: pan required")
	}
	if a.mock {
		return mockAuthorize(req), nil
	}
	form := a.baseForm("01", "01") // 01/01 = 直接消费
	form.Set("orderId", req.IdempotencyKey)
	form.Set("txnTime", time.Now().Format("20060102150405"))
	form.Set("txnAmount", strconv.FormatInt(req.Amount, 10)) // minor unit
	form.Set("currencyCode", currencyToISONum(req.Currency))
	form.Set("accNo", req.PAN) // 实际生产应用银联公钥加密 PAN，dev 简化
	form.Set("certId", a.cfg.CertID)

	if err := a.signForm(form); err != nil {
		return nil, fmt.Errorf("unionpay sign: %w", err)
	}

	resp, err := a.hc.PostForm(ctx, "/gateway/api/backTransReq.do", nil, form)
	if err != nil {
		return nil, fmt.Errorf("unionpay http: %w", err)
	}
	rb, err := parseFormResponse(resp.Body)
	if err != nil {
		return nil, err
	}
	out := &processor.NetworkAuthResponse{
		NetworkRefNo:  rb.Get("queryId"),       // 银联返的查询流水号
		ARN:           rb.Get("traceNo"),
		MaskedPAN:     maskPAN(req.PAN),
		Network:       "unionpay",
	}
	respCode := rb.Get("respCode")
	switch respCode {
	case "00":
		out.Status = "approved"
	case "03", "04", "05": // 部分商户处于风控审核
		out.Status = "pending"
	default:
		out.Status = "declined"
		out.DeclineCode = respCode
		out.DeclineReason = rb.Get("respMsg")
		out.DeclineCategory = mapUPDeclineCode(respCode)
	}
	if req.ThreeDS != nil {
		out.ThreeDSEci = req.ThreeDS.ECI
		out.ThreeDSStatus = mapEciToStatus(req.ThreeDS.ECI)
	}
	return out, nil
}

func (a *Adapter) Capture(ctx context.Context, req *processor.NetworkCaptureRequest) (*processor.NetworkCaptureResponse, error) {
	if a.mock {
		return &processor.NetworkCaptureResponse{NetworkRefNo: req.NetworkRefNo, Status: "captured", CapturedAmount: req.Amount}, nil
	}
	form := a.baseForm("03", "00") // 预授权完成
	form.Set("orderId", req.NetworkRefNo+"-cap")
	form.Set("txnTime", time.Now().Format("20060102150405"))
	form.Set("txnAmount", strconv.FormatInt(req.Amount, 10))
	form.Set("currencyCode", currencyToISONum(req.Currency))
	form.Set("origQryId", req.NetworkRefNo)
	form.Set("certId", a.cfg.CertID)
	if err := a.signForm(form); err != nil {
		return nil, err
	}
	resp, err := a.hc.PostForm(ctx, "/gateway/api/backTransReq.do", nil, form)
	if err != nil {
		return nil, fmt.Errorf("unionpay capture: %w", err)
	}
	rb, _ := parseFormResponse(resp.Body)
	out := &processor.NetworkCaptureResponse{NetworkRefNo: rb.Get("queryId"), CapturedAmount: req.Amount}
	if rb.Get("respCode") == "00" {
		out.Status = "captured"
	} else {
		out.Status = "failed"
	}
	return out, nil
}

func (a *Adapter) Refund(ctx context.Context, req *processor.NetworkRefundRequest) (*processor.NetworkRefundResponse, error) {
	if a.mock {
		return &processor.NetworkRefundResponse{RefundRefNo: "uprf_" + req.NetworkRefNo, Status: "refunded", RefundedAmount: req.Amount}, nil
	}
	form := a.baseForm("04", "00") // 退货
	form.Set("orderId", req.NetworkRefNo+"-rf")
	form.Set("txnTime", time.Now().Format("20060102150405"))
	form.Set("txnAmount", strconv.FormatInt(req.Amount, 10))
	form.Set("currencyCode", currencyToISONum(req.Currency))
	form.Set("origQryId", req.NetworkRefNo)
	form.Set("certId", a.cfg.CertID)
	if err := a.signForm(form); err != nil {
		return nil, err
	}
	resp, err := a.hc.PostForm(ctx, "/gateway/api/backTransReq.do", nil, form)
	if err != nil {
		return nil, fmt.Errorf("unionpay refund: %w", err)
	}
	rb, _ := parseFormResponse(resp.Body)
	out := &processor.NetworkRefundResponse{RefundRefNo: rb.Get("queryId"), RefundedAmount: req.Amount}
	if rb.Get("respCode") == "00" {
		out.Status = "refunded"
	} else {
		out.Status = "failed"
	}
	return out, nil
}

func (a *Adapter) Void(ctx context.Context, req *processor.NetworkVoidRequest) (*processor.NetworkVoidResponse, error) {
	if a.mock {
		return &processor.NetworkVoidResponse{Status: "voided"}, nil
	}
	form := a.baseForm("31", "00") // 消费撤销
	form.Set("orderId", req.NetworkRefNo+"-vd")
	form.Set("txnTime", time.Now().Format("20060102150405"))
	form.Set("origQryId", req.NetworkRefNo)
	form.Set("certId", a.cfg.CertID)
	if err := a.signForm(form); err != nil {
		return nil, err
	}
	resp, err := a.hc.PostForm(ctx, "/gateway/api/backTransReq.do", nil, form)
	if err != nil {
		return nil, fmt.Errorf("unionpay void: %w", err)
	}
	rb, _ := parseFormResponse(resp.Body)
	if rb.Get("respCode") == "00" {
		return &processor.NetworkVoidResponse{Status: "voided"}, nil
	}
	return &processor.NetworkVoidResponse{Status: "failed"}, nil
}

func (a *Adapter) Query(ctx context.Context, req *processor.NetworkQueryRequest) (*processor.NetworkQueryResponse, error) {
	if a.mock {
		return &processor.NetworkQueryResponse{NetworkRefNo: req.NetworkRefNo, Status: "approved"}, nil
	}
	form := a.baseForm("00", "00")
	form.Set("orderId", req.NetworkRefNo+"-q")
	form.Set("txnTime", time.Now().Format("20060102150405"))
	form.Set("origQryId", req.NetworkRefNo)
	form.Set("certId", a.cfg.CertID)
	if err := a.signForm(form); err != nil {
		return nil, err
	}
	resp, err := a.hc.PostForm(ctx, "/gateway/api/queryTrans.do", nil, form)
	if err != nil {
		return nil, fmt.Errorf("unionpay query: %w", err)
	}
	rb, _ := parseFormResponse(resp.Body)
	out := &processor.NetworkQueryResponse{NetworkRefNo: rb.Get("queryId")}
	amt, _ := strconv.ParseInt(rb.Get("txnAmount"), 10, 64)
	out.Amount = amt
	out.Currency = isoNumToCurrency(rb.Get("currencyCode"))
	switch rb.Get("respCode") {
	case "00":
		out.Status = "approved"
	case "03", "04", "05":
		out.Status = "pending"
	default:
		out.Status = "declined"
		out.DeclineCode = rb.Get("respCode")
	}
	return out, nil
}

// ─── helpers ────────────────────────────────────────────────────────────

// baseForm 公共字段。txnType / txnSubType 由各动词决定。
func (a *Adapter) baseForm(txnType, txnSubType string) url.Values {
	form := url.Values{}
	form.Set("version", upVersion)
	form.Set("encoding", upEncoding)
	form.Set("signMethod", upSignMethod)
	form.Set("bizType", upBizType)
	form.Set("accessType", upAccessType)
	form.Set("txnType", txnType)
	form.Set("txnSubType", txnSubType)
	form.Set("merId", a.cfg.MerchantID)
	form.Set("backUrl", "") // 异步通知地址：dev 不用；prod 由 caller 传
	form.Set("channelType", "07") // 07 = 互联网
	return form
}

// signForm 银联签名规则：
//   1. 取 form 全部非空 / 非 signature 字段
//   2. 按 key 升序拼 "k=v&k=v"
//   3. SHA256(message) → bytes → RSA-SHA256 sign → base64
//   4. 写入 signature
func (a *Adapter) signForm(form url.Values) error {
	if a.privateKey == nil {
		return errors.New("unionpay: private_key not loaded")
	}
	canonical := httpx.SortedFormString(form, "signature")
	sig, err := httpx.RSASignSHA256(a.privateKey, []byte(canonical))
	if err != nil {
		return err
	}
	form.Set("signature", sig)
	return nil
}

// parseFormResponse 银联响应也是 form-encoded（不是 JSON），用 url.ParseQuery 解。
func parseFormResponse(body []byte) (url.Values, error) {
	return url.ParseQuery(string(body))
}

// mapUPDeclineCode 银联响应码归类（节选）：
//   00 = 成功；03/04/05 = 处理中；其它 = 失败
//   12/13/14/57 = 卡相关错误（HARD 拒）
//   51 = 余额不足（SOFT）
//   05 = 不予承兑（HARD 风控）
//   34 = 涉嫌欺诈（HARD）
func mapUPDeclineCode(code string) string {
	switch code {
	case "12", "13", "14", "34", "37", "41", "43", "57", "62":
		return "HARD"
	case "51", "61", "65", "75":
		return "SOFT"
	}
	return "SOFT"
}

func currencyToISONum(c string) string {
	switch strings.ToUpper(c) {
	case "CNY":
		return "156"
	case "USD":
		return "840"
	case "EUR":
		return "978"
	case "JPY":
		return "392"
	case "HKD":
		return "344"
	case "GBP":
		return "826"
	}
	return ""
}

func isoNumToCurrency(n string) string {
	switch n {
	case "156":
		return "CNY"
	case "840":
		return "USD"
	case "978":
		return "EUR"
	case "392":
		return "JPY"
	case "344":
		return "HKD"
	case "826":
		return "GBP"
	}
	return ""
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

// mockAuthorize:
//   62 (银联 BIN): approved
//   6225:           SOFT decline (51 - 余额不足)
//   6226:           HARD decline (34 - 涉嫌欺诈)
func mockAuthorize(req *processor.NetworkAuthRequest) *processor.NetworkAuthResponse {
	bin := req.PAN
	if len(bin) > 4 {
		bin = bin[:4]
	}
	out := &processor.NetworkAuthResponse{
		NetworkRefNo: "upmock_" + req.IdempotencyKey,
		ARN:          "ARN_" + req.IdempotencyKey,
		MaskedPAN:    maskPAN(req.PAN),
		Network:      "unionpay",
	}
	switch bin {
	case "6225":
		out.Status = "declined"
		out.DeclineCode = "51"
		out.DeclineReason = "余额不足"
		out.DeclineCategory = "SOFT"
	case "6226":
		out.Status = "declined"
		out.DeclineCode = "34"
		out.DeclineReason = "涉嫌欺诈"
		out.DeclineCategory = "HARD"
		out.FraudScore = 95
	default:
		out.Status = "approved"
		out.AVSResult = "U"
		out.CVVResult = "U"
		out.FraudScore = 20
		if strings.HasPrefix(bin, "62") {
			out.AVSResult = "Y"
			out.CVVResult = "M"
			out.FraudScore = 8
		}
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
