// Package service 加 user_card 业务方法。
//
// PCI 严格纪律：本服务**永远不接受 PAN**（包括函数参数 / DB / 日志）。
//
// 绑卡流程（PAN 单跳化）：
//
//	1. 浏览器 HTTPS POST 直发 card-center → card-center 返 stored_token + masked + network
//	2. 浏览器 POST 到 api-gateway /cards/attach 带 stored_token（无 PAN）
//	3. api-gateway 调本 service.AttachCard → 写 user_card 行（无 PAN，只有 stored_token + 元数据）
//
// 本文件之前有 AddCard(...PAN...) 直接调 card-center.Tokenize 的版本，已废弃 — 那条
// 路径让 user-merchant-core 进程见到 PAN，不符 SAQ-A 范围。
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/user-merchant-core/internal/cardcenterclient"
	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/internal/repo"
)

// UserCardService 用户存卡 / 列卡 / 删卡业务
type UserCardService struct {
	repo       repo.UserCardRepository
	cardCenter *cardcenterclient.Client
	logger     *zap.Logger
}

// NewUserCardService 构造。cardCenter=nil 时所有 Add/Delete 都报错（dev/单测可用）。
func NewUserCardService(r repo.UserCardRepository, cc *cardcenterclient.Client, logger *zap.Logger) *UserCardService {
	return &UserCardService{repo: r, cardCenter: cc, logger: logger}
}

// AttachCardInput 入参 — **永远不含 PAN/CVV**。
//
// stored_token 是浏览器先 HTTPS POST 到 card-center 拿到的；本服务只负责把
// 这条记录持久化到 user_card 表。
type AttachCardInput struct {
	UserID      int64
	StoredToken string // tok_card_xxx，由 card-center 颁发
	MaskedPAN   string // 411111******1111
	Network     string // visa / mastercard / ...
	ExpMonth    int
	ExpYear     int
	HolderName  string
	TraceID     string
	SetDefault  bool
}

// AttachCardOutput
type AttachCardOutput struct {
	UserCardID int64
	MaskedPAN  string
	Network    string
}

// AttachCard 用户存卡持久化入口。
//
// 协议契约：
//   - 调用方（api-gateway）已经从 jwt 拿到权威 user_id；不接受请求体里的 user_id
//   - stored_token 必须是 card-center 颁发格式（tok_card_*）；其它格式硬拒
//   - masked_pan 必须含 *；防止异常前端误传完整 PAN
//   - 本服务**不调** card-center.Tokenize（PAN 单跳已经在 浏览器 ↔ card-center 之间完成）
func (s *UserCardService) AttachCard(ctx context.Context, in *AttachCardInput) (*AttachCardOutput, error) {
	if in == nil || in.UserID == 0 || in.StoredToken == "" {
		return nil, fmt.Errorf("%w: user_id / stored_token required", domain.ErrValidation)
	}
	if !strings.HasPrefix(in.StoredToken, "tok_card_") {
		return nil, fmt.Errorf("%w: stored_token format invalid", domain.ErrValidation)
	}
	if in.MaskedPAN == "" || !strings.Contains(in.MaskedPAN, "*") {
		// 防止上游误传 PAN 进 masked_pan 字段
		return nil, fmt.Errorf("%w: masked_pan must be masked (contain '*')", domain.ErrValidation)
	}

	tokenHash := sha256Hex(in.StoredToken)
	card := &domain.UserCard{
		UserID:      in.UserID,
		StoredToken: in.StoredToken,
		TokenHash:   tokenHash,
		MaskedPAN:   in.MaskedPAN,
		Network:     in.Network,
		ExpMonth:    in.ExpMonth,
		ExpYear:     in.ExpYear,
		HolderName:  in.HolderName,
		Status:      domain.UserCardActive,
		IsDefault:   in.SetDefault,
	}
	if err := s.repo.Insert(ctx, card); err != nil {
		// 入库失败：让 card-center audit 知道这条 stored_token 没被业务持有 →
		// 异步调 card-center.DeleteCard 标 revoked，best-effort
		if s.cardCenter != nil {
			go func(tok string) {
				if cerr := s.cardCenter.DeleteCard(context.Background(), in.UserID,
					tok, "user_card insert failed", in.TraceID); cerr != nil {
					s.logger.Warn("rollback card-center DeleteCard failed",
						zap.Int64("user_id", in.UserID), zap.Error(cerr))
				}
			}(in.StoredToken)
		}
		return nil, fmt.Errorf("user_card insert: %w", err)
	}

	if in.SetDefault {
		if err := s.repo.SetDefault(ctx, in.UserID, card.ID); err != nil {
			s.logger.Warn("set default failed (card stored, but not default)",
				zap.Int64("user_card_id", card.ID), zap.Error(err))
		}
	}

	return &AttachCardOutput{
		UserCardID: card.ID,
		MaskedPAN:  card.MaskedPAN,
		Network:    card.Network,
	}, nil
}

// ListCards 列出用户 active 卡（前端展示用）。
//
// 安全：返回的 UserCard 把 StoredToken 字段清空，防止前端误存。
// 真实 stored_token 仅在支付时由 order-core 内部取（GetByID）。
func (s *UserCardService) ListCards(ctx context.Context, userID int64) ([]*domain.UserCard, error) {
	cards, err := s.repo.ListActiveByUser(ctx, userID, 20)
	if err != nil {
		return nil, err
	}
	for _, c := range cards {
		c.StoredToken = "" // 不外传
	}
	return cards, nil
}

// DeleteCard 删卡：
//   1. 业务层 soft delete user_card 行
//   2. 通知 card-center revoke stored_token（异步，best-effort）
func (s *UserCardService) DeleteCard(ctx context.Context, userID, userCardID int64, traceID string) error {
	card, err := s.repo.GetByID(ctx, userID, userCardID)
	if err != nil {
		return err
	}
	if err := s.repo.SoftDelete(ctx, userID, userCardID); err != nil {
		return err
	}
	// 异步通知 card-center 把这条 stored_token 标 revoked
	if s.cardCenter != nil {
		go func(tok string) {
			if cerr := s.cardCenter.DeleteCard(context.Background(), userID, tok,
				"user requested delete", traceID); cerr != nil {
				s.logger.Warn("card-center DeleteCard async failed (db deleted)",
					zap.Int64("user_card_id", userCardID), zap.Error(cerr))
			}
		}(card.StoredToken)
	}
	return nil
}

// SetDefault 设置默认卡
func (s *UserCardService) SetDefault(ctx context.Context, userID, userCardID int64) error {
	return s.repo.SetDefault(ctx, userID, userCardID)
}

// GetStoredTokenForPayment 内部用：order-core 创建 PI 时通过本方法拿 stored_token，
// 然后调 card-center.CreatePaymentToken 派生支付 token。
//
// 注意：不外暴 gRPC；只允许 user-merchant-core 内 / 受信内部 RPC 调用。
func (s *UserCardService) GetStoredTokenForPayment(ctx context.Context, userID, userCardID int64) (storedToken, maskedPAN, network string, err error) {
	card, err := s.repo.GetByID(ctx, userID, userCardID)
	if err != nil {
		return "", "", "", err
	}
	if !card.IsActive(time.Now()) {
		return "", "", "", domain.ErrUserCardExpired
	}
	return card.StoredToken, card.MaskedPAN, card.Network, nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
