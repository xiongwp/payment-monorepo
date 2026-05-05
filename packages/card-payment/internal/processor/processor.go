// Package processor 实现 card-payment 的核心交易流程：
//
//   payment_token → cardCenter.Detokenize → PAN → network.Auth → 网络结果
//
// **关键纪律**：
//   - PAN 仅在本包函数局部变量内出现
//   - 函数返回前必须 zero out PAN 局部变量（Go 没有手动 zero memory，但这里
//     主动覆盖 + 不传给除 network adapter 外的任何地方）
//   - 任何 logger 调用都要明确字段，禁用 zap.Any(req)
//   - DB 持久化只存 masked_pan + network_ref_no + amount + status
package processor

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Network 卡组织 adapter 接口（visa / mastercard / jcb / amex / unionpay 各一个实现）
type Network interface {
	Name() string
	Authorize(ctx context.Context, req *NetworkAuthRequest) (*NetworkAuthResponse, error)
	Capture(ctx context.Context, req *NetworkCaptureRequest) (*NetworkCaptureResponse, error)
	Refund(ctx context.Context, req *NetworkRefundRequest) (*NetworkRefundResponse, error)
	Void(ctx context.Context, req *NetworkVoidRequest) (*NetworkVoidResponse, error)
	Query(ctx context.Context, req *NetworkQueryRequest) (*NetworkQueryResponse, error)
}

// NetworkAuthRequest 给 network adapter 的 input。**唯一**含 PAN 的结构体。
//
// adapter 实现必须：
//   1. 直接发 HTTPS request 给卡组织 endpoint
//   2. 不要把 req 整个 log（zap.Any(req) = log PAN，禁）
//   3. 不要把 req 缓存到任何地方
type NetworkAuthRequest struct {
	PAN        string
	ExpMonth   int
	ExpYear    int
	HolderName string
	Amount     int64
	Currency   string
	MerchantDescriptor string
	ThreeDS    *ThreeDSData
	IdempotencyKey string
}

type NetworkAuthResponse struct {
	NetworkRefNo  string // 卡组织返的交易 ID
	Status        string // "approved" / "declined" / "pending"
	DeclineCode   string
	DeclineReason string
	ARN           string
	MaskedPAN     string // 由 adapter 生成（防止外层依赖 PAN 重新算）
	Network       string
}

type ThreeDSData struct {
	Version   string
	ECI       string
	CAVV      string
	XID       string
	DSTransID string
}

type NetworkCaptureRequest struct {
	NetworkRefNo string
	Amount       int64
	Currency     string
}
type NetworkCaptureResponse struct {
	NetworkRefNo   string
	Status         string
	CapturedAmount int64
}

type NetworkRefundRequest struct {
	NetworkRefNo string
	Amount       int64
	Currency     string
	Reason       string
}
type NetworkRefundResponse struct {
	RefundRefNo    string
	Status         string
	RefundedAmount int64
}

type NetworkVoidRequest struct {
	NetworkRefNo string
	Reason       string
}
type NetworkVoidResponse struct {
	Status string
}

type NetworkQueryRequest struct {
	NetworkRefNo string
}
type NetworkQueryResponse struct {
	NetworkRefNo string
	Status       string
	Amount       int64
	Currency     string
	DeclineCode  string
}

// CardCenter 抽象 card-center 客户端的最小接口（仅 Detokenize）。
//
// 实现见 internal/cardcenterclient/client.go。生产路径走 mTLS 到独立 DC 内
// 部署的 card-center 副本组（DNS round-robin）。
type CardCenter interface {
	// Detokenize payment_token + pi_id 校验 → 返回 PAN payload。
	Detokenize(ctx context.Context, paymentToken, piID string) (*Detokenized, error)
}

// Detokenized 跟 card-center.vault.Detokenized 同形，避免循环 import。
type Detokenized struct {
	PAN        string
	ExpMonth   int
	ExpYear    int
	HolderName string
	PIID       string
	Amount     int64
	Currency   string
}

// CardTransactionRepo 持久化层接口。**只存** masked_pan + 元数据，不存 PAN。
type CardTransactionRepo interface {
	Insert(ctx context.Context, tx *CardTransaction) error
	UpdateStatus(ctx context.Context, networkRefNo string, status, declineCode string) error
	GetByPI(ctx context.Context, piID string) (*CardTransaction, error)
	GetByNetworkRef(ctx context.Context, networkRefNo string) (*CardTransaction, error)
}

// CardTransaction DB 行（**严禁**含 PAN / CVV / track data）
type CardTransaction struct {
	ID             int64     `gorm:"column:id;primaryKey;autoIncrement"`
	PIID           string    `gorm:"column:pi_id"`
	Network        string    `gorm:"column:network"`
	NetworkRefNo   string    `gorm:"column:network_ref_no"`
	MaskedPAN      string    `gorm:"column:masked_pan"` // BIN(6) + last4
	Amount         int64     `gorm:"column:amount"`
	Currency       string    `gorm:"column:currency"`
	Status         string    `gorm:"column:status"`
	DeclineCode    string    `gorm:"column:decline_code"`
	DeclineReason  string    `gorm:"column:decline_reason"`
	ARN            string    `gorm:"column:arn"`
	IdempotencyKey string    `gorm:"column:idempotency_key"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

// Processor 主类
type Processor struct {
	cardCenter CardCenter
	networks   map[string]Network // "visa" → visaAdapter
	repo       CardTransactionRepo
	logger     *zap.Logger
}

func NewProcessor(cc CardCenter, networks map[string]Network, repo CardTransactionRepo, logger *zap.Logger) *Processor {
	return &Processor{
		cardCenter: cc,
		networks:   networks,
		repo:       repo,
		logger:     logger,
	}
}

// Authorize 处理 payment-channel 来的 Authorize 请求。
//
// 关键路径（PAN 生命周期 < 1ms，仅在本函数 stack 内）：
//
//	1. detok = cardCenter.Detokenize(payment_token, pi_id)   // 拿 PAN
//	2. authReq.PAN = detok.PAN                                // 仅传给 network adapter
//	3. resp = network.Authorize(authReq)                      // adapter 发 HTTPS 给 Visa
//	4. detok = nil; authReq.PAN = ""                          // 主动清栈
//	5. repo.Insert(masked_pan, network_ref_no, status, ...)   // DB 不存 PAN
//	6. return (network_ref_no, status) 给 payment-channel
type AuthorizeInput struct {
	PaymentToken       string
	PIID               string
	Amount             int64
	Currency           string
	Network            string // 期望 network；空则 BIN 自动判断
	MerchantDescriptor string
	IdempotencyKey     string
	ThreeDS            *ThreeDSData
}

type AuthorizeOutput struct {
	NetworkRefNo  string
	Status        string
	DeclineCode   string
	DeclineReason string
	MaskedPAN     string
	Network       string
	ARN           string
}

func (p *Processor) Authorize(ctx context.Context, in *AuthorizeInput) (*AuthorizeOutput, error) {
	if in == nil || in.PaymentToken == "" || in.PIID == "" {
		return nil, fmt.Errorf("authorize: payment_token / pi_id required")
	}

	// 幂等检查：同一 idempotency_key 已处理过 → 返原结果
	if in.IdempotencyKey != "" {
		if existing, err := p.repo.GetByPI(ctx, in.PIID); err == nil && existing != nil &&
			existing.IdempotencyKey == in.IdempotencyKey {
			return &AuthorizeOutput{
				NetworkRefNo: existing.NetworkRefNo,
				Status:       existing.Status,
				DeclineCode:  existing.DeclineCode,
				MaskedPAN:    existing.MaskedPAN,
				Network:      existing.Network,
			}, nil
		}
	}

	// ─── PAN 生命周期开始 ───
	detok, err := p.cardCenter.Detokenize(ctx, in.PaymentToken, in.PIID)
	if err != nil {
		return nil, fmt.Errorf("detokenize: %w", err)
	}
	defer func() {
		// 主动清栈：把 plaintext PAN 字段覆盖。Go 没强制 zero，但赋空避免后续
		// 误用同一变量；编译器若内联可能优化掉，关键还是不让它流到 log / DB。
		detok.PAN = ""
		detok.HolderName = ""
	}()

	network := in.Network
	if network == "" {
		network = detectNetworkFromPAN(detok.PAN)
	}
	adapter, ok := p.networks[network]
	if !ok {
		return nil, fmt.Errorf("authorize: network %q not supported", network)
	}

	masked := maskPAN(detok.PAN)
	authReq := &NetworkAuthRequest{
		PAN:                detok.PAN,
		ExpMonth:           detok.ExpMonth,
		ExpYear:            detok.ExpYear,
		HolderName:         detok.HolderName,
		Amount:             in.Amount,
		Currency:           in.Currency,
		MerchantDescriptor: in.MerchantDescriptor,
		ThreeDS:            in.ThreeDS,
		IdempotencyKey:     in.IdempotencyKey,
	}
	defer func() {
		authReq.PAN = ""
		authReq.HolderName = ""
	}()

	resp, err := adapter.Authorize(ctx, authReq)
	// ─── PAN 生命周期结束（authReq / detok 在 defer 里清掉）───
	if err != nil {
		// network 故障也要写 DB，方便对账（status=error）
		_ = p.repo.Insert(ctx, &CardTransaction{
			PIID:           in.PIID,
			Network:        network,
			MaskedPAN:      masked,
			Amount:         in.Amount,
			Currency:       in.Currency,
			Status:         "error",
			DeclineCode:    "NETWORK_ERROR",
			IdempotencyKey: in.IdempotencyKey,
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		})
		p.logger.Error("network authorize failed",
			zap.String("pi_id", in.PIID),
			zap.String("network", network),
			zap.Error(err))
		return nil, err
	}

	// 持久化（**只存 masked**）
	if err := p.repo.Insert(ctx, &CardTransaction{
		PIID:           in.PIID,
		Network:        network,
		NetworkRefNo:   resp.NetworkRefNo,
		MaskedPAN:      masked,
		Amount:         in.Amount,
		Currency:       in.Currency,
		Status:         resp.Status,
		DeclineCode:    resp.DeclineCode,
		IdempotencyKey: in.IdempotencyKey,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}); err != nil {
		// DB 写失败但卡组织已扣 → 严重不一致，需要后台对账兜底
		p.logger.Error("persist card_transaction failed (network already charged)",
			zap.String("pi_id", in.PIID),
			zap.String("network_ref_no", resp.NetworkRefNo),
			zap.Error(err))
		// 仍然返回 network 的成功结果，让上游知道钱扣了
	}

	return &AuthorizeOutput{
		NetworkRefNo:  resp.NetworkRefNo,
		Status:        resp.Status,
		DeclineCode:   resp.DeclineCode,
		DeclineReason: resp.DeclineReason,
		MaskedPAN:     masked,
		Network:       network,
		ARN:           resp.ARN,
	}, nil
}

// Capture / Refund / Void / Query 不需要 PAN，直接基于 network_ref_no 操作
// （卡组织侧已绑定原 transaction）。这里仅 stub，下个 stage 实现。

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

func detectNetworkFromPAN(pan string) string {
	if len(pan) < 6 {
		return "unknown"
	}
	switch {
	case pan[0] == '4':
		return "visa"
	case pan[0] == '5' && pan[1] >= '1' && pan[1] <= '5':
		return "mastercard"
	case pan[:2] == "34" || pan[:2] == "37":
		return "amex"
	case pan[:2] == "35":
		return "jcb"
	case pan[:2] == "62":
		return "unionpay"
	}
	return "unknown"
}
