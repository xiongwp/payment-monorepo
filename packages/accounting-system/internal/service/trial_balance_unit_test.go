package service

import (
	"context"
	"errors"
	"testing"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"github.com/xiongwp/accounting-system/internal/repository"
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
	// live rows per tableIndex (for QueryShardLiveSummary)
	liveRows map[int][]repository.ShardTrialBalanceRow
	// drilldown rows per tableIndex (for QueryShardDrilldown)
	drilldownRows map[int][]repository.AccountBalanceRow
}

func (m *mockTrialBalanceRepo) QueryShardLiveSummary(
	_ context.Context, _, tableIndex int, _ string,
) ([]repository.ShardTrialBalanceRow, error) {
	if e, ok := m.errors[tableIndex]; ok {
		return nil, e
	}
	if rows, ok := m.liveRows[tableIndex]; ok {
		return rows, nil
	}
	return nil, nil
}

func (m *mockTrialBalanceRepo) QueryShardDrilldown(
	_ context.Context, _, tableIndex int, _, _ string, _, _ int, _ string, _ int,
) ([]repository.AccountBalanceRow, error) {
	if e, ok := m.errors[tableIndex]; ok {
		return nil, e
	}
	if rows, ok := m.drilldownRows[tableIndex]; ok {
		return rows, nil
	}
	return nil, nil
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

// ─── RunTrialBalance – business_type 维度 ─────────────────────────────────────
// 验证同 (category, type) 但不同 business_type 的两条行不会被合并:
// 分类明细表必须按业务类型摊开,跟 live 同维度。
func TestRunTrialBalance_SplitByBusinessType(t *testing.T) {
	repo := &mockTrialBalanceRepo{
		rows: map[int][]repository.ShardTrialBalanceRow{
			0: {
				// 同样是 ASSET × USER,但 business_type 不同 — 应该出两行 summary。
				{
					AccountCategory: model.AccountCategoryAsset, AccountType: model.AccountTypeUser,
					AccountBusinessType: 1, AccountCount: 3,
					SumBeginning: 0, SumEnding: 30000, SumDebit: 30000, SumCredit: 0,
				},
				{
					AccountCategory: model.AccountCategoryAsset, AccountType: model.AccountTypeUser,
					AccountBusinessType: 5, AccountCount: 2,
					SumBeginning: 0, SumEnding: 20000, SumDebit: 20000, SumCredit: 0,
				},
			},
			1: {
				// 同分片同 (category,type,biz) 跨分片应该 SUM 合并 — 验证 key 一致时聚合。
				{
					AccountCategory: model.AccountCategoryAsset, AccountType: model.AccountTypeUser,
					AccountBusinessType: 1, AccountCount: 2,
					SumBeginning: 0, SumEnding: 20000, SumDebit: 20000, SumCredit: 0,
				},
			},
		},
		errors: map[int]error{},
	}

	svc := newTrialBalanceSvc(repo)
	result, err := svc.RunTrialBalanceByCurrency(context.Background(), "2024-01-01", "PHP", 0)
	require.NoError(t, err)

	// 两行 summary:biz_type=1 (跨分片合并) 与 biz_type=5。
	require.Len(t, result.Summaries, 2)

	// 按 (cat, type, biz) 升序:都是 ASSET/USER,所以按 biz 升 -> 1 在前,5 在后。
	biz1 := result.Summaries[0]
	assert.Equal(t, 1, biz1.BusinessType)
	assert.Equal(t, "category_type_business", biz1.Level)
	assert.Equal(t, int64(5), biz1.AccountCount)   // 3 + 2 跨分片合并
	assert.Equal(t, int64(50000), biz1.SumEnding)  // 30000 + 20000
	assert.Equal(t, int64(50000), biz1.SumDebit)

	biz5 := result.Summaries[1]
	assert.Equal(t, 5, biz5.BusinessType)
	assert.Equal(t, int64(2), biz5.AccountCount)
	assert.Equal(t, int64(20000), biz5.SumEnding)
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

// ─── RunLiveTrialBalance – happy path (asset = liability + equity) ─────────────

func TestRunLiveTrialBalance_EquationValid(t *testing.T) {
	// Live: Asset balance 100000 = Liability 60000 + Equity 40000.
	// 注意 live 三层分组（带 business_type）。
	repo := &mockTrialBalanceRepo{
		liveRows: map[int][]repository.ShardTrialBalanceRow{
			0: {
				{AccountCategory: model.AccountCategoryAsset, AccountType: model.AccountTypeTransitChannelReceivable,
					AccountBusinessType: 201, AccountCount: 2, SumEnding: 100000},
			},
			1: {
				{AccountCategory: model.AccountCategoryLiability, AccountType: model.AccountTypeUser,
					AccountBusinessType: 1, AccountCount: 3, SumEnding: 60000},
				{AccountCategory: model.AccountCategoryEquity, AccountType: model.AccountTypePlatform,
					AccountBusinessType: 100, AccountCount: 1, SumEnding: 40000},
			},
		},
		errors: map[int]error{},
	}

	svc := newTrialBalanceSvc(repo)
	result, err := svc.RunLiveTrialBalance(context.Background(), "PHP")
	require.NoError(t, err)

	assert.Equal(t, "live", result.SnapshotDate)
	// live 无借贷流水：借贷平衡恒成立。
	assert.True(t, result.IsBalanced)
	assert.Equal(t, int64(0), result.TotalDebit)
	assert.Equal(t, int64(0), result.TotalCredit)

	assert.Equal(t, int64(100000), result.AssetEndingBalance)
	assert.Equal(t, int64(60000), result.LiabilityEndingBalance)
	assert.Equal(t, int64(40000), result.EquityEndingBalance)
	// 资产 100000 == 负债 60000 + 权益 40000 → equation valid
	assert.True(t, result.IsEquationValid)
	assert.Equal(t, int64(0), result.EquationDiff)

	// 三层维度：business_type / level 已填充。
	require.Len(t, result.Summaries, 3)
	for _, s := range result.Summaries {
		assert.Equal(t, "category_type_business", s.Level)
		assert.NotZero(t, s.BusinessType)
	}
}

// TestRunLiveTrialBalance_InflightNotFalseAlarm 验证"不误报"：一笔双分录部分确认
// （Asset 腿已 Confirm，balance 已 +100000；Liability 腿仍 TRYING，balance 未动但
// PendingNet=+100000）。裸读恒等式不平（EquationDiff=100000），但在途投影后归零。
func TestRunLiveTrialBalance_InflightNotFalseAlarm(t *testing.T) {
	repo := &mockTrialBalanceRepo{
		liveRows: map[int][]repository.ShardTrialBalanceRow{
			0: {
				// Asset 腿已确认：balance 已落 100000，无在途。
				{AccountCategory: model.AccountCategoryAsset, AccountType: model.AccountTypeTransitChannelReceivable,
					AccountBusinessType: 201, AccountCount: 1, SumEnding: 100000, PendingNet: 0, InflightTccCount: 0},
			},
			1: {
				// Liability 腿仍 TRYING：balance 未动（SumEnding=0），在途净额 +100000。
				{AccountCategory: model.AccountCategoryLiability, AccountType: model.AccountTypeUser,
					AccountBusinessType: 1, AccountCount: 1, SumEnding: 0, PendingNet: 100000, InflightTccCount: 1},
			},
		},
		errors: map[int]error{},
	}

	svc := newTrialBalanceSvc(repo)
	result, err := svc.RunLiveTrialBalance(context.Background(), "PHP")
	require.NoError(t, err)

	// 裸读：资产 100000 vs 负债 0 → 不平（这正是会误报的地方）。
	assert.Equal(t, int64(100000), result.AssetEndingBalance)
	assert.Equal(t, int64(0), result.LiabilityEndingBalance)
	assert.Equal(t, int64(100000), result.EquationDiff)
	assert.False(t, result.IsEquationValid)

	// 在途投影后：差额归零 → 健康，不误报。
	assert.True(t, result.IsLive)
	assert.Equal(t, int64(1), result.InflightTccCount)
	assert.Equal(t, int64(-100000), result.InflightEquationContribution)
	assert.Equal(t, int64(0), result.AdjustedEquationDiff)
	assert.True(t, result.IsHealthy)
}

// TestRunLiveTrialBalance_TrulyImbalanced 验证真不平不会被在途掩盖：无在途，但裸读不平
// → AdjustedEquationDiff 仍非 0，IsHealthy=false。
func TestRunLiveTrialBalance_TrulyImbalanced(t *testing.T) {
	repo := &mockTrialBalanceRepo{
		liveRows: map[int][]repository.ShardTrialBalanceRow{
			0: {
				{AccountCategory: model.AccountCategoryAsset, AccountType: model.AccountTypeTransitChannelReceivable,
					AccountBusinessType: 201, AccountCount: 1, SumEnding: 100000, PendingNet: 0, InflightTccCount: 0},
			},
			1: {
				// 负债只有 90000，且无在途 → 真差 10000。
				{AccountCategory: model.AccountCategoryLiability, AccountType: model.AccountTypeUser,
					AccountBusinessType: 1, AccountCount: 1, SumEnding: 90000, PendingNet: 0, InflightTccCount: 0},
			},
		},
		errors: map[int]error{},
	}

	svc := newTrialBalanceSvc(repo)
	result, err := svc.RunLiveTrialBalance(context.Background(), "PHP")
	require.NoError(t, err)

	assert.Equal(t, int64(10000), result.EquationDiff)
	assert.Equal(t, int64(0), result.InflightEquationContribution)
	assert.Equal(t, int64(10000), result.AdjustedEquationDiff)
	assert.False(t, result.IsHealthy) // 没有在途可解释 → 真不平
}

func TestRunLiveTrialBalance_RequiresCurrency(t *testing.T) {
	svc := newTrialBalanceSvc(&mockTrialBalanceRepo{})
	_, err := svc.RunLiveTrialBalance(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "currency is required")
}

// ─── Drilldown – sorted by |balance| desc ─────────────────────────────────────

func TestDrilldown_SortedByAbsBalanceDesc(t *testing.T) {
	repo := &mockTrialBalanceRepo{
		drilldownRows: map[int][]repository.AccountBalanceRow{
			0: {
				{AccountNo: "A-small", AccountType: model.AccountTypeUser, AccountBusinessType: 1, Balance: 500, Ending: 500},
				{AccountNo: "A-bignegative", AccountType: model.AccountTypeUser, AccountBusinessType: 1, Balance: -90000, Ending: -90000},
			},
			1: {
				{AccountNo: "A-big", AccountType: model.AccountTypeUser, AccountBusinessType: 1, Balance: 100000, Ending: 100000},
				{AccountNo: "A-mid", AccountType: model.AccountTypeUser, AccountBusinessType: 1, Balance: 20000, Ending: 20000},
			},
		},
		errors: map[int]error{},
	}

	svc := newTrialBalanceSvc(repo)
	details, err := svc.Drilldown(context.Background(), "PHP", "LIABILITY", 1, 1, "", 0)
	require.NoError(t, err)
	require.Len(t, details, 4)

	// 按 |balance| 降序：100000 > 90000(abs) > 20000 > 500
	assert.Equal(t, "A-big", details[0].AccountNo)
	assert.Equal(t, "A-bignegative", details[1].AccountNo)
	assert.Equal(t, "A-mid", details[2].AccountNo)
	assert.Equal(t, "A-small", details[3].AccountNo)
}

func TestDrilldown_RequiresCurrency(t *testing.T) {
	svc := newTrialBalanceSvc(&mockTrialBalanceRepo{})
	_, err := svc.Drilldown(context.Background(), "", "", 0, 0, "", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "currency is required")
}
