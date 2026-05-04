package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/shadow"
)

// ledger 主 / 影子表名常量（避免到处拼字面量）
const (
	tblGLAccount     = "gl_account"
	tblGLTransaction = "gl_transaction"
	tblGLEntry       = "gl_entry"
)

// 内联 helper：按 ctx 解析表名
func glAccount(ctx context.Context) string     { return shadow.TableName(ctx, tblGLAccount) }
func glTransaction(ctx context.Context) string { return shadow.TableName(ctx, tblGLTransaction) }
func glEntry(ctx context.Context) string       { return shadow.TableName(ctx, tblGLEntry) }

// LedgerRepository GL accounts + entries + transactions. All non-sharded
// (meta DB): ledger tables are the canonical source of truth and must be
// globally queryable for reconciliation + audit.
type LedgerRepository interface {
	// Post applies a balanced multi-leg transaction atomically:
	//   1) INSERT gl_transaction
	//   2) INSERT every gl_entry
	//   3) UPDATE gl_account running balances (optimistic version check)
	// Returns the persisted transaction id.
	Post(ctx context.Context, txnID string, req *domain.PostingRequest) (*domain.GLTransaction, error)

	// Accounts
	CreateAccount(ctx context.Context, a *domain.GLAccount) error
	GetAccount(ctx context.Context, id string) (*domain.GLAccount, error)
	ListAccounts(ctx context.Context, ownerType domain.AccountOwnerType, ownerID string, limit, offset int) ([]*domain.GLAccount, int64, error)
	EnsureAccount(ctx context.Context, a *domain.GLAccount) (*domain.GLAccount, error)

	// Entries + transactions (read-side for audit UI)
	ListEntries(ctx context.Context, accountID string, since, until *time.Time, limit, offset int) ([]*domain.GLEntry, int64, error)
	ListTransactions(ctx context.Context, eventType, refType, refID string, limit, offset int) ([]*domain.GLTransaction, int64, error)
	GetTransaction(ctx context.Context, id string) (*domain.GLTransaction, []*domain.GLEntry, error)
}

type ledgerRepo struct{ mgr *Manager }

// NewLedgerRepository 构造
func NewLedgerRepository(mgr *Manager) LedgerRepository {
	return &ledgerRepo{mgr: mgr}
}

func (r *ledgerRepo) db() *gorm.DB { return r.mgr.GetMeta() }

// ─── Post (the heart of the ledger) ─────────────────────────────────────────

func (r *ledgerRepo) Post(ctx context.Context, txnID string, req *domain.PostingRequest) (*domain.GLTransaction, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if txnID == "" {
		return nil, fmt.Errorf("%w: txn_id required", domain.ErrValidation)
	}

	// 解析主 / 影子表（一个 ctx 内固定，避免事务里部分表分歧）
	tblAcct, tblTxn, tblEntry := glAccount(ctx), glTransaction(ctx), glEntry(ctx)

	var out *domain.GLTransaction
	err := r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) account existence + type consistency check
		accounts := make(map[string]*domain.GLAccount, len(req.Lines))
		for _, l := range req.Lines {
			if _, ok := accounts[l.AccountID]; ok {
				continue
			}
			var a domain.GLAccount
			if err := tx.Table(tblAcct).Where("id = ?", l.AccountID).First(&a).Error; errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: %s", domain.ErrLedgerAccountNotFound, l.AccountID)
			} else if err != nil {
				return err
			}
			accounts[l.AccountID] = &a
		}

		// 2) totals
		var total int64
		for _, l := range req.Lines {
			if l.Debit > 0 {
				total += l.Debit
			}
		}

		// 3) INSERT transaction
		txn := &domain.GLTransaction{
			ID:          txnID,
			EventType:   req.EventType,
			RefType:     req.RefType,
			RefID:       req.RefID,
			TotalDebit:  total,
			TotalCredit: total,
			Memo:        req.Memo,
		}
		if err := tx.Table(tblTxn).Create(txn).Error; err != nil {
			return err
		}

		// 4) INSERT entries + update account balances (per account in aggregate to
		//    minimize row-level locking churn)
		perAccountDebit := make(map[string]int64, len(accounts))
		perAccountCredit := make(map[string]int64, len(accounts))
		for _, l := range req.Lines {
			entry := &domain.GLEntry{
				TxnID:        txnID,
				AccountID:    l.AccountID,
				DebitAmount:  l.Debit,
				CreditAmount: l.Credit,
				Currency:     accounts[l.AccountID].Currency,
				Memo:         l.Memo,
			}
			if err := tx.Table(tblEntry).Create(entry).Error; err != nil {
				return err
			}
			perAccountDebit[l.AccountID] += l.Debit
			perAccountCredit[l.AccountID] += l.Credit
		}
		for id, a := range accounts {
			for attempt := 0; attempt < 3; attempt++ {
				res := tx.Table(tblAcct).
					Where("id = ? AND version = ?", id, a.Version).
					Updates(map[string]any{
						"debit_balance":  gorm.Expr("debit_balance + ?", perAccountDebit[id]),
						"credit_balance": gorm.Expr("credit_balance + ?", perAccountCredit[id]),
						"version":        gorm.Expr("version + 1"),
					})
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected == 1 {
					break
				}
				var fresh domain.GLAccount
				if err := tx.Table(tblAcct).Where("id = ?", id).First(&fresh).Error; err != nil {
					return err
				}
				a.Version = fresh.Version
				if attempt == 2 {
					return fmt.Errorf("%w: account %s", domain.ErrLedgerOptimisticLock, id)
				}
			}
		}
		out = txn
		return nil
	})
	return out, err
}

// ─── account CRUD ────────────────────────────────────────────────────────────

func (r *ledgerRepo) CreateAccount(ctx context.Context, a *domain.GLAccount) error {
	if a.ID == "" || a.Name == "" || a.Type == "" || a.OwnerType == "" {
		return fmt.Errorf("%w: id/name/type/owner_type required", domain.ErrValidation)
	}
	if a.Currency == "" {
		a.Currency = "PHP"
	}
	if a.Status == "" {
		a.Status = "active"
	}
	return r.db().WithContext(ctx).Table(glAccount(ctx)).Create(a).Error
}

func (r *ledgerRepo) GetAccount(ctx context.Context, id string) (*domain.GLAccount, error) {
	var a domain.GLAccount
	err := r.db().WithContext(ctx).Table(glAccount(ctx)).Where("id = ?", id).First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrLedgerAccountNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *ledgerRepo) ListAccounts(ctx context.Context, ownerType domain.AccountOwnerType, ownerID string, limit, offset int) ([]*domain.GLAccount, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := r.db().WithContext(ctx).Table(glAccount(ctx))
	if ownerType != "" {
		q = q.Where("owner_type = ?", ownerType)
	}
	if ownerID != "" {
		q = q.Where("owner_id = ?", ownerID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []*domain.GLAccount
	if err := q.Order("id ASC").Limit(limit).Offset(offset).Find(&out).Error; err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// EnsureAccount creates the account if it doesn't exist; returns the (now-
// existing) row. Idempotent; safe to call on every merchant/channel
// provisioning.
func (r *ledgerRepo) EnsureAccount(ctx context.Context, a *domain.GLAccount) (*domain.GLAccount, error) {
	existing, err := r.GetAccount(ctx, a.ID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, domain.ErrLedgerAccountNotFound) {
		return nil, err
	}
	if err := r.CreateAccount(ctx, a); err != nil {
		// Race: another caller created it between Get+Create
		if isDupKey(err) {
			return r.GetAccount(ctx, a.ID)
		}
		return nil, err
	}
	return a, nil
}

// ─── read side ───────────────────────────────────────────────────────────────

func (r *ledgerRepo) ListEntries(ctx context.Context, accountID string, since, until *time.Time, limit, offset int) ([]*domain.GLEntry, int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := r.db().WithContext(ctx).Table(glEntry(ctx))
	if accountID != "" {
		q = q.Where("account_id = ?", accountID)
	}
	if since != nil {
		q = q.Where("created >= ?", *since)
	}
	if until != nil {
		q = q.Where("created < ?", *until)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []*domain.GLEntry
	if err := q.Order("id DESC").Limit(limit).Offset(offset).Find(&out).Error; err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (r *ledgerRepo) ListTransactions(ctx context.Context, eventType, refType, refID string, limit, offset int) ([]*domain.GLTransaction, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := r.db().WithContext(ctx).Table(glTransaction(ctx))
	if eventType != "" {
		q = q.Where("event_type = ?", eventType)
	}
	if refType != "" {
		q = q.Where("ref_type = ?", refType)
	}
	if refID != "" {
		q = q.Where("ref_id = ?", refID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []*domain.GLTransaction
	if err := q.Order("created DESC").Limit(limit).Offset(offset).Find(&out).Error; err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (r *ledgerRepo) GetTransaction(ctx context.Context, id string) (*domain.GLTransaction, []*domain.GLEntry, error) {
	var txn domain.GLTransaction
	if err := r.db().WithContext(ctx).Table(glTransaction(ctx)).Where("id = ?", id).First(&txn).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, fmt.Errorf("transaction not found: %s", id)
	} else if err != nil {
		return nil, nil, err
	}
	var entries []*domain.GLEntry
	if err := r.db().WithContext(ctx).Table(glEntry(ctx)).Where("txn_id = ?", id).Order("id").Find(&entries).Error; err != nil {
		return nil, nil, err
	}
	return &txn, entries, nil
}
