package commonutil

import "github.com/xiongwp/accounting-system/internal/domain/model"

func IsAccountCanNegative(accountType model.AccountType) bool {
	switch accountType {
	case model.AccountTypeUser, model.AccountTypeMerchant, model.AccountTypeMerchantPendingSettle:
		return false
	default:
		return true
	}
}

// IsAssetOrExpense reports whether the account category follows the debit-positive convention.
// For ASSET and EXPENSE accounts debit increases the balance; for others (LIABILITY, EQUITY, REVENUE) credit does.
func IsAssetOrExpense(category model.AccountCategory) bool {
	return category == model.AccountCategoryAsset || category == model.AccountCategoryExpense
}
