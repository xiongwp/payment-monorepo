// Package visa 实现 processor.Network 接口，调 Visa Net API。
//
// 当前是 mock 实现：dev 路径返回固定 approved；生产应按 Visa Net 文档对接：
//   - mTLS（Visa MAS 证书）
//   - JWT signing for VDP
//   - ISO 8583 / VisaNet API endpoints
package visa

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/card-payment/internal/processor"
)

// Adapter Visa Net API 适配器
type Adapter struct {
	endpoint string
	apiKey   string
	cert     string // mTLS client cert path
	timeout  time.Duration
	logger   *zap.Logger
	mock     bool
}

// Config visa adapter 配置
type Config struct {
	Endpoint string
	APIKey   string
	Cert     string
	Timeout  time.Duration
	Mock     bool // dev 用 mock=true
}

// New 构造
func New(cfg Config, logger *zap.Logger) *Adapter {
	t := cfg.Timeout
	if t <= 0 {
		t = 30 * time.Second
	}
	return &Adapter{
		endpoint: cfg.Endpoint,
		apiKey:   cfg.APIKey,
		cert:     cfg.Cert,
		timeout:  t,
		logger:   logger,
		mock:     cfg.Mock || cfg.Endpoint == "",
	}
}

// Name 实现
func (a *Adapter) Name() string { return "visa" }

// Authorize 调 Visa Net Authorize API
//
// 注意：req.PAN 仅在本函数内使用，不能 log，不能传给除 HTTP body 之外的任何地方。
func (a *Adapter) Authorize(ctx context.Context, req *processor.NetworkAuthRequest) (*processor.NetworkAuthResponse, error) {
	if req == nil || req.PAN == "" {
		return nil, errors.New("visa: pan required")
	}

	if a.mock {
		// dev mock：BIN 4242 永远 approved，BIN 4000 永远 declined
		bin := req.PAN[:4]
		switch {
		case bin == "4242":
			return &processor.NetworkAuthResponse{
				NetworkRefNo: "vmock_" + req.IdempotencyKey,
				Status:       "approved",
				MaskedPAN:    maskPAN(req.PAN),
				Network:      "visa",
				ARN:          "ARN" + req.IdempotencyKey,
			}, nil
		case strings.HasPrefix(bin, "4000"):
			return &processor.NetworkAuthResponse{
				NetworkRefNo:  "vmock_" + req.IdempotencyKey,
				Status:        "declined",
				DeclineCode:   "INSUFFICIENT_FUNDS",
				DeclineReason: "mock decline for BIN starting 4000",
				MaskedPAN:     maskPAN(req.PAN),
				Network:       "visa",
			}, nil
		}
		return &processor.NetworkAuthResponse{
			NetworkRefNo: "vmock_" + req.IdempotencyKey,
			Status:       "approved",
			MaskedPAN:    maskPAN(req.PAN),
			Network:      "visa",
		}, nil
	}

	// 生产路径：调 Visa Net API
	// 实现：
	//   1. 构造 ISO 8583 0100 message 或 VDP REST request
	//   2. 用 mTLS client cert + apiKey signing
	//   3. POST 到 endpoint
	//   4. 解析响应 → NetworkAuthResponse
	//
	// 这里留 TODO，生产对接时 fill in。
	a.logger.Warn("visa adapter: production endpoint not implemented")
	return nil, errors.New("visa: production endpoint TODO")
}

// Capture / Refund / Void / Query 类似模板，先 mock 返回。
func (a *Adapter) Capture(ctx context.Context, req *processor.NetworkCaptureRequest) (*processor.NetworkCaptureResponse, error) {
	if a.mock {
		return &processor.NetworkCaptureResponse{
			NetworkRefNo:   req.NetworkRefNo,
			Status:         "captured",
			CapturedAmount: req.Amount,
		}, nil
	}
	return nil, errors.New("visa: capture TODO")
}

func (a *Adapter) Refund(ctx context.Context, req *processor.NetworkRefundRequest) (*processor.NetworkRefundResponse, error) {
	if a.mock {
		return &processor.NetworkRefundResponse{
			RefundRefNo:    "vrf_" + req.NetworkRefNo,
			Status:         "refunded",
			RefundedAmount: req.Amount,
		}, nil
	}
	return nil, errors.New("visa: refund TODO")
}

func (a *Adapter) Void(ctx context.Context, req *processor.NetworkVoidRequest) (*processor.NetworkVoidResponse, error) {
	if a.mock {
		return &processor.NetworkVoidResponse{Status: "voided"}, nil
	}
	return nil, errors.New("visa: void TODO")
}

func (a *Adapter) Query(ctx context.Context, req *processor.NetworkQueryRequest) (*processor.NetworkQueryResponse, error) {
	if a.mock {
		return &processor.NetworkQueryResponse{
			NetworkRefNo: req.NetworkRefNo,
			Status:       "approved",
		}, nil
	}
	return nil, errors.New("visa: query TODO")
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
