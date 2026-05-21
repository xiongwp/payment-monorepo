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
// 【方向 B 分片】按 account_no 哈希分 100 片，与 account_transaction、account 同片。
// anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC。
//
// 查找路径：业务方按 (flow_id, logical_account_id) 想找 anchor，必须先查
// FlowAnchorRouteRepository 拿到 account_no，再来本表查。
//
// 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4, §3.4.1, §5.2
type AnchorRepository interface {
	// RouteByAccountNo 路由：account_no → (db, table)。复用 sharding.Router.RouteByAccountNo。
	RouteByAccountNo(accountNo string) (dbIndex, globalTableIndex int)

	// InsertInTx 在调用方提供的 gorm.DB 事务上插入 anchor。
	// **同事务必须与对应的 account_transaction 写入合并**（这是方向 B 的核心收益）。
	// 唯一冲突 (uk_flow_account) 返回 ErrAnchorAlreadyExists。
	InsertInTx(ctx context.Context, tx *gorm.DB, anchor *model.TxAccountAnchor, globalTableIndex int) error

	// GetByFlowAndAccount 按 (flow_id, account_no) 查询；未命中返回 (nil, nil)。
	// 调用方必须先通过 FlowAnchorRoute 拿到 account_no。
	GetByFlowAndAccount(ctx context.Context, flowID string, accountNo string) (*model.TxAccountAnchor, error)

	// GetByFlowAndAccountInTx 同上，但在指定事务里读。
	GetByFlowAndAccountInTx(ctx context.Context, tx *gorm.DB, flowID string, accountNo string, globalTableIndex int) (*model.TxAccountAnchor, error)

	// UpdatePostingInTx anchor 上记一笔分录后更新计数与 direction_mask。
	// CAS on version。CAS 失败返回 ErrAnchorVersionConflict。
	UpdatePostingInTx(ctx context.Context, tx *gorm.DB, globalTableIndex int,
		anchorID int64, expectedVersion int64,
		newPostingAt time.Time, newMask model.AnchorDirectionMask) error

	// UpdateStatusInTx 改 anchor.status；状态转换合法性由调用方先用 model.CanTransitionAnchor 检验。
	UpdateStatusInTx(ctx context.Context, tx *gorm.DB, globalTableIndex int,
		anchorID int64, expectedVersion int64, newStatus model.AnchorStatus) error

	// MarkMigratedInTx 强制迁移：填 migrated_to_account_no / migration_voucher_no /
	// migration_chain_depth++ / status=migrated。CAS on version。
	MarkMigratedInTx(ctx context.Context, tx *gorm.DB, globalTableIndex int,
		anchorID int64, expectedVersion int64,
		targetAccountNo string, voucherNo string) error

	// CountOpenByAccountNo 收敛 job 使用：统计某 instance 上 status ∈ {trying, active} 的 anchor 数量。
	// 【方向 B 收益】单分片查询，无需 fan-out。
	CountOpenByAccountNo(ctx context.Context, accountNo string) (int64, error)

	// CountStuckByAccountNo 收敛 job：stuck > 0 时阻断推进到 frozen。
	CountStuckByAccountNo(ctx context.Context, accountNo string) (int64, error)

	// OldestOpenAnchoredAt 收敛 job：找到 instance 上最老的 open anchor 的 anchored_at；无 open 返回 (nil, nil)。
	OldestOpenAnchoredAt(ctx context.Context, accountNo string) (*time.Time, error)

	// ListStuckCandidates 列出 status=active 且 last_posting_at < cutoff 的 anchor。
	ListStuckCandidates(ctx context.Context, accountNo string, cutoff time.Time, limit int) ([]*model.TxAccountAnchor, error)
}

// 错误：repo 层暴露的标准错误。
var (
	ErrAnchorAlreadyExists   = errors.New("tx_account_anchor already exists (uk_flow_account)")
	ErrAnchorVersionConflict = errors.New("tx_account_anchor version conflict (CAS failed)")
	ErrAnchorNotFound        = errors.New("tx_account_anchor not found")
	ErrAnchorShardMisrouted  = errors.New("anchor shard index out of range")
)

type anchorRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewAnchorRepository 创建实例。
func NewAnchorRepository(dbManager *database.Manager, router *sharding.Router) AnchorRepository {
	return &anchorRepository{dbManager: dbManager, router: router}
}

// RouteByAccountNo 通过 sharding.Router 路由（复用已有 account_no 路由逻辑，
// 保证 anchor 与 account/transaction 同分片）。
func (r *anchorRepository) RouteByAccountNo(accountNo string) (dbIndex, globalTableIndex int) {
	return r.router.RouteByAccountNo(accountNo)
}

// hashStringMod100 FNV-1a 64bit hash mod ShardTableTotal。
// 注意：anchor 表方向 B 不用这个（用 RouteByAccountNo），但保留给 flow_anchor_route 使用。
func hashStringMod100(s string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int(h.Sum64() % uint64(sharding.ShardTableTotal))
}

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
	if anchor.FlowID == "" {
		return errors.New("anchor: flow_id required")
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

	// 方向 B 路由校验：调用方传入的 globalTableIndex 必须与 RouteByAccountNo 一致
	_, expectedTbl := r.RouteByAccountNo(anchor.AccountNo)
	if expectedTbl != globalTableIndex {
		return fmt.Errorf("%w: caller said gtbl=%d but RouteByAccountNo(%q)=%d",
			ErrAnchorShardMisrouted, globalTableIndex, anchor.AccountNo, expectedTbl)
	}

	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return err
	}
	if err := tx.WithContext(ctx).Table(tableName).Create(anchor).Error; err != nil {
		if isDuplicateKeyErr(err) {
			return fmt.Errorf("%w: flow_id=%s account_no=%s",
				ErrAnchorAlreadyExists, anchor.FlowID, anchor.AccountNo)
		}
		return fmt.Errorf("anchor: insert: %w", err)
	}
	return nil
}

func (r *anchorRepository) GetByFlowAndAccount(
	ctx context.Context, flowID string, accountNo string,
) (*model.TxAccountAnchor, error) {
	if flowID == "" {
		return nil, errors.New("anchor: empty flow_id")
	}
	if accountNo == "" {
		return nil, errors.New("anchor: empty account_no")
	}
	_, gtblIdx := r.RouteByAccountNo(accountNo)
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
		Where("flow_id = ? AND account_no = ?", flowID, accountNo).
		Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("anchor: get by (flow,acc): %w", res.Error)
	}
	return &row, nil
}

func (r *anchorRepository) GetByFlowAndAccountInTx(
	ctx context.Context, tx *gorm.DB,
	flowID string, accountNo string, globalTableIndex int,
) (*model.TxAccountAnchor, error) {
	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return nil, err
	}
	var row model.TxAccountAnchor
	res := tx.WithContext(ctx).Table(tableName).
		Where("flow_id = ? AND account_no = ?", flowID, accountNo).
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
			"last_posting_at": newPostingAt,
			"posting_count":   gorm.Expr("posting_count + 1"),
			"direction_mask":  newMask,
			"version":         gorm.Expr("version + 1"),
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
			"status":                 model.AnchorStatusMigrated,
			"migrated_to_account_no": targetAccountNo,
			"migration_voucher_no":   voucherNo,
			"migration_chain_depth":  gorm.Expr("migration_chain_depth + 1"),
			"version":                gorm.Expr("version + 1"),
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

// CountOpenByAccountNo 方向 B 收益：单片查询，无 fan-out。
func (r *anchorRepository) CountOpenByAccountNo(ctx context.Context, accountNo string) (int64, error) {
	_, gtblIdx := r.RouteByAccountNo(accountNo)
	tableName, err := r.shardTableName(ctx, gtblIdx)
	if err != nil {
		return 0, err
	}
	db, err := r.dbForShard(gtblIdx)
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

func (r *anchorRepository) CountStuckByAccountNo(ctx context.Context, accountNo string) (int64, error) {
	_, gtblIdx := r.RouteByAccountNo(accountNo)
	tableName, err := r.shardTableName(ctx, gtblIdx)
	if err != nil {
		return 0, err
	}
	db, err := r.dbForShard(gtblIdx)
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
	ctx context.Context, accountNo string,
) (*time.Time, error) {
	_, gtblIdx := r.RouteByAccountNo(accountNo)
	tableName, err := r.shardTableName(ctx, gtblIdx)
	if err != nil {
		return nil, err
	}
	db, err := r.dbForShard(gtblIdx)
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
	ctx context.Context, accountNo string, cutoff time.Time, limit int,
) ([]*model.TxAccountAnchor, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	_, gtblIdx := r.RouteByAccountNo(accountNo)
	tableName, err := r.shardTableName(ctx, gtblIdx)
	if err != nil {
		return nil, err
	}
	db, err := r.dbForShard(gtblIdx)
	if err != nil {
		return nil, err
	}
	var rows []*model.TxAccountAnchor
	q := db.WithContext(ctx).Table(tableName).
		Where("status = ? AND last_posting_at < ? AND account_no = ?",
			model.AnchorStatusActive, cutoff, accountNo)
	if err := q.Order("anchored_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("anchor: list stuck candidates: %w", err)
	}
	return rows, nil
}

// isDuplicateKeyErr 检测 MySQL duplicate key 错误（错误码 1062）。
func isDuplicateKeyErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, "1062") || contains(s, "Duplicate entry") || contains(s, "duplicate key")
}

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
