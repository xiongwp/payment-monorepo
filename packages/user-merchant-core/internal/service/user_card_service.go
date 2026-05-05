// Package service 加 user_card 业务方法。
//
// 关键纪律：本服务**不见** PAN。AddCard 入口接到 PAN 后立即 forward 给
// card-center.Tokenize；card-center 返 stored_token 后存 user_card 表。
// PAN 仅在 AddCard 函数 stack 内出现；用 defer 主动清栈。
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// AddCardInput 入参
type AddCardInput struct {
	UserID     int64
	PAN        string // 仅本函数 stack 出现
	ExpMonth   int
	ExpYear    int
	HolderName string
	TraceID    string
	SetDefault bool
}

// AddCardOutput
type AddCardOutput struct {
	UserCardID int64
	MaskedPAN  string
	Network    string
}

// AddCard 用户存卡入口。
//
// 流程：
//  1. 调 card-center.Tokenize 拿 stored_token（card-center 内 KMS 加密）
//  2. 写 user_card 行（**只**存 token + masked + 元数据，不存 PAN）
//  3. SetDefault 选项：把它设成默认卡（同用户其它 default=false）
//
// PAN 生命周期：仅在 input.PAN 字段，函数返回前 defer 清空。
func (s *UserCardService) AddCard(ctx context.Context, in *AddCardInput) (*AddCardOutput, error) {
	if in == nil || in.UserID == 0 || in.PAN == "" {
		return nil, fmt.Errorf("%w: user_id / pan required", domain.ErrValidation)
	}
	defer func() {
		// 主动清 PAN（Go 没强 zero memory，但赋空避免后续误用）
		in.PAN = ""
		in.HolderName = ""
	}()
	if s.cardCenter == nil {
		return nil, fmt.Errorf("AddCard: card-center client not configured (cannot tokenize)")
	}

	tokResp, err := s.cardCenter.Tokenize(ctx, &cardcenterclient.TokenizeRequest{
		UserID:     in.UserID,
		PAN:        in.PAN,
		ExpMonth:   in.ExpMonth,
		ExpYear:    in.ExpYear,
		HolderName: in.HolderName,
		TraceID:    in.TraceID,
	})
	if err != nil {
		return nil, fmt.Errorf("tokenize: %w", err)
	}

	tokenHash := sha256Hex(tokResp.StoredToken)
	card := &domain.UserCard{
		UserID:      in.UserID,
		StoredToken: tokResp.StoredToken,
		TokenHash:   tokenHash,
		MaskedPAN:   tokResp.MaskedPAN,
		Network:     tokResp.Network,
		ExpMonth:    in.ExpMonth,
		ExpYear:     in.ExpYear,
		HolderName:  in.HolderName,
		Status:      domain.UserCardActive,
		IsDefault:   in.SetDefault,
	}
	if err := s.repo.Insert(ctx, card); err != nil {
		// 入库失败：card-center 已经发了 token；调 DeleteCard 标 stored_token revoked
		// 让 card-center audit 知道这条 token 实际未被使用。best-effort，不阻塞错误。
		go func() {
			if cerr := s.cardCenter.DeleteCard(context.Background(), in.UserID,
				tokResp.StoredToken, "user_card insert failed", in.TraceID); cerr != nil {
				s.logger.Warn("rollback card-center DeleteCard failed",
					zap.Int64("user_id", in.UserID), zap.Error(cerr))
			}
		}()
		return nil, fmt.Errorf("user_card insert: %w", err)
	}

	if in.SetDefault {
		if err := s.repo.SetDefault(ctx, in.UserID, card.ID); err != nil {
			s.logger.Warn("set default failed (card stored, but not default)",
				zap.Int64("user_card_id", card.ID), zap.Error(err))
		}
	}

	return &AddCardOutput{
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
