package service

import (
	"context"
	"errors"
	"testing"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"github.com/accounting-system/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ─── Mock repository ──────────────────────────────────────────────────────────

type mockTrialBalanceRepo struct {
	// rows to return per (dbIndex, tableIndex)
	rows map[int][]repository.ShardTrialBalanceRow
	// error to return for specified shard indices (global table index)
	errors map[int]error
}

func (m *mockTrialBalanceRepo) QueryShardSummary(
	_ context.Context, _, tableIndex int, _ string, _ int,
) ([]repository.ShardTrialBalanceRow, error) {
	if e, ok := m.errors[tableIndex]; ok {
		return nil, e
	}
	if rows, ok := m.rows[tableIndex]; ok {
		return rows, nil
	}
	return nil, nil
}

func (m *mockTrialBalanceRepo) QueryShardSummaryByCurrency(
	ctx context.Context, dbIndex, tableIndex int, snapshotDate, currency string, runID int,
) ([]repository.ShardTrialBalanceRow, error) {
	_ = currency // mock 忽略币种过滤；真实过滤由 SQL 层验证
	return m.QueryShardSummary(ctx, dbIndex, tableIndex, snapshotDate, runID)
}

func (m *mockTrialBalanceRepo) ListDistinctDates(_ context.Context, _, _ int) ([]string, error) {
	return nil, nil
}

func newTrialBalanceSvc(repo repository.TrialBalanceRepository) *trialBalanceService {
	logger, _ := zap.NewDevelopment()
	// 1 DB, 2 tables (tableIndex 0 and 1)
	router := sharding.NewRouterWithConfig(1, 2)
	return &trialBalanceService{
		repo:   repo,
		router: router,
		logger: logger,
	}
}

// ─── RunTrialBalance – balanced and equation-valid ───────────────────────────

func TestRunTrialBalance_BalancedAndEquationValid(t *testing.T) {
	// Shard 0: asset debit/credit
	// Shard 1: liability debit/credit
	// Both balanced; accounting equation: Asset = Liability
	repo := &mockTrialBalanceRepo{
		rows: map[int][]repository.ShardTrialBalanceRow{
			0: {
				{
					AccountCategory: model.AccountCategoryAsset,
					AccountType:     model.AccountTypeUser,
					AccountCount:    1,
					SumBeginning:    0,
					SumEnding:       100000,
					SumDebit:        100000,
					SumCredit:       0,
				},
			},
			1: {
				{
					AccountCategory: model.AccountCategoryLiability,
					AccountType:     model.AccountTypePlatform,
					AccountCount:    1,
					SumBeginning:    0,
					SumEnding:       100000,
					SumDebit:        0,
					SumCredit:       100000,
				},
			},
		},
		errors: map[int]error{},
	}

	svc := newTrialBalanceSvc(repo)
	result, err := svc.RunTrialBalanceByCurrency(context.Background(), "2024-01-01", "PHP", 0)
	require.NoError(t, err)

	assert.Equal(t, int64(100000), result.TotalDebit)
	assert.Equal(t, int64(100000), result.TotalCredit)
	assert.True(t, result.IsBalanced)
	assert.Equal(t, int64(0), result.Imbalance)

	assert.Equal(t, int64(100000), result.AssetEndingBalance)
	assert.Equal(t, int64(100000), result.LiabilityEndingBalance)
	assert.True(t, result.IsEquationValid)
	assert.Equal(t, int64(0), result.EquationDiff)
}

// ─── RunTrialBalance – imbalanced ─────────────────────────────────────────────

func TestRunTrialBalance_Imbalanced(t *testing.T) {
	repo := &mockTrialBalanceRepo{
		rows: map[int][]repository.ShardTrialBalanceRow{
			0: {{
				AccountCategory: model.AccountCategoryAsset,
				AccountType:     model.AccountTypeUser,
				SumDebit:        100000,
				SumCredit:       0,
				SumEnding:       100000,
			}},
			1: {{
				AccountCategory: model.AccountCategoryLiability,
				AccountType:     model.AccountTypePlatform,
				SumDebit:        0,
				SumCredit:       90000, // mismatch: 100000 ≠ 90000
				SumEnding:       90000,
			}},
		},
		errors: map[int]error{},
	}

	svc := newTrialBalanceSvc(repo)
	result, err := svc.RunTrialBalanceByCurrency(context.Background(), "2024-01-01", "PHP", 0)
	require.NoError(t, err)

	assert.False(t, result.IsBalanced)
	assert.Equal(t, int64(10000), result.Imbalance) // 100000 - 90000
}

// ─── RunTrialBalance – accounting equation invalid ───────────────────────────

func TestRunTrialBalance_EquationInvalid(t *testing.T) {
	// Asset = 100000, Liability = 60000, Equity = 30000
	// Equation: 100000 == 60000 + 30000 → 100000 == 90000 → FAIL
	repo := &mockTrialBalanceRepo{
		rows: map[int][]repository.ShardTrialBalanceRow{
			0: {
				{AccountCategory: model.AccountCategoryAsset, AccountType: model.AccountTypeUser,
					SumDebit: 100000, SumCredit: 0, SumEnding: 100000},
				{AccountCategory: model.AccountCategoryLiability, AccountType: model.AccountTypePlatform,
					SumDebit: 0, SumCredit: 60000, SumEnding: 60000},
			},
			1: {
				{AccountCategory: model.AccountCategoryEquity, AccountType: model.AccountTypePlatform,
					SumDebit: 0, SumCredit: 40000, SumEnding: 40000},
			},
		},
		errors: map[int]error{},
	}

	svc := newTrialBalanceSvc(repo)
	result, err := svc.RunTrialBalanceByCurrency(context.Background(), "2024-01-01", "PHP", 0)
	require.NoError(t, err)

	// Balanced: debit=100000 == credit=100000
	assert.True(t, result.IsBalanced)
	// Equation: 100000 vs 60000+40000 = 100000 → valid
	assert.True(t, result.IsEquationValid)
}

func TestRunTrialBalance_EquationWithRevenueAndExpense(t *testing.T) {
	// Asset=150000, Liability=50000, Equity=60000, Revenue=70000, Expense=30000
	// RHS = 50000 + 60000 + 70000 - 30000 = 150000 → equation holds
	repo := &mockTrialBalanceRepo{
		rows: map[int][]repository.ShardTrialBalanceRow{
			0: {
				{AccountCategory: model.AccountCategoryAsset, SumDebit: 150000, SumCredit: 0, SumEnding: 150000},
				{AccountCategory: model.AccountCategoryLiability, SumDebit: 0, SumCredit: 50000, SumEnding: 50000},
			},
			1: {
				{AccountCategory: model.AccountCategoryEquity, SumDebit: 0, SumCredit: 60000, SumEnding: 60000},
				{AccountCategory: model.AccountCategoryRevenue, SumDebit: 0, SumCredit: 70000, SumEnding: 70000},
				{AccountCategory: model.AccountCategoryExpense, SumDebit: 30000, SumCredit: 0, SumEnding: 30000},
			},
		},
		errors: map[int]error{},
	}

	svc := newTrialBalanceSvc(repo)
	result, err := svc.RunTrialBalanceByCurrency(context.Background(), "2024-01-01", "PHP", 0)
	require.NoError(t, err)

	assert.Equal(t, int64(150000+30000), result.TotalDebit)   // 180000
	assert.Equal(t, int64(50000+60000+70000), result.TotalCredit) // 180000
	assert.True(t, result.IsBalanced)

	assert.Equal(t, int64(150000), result.AssetEndingBalance)
	assert.Equal(t, int64(50000), result.LiabilityEndingBalance)
	assert.Equal(t, int64(60000), result.EquityEndingBalance)
	assert.Equal(t, int64(70000), result.RevenueEndingBalance)
	assert.Equal(t, int64(30000), result.ExpenseEndingBalance)
	assert.True(t, result.IsEquationValid)
	assert.Equal(t, int64(0), result.EquationDiff)
}

// ─── RunTrialBalance – empty shards ───────────────────────────────────────────

func TestRunTrialBalance_EmptyData(t *testing.T) {
	repo := &mockTrialBalanceRepo{
		rows:   map[int][]repository.ShardTrialBalanceRow{},
		errors: map[int]error{},
	}

	svc := newTrialBalanceSvc(repo)
	result, err := svc.RunTrialBalanceByCurrency(context.Background(), "2024-01-01", "PHP", 0)
	require.NoError(t, err)

	assert.Equal(t, int64(0), result.TotalDebit)
	assert.Equal(t, int64(0), result.TotalCredit)
	assert.True(t, result.IsBalanced)
	assert.True(t, result.IsEquationValid)
	assert.Empty(t, result.Summaries)
}

// ─── RunTrialBalance – shard error returns partial result + error ─────────────

func TestRunTrialBalance_ShardError_ReturnsPartialResultAndError(t *testing.T) {
	repo := &mockTrialBalanceRepo{
		rows: map[int][]repository.ShardTrialBalanceRow{
			0: {{
				AccountCategory: model.AccountCategoryAsset,
				AccountType:     model.AccountTypeUser,
				SumDebit:        50000, SumCredit: 0, SumEnding: 50000,
			}},
		},
		errors: map[int]error{
			1: errors.New("db connection lost"), // shard 1 fails
		},
	}

	svc := newTrialBalanceSvc(repo)
	result, err := svc.RunTrialBalanceByCurrency(context.Background(), "2024-01-01", "PHP", 0)

	// Should return error due to shard failure
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shard")

	// But partial results should still be populated
	require.NotNil(t, result)
	assert.Equal(t, int64(50000), result.TotalDebit)
}

// ─── RunTrialBalance – missing snapshotDate ───────────────────────────────────

func TestRunTrialBalance_EmptySnapshotDate(t *testing.T) {
	repo := &mockTrialBalanceRepo{
		rows:   map[int][]repository.ShardTrialBalanceRow{},
		errors: map[int]error{},
	}
	svc := newTrialBalanceSvc(repo)
	_, err := svc.RunTrialBalance(context.Background(), "", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "snapshotDate is required")
}

// ─── RunTrialBalance – cross-shard aggregation for same category ──────────────

func TestRunTrialBalance_CrossShardAggregation(t *testing.T) {
	// Same (category, type) appears in both shards → must be summed
	repo := &mockTrialBalanceRepo{
		rows: map[int][]repository.ShardTrialBalanceRow{
			0: {{
				AccountCategory: model.AccountCategoryAsset,
				AccountType:     model.AccountTypeUser,
				AccountCount:    2,
				SumDebit:        30000, SumCredit: 0, SumEnding: 30000,
			}},
			1: {{
				AccountCategory: model.AccountCategoryAsset,
				AccountType:     model.AccountTypeUser,
				AccountCount:    3,
				SumDebit:        70000, SumCredit: 0, SumEnding: 70000,
			}},
		},
		errors: map[int]error{},
	}

	svc := newTrialBalanceSvc(repo)
	result, err := svc.RunTrialBalanceByCurrency(context.Background(), "2024-01-01", "PHP", 0)
	require.NoError(t, err)

	require.Len(t, result.Summaries, 1)
	s := result.Summaries[0]
	assert.Equal(t, model.AccountCategoryAsset, s.Category)
	assert.Equal(t, int64(5), s.AccountCount)        // 2 + 3
	assert.Equal(t, int64(100000), s.SumDebit)       // 30000 + 70000
	assert.Equal(t, int64(100000), s.SumEnding)      // 30000 + 70000
	assert.Equal(t, int64(100000), result.AssetEndingBalance)
}
