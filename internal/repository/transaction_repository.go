package repository

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
)

// TransactionRepository 交易流水仓储接口
type TransactionRepository interface {
	// CreateTransaction 创建交易流水
	CreateTransaction(ctx context.Context, tx *gorm.DB, transaction *model.AccountTransaction, dbIndex, tableIndex int) error

	// GetTransactionByID 根据交易ID查询流水
	GetTransactionByID(ctx context.Context, transactionID string) (*model.AccountTransaction, error)

	// ListTransactionsByAccountNo 查询账户流水
	ListTransactionsByAccountNo(ctx context.Context, accountNo string, startDate, endDate string, offset, limit int) ([]*model.AccountTransaction, error)

	// ListByCutDateChunk 单 chunk 读取本分片 cut_date=? AND status=SUCCESS [AND currency=?]
	// AND id > fromIDExclusive 的流水，按 id ASC LIMIT chunkLimit。
	//
	// cut_date 由 booking 入口处 computeCutDate 一次性确定并 propagate 到所有 entry +
	// tcc_coordinator —— 同一 voucher 所有 entry 必然共享同一 cut_date，整张凭证或
	// 全入选、或全不入 → 试算平衡天然成立。
	//
	// drain 由调用方在扫描前完成（waitForCoordinatorsDrained 确保 cut_date<=X 的所有
	// CONFIRMING coordinator 都已落定），所以扫描看到的就是 final state。
	ListByCutDateChunk(ctx context.Context, dbIndex, tableIndex int,
		cutDate, currency string,
		fromIDExclusive uint64, chunkLimit int) ([]*model.AccountTransaction, error)

	// ListBufferedAccountNosByCutDate 返回该分片 cut_date=? AND status=SUCCESS
	// AND booking_type=BUFFERED [AND currency=?] 的 distinct account_no。零流水返回 nil。
	ListBufferedAccountNosByCutDate(ctx context.Context, dbIndex, tableIndex int,
		cutDate, currency string) ([]string, error)

	// ListTransactions 分页查询流水，支持按 account_no / business_no / transaction_id + 日期范围过滤。
	// 返回分页结果、总记录数和错误。
	// 规则：
	//   - transaction_id 给出时：精确查单条（total=1），忽略其他过滤条件。
	//   - account_no 给出时：路由到单一分片，高效查询。
	//   - 仅 business_no 给出时：并行扫描所有分片（admin 场景可接受）。
	//   - 未给出任何过滤条件时返回错误（防止全表扫描）。
	// 结果按 transaction_time DESC 排序（最新在前）。
	ListTransactions(ctx context.Context, f ListTransactionFilter) ([]*model.AccountTransaction, int64, error)
}

// ListTransactionFilter 流水查询过滤条件
type ListTransactionFilter struct {
	AccountNo     string
	BusinessNo    string
	TransactionID string
	StartDate     string // YYYY-MM-DD，空时默认最近 90 天
	EndDate       string // YYYY-MM-DD，空时默认今天
	Offset        int
	Limit         int // 默认 30，最大 100
}

type transactionRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewTransactionRepository 创建交易流水仓储
func NewTransactionRepository(dbManager *database.Manager, router *sharding.Router) TransactionRepository {
	return &transactionRepository{
		dbManager: dbManager,
		router:    router,
	}
}

// CreateTransaction 创建交易流水。
//
// 写入前断言：cut_date 必须有值。
// schema 是 NOT NULL DATE，零值 time.Time 经 gorm 序列化得到 '0000-00-00'，
// 配合 trial balance 的 WHERE cut_date='YYYY-MM-DD' 过滤会让该 entry 错失，
// 同 voucher 跨 entry cut_date 不一致 → 借贷过滤后不平。
// 这里在 boundary 强制要求 caller 必须设 cut_date，让 bug 一发生就 fail-fast，
// 而不是悄悄落库等到 trial balance 才发现。
func (r *transactionRepository) CreateTransaction(ctx context.Context, tx *gorm.DB, transaction *model.AccountTransaction, dbIndex, tableIndex int) error {
	if transaction == nil {
		return errors.New("CreateTransaction: nil transaction")
	}
	if transaction.CutDate == "" || transaction.CutDate == "0000-00-00" {
		return fmt.Errorf("CreateTransaction: cut_date must be set on every entry (got %q) — caller bug, "+
			"will silently break trial balance per-day filter", transaction.CutDate)
	}
	tableName := r.router.GetTableName("account_transaction", tableIndex)
	return tx.WithContext(ctx).Table(tableName).Create(transaction).Error
}

// GetTransactionByID 根据交易ID查询流水
func (r *transactionRepository) GetTransactionByID(ctx context.Context, transactionID string) (*model.AccountTransaction, error) {
	dbIndex, tableIndex := r.extractShardInfoFromTransactionID(transactionID)

	tableName := r.router.GetTableName("account_transaction", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, err
	}

	var transaction model.AccountTransaction
	result := db.WithContext(ctx).Table(tableName).Where("transaction_id = ?", transactionID).First(&transaction)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, result.Error
	}

	return &transaction, nil
}

// ListTransactionsByAccountNo 查询账户流水
func (r *transactionRepository) ListTransactionsByAccountNo(ctx context.Context, accountNo string, startDate, endDate string, offset, limit int) ([]*model.AccountTransaction, error) {
	dbIndex, tableIndex := r.router.RouteByAccountNo(accountNo)
	tableName := r.router.GetTableName("account_transaction", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, err
	}

	var transactions []*model.AccountTransaction
	result := db.WithContext(ctx).Table(tableName).
		Where("account_no = ? AND transaction_date >= ? AND transaction_date <= ?", accountNo, startDate, endDate).
		Order("transaction_time DESC").
		Limit(limit).Offset(offset).
		Find(&transactions)

	return transactions, result.Error
}

// ListByCutDateChunk 见接口注释。
// WHERE status=SUCCESS AND cut_date=? AND id > ? [AND currency=?] ORDER BY id ASC LIMIT ?
func (r *transactionRepository) ListByCutDateChunk(
	ctx context.Context,
	dbIndex, tableIndex int,
	cutDate, currency string,
	fromIDExclusive uint64,
	chunkLimit int,
) ([]*model.AccountTransaction, error) {
	if chunkLimit <= 0 {
		chunkLimit = 1000
	}
	tableName := r.router.GetTableName("account_transaction", tableIndex)
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, err
	}
	q := db.WithContext(ctx).Table(tableName).
		Where("status = ? AND cut_date = ? AND id > ?",
			model.TransactionStatusSuccess, cutDate, fromIDExclusive)
	if currency != "" {
		q = q.Where("currency = ?", currency)
	}
	var rows []*model.AccountTransaction
	if err := q.Order("id ASC").Limit(chunkLimit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("ListByCutDateChunk db[%d] table[%02d] cut_date=%s currency=%q from=%d: %w",
			dbIndex, tableIndex, cutDate, currency, fromIDExclusive, err)
	}
	return rows, nil
}

// ListBufferedAccountNosByCutDate 返回 cut_date 内有 buffered booking 的账户号。
func (r *transactionRepository) ListBufferedAccountNosByCutDate(
	ctx context.Context,
	dbIndex, tableIndex int,
	cutDate, currency string,
) ([]string, error) {
	tableName := r.router.GetTableName("account_transaction", tableIndex)
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, err
	}
	q := db.WithContext(ctx).Table(tableName).
		Distinct("account_no").
		Where("status = ? AND cut_date = ? AND booking_type = ?",
			model.TransactionStatusSuccess, cutDate, model.TransactionBookingTypeBuffered)
	if currency != "" {
		q = q.Where("currency = ?", currency)
	}
	var accNos []string
	if err := q.Pluck("account_no", &accNos).Error; err != nil {
		return nil, fmt.Errorf("ListBufferedAccountNosByCutDate db[%d] table[%02d] cut_date=%s: %w",
			dbIndex, tableIndex, cutDate, err)
	}
	return accNos, nil
}

// ListTransactions 通用分页查询（见接口注释）。
func (r *transactionRepository) ListTransactions(ctx context.Context, f ListTransactionFilter) ([]*model.AccountTransaction, int64, error) {
	// 参数规范化
	if f.Limit <= 0 {
		f.Limit = 30
	}
	if f.Limit > 100 {
		f.Limit = 100
	}
	now := time.Now()
	if f.EndDate == "" {
		f.EndDate = now.Format("2006-01-02")
	}
	if f.StartDate == "" {
		f.StartDate = now.AddDate(0, 0, -90).Format("2006-01-02")
	}

	// ── Case 1: 精确查单条（transaction_id） ──────────────────────────────
	if f.TransactionID != "" {
		tx, err := r.GetTransactionByID(ctx, f.TransactionID)
		if err != nil {
			return nil, 0, err
		}
		if tx == nil {
			return nil, 0, nil
		}
		return []*model.AccountTransaction{tx}, 1, nil
	}

	// ── Case 2: account_no 已知 → 单分片高效查询 ─────────────────────────
	if f.AccountNo != "" {
		return r.listByAccountNo(ctx, f)
	}

	// ── Case 3: 仅 business_no → 并行扫描所有分片 ────────────────────────
	if f.BusinessNo != "" {
		return r.listByBusinessNoAllShards(ctx, f)
	}

	return nil, 0, fmt.Errorf("at least one filter (transaction_id, account_no, business_no) must be specified")
}

// listByAccountNo 在单一分片上查询（含 COUNT）
func (r *transactionRepository) listByAccountNo(ctx context.Context, f ListTransactionFilter) ([]*model.AccountTransaction, int64, error) {
	dbIndex, tableIndex := r.router.RouteByAccountNo(f.AccountNo)
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, 0, fmt.Errorf("get db[%d]: %w", dbIndex, err)
	}
	tableName := r.router.GetTableName("account_transaction", tableIndex)

	q := db.WithContext(ctx).Table(tableName).
		Where("account_no = ? AND transaction_date >= ? AND transaction_date <= ?",
			f.AccountNo, f.StartDate, f.EndDate)
	if f.BusinessNo != "" {
		q = q.Where("business_no = ?", f.BusinessNo)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count transactions: %w", err)
	}

	var rows []*model.AccountTransaction
	if err := q.Order("transaction_time DESC").
		Limit(f.Limit).Offset(f.Offset).
		Find(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("list transactions: %w", err)
	}
	return rows, total, nil
}

// listByBusinessNoAllShards 并行扫描全部分片，收集结果后在 Go 层排序分页
func (r *transactionRepository) listByBusinessNoAllShards(ctx context.Context, f ListTransactionFilter) ([]*model.AccountTransaction, int64, error) {
	shards := r.router.GetAllShards()

	type shardResult struct {
		rows []*model.AccountTransaction
		err  error
	}
	results := make([]shardResult, len(shards))

	var wg sync.WaitGroup
	for i, shard := range shards {
		i, shard := i, shard
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := r.dbManager.GetDB(shard.DBIndex)
			if err != nil {
				results[i].err = fmt.Errorf("get db[%d]: %w", shard.DBIndex, err)
				return
			}
			tableName := r.router.GetTableName("account_transaction", shard.TableIndex)
			var rows []*model.AccountTransaction
			if err := db.WithContext(ctx).Table(tableName).
				Where("business_no = ? AND transaction_date >= ? AND transaction_date <= ?",
					f.BusinessNo, f.StartDate, f.EndDate).
				Order("transaction_time DESC").
				Find(&rows).Error; err != nil {
				results[i].err = err
				return
			}
			results[i].rows = rows
		}()
	}
	wg.Wait()

	var all []*model.AccountTransaction
	for _, res := range results {
		if res.err != nil {
			return nil, 0, res.err
		}
		all = append(all, res.rows...)
	}

	// Sort by transaction_time DESC (newest first) across all shards
	sortTransactionsByTimeDesc(all)

	total := int64(len(all))

	// Apply pagination
	start := f.Offset
	if start >= len(all) {
		return nil, total, nil
	}
	end := start + f.Limit
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], total, nil
}

// sortTransactionsByTimeDesc sorts in-place, newest first.
func sortTransactionsByTimeDesc(rows []*model.AccountTransaction) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].TransactionTime.After(rows[j-1].TransactionTime); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// extractShardInfoFromTransactionID 从交易ID中提取分片信息
// 交易ID格式：{1d-dbIdx}{2d-tableIdx}{timestamp}{suffix}，前3字符编码路由信息
func (r *transactionRepository) extractShardInfoFromTransactionID(transactionID string) (dbIndex, tableIndex int) {
	return r.router.RouteByAccountNo(transactionID)
}
