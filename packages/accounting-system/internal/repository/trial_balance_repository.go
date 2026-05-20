package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"github.com/xiongwp/payment-util/shadow"
)

// ShardTrialBalanceRow holds the aggregated result for one (account_category, account_type)
// group from a single shard's snapshot + account JOIN.
// Amount fields are in ISO minor unit × 100 (see currency package).
type ShardTrialBalanceRow struct {
	AccountCategory model.AccountCategory `gorm:"column:account_category"`
	AccountType     model.AccountType     `gorm:"column:account_type"`
	AccountCount    int64                 `gorm:"column:account_count"`
	SumBeginning    int64                 `gorm:"column:sum_beginning"`
	SumEnding       int64                 `gorm:"column:sum_ending"`
	SumDebit        int64                 `gorm:"column:sum_debit"`
	SumCredit       int64                 `gorm:"column:sum_credit"`
}

// TrialBalanceRepository 试算平衡仓储接口
type TrialBalanceRepository interface {
	// QueryShardSummaryByCurrency 同 QueryShardSummary，currency 非空时过滤到该币种。
	QueryShardSummaryByCurrency(ctx context.Context, dbIndex, tableIndex int, snapshotDate, currency string, runID int) ([]ShardTrialBalanceRow, error)

	// QueryShardSummary JOINs account_balance_snapshot_XX with account_XX for one shard,
	// returning amounts grouped by (account_category, account_type) for the given snapshotDate and runID.
	// When runID == 0, no run_id filter is applied (legacy / unit-test mode).
	QueryShardSummary(ctx context.Context, dbIndex, tableIndex int, snapshotDate string, runID int) ([]ShardTrialBalanceRow, error)
	// ListDistinctDates returns all distinct snapshot_dates in the given shard, ordered DESC.
	ListDistinctDates(ctx context.Context, dbIndex, tableIndex int) ([]string, error)
}

type trialBalanceRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewTrialBalanceRepository 创建试算平衡仓储
func NewTrialBalanceRepository(dbManager *database.Manager, router *sharding.Router) TrialBalanceRepository {
	return &trialBalanceRepository{
		dbManager: dbManager,
		router:    router,
	}
}

// ListDistinctDates returns distinct snapshot_dates in the given shard, ordered DESC.
// 走只读副本：snapshot 数据稳定（日切完成后小时级不变），复制延迟无影响。
func (r *trialBalanceRepository) ListDistinctDates(ctx context.Context, dbIndex, tableIndex int) ([]string, error) {
	tableName := fmt.Sprintf("account_balance_snapshot_%02d", tableIndex)

	db, err := r.dbManager.GetReadDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("trial_balance ListDistinctDates: get db[%d]: %w", dbIndex, err)
	}

	var dates []string
	err = db.WithContext(ctx).Table(tableName).
		Distinct("snapshot_date").
		Order("snapshot_date DESC").
		Pluck("snapshot_date", &dates).Error
	if err != nil {
		return nil, fmt.Errorf("trial_balance ListDistinctDates db[%d] table[%02d]: %w", dbIndex, tableIndex, err)
	}
	return dates, nil
}

// QueryShardSummary queries one shard by joining account_balance_snapshot_XX with account_XX.
// Table names are constructed from controlled integer indices — not user input.
// When runID > 0, filters snapshots to that specific run; when runID == 0, no run filter (legacy mode).
func (r *trialBalanceRepository) QueryShardSummary(ctx context.Context, dbIndex, tableIndex int, snapshotDate string, runID int) ([]ShardTrialBalanceRow, error) {
	return r.QueryShardSummaryByCurrency(ctx, dbIndex, tableIndex, snapshotDate, "", runID)
}

// QueryShardSummaryByCurrency 同 QueryShardSummary 语义，currency 非空时 WHERE 额外加
// `AND s.currency = ?`（snapshot 表有 currency 列；day-cut 按币种跑时写进去）。
func (r *trialBalanceRepository) QueryShardSummaryByCurrency(ctx context.Context, dbIndex, tableIndex int, snapshotDate, currency string, runID int) ([]ShardTrialBalanceRow, error) {
	// shadow 路由：压测流量自动加 _shadow 后缀，与 callback 路径保持一致。
	snapshotTable := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))
	accountTable := shadow.TableName(ctx, fmt.Sprintf("account_%02d", tableIndex))

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("trial_balance QueryShardSummary: get db[%d]: %w", dbIndex, err)
	}

	where := []string{"s.snapshot_date = ?"}
	args := []interface{}{snapshotDate}
	if runID > 0 {
		where = append(where, "s.run_id = ?")
		args = append(args, runID)
	}
	if currency != "" {
		where = append(where, "s.currency = ?")
		args = append(args, currency)
	}

	//nolint:gosec // table names derived from controlled integer indices, not user input.
	query := fmt.Sprintf(`
			SELECT
				a.account_category,
				a.account_type,
				COUNT(*)                 AS account_count,
				SUM(s.beginning_balance) AS sum_beginning,
				SUM(s.ending_balance)    AS sum_ending,
				SUM(s.total_debit)       AS sum_debit,
				SUM(s.total_credit)      AS sum_credit
			FROM %s s
			INNER JOIN %s a ON s.account_no = a.account_no
			WHERE %s
			GROUP BY a.account_category, a.account_type
		`, snapshotTable, accountTable, strings.Join(where, " AND "))

	var rows []ShardTrialBalanceRow
	if err := db.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("trial_balance QueryShardSummary db[%d] table[%02d]: %w", dbIndex, tableIndex, err)
	}
	return rows, nil
}
