// Package workflow — Wallet 主要 use case 实现.
//
// 全部走 accounting AtomicBatchBooking, wallet 服务自己只:
//   1. 校验 wallet 状态 (frozen / closed 拒绝)
//   2. 校验余额够 (accounting 仍会复验, 这里早 fail 快)
//   3. 写本地 Transaction 记录 (审计 + 历史查询冗余)
//   4. 调 accounting 落账
//   5. 回填 voucher_no

package workflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"reconcile-system/packages/wallet-service/internal/domain"

	accountingv1 "github.com/xiongwp/accounting-system/api/proto/accounting/v1"
)

// Service ...
type Service struct {
	Wallets WalletRepo
	Txns    TxnRepo
	Acct    AccountingClient
	Audit   AuditClient
	Now     func() time.Time
}

// WalletRepo ...
type WalletRepo interface {
	Get(ctx context.Context, ownerType, ownerID string) (*domain.Wallet, error)
	Save(ctx context.Context, w *domain.Wallet) error
}

// TxnRepo ...
type TxnRepo interface {
	Save(ctx context.Context, t *domain.Transaction) error
	List(ctx context.Context, ownerType, ownerID string, limit int) ([]*domain.Transaction, error)
}

// AccountingClient ...
type AccountingClient interface {
	AtomicBatchBooking(ctx context.Context, in *accountingv1.AtomicBatchBookingRequest) (*accountingv1.AtomicBatchBookingResponse, error)
	GetBalanceSnapshot(ctx context.Context, in *accountingv1.GetBalanceSnapshotRequest) (*accountingv1.GetBalanceSnapshotResponse, error)
}

// AuditClient ...
type AuditClient interface {
	Write(ctx context.Context, ev map[string]any) error
}

// ─── Pay (钱包余额支付到商户) ────────────────────────────────────────

// PayRequest ...
type PayRequest struct {
	OwnerType   string // customer / merchant
	OwnerID     string
	Currency    string
	AmountMinor int64
	MerchantID  string // 收款商户
	OrderID     string // 关联订单
	Description string
}

// Pay 钱包支付到商户.
//
// 流程:
//   1. 检查 wallet status (active)
//   2. 查 accounting 余额, 不够直接 fail
//   3. 写 pending Transaction
//   4. accounting AtomicBatch:
//        DEBIT  cust_wallet/{owner}/{currency}  amount
//        CREDIT merc_collected/{merchant_id}    amount
//   5. 更新 Transaction completed + voucher_no
func (s *Service) Pay(ctx context.Context, req PayRequest) (*domain.Transaction, error) {
	if req.AmountMinor <= 0 {
		return nil, errors.New("amount must be positive")
	}
	w, err := s.Wallets.Get(ctx, req.OwnerType, req.OwnerID)
	if err != nil {
		return nil, fmt.Errorf("wallet: %w", err)
	}
	if w.Status != domain.StatusActive {
		return nil, fmt.Errorf("wallet not active: %s", w.Status)
	}

	srcAcct := domain.AccountID(req.OwnerType, req.OwnerID, req.Currency)

	// 余额检查 (早 fail; accounting 还会再校验)
	bal, err := s.balance(ctx, srcAcct, req.Currency)
	if err != nil {
		return nil, fmt.Errorf("balance: %w", err)
	}
	if bal < req.AmountMinor {
		return nil, fmt.Errorf("insufficient balance: have %d, need %d", bal, req.AmountMinor)
	}

	txn := &domain.Transaction{
		TxnID:        genID("txn_"),
		OwnerType:    req.OwnerType,
		OwnerID:      req.OwnerID,
		Type:         domain.TypePay,
		Currency:     req.Currency,
		AmountMinor:  req.AmountMinor,
		Direction:    "out",
		Counterparty: "merchant:" + req.MerchantID,
		RefID:        req.OrderID,
		Status:       domain.TxStatusPending,
		CreatedAt:    s.now(),
	}
	if err := s.Txns.Save(ctx, txn); err != nil {
		return nil, err
	}

	resp, err := s.Acct.AtomicBatchBooking(ctx, &accountingv1.AtomicBatchBookingRequest{
		BatchRequestId:  txn.TxnID,
		BatchBusinessNo: txn.TxnID,
		Description:     fmt.Sprintf("wallet pay txn=%s", txn.TxnID),
		Requests: []*accountingv1.AtomicBatchBookingEntry{{
			RequestId:  txn.TxnID + "-main",
			BusinessNo: txn.TxnID,
			Currency:   req.Currency,
			Description: req.Description,
			Entries: []*accountingv1.AccountingEntry{
				{AccountId: srcAcct, Amount: strconv.FormatInt(req.AmountMinor, 10),
					Direction: accountingv1.Direction_DIRECTION_DEBIT},
				{AccountId: "merc_collected/" + req.MerchantID,
					Amount: strconv.FormatInt(req.AmountMinor, 10),
					Direction: accountingv1.Direction_DIRECTION_CREDIT},
			},
		}},
	})
	if err != nil || !resp.AllSuccess {
		txn.Status = domain.TxStatusFailed
		txn.FailureReason = errMsg(err, resp)
		_ = s.Txns.Save(ctx, txn)
		return txn, errors.New(txn.FailureReason)
	}

	if len(resp.Results) > 0 {
		txn.VoucherNo = resp.Results[0].VoucherNo
	}
	txn.Status = domain.TxStatusCompleted
	// HIGH-FIX-3: 之前 _ = Txns.Save 吞错; accounting 已 AtomicBatchBooking 落账 (钱真动了)
	// 但本地 txn 仍 Pending → 余额查询/对账时跟 accounting 不一致, 用户投诉时查不到完成时间.
	// 失败 propagate, caller 拿到 (txn, err) 已知 voucher_no, 走对账修复 (booking 已成功 不能回滚).
	if err := s.Txns.Save(ctx, txn); err != nil {
		return txn, fmt.Errorf("CRITICAL: accounting booking succeeded (voucher=%s) but txn save failed; "+
			"manual reconciliation required: %w", txn.VoucherNo, err)
	}
	s.audit(ctx, txn)
	return txn, nil
}

// ─── Transfer (钱包间转账) ───────────────────────────────────────────

// TransferRequest ...
type TransferRequest struct {
	FromOwnerType, FromOwnerID string
	ToOwnerType,   ToOwnerID   string
	Currency                   string
	AmountMinor                int64
	Memo                       string
}

// Transfer 钱包到钱包. 单 batch 原子.
func (s *Service) Transfer(ctx context.Context, req TransferRequest) (*domain.Transaction, error) {
	if req.AmountMinor <= 0 {
		return nil, errors.New("amount must be positive")
	}
	src, err := s.Wallets.Get(ctx, req.FromOwnerType, req.FromOwnerID)
	if err != nil {
		return nil, fmt.Errorf("source wallet: %w", err)
	}
	if src.Status != domain.StatusActive {
		return nil, fmt.Errorf("source frozen: %s", src.Status)
	}
	dst, err := s.Wallets.Get(ctx, req.ToOwnerType, req.ToOwnerID)
	if err != nil {
		return nil, fmt.Errorf("dest wallet: %w", err)
	}
	if dst.Status != domain.StatusActive {
		return nil, fmt.Errorf("dest frozen: %s", dst.Status)
	}

	srcAcct := domain.AccountID(req.FromOwnerType, req.FromOwnerID, req.Currency)
	dstAcct := domain.AccountID(req.ToOwnerType, req.ToOwnerID, req.Currency)

	bal, _ := s.balance(ctx, srcAcct, req.Currency)
	if bal < req.AmountMinor {
		return nil, fmt.Errorf("insufficient: have %d need %d", bal, req.AmountMinor)
	}

	txn := &domain.Transaction{
		TxnID: genID("txn_"), OwnerType: req.FromOwnerType, OwnerID: req.FromOwnerID,
		Type: domain.TypeTransfer, Currency: req.Currency, AmountMinor: req.AmountMinor,
		Direction: "out", Counterparty: req.ToOwnerType + ":" + req.ToOwnerID,
		Status: domain.TxStatusPending, CreatedAt: s.now(),
	}
	// HIGH-FIX-3: Pending 落盘失败直接返错; 没 txn 行就不调 booking, 避免 booking 完了
	// 找不到对应 txn 行的孤儿. (跟 Pay 起手 Save 同款行为, 不再 swallow).
	if err := s.Txns.Save(ctx, txn); err != nil {
		return nil, fmt.Errorf("save pending transfer txn: %w", err)
	}

	resp, err := s.Acct.AtomicBatchBooking(ctx, &accountingv1.AtomicBatchBookingRequest{
		BatchRequestId:  txn.TxnID,
		BatchBusinessNo: txn.TxnID,
		Description:     "wallet transfer: " + req.Memo,
		Requests: []*accountingv1.AtomicBatchBookingEntry{{
			RequestId:  txn.TxnID,
			BusinessNo: txn.TxnID,
			Currency:   req.Currency,
			Entries: []*accountingv1.AccountingEntry{
				{AccountId: srcAcct, Amount: strconv.FormatInt(req.AmountMinor, 10),
					Direction: accountingv1.Direction_DIRECTION_DEBIT},
				{AccountId: dstAcct, Amount: strconv.FormatInt(req.AmountMinor, 10),
					Direction: accountingv1.Direction_DIRECTION_CREDIT},
			},
		}},
	})
	if err != nil || !resp.AllSuccess {
		txn.Status = domain.TxStatusFailed
		txn.FailureReason = errMsg(err, resp)
		_ = s.Txns.Save(ctx, txn)
		return txn, errors.New(txn.FailureReason)
	}
	if len(resp.Results) > 0 {
		txn.VoucherNo = resp.Results[0].VoucherNo
	}
	txn.Status = domain.TxStatusCompleted
	// HIGH-FIX-3: 跟 Pay 同款: booking 成功后 Save 失败 propagate, 防余额错乱.
	if err := s.Txns.Save(ctx, txn); err != nil {
		return txn, fmt.Errorf("CRITICAL: transfer booking succeeded (voucher=%s) but src txn save failed; "+
			"manual reconciliation required: %w", txn.VoucherNo, err)
	}

	// 也给 dst 钱包记一条 in 方向 (便于历史查询)
	dstTxn := *txn
	dstTxn.ID = 0
	dstTxn.TxnID = txn.TxnID + "-in"
	dstTxn.OwnerType = req.ToOwnerType
	dstTxn.OwnerID = req.ToOwnerID
	dstTxn.Direction = "in"
	dstTxn.Counterparty = req.FromOwnerType + ":" + req.FromOwnerID
	// dst 仅是历史镜像 (booking 已落完整双边), 失败 log 但不阻断 — src 完成已足够.
	if err := s.Txns.Save(ctx, &dstTxn); err != nil {
		// 失败用 audit 记录, 让 ops 知道 dst 历史镜像缺失需补.
		s.audit(ctx, &domain.Transaction{
			TxnID: dstTxn.TxnID, OwnerType: dstTxn.OwnerType, OwnerID: dstTxn.OwnerID,
			Type: domain.TypeTransfer, Status: domain.TxStatusFailed,
			FailureReason: "dst history mirror save failed: " + err.Error(),
			CreatedAt:     s.now(),
		})
	}

	s.audit(ctx, txn)
	return txn, nil
}

// ─── Freeze / Unfreeze ──────────────────────────────────────────────

// Freeze 冻结钱包 (合规 / 异常检测触发).
func (s *Service) Freeze(ctx context.Context, ownerType, ownerID, reason string) error {
	w, err := s.Wallets.Get(ctx, ownerType, ownerID)
	if err != nil {
		return err
	}
	w.Status = domain.StatusFrozen
	now := s.now()
	w.FrozenAt = &now
	w.FrozenReason = reason
	_ = s.Wallets.Save(ctx, w)
	s.audit(ctx, &domain.Transaction{
		TxnID: genID("frz_"), OwnerType: ownerType, OwnerID: ownerID,
		Type: domain.TypeFreeze, Status: domain.TxStatusCompleted,
		FailureReason: reason, CreatedAt: now,
	})
	return nil
}

// ─── balance helper ───────────────────────────────────────────────

func (s *Service) balance(ctx context.Context, account, currency string) (int64, error) {
	resp, err := s.Acct.GetBalanceSnapshot(ctx, &accountingv1.GetBalanceSnapshotRequest{
		AccountId: account, Currency: currency,
	})
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(resp.Snapshot.GetBalance(), 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Balances 查所有币种余额 (multi-currency wallet view)。
func (s *Service) Balances(ctx context.Context, ownerType, ownerID string, currencies []string) ([]domain.Balance, error) {
	out := []domain.Balance{}
	for _, ccy := range currencies {
		acc := domain.AccountID(ownerType, ownerID, ccy)
		amt, err := s.balance(ctx, acc, ccy)
		if err != nil {
			continue // 该币种没账户, 跳
		}
		out = append(out, domain.Balance{
			Currency: ccy, AvailableMinor: amt, TotalMinor: amt,
		})
	}
	return out, nil
}

// ─── helpers ─────────────────────────────────────────────────────

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Service) audit(ctx context.Context, txn *domain.Transaction) {
	if s.Audit == nil {
		return
	}
	_ = s.Audit.Write(ctx, map[string]any{
		"action":   "wallet." + txn.Type,
		"txn_id":   txn.TxnID,
		"owner":    txn.OwnerType + ":" + txn.OwnerID,
		"amount":   txn.AmountMinor,
		"currency": txn.Currency,
		"voucher":  txn.VoucherNo,
		"status":   txn.Status,
	})
}

func errMsg(err error, resp *accountingv1.AtomicBatchBookingResponse) string {
	if err != nil {
		return err.Error()
	}
	if resp != nil && !resp.AllSuccess {
		return fmt.Sprintf("partial-fail: %s", resp.Message)
	}
	return "unknown"
}

func genID(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}
