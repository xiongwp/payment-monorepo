package domain

import (
	"errors"
	"time"
)

// ErrLedgerUnbalanced posting's debits ≠ credits.
var ErrLedgerUnbalanced = errors.New("ledger entries unbalanced")

// ErrLedgerAccountNotFound account lookup miss.
var ErrLedgerAccountNotFound = errors.New("ledger account not found")

// ErrLedgerOptimisticLock version bump failed.
var ErrLedgerOptimisticLock = errors.New("ledger account concurrent update")

// AccountType ledger account classification.
type AccountType string

const (
	AccountAsset     AccountType = "asset"
	AccountLiability AccountType = "liability"
	AccountRevenue   AccountType = "revenue"
	AccountExpense   AccountType = "expense"
	AccountEquity    AccountType = "equity"
)

// AccountOwnerType scope of the account.
type AccountOwnerType string

const (
	OwnerPlatform AccountOwnerType = "platform"
	OwnerMerchant AccountOwnerType = "merchant"
	OwnerChannel  AccountOwnerType = "channel"
)

// GLAccount chart-of-accounts row (meta DB, non-sharded).
type GLAccount struct {
	ID            string           `gorm:"column:id;primaryKey;type:varchar(64)"  json:"id"`
	Name          string           `gorm:"column:name;type:varchar(128)"          json:"name"`
	Type          AccountType      `gorm:"column:type;type:varchar(16)"           json:"type"`
	OwnerType     AccountOwnerType `gorm:"column:owner_type;type:varchar(16)"     json:"owner_type"`
	OwnerID       string           `gorm:"column:owner_id;type:varchar(64)"       json:"owner_id,omitempty"`
	Currency      string           `gorm:"column:currency;type:char(3)"           json:"currency"`
	DebitBalance  int64            `gorm:"column:debit_balance"                   json:"debit_balance"`
	CreditBalance int64            `gorm:"column:credit_balance"                  json:"credit_balance"`
	Version       int64            `gorm:"column:version"                         json:"version"`
	Status        string           `gorm:"column:status;type:varchar(16)"         json:"status"`
	Metadata      Metadata         `gorm:"column:metadata;type:json"              json:"metadata,omitempty"`
	Created       time.Time        `gorm:"column:created"                         json:"created"`
	Updated       time.Time        `gorm:"column:updated"                         json:"updated"`
}

// TableName GORM
func (GLAccount) TableName() string { return "gl_account" }

// NetBalance computes the "natural direction" balance: for asset/expense
// accounts net = debit - credit (positive = more asset / expense), for
// liability/equity/revenue net = credit - debit (positive = more owed / equity / income).
func (a *GLAccount) NetBalance() int64 {
	switch a.Type {
	case AccountAsset, AccountExpense:
		return a.DebitBalance - a.CreditBalance
	default:
		return a.CreditBalance - a.DebitBalance
	}
}

// GLTransaction parent of a group of entries that sum to zero.
type GLTransaction struct {
	ID          string    `gorm:"column:id;primaryKey;type:varchar(64)"  json:"id"`
	EventType   string    `gorm:"column:event_type;type:varchar(64)"     json:"event_type"`
	RefType     string    `gorm:"column:ref_type;type:varchar(32)"       json:"ref_type,omitempty"`
	RefID       string    `gorm:"column:ref_id;type:varchar(64)"         json:"ref_id,omitempty"`
	TotalDebit  int64     `gorm:"column:total_debit"                     json:"total_debit"`
	TotalCredit int64     `gorm:"column:total_credit"                    json:"total_credit"`
	Memo        string    `gorm:"column:memo;type:varchar(512)"          json:"memo,omitempty"`
	Reverses    string    `gorm:"column:reverses;type:varchar(64)"       json:"reverses,omitempty"`
	Created     time.Time `gorm:"column:created"                         json:"created"`
}

// TableName GORM
func (GLTransaction) TableName() string { return "gl_transaction" }

// GLEntry immutable double-entry line item.
type GLEntry struct {
	ID            int64     `gorm:"column:id;primaryKey;autoIncrement"     json:"id"`
	TxnID         string    `gorm:"column:txn_id;type:varchar(64)"         json:"txn_id"`
	AccountID     string    `gorm:"column:account_id;type:varchar(64)"     json:"account_id"`
	DebitAmount   int64     `gorm:"column:debit_amount"                    json:"debit_amount"`
	CreditAmount  int64     `gorm:"column:credit_amount"                   json:"credit_amount"`
	Currency      string    `gorm:"column:currency;type:char(3)"           json:"currency"`
	Memo          string    `gorm:"column:memo;type:varchar(512)"          json:"memo,omitempty"`
	Created       time.Time `gorm:"column:created"                         json:"created"`
}

// TableName GORM
func (GLEntry) TableName() string { return "gl_entry" }

// PostingLine input to Post() describing a single debit or credit leg.
type PostingLine struct {
	AccountID string
	Debit     int64 // exactly one of Debit/Credit > 0
	Credit    int64
	Memo      string
}

// PostingRequest input to Post() describing a complete transaction.
type PostingRequest struct {
	EventType string
	RefType   string
	RefID     string
	Memo      string
	Lines     []PostingLine
}

// Validate ensures Post() preconditions: at least 2 lines, debit_sum =
// credit_sum, each line is single-sided and positive.
func (p *PostingRequest) Validate() error {
	if p == nil {
		return ErrValidation
	}
	if len(p.Lines) < 2 {
		return ErrLedgerUnbalanced
	}
	var d, c int64
	for _, l := range p.Lines {
		if l.AccountID == "" {
			return ErrValidation
		}
		if (l.Debit > 0 && l.Credit > 0) || (l.Debit == 0 && l.Credit == 0) {
			return ErrValidation
		}
		if l.Debit < 0 || l.Credit < 0 {
			return ErrValidation
		}
		d += l.Debit
		c += l.Credit
	}
	if d != c {
		return ErrLedgerUnbalanced
	}
	return nil
}
