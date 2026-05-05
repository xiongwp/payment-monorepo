// Package amex 实现 processor.Network，调 American Express OptBlue / Direct API。
//
// 生产对接要点（TODO）：
//   - AMEX 直连（OptBlue）需要 BIN sponsorship + ARC（Acquirer Reference Code）
//   - 通常 PSP 走 OptBlue 拉 AMEX 流量更现实，跟 acquirer 一起做
//   - 注意：PAN 可能 15 位（其它卡组织通常 16 位），mask 逻辑要兼容
//   - SafeKey 3DS 是 AMEX 自家方案
package amex

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

func (a *Adapter) Name() string { return "amex" }

func (a *Adapter) Authorize(ctx context.Context, req *processor.NetworkAuthRequest) (*processor.NetworkAuthResponse, error) {
	if req == nil || req.PAN == "" {
		return nil, errors.New("amex: pan required")
	}
	if a.mock {
		return &processor.NetworkAuthResponse{
			NetworkRefNo: "amexmock_" + req.IdempotencyKey,
			Status:       "approved",
			MaskedPAN:    maskPAN(req.PAN),
			Network:      "amex",
			ARN:          "ARN_AMEX_" + req.IdempotencyKey,
		}, nil
	}
	a.logger.Warn("amex adapter: production endpoint not implemented")
	return nil, errors.New("amex: production endpoint TODO")
}

func (a *Adapter) Capture(ctx context.Context, req *processor.NetworkCaptureRequest) (*processor.NetworkCaptureResponse, error) {
	if a.mock {
		return &processor.NetworkCaptureResponse{
			NetworkRefNo:   req.NetworkRefNo,
			Status:         "captured",
			CapturedAmount: req.Amount,
		}, nil
	}
	return nil, errors.New("amex: capture TODO")
}

func (a *Adapter) Refund(ctx context.Context, req *processor.NetworkRefundRequest) (*processor.NetworkRefundResponse, error) {
	if a.mock {
		return &processor.NetworkRefundResponse{
			RefundRefNo:    "amexrf_" + req.NetworkRefNo,
			Status:         "refunded",
			RefundedAmount: req.Amount,
		}, nil
	}
	return nil, errors.New("amex: refund TODO")
}

func (a *Adapter) Void(ctx context.Context, req *processor.NetworkVoidRequest) (*processor.NetworkVoidResponse, error) {
	if a.mock {
		return &processor.NetworkVoidResponse{Status: "voided"}, nil
	}
	return nil, errors.New("amex: void TODO")
}

func (a *Adapter) Query(ctx context.Context, req *processor.NetworkQueryRequest) (*processor.NetworkQueryResponse, error) {
	if a.mock {
		return &processor.NetworkQueryResponse{NetworkRefNo: req.NetworkRefNo, Status: "approved"}, nil
	}
	return nil, errors.New("amex: query TODO")
}

// maskPAN AMEX 兼容 15 位（其它 16 位）
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
