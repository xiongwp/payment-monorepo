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
	Category model.AccountCategory `json:"category"`
	Type     model.AccountType     `json:"type"`
	// BusinessType 是第 3 层维度（account_business_type）。
	// snapshot 试算（RunTrialBalanceByCurrency）不下钻到该层，恒为 0；
	// live 试算（RunLiveTrialBalance）按该层分组，非 0。
	BusinessType int `json:"business_type"`
	// Level 标记本行汇总到哪一层："category_type"（2 层）或 "category_type_business"（3 层）。
	Level        string `json:"level"`
	AccountCount int64  `json:"account_count"`
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

	// RunLiveTrialBalance 实时试算：不依赖 day-cut snapshot，直接对 account 表当前
	// balance 跨 100 分片聚合，按 (category, type, business_type) 三层汇总。
	// SnapshotDate 置为 "live"。仍跑会计恒等式校验（资产 = 负债+权益+收入-费用），
	// live 下用 ending 余额方向校验（无期初/借贷流水）。
	RunLiveTrialBalance(ctx context.Context, currency string) (*TrialBalanceResult, error)

	// Drilldown 第 4 层下钻：跨分片返回具体 account_no 明细，按 |balance| 降序排（找大额异常账户）。
	// snapshotDate 空 → live；非空 → 查该日 snapshot。category/accountType/businessType 任意组合过滤。
	Drilldown(ctx context.Context, currency, category string, accountType, businessType int, snapshotDate string, runID int) ([]*AccountBalanceDetail, error)
}

// AccountBalanceDetail 下钻明细（第 4 层）。Amount fields are in ISO minor unit × 100.
type AccountBalanceDetail struct {
	AccountNo           string            `json:"account_no"`
	AccountType         model.AccountType `json:"account_type"`
	AccountBusinessType int               `json:"account_business_type"`
	Beginning           int64             `json:"beginning"`
	Ending              int64             `json:"ending"`
	Debit               int64             `json:"debit"`
	Credit              int64             `json:"credit"`
	Balance             int64             `json:"balance"`
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
					Level:        "category_type",
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

// liveKey uniquely identifies a (category, type, business_type) triple for live trial balance.
type liveKey struct {
	category model.AccountCategory
	typ      model.AccountType
	bizType  int
}

// RunLiveTrialBalance 实时试算。复用 RunTrialBalanceByCurrency 的并发扇出模式
// （concurrency=20）跨 100 分片调 QueryShardLiveSummary，聚合后跑会计恒等式校验。
//
// 与 snapshot 试算的关键区别：
//   - 数据源是 account 表当前 balance，不 JOIN snapshot；
//   - 没有期初余额 / 本期借贷流水，SumBeginning/SumDebit/SumCredit 恒为 0；
//   - 借贷平衡（IsBalanced）在 live 下退化为恒成立（debit==credit==0）；
//   - 会计恒等式用绝对期末余额方向校验：资产 == 负债+权益+收入-费用。
func (s *trialBalanceService) RunLiveTrialBalance(ctx context.Context, currency string) (*TrialBalanceResult, error) {
	if currency == "" {
		return nil, fmt.Errorf("trial_balance: currency is required (per-currency execution only)")
	}

	s.logger.Info("live trial balance started", zap.String("currency", currency))

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
			rows, err := s.repo.QueryShardLiveSummary(ctx, shard.DBIndex, shard.TableIndex, currency)
			results[i] = shardResult{rows: rows, err: err}
		}()
	}
	wg.Wait()

	aggregated := make(map[liveKey]*CategorySummary)
	var shardErrors int
	for i, res := range results {
		if res.err != nil {
			shardErrors++
			shard := shards[i]
			s.logger.Warn("live_trial_balance: shard query failed",
				zap.Int("dbIndex", shard.DBIndex),
				zap.Int("tableIndex", shard.TableIndex),
				zap.Error(res.err))
			continue
		}
		for _, row := range res.rows {
			k := liveKey{category: row.AccountCategory, typ: row.AccountType, bizType: row.AccountBusinessType}
			if cur, ok := aggregated[k]; ok {
				cur.AccountCount += row.AccountCount
				cur.SumEnding += row.SumEnding
			} else {
				aggregated[k] = &CategorySummary{
					Category:     row.AccountCategory,
					Type:         row.AccountType,
					BusinessType: row.AccountBusinessType,
					Level:        "category_type_business",
					AccountCount: row.AccountCount,
					SumEnding:    row.SumEnding,
				}
			}
		}
	}

	summaries := make([]*CategorySummary, 0, len(aggregated))
	for _, v := range aggregated {
		summaries = append(summaries, v)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Category != summaries[j].Category {
			return string(summaries[i].Category) < string(summaries[j].Category)
		}
		if summaries[i].Type != summaries[j].Type {
			return summaries[i].Type < summaries[j].Type
		}
		return summaries[i].BusinessType < summaries[j].BusinessType
	})

	result := &TrialBalanceResult{
		SnapshotDate: "live",
		Summaries:    summaries,
	}

	for _, summary := range summaries {
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

	// live 没有借贷流水：debit == credit == 0，借贷平衡恒成立。
	result.TotalDebit = 0
	result.TotalCredit = 0
	result.Imbalance = 0
	result.IsBalanced = true

	// 会计恒等式：用绝对期末余额方向校验（live 无期初，净变动即余额本身）。
	result.EquationDiff = result.AssetEndingBalance -
		(result.LiabilityEndingBalance + result.EquityEndingBalance + result.RevenueEndingBalance - result.ExpenseEndingBalance)
	result.IsEquationValid = result.EquationDiff == 0

	s.logger.Info("live trial balance completed",
		zap.String("currency", currency),
		zap.Int("shardErrors", shardErrors),
		zap.Bool("isEquationValid", result.IsEquationValid),
		zap.String("equationDiff", strconv.FormatInt(result.EquationDiff, 10)),
	)

	if shardErrors > 0 {
		return result, fmt.Errorf("live_trial_balance: %d shard(s) failed; results are partial", shardErrors)
	}
	return result, nil
}

// Drilldown 第 4 层下钻：跨 100 分片扇出调 QueryShardDrilldown，合并后按 |balance| 降序。
func (s *trialBalanceService) Drilldown(ctx context.Context, currency, category string, accountType, businessType int, snapshotDate string, runID int) ([]*AccountBalanceDetail, error) {
	if currency == "" {
		return nil, fmt.Errorf("trial_balance: currency is required")
	}

	shards := s.router.GetAllShards()
	type shardResult struct {
		rows []repository.AccountBalanceRow
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
			rows, err := s.repo.QueryShardDrilldown(ctx, shard.DBIndex, shard.TableIndex, currency, category, accountType, businessType, snapshotDate, runID)
			results[i] = shardResult{rows: rows, err: err}
		}()
	}
	wg.Wait()

	details := make([]*AccountBalanceDetail, 0)
	var shardErrors int
	for i, res := range results {
		if res.err != nil {
			shardErrors++
			shard := shards[i]
			s.logger.Warn("drilldown: shard query failed",
				zap.Int("dbIndex", shard.DBIndex),
				zap.Int("tableIndex", shard.TableIndex),
				zap.Error(res.err))
			continue
		}
		for _, row := range res.rows {
			details = append(details, &AccountBalanceDetail{
				AccountNo:           row.AccountNo,
				AccountType:         row.AccountType,
				AccountBusinessType: row.AccountBusinessType,
				Beginning:           row.Beginning,
				Ending:              row.Ending,
				Debit:               row.Debit,
				Credit:              row.Credit,
				Balance:             row.Balance,
			})
		}
	}

	// 按 |balance| 降序：异常大额账户排最前，方便人工排查。
	sort.Slice(details, func(i, j int) bool {
		return absInt64(details[i].Balance) > absInt64(details[j].Balance)
	})

	if shardErrors > 0 {
		return details, fmt.Errorf("drilldown: %d shard(s) failed; results are partial", shardErrors)
	}
	return details, nil
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
