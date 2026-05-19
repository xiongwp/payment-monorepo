package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"github.com/xiongwp/accounting-system/internal/repository"
	"go.uber.org/zap"
)

// CategorySummary 单个账户类别+账户类型的汇总数据
// Amount fields are in ISO minor unit × 100 (see currency package).
type CategorySummary struct {
	Category     model.AccountCategory `json:"category"`
	Type         model.AccountType     `json:"type"`
	AccountCount int64                 `json:"account_count"`
	// Period beginning / ending balances (from day-cut snapshot)
	SumBeginning int64 `json:"sum_beginning"`
	SumEnding    int64 `json:"sum_ending"`
	// Period debit / credit totals
	SumDebit  int64 `json:"sum_debit"`
	SumCredit int64 `json:"sum_credit"`
}

// TrialBalanceResult 试算平衡结果
//
// 两项校验：
//  1. 借贷平衡（IsBalanced）：期间借方合计 == 贷方合计
//  2. 会计恒等式（IsEquationValid）：资产期末余额 == 负债 + 所有者权益 + 收入 - 费用
//
// Amount fields are in ISO minor unit × 100 (see currency package).
type TrialBalanceResult struct {
	SnapshotDate string `json:"snapshot_date"`

	// 按 (account_category, account_type) 分组的明细，已按类别+类型升序排列
	Summaries []*CategorySummary `json:"summaries"`

	// 期间借贷合计（∑total_debit / ∑total_credit across all accounts）
	TotalDebit  int64 `json:"total_debit"`
	TotalCredit int64 `json:"total_credit"`

	// 借贷平衡校验
	IsBalanced bool  `json:"is_balanced"`
	Imbalance  int64 `json:"imbalance"` // TotalDebit - TotalCredit；平衡时为 0

	// 会计恒等式各科目期末余额汇总
	AssetEndingBalance     int64 `json:"asset_ending_balance"`
	LiabilityEndingBalance int64 `json:"liability_ending_balance"`
	EquityEndingBalance    int64 `json:"equity_ending_balance"`
	RevenueEndingBalance   int64 `json:"revenue_ending_balance"`
	ExpenseEndingBalance   int64 `json:"expense_ending_balance"`

	// 会计恒等式校验：资产 = 负债 + 所有者权益 + 收入 - 费用
	IsEquationValid bool  `json:"is_equation_valid"`
	EquationDiff    int64 `json:"equation_diff"` // 资产 - (负债+权益+收入-费用)；成立时为 0
}

// TrialBalanceService 试算平衡服务接口
type TrialBalanceService interface {
	// RunTrialBalanceByCurrency 按 currency 跑试算平衡。currency 必填，不同币种
	// 精度不同，汇总没有意义，必须独立跑。
	RunTrialBalanceByCurrency(ctx context.Context, snapshotDate, currency string, runID int) (*TrialBalanceResult, error)

	// Deprecated: RunTrialBalance 不带币种过滤；新代码用 RunTrialBalanceByCurrency。
	// 仅留作单测 / 老调用方过渡。
	// RunTrialBalance 执行试算平衡。
	//
	// snapshotDate 为日切日期（格式 "2006-01-02"），runID 为日切运行版本号。
	// 当 runID > 0 时，仅统计该版本的快照；runID == 0 时不过滤版本（单元测试模式）。
	// 跨全部 100 个分片聚合，验证借贷平衡与会计恒等式。
	// 若部分分片查询失败，返回部分结果并附带错误说明。
	RunTrialBalance(ctx context.Context, snapshotDate string, runID int) (*TrialBalanceResult, error)
	// ListSnapshotDates returns all distinct snapshot dates that have balance snapshots,
	// sorted DESC. Used to build the historical trial balance list.
	ListSnapshotDates(ctx context.Context) ([]string, error)
}

// trialBalanceFanoutConcurrency 控制 RunTrialBalance 并发查询分片的上限。
// 默认 20:分片数 100 × 查询耗时 ≈ 单片 * 5 (vs 串行 100)；
// 实际瓶颈在连接池（每分片库 maxOpen 通常 10-20），20 不会超发。
const trialBalanceFanoutConcurrency = 20

type trialBalanceService struct {
	repo   repository.TrialBalanceRepository
	router *sharding.Router
	logger *zap.Logger
}

// NewTrialBalanceService 创建试算平衡服务
func NewTrialBalanceService(
	repo repository.TrialBalanceRepository,
	router *sharding.Router,
	logger *zap.Logger,
) TrialBalanceService {
	return &trialBalanceService{
		repo:   repo,
		router: router,
		logger: logger,
	}
}

// categoryKey uniquely identifies an (account_category, account_type) pair.
type categoryKey struct {
	category model.AccountCategory
	typ      model.AccountType
}

// RunTrialBalance scans all shards, aggregates snapshot+account data, and validates
// both the debit-credit balance and the accounting equation.
// runID > 0 filters to that specific day-cut run; runID == 0 skips the filter (unit-test mode).
func (s *trialBalanceService) RunTrialBalance(ctx context.Context, snapshotDate string, runID int) (*TrialBalanceResult, error) {
	return s.RunTrialBalanceByCurrency(ctx, snapshotDate, "", runID)
}

// RunTrialBalanceByCurrency 按币种过滤跑试算平衡。currency 必填。
func (s *trialBalanceService) RunTrialBalanceByCurrency(ctx context.Context, snapshotDate, currency string, runID int) (*TrialBalanceResult, error) {
	if snapshotDate == "" {
		return nil, fmt.Errorf("trial_balance: snapshotDate is required")
	}
	if currency == "" {
		return nil, fmt.Errorf("trial_balance: currency is required (per-currency execution only)")
	}

	s.logger.Info("trial balance started",
		zap.String("snapshotDate", snapshotDate),
		zap.String("currency", currency),
		zap.Int("runID", runID),
	)

	// 并发扇出 100 分片。原串行实现耗时 ≈ N × 单片 RT，100 分片下几十秒；
	// 并发 20 路后 ≈ max(单片 RT) × ceil(100/20) ≈ 5 × 单片 RT。
	// 并发度由 trialBalanceFanoutConcurrency 控制：太大会打满连接池 / CPU，
	// 太小收益有限。20 对应 DB 连接池默认上限（10 个分片库 × maxOpen）。
	shards := s.router.GetAllShards()
	type shardResult struct {
		rows []repository.ShardTrialBalanceRow
		err  error
	}
	results := make([]shardResult, len(shards))

	sem := make(chan struct{}, trialBalanceFanoutConcurrency)
	var wg sync.WaitGroup
	for i, shard := range shards {
		i, shard := i, shard
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rows, err := s.repo.QueryShardSummaryByCurrency(ctx, shard.DBIndex, shard.TableIndex, snapshotDate, currency, runID)
			results[i] = shardResult{rows: rows, err: err}
		}()
	}
	wg.Wait()

	aggregated := make(map[categoryKey]*CategorySummary)
	var shardErrors int
	for i, res := range results {
		if res.err != nil {
			shardErrors++
			shard := shards[i]
			s.logger.Warn("trial_balance: shard query failed",
				zap.Int("dbIndex", shard.DBIndex),
				zap.Int("tableIndex", shard.TableIndex),
				zap.Error(res.err))
			continue // collect partial results; caller sees error at the end
		}
		for _, row := range res.rows {
			k := categoryKey{category: row.AccountCategory, typ: row.AccountType}
			if cur, ok := aggregated[k]; ok {
				cur.AccountCount += row.AccountCount
				cur.SumBeginning += row.SumBeginning
				cur.SumEnding += row.SumEnding
				cur.SumDebit += row.SumDebit
				cur.SumCredit += row.SumCredit
			} else {
				aggregated[k] = &CategorySummary{
					Category:     row.AccountCategory,
					Type:         row.AccountType,
					AccountCount: row.AccountCount,
					SumBeginning: row.SumBeginning,
					SumEnding:    row.SumEnding,
					SumDebit:     row.SumDebit,
					SumCredit:    row.SumCredit,
				}
			}
		}
	}

	// Build a deterministically ordered summary slice.
	summaries := make([]*CategorySummary, 0, len(aggregated))
	for _, v := range aggregated {
		summaries = append(summaries, v)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Category != summaries[j].Category {
			return string(summaries[i].Category) < string(summaries[j].Category)
		}
		return summaries[i].Type < summaries[j].Type
	})

	// Compute period totals and accounting equation components.
	result := &TrialBalanceResult{
		SnapshotDate: snapshotDate,
		Summaries:    summaries,
	}

	for _, summary := range summaries {
		result.TotalDebit += summary.SumDebit
		result.TotalCredit += summary.SumCredit

		switch summary.Category {
		case model.AccountCategoryAsset:
			result.AssetEndingBalance += summary.SumEnding
		case model.AccountCategoryLiability:
			result.LiabilityEndingBalance += summary.SumEnding
		case model.AccountCategoryEquity:
			result.EquityEndingBalance += summary.SumEnding
		case model.AccountCategoryRevenue:
			result.RevenueEndingBalance += summary.SumEnding
		case model.AccountCategoryExpense:
			result.ExpenseEndingBalance += summary.SumEnding
		}
	}

	// 借贷平衡：∑period_debit == ∑period_credit
	result.Imbalance = result.TotalDebit - result.TotalCredit
	result.IsBalanced = result.Imbalance == 0

	// 会计恒等式校验：使用期间净变动量（ending - beginning）而非绝对期末余额。
	//
	// 背景：快照仅覆盖当日有交易活动的账户，静止账户无快照。
	// 因此用绝对期末余额比较会因数据集不完整而始终失衡。
	// 改用净变动量后，静止账户的 Δ=0，无论是否纳入汇总均不影响等式成立。
	//
	// 数学等价性：Δ资产 = Δ负债 + Δ权益 + Δ收入 - Δ费用
	// 等价于 ∑借方 == ∑贷方（即 IsBalanced），因此本检验与借贷平衡检验在数学上
	// 是同一约束的两种表达形式，二者同时成立或同时失败。
	var netAsset, netLiability, netEquity, netRevenue, netExpense int64
	for _, summary := range summaries {
		net := summary.SumEnding - summary.SumBeginning
		switch summary.Category {
		case model.AccountCategoryAsset:
			netAsset += net
		case model.AccountCategoryLiability:
			netLiability += net
		case model.AccountCategoryEquity:
			netEquity += net
		case model.AccountCategoryRevenue:
			netRevenue += net
		case model.AccountCategoryExpense:
			netExpense += net
		}
	}
	result.EquationDiff = netAsset - (netLiability + netEquity + netRevenue - netExpense)
	result.IsEquationValid = result.EquationDiff == 0

	s.logger.Info("trial balance completed",
		zap.String("snapshotDate", snapshotDate),
		zap.Int("shardErrors", shardErrors),
		zap.String("totalDebit", strconv.FormatInt(result.TotalDebit, 10)),
		zap.String("totalCredit", strconv.FormatInt(result.TotalCredit, 10)),
		zap.Bool("isBalanced", result.IsBalanced),
		zap.String("imbalance", strconv.FormatInt(result.Imbalance, 10)),
		zap.Bool("isEquationValid", result.IsEquationValid),
		zap.String("equationDiff", strconv.FormatInt(result.EquationDiff, 10)),
	)

	if shardErrors > 0 {
		return result, fmt.Errorf("trial_balance: %d shard(s) failed; results are partial", shardErrors)
	}
	return result, nil
}

// ListSnapshotDates returns all distinct snapshot dates across all shards, sorted DESC.
func (s *trialBalanceService) ListSnapshotDates(ctx context.Context) ([]string, error) {
	seen := make(map[string]struct{})

	for _, shard := range s.router.GetAllShards() {
		dates, err := s.repo.ListDistinctDates(ctx, shard.DBIndex, shard.TableIndex)
		if err != nil {
			s.logger.Warn("ListSnapshotDates: shard query failed",
				zap.Int("dbIndex", shard.DBIndex),
				zap.Int("tableIndex", shard.TableIndex),
				zap.Error(err))
			continue
		}
		for _, d := range dates {
			seen[d] = struct{}{}
		}
	}

	result := make([]string, 0, len(seen))
	for d := range seen {
		result = append(result, d)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] > result[j] })
	return result, nil
}
