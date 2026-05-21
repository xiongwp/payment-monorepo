package repository

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
)

// AnchorRepository 管理 tx_account_anchor 分片表（100 片）。
//
// 分片策略：(related_request_id) 经 FNV-1a 哈希 mod 100 → globalTblIdx；
//          dbIdx = globalTblIdx / 10。
// 与 account_transaction 表的对齐由 booking router 保证（router 决定 entry 写哪个
// 分片 → 该分片同时承接 anchor 写入）。
//
// 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4, §3.4.1, §5.2
type AnchorRepository interface {
	// RouteByRequestID 暴露分片路由函数，便于 booking router 在写入 entry 前
	// 决定落到哪个分片。
	RouteByRequestID(relatedRequestID string) (dbIndex, globalTableIndex int)

	// InsertInTx 在调用方提供的 gorm.DB 事务上插入 anchor。
	// 必须与对应 entry 写入处于同一本地事务。
	// 唯一冲突 (uk_req_logical) 返回 ErrAnchorAlreadyExists。
	InsertInTx(ctx context.Context, tx *gorm.DB, anchor *model.TxAccountAnchor, globalTableIndex int) error

	// GetByRequestAndLogical 按 (related_request_id, logical_account_id) 查询；
	// 未命中返回 (nil, nil)。
	GetByRequestAndLogical(ctx context.Context, relatedRequestID string, logicalAccountID int64) (*model.TxAccountAnchor, error)

	// GetByRequestAndLogicalInTx 同上，但在指定事务里读。
	// 用于路由层"读—改—写"路径，避免幻读。
	GetByRequestAndLogicalInTx(ctx context.Context, tx *gorm.DB, relatedRequestID string, logicalAccountID int64, globalTableIndex int) (*model.TxAccountAnchor, error)

	// UpdatePostingInTx anchor 上记一笔分录后更新计数与 direction_mask。
	// CAS on version。CAS 失败返回 ErrAnchorVersionConflict。
	UpdatePostingInTx(ctx context.Context, tx *gorm.DB, globalTableIndex int,
		anchorID int64, expectedVersion int64,
		newPostingAt time.Time, newMask model.AnchorDirectionMask) error

	// UpdateStatusInTx 改 anchor.status；状态转换合法性由调用方先用 model.CanTransitionAnchor
	// 检验，repo 层再做 CAS。
	UpdateStatusInTx(ctx context.Context, tx *gorm.DB, globalTableIndex int,
		anchorID int64, expectedVersion int64, newStatus model.AnchorStatus) error

	// MarkMigratedInTx 强制迁移：填 migrated_to_account_no / migration_voucher_no /
	// migration_chain_depth++ / status=migrated。CAS on version。
	MarkMigratedInTx(ctx context.Context, tx *gorm.DB, globalTableIndex int,
		anchorID int64, expectedVersion int64,
		targetAccountNo string, voucherNo string) error

	// CountOpenByAccountNo 收敛 job 使用：统计某 instance 上 status ∈ {trying, active}
	// 的 anchor 数量。
	CountOpenByAccountNo(ctx context.Context, accountNo string, globalTableIndex int) (int64, error)

	// CountStuckByAccountNo 收敛 job：stuck > 0 时阻断推进到 frozen。
	CountStuckByAccountNo(ctx context.Context, accountNo string, globalTableIndex int) (int64, error)

	// OldestOpenAnchoredAt 收敛 job：找到 instance 上最老的 open anchor 的 anchored_at；
	// 无 open 返回 (nil, nil)。
	OldestOpenAnchoredAt(ctx context.Context, accountNo string, globalTableIndex int) (*time.Time, error)

	// ListStuckCandidates 列出所有 status=active 且 last_posting_at < cutoff 的 anchor，
	// 用于强制迁移 / stuck 检测。返回 globalTableIndex 维度的结果（调用方按分片合并）。
	ListStuckCandidates(ctx context.Context, globalTableIndex int, accountNo string, cutoff time.Time, limit int) ([]*model.TxAccountAnchor, error)
}

// 错误：repo 层暴露的标准错误。
var (
	ErrAnchorAlreadyExists      = errors.New("tx_account_anchor already exists (uk_req_logical)")
	ErrAnchorVersionConflict    = errors.New("tx_account_anchor version conflict (CAS failed)")
	ErrAnchorNotFound           = errors.New("tx_account_anchor not found")
	ErrAnchorShardMisrouted     = errors.New("anchor shard index out of range")
)

type anchorRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewAnchorRepository 创建实例。
func NewAnchorRepository(dbManager *database.Manager, router *sharding.Router) AnchorRepository {
	return &anchorRepository{dbManager: dbManager, router: router}
}

// hashStringMod100 FNV-1a 64bit hash mod ShardTableTotal。
//
// 用 FNV 而非 SHA：anchor 分片只需要分布均匀，不需要加密强度，FNV 更快且与
// payment-monorepo 内其他热点路径选择一致（low-overhead）。
func hashStringMod100(s string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int(h.Sum64() % uint64(sharding.ShardTableTotal))
}

// RouteByRequestID see interface.
func (r *anchorRepository) RouteByRequestID(relatedRequestID string) (dbIndex, globalTableIndex int) {
	if relatedRequestID == "" {
		// fail-safe：空 request_id 落到 shard 0，并由上层 validation 拒绝
		return 0, 0
	}
	gtblIdx := hashStringMod100(relatedRequestID)
	dbIdx := gtblIdx / sharding.ShardTablePerDB
	return dbIdx, gtblIdx
}

// shardTableName 内部：根据 globalTableIndex 生成实际表名。
// 注意：使用 router.TableName(ctx, ...) 可让 shadow 流量自动落到 _shadow 表。
func (r *anchorRepository) shardTableName(ctx context.Context, globalTableIndex int) (string, error) {
	if globalTableIndex < 0 || globalTableIndex >= sharding.ShardTableTotal {
		return "", fmt.Errorf("%w: globalTableIndex=%d", ErrAnchorShardMisrouted, globalTableIndex)
	}
	return r.router.TableName(ctx, "tx_account_anchor", globalTableIndex), nil
}

func (r *anchorRepository) dbForShard(globalTableIndex int) (*gorm.DB, error) {
	if globalTableIndex < 0 || globalTableIndex >= sharding.ShardTableTotal {
		return nil, fmt.Errorf("%w: globalTableIndex=%d", ErrAnchorShardMisrouted, globalTableIndex)
	}
	dbIdx := globalTableIndex / sharding.ShardTablePerDB
	db, err := r.dbManager.GetDB(dbIdx)
	if err != nil {
		return nil, fmt.Errorf("anchor: get db[%d]: %w", dbIdx, err)
	}
	return db, nil
}

func (r *anchorRepository) InsertInTx(
	ctx context.Context, tx *gorm.DB, anchor *model.TxAccountAnchor, globalTableIndex int,
) error {
	if anchor == nil {
		return errors.New("anchor: nil input")
	}
	if anchor.RelatedRequestID == "" {
		return errors.New("anchor: related_request_id required")
	}
	if anchor.LogicalAccountID == 0 {
		return errors.New("anchor: logical_account_id required")
	}
	if anchor.AccountNo == "" {
		return errors.New("anchor: account_no required")
	}
	if anchor.AnchoredAt.IsZero() {
		anchor.AnchoredAt = time.Now().UTC()
	}
	if anchor.LastPostingAt.IsZero() {
		anchor.LastPostingAt = anchor.AnchoredAt
	}
	if anchor.PostingCount <= 0 {
		anchor.PostingCount = 1
	}

	// 路由校验：调用方传入的 globalTableIndex 必须与 hash(related_request_id) 一致
	// 否则就是 booking router bug（写入串片）。
	_, expectedTbl := r.RouteByRequestID(anchor.RelatedRequestID)
	if expectedTbl != globalTableIndex {
		return fmt.Errorf("%w: caller said gtbl=%d but hash(%q)=%d",
			ErrAnchorShardMisrouted, globalTableIndex, anchor.RelatedRequestID, expectedTbl)
	}

	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return err
	}
	// 直接 INSERT；唯一索引 uk_req_logical 由 DB 保证并发安全。
	if err := tx.WithContext(ctx).Table(tableName).Create(anchor).Error; err != nil {
		// MySQL duplicate key 错误 1062 → 包装为业务层可识别的 ErrAnchorAlreadyExists
		if isDuplicateKeyErr(err) {
			return fmt.Errorf("%w: req_id=%s la_id=%d",
				ErrAnchorAlreadyExists, anchor.RelatedRequestID, anchor.LogicalAccountID)
		}
		return fmt.Errorf("anchor: insert: %w", err)
	}
	return nil
}

func (r *anchorRepository) GetByRequestAndLogical(
	ctx context.Context, relatedRequestID string, logicalAccountID int64,
) (*model.TxAccountAnchor, error) {
	if relatedRequestID == "" {
		return nil, errors.New("anchor: empty request_id")
	}
	_, gtblIdx := r.RouteByRequestID(relatedRequestID)
	tableName, err := r.shardTableName(ctx, gtblIdx)
	if err != nil {
		return nil, err
	}
	db, err := r.dbForShard(gtblIdx)
	if err != nil {
		return nil, err
	}
	var row model.TxAccountAnchor
	res := db.WithContext(ctx).Table(tableName).
		Where("related_request_id = ? AND logical_account_id = ?", relatedRequestID, logicalAccountID).
		Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("anchor: get by (req,la): %w", res.Error)
	}
	return &row, nil
}

func (r *anchorRepository) GetByRequestAndLogicalInTx(
	ctx context.Context, tx *gorm.DB,
	relatedRequestID string, logicalAccountID int64, globalTableIndex int,
) (*model.TxAccountAnchor, error) {
	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return nil, err
	}
	var row model.TxAccountAnchor
	res := tx.WithContext(ctx).Table(tableName).
		Where("related_request_id = ? AND logical_account_id = ?", relatedRequestID, logicalAccountID).
		Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("anchor: get in tx: %w", res.Error)
	}
	return &row, nil
}

func (r *anchorRepository) UpdatePostingInTx(
	ctx context.Context, tx *gorm.DB, globalTableIndex int,
	anchorID int64, expectedVersion int64,
	newPostingAt time.Time, newMask model.AnchorDirectionMask,
) error {
	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return err
	}
	res := tx.WithContext(ctx).Table(tableName).
		Where("id = ? AND version = ?", anchorID, expectedVersion).
		Updates(map[string]any{
			"last_posting_at":  newPostingAt,
			"posting_count":    gorm.Expr("posting_count + 1"),
			"direction_mask":   newMask,
			"version":          gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("anchor: update posting: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id=%d expected_version=%d",
			ErrAnchorVersionConflict, anchorID, expectedVersion)
	}
	return nil
}

func (r *anchorRepository) UpdateStatusInTx(
	ctx context.Context, tx *gorm.DB, globalTableIndex int,
	anchorID int64, expectedVersion int64, newStatus model.AnchorStatus,
) error {
	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return err
	}
	res := tx.WithContext(ctx).Table(tableName).
		Where("id = ? AND version = ?", anchorID, expectedVersion).
		Updates(map[string]any{
			"status":  newStatus,
			"version": gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("anchor: update status: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id=%d expected_version=%d",
			ErrAnchorVersionConflict, anchorID, expectedVersion)
	}
	return nil
}

func (r *anchorRepository) MarkMigratedInTx(
	ctx context.Context, tx *gorm.DB, globalTableIndex int,
	anchorID int64, expectedVersion int64,
	targetAccountNo string, voucherNo string,
) error {
	if targetAccountNo == "" {
		return errors.New("anchor: MarkMigrated empty target account_no")
	}
	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return err
	}
	res := tx.WithContext(ctx).Table(tableName).
		Where("id = ? AND version = ? AND status = ?", anchorID, expectedVersion, model.AnchorStatusActive).
		Updates(map[string]any{
			"status":                  model.AnchorStatusMigrated,
			"migrated_to_account_no":  targetAccountNo,
			"migration_voucher_no":    voucherNo,
			"migration_chain_depth":   gorm.Expr("migration_chain_depth + 1"),
			"version":                 gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("anchor: mark migrated: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id=%d expected_version=%d (or status != active)",
			ErrAnchorVersionConflict, anchorID, expectedVersion)
	}
	return nil
}

func (r *anchorRepository) CountOpenByAccountNo(
	ctx context.Context, accountNo string, globalTableIndex int,
) (int64, error) {
	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return 0, err
	}
	db, err := r.dbForShard(globalTableIndex)
	if err != nil {
		return 0, err
	}
	var count int64
	res := db.WithContext(ctx).Table(tableName).
		Where("account_no = ? AND status IN ?", accountNo,
			[]model.AnchorStatus{model.AnchorStatusTrying, model.AnchorStatusActive}).
		Count(&count)
	if res.Error != nil {
		return 0, fmt.Errorf("anchor: count open: %w", res.Error)
	}
	return count, nil
}

func (r *anchorRepository) CountStuckByAccountNo(
	ctx context.Context, accountNo string, globalTableIndex int,
) (int64, error) {
	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return 0, err
	}
	db, err := r.dbForShard(globalTableIndex)
	if err != nil {
		return 0, err
	}
	var count int64
	res := db.WithContext(ctx).Table(tableName).
		Where("account_no = ? AND status = ?", accountNo, model.AnchorStatusStuck).
		Count(&count)
	if res.Error != nil {
		return 0, fmt.Errorf("anchor: count stuck: %w", res.Error)
	}
	return count, nil
}

func (r *anchorRepository) OldestOpenAnchoredAt(
	ctx context.Context, accountNo string, globalTableIndex int,
) (*time.Time, error) {
	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return nil, err
	}
	db, err := r.dbForShard(globalTableIndex)
	if err != nil {
		return nil, err
	}
	var row struct {
		AnchoredAt time.Time `gorm:"column:anchored_at"`
	}
	res := db.WithContext(ctx).Table(tableName).
		Select("MIN(anchored_at) AS anchored_at").
		Where("account_no = ? AND status IN ?", accountNo,
			[]model.AnchorStatus{model.AnchorStatusTrying, model.AnchorStatusActive}).
		Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("anchor: oldest open: %w", res.Error)
	}
	if row.AnchoredAt.IsZero() {
		return nil, nil
	}
	t := row.AnchoredAt
	return &t, nil
}

func (r *anchorRepository) ListStuckCandidates(
	ctx context.Context, globalTableIndex int, accountNo string, cutoff time.Time, limit int,
) ([]*model.TxAccountAnchor, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return nil, err
	}
	db, err := r.dbForShard(globalTableIndex)
	if err != nil {
		return nil, err
	}
	var rows []*model.TxAccountAnchor
	q := db.WithContext(ctx).Table(tableName).
		Where("status = ? AND last_posting_at < ?", model.AnchorStatusActive, cutoff)
	if accountNo != "" {
		q = q.Where("account_no = ?", accountNo)
	}
	if err := q.Order("anchored_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("anchor: list stuck candidates: %w", err)
	}
	return rows, nil
}

// isDuplicateKeyErr 检测 MySQL duplicate key 错误（错误码 1062）。
// 我们容忍 GORM/driver 升级时的错误类型变化——用错误消息子串而非具体类型。
func isDuplicateKeyErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// MySQL: "Error 1062: Duplicate entry"
	// MariaDB: similar
	// GORM wraps but preserves the original message.
	return contains(s, "1062") || contains(s, "Duplicate entry") || contains(s, "duplicate key")
}

// contains 不引入 strings 包以保持 repo 文件依赖面简洁。
func contains(s, substr string) bool {
	if substr == "" {
		return true
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
