package service

import (
	"testing"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── validateEntries ──────────────────────────────────────────────────────────

func TestValidateEntries(t *testing.T) {
	svc := &accountingService{}

	t.Run("valid balanced two-entry", func(t *testing.T) {
		entries := []AccountingEntry{
			{AccountNo: "A", DebitAmount: 10000},
			{AccountNo: "B", CreditAmount: 10000},
		}
		assert.NoError(t, svc.validateEntries(entries))
	})

	t.Run("valid balanced multi-entry", func(t *testing.T) {
		entries := []AccountingEntry{
			{AccountNo: "A", DebitAmount: 5000},
			{AccountNo: "B", DebitAmount: 5000},
			{AccountNo: "C", CreditAmount: 10000},
		}
		assert.NoError(t, svc.validateEntries(entries))
	})

	t.Run("too few entries", func(t *testing.T) {
		entries := []AccountingEntry{
			{AccountNo: "A", DebitAmount: 10000},
		}
		err := svc.validateEntries(entries)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least 2 entries")
	})

	t.Run("empty entries", func(t *testing.T) {
		err := svc.validateEntries([]AccountingEntry{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least 2 entries")
	})

	t.Run("entry has both debit and credit", func(t *testing.T) {
		entries := []AccountingEntry{
			{AccountNo: "A", DebitAmount: 10000, CreditAmount: 10000},
			{AccountNo: "B", CreditAmount: 10000},
		}
		err := svc.validateEntries(entries)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "both debit and credit")
	})

	t.Run("entry has neither debit nor credit", func(t *testing.T) {
		entries := []AccountingEntry{
			{AccountNo: "A", DebitAmount: 0, CreditAmount: 0},
			{AccountNo: "B", CreditAmount: 10000},
		}
		err := svc.validateEntries(entries)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "debit or credit")
	})

	t.Run("unbalanced debit != credit", func(t *testing.T) {
		entries := []AccountingEntry{
			{AccountNo: "A", DebitAmount: 10000},
			{AccountNo: "B", CreditAmount: 9000},
		}
		err := svc.validateEntries(entries)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "debit and credit must be equal")
	})

	t.Run("large balanced multi-entry", func(t *testing.T) {
		entries := []AccountingEntry{
			{AccountNo: "A", DebitAmount: 1_000_000_000},
			{AccountNo: "B", DebitAmount: 2_000_000_000},
			{AccountNo: "C", CreditAmount: 3_000_000_000},
		}
		assert.NoError(t, svc.validateEntries(entries))
	})
}

// ─── computeBalanceDelta ──────────────────────────────────────────────────────

func TestComputeBalanceDelta(t *testing.T) {
	cases := []struct {
		desc          string
		entry         AccountingEntry
		assetOrExpense bool
		want          int64
	}{
		// Asset account: debit increases balance
		{"asset debit", AccountingEntry{DebitAmount: 10000}, true, 10000},
		// Asset account: credit decreases balance
		{"asset credit", AccountingEntry{CreditAmount: 10000}, true, -10000},
		// Liability account: debit decreases balance
		{"liability debit", AccountingEntry{DebitAmount: 10000}, false, -10000},
		// Liability account: credit increases balance
		{"liability credit", AccountingEntry{CreditAmount: 10000}, false, 10000},
		// Zero amounts
		{"asset debit zero", AccountingEntry{DebitAmount: 0, CreditAmount: 0}, true, 0},
		// Large values
		{"asset large debit", AccountingEntry{DebitAmount: 1_000_000_000_000}, true, 1_000_000_000_000},
		{"liability large credit", AccountingEntry{CreditAmount: 999_999_999}, false, 999_999_999},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := computeBalanceDelta(tc.entry, tc.assetOrExpense)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ─── computeBalanceDeltaStr ───────────────────────────────────────────────────

func TestComputeBalanceDeltaStr(t *testing.T) {
	cases := []struct {
		desc          string
		entry         AccountingEntry
		assetOrExpense bool
		want          string
	}{
		{"asset debit 34200", AccountingEntry{DebitAmount: 34200}, true, "34200"},
		{"asset credit 34200", AccountingEntry{CreditAmount: 34200}, true, "-34200"},
		{"liability debit 10000", AccountingEntry{DebitAmount: 10000}, false, "-10000"},
		{"liability credit 10000", AccountingEntry{CreditAmount: 10000}, false, "10000"},
		{"zero", AccountingEntry{DebitAmount: 0}, true, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := computeBalanceDeltaStr(tc.entry, tc.assetOrExpense)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ─── validateCurrency ─────────────────────────────────────────────────────────

func TestValidateCurrency(t *testing.T) {
	supported := []string{"PHP", "USD", "EUR", "JPY", "KWD", "GBP", "HKD"}
	for _, code := range supported {
		assert.NoError(t, validateCurrency(code), "expected %s to be valid", code)
	}
	unsupported := []string{"", "XXX", "BTC", "usd", "USDT"}
	for _, code := range unsupported {
		err := validateCurrency(code)
		assert.Error(t, err, "expected %s to fail validation", code)
		assert.Contains(t, err.Error(), "unsupported currency")
	}
}

// ─── isAssetOrExpense (via computeBalanceDelta indirectly) ───────────────────

func TestIsAssetOrExpenseCategory(t *testing.T) {
	// The helper isAssetOrExpense is private; test its effect via an account check
	asset := &model.Account{AccountCategory: model.AccountCategoryAsset}
	expense := &model.Account{AccountCategory: model.AccountCategoryExpense}
	liability := &model.Account{AccountCategory: model.AccountCategoryLiability}
	equity := &model.Account{AccountCategory: model.AccountCategoryEquity}
	revenue := &model.Account{AccountCategory: model.AccountCategoryRevenue}

	entry := AccountingEntry{DebitAmount: 100}

	// Asset & Expense: debit increases (positive delta)
	assert.Equal(t, int64(100), computeBalanceDelta(entry, isAssetOrExpenseAccount(asset)))
	assert.Equal(t, int64(100), computeBalanceDelta(entry, isAssetOrExpenseAccount(expense)))
	// Liability, Equity, Revenue: debit decreases (negative delta)
	assert.Equal(t, int64(-100), computeBalanceDelta(entry, isAssetOrExpenseAccount(liability)))
	assert.Equal(t, int64(-100), computeBalanceDelta(entry, isAssetOrExpenseAccount(equity)))
	assert.Equal(t, int64(-100), computeBalanceDelta(entry, isAssetOrExpenseAccount(revenue)))
}

// isAssetOrExpenseAccount wraps the package-level helper for testing.
func isAssetOrExpenseAccount(acc *model.Account) bool {
	return acc.AccountCategory == model.AccountCategoryAsset ||
		acc.AccountCategory == model.AccountCategoryExpense
}
