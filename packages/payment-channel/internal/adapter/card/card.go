// Package card 卡支付 channel adapter — 临时 STUB.
//
// 原版通过 Kitex client 调跨服务 card-payment, 但 cross-service kitex_gen
// 还没在 docker build 流程里 wire 进去 (需要 additional_contexts + replace).
// 暂改 stub: 所有方法返回 mock success, 等 sibling sourcing 接通后恢复.
package card

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/payment-channel/internal/channel"
)

// Config card adapter 配置
type Config struct {
	CardPaymentEndpoint string
	RPCTimeout          time.Duration
}

// Adapter 实现 channel.Adapter (mock 模式, 永远返成功).
type Adapter struct {
	logger  *zap.Logger
	timeout time.Duration
}

// New 构造 mock adapter.
func New(cfg Config, logger *zap.Logger) (*Adapter, error) {
	if cfg.RPCTimeout == 0 {
		cfg.RPCTimeout = 30 * time.Second
	}
	if logger != nil {
		logger.Warn("card adapter STUB — card-payment Kitex client wiring pending (sibling sourcing not in build context)",
			zap.String("endpoint", cfg.CardPaymentEndpoint))
	}
	return &Adapter{logger: logger, timeout: cfg.RPCTimeout}, nil
}

func (a *Adapter) Name() string { return "card" }

func (a *Adapter) Charge(_ context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	return &channel.ChargeResponse{Result: channel.ResultSucceeded, ExternalRefNo: "vmock_" + req.IdempotencyKey}, nil
}

func (a *Adapter) Capture(_ context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

func (a *Adapter) Void(_ context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

func (a *Adapter) Refund(_ context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: "vrf_" + req.ExternalRefNo}, nil
}

func (a *Adapter) Query(_ context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	return &channel.QueryResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	_ = headers
	_ = body
	return nil, errors.New("card adapter STUB: webhook flow disabled")
}

func (a *Adapter) Close() error { return nil }
