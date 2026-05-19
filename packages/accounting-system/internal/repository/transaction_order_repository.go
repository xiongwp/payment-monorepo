package repository

// TransactionOrderRepository 交易订单仓储（按 business_no 后2位分库分表）
//
// 路由规则：n = businessNo(numeric) % 100; dbIdx = n/10; globalTableIdx = n
//
// 存储设计（双表分离）：
//   - transaction_order_{nn}      主表：仅状态字段（小行宽，高缓存命中率）
//   - transaction_order_extra_{nn} 扩展表：req_hash/voucher_no/tx_ids/req_params JSON
//     扩展字段最长 4096 字符；热路径（状态查询/更新）不访问扩展表。
//
// 幂等键：(order_no, business_type, business_no) — 同一 requestId 可用于不同业务订单，
//         但同一 (requestId + businessType + businessNo) 三元组只执行一次。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
)

// isDuplicateKey 检测 MySQL 1062 重复键错误（用于扩展表 upsert 的幂等保护）
func isDuplicateKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Duplicate entry")
}

// TransactionOrderRepository 交易订单仓储接口
type TransactionOrderRepository interface {
	// Create 创建新订单（同时写主表和扩展表，order.Extra 写入扩展表）
	Create(ctx context.Context, order *model.TransactionOrder) error

	// GetByOrderKey 按 (orderNo, businessType, businessNo) 唯一键查询
	// 自动从扩展表加载 order.Extra（若存在）
	GetByOrderKey(ctx context.Context, orderNo, businessType, businessNo string) (*model.TransactionOrder, error)

	// UpdateToProcessing CAS 将 pending/failed 置为 processing（businessNo 用于路由）
	// 只更新主表；返回 affected rows，调用方据此判断是否抢到执行权
	UpdateToProcessing(ctx context.Context, orderNo, businessType, businessNo string) (int64, error)

	// UpdateSuccess 标记订单成功：更新主表 status/voucher_no，更新扩展表 extra（含最终 TxIDs）
	UpdateSuccess(ctx context.Context, orderNo, businessType, businessNo, voucherNo, extra string) error

	// UpdateFailed 标记订单失败，写入错误信息并自增 retry_count（只更新主表）
	UpdateFailed(ctx context.Context, orderNo, businessType, businessNo, errorMsg string) error

	// ResetStuckProcessing 遍历所有分片，将超时 PROCESSING 订单重置为 FAILED
	ResetStuckProcessing(ctx context.Context, olderThan time.Duration) (int64, error)

	// MaxID 返回该分片当前 MAX(id)；用于 day-cut 触发时快照「这一刻已下单的最大订单 id」。
	// 仅供审计 / 对账（cut_max_order_id 列）；day-cut 实际扫描走 account_transaction.id。
	// 零行返回 0。
	MaxID(ctx context.Context, dbIndex, tableIndex int) (uint64, error)

	// ResetForRetry 把单个 order 重置成可重试状态. 用于 trigger 部分失败后人工 / API 重试.
	//
	// 异常场景对齐 (SP-AC-7):
	//   - status=Success → 不动, 返 ResetSkippedSuccess  (idempotent, 同 order_no 不允许重置已落账的)
	//   - status=Processing 且 voucher_no 非空 → phantom processing, 修正到 Success, 返 ResetPhantomFixed
	//   - status=Processing 且 voucher_no 空     → 真 stuck, 改 Failed (retry_count 不动), 返 ResetUnstuck
	//   - status=Failed → 不动 (已可重试), 返 ResetAlreadyFailed
	//   - status=Pending → 也归 Failed (让 CAS 抢占重跑), 返 ResetPending
	//   - force=true: 不论 status, 改 Failed + retry_count=0 (admin 紧急通道)
	//
	// 返回 ResetResult 描述执行了什么动作, 让上游能区分 "已成功别再试" vs "已重置可重试".
	ResetForRetry(ctx context.Context, orderNo, businessType, businessNo string, force bool) (*ResetResult, error)
}

// ResetResult — ResetForRetry 的执行结果.
type ResetResult struct {
	Action       string // "skipped_success" / "phantom_fixed" / "unstuck" / "already_failed" / "reset_pending" / "forced"
	PrevStatus   int8
	CurrStatus   int8
	VoucherNo    string
	RetryCount   int
	MaxRetry     int
	ErrorMessage string
}

type transactionOrderRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewTransactionOrderRepository 创建交易订单仓储
func NewTransactionOrderRepository(dbManager *database.Manager, router *sharding.Router) TransactionOrderRepository {
	return &transactionOrderRepository{
		dbManager: dbManager,
		router:    router,
	}
}

// route 根据 businessNo 路由到目标库和表（返回主表名和扩展表名）
func (r *transactionOrderRepository) route(ctx context.Context, businessNo string) (db *gorm.DB, mainTable, extraTable string, err error) {
	dbIndex, tableIndex := r.router.RouteByNumericStr(businessNo)
	gdb, e := r.dbManager.GetDB(dbIndex)
	if e != nil {
		return nil, "", "", fmt.Errorf("transaction_order route(%s): get db[%d]: %w", businessNo, dbIndex, e)
	}
	main := r.router.GetTableName("transaction_order", tableIndex)
	extra := r.router.GetTableName("transaction_order_extra", tableIndex)
	return gdb.WithContext(ctx), main, extra, nil
}

// truncateExtra 确保 extra 不超过 4096 字符上限
func truncateExtra(extra string) string {
	if len(extra) <= model.TransactionOrderExtraMaxLen {
		return extra
	}
	// 截断并用 "…" 替代末尾，保证是合法（虽不完整的）JSON 前缀
	return extra[:model.TransactionOrderExtraMaxLen-1] + "…"
}

// Create 创建新订单（同时写主表和扩展表）
func (r *transactionOrderRepository) Create(ctx context.Context, order *model.TransactionOrder) error {
	db, mainTable, extraTable, err := r.route(ctx, order.BusinessNo)
	if err != nil {
		return err
	}

	// 写主表
	if err := db.Table(mainTable).Create(order).Error; err != nil {
		return err
	}

	// 写扩展表（extra 字段）
	if order.Extra != "" {
		extra := &model.TransactionOrderExtra{
			OrderNo:      order.OrderNo,
			BusinessNo:   order.BusinessNo,
			BusinessType: order.BusinessType,
			Extra:        truncateExtra(order.Extra),
		}
		if err := db.Table(extraTable).Create(extra).Error; err != nil {
			// 扩展表写入失败：主表记录已存在，返回错误（幂等重试时会重新写入）
			return fmt.Errorf("create order_extra: %w", err)
		}
	}
	return nil
}

// GetByOrderKey 按 (orderNo, businessType, businessNo) 唯一键查询
// 同时从扩展表加载 Extra 字段
func (r *transactionOrderRepository) GetByOrderKey(ctx context.Context, orderNo, businessType, businessNo string) (*model.TransactionOrder, error) {
	db, mainTable, extraTable, err := r.route(ctx, businessNo)
	if err != nil {
		return nil, err
	}

	// 查主表
	var order model.TransactionOrder
	result := db.Table(mainTable).
		Where("order_no = ? AND business_type = ? AND business_no = ?", orderNo, businessType, businessNo).
		First(&order)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("query transaction_order failed: %w", result.Error)
	}

	// 查扩展表（加载 Extra）
	var extra model.TransactionOrderExtra
	extraResult := db.Table(extraTable).
		Where("order_no = ? AND business_type = ? AND business_no = ?", orderNo, businessType, businessNo).
		First(&extra)
	if extraResult.Error == nil {
		order.Extra = extra.Extra
	}
	// 扩展表记录不存在时（旧数据或写入失败），Extra 为空字符串，上层按空处理

	return &order, nil
}

// UpdateToProcessing CAS 置为 processing（只更新主表）
func (r *transactionOrderRepository) UpdateToProcessing(ctx context.Context, orderNo, businessType, businessNo string) (int64, error) {
	db, mainTable, _, err := r.route(ctx, businessNo)
	if err != nil {
		return 0, err
	}

	result := db.Table(mainTable).
		Where("order_no = ? AND business_type = ? AND business_no = ? AND status IN (?)",
			orderNo, businessType, businessNo,
			[]int8{model.TransactionOrderStatusPending, model.TransactionOrderStatusFailed},
		).
		Updates(map[string]interface{}{
			"status":     model.TransactionOrderStatusProcessing,
			"updated_at": time.Now(),
		})

	if result.Error != nil {
		return 0, fmt.Errorf("update order to processing failed: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// UpdateSuccess 标记订单成功：主表更新 status/voucher_no，扩展表 upsert extra
func (r *transactionOrderRepository) UpdateSuccess(ctx context.Context, orderNo, businessType, businessNo, voucherNo, extra string) error {
	db, mainTable, extraTable, err := r.route(ctx, businessNo)
	if err != nil {
		return err
	}

	// 更新主表
	if err := db.Table(mainTable).
		Where("order_no = ? AND business_type = ? AND business_no = ?", orderNo, businessType, businessNo).
		Updates(map[string]interface{}{
			"status":     model.TransactionOrderStatusSuccess,
			"voucher_no": voucherNo,
			"updated_at": time.Now(),
		}).Error; err != nil {
		return fmt.Errorf("update order success (main): %w", err)
	}

	// Upsert 扩展表（extra 含最终 TxIDs）
	if extra != "" {
		truncated := truncateExtra(extra)
		updateResult := db.Table(extraTable).
			Where("order_no = ? AND business_type = ? AND business_no = ?", orderNo, businessType, businessNo).
			Updates(map[string]interface{}{
				"extra":      truncated,
				"updated_at": time.Now(),
			})
		if updateResult.Error != nil {
			return fmt.Errorf("update order_extra success: %w", updateResult.Error)
		}
		if updateResult.RowsAffected == 0 {
			// 扩展记录不存在（极少数情况）：insert
			ins := &model.TransactionOrderExtra{
				OrderNo:      orderNo,
				BusinessNo:   businessNo,
				BusinessType: businessType,
				Extra:        truncated,
			}
			if err := db.Table(extraTable).Create(ins).Error; err != nil && !isDuplicateKey(err) {
				return fmt.Errorf("insert order_extra on success: %w", err)
			}
		}
	}
	return nil
}

// UpdateFailed 标记订单失败并自增重试次数（只更新主表）
func (r *transactionOrderRepository) UpdateFailed(ctx context.Context, orderNo, businessType, businessNo, errorMsg string) error {
	db, mainTable, _, err := r.route(ctx, businessNo)
	if err != nil {
		return err
	}

	return db.Table(mainTable).
		Where("order_no = ? AND business_type = ? AND business_no = ?", orderNo, businessType, businessNo).
		Updates(map[string]interface{}{
			"status":        model.TransactionOrderStatusFailed,
			"error_message": errorMsg,
			"retry_count":   gorm.Expr("retry_count + 1"),
			"updated_at":    time.Now(),
		}).Error
}

// ResetForRetry — 按 order_no 把订单重置成可重试态.
// 关键: 不破坏已成功的资金落账, phantom processing 主动修正 → Success.
func (r *transactionOrderRepository) ResetForRetry(ctx context.Context, orderNo, businessType, businessNo string, force bool) (*ResetResult, error) {
	db, mainTable, _, err := r.route(ctx, businessNo)
	if err != nil {
		return nil, err
	}

	var ord model.TransactionOrder
	if err := db.Table(mainTable).
		Where("order_no = ? AND business_type = ? AND business_no = ?", orderNo, businessType, businessNo).
		First(&ord).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("order %q not found (shard route by business_no=%q)", orderNo, businessNo)
		}
		return nil, fmt.Errorf("load order for reset: %w", err)
	}

	res := &ResetResult{
		PrevStatus:   ord.Status,
		CurrStatus:   ord.Status,
		VoucherNo:    ord.VoucherNo,
		RetryCount:   int(ord.RetryCount),
		MaxRetry:     int(ord.MaxRetryCount),
		ErrorMessage: ord.ErrorMessage,
	}

	// force=true: 紧急通道, 不管当前 status, 归 Failed + retry_count=0.
	if force {
		if err := db.Table(mainTable).
			Where("order_no = ? AND business_type = ? AND business_no = ?", orderNo, businessType, businessNo).
			Updates(map[string]interface{}{
				"status":        model.TransactionOrderStatusFailed,
				"retry_count":   0,
				"error_message": "reset by admin force",
				"updated_at":    time.Now(),
			}).Error; err != nil {
			return nil, fmt.Errorf("force reset: %w", err)
		}
		res.Action = "forced"
		res.CurrStatus = model.TransactionOrderStatusFailed
		res.RetryCount = 0
		return res, nil
	}

	switch ord.Status {
	case model.TransactionOrderStatusSuccess:
		// 已成功, 绝对不允许重置 (避免破坏资金).
		res.Action = "skipped_success"
		return res, nil

	case model.TransactionOrderStatusProcessing:
		// 区分真 stuck vs phantom (实际已成功但 status 没被改).
		if ord.VoucherNo != "" {
			// Phantom processing — voucher 已生成, 直接修正到 Success.
			if err := db.Table(mainTable).
				Where("order_no = ? AND business_type = ? AND business_no = ? AND status = ?",
					orderNo, businessType, businessNo, model.TransactionOrderStatusProcessing).
				Updates(map[string]interface{}{
					"status":     model.TransactionOrderStatusSuccess,
					"updated_at": time.Now(),
				}).Error; err != nil {
				return nil, fmt.Errorf("phantom fix to success: %w", err)
			}
			res.Action = "phantom_fixed"
			res.CurrStatus = model.TransactionOrderStatusSuccess
			return res, nil
		}
		// 真 stuck — 改 Failed (retry_count 不动, 让 CreateTransaction 走重试分支).
		if err := db.Table(mainTable).
			Where("order_no = ? AND business_type = ? AND business_no = ? AND status = ?",
				orderNo, businessType, businessNo, model.TransactionOrderStatusProcessing).
			Updates(map[string]interface{}{
				"status":        model.TransactionOrderStatusFailed,
				"error_message": "reset_for_retry: was stuck in PROCESSING",
				"updated_at":    time.Now(),
			}).Error; err != nil {
			return nil, fmt.Errorf("unstuck processing: %w", err)
		}
		res.Action = "unstuck"
		res.CurrStatus = model.TransactionOrderStatusFailed
		return res, nil

	case model.TransactionOrderStatusFailed:
		// 已可重试, 不需要再动.
		res.Action = "already_failed"
		return res, nil

	case model.TransactionOrderStatusPending:
		// Pending 一般说明 CAS 还没抢, 也归 Failed 让 CreateTransaction 路径重新抢.
		if err := db.Table(mainTable).
			Where("order_no = ? AND business_type = ? AND business_no = ? AND status = ?",
				orderNo, businessType, businessNo, model.TransactionOrderStatusPending).
			Updates(map[string]interface{}{
				"status":     model.TransactionOrderStatusFailed,
				"updated_at": time.Now(),
			}).Error; err != nil {
			return nil, fmt.Errorf("reset pending: %w", err)
		}
		res.Action = "reset_pending"
		res.CurrStatus = model.TransactionOrderStatusFailed
		return res, nil

	default:
		return nil, fmt.Errorf("unknown order status %d", ord.Status)
	}
}

// ─── FreezeOrderRepository ────────────────────────────────────────────────────
//
// 冻结订单仓储：与普通 TransactionOrderRepository 不同，所有方法接受显式的
// (*gorm.DB, tableIndex) 参数，由调用方（FreezeService）根据 accountNo 路由后传入。
// 这使得冻结订单与账户落在同一物理分片，从而可在同一 DB 事务中原子更新。
//
// 底层仍使用 transaction_order_{nn} / transaction_order_extra_{nn} 表。

// FreezeOrderRepository 冻结订单仓储接口
type FreezeOrderRepository interface {
	// Create 在指定 *gorm.DB（可为 TX）和 tableIndex 内创建冻结订单。
	Create(ctx context.Context, db *gorm.DB, order *model.TransactionOrder, tableIndex int) error

	// GetByKey 在指定 *gorm.DB 和 tableIndex 内按 (orderNo, businessType, businessNo) 查询。
	GetByKey(ctx context.Context, db *gorm.DB, orderNo, businessType, businessNo string, tableIndex int) (*model.TransactionOrder, error)

	// UpdateToFrozen CAS: status=PENDING(0) → PROCESSING/FROZEN(1)，必须在 TX 内调用。
	UpdateToFrozen(ctx context.Context, tx *gorm.DB, orderNo, businessType, businessNo string, tableIndex int) (int64, error)

	// UpdateSuccess 标记订单成功（status=2），写入 voucherNo，必须在 TX 内调用。
	UpdateSuccess(ctx context.Context, tx *gorm.DB, orderNo, businessType, businessNo, voucherNo string, tableIndex int) error

	// UpdateFailed 标记订单失败（status=3），写入错误信息，必须在 TX 内调用。
	UpdateFailed(ctx context.Context, tx *gorm.DB, orderNo, businessType, businessNo, errMsg string, tableIndex int) error
}

type freezeOrderRepository struct {
	router *sharding.Router
}

// NewFreezeOrderRepository 创建冻结订单仓储
func NewFreezeOrderRepository(router *sharding.Router) FreezeOrderRepository {
	return &freezeOrderRepository{router: router}
}

func (r *freezeOrderRepository) Create(ctx context.Context, db *gorm.DB, order *model.TransactionOrder, tableIndex int) error {
	mainTable := r.router.GetTableName("transaction_order", tableIndex)
	if err := db.WithContext(ctx).Table(mainTable).Create(order).Error; err != nil {
		return err
	}
	if order.Extra != "" {
		extraTable := r.router.GetTableName("transaction_order_extra", tableIndex)
		extra := &model.TransactionOrderExtra{
			OrderNo:      order.OrderNo,
			BusinessNo:   order.BusinessNo,
			BusinessType: order.BusinessType,
			Extra:        truncateExtra(order.Extra),
		}
		if err := db.WithContext(ctx).Table(extraTable).Create(extra).Error; err != nil {
			return fmt.Errorf("create freeze order_extra: %w", err)
		}
	}
	return nil
}

func (r *freezeOrderRepository) GetByKey(ctx context.Context, db *gorm.DB, orderNo, businessType, businessNo string, tableIndex int) (*model.TransactionOrder, error) {
	mainTable := r.router.GetTableName("transaction_order", tableIndex)
	var order model.TransactionOrder
	result := db.WithContext(ctx).Table(mainTable).
		Where("order_no = ? AND business_type = ? AND business_no = ?", orderNo, businessType, businessNo).
		First(&order)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get freeze order: %w", result.Error)
	}
	// Load extra (frozen_account_no etc.)
	extraTable := r.router.GetTableName("transaction_order_extra", tableIndex)
	var extra model.TransactionOrderExtra
	if err := db.WithContext(ctx).Table(extraTable).
		Where("order_no = ? AND business_type = ? AND business_no = ?", orderNo, businessType, businessNo).
		First(&extra).Error; err == nil {
		order.Extra = extra.Extra
	}
	return &order, nil
}

func (r *freezeOrderRepository) UpdateToFrozen(ctx context.Context, tx *gorm.DB, orderNo, businessType, businessNo string, tableIndex int) (int64, error) {
	mainTable := r.router.GetTableName("transaction_order", tableIndex)
	result := tx.WithContext(ctx).Table(mainTable).
		Where("order_no = ? AND business_type = ? AND business_no = ? AND status = ?",
			orderNo, businessType, businessNo, model.TransactionOrderStatusPending).
		Updates(map[string]interface{}{
			"status":     model.TransactionOrderStatusProcessing, // 1 = FROZEN
			"updated_at": time.Now(),
		})
	if result.Error != nil {
		return 0, fmt.Errorf("update freeze order to frozen: %w", result.Error)
	}
	return result.RowsAffected, nil
}

func (r *freezeOrderRepository) UpdateSuccess(ctx context.Context, tx *gorm.DB, orderNo, businessType, businessNo, voucherNo string, tableIndex int) error {
	mainTable := r.router.GetTableName("transaction_order", tableIndex)
	return tx.WithContext(ctx).Table(mainTable).
		Where("order_no = ? AND business_type = ? AND business_no = ?", orderNo, businessType, businessNo).
		Updates(map[string]interface{}{
			"status":     model.TransactionOrderStatusSuccess,
			"voucher_no": voucherNo,
			"updated_at": time.Now(),
		}).Error
}

func (r *freezeOrderRepository) UpdateFailed(ctx context.Context, tx *gorm.DB, orderNo, businessType, businessNo, errMsg string, tableIndex int) error {
	mainTable := r.router.GetTableName("transaction_order", tableIndex)
	return tx.WithContext(ctx).Table(mainTable).
		Where("order_no = ? AND business_type = ? AND business_no = ?", orderNo, businessType, businessNo).
		Updates(map[string]interface{}{
			"status":        model.TransactionOrderStatusFailed,
			"error_message": errMsg,
			"updated_at":    time.Now(),
		}).Error
}

// ResetStuckProcessing 并发遍历所有分库分表，将超时 PROCESSING 订单重置为 FAILED（只操作主表）
// 各分片独立更新，并行执行将总耗时从 O(100×shard_latency) 降至 O(shard_latency)。
func (r *transactionOrderRepository) ResetStuckProcessing(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	shards := r.router.GetAllShards()

	type shardResult struct {
		affected int64
		err      error
	}
	results := make([]shardResult, len(shards))

	var wg sync.WaitGroup
	for i, shard := range shards {
		wg.Add(1)
		go func(idx int, s sharding.ShardInfo) {
			defer wg.Done()
			db, err := r.dbManager.GetDB(s.DBIndex)
			if err != nil {
				results[idx].err = fmt.Errorf("get db[%d]: %w", s.DBIndex, err)
				return
			}
			tableName := r.router.GetTableName("transaction_order", s.TableIndex)
			result := db.WithContext(ctx).Table(tableName).
				Where("status = ? AND updated_at < ?",
					model.TransactionOrderStatusProcessing, cutoff).
				Updates(map[string]interface{}{
					"status":        model.TransactionOrderStatusFailed,
					"error_message": "reset by recovery: stuck in PROCESSING state",
					"retry_count":   gorm.Expr("retry_count + 1"),
					"updated_at":    time.Now(),
				})
			if result.Error != nil {
				results[idx].err = fmt.Errorf("db[%d] table %s: %w", s.DBIndex, tableName, result.Error)
				return
			}
			results[idx].affected = result.RowsAffected
		}(i, shard)
	}
	wg.Wait()

	var totalAffected int64
	for _, res := range results {
		if res.err != nil {
			return totalAffected, fmt.Errorf("ResetStuckProcessing: %w", res.err)
		}
		totalAffected += res.affected
	}
	return totalAffected, nil
}

// MaxID 见接口注释。COALESCE(MAX(id), 0)。零行 / 当前未注册分片 → 返回 0。
func (r *transactionOrderRepository) MaxID(ctx context.Context, dbIndex, tableIndex int) (uint64, error) {
	tableName := r.router.GetTableName("transaction_order", tableIndex)
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return 0, fmt.Errorf("transaction_order MaxID: get db[%d]: %w", dbIndex, err)
	}
	var maxID *uint64
	if err := db.WithContext(ctx).Table(tableName).
		Select("COALESCE(MAX(id), 0)").
		Scan(&maxID).Error; err != nil {
		return 0, fmt.Errorf("transaction_order MaxID db[%d] table[%02d]: %w", dbIndex, tableIndex, err)
	}
	if maxID == nil {
		return 0, nil
	}
	return *maxID, nil
}
