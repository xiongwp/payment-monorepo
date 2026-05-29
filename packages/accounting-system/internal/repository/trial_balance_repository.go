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
	AccountCategory     model.AccountCategory `gorm:"column:account_category"`
	AccountType         model.AccountType     `gorm:"column:account_type"`
	AccountBusinessType int                   `gorm:"column:account_business_type"`
	AccountCount        int64                 `gorm:"column:account_count"`
	SumBeginning        int64                 `gorm:"column:sum_beginning"`
	SumEnding           int64                 `gorm:"column:sum_ending"`
	SumDebit            int64                 `gorm:"column:sum_debit"`
	SumCredit           int64                 `gorm:"column:sum_credit"`
}

// AccountBalanceRow holds one concrete account's balance for the drill-down view.
// 下钻第 4 层（具体 account_no）。Amount fields are in ISO minor unit × 100.
type AccountBalanceRow struct {
	AccountNo           string            `gorm:"column:account_no"`
	AccountType         model.AccountType `gorm:"column:account_type"`
	AccountBusinessType int               `gorm:"column:account_business_type"`
	Beginning           int64             `gorm:"column:beginning"`
	Ending              int64             `gorm:"column:ending"`
	Debit               int64             `gorm:"column:debit"`
	Credit              int64             `gorm:"column:credit"`
	Balance             int64             `gorm:"column:balance"`
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

	// QueryShardLiveSummary 不 JOIN snapshot，直接对当前 account_XX 表的 balance 聚合，
	// 按 (account_category, account_type, account_business_type) 分组。
	// live 没有期初/借贷流水概念：sum_ending = SUM(balance)，beginning/debit/credit 恒为 0。
	QueryShardLiveSummary(ctx context.Context, dbIndex, tableIndex int, currency string) ([]ShardTrialBalanceRow, error)

	// QueryShardDrilldown 下钻到具体账户级别，返回每个 account_no 的余额。
	// snapshotDate 空 → 查 live（account 表 balance）；非空 → 查 snapshot 表 ending/beginning/debit/credit。
	// category / accountType / businessType 任意组合作为 WHERE 过滤（空 / <=0 表示不过滤）。
	QueryShardDrilldown(ctx context.Context, dbIndex, tableIndex int, currency, category string, accountType, businessType int, snapshotDate string, runID int) ([]AccountBalanceRow, error)
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

// QueryShardLiveSummary 实时试算：直接对 account_XX 当前 balance 聚合，不依赖 day-cut snapshot。
// 与 snapshot 试算的关键区别：live 只有"当前余额"快照，没有期初余额、本期借贷流水，
// 因此 sum_beginning / sum_debit / sum_credit 一律为 0，sum_ending = SUM(balance)。
// GROUP BY 比 snapshot 多 account_business_type 这一层（第 3 层）。
func (r *trialBalanceRepository) QueryShardLiveSummary(ctx context.Context, dbIndex, tableIndex int, currency string) ([]ShardTrialBalanceRow, error) {
	// shadow 路由：压测流量自动加 _shadow 后缀。
	accountTable := shadow.TableName(ctx, fmt.Sprintf("account_%02d", tableIndex))

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("trial_balance QueryShardLiveSummary: get db[%d]: %w", dbIndex, err)
	}

	where := []string{"status != ?"}
	args := []interface{}{int8(model.AccountStatusDisabled)}
	if currency != "" {
		where = append(where, "currency = ?")
		args = append(args, currency)
	}

	//nolint:gosec // table name derived from controlled integer index, not user input.
	query := fmt.Sprintf(`
			SELECT
				account_category,
				account_type,
				account_business_type,
				COUNT(*)       AS account_count,
				0              AS sum_beginning,
				SUM(balance)   AS sum_ending,
				0              AS sum_debit,
				0              AS sum_credit
			FROM %s
			WHERE %s
			GROUP BY account_category, account_type, account_business_type
		`, accountTable, strings.Join(where, " AND "))

	var rows []ShardTrialBalanceRow
	if err := db.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("trial_balance QueryShardLiveSummary db[%d] table[%02d]: %w", dbIndex, tableIndex, err)
	}
	return rows, nil
}

// drilldownLimit 单分片下钻返回上限，防止热门 (category,type) 单片返回过多撑爆内存。
const drilldownLimit = 1000

// QueryShardDrilldown 第 4 层下钻：返回具体 account_no 的余额。
// snapshotDate 空 → 查 live（account 表）；非空 → 查 snapshot 表（JOIN account 取维度列）。
func (r *trialBalanceRepository) QueryShardDrilldown(ctx context.Context, dbIndex, tableIndex int, currency, category string, accountType, businessType int, snapshotDate string, runID int) ([]AccountBalanceRow, error) {
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("trial_balance QueryShardDrilldown: get db[%d]: %w", dbIndex, err)
	}

	accountTable := shadow.TableName(ctx, fmt.Sprintf("account_%02d", tableIndex))

	var query string
	where := []string{}
	args := []interface{}{}

	if snapshotDate == "" {
		// live：直接读 account 表当前 balance。
		where = append(where, "a.status != ?")
		args = append(args, int8(model.AccountStatusDisabled))
		if currency != "" {
			where = append(where, "a.currency = ?")
			args = append(args, currency)
		}
		if category != "" {
			where = append(where, "a.account_category = ?")
			args = append(args, category)
		}
		if accountType > 0 {
			where = append(where, "a.account_type = ?")
			args = append(args, accountType)
		}
		if businessType > 0 {
			where = append(where, "a.account_business_type = ?")
			args = append(args, businessType)
		}
		//nolint:gosec // table name derived from controlled integer index, not user input.
		query = fmt.Sprintf(`
				SELECT
					a.account_no             AS account_no,
					a.account_type           AS account_type,
					a.account_business_type  AS account_business_type,
					0                        AS beginning,
					a.balance                AS ending,
					0                        AS debit,
					0                        AS credit,
					a.balance                AS balance
				FROM %s a
				WHERE %s
				LIMIT %d
			`, accountTable, strings.Join(where, " AND "), drilldownLimit)
	} else {
		// snapshot：JOIN snapshot + account，取 ending/beginning/debit/credit。
		snapshotTable := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))
		where = append(where, "s.snapshot_date = ?")
		args = append(args, snapshotDate)
		if runID > 0 {
			where = append(where, "s.run_id = ?")
			args = append(args, runID)
		}
		if currency != "" {
			where = append(where, "s.currency = ?")
			args = append(args, currency)
		}
		if category != "" {
			where = append(where, "a.account_category = ?")
			args = append(args, category)
		}
		if accountType > 0 {
			where = append(where, "a.account_type = ?")
			args = append(args, accountType)
		}
		if businessType > 0 {
			where = append(where, "a.account_business_type = ?")
			args = append(args, businessType)
		}
		//nolint:gosec // table names derived from controlled integer indices, not user input.
		query = fmt.Sprintf(`
				SELECT
					a.account_no             AS account_no,
					a.account_type           AS account_type,
					a.account_business_type  AS account_business_type,
					s.beginning_balance      AS beginning,
					s.ending_balance         AS ending,
					s.total_debit            AS debit,
					s.total_credit           AS credit,
					s.ending_balance         AS balance
				FROM %s s
				INNER JOIN %s a ON s.account_no = a.account_no
				WHERE %s
				LIMIT %d
			`, snapshotTable, accountTable, strings.Join(where, " AND "), drilldownLimit)
	}

	var rows []AccountBalanceRow
	if err := db.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("trial_balance QueryShardDrilldown db[%d] table[%02d]: %w", dbIndex, tableIndex, err)
	}
	return rows, nil
}
