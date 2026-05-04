package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiongwp/payment-channel/internal/domain"
	"github.com/xiongwp/payment-channel/internal/sharding"
)

// AcquirerTxRepository acquirer_tx 分片仓储。
type AcquirerTxRepository interface {
	Insert(ctx context.Context, tx *domain.AcquirerTx) error
	FindByIdem(ctx context.Context, piID, adapter, idem string) (*domain.AcquirerTx, error)
	FindByID(ctx context.Context, piID, aqID string) (*domain.AcquirerTx, error)
	UpdateResult(ctx context.Context, piID string, id uint64, fields map[string]any) error
	// ListPendingRetries 返回 state=failed AND next_retry_at<=now —— 注意：
	// 修复 P0-1 后这条路径只用于重发「明确失败」的 op（4xx 业务错误）。
	// state=unknown 走 ListUnknownTxs + Query 推进，绝不重发原请求。
	ListPendingRetries(ctx context.Context, limit int) ([]*domain.AcquirerTx, error)
	// ListUnknownTxs 返回 state=unknown OR state=pending 且 created_at < older
	// 的行（即超时还没拿到响应的）；交给 PendingQueryWorker 用 Query 推进。
	// olderThan: 至少多久没动静才捞（给同步路径让位，避免抢占同笔 charge 的
	// inflight UPDATE 写）；queryThrottle: 上次 query 推进时间不能比 now-queryThrottle
	// 还新（避免高频空轮询）。
	ListUnknownTxs(ctx context.Context, limit int, olderThan, queryThrottle time.Duration) ([]*domain.AcquirerTx, error)
	// ListStuckPending 返回 state=pending|unknown AND created_at<now-stuckAfter
	// 的「僵尸」行；调用方把它们走 Cancel/Void 关单（最终一致兜底）。
	ListStuckPending(ctx context.Context, limit int, stuckAfter time.Duration) ([]*domain.AcquirerTx, error)
}

type acquirerTxRepo struct {
	mgr    *Manager
	router *sharding.Router
}

func NewAcquirerTxRepository(mgr *Manager, r *sharding.Router) AcquirerTxRepository {
	return &acquirerTxRepo{mgr: mgr, router: r}
}

// acquirerTxRow gorm 映射行。tag 用 column + gorm:"-"; 有的字段要 JSON 序列化。
type acquirerTxRow struct {
	ID              uint64  `gorm:"column:id;primaryKey"`
	AqID            string  `gorm:"column:aq_id"`
	PiID            string  `gorm:"column:pi_id"`
	Adapter         string  `gorm:"column:adapter"`
	Action          string  `gorm:"column:action"`
	IdempotencyKey  string  `gorm:"column:idempotency_key"`
	State           string  `gorm:"column:state"`
	ExternalRefNo   string  `gorm:"column:external_ref_no"`
	Amount          int64   `gorm:"column:amount"`
	Currency        string  `gorm:"column:currency"`
	FailureCode     string  `gorm:"column:failure_code"`
	RawFailureCode  string  `gorm:"column:raw_failure_code"`
	RequestMethod   string  `gorm:"column:request_method"`
	RequestURL      string  `gorm:"column:request_url"`
	RequestHeaders  string  `gorm:"column:request_headers"`
	RequestBody     string  `gorm:"column:request_body"`
	ResponseStatus  int     `gorm:"column:response_status"`
	ResponseHeaders string  `gorm:"column:response_headers"`
	ResponseBody    string  `gorm:"column:response_body"`
	LatencyMs       int     `gorm:"column:latency_ms"`
	Attempt         int     `gorm:"column:attempt"`
	NextRetryAt     *string `gorm:"column:next_retry_at"`
	LastQueryAt     *string `gorm:"column:last_query_at"`
	QueryCount      int     `gorm:"column:query_count"`
	CreatedAt       string  `gorm:"column:created_at;default:CURRENT_TIMESTAMP;->"`
	UpdatedAt       string  `gorm:"column:updated_at;default:CURRENT_TIMESTAMP;->"`
}

func (r *acquirerTxRepo) table(ctx context.Context, piID string) (*gorm.DB, string, error) {
	db, tblIdx := r.router.RouteByPrefixedID(piID)
	shard, err := r.mgr.GetShard(db)
	if err != nil {
		return nil, "", err
	}
	return shard, r.router.TableName(ctx, "acquirer_tx", tblIdx), nil
}

func (r *acquirerTxRepo) Insert(ctx context.Context, tx *domain.AcquirerTx) error {
	shard, tbl, err := r.table(ctx, tx.PiID)
	if err != nil {
		return err
	}
	row := toRow(tx)
	res := shard.WithContext(ctx).Table(tbl).Create(&row)
	if res.Error != nil {
		if isDup(res.Error) {
			return domain.ErrIdempotentHit
		}
		return res.Error
	}
	tx.ID = row.ID
	return nil
}

func (r *acquirerTxRepo) FindByIdem(ctx context.Context, piID, adapter, idem string) (*domain.AcquirerTx, error) {
	shard, tbl, err := r.table(ctx, piID)
	if err != nil {
		return nil, err
	}
	var row acquirerTxRow
	err = shard.WithContext(ctx).Table(tbl).
		Where("adapter = ? AND idempotency_key = ?", adapter, idem).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return fromRow(&row), nil
}

func (r *acquirerTxRepo) FindByID(ctx context.Context, piID, aqID string) (*domain.AcquirerTx, error) {
	shard, tbl, err := r.table(ctx, piID)
	if err != nil {
		return nil, err
	}
	var row acquirerTxRow
	err = shard.WithContext(ctx).Table(tbl).Where("aq_id = ?", aqID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrAcquirerTxNotFound
	}
	if err != nil {
		return nil, err
	}
	return fromRow(&row), nil
}

func (r *acquirerTxRepo) UpdateResult(ctx context.Context, piID string, id uint64, fields map[string]any) error {
	shard, tbl, err := r.table(ctx, piID)
	if err != nil {
		return err
	}
	return shard.WithContext(ctx).Table(tbl).Where("id = ?", id).Updates(fields).Error
}

// ListPendingRetries 扫描所有分片，取 state=failed AND next_retry_at<=now 的行。
//
// 修复 P0-1 后：仅 state=failed（明确业务失败 → 可安全重发原请求）走这条路径。
// state=unknown 走 ListUnknownTxs + Query。
//
// 优化：100 张分片表 fan-out 并行扫，总延迟 ≈ 单片耗时（而不是 100×）。每分片
// 最多取 perShardLimit 行，最后汇总不超过 limit。DB 连接池 MaxOpen=100 足够吃下
// 这批并发查询。
func (r *acquirerTxRepo) ListPendingRetries(ctx context.Context, limit int) ([]*domain.AcquirerTx, error) {
	if limit <= 0 {
		limit = 200
	}
	shards := r.router.AllShards()
	// 每分片最多取这么多；总分片 × 每片上限 应该大于 limit 才能摸到 limit。
	perShardLimit := (limit + len(shards) - 1) / len(shards)
	if perShardLimit < 16 {
		perShardLimit = 16
	}

	type shardResult struct {
		rows []acquirerTxRow
		err  error
	}
	results := make([]shardResult, len(shards))

	var wg sync.WaitGroup
	for i, pair := range shards {
		dbIdx, tblIdx := pair[0], pair[1]
		shard, err := r.mgr.GetShard(dbIdx)
		if err != nil {
			results[i] = shardResult{err: err}
			continue
		}
		tbl := r.router.TableName(ctx, "acquirer_tx", tblIdx)
		wg.Add(1)
		go func(idx int, db *gorm.DB, table string) {
			defer wg.Done()
			var rows []acquirerTxRow
			if err := db.WithContext(ctx).Table(table).
				Where("state = ? AND next_retry_at IS NOT NULL AND next_retry_at <= NOW()", string(domain.AcquirerTxFailed)).
				Order("next_retry_at ASC").
				Limit(perShardLimit).
				Find(&rows).Error; err != nil {
				results[idx] = shardResult{err: err}
				return
			}
			results[idx] = shardResult{rows: rows}
		}(i, shard, tbl)
	}
	wg.Wait()

	out := make([]*domain.AcquirerTx, 0, limit)
	for _, res := range results {
		if res.err != nil {
			return nil, res.err
		}
		for i := range res.rows {
			out = append(out, fromRow(&res.rows[i]))
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// ListUnknownTxs see interface doc.
func (r *acquirerTxRepo) ListUnknownTxs(ctx context.Context, limit int, olderThan, queryThrottle time.Duration) ([]*domain.AcquirerTx, error) {
	if limit <= 0 {
		limit = 200
	}
	if olderThan <= 0 {
		olderThan = 30 * time.Second
	}
	if queryThrottle <= 0 {
		queryThrottle = 30 * time.Second
	}
	shards := r.router.AllShards()
	perShardLimit := (limit + len(shards) - 1) / len(shards)
	if perShardLimit < 16 {
		perShardLimit = 16
	}
	type shardResult struct {
		rows []acquirerTxRow
		err  error
	}
	results := make([]shardResult, len(shards))
	var wg sync.WaitGroup
	for i, pair := range shards {
		dbIdx, tblIdx := pair[0], pair[1]
		shard, err := r.mgr.GetShard(dbIdx)
		if err != nil {
			results[i] = shardResult{err: err}
			continue
		}
		tbl := r.router.TableName(ctx, "acquirer_tx", tblIdx)
		wg.Add(1)
		go func(idx int, db *gorm.DB, table string) {
			defer wg.Done()
			var rows []acquirerTxRow
			if err := db.WithContext(ctx).Table(table).
				Where(
					"state IN (?, ?) AND created_at <= DATE_SUB(NOW(), INTERVAL ? SECOND) AND (last_query_at IS NULL OR last_query_at <= DATE_SUB(NOW(), INTERVAL ? SECOND))",
					string(domain.AcquirerTxUnknown), string(domain.AcquirerTxPending),
					int(olderThan.Seconds()), int(queryThrottle.Seconds()),
				).
				Order("created_at ASC").
				Limit(perShardLimit).
				Find(&rows).Error; err != nil {
				results[idx] = shardResult{err: err}
				return
			}
			results[idx] = shardResult{rows: rows}
		}(i, shard, tbl)
	}
	wg.Wait()

	out := make([]*domain.AcquirerTx, 0, limit)
	for _, res := range results {
		if res.err != nil {
			return nil, res.err
		}
		for i := range res.rows {
			out = append(out, fromRow(&res.rows[i]))
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// ListStuckPending see interface doc.
func (r *acquirerTxRepo) ListStuckPending(ctx context.Context, limit int, stuckAfter time.Duration) ([]*domain.AcquirerTx, error) {
	if limit <= 0 {
		limit = 100
	}
	if stuckAfter <= 0 {
		stuckAfter = 24 * time.Hour
	}
	shards := r.router.AllShards()
	perShardLimit := (limit + len(shards) - 1) / len(shards)
	if perShardLimit < 8 {
		perShardLimit = 8
	}
	type shardResult struct {
		rows []acquirerTxRow
		err  error
	}
	results := make([]shardResult, len(shards))
	var wg sync.WaitGroup
	for i, pair := range shards {
		dbIdx, tblIdx := pair[0], pair[1]
		shard, err := r.mgr.GetShard(dbIdx)
		if err != nil {
			results[i] = shardResult{err: err}
			continue
		}
		tbl := r.router.TableName(ctx, "acquirer_tx", tblIdx)
		wg.Add(1)
		go func(idx int, db *gorm.DB, table string) {
			defer wg.Done()
			var rows []acquirerTxRow
			if err := db.WithContext(ctx).Table(table).
				Where(
					"state IN (?, ?) AND created_at <= DATE_SUB(NOW(), INTERVAL ? SECOND)",
					string(domain.AcquirerTxUnknown), string(domain.AcquirerTxPending),
					int(stuckAfter.Seconds()),
				).
				Order("created_at ASC").
				Limit(perShardLimit).
				Find(&rows).Error; err != nil {
				results[idx] = shardResult{err: err}
				return
			}
			results[idx] = shardResult{rows: rows}
		}(i, shard, tbl)
	}
	wg.Wait()
	out := make([]*domain.AcquirerTx, 0, limit)
	for _, res := range results {
		if res.err != nil {
			return nil, res.err
		}
		for i := range res.rows {
			out = append(out, fromRow(&res.rows[i]))
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// ---- row <-> domain --------------------------------------------------------

func toRow(tx *domain.AcquirerTx) acquirerTxRow {
	reqH, _ := json.Marshal(tx.RequestHeaders)
	respH, _ := json.Marshal(tx.ResponseHeaders)
	row := acquirerTxRow{
		ID:              tx.ID,
		AqID:            tx.AqID,
		PiID:            tx.PiID,
		Adapter:         tx.Adapter,
		Action:          string(tx.Action),
		IdempotencyKey:  tx.IdempotencyKey,
		State:           string(tx.State),
		ExternalRefNo:   tx.ExternalRefNo,
		Amount:          tx.Amount,
		Currency:        tx.Currency,
		FailureCode:     tx.FailureCode,
		RawFailureCode:  tx.RawFailureCode,
		RequestMethod:   tx.RequestMethod,
		RequestURL:      tx.RequestURL,
		RequestHeaders:  string(reqH),
		RequestBody:     tx.RequestBody,
		ResponseStatus:  tx.ResponseStatus,
		ResponseHeaders: string(respH),
		ResponseBody:    tx.ResponseBody,
		LatencyMs:       tx.LatencyMs,
		Attempt:         tx.Attempt,
		QueryCount:      tx.QueryCount,
	}
	if tx.NextRetry != nil {
		s := tx.NextRetry.UTC().Format("2006-01-02 15:04:05")
		row.NextRetryAt = &s
	}
	if tx.LastQueryAt != nil {
		s := tx.LastQueryAt.UTC().Format("2006-01-02 15:04:05")
		row.LastQueryAt = &s
	}
	return row
}

func fromRow(row *acquirerTxRow) *domain.AcquirerTx {
	tx := &domain.AcquirerTx{
		ID:             row.ID,
		AqID:           row.AqID,
		PiID:           row.PiID,
		Adapter:        row.Adapter,
		Action:         domain.AcquirerAction(row.Action),
		IdempotencyKey: row.IdempotencyKey,
		State:          domain.AcquirerTxState(row.State),
		ExternalRefNo:  row.ExternalRefNo,
		Amount:         row.Amount,
		Currency:       row.Currency,
		FailureCode:    row.FailureCode,
		RawFailureCode: row.RawFailureCode,
		RequestMethod:  row.RequestMethod,
		RequestURL:     row.RequestURL,
		RequestBody:    row.RequestBody,
		ResponseStatus: row.ResponseStatus,
		ResponseBody:   row.ResponseBody,
		LatencyMs:      row.LatencyMs,
		Attempt:        row.Attempt,
		QueryCount:     row.QueryCount,
	}
	if row.RequestHeaders != "" {
		_ = json.Unmarshal([]byte(row.RequestHeaders), &tx.RequestHeaders)
	}
	if row.ResponseHeaders != "" {
		_ = json.Unmarshal([]byte(row.ResponseHeaders), &tx.ResponseHeaders)
	}
	if row.LastQueryAt != nil {
		if t, err := time.Parse("2006-01-02 15:04:05", *row.LastQueryAt); err == nil {
			tx.LastQueryAt = &t
		}
	}
	return tx
}

// isDup 判断 MySQL 1062（Duplicate entry）错误，用来把 UNIQUE 冲突映射成幂等命中。
func isDup(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1062 {
		return true
	}
	// 某些驱动封装成 error message 就直接 grep。
	return strings.Contains(err.Error(), "Duplicate entry")
}

// 供外层需要拼接带 sprintf 的场景（目前未用到，仅作占位防止 fmt 导入告警）
var _ = fmt.Sprintf
