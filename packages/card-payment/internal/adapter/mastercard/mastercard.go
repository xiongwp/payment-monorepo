// Package mastercard 实现 processor.Network，调 Mastercard MIP / MIP-CE。
//
// 生产对接要点（TODO）：
//   - mTLS（Mastercard MAS 证书 + 项目 cert，跟 Visa MAS 不互通）
//   - OAuth body signing (consumer key + private key)
//   - MIP REST endpoint：authorization / capture / refund / reversal
//   - 5xx / 网络错走重试，4xx decline 直接返
package mastercard

import (
	"context"
	"errors"
	"strings"
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

func (a *Adapter) Name() string { return "mastercard" }

func (a *Adapter) Authorize(ctx context.Context, req *processor.NetworkAuthRequest) (*processor.NetworkAuthResponse, error) {
	if req == nil || req.PAN == "" {
		return nil, errors.New("mastercard: pan required")
	}
	if a.mock {
		// dev mock：BIN 5555 approved，5111 declined，其它 approved
		bin := req.PAN[:4]
		switch {
		case strings.HasPrefix(bin, "5111"):
			return &processor.NetworkAuthResponse{
				NetworkRefNo:  "mcmock_" + req.IdempotencyKey,
				Status:        "declined",
				DeclineCode:   "DO_NOT_HONOR",
				DeclineReason: "mock decline for BIN 5111",
				MaskedPAN:     maskPAN(req.PAN),
				Network:       "mastercard",
			}, nil
		}
		return &processor.NetworkAuthResponse{
			NetworkRefNo: "mcmock_" + req.IdempotencyKey,
			Status:       "approved",
			MaskedPAN:    maskPAN(req.PAN),
			Network:      "mastercard",
			ARN:          "ARN_MC_" + req.IdempotencyKey,
		}, nil
	}
	a.logger.Warn("mastercard adapter: production endpoint not implemented")
	return nil, errors.New("mastercard: production endpoint TODO")
}

func (a *Adapter) Capture(ctx context.Context, req *processor.NetworkCaptureRequest) (*processor.NetworkCaptureResponse, error) {
	if a.mock {
		return &processor.NetworkCaptureResponse{
			NetworkRefNo:   req.NetworkRefNo,
			Status:         "captured",
			CapturedAmount: req.Amount,
		}, nil
	}
	return nil, errors.New("mastercard: capture TODO")
}

func (a *Adapter) Refund(ctx context.Context, req *processor.NetworkRefundRequest) (*processor.NetworkRefundResponse, error) {
	if a.mock {
		return &processor.NetworkRefundResponse{
			RefundRefNo:    "mcrf_" + req.NetworkRefNo,
			Status:         "refunded",
			RefundedAmount: req.Amount,
		}, nil
	}
	return nil, errors.New("mastercard: refund TODO")
}

func (a *Adapter) Void(ctx context.Context, req *processor.NetworkVoidRequest) (*processor.NetworkVoidResponse, error) {
	if a.mock {
		return &processor.NetworkVoidResponse{Status: "voided"}, nil
	}
	return nil, errors.New("mastercard: void TODO")
}

func (a *Adapter) Query(ctx context.Context, req *processor.NetworkQueryRequest) (*processor.NetworkQueryResponse, error) {
	if a.mock {
		return &processor.NetworkQueryResponse{NetworkRefNo: req.NetworkRefNo, Status: "approved"}, nil
	}
	return nil, errors.New("mastercard: query TODO")
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
