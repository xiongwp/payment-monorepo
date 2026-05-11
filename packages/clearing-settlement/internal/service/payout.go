// payout.go — 提现服务。
//
// 入口:
//   PayoutService.Request           商户/ops 发起提现请求
//   PayoutService.Approve           ops 复核大额提现
//   PayoutService.Send              cron 调银行 API 真正出账
//   PayoutService.MarkSettled       银行 webhook 回调标记到账
//   PayoutService.MarkFailed        银行回退 + 资金归还可用余额
//
// 准备金规则:
//   ReservePolicy.Apply(merchant, amount)  → 算本次该扣多少 reserve
//   ReleaseScheduler 每日扫到期 hold → 释放进可用余额

package service

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/clearing-settlement/internal/domain"
)

// PayoutRepository 抽象 — 真实实现 MySQL（按 merchant_id hash 分库）。
type PayoutRepository interface {
	// 银行账户
	GetBankAccount(ctx context.Context, id int64) (*domain.MerchantBankAccount, error)
	GetDefaultBankAccount(ctx context.Context, merchantID, currency string) (*domain.MerchantBankAccount, error)
	// Payout
	CreatePayout(ctx context.Context, p *domain.Payout) (int64, error)
	GetPayout(ctx context.Context, id int64) (*domain.Payout, error)
	UpdatePayoutStatus(ctx context.Context, id int64, status domain.PayoutStatus, fields map[string]any) error
	ListPendingApproval(ctx context.Context, limit int) ([]*domain.Payout, error)
	ListPendingSend(ctx context.Context, limit int) ([]*domain.Payout, error)
	// 幂等
	GetPayoutByIdempotencyKey(ctx context.Context, key string) (*domain.Payout, error)
	// Reserve
	GetReserveAccount(ctx context.Context, merchantID, currency string) (*domain.ReserveAccount, error)
	CreateReserveHold(ctx context.Context, h *domain.ReserveHold) (int64, error)
	ListExpiredHolds(ctx context.Context, now time.Time, limit int) ([]*domain.ReserveHold, error)
	MarkHoldReleased(ctx context.Context, holdID int64) error
	// PayoutStatement
	CreatePayoutStatement(ctx context.Context, ps *domain.PayoutStatement) (int64, error)
}

// BankAPIClient 抽象银行 API 调用（生产换 SWIFT/ACH/SEPA adapter）。
type BankAPIClient interface {
	// Send 提交一笔转账到银行。返 bank_ref（银行端流水号）+ error。
	// 成功只代表"提交受理"，到账确认要看后续 webhook / 对账文件。
	Send(ctx context.Context, p *domain.Payout, acct *domain.MerchantBankAccount) (bankRef string, err error)
}

// PayoutService 主对外接口。
type PayoutService struct {
	repo      PayoutRepository
	bank      BankAPIClient
	log       *zap.Logger
	approvalThresholdMinor int64 // 超过这个金额需要 ops 复核
}

// NewPayoutService 构造。
func NewPayoutService(repo PayoutRepository, bank BankAPIClient, log *zap.Logger) *PayoutService {
	if log == nil {
		log = zap.NewNop()
	}
	return &PayoutService{
		repo:                   repo,
		bank:                   bank,
		log:                    log,
		approvalThresholdMinor: 1_000_000, // $10,000
	}
}

// SetApprovalThreshold 改大额阈值（dev 测试或 ops 调整）。
func (s *PayoutService) SetApprovalThreshold(minor int64) {
	s.approvalThresholdMinor = minor
}

// PayoutRequest 创建提现请求的入参。
type PayoutRequest struct {
	MerchantID     string
	BankAccountID  int64 // 0 = 用默认
	AmountMinor    int64
	Currency       string
	RequestedBy    string // 'merchant'|'ops'|'cron'
	IdempotencyKey string // 防重复提单
	TraceID        string
}

// Request 发起提现请求。
//
// 幂等：同 idempotency_key 已存在 → 返已有 Payout 不重建。
// 大额（> approvalThresholdMinor）状态 = pending_approval，等 ops Approve；
// 小额直接 approved 进 send 队列。
func (s *PayoutService) Request(ctx context.Context, req PayoutRequest) (*domain.Payout, error) {
	if req.MerchantID == "" || req.AmountMinor <= 0 || req.Currency == "" {
		return nil, fmt.Errorf("merchant_id/amount/currency required")
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = computeIdempotencyKey(req.MerchantID,
			fmt.Sprintf("%d", req.AmountMinor), req.Currency,
			time.Now().Format("2006-01-02T15"))
	}
	// 幂等 check
	if existing, _ := s.repo.GetPayoutByIdempotencyKey(ctx, req.IdempotencyKey); existing != nil {
		s.log.Info("payout idempotent hit",
			zap.String("idempotency_key", req.IdempotencyKey),
			zap.Int64("existing_id", existing.ID))
		return existing, nil
	}

	// 解析 bank account
	var acct *domain.MerchantBankAccount
	var err error
	if req.BankAccountID > 0 {
		acct, err = s.repo.GetBankAccount(ctx, req.BankAccountID)
	} else {
		acct, err = s.repo.GetDefaultBankAccount(ctx, req.MerchantID, req.Currency)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve bank account: %w", err)
	}
	if acct == nil {
		return nil, fmt.Errorf("no bank account for merchant %s currency %s", req.MerchantID, req.Currency)
	}
	if acct.Status != domain.BankAcctVerified {
		return nil, fmt.Errorf("bank account %d not verified (status=%s)", acct.ID, acct.Status)
	}
	if acct.MerchantID != req.MerchantID {
		return nil, fmt.Errorf("bank account belongs to different merchant")
	}

	// 计算 bank fee（生产按 currency / amount 阶梯收；这里简化定额）
	bankFee := int64(50) // 50 cents/centavos 平 transfer 费
	if req.AmountMinor > 1_000_000 {
		bankFee = 100
	}

	status := domain.PayoutApproved
	if req.AmountMinor >= s.approvalThresholdMinor {
		status = domain.PayoutPendingApproval
	}

	now := time.Now().UTC()
	p := &domain.Payout{
		MerchantID:     req.MerchantID,
		BankAccountID:  acct.ID,
		AmountMinor:    req.AmountMinor - bankFee,
		GrossMinor:     req.AmountMinor,
		BankFeeMinor:   bankFee,
		Currency:       req.Currency,
		Status:         status,
		RequestedBy:    req.RequestedBy,
		RequestedAt:    now,
		IdempotencyKey: req.IdempotencyKey,
		TraceID:        req.TraceID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	id, err := s.repo.CreatePayout(ctx, p)
	if err != nil {
		return nil, err
	}
	p.ID = id
	s.log.Info("payout requested",
		zap.Int64("id", id),
		zap.String("merchant_id", req.MerchantID),
		zap.Int64("amount_minor", req.AmountMinor),
		zap.String("status", string(status)))
	return p, nil
}

// Approve ops 复核大额提现。
func (s *PayoutService) Approve(ctx context.Context, payoutID int64, approver string) error {
	p, err := s.repo.GetPayout(ctx, payoutID)
	if err != nil {
		return err
	}
	if p.Status != domain.PayoutPendingApproval {
		return fmt.Errorf("payout %d not in pending_approval (got %s)", payoutID, p.Status)
	}
	if approver == p.RequestedBy {
		return fmt.Errorf("approver cannot be requester")
	}
	now := time.Now().UTC()
	return s.repo.UpdatePayoutStatus(ctx, payoutID, domain.PayoutApproved, map[string]any{
		"approved_by":  approver,
		"approved_at":  &now,
		"updated_at":   now,
	})
}

// Reject ops 拒绝。
func (s *PayoutService) Reject(ctx context.Context, payoutID int64, by, reason string) error {
	now := time.Now().UTC()
	return s.repo.UpdatePayoutStatus(ctx, payoutID, domain.PayoutRejected, map[string]any{
		"approved_by":      by,
		"approved_at":      &now,
		"failure_message":  reason,
		"updated_at":       now,
	})
}

// Send cron 调银行 API 把 approved 状态的 payout 真正打出去。
// 批量处理 — 单次最多 limit 条。
func (s *PayoutService) Send(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	pending, err := s.repo.ListPendingSend(ctx, limit)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, p := range pending {
		acct, err := s.repo.GetBankAccount(ctx, p.BankAccountID)
		if err != nil || acct == nil {
			s.markFailed(ctx, p.ID, "bank_account_missing", err)
			continue
		}
		bankRef, err := s.bank.Send(ctx, p, acct)
		if err != nil {
			s.markFailed(ctx, p.ID, "bank_api_error", err)
			continue
		}
		now := time.Now().UTC()
		s.repo.UpdatePayoutStatus(ctx, p.ID, domain.PayoutSent, map[string]any{
			"bank_ref":    bankRef,
			"sent_at":     &now,
			"updated_at":  now,
		})
		sent++
		s.log.Info("payout sent to bank",
			zap.Int64("id", p.ID), zap.String("bank_ref", bankRef))
	}
	return sent, nil
}

// MarkSettled 银行 webhook 回调到账。
func (s *PayoutService) MarkSettled(ctx context.Context, payoutID int64, bankRef string) error {
	now := time.Now().UTC()
	return s.repo.UpdatePayoutStatus(ctx, payoutID, domain.PayoutSettled, map[string]any{
		"settled_at": &now,
		"bank_ref":   bankRef,
		"updated_at": now,
	})
}

// MarkFailed 银行回退（资金应该归还可用余额，由 caller 单独触发 reserve 调整）。
func (s *PayoutService) MarkFailed(ctx context.Context, payoutID int64, code, msg string) error {
	return s.markFailed(ctx, payoutID, code, fmt.Errorf("%s", msg))
}

func (s *PayoutService) markFailed(ctx context.Context, id int64, code string, err error) error {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	s.log.Warn("payout failed",
		zap.Int64("id", id), zap.String("code", code), zap.String("msg", msg))
	return s.repo.UpdatePayoutStatus(ctx, id, domain.PayoutFailed, map[string]any{
		"failure_code":    code,
		"failure_message": msg,
		"updated_at":      time.Now().UTC(),
	})
}

// computeIdempotencyKey 自动生成 idempotency key — 同 merchant 同小时同金额视为重复。
func computeIdempotencyKey(merchantID, amount, currency, hour string) string {
	h := sha1.New()
	h.Write([]byte(merchantID))
	h.Write([]byte{0})
	h.Write([]byte(amount))
	h.Write([]byte{0})
	h.Write([]byte(currency))
	h.Write([]byte{0})
	h.Write([]byte(hour))
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// ─── Reserve / Hold 管理 ─────────────────────────────────────────────

// ReleaseExpiredHolds cron 每日跑 — 释放已到期的 reserve_hold 回可用余额。
func (s *PayoutService) ReleaseExpiredHolds(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 1000
	}
	now := time.Now().UTC()
	holds, err := s.repo.ListExpiredHolds(ctx, now, limit)
	if err != nil {
		return 0, err
	}
	released := 0
	for _, h := range holds {
		if err := s.repo.MarkHoldReleased(ctx, h.ID); err != nil {
			s.log.Warn("release hold failed",
				zap.Int64("hold_id", h.ID), zap.Error(err))
			continue
		}
		released++
		s.log.Info("reserve hold released",
			zap.Int64("hold_id", h.ID),
			zap.String("merchant_id", h.MerchantID),
			zap.Int64("amount_minor", h.AmountMinor))
	}
	return released, nil
}
