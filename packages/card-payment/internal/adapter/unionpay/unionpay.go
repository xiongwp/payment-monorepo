// Package unionpay 实现 processor.Network，调中国银联（CUP）UnionPay International API.
//
// 生产对接要点（TODO）：
//   - 银联国际接口走签名（RSA SHA256 + 商户证书）
//   - 跨境收单需要银联 BIN sponsor
//   - 国际卡 ¥+USD 结算 vs 国内 ¥ 结算，路由不同
//   - 支持云闪付 NFC、二维码扫码、UnionPay Online Payment（UPOP）多种渠道
package unionpay

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/card-payment/internal/processor"
)

type Adapter struct {
	endpoint string
	apiKey   string
	timeout  time.Duration
	logger   *zap.Logger
	mock     bool
}

type Config struct {
	Endpoint string
	APIKey   string
	Cert     string
	Timeout  time.Duration
	Mock     bool
}

func New(cfg Config, logger *zap.Logger) *Adapter {
	t := cfg.Timeout
	if t <= 0 {
		t = 30 * time.Second
	}
	return &Adapter{
		endpoint: cfg.Endpoint,
		apiKey:   cfg.APIKey,
		timeout:  t,
		logger:   logger,
		mock:     cfg.Mock || cfg.Endpoint == "",
	}
}

func (a *Adapter) Name() string { return "unionpay" }

func (a *Adapter) Authorize(ctx context.Context, req *processor.NetworkAuthRequest) (*processor.NetworkAuthResponse, error) {
	if req == nil || req.PAN == "" {
		return nil, errors.New("unionpay: pan required")
	}
	if a.mock {
		return &processor.NetworkAuthResponse{
			NetworkRefNo: "upmock_" + req.IdempotencyKey,
			Status:       "approved",
			MaskedPAN:    maskPAN(req.PAN),
			Network:      "unionpay",
			ARN:          "ARN_UP_" + req.IdempotencyKey,
		}, nil
	}
	a.logger.Warn("unionpay adapter: production endpoint not implemented")
	return nil, errors.New("unionpay: production endpoint TODO")
}

func (a *Adapter) Capture(ctx context.Context, req *processor.NetworkCaptureRequest) (*processor.NetworkCaptureResponse, error) {
	if a.mock {
		return &processor.NetworkCaptureResponse{
			NetworkRefNo:   req.NetworkRefNo,
			Status:         "captured",
			CapturedAmount: req.Amount,
		}, nil
	}
	return nil, errors.New("unionpay: capture TODO")
}

func (a *Adapter) Refund(ctx context.Context, req *processor.NetworkRefundRequest) (*processor.NetworkRefundResponse, error) {
	if a.mock {
		return &processor.NetworkRefundResponse{
			RefundRefNo:    "uprf_" + req.NetworkRefNo,
			Status:         "refunded",
			RefundedAmount: req.Amount,
		}, nil
	}
	return nil, errors.New("unionpay: refund TODO")
}

func (a *Adapter) Void(ctx context.Context, req *processor.NetworkVoidRequest) (*processor.NetworkVoidResponse, error) {
	if a.mock {
		return &processor.NetworkVoidResponse{Status: "voided"}, nil
	}
	return nil, errors.New("unionpay: void TODO")
}

func (a *Adapter) Query(ctx context.Context, req *processor.NetworkQueryRequest) (*processor.NetworkQueryResponse, error) {
	if a.mock {
		return &processor.NetworkQueryResponse{NetworkRefNo: req.NetworkRefNo, Status: "approved"}, nil
	}
	return nil, errors.New("unionpay: query TODO")
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
