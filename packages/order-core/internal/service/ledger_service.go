// Package service — ledger service with posting helpers for payment events.
//
// The raw LedgerRepository.Post gives you a "dumb" double-entry journal. This
// service wraps it with the specific posting rules for our business events
// (charge / refund / fee / settlement) so callers don't need to reconstruct
// account naming conventions and debit/credit sides on every emission site.
package service

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/idgen"
	"github.com/xiongwp/order-core/internal/repo"
	"github.com/xiongwp/payment-util/shadow"
)

// LedgerService higher-level operations on the GL.
//
// Account naming convention (enforced by AccountID helpers below):
//   acct_platform_cash                        — our operating bank account (asset)
//   acct_platform_fees_revenue                — fees earned from merchants (revenue)
//   acct_platform_channel_expense             — costs we pay channels (expense)
//   acct_channel_receivable_<channel_name>    — channel owes us (asset)
//   acct_channel_payable_<channel_name>       — we owe channel for refunds (liability)
//   acct_merchant_payable_<merchant_id>       — we owe merchant (liability)
//   acct_merchant_receivable_<merchant_id>    — merchant owes us (asset)  rare
//
// Posting rules (single-currency PHP):
//
//   PostCharge(mchID, channel, gross, fee)
//     DR acct_channel_receivable_<channel>   gross
//     CR acct_merchant_payable_<mchID>       (gross - fee)
//     CR acct_platform_fees_revenue           fee
//
//   PostRefund(mchID, channel, gross, feeRebate)
//     DR acct_merchant_payable_<mchID>       (gross - feeRebate)
//     DR acct_platform_fees_revenue           feeRebate            — reverse of fee
//     CR acct_channel_receivable_<channel>   gross
//
//   PostSettlementPayout(mchID, amount)       — we wire to merchant
//     DR acct_merchant_payable_<mchID>       amount
//     CR acct_platform_cash                  amount
//
//   PostChannelSettlementReceived(channel, amount)
//     DR acct_platform_cash                  amount
//     CR acct_channel_receivable_<channel>   amount
type LedgerService interface {
	EnsureMerchantAccounts(ctx context.Context, merchantID string) error
	EnsureChannelAccounts(ctx context.Context, channelName string) error
	EnsurePlatformAccounts(ctx context.Context) error

	PostCharge(ctx context.Context, merchantID, channel, refType, refID string, grossMinor, feeMinor int64) (*domain.GLTransaction, error)
	PostRefund(ctx context.Context, merchantID, channel, refType, refID string, grossMinor, feeRebateMinor int64) (*domain.GLTransaction, error)
	PostSettlementPayout(ctx context.Context, merchantID, refID string, amountMinor int64) (*domain.GLTransaction, error)
	PostChannelSettlement(ctx context.Context, channel, refID string, amountMinor int64) (*domain.GLTransaction, error)
	PostAdjustment(ctx context.Context, memo, actor string, lines []domain.PostingLine) (*domain.GLTransaction, error)

	// Reads
	MerchantPayableBalance(ctx context.Context, merchantID string) (int64, error)
	GetAccount(ctx context.Context, id string) (*domain.GLAccount, error)
	ListAccounts(ctx context.Context, ownerType domain.AccountOwnerType, ownerID string, limit, offset int) ([]*domain.GLAccount, int64, error)
	ListEntries(ctx context.Context, accountID string, since, until *time.Time, limit, offset int) ([]*domain.GLEntry, int64, error)
	ListTransactions(ctx context.Context, eventType, refType, refID string, limit, offset int) ([]*domain.GLTransaction, int64, error)
	GetTransaction(ctx context.Context, id string) (*domain.GLTransaction, []*domain.GLEntry, error)
}

type ledgerService struct {
	repo   repo.LedgerRepository
	idg    idgen.IDGenerator
	logger *zap.Logger
}

// NewLedgerService 构造
func NewLedgerService(r repo.LedgerRepository, g idgen.IDGenerator, logger *zap.Logger) LedgerService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ledgerService{repo: r, idg: g, logger: logger}
}

// ─── account naming ──────────────────────────────────────────────────────────

func acctPlatformCash() string       { return "acct_platform_cash" }
func acctPlatformFees() string       { return "acct_platform_fees_revenue" }
func acctPlatformChannelExp() string { return "acct_platform_channel_expense" }

func acctMerchantPayable(id string) string    { return "acct_merchant_payable_" + id }
func acctMerchantReceivable(id string) string { return "acct_merchant_receivable_" + id }

func acctChannelReceivable(name string) string { return "acct_channel_receivable_" + name }
func acctChannelPayable(name string) string    { return "acct_channel_payable_" + name }

// ─── ensure (idempotent provisioning) ───────────────────────────────────────

func (s *ledgerService) EnsurePlatformAccounts(ctx context.Context) error {
	for _, a := range []*domain.GLAccount{
		{ID: acctPlatformCash(), Name: "Platform Cash", Type: domain.AccountAsset, OwnerType: domain.OwnerPlatform},
		{ID: acctPlatformFees(), Name: "Platform Fees Revenue", Type: domain.AccountRevenue, OwnerType: domain.OwnerPlatform},
		{ID: acctPlatformChannelExp(), Name: "Channel Cost Expense", Type: domain.AccountExpense, OwnerType: domain.OwnerPlatform},
	} {
		if _, err := s.repo.EnsureAccount(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

func (s *ledgerService) EnsureMerchantAccounts(ctx context.Context, merchantID string) error {
	for _, a := range []*domain.GLAccount{
		{ID: acctMerchantPayable(merchantID), Name: "Merchant " + merchantID + " Payable", Type: domain.AccountLiability, OwnerType: domain.OwnerMerchant, OwnerID: merchantID},
		{ID: acctMerchantReceivable(merchantID), Name: "Merchant " + merchantID + " Receivable", Type: domain.AccountAsset, OwnerType: domain.OwnerMerchant, OwnerID: merchantID},
	} {
		if _, err := s.repo.EnsureAccount(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

func (s *ledgerService) EnsureChannelAccounts(ctx context.Context, channel string) error {
	for _, a := range []*domain.GLAccount{
		{ID: acctChannelReceivable(channel), Name: "Channel " + channel + " Receivable", Type: domain.AccountAsset, OwnerType: domain.OwnerChannel, OwnerID: channel},
		{ID: acctChannelPayable(channel), Name: "Channel " + channel + " Payable", Type: domain.AccountLiability, OwnerType: domain.OwnerChannel, OwnerID: channel},
	} {
		if _, err := s.repo.EnsureAccount(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

// ─── posting helpers ────────────────────────────────────────────────────────

// newTxnID 按位编码生成 GL 流水 ID（idType=110, 全局非分片所以 globalTbl=0）。
func (s *ledgerService) newTxnID(ctx context.Context) (string, error) {
	n, err := s.idg.NextID(ctx, idgen.BizTagLedgerTxn)
	if err != nil {
		return "", err
	}
	return shadow.EncodeIDStr(ctx, shadow.IDTypeOrderGLTxn, 0, n)
}

func (s *ledgerService) PostCharge(ctx context.Context, merchantID, channel, refType, refID string, grossMinor, feeMinor int64) (*domain.GLTransaction, error) {
	if grossMinor <= 0 {
		return nil, fmt.Errorf("%w: gross must be > 0", domain.ErrValidation)
	}
	if feeMinor < 0 || feeMinor > grossMinor {
		return nil, fmt.Errorf("%w: fee must be in [0, gross]", domain.ErrValidation)
	}
	if err := s.ensureFor(ctx, merchantID, channel); err != nil {
		return nil, err
	}
	txnID, err := s.newTxnID(ctx)
	if err != nil {
		return nil, err
	}
	lines := []domain.PostingLine{
		{AccountID: acctChannelReceivable(channel), Debit: grossMinor, Memo: "charge gross from channel"},
		{AccountID: acctMerchantPayable(merchantID), Credit: grossMinor - feeMinor, Memo: "merchant net"},
	}
	if feeMinor > 0 {
		lines = append(lines, domain.PostingLine{AccountID: acctPlatformFees(), Credit: feeMinor, Memo: "platform fee"})
	}
	return s.repo.Post(ctx, txnID, &domain.PostingRequest{
		EventType: "charge.succeeded",
		RefType:   refType, RefID: refID,
		Memo:  fmt.Sprintf("charge %s via %s", refID, channel),
		Lines: lines,
	})
}

func (s *ledgerService) PostRefund(ctx context.Context, merchantID, channel, refType, refID string, grossMinor, feeRebateMinor int64) (*domain.GLTransaction, error) {
	if grossMinor <= 0 {
		return nil, fmt.Errorf("%w: gross must be > 0", domain.ErrValidation)
	}
	if feeRebateMinor < 0 || feeRebateMinor > grossMinor {
		return nil, fmt.Errorf("%w: fee_rebate must be in [0, gross]", domain.ErrValidation)
	}
	if err := s.ensureFor(ctx, merchantID, channel); err != nil {
		return nil, err
	}
	txnID, err := s.newTxnID(ctx)
	if err != nil {
		return nil, err
	}
	lines := []domain.PostingLine{
		{AccountID: acctMerchantPayable(merchantID), Debit: grossMinor - feeRebateMinor, Memo: "refund claws back merchant net"},
		{AccountID: acctChannelReceivable(channel), Credit: grossMinor, Memo: "refund channel receivable reversed"},
	}
	if feeRebateMinor > 0 {
		lines = append(lines, domain.PostingLine{AccountID: acctPlatformFees(), Debit: feeRebateMinor, Memo: "refund fee rebate"})
	}
	return s.repo.Post(ctx, txnID, &domain.PostingRequest{
		EventType: "refund.succeeded",
		RefType:   refType, RefID: refID,
		Memo:  fmt.Sprintf("refund %s via %s", refID, channel),
		Lines: lines,
	})
}

func (s *ledgerService) PostSettlementPayout(ctx context.Context, merchantID, refID string, amountMinor int64) (*domain.GLTransaction, error) {
	if amountMinor <= 0 {
		return nil, fmt.Errorf("%w: amount must be > 0", domain.ErrValidation)
	}
	if err := s.EnsureMerchantAccounts(ctx, merchantID); err != nil {
		return nil, err
	}
	if err := s.EnsurePlatformAccounts(ctx); err != nil {
		return nil, err
	}
	txnID, err := s.newTxnID(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.Post(ctx, txnID, &domain.PostingRequest{
		EventType: "settlement.payout",
		RefType:   "settlement", RefID: refID,
		Memo: fmt.Sprintf("payout to merchant %s", merchantID),
		Lines: []domain.PostingLine{
			{AccountID: acctMerchantPayable(merchantID), Debit: amountMinor, Memo: "payout discharges merchant payable"},
			{AccountID: acctPlatformCash(), Credit: amountMinor, Memo: "cash out"},
		},
	})
}

func (s *ledgerService) PostChannelSettlement(ctx context.Context, channel, refID string, amountMinor int64) (*domain.GLTransaction, error) {
	if amountMinor <= 0 {
		return nil, fmt.Errorf("%w: amount must be > 0", domain.ErrValidation)
	}
	if err := s.EnsureChannelAccounts(ctx, channel); err != nil {
		return nil, err
	}
	if err := s.EnsurePlatformAccounts(ctx); err != nil {
		return nil, err
	}
	txnID, err := s.newTxnID(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.Post(ctx, txnID, &domain.PostingRequest{
		EventType: "settlement.received",
		RefType:   "settlement", RefID: refID,
		Memo: fmt.Sprintf("channel %s remitted %d", channel, amountMinor),
		Lines: []domain.PostingLine{
			{AccountID: acctPlatformCash(), Debit: amountMinor, Memo: "received from channel"},
			{AccountID: acctChannelReceivable(channel), Credit: amountMinor, Memo: "channel receivable cleared"},
		},
	})
}

// PostAdjustment runs an arbitrary balanced multi-leg posting, used for
// manual corrections from the admin UI. Accounts are validated by Post();
// service only adds an audit marker in the memo.
func (s *ledgerService) PostAdjustment(ctx context.Context, memo, actor string, lines []domain.PostingLine) (*domain.GLTransaction, error) {
	txnID, err := s.newTxnID(ctx)
	if err != nil {
		return nil, err
	}
	if memo == "" {
		memo = "manual adjustment"
	}
	return s.repo.Post(ctx, txnID, &domain.PostingRequest{
		EventType: "adjustment.manual",
		RefType:   "admin", RefID: actor,
		Memo:  memo,
		Lines: lines,
	})
}

// ─── reads ───────────────────────────────────────────────────────────────────

func (s *ledgerService) MerchantPayableBalance(ctx context.Context, merchantID string) (int64, error) {
	a, err := s.repo.GetAccount(ctx, acctMerchantPayable(merchantID))
	if err != nil {
		return 0, err
	}
	return a.NetBalance(), nil
}

func (s *ledgerService) GetAccount(ctx context.Context, id string) (*domain.GLAccount, error) {
	return s.repo.GetAccount(ctx, id)
}

func (s *ledgerService) ListAccounts(ctx context.Context, ownerType domain.AccountOwnerType, ownerID string, limit, offset int) ([]*domain.GLAccount, int64, error) {
	return s.repo.ListAccounts(ctx, ownerType, ownerID, limit, offset)
}

func (s *ledgerService) ListEntries(ctx context.Context, accountID string, since, until *time.Time, limit, offset int) ([]*domain.GLEntry, int64, error) {
	return s.repo.ListEntries(ctx, accountID, since, until, limit, offset)
}

func (s *ledgerService) ListTransactions(ctx context.Context, eventType, refType, refID string, limit, offset int) ([]*domain.GLTransaction, int64, error) {
	return s.repo.ListTransactions(ctx, eventType, refType, refID, limit, offset)
}

func (s *ledgerService) GetTransaction(ctx context.Context, id string) (*domain.GLTransaction, []*domain.GLEntry, error) {
	return s.repo.GetTransaction(ctx, id)
}

// ensureFor provisions platform + merchant + channel accounts in one call.
func (s *ledgerService) ensureFor(ctx context.Context, merchantID, channel string) error {
	if err := s.EnsurePlatformAccounts(ctx); err != nil {
		return err
	}
	if merchantID != "" {
		if err := s.EnsureMerchantAccounts(ctx, merchantID); err != nil {
			return err
		}
	}
	if channel != "" {
		if err := s.EnsureChannelAccounts(ctx, channel); err != nil {
			return err
		}
	}
	return nil
}
