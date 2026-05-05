// Package jcb 实现 processor.Network，调 JCB 卡组织 API。
//
// 生产对接要点（TODO）：
//   - JCB 网络（J/Smart）通过收单行 BIN sponsor，多数情况下走 acquirer 接口
//     而非 JCB 直连
//   - HMAC + timestamp 签名（不是 OAuth）
//   - 日元结算注意金额 unit 是整数（无小数）
package jcb

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

func (a *Adapter) Name() string { return "jcb" }

func (a *Adapter) Authorize(ctx context.Context, req *processor.NetworkAuthRequest) (*processor.NetworkAuthResponse, error) {
	if req == nil || req.PAN == "" {
		return nil, errors.New("jcb: pan required")
	}
	if a.mock {
		return &processor.NetworkAuthResponse{
			NetworkRefNo: "jcbmock_" + req.IdempotencyKey,
			Status:       "approved",
			MaskedPAN:    maskPAN(req.PAN),
			Network:      "jcb",
			ARN:          "ARN_JCB_" + req.IdempotencyKey,
		}, nil
	}
	a.logger.Warn("jcb adapter: production endpoint not implemented")
	return nil, errors.New("jcb: production endpoint TODO")
}

func (a *Adapter) Capture(ctx context.Context, req *processor.NetworkCaptureRequest) (*processor.NetworkCaptureResponse, error) {
	if a.mock {
		return &processor.NetworkCaptureResponse{
			NetworkRefNo:   req.NetworkRefNo,
			Status:         "captured",
			CapturedAmount: req.Amount,
		}, nil
	}
	return nil, errors.New("jcb: capture TODO")
}

func (a *Adapter) Refund(ctx context.Context, req *processor.NetworkRefundRequest) (*processor.NetworkRefundResponse, error) {
	if a.mock {
		return &processor.NetworkRefundResponse{
			RefundRefNo:    "jcbrf_" + req.NetworkRefNo,
			Status:         "refunded",
			RefundedAmount: req.Amount,
		}, nil
	}
	return nil, errors.New("jcb: refund TODO")
}

func (a *Adapter) Void(ctx context.Context, req *processor.NetworkVoidRequest) (*processor.NetworkVoidResponse, error) {
	if a.mock {
		return &processor.NetworkVoidResponse{Status: "voided"}, nil
	}
	return nil, errors.New("jcb: void TODO")
}

func (a *Adapter) Query(ctx context.Context, req *processor.NetworkQueryRequest) (*processor.NetworkQueryResponse, error) {
	if a.mock {
		return &processor.NetworkQueryResponse{NetworkRefNo: req.NetworkRefNo, Status: "approved"}, nil
	}
	return nil, errors.New("jcb: query TODO")
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
