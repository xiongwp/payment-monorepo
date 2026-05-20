package repository

// SettlementOutboxRepository 结算 Outbox 仓储
//
// 分片策略：
//   按 voucher_no 路由，分库 + 分表（10库 × 每库10表 = 100张物理表）。
//   - 单记录操作（Create / Mark*）：用 voucher_no 路由到目标 DB 和目标表
//   - 跨分片扫描（FindByStatus / FindStuckPending / FindUnprocessed）：
//     遍历全部 100 个分片，并发查询后合并结果

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// isMySQLDuplicateKey 判断是否为 MySQL 唯一键冲突（error code 1062）
func isMySQLDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1062
	}
	return false
}

// SettlementOutboxRepository 结算 Outbox 仓储接口
type SettlementOutboxRepository interface {
	// Create 写入新 outbox 记录（status=PENDING）
	Create(ctx context.Context, outbox *model.SettlementOutbox) error

	// ResetFailedToPending 将 FAILED 状态的 outbox 重置为 PENDING 供热路径重试使用。
	//
	// 场景：hotPathBooking 第一次执行时 outbox 写成功（PENDING），但 Redis Transfer 失败，
	// 随即调用 MarkFailed → outbox 变为 FAILED。重试时 Create 会遇到 DUPLICATE KEY。
	// 此方法将 FAILED 记录重置为 PENDING（含新的 event_data），使重试可以继续。
	//
	// 若记录不存在或状态不是 FAILED，返回错误（调用方应视为冲突或逻辑错误）。
	ResetFailedToPending(ctx context.Context, outbox *model.SettlementOutbox) error

	// MarkRedisDone 将记录状态置为 REDIS_DONE（Redis 已更新）
	MarkRedisDone(ctx context.Context, voucherNo string) error

	// MarkMySQLDone 将记录状态置为 MYSQL_DONE（全链路完成）
	MarkMySQLDone(ctx context.Context, voucherNo string) error

	// MarkFailed 标记为失败并记录错误信息（自增 retry_count）
	MarkFailed(ctx context.Context, voucherNo, errorMsg string) error

	// FindByStatus 跨分片查询指定状态的记录（按 created_at 升序，最多返回 limit 条）
	FindByStatus(ctx context.Context, status model.OutboxStatus, limit int) ([]*model.SettlementOutbox, error)

	// FindByVoucher 按 voucher_no 直接路由到对应分片并读取单条。
	// 找不到返回 nil, nil。供 OutboxWorker 在 Kafka 通知到达时立即拉取对应条目处理。
	FindByVoucher(ctx context.Context, voucherNo string) (*model.SettlementOutbox, error)

	// FindStuckPending 跨分片查询超时未进入 REDIS_DONE 的 PENDING 记录（进程崩溃场景）
	FindStuckPending(ctx context.Context, olderThan time.Duration, limit int) ([]*model.SettlementOutbox, error)

	// FindUnprocessed 跨分片查询未完成记录（PENDING + REDIS_DONE）。
	//
	// upToDate 格式 "2006-01-02"：仅返回 transaction_date <= upToDate 的记录，
	// 供日切按日边界精确补偿 outbox delta，避免将次日热路径交易的增量误计入当天快照。
	// 传空字符串时不做日期过滤（用于缓存预热等场景）。
	FindUnprocessed(ctx context.Context, upToDate string) ([]*model.SettlementOutbox, error)

	// IncrementRetry 递增失败记录的 retry_count 并记录错误信息（不改变 status）
	// 供 OutboxWorker MySQL Writer 在写入失败时调用，达到阈值后由调用方决定是否 MarkFailed
	IncrementRetry(ctx context.Context, voucherNo, errorMsg string) error

	// DeleteOldDone 跨分片删除超过 olderThan 时间的 MYSQL_DONE 记录，防止表无限增长
	// 建议：每小时执行，保留最近 7 天的 MYSQL_DONE 记录用于审计
	// 返回：删除的总行数
	DeleteOldDone(ctx context.Context, olderThan time.Duration) (int64, error)
}

type settlementOutboxRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewSettlementOutboxRepository 创建 Outbox 仓储
func NewSettlementOutboxRepository(dbManager *database.Manager, router *sharding.Router) (SettlementOutboxRepository, error) {
	return &settlementOutboxRepository{dbManager: dbManager, router: router}, nil
}

// route 根据 voucher_no 路由到目标 DB 和目标表名
// voucher_no 格式：前3字符 = {1d-dbIdx}{2d-tableIdx}
func (r *settlementOutboxRepository) route(ctx context.Context, voucherNo string) (*gorm.DB, string, error) {
	dbIndex, tableIndex := r.router.RouteByAccountNo(voucherNo)
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, "", fmt.Errorf("settlement outbox route(%s): get db[%d]: %w", voucherNo, dbIndex, err)
	}
	tableName := r.router.GetTableName("settlement_outbox", tableIndex)
	return db.WithContext(ctx), tableName, nil
}

// ─── 单记录操作（按 voucher_no 路由）─────────────────────────────────────────

func (r *settlementOutboxRepository) Create(ctx context.Context, outbox *model.SettlementOutbox) error {
	db, tableName, err := r.route(ctx, outbox.VoucherNo)
	if err != nil {
		return err
	}
	now := time.Now()
	outbox.CreatedAt = now
	outbox.UpdatedAt = now
	return db.Table(tableName).Create(outbox).Error
}

func (r *settlementOutboxRepository) ResetFailedToPending(ctx context.Context, outbox *model.SettlementOutbox) error {
	db, tableName, err := r.route(ctx, outbox.VoucherNo)
	if err != nil {
		return err
	}
	now := time.Now()
	result := db.Table(tableName).
		Where("voucher_no = ? AND status = ?", outbox.VoucherNo, model.OutboxStatusFailed).
		Updates(map[string]interface{}{
			"status":      model.OutboxStatusPending,
			"event_data":  outbox.EventData,
			"error_msg":   "",
			"retry_count": 0,
			"updated_at":  now,
		})
	if result.Error != nil {
		return fmt.Errorf("reset outbox failed-to-pending(%s): %w", outbox.VoucherNo, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("reset outbox failed-to-pending(%s): record not found or not in FAILED state", outbox.VoucherNo)
	}
	return nil
}

// FindByVoucher 按 voucher_no 路由到对应分片读取单条。
func (r *settlementOutboxRepository) FindByVoucher(ctx context.Context, voucherNo string) (*model.SettlementOutbox, error) {
	db, tableName, err := r.route(ctx, voucherNo)
	if err != nil {
		return nil, err
	}
	var rec model.SettlementOutbox
	res := db.Table(tableName).Where("voucher_no = ?", voucherNo).Take(&rec)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("find outbox by voucher(%s): %w", voucherNo, res.Error)
	}
	return &rec, nil
}

func (r *settlementOutboxRepository) MarkRedisDone(ctx context.Context, voucherNo string) error {
	db, tableName, err := r.route(ctx, voucherNo)
	if err != nil {
		return err
	}
	return db.Table(tableName).
		Where("voucher_no = ?", voucherNo).
		Updates(map[string]interface{}{
			"status":     model.OutboxStatusRedisDone,
			"updated_at": time.Now(),
		}).Error
}

func (r *settlementOutboxRepository) MarkMySQLDone(ctx context.Context, voucherNo string) error {
	db, tableName, err := r.route(ctx, voucherNo)
	if err != nil {
		return err
	}
	return db.Table(tableName).
		Where("voucher_no = ?", voucherNo).
		Updates(map[string]interface{}{
			"status":     model.OutboxStatusMySQLDone,
			"updated_at": time.Now(),
		}).Error
}

func (r *settlementOutboxRepository) MarkFailed(ctx context.Context, voucherNo, errorMsg string) error {
	db, tableName, err := r.route(ctx, voucherNo)
	if err != nil {
		return err
	}
	return db.Table(tableName).
		Where("voucher_no = ?", voucherNo).
		Updates(map[string]interface{}{
			"status":      model.OutboxStatusFailed,
			"error_msg":   errorMsg,
			"retry_count": gorm.Expr("retry_count + 1"),
			"updated_at":  time.Now(),
		}).Error
}

// IncrementRetry 递增 retry_count 并记录错误信息（不改变 status，保持 REDIS_DONE 供重试）
//
// CAS `WHERE status IN (PENDING, REDIS_DONE)`：多实例并发恢复同一行时，只有本次
// UPDATE 真正命中（即行的 status 仍然是可重试状态）的实例才拿到 RowsAffected=1，
// 其他实例得 0 行从而跳过重试，避免重复打 Redis / MySQL。recoverStuckPending
// 依赖这个 RowsAffected 来决定要不要调 retryRedis。
func (r *settlementOutboxRepository) IncrementRetry(ctx context.Context, voucherNo, errorMsg string) error {
	db, tableName, err := r.route(ctx, voucherNo)
	if err != nil {
		return err
	}
	res := db.Table(tableName).
		Where("voucher_no = ? AND status IN ?", voucherNo, []model.OutboxStatus{
			model.OutboxStatusPending,
			model.OutboxStatusRedisDone,
		}).
		Updates(map[string]interface{}{
			"retry_count": gorm.Expr("retry_count + 1"),
			"error_msg":   errorMsg,
			"updated_at":  time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 行已被其他实例推进到 MYSQL_DONE / FAILED，或根本不存在：幂等返回，调用方
		// 通过"不 retryRedis"的方式跳过本次。
		return fmt.Errorf("outbox %s: status already advanced (no increment applied)", voucherNo)
	}
	return nil
}

// ─── 跨分片扫描（并发遍历全部 100 个分片）────────────────────────────────────

// perShardLimit 计算每个分片的查询上限，避免全量拉取后再截断导致过度抓取。
// 公式：ceil(limit / numShards) * 3 + 10，保留 3 倍冗余以应对轻微倾斜；
// voucher_no 按 hash 路由，分布均匀，此冗余足够覆盖正常偏差。
func perShardLimit(limit, numShards int) int {
	if numShards <= 0 || limit <= 0 {
		return limit
	}
	perShard := (limit+numShards-1)/numShards*3 + 10
	if perShard > limit {
		return limit
	}
	return perShard
}

func (r *settlementOutboxRepository) FindByStatus(ctx context.Context, status model.OutboxStatus, limit int) ([]*model.SettlementOutbox, error) {
	numShards := len(r.router.GetAllShards())
	psl := perShardLimit(limit, numShards)
	return r.scanAllShards(ctx, func(db *gorm.DB, tableName string) ([]*model.SettlementOutbox, error) {
		var records []*model.SettlementOutbox
		err := db.Table(tableName).
			Where("status = ?", status).
			Order("created_at ASC").
			Limit(psl).
			Find(&records).Error
		return records, err
	}, limit)
}

func (r *settlementOutboxRepository) FindStuckPending(ctx context.Context, olderThan time.Duration, limit int) ([]*model.SettlementOutbox, error) {
	cutoff := time.Now().Add(-olderThan)
	numShards := len(r.router.GetAllShards())
	psl := perShardLimit(limit, numShards)
	return r.scanAllShards(ctx, func(db *gorm.DB, tableName string) ([]*model.SettlementOutbox, error) {
		var records []*model.SettlementOutbox
		err := db.Table(tableName).
			Where("status = ? AND created_at < ?", model.OutboxStatusPending, cutoff).
			Order("created_at ASC").
			Limit(psl).
			Find(&records).Error
		return records, err
	}, limit)
}

func (r *settlementOutboxRepository) FindUnprocessed(ctx context.Context, upToDate string) ([]*model.SettlementOutbox, error) {
	// 限制最多返回 5000 条：此方法用于日切 delta 补偿和缓存预热（best-effort），
	// 超出限制的记录会在下次预热或 OutboxWorker 正常处理时补偿。
	const warmLimit = 5000
	numShards := len(r.router.GetAllShards())
	psl := perShardLimit(warmLimit, numShards) // 100 分片时约 60 条/分片
	return r.scanAllShards(ctx, func(db *gorm.DB, tableName string) ([]*model.SettlementOutbox, error) {
		var records []*model.SettlementOutbox
		q := db.Table(tableName).
			Where("status IN (?)", []model.OutboxStatus{
				model.OutboxStatusPending,
				model.OutboxStatusRedisDone,
			})
		if upToDate != "" {
			q = q.Where("transaction_date <= ?", upToDate)
		}
		err := q.Order("created_at ASC").Limit(psl).Find(&records).Error
		return records, err
	}, warmLimit)
}

// scanAllShards 并发遍历全部 100 个分片，合并结果后按 created_at 升序截取前 limit 条
// limit=0 表示不限制数量
func (r *settlementOutboxRepository) scanAllShards(
	ctx context.Context,
	query func(db *gorm.DB, tableName string) ([]*model.SettlementOutbox, error),
	limit int,
) ([]*model.SettlementOutbox, error) {
	shards := r.router.GetAllShards()

	type shardResult struct {
		records []*model.SettlementOutbox
		err     error
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
			tableName := r.router.GetTableName("settlement_outbox", s.TableIndex)
			recs, err := query(db.WithContext(ctx), tableName)
			results[idx] = shardResult{records: recs, err: err}
		}(i, shard)
	}
	wg.Wait()

	// 合并、检查错误
	var all []*model.SettlementOutbox
	for _, res := range results {
		if res.err != nil {
			return nil, res.err
		}
		all = append(all, res.records...)
	}

	// 全局按 created_at 升序排序后截取
	sort.Slice(all, func(i, j int) bool {
		return all[i].CreatedAt.Before(all[j].CreatedAt)
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

const (
	deleteOldDonePerShardTimeout = 30 * time.Second // 上调，配合 LIMIT 循环
	// 单条 DELETE 行数上限。MySQL DELETE 持锁时间正比于行数；5000 行典型 < 100ms，
	// 对二进制日志 / 复制压力可控。再大易触发慢查询告警 + 复制延迟。
	deleteOldDoneBatchLimit = 5000
	// 单分片单 tick 最多迭代多少批，防止某个分片有海量积压时把窗口全占了。
	// 5 × 5000 = 25000 行/分片/tick；下次 tick 继续。
	deleteOldDoneMaxBatchesPerShard = 5
)

// DeleteOldDone 并发遍历全部 100 个分片，删除超过 olderThan 时间的 MYSQL_DONE 记录。
//
// 设计要点（优于无 LIMIT 单条 DELETE）：
//   - 每分片用 LIMIT 5000 分批 DELETE，单条事务持锁时间可控（< 100ms 典型）
//     避免对复制 / 在线读写造成长时间阻塞
//   - 单 tick 内一个分片最多迭代 deleteOldDoneMaxBatchesPerShard 批，剩余留给下一 tick
//   - 单分片独立 timeout 防止个别慢分片拖累整体
//   - 任一分片首次报错立即停止该分片后续批（不影响其他分片）
//
// 注意：MySQL DELETE ... LIMIT 是非确定性的（随主键扫描顺序删除），但因为我们 WHERE
// 限定的是 status + updated_at，删完一批后的下一批仍只命中"还没删的旧 MYSQL_DONE"，
// 不会跳过任何应删行。
func (r *settlementOutboxRepository) DeleteOldDone(ctx context.Context, olderThan time.Duration) (int64, error) {
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
			tableName := r.router.GetTableName("settlement_outbox", s.TableIndex)
			shardCtx, cancel := context.WithTimeout(ctx, deleteOldDonePerShardTimeout)
			defer cancel()

			// 分批循环：直到这批返回 rowsAffected < limit（说明本分片无更多旧记录）
			// 或达到 maxBatches（剩下的留给下一个 tick）或 ctx 超时。
			var shardTotal int64
			for batch := 0; batch < deleteOldDoneMaxBatchesPerShard; batch++ {
				if shardCtx.Err() != nil {
					results[idx].err = shardCtx.Err()
					return
				}
				result := db.WithContext(shardCtx).
					Table(tableName).
					Where("status = ? AND updated_at < ?", model.OutboxStatusMySQLDone, cutoff).
					Limit(deleteOldDoneBatchLimit).
					Delete(&model.SettlementOutbox{})
				if result.Error != nil {
					results[idx] = shardResult{affected: shardTotal, err: result.Error}
					return
				}
				shardTotal += result.RowsAffected
				if result.RowsAffected < int64(deleteOldDoneBatchLimit) {
					break // 本分片旧记录已删完
				}
			}
			results[idx] = shardResult{affected: shardTotal}
		}(i, shard)
	}
	wg.Wait()

	var total int64
	for _, res := range results {
		if res.err != nil {
			return total, res.err
		}
		total += res.affected
	}
	return total, nil
}
