package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"github.com/xiongwp/payment-util/shadow"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BalanceSnapshotRepository 账户余额快照仓储接口
type BalanceSnapshotRepository interface {
	// Upsert inserts or updates a balance snapshot for a specific shard.
	// Called by day_cut_service which already knows dbIndex and tableIndex.
	// Uses gorm clause.OnConflict to upsert on (account_no, snapshot_date).
	Upsert(ctx context.Context, dbIndex, tableIndex int, snapshot *model.AccountBalanceSnapshot) error
	// UpsertBatch inserts or updates a batch of balance snapshots for a specific shard.
	// Uses CreateInBatches to reduce round trips; batchSize controls records per INSERT.
	// 语义：覆盖（OVERWRITE）现有 snapshot 字段。日切非分段模式（旧路径）使用此方法。
	UpsertBatch(ctx context.Context, dbIndex, tableIndex int, snapshots []*model.AccountBalanceSnapshot) error
	// UpsertBatchIncremental 增量 upsert：用于日切分段（chunked）模式。
	//
	// 语义（与 UpsertBatch 不同）：
	//   - 首次 INSERT：所有字段从 VALUES 写入，包括 beginning_balance（账户在本 run 中的期初）
	//   - ON DUPLICATE KEY：
	//       total_debit       += VALUES(total_debit)
	//       total_credit      += VALUES(total_credit)
	//       transaction_count += VALUES(transaction_count)
	//       ending_balance     = VALUES(ending_balance)        -- 始终覆盖为本 chunk 最后一笔的 BalanceAfter
	//       last_transaction_id = VALUES(last_transaction_id) -- 始终覆盖为本 chunk 最后一笔
	//       beginning_balance  = beginning_balance              -- 保留（仅首次 INSERT 写入）
	//
	// 用法：每个 chunk 处理完调一次，传入"本 chunk 内每个账户的局部统计"。
	// 多 chunk 累积后，total_debit/total_credit/count 是真值；ending_balance 是最新一笔的 BalanceAfter。
	UpsertBatchIncremental(ctx context.Context, dbIndex, tableIndex int, snapshots []*model.AccountBalanceSnapshot) error
	// UpsertBatchIncrementalTx 同 UpsertBatchIncremental 但在给定事务上执行（原子 chunk commit 用）。
	UpsertBatchIncrementalTx(tx *gorm.DB, tableIndex int, snapshots []*model.AccountBalanceSnapshot) error
	// FinalizeShardSnapshot 在所有 chunk 处理完后，更新 buffered booking 账户的
	// beginning_balance / ending_balance 修正值（fixBufferedAccountBalances 用）。
	// 直接 UPDATE WHERE account_no IN (...) AND snapshot_date = ? AND run_id = ?。
	UpdateBufferedFinal(ctx context.Context, dbIndex, tableIndex int, snapshotDate string, runID int, accountNo string, beginningBalance, endingBalance int64) error
	// GetByAccountAndDate queries snapshot by account_no and optional date.
	// If date is empty, returns the latest snapshot ordered by snapshot_date DESC.
	// Returns nil if not found.
	//
	// 警告：未过滤 run_id，多 run 场景下命中哪一行不确定。需要精确指定 run 的调用方
	// 应当使用 GetByAccountDateRun。
	GetByAccountAndDate(ctx context.Context, accountNo, date string) (*model.AccountBalanceSnapshot, error)
	// GetByAccountDateRun 精确按 (account_no, snapshot_date, run_id) 命中一行。
	// 用于 day-cut finalize 阶段：必须读到本 run 自己的 snapshot 修正 buffered 账户的
	// beginning/ending；若用 GetByAccountAndDate 在多 run 历史下会随机命中旧 run 的快照。
	GetByAccountDateRun(ctx context.Context, accountNo, date string, runID int) (*model.AccountBalanceSnapshot, error)
	// GetLatest returns the most recent snapshot for an account.
	GetLatest(ctx context.Context, accountNo string) (*model.AccountBalanceSnapshot, error)
	// ListDistinctDates returns all distinct snapshot_dates in the given shard, ordered DESC.
	ListDistinctDates(ctx context.Context, dbIndex, tableIndex int) ([]string, error)

	// ListByCutAndRun 列出某分片本次 (cutDate, runID) 的所有 snapshot 行（按 account_no 字典序）。
	// 用于 sealRunSnapshots：取 total_debit/total_credit 计算 canonical begin/end。
	ListByCutAndRun(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int) ([]*model.AccountBalanceSnapshot, error)

	// GetPrevEnding 返回该账户在 < beforeDate 的最近一次成功 snapshot 的 ending_balance。
	// 找不到（首日 / 该账户从未 cut）→ found=false, ending=0。仅看 status=COMPLETED 的 run？
	// 注：snapshot 表本身没有 status 字段（status 在 day_cut_control）；这里
	// 简化为"最近 snapshot_date < beforeDate 的同一账户行"。Day-cut 写 snapshot 是
	// 整个 run 完成时才写，部分写不会发生（chunk + commit 原子）。
	GetPrevEnding(ctx context.Context, dbIndex, tableIndex int, accountNo, beforeDate string) (ending int64, found bool, err error)

	// SetBeginAndEndTx 在给定事务上批量更新本次 (cutDate, runID) 的
	// beginning_balance / ending_balance。sealRunSnapshots 用：
	// 一次 UPDATE 应用到全部账户。每行带 account_no，CASE WHEN account_no=? THEN ? ELSE col END。
	SetBeginAndEnd(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int,
		begs map[string]int64, ends map[string]int64) error
}

type balanceSnapshotRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewBalanceSnapshotRepository 创建账户余额快照仓储
func NewBalanceSnapshotRepository(dbManager *database.Manager, router *sharding.Router) BalanceSnapshotRepository {
	return &balanceSnapshotRepository{
		dbManager: dbManager,
		router:    router,
	}
}

// Upsert inserts or updates a single balance snapshot, conflicting on (account_no, snapshot_date, run_id).
func (r *balanceSnapshotRepository) Upsert(ctx context.Context, dbIndex, tableIndex int, snapshot *model.AccountBalanceSnapshot) error {
	tableName := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("balance_snapshot Upsert: get db[%d]: %w", dbIndex, err)
	}

	return db.WithContext(ctx).Table(tableName).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "account_no"},
				{Name: "snapshot_date"},
				{Name: "run_id"},
			},
			DoUpdates: clause.AssignmentColumns([]string{
				"beginning_balance",
				"ending_balance",
				"total_debit",
				"total_credit",
				"transaction_count",
				"last_transaction_id",
			}),
		}).
		Create(snapshot).Error
}

// UpsertBatch inserts or updates a slice of balance snapshots in a single call.
// Snapshots are written in batches of 500 rows to keep individual INSERTs small.
// Idempotent: conflicts on (account_no, snapshot_date, run_id) trigger an UPDATE.
func (r *balanceSnapshotRepository) UpsertBatch(ctx context.Context, dbIndex, tableIndex int, snapshots []*model.AccountBalanceSnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}
	tableName := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("balance_snapshot UpsertBatch: get db[%d]: %w", dbIndex, err)
	}

	const batchSize = 500
	return db.WithContext(ctx).Table(tableName).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "account_no"},
				{Name: "snapshot_date"},
				{Name: "run_id"},
			},
			DoUpdates: clause.AssignmentColumns([]string{
				"beginning_balance",
				"ending_balance",
				"total_debit",
				"total_credit",
				"transaction_count",
				"last_transaction_id",
			}),
		}).
		CreateInBatches(snapshots, batchSize).Error
}

// UpsertBatchIncremental 增量 upsert（断点续跑核心写法）。详见接口注释。
//
// 实现：
//   使用 GORM clause.OnConflict + clause.Assignments 表达式，把 ON DUPLICATE KEY 的
//   UPDATE 子句写成 col = col + VALUES(col) 而非 col = VALUES(col)。
//   beginning_balance 不出现在 UPDATE 子句里，所以仅首次 INSERT 时来自 VALUES。
//
// 注意：此方法假定 snapshots 中每条都已是"本 chunk 内该账户的局部统计"，
// 不要传入跨 chunk 累积值，否则会重复累加。
func (r *balanceSnapshotRepository) UpsertBatchIncremental(ctx context.Context, dbIndex, tableIndex int, snapshots []*model.AccountBalanceSnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}
	tableName := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("balance_snapshot UpsertBatchIncremental: get db[%d]: %w", dbIndex, err)
	}

	const batchSize = 500
	return db.WithContext(ctx).Table(tableName).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "account_no"},
				{Name: "snapshot_date"},
				{Name: "run_id"},
			},
			DoUpdates: clause.Assignments(map[string]interface{}{
				// 累加：本 chunk 的局部 debit/credit/count → 加到已存在的累计值
				"total_debit":         gorm.Expr("total_debit + VALUES(total_debit)"),
				"total_credit":        gorm.Expr("total_credit + VALUES(total_credit)"),
				"transaction_count":   gorm.Expr("transaction_count + VALUES(transaction_count)"),
				// 覆盖：本 chunk 最后一笔 BalanceAfter / TransactionID 即"截至此 chunk 的最新值"
				"ending_balance":      gorm.Expr("VALUES(ending_balance)"),
				"last_transaction_id": gorm.Expr("VALUES(last_transaction_id)"),
				// beginning_balance 故意不在 DoUpdates → 只在首次 INSERT 时写入，后续 chunk 保留首次值
			}),
		}).
		CreateInBatches(snapshots, batchSize).Error
}

// UpsertBatchIncrementalTx 同 UpsertBatchIncremental 但在给定 tx 上执行。
// day-cut atomicChunkCommit 用：snapshot upsert + cursor advance 在同一事务里防崩溃后重复累加。
func (r *balanceSnapshotRepository) UpsertBatchIncrementalTx(tx *gorm.DB, tableIndex int, snapshots []*model.AccountBalanceSnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}
	// tx 由调用方 (db.WithContext(ctx).Transaction(...)) 传入；ctx 已附在 tx.Statement.Context 上。
	tableName := shadow.TableName(tx.Statement.Context, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))
	const batchSize = 500
	return tx.Table(tableName).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "account_no"},
				{Name: "snapshot_date"},
				{Name: "run_id"},
			},
			DoUpdates: clause.Assignments(map[string]interface{}{
				"total_debit":         gorm.Expr("total_debit + VALUES(total_debit)"),
				"total_credit":        gorm.Expr("total_credit + VALUES(total_credit)"),
				"transaction_count":   gorm.Expr("transaction_count + VALUES(transaction_count)"),
				"ending_balance":      gorm.Expr("VALUES(ending_balance)"),
				"last_transaction_id": gorm.Expr("VALUES(last_transaction_id)"),
			}),
		}).
		CreateInBatches(snapshots, batchSize).Error
}

// UpdateBufferedFinal 单账户 finalize 更新（fixBufferedAccountBalances 用）。
// 单条而非批量，因为 buffered 账户通常 < 100 个/分片，调用频率低不必为之优化批量。
func (r *balanceSnapshotRepository) UpdateBufferedFinal(ctx context.Context, dbIndex, tableIndex int, snapshotDate string, runID int, accountNo string, beginningBalance, endingBalance int64) error {
	tableName := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("balance_snapshot UpdateBufferedFinal: get db[%d]: %w", dbIndex, err)
	}
	return db.WithContext(ctx).Table(tableName).
		Where("account_no = ? AND snapshot_date = ? AND run_id = ?", accountNo, snapshotDate, runID).
		Updates(map[string]interface{}{
			"beginning_balance": beginningBalance,
			"ending_balance":    endingBalance,
		}).Error
}

// GetByAccountAndDate queries snapshot by account_no and optional date.
// If date is empty, returns the latest snapshot ordered by snapshot_date DESC.
//
// 路由到只读副本（snapshot 是日切产物，写入后小时级稳定，复制延迟无业务影响）。
// GetReadDB 在未配置 read_dsn 时回落到主库，不影响行为。
func (r *balanceSnapshotRepository) GetByAccountAndDate(ctx context.Context, accountNo, date string) (*model.AccountBalanceSnapshot, error) {
	dbIndex, tableIndex := r.router.RouteByAccountNo(accountNo)
	tableName := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))

	db, err := r.dbManager.GetReadDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("balance_snapshot GetByAccountAndDate: get db[%d]: %w", dbIndex, err)
	}

	query := db.WithContext(ctx).Table(tableName).Where("account_no = ?", accountNo)
	if date != "" {
		query = query.Where("snapshot_date = ?", date)
	} else {
		query = query.Order("snapshot_date DESC")
	}

	var snapshot model.AccountBalanceSnapshot
	result := query.First(&snapshot)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("balance_snapshot GetByAccountAndDate: %w", result.Error)
	}

	return &snapshot, nil
}

// ListDistinctDates returns distinct snapshot_dates in the given shard, ordered DESC.
// 走只读副本：日期列表是聚合查询（DISTINCT），且 snapshot 表数据稳定。
func (r *balanceSnapshotRepository) ListDistinctDates(ctx context.Context, dbIndex, tableIndex int) ([]string, error) {
	tableName := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))

	db, err := r.dbManager.GetReadDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("balance_snapshot ListDistinctDates: get db[%d]: %w", dbIndex, err)
	}

	var dates []string
	err = db.WithContext(ctx).Table(tableName).
		Distinct("snapshot_date").
		Order("snapshot_date DESC").
		Pluck("snapshot_date", &dates).Error
	if err != nil {
		return nil, fmt.Errorf("balance_snapshot ListDistinctDates: %w", err)
	}
	return dates, nil
}

// GetByAccountDateRun 精确查询 (account_no, snapshot_date, run_id)。
// 走主库：finalize 阶段刚写完 snapshot 立刻读，replica 可能没复制过来。
// 这里 read-after-write 一致性比命中 replica 重要。
func (r *balanceSnapshotRepository) GetByAccountDateRun(ctx context.Context, accountNo, date string, runID int) (*model.AccountBalanceSnapshot, error) {
	dbIndex, tableIndex := r.router.RouteByAccountNo(accountNo)
	tableName := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("balance_snapshot GetByAccountDateRun: get db[%d]: %w", dbIndex, err)
	}

	var snapshot model.AccountBalanceSnapshot
	result := db.WithContext(ctx).Table(tableName).
		Where("account_no = ? AND snapshot_date = ? AND run_id = ?", accountNo, date, runID).
		First(&snapshot)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("balance_snapshot GetByAccountDateRun: %w", result.Error)
	}
	return &snapshot, nil
}

// GetLatest returns the most recent snapshot for an account.
// 走只读副本：snapshot 数据稳定。
func (r *balanceSnapshotRepository) GetLatest(ctx context.Context, accountNo string) (*model.AccountBalanceSnapshot, error) {
	dbIndex, tableIndex := r.router.RouteByAccountNo(accountNo)
	tableName := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))

	db, err := r.dbManager.GetReadDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("balance_snapshot GetLatest: get db[%d]: %w", dbIndex, err)
	}

	var snapshot model.AccountBalanceSnapshot
	result := db.WithContext(ctx).Table(tableName).
		Where("account_no = ?", accountNo).
		Order("snapshot_date DESC").
		First(&snapshot)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("balance_snapshot GetLatest: %w", result.Error)
	}

	return &snapshot, nil
}

// ListByCutAndRun 列出某分片本次 (cutDate, runID) 的所有 snapshot 行（按 account_no 字典序）。
// sealRunSnapshots 用：取每行的 total_debit/total_credit 作为本日 net delta 的来源。
//
// 走主库：sealRunSnapshots 紧跟在 chunk loop 之后调用，replica 可能未复制完。
func (r *balanceSnapshotRepository) ListByCutAndRun(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int) ([]*model.AccountBalanceSnapshot, error) {
	tableName := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("balance_snapshot ListByCutAndRun: get db[%d]: %w", dbIndex, err)
	}

	var rows []*model.AccountBalanceSnapshot
	if err := db.WithContext(ctx).Table(tableName).
		Where("snapshot_date = ? AND run_id = ?", cutDate, runID).
		Order("account_no ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("balance_snapshot ListByCutAndRun: %w", err)
	}
	return rows, nil
}

// GetPrevEnding 返回该账户在 < beforeDate 的最近一次 snapshot 的 ending_balance。
// 找不到（首日 / 该账户从未 cut）→ found=false, ending=0。
//
// 实现：取 snapshot_date < beforeDate 的最大 snapshot_date 行；同 date 多 run 取
// 最大 run_id（重跑修正后的 ending 优先于旧 run）。
func (r *balanceSnapshotRepository) GetPrevEnding(ctx context.Context, dbIndex, tableIndex int, accountNo, beforeDate string) (int64, bool, error) {
	tableName := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return 0, false, fmt.Errorf("balance_snapshot GetPrevEnding: get db[%d]: %w", dbIndex, err)
	}

	var snapshot model.AccountBalanceSnapshot
	result := db.WithContext(ctx).Table(tableName).
		Where("account_no = ? AND snapshot_date < ?", accountNo, beforeDate).
		Order("snapshot_date DESC, run_id DESC").
		First(&snapshot)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("balance_snapshot GetPrevEnding: %w", result.Error)
	}
	return snapshot.EndingBalance, true, nil
}

// SetBeginAndEnd 在本次 (cutDate, runID) 的 snapshot 行上批量更新 beginning_balance /
// ending_balance。sealRunSnapshots 用：把 canonical 计算结果一次性 flush 回去。
//
// 实现：CASE WHEN account_no = ? THEN ? END 拼出大 UPDATE。GORM 不直接支持
// per-row 不同 SET 值的批量 UPDATE，因此用 raw expression。账户数典型 < 10k/分片
// → 单 SQL 可承受；若超大，按 batchSize 分批。
//
// 注：begs / ends 可以包含不同的 keyset。最终 UPDATE 只影响二者并集的账户。
// 对于只在一个 map 中的账户，缺失值视为不变（保留 ON DUPLICATE KEY 写入的中间值）。
// 但实际 sealRunSnapshots 总是同时设置 beginning + ending，所以这里假设 keyset 一致。
func (r *balanceSnapshotRepository) SetBeginAndEnd(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int, begs map[string]int64, ends map[string]int64) error {
	if len(begs) == 0 && len(ends) == 0 {
		return nil
	}
	// 求并集 keyset
	keys := make(map[string]struct{}, len(begs)+len(ends))
	for k := range begs {
		keys[k] = struct{}{}
	}
	for k := range ends {
		keys[k] = struct{}{}
	}
	if len(keys) == 0 {
		return nil
	}

	// shadow 路由：压测流量走 _shadow 副本表，与 callback 路径一致
	tableName := shadow.TableName(ctx, fmt.Sprintf("account_balance_snapshot_%02d", tableIndex))
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("balance_snapshot SetBeginAndEnd: get db[%d]: %w", dbIndex, err)
	}

	// 分批避免单 SQL 过长（MySQL max_allowed_packet 默认 64MB，留余量按 1000 切）。
	const batchSize = 1000
	accountList := make([]string, 0, len(keys))
	for k := range keys {
		accountList = append(accountList, k)
	}

	for start := 0; start < len(accountList); start += batchSize {
		end := start + batchSize
		if end > len(accountList) {
			end = len(accountList)
		}
		batch := accountList[start:end]

		// 构造：UPDATE t SET
		//   beginning_balance = CASE account_no WHEN ? THEN ? ... END,
		//   ending_balance    = CASE account_no WHEN ? THEN ? ... END
		// WHERE account_no IN (...) AND snapshot_date = ? AND run_id = ?
		var begCase, endCase string
		args := make([]interface{}, 0, len(batch)*4+3)
		begCase = "CASE account_no"
		for _, accNo := range batch {
			begCase += " WHEN ? THEN ?"
			args = append(args, accNo, begs[accNo])
		}
		begCase += " END"

		endCase = "CASE account_no"
		for _, accNo := range batch {
			endCase += " WHEN ? THEN ?"
			args = append(args, accNo, ends[accNo])
		}
		endCase += " END"

		args = append(args, batch, cutDate, runID)
		sql := fmt.Sprintf(
			"UPDATE %s SET beginning_balance = %s, ending_balance = %s WHERE account_no IN ? AND snapshot_date = ? AND run_id = ?",
			tableName, begCase, endCase,
		)
		if err := db.WithContext(ctx).Exec(sql, args...).Error; err != nil {
			return fmt.Errorf("balance_snapshot SetBeginAndEnd: %w", err)
		}
	}
	return nil
}
