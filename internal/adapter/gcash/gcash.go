// Package gcash 实现 GCash Partner API adapter（走 Alipay+ mPaaS 骨干）。
//
// 参考：https://miniprogram.gcash.com/docs/miniprogram_gcash/mpdev/v1_pay
// 也可经 Adyen / Checkout.com / 2C2P / EBANX 这类 PSP 走统一 API —— 本包以
// 直连 Alipay+ 网关为蓝本，切 PSP 时只换 base URL 和鉴权 header。
//
// 鉴权：RSA-SHA256 签名 + partnerId。
//   Request-Time / Signature 两个 header。
package gcash

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
)

const (
	baseSandbox = "https://open-gw-pre.mpaas.cn-hangzhou.aliyuncs.com"
	baseProd    = "https://open-gw.mpaas.cn-hangzhou.aliyuncs.com"

	pathPay     = "/v1/payments/pay"
	pathRefund  = "/v1/payments/refund"
	pathInquiry = "/v1/payments/inquiryPayment"

	productCodePH = "51051000101000100000" // GCash Wallet（Alipay+ CONNECT_WALLET）

	// MetaFlow 决定本次 Charge 走哪种 GCash 接入方式：
	//   ""              ── 默认 H5 / app redirect（CONNECT_WALLET）
	//   "miniprogram"   ── GCash 小程序 JSAPI：Charge 返回 paymentId，
	//                       小程序内通过 my.tradePay({paymentId}) 唤起支付
	// 通过 PaymentRequest.Metadata["gcash_flow"] 传入。
	MetaFlow            = "gcash_flow"
	FlowMiniProgram     = "miniprogram"
	paymentMethodMiniApp = "MINI_APP" // GCash Partner API 小程序 JSAPI 渠道码

	// ActionTypeMiniProgramInvoke 在 RequiredAction.Type 上的字符串。
	// payment-core 与 order-core 也用同一字符串识别。
	ActionTypeMiniProgramInvoke = "mini_program_invoke"
)

type Config struct {
	Env          string
	PartnerID    string
	MerchantPriv string // PEM 格式 RSA 私钥（商户）
	GCashPubKey  string // PEM 格式 RSA 公钥（验签回调）
	NotifyURL    string
	BaseURL      string // 非空时覆盖 sandbox/prod，用于 mockserver 或第三方 PSP 代理
}

type Adapter struct {
	cfg  Config
	h    *http.Client
	priv *rsa.PrivateKey
	pub  *rsa.PublicKey
}

func New(cfg Config) (*Adapter, error) {
	a := &Adapter{cfg: cfg, h: channel.NewHTTPClient(15 * time.Second)}
	if cfg.MerchantPriv != "" {
		priv, err := parsePrivKey(cfg.MerchantPriv)
		if err != nil {
			return nil, fmt.Errorf("gcash: parse priv: %w", err)
		}
		a.priv = priv
	}
	if cfg.GCashPubKey != "" {
		pub, err := parsePubKey(cfg.GCashPubKey)
		if err != nil {
			return nil, fmt.Errorf("gcash: parse pub: %w", err)
		}
		a.pub = pub
	}
	return a, nil
}

func (a *Adapter) Name() string { return "gcash" }

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

type payRequest struct {
	PartnerID          string `json:"partnerId"`
	PaymentRequestID   string `json:"paymentRequestId"`
	PaymentAmount      amount `json:"paymentAmount"`
	PaymentMethod      method `json:"paymentMethod"`
	PaymentFactor      factor `json:"paymentFactor"`
	ProductCode        string `json:"productCode"`
	PaymentRedirectURL string `json:"paymentRedirectUrl"`
	PaymentNotifyURL   string `json:"paymentNotifyUrl"`
	PaymentExpiryTime  string `json:"paymentExpiryTime"`
	Order              *order `json:"order,omitempty"`
}

type amount struct {
	Currency string `json:"currency"`
	Value    string `json:"value"`
}

type method struct {
	PaymentMethodType string `json:"paymentMethodType"`
}

type factor struct {
	IsAuthorization bool `json:"isAuthorization"`
}

type order struct {
	ReferenceOrderID string `json:"referenceOrderId"`
	OrderDescription string `json:"orderDescription"`
}

type payResponse struct {
	Result struct {
		ResultCode    string `json:"resultCode"`
		ResultStatus  string `json:"resultStatus"` // S/F/U
		ResultMessage string `json:"resultMessage"`
	} `json:"result"`
	PaymentRequestID string `json:"paymentRequestId"`
	PaymentID        string `json:"paymentId"`
	PaymentTime      string `json:"paymentTime"`
	NormalURL        string `json:"normalUrl"`
	SchemeURL        string `json:"schemeUrl"`
	ApplinkURL       string `json:"applinkUrl"`
}

func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	// 接入方式：默认 CONNECT_WALLET（H5/app redirect），通过 metadata
	// gcash_flow=miniprogram 切换到小程序 JSAPI 模式（MINI_APP）。
	// 两种模式 GCash 后端用不同的 paymentMethodType 触发不同的下游处理；
	// MINI_APP 返回的 paymentId 直接给到小程序前端调用 my.tradePay，
	// 不再生成 H5 跳转 URL。
	flow := ""
	if req.Metadata != nil {
		flow = req.Metadata[MetaFlow]
	}
	pmType := "CONNECT_WALLET"
	if flow == FlowMiniProgram {
		pmType = paymentMethodMiniApp
	}

	body := payRequest{
		PartnerID:          a.cfg.PartnerID,
		PaymentRequestID:   req.IdempotencyKey,
		PaymentAmount:      amount{Currency: req.Currency, Value: fmt.Sprintf("%d", req.Amount)},
		PaymentMethod:      method{PaymentMethodType: pmType},
		PaymentFactor:      factor{IsAuthorization: !req.CaptureImmediate},
		ProductCode:        productCodePH,
		PaymentRedirectURL: req.ReturnURL,
		PaymentNotifyURL:   a.cfg.NotifyURL,
		PaymentExpiryTime:  time.Now().Add(15 * time.Minute).UTC().Format("2006-01-02T15:04:05Z"),
		Order:              &order{ReferenceOrderID: req.PiID, OrderDescription: req.Description},
	}
	var resp payResponse
	if err := a.doSigned(ctx, pathPay, body, &resp); err != nil {
		return nil, err
	}
	switch resp.Result.ResultStatus {
	case "S":
		// 成功无需用户动作（极少数小额免密场景）
		return &channel.ChargeResponse{
			Result:        channel.ResultSucceeded,
			ExternalRefNo: resp.PaymentID,
		}, nil
	case "U":
		// 小程序模式：把 paymentId / partnerId / signMethod 装进 Extra，前端用
		// my.tradePay({paymentId}) 唤起支付。RedirectURL 留空——小程序无跳转语义。
		if flow == FlowMiniProgram {
			return &channel.ChargeResponse{
				Result:        channel.ResultRequiresAction,
				ExternalRefNo: resp.PaymentID,
				RequiredAction: &channel.RequiredAction{
					Type:      ActionTypeMiniProgramInvoke,
					ExpiresAt: time.Now().Add(15 * time.Minute),
					Extra: map[string]string{
						"payment_id":  resp.PaymentID,
						"partner_id":  a.cfg.PartnerID,
						"sign_method": "RSA256",
						// 小程序 JS 需要的最小集合：
						//   my.tradePay({ paymentId: <Extra.payment_id> }, cb)
						// 商户在 GCash 小程序后台已绑定 partnerId，前端无需用它做签名；
						// 暴露出来仅用于审计 / 多 partner 调试。
					},
				},
			}, nil
		}
		// 默认 H5 / app redirect 模式
		return &channel.ChargeResponse{
			Result:        channel.ResultRequiresAction,
			ExternalRefNo: resp.PaymentID,
			RequiredAction: &channel.RequiredAction{
				Type:        "app_redirect",
				RedirectURL: firstNonEmpty(resp.SchemeURL, resp.ApplinkURL, resp.NormalURL),
				Scheme:      "universal",
				ReturnURL:   req.ReturnURL,
				ExpiresAt:   time.Now().Add(15 * time.Minute),
			},
		}, nil
	default: // "F"
		return &channel.ChargeResponse{
			Result:         channel.ResultFailed,
			FailureCode:    channel.MapFailure("gcash", resp.Result.ResultCode),
			RawFailureCode: resp.Result.ResultCode,
			FailureMessage: resp.Result.ResultMessage,
		}, nil
	}
}

// Capture / Void: GCash Partner API 的授权-捕获一般单独 API；示意保留。
func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}
func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

// ---- Refund ---------------------------------------------------------------

type refundRequest struct {
	PartnerID       string `json:"partnerId"`
	PaymentID       string `json:"paymentId"`
	RefundRequestID string `json:"refundRequestId"`
	RefundAmount    amount `json:"refundAmount"`
	RefundReason    string `json:"refundReason,omitempty"`
}

type refundResponse struct {
	Result struct {
		ResultCode   string `json:"resultCode"`
		ResultStatus string `json:"resultStatus"`
	} `json:"result"`
	RefundID string `json:"refundId"`
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	body := refundRequest{
		PartnerID:       a.cfg.PartnerID,
		PaymentID:       req.ExternalRefNo,
		RefundRequestID: req.IdempotencyKey,
		RefundAmount:    amount{Currency: "PHP", Value: fmt.Sprintf("%d", req.Amount)},
		RefundReason:    req.Reason,
	}
	var resp refundResponse
	if err := a.doSigned(ctx, pathRefund, body, &resp); err != nil {
		return nil, err
	}
	if resp.Result.ResultStatus == "S" {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: resp.RefundID}, nil
	}
	return &channel.OpResponse{
		Result:         channel.ResultFailed,
		FailureCode:    channel.MapFailure("gcash", resp.Result.ResultCode),
		RawFailureCode: resp.Result.ResultCode,
	}, nil
}

// ---- Query ----------------------------------------------------------------

type inquiryRequest struct {
	PartnerID        string `json:"partnerId"`
	PaymentRequestID string `json:"paymentRequestId"`
}

type inquiryResponse struct {
	Result struct {
		ResultCode   string `json:"resultCode"`
		ResultStatus string `json:"resultStatus"`
	} `json:"result"`
	PaymentID     string `json:"paymentId"`
	PaymentStatus string `json:"paymentStatus"` // SUCCESS / PROCESSING / FAIL
	PaymentAmount amount `json:"paymentAmount"`
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	body := inquiryRequest{PartnerID: a.cfg.PartnerID, PaymentRequestID: req.ExternalRefNo}
	var resp inquiryResponse
	if err := a.doSigned(ctx, pathInquiry, body, &resp); err != nil {
		return nil, err
	}
	var rt channel.ResultType
	switch resp.PaymentStatus {
	case "SUCCESS":
		rt = channel.ResultSucceeded
	case "FAIL":
		rt = channel.ResultFailed
	default:
		rt = channel.ResultProcessing
	}
	return &channel.QueryResponse{Result: rt, ExternalRefNo: resp.PaymentID}, nil
}

// ---- Webhook --------------------------------------------------------------

type notifyBody struct {
	PaymentRequestID string `json:"paymentRequestId"`
	PaymentID        string `json:"paymentId"`
	PaymentStatus    string `json:"paymentStatus"`
	PaymentAmount    amount `json:"paymentAmount"`
	PaymentTime      string `json:"paymentTime"`
}

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	signB64 := headers["Signature"]
	sigOK := a.verifySign(body, signB64)
	var nb notifyBody
	if err := json.Unmarshal(body, &nb); err != nil {
		return nil, err
	}
	evt := &channel.WebhookEvent{
		EventID:       nb.PaymentRequestID,
		PiID:          nb.PaymentRequestID, // 由上层 ref → pi_id 映射
		ExternalRefNo: nb.PaymentID,
		Timestamp:     parseGCashTime(nb.PaymentTime),
		SignatureOK:   sigOK,
		Raw:           map[string]string{"paymentStatus": nb.PaymentStatus},
	}
	switch nb.PaymentStatus {
	case "SUCCESS":
		evt.EventType = "charge.succeeded"
	case "FAIL", "FAILED":
		evt.EventType = "charge.failed"
	default:
		evt.EventType = "payment_intent.requires_action"
	}
	return evt, nil
}

// ---- signing --------------------------------------------------------------

func (a *Adapter) doSigned(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	canonical := fmt.Sprintf("POST %s\n%s.%s.%s", path, a.cfg.PartnerID, now, string(b))
	sign, err := a.sign([]byte(canonical))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Request-Time", now)
	req.Header.Set("client-id", a.cfg.PartnerID)
	req.Header.Set("Signature", fmt.Sprintf("algorithm=RSA256,keyVersion=1,signature=%s", sign))
	resp, err := a.h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func (a *Adapter) sign(buf []byte) (string, error) {
	if a.priv == nil {
		return "", fmt.Errorf("gcash: merchant priv not configured")
	}
	sum := sha256.Sum256(buf)
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

func (a *Adapter) verifySign(buf []byte, b64 string) bool {
	if a.pub == nil || b64 == "" {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(buf)
	return rsa.VerifyPKCS1v15(a.pub, crypto.SHA256, sum[:], sig) == nil
}

func parsePrivKey(pemStr string) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode([]byte(pemStr))
	if blk == nil {
		return nil, fmt.Errorf("invalid pem")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	pk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not rsa")
	}
	return pk, nil
}

func parsePubKey(pemStr string) (*rsa.PublicKey, error) {
	blk, _ := pem.Decode([]byte(pemStr))
	if blk == nil {
		return nil, fmt.Errorf("invalid pem")
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	pk, ok := k.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not rsa")
	}
	return pk, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func parseGCashTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}
