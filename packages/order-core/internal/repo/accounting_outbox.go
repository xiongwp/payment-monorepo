package repo

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/sharding"
)

// AccountingOutboxRepository 记账 outbox 仓储。
//
// 按 payment_intent_id 路由：同一 PI 的 charge/refund/记账都落同一物理库，
// 方便联合查询。
//
// 多副本并发安全：worker 用 ClaimBatch + ListByClaimToken 替代 ListPending；
// 同一行同时只能被一个 worker 处理。Mark* 系列也带 claim_token 条件兜底（防止
// claim lease 过期被抢占后原 worker 仍写状态）。
type AccountingOutboxRepository interface {
	// Insert 入队一条 pending 行；若 request_id 已存在，返回原行 +
	// ErrAccountingOutboxDuplicate。
	Insert(ctx context.Context, row *domain.AccountingOutbox) (*domain.AccountingOutbox, error)
	// ListPending 扫描所有分片，取 pending 且 next_attempt_at<=now 的行。
	//
	// Deprecated: 多副本部署下 ListPending + 自行处理无法保证同一行只被一个 worker 抓取，
	// 会导致重复投递（accounting 侧靠 request_id 唯一索引兜底，但语义上是异常）。
	// 新代码走 ClaimBatch + ListByClaimToken；保留 ListPending 仅供单测 / 巡检使用。
	ListPending(ctx context.Context, now time.Time, limit int) ([]*domain.AccountingOutbox, error)
	// ClaimBatch 跨所有分片表 claim 一批 due 行：原子 UPDATE 把候选行的 next_attempt_at
	// 推到远未来 + 写入本 worker 生成的 claim_token，其他 worker 因 next_attempt_at
	// 在未来天然跳过。返回 claim_token 和跨分片实际 claim 到的总行数。
	//
	// limit 是单张分片表 claim 上限，跨 100 张表后实际 claim 量可能远大于 limit。
	// claimLease 是租约时长（次次 attempt 时被抢回前的预算），建议 5-10min；
	// 投递成功 / 失败时 worker 会显式回写状态，租约自然解除。
	ClaimBatch(ctx context.Context, now time.Time, perTableLimit int, claimLease time.Duration) (claimToken string, claimed int, err error)
	// ListByClaimToken 跨分片读回某 claim_token 对应的所有行。
	ListByClaimToken(ctx context.Context, claimToken string) ([]*domain.AccountingOutbox, error)
	// MarkSent 成功投递：status→sent + sent_at + 清空 claim_token。
	// 仅在 row.ClaimToken 仍持有时生效（防止租约过期后写覆盖）。
	MarkSent(ctx context.Context, row *domain.AccountingOutbox) error
	// PurgeSentBefore 删除 status=sent 且 sent_at < before 的行，每个分片最多 limit。
	// 返回跨所有分片的删除总数。dead-letter (status=failed) 不删，留给运维核查。
	PurgeSentBefore(ctx context.Context, before time.Time, limit int) (int64, error)
	// CountDeadLetters 统计 status=failed 的行数（跨分片）。供 metric 告警。
	CountDeadLetters(ctx context.Context) (int64, error)
	// StatsPending 跨分片统计 pending 行数 + 最老一条的 created 时间。
	// 用于 lag 监控指标：pending count 反映积压量，oldest age 反映滞留时长。
	// pending=0 时返回 (0, time.Time{}, nil)。
	StatsPending(ctx context.Context) (count int64, oldest time.Time, err error)
	// MarkRetry 失败但可重试：attempts++ + next_attempt_at + last_error + 清 claim_token。
	// 仅在 row.ClaimToken 仍持有时生效。
	MarkRetry(ctx context.Context, row *domain.AccountingOutbox, nextAt time.Time, errMsg string) error
	// MarkFailed 终态失败：status→failed + last_error + 清 claim_token。
	// 仅在 row.ClaimToken 仍持有时生效。
	MarkFailed(ctx context.Context, row *domain.AccountingOutbox, errMsg string) error
}

type accountingOutboxRepo struct {
	mgr    *Manager
	router *sharding.Router
}

// NewAccountingOutboxRepository 构造。
func NewAccountingOutboxRepository(mgr *Manager, r *sharding.Router) AccountingOutboxRepository {
	return &accountingOutboxRepo{mgr: mgr, router: r}
}

const accountingOutboxTable = "accounting_outbox"

func (r *accountingOutboxRepo) shardOf(ctx context.Context, piID string) (*gorm.DB, string, error) {
	dbIdx, tblIdx := r.router.RouteByPrefixedID(piID)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, "", err
	}
	return db, r.router.TableName(ctx, accountingOutboxTable, tblIdx), nil
}

func (r *accountingOutboxRepo) Insert(ctx context.Context, row *domain.AccountingOutbox) (*domain.AccountingOutbox, error) {
	if row.PaymentIntentID == "" || row.RequestID == "" {
		return nil, fmt.Errorf("%w: payment_intent_id and request_id required", domain.ErrValidation)
	}
	if row.Status == "" {
		row.Status = domain.AccountingOutboxPending
	}
	// 给 next_attempt_at 一个确定值（= now），避免 NULL。让 ListPending 的 WHERE
	// 条件从 "status=? AND (next_attempt_at IS NULL OR next_attempt_at<=?)" 简化为
	// "status=? AND next_attempt_at<=?"，直接吃 idx_status_next 复合索引，
	// 不再因 NULL 分支走全表扫。
	if row.NextAttemptAt == nil {
		now := time.Now()
		row.NextAttemptAt = &now
	}
	db, tbl, err := r.shardOf(ctx, row.PaymentIntentID)
	if err != nil {
		return nil, err
	}
	err = db.WithContext(ctx).Table(tbl).Create(row).Error
	if err == nil {
		return row, nil
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		existing, getErr := r.getByRequestID(ctx, db, tbl, row.RequestID)
		if getErr != nil {
			return nil, fmt.Errorf("dedupe lookup after dup: %w (orig: %v)", getErr, err)
		}
		return existing, domain.ErrAccountingOutboxDuplicate
	}
	if strings.Contains(err.Error(), "Duplicate entry") {
		existing, getErr := r.getByRequestID(ctx, db, tbl, row.RequestID)
		if getErr != nil {
			return nil, fmt.Errorf("dedupe lookup after dup: %w (orig: %v)", getErr, err)
		}
		return existing, domain.ErrAccountingOutboxDuplicate
	}
	return nil, err
}

func (r *accountingOutboxRepo) getByRequestID(ctx context.Context, db *gorm.DB, tbl, reqID string) (*domain.AccountingOutbox, error) {
	var out domain.AccountingOutbox
	err := db.WithContext(ctx).Table(tbl).Where("request_id = ?", reqID).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("accounting outbox not found: %s", reqID)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *accountingOutboxRepo) ListPending(ctx context.Context, now time.Time, limit int) ([]*domain.AccountingOutbox, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []*domain.AccountingOutbox
	for i := 0; i < r.router.DBCount(); i++ {
		db, err := r.mgr.GetShard(i)
		if err != nil {
			return nil, err
		}
		for j := 0; j < r.router.TablePerDB(); j++ {
			tbl := r.router.TableName(ctx, accountingOutboxTable, i*r.router.TablePerDB()+j)
			var rows []*domain.AccountingOutbox
			// 走 idx_status_next (status, next_attempt_at) 复合索引：
			//   - Insert 已保证 next_attempt_at 非 NULL → 主路径是等值+范围，吃满索引
			//   - IS NULL 兜底是兼容老数据（历史上曾存在 NULL 行），MySQL 会走
			//     index merge union，成本低且不影响新流量
			q := db.WithContext(ctx).Table(tbl).
				Where("status = ?", domain.AccountingOutboxPending).
				Where("next_attempt_at <= ? OR next_attempt_at IS NULL", now).
				Order("next_attempt_at, created").
				Limit(limit)
			if err := q.Find(&rows).Error; err != nil {
				return nil, fmt.Errorf("list pending accounting outbox on %s: %w", tbl, err)
			}
			out = append(out, rows...)
			if len(out) >= limit {
				return out[:limit], nil
			}
		}
	}
	return out, nil
}

// markCondition 构造 Mark{Sent,Retry,Failed} 的 WHERE：仅当 row.ClaimToken 仍持有
// 该行才允许写状态，避免 claim 租约过期被别的 worker 抢回后原 worker 仍覆盖状态。
// row.ClaimToken 为空（兼容 ListPending 老路径 / 测试）时退化为单纯按 id 匹配。
func markCondition(db *gorm.DB, row *domain.AccountingOutbox) *gorm.DB {
	q := db.Where("id = ?", row.ID)
	if row.ClaimToken != "" {
		q = q.Where("claim_token = ?", row.ClaimToken)
	}
	return q
}

func (r *accountingOutboxRepo) MarkSent(ctx context.Context, row *domain.AccountingOutbox) error {
	db, tbl, err := r.shardOf(ctx, row.PaymentIntentID)
	if err != nil {
		return err
	}
	res := markCondition(db.WithContext(ctx).Table(tbl), row).
		Updates(map[string]any{
			"status":      domain.AccountingOutboxSent,
			"sent_at":     gorm.Expr("CURRENT_TIMESTAMP(3)"),
			"claim_token": "", // 释放 claim（统一用空串，与 ClaimBatch WHERE 兼容）
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 行未被本 token 持有 → 租约已被抢，状态不应由本 worker 写。
		// 调用方一般是 worker.process()，看到这个 err 应只记日志（accounting-system 已成功 + idempotent 兜底）。
		return fmt.Errorf("accounting outbox: claim lost or row gone (id=%s token=%s)", row.ID, row.ClaimToken)
	}
	return nil
}

func (r *accountingOutboxRepo) MarkRetry(ctx context.Context, row *domain.AccountingOutbox, nextAt time.Time, errMsg string) error {
	db, tbl, err := r.shardOf(ctx, row.PaymentIntentID)
	if err != nil {
		return err
	}
	res := markCondition(db.WithContext(ctx).Table(tbl), row).
		Updates(map[string]any{
			"attempts":        gorm.Expr("attempts + 1"),
			"next_attempt_at": nextAt,
			"last_error":      errMsg,
			"claim_token":     "", // 释放 claim 让下个 worker 重新 claim
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("accounting outbox: claim lost on retry (id=%s)", row.ID)
	}
	return nil
}

func (r *accountingOutboxRepo) MarkFailed(ctx context.Context, row *domain.AccountingOutbox, errMsg string) error {
	db, tbl, err := r.shardOf(ctx, row.PaymentIntentID)
	if err != nil {
		return err
	}
	res := markCondition(db.WithContext(ctx).Table(tbl), row).
		Updates(map[string]any{
			"status":      domain.AccountingOutboxFailed,
			"last_error":  errMsg,
			"claim_token": "", // 释放 claim（统一用空串，与 ClaimBatch WHERE 兼容）
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("accounting outbox: claim lost on failure (id=%s)", row.ID)
	}
	return nil
}

// ClaimBatch 跨所有 100 张分片表 claim 一批 due 行。
//
// 实现：对每张表执行
//
//	UPDATE accounting_outbox_NN
//	   SET claim_token = ?, next_attempt_at = ?  (-- 推到远未来 = 租约过期前)
//	 WHERE status = 'pending'
//	   AND (next_attempt_at IS NULL OR next_attempt_at <= ?)  (-- now)
//	   AND (claim_token IS NULL)
//	 ORDER BY next_attempt_at
//	 LIMIT N
//
// 多 worker / 多 pod 同时 claim 同一行时，InnoDB 行锁保证 UPDATE 串行，
// 一个 worker UPDATE 后另一个的 WHERE 自动失败（claim_token 已不再 NULL）。
//
// 实际 claim 总数最多 = perTableLimit × 100 张表，单 Tick 不要设太大。
func (r *accountingOutboxRepo) ClaimBatch(ctx context.Context, now time.Time, perTableLimit int, claimLease time.Duration) (string, int, error) {
	if perTableLimit <= 0 {
		perTableLimit = 50
	}
	if claimLease <= 0 {
		claimLease = 5 * time.Minute
	}
	// claim_token 唯一性：纳秒时间戳 + rand63 让多 pod、多协程同时 ClaimBatch 不撞。
	// 不用 UUID 是为了 idx_claim 在 BTREE 下前缀压缩友好。
	claimToken := fmt.Sprintf("acct:%d:%d", time.Now().UnixNano(), rand.Int63())
	leaseUntil := now.Add(claimLease)

	total := 0
	for i := 0; i < r.router.DBCount(); i++ {
		db, err := r.mgr.GetShard(i)
		if err != nil {
			return claimToken, total, err
		}
		for j := 0; j < r.router.TablePerDB(); j++ {
			tbl := r.router.TableName(ctx, accountingOutboxTable, i*r.router.TablePerDB()+j)
			// MySQL 不允许 UPDATE + 子查询同表，但单表 LIMIT 没问题：
			// `UPDATE t SET ... WHERE ... ORDER BY ... LIMIT N` 是合法 InnoDB 用法。
			// claim_token 兼容 NULL（migrate 后的老数据）和 ''（gorm Create 插入的零值）。
			res := db.WithContext(ctx).Table(tbl).
				Where("status = ?", domain.AccountingOutboxPending).
				Where("next_attempt_at IS NULL OR next_attempt_at <= ?", now).
				Where("claim_token IS NULL OR claim_token = ''").
				Order("next_attempt_at, created").
				Limit(perTableLimit).
				Updates(map[string]any{
					"claim_token":     claimToken,
					"next_attempt_at": leaseUntil, // 租约：lease 期内别的 worker 看不到这行
				})
			if res.Error != nil {
				return claimToken, total, fmt.Errorf("claim batch on %s: %w", tbl, res.Error)
			}
			total += int(res.RowsAffected)
		}
	}
	return claimToken, total, nil
}

// ListByClaimToken 跨所有分片表读回某 claim_token 对应的行。
// 走 idx_claim 索引，读取量 = 上一步 ClaimBatch 实际 claim 到的总数。
func (r *accountingOutboxRepo) ListByClaimToken(ctx context.Context, claimToken string) ([]*domain.AccountingOutbox, error) {
	if claimToken == "" {
		return nil, nil
	}
	var out []*domain.AccountingOutbox
	for i := 0; i < r.router.DBCount(); i++ {
		db, err := r.mgr.GetShard(i)
		if err != nil {
			return nil, err
		}
		for j := 0; j < r.router.TablePerDB(); j++ {
			tbl := r.router.TableName(ctx, accountingOutboxTable, i*r.router.TablePerDB()+j)
			var rows []*domain.AccountingOutbox
			if err := db.WithContext(ctx).Table(tbl).
				Where("claim_token = ?", claimToken).
				Find(&rows).Error; err != nil {
				return nil, fmt.Errorf("list by claim_token on %s: %w", tbl, err)
			}
			out = append(out, rows...)
		}
	}
	return out, nil
}

// PurgeSentBefore 按分片扫 accounting_outbox_*，删除 status=sent 且 sent_at < before 的行。
// 实现为每分片单独 DELETE ... LIMIT N，避免一锁锁一张大表。
func (r *accountingOutboxRepo) PurgeSentBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	var total int64
	for i := 0; i < r.router.DBCount(); i++ {
		db, err := r.mgr.GetShard(i)
		if err != nil {
			return total, err
		}
		for j := 0; j < r.router.TablePerDB(); j++ {
			tbl := r.router.TableName(ctx, accountingOutboxTable, i*r.router.TablePerDB()+j)
			res := db.WithContext(ctx).Table(tbl).
				Where("status = ?", domain.AccountingOutboxSent).
				Where("sent_at IS NOT NULL AND sent_at < ?", before).
				Limit(limit).
				Delete(&domain.AccountingOutbox{})
			if res.Error != nil {
				return total, fmt.Errorf("purge sent on %s: %w", tbl, res.Error)
			}
			total += res.RowsAffected
		}
	}
	return total, nil
}

// CountDeadLetters 统计所有分片里 status=failed 的行数（供 metric / 告警）。
func (r *accountingOutboxRepo) CountDeadLetters(ctx context.Context) (int64, error) {
	var total int64
	for i := 0; i < r.router.DBCount(); i++ {
		db, err := r.mgr.GetShard(i)
		if err != nil {
			return total, err
		}
		for j := 0; j < r.router.TablePerDB(); j++ {
			tbl := r.router.TableName(ctx, accountingOutboxTable, i*r.router.TablePerDB()+j)
			var cnt int64
			if err := db.WithContext(ctx).Table(tbl).
				Where("status = ?", domain.AccountingOutboxFailed).
				Count(&cnt).Error; err != nil {
				return total, fmt.Errorf("count failed on %s: %w", tbl, err)
			}
			total += cnt
		}
	}
	return total, nil
}

// StatsPending 跨所有分片统计 pending 行数 + 最老一条的 created。
//
// 实现：每张分片表跑两条 SQL（COUNT + MIN(created)），跨 100 表合计。
// 对每张表 due rows（next_attempt_at <= now or NULL）才算 lag —— 未到时间的
// 行不算积压。pending 总数 = 所有 due 行的总和。
//
// 跨 100 张表的 COUNT 比较贵，但每个 worker tick（500ms-2s）只跑一次，
// 跟 sweeper / cleanup 同量级，不影响 hot path。
func (r *accountingOutboxRepo) StatsPending(ctx context.Context) (int64, time.Time, error) {
	var total int64
	var oldest time.Time
	now := time.Now()
	for i := 0; i < r.router.DBCount(); i++ {
		db, err := r.mgr.GetShard(i)
		if err != nil {
			return total, oldest, err
		}
		for j := 0; j < r.router.TablePerDB(); j++ {
			tbl := r.router.TableName(ctx, accountingOutboxTable, i*r.router.TablePerDB()+j)
			var cnt int64
			q := db.WithContext(ctx).Table(tbl).
				Where("status = ?", domain.AccountingOutboxPending).
				Where("next_attempt_at IS NULL OR next_attempt_at <= ?", now)
			if err := q.Count(&cnt).Error; err != nil {
				return total, oldest, fmt.Errorf("stats pending count on %s: %w", tbl, err)
			}
			if cnt == 0 {
				continue
			}
			total += cnt
			var ts time.Time
			if err := db.WithContext(ctx).Table(tbl).
				Where("status = ?", domain.AccountingOutboxPending).
				Where("next_attempt_at IS NULL OR next_attempt_at <= ?", now).
				Select("MIN(created)").Row().Scan(&ts); err == nil && !ts.IsZero() {
				if oldest.IsZero() || ts.Before(oldest) {
					oldest = ts
				}
			}
		}
	}
	return total, oldest, nil
}
