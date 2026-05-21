package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
)

// FlowAnchorRouteRepository 管理 flow_anchor_route 路由索引表（100 片）。
//
// 【方向 B 关键基础设施】用途：通过 (flow_id, logical_account_id) → account_no 解析，
// 让 router 知道"本资金流在某 logical_account 上锁定到了哪个 instance"。
//
// 分片：按 flow_id FNV-1a 哈希到 100 片。
//
// 不变量（I0 物化）：
//   - uk_flow_logical 保证 (flow_id, logical_account_id) 唯一
//   - 一旦写入，flow 在该 LA 上的 account_no 锁定
//   - instance 轮换（active 切换）不修改 routing → flow 永远找回原锁定 instance
//   - 仅强制迁移才会更新 account_no + chain_depth++
//
// 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4（方向 B 章节）
type FlowAnchorRouteRepository interface {
	// RouteByFlowID 路由：flow_id → (db, table)。FNV-1a 哈希。
	RouteByFlowID(flowID string) (dbIndex, globalTableIndex int)

	// InsertInTx 首次锚定时写入路由行。
	// 唯一冲突 (uk_flow_logical) 返回 ErrFlowRouteAlreadyExists（业务方应读已有 anchor 处理）。
	InsertInTx(ctx context.Context, tx *gorm.DB, route *model.FlowAnchorRoute, globalTableIndex int) error

	// Insert 不带事务的快捷方法（首次锚定的路由写入是独立步骤）。
	Insert(ctx context.Context, route *model.FlowAnchorRoute) error

	// GetByFlowAndLogical 路由层热路径主要入口：通过 (flow_id, LA_id) 找 account_no。
	// 未命中返回 (nil, nil) —— 表示这是 flow 在该 LA 上的首次操作。
	GetByFlowAndLogical(ctx context.Context, flowID string, logicalAccountID int64) (*model.FlowAnchorRoute, error)

	// UpdateAccountNoInTx 强制迁移时更新 account_no + chain_depth++。CAS on version。
	UpdateAccountNoInTx(ctx context.Context, tx *gorm.DB, globalTableIndex int,
		routeID int64, expectedVersion int64, newAccountNo string) error
}

// 错误：route 表特有错误。
var (
	ErrFlowRouteAlreadyExists  = errors.New("flow_anchor_route already exists (uk_flow_logical)")
	ErrFlowRouteVersionConflict = errors.New("flow_anchor_route version conflict (CAS failed)")
	ErrFlowRouteShardMisrouted = errors.New("flow_anchor_route shard index out of range")
)

type flowAnchorRouteRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewFlowAnchorRouteRepository 创建实例。
func NewFlowAnchorRouteRepository(dbManager *database.Manager, router *sharding.Router) FlowAnchorRouteRepository {
	return &flowAnchorRouteRepository{dbManager: dbManager, router: router}
}

// RouteByFlowID FNV-1a hash of flow_id mod 100 → globalTblIdx; dbIdx = globalTblIdx / 10
func (r *flowAnchorRouteRepository) RouteByFlowID(flowID string) (dbIndex, globalTableIndex int) {
	if flowID == "" {
		return 0, 0
	}
	gtblIdx := hashStringMod100(flowID)
	dbIdx := gtblIdx / sharding.ShardTablePerDB
	return dbIdx, gtblIdx
}

func (r *flowAnchorRouteRepository) shardTableName(ctx context.Context, globalTableIndex int) (string, error) {
	if globalTableIndex < 0 || globalTableIndex >= sharding.ShardTableTotal {
		return "", fmt.Errorf("%w: globalTableIndex=%d", ErrFlowRouteShardMisrouted, globalTableIndex)
	}
	return r.router.TableName(ctx, "flow_anchor_route", globalTableIndex), nil
}

func (r *flowAnchorRouteRepository) dbForShard(globalTableIndex int) (*gorm.DB, error) {
	if globalTableIndex < 0 || globalTableIndex >= sharding.ShardTableTotal {
		return nil, fmt.Errorf("%w: globalTableIndex=%d", ErrFlowRouteShardMisrouted, globalTableIndex)
	}
	dbIdx := globalTableIndex / sharding.ShardTablePerDB
	db, err := r.dbManager.GetDB(dbIdx)
	if err != nil {
		return nil, fmt.Errorf("flow_anchor_route: get db[%d]: %w", dbIdx, err)
	}
	return db, nil
}

func (r *flowAnchorRouteRepository) validateInput(route *model.FlowAnchorRoute) error {
	if route == nil {
		return errors.New("flow_anchor_route: nil input")
	}
	if route.FlowID == "" {
		return errors.New("flow_anchor_route: flow_id required")
	}
	if route.LogicalAccountID == 0 {
		return errors.New("flow_anchor_route: logical_account_id required")
	}
	if route.AccountNo == "" {
		return errors.New("flow_anchor_route: account_no required")
	}
	return nil
}

func (r *flowAnchorRouteRepository) InsertInTx(
	ctx context.Context, tx *gorm.DB, route *model.FlowAnchorRoute, globalTableIndex int,
) error {
	if err := r.validateInput(route); err != nil {
		return err
	}
	// 路由校验
	_, expectedTbl := r.RouteByFlowID(route.FlowID)
	if expectedTbl != globalTableIndex {
		return fmt.Errorf("%w: caller said gtbl=%d but hash(%q)=%d",
			ErrFlowRouteShardMisrouted, globalTableIndex, route.FlowID, expectedTbl)
	}
	if route.CreatedAt.IsZero() {
		route.CreatedAt = time.Now().UTC()
	}
	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return err
	}
	if err := tx.WithContext(ctx).Table(tableName).Create(route).Error; err != nil {
		if isDuplicateKeyErr(err) {
			return fmt.Errorf("%w: flow_id=%s la_id=%d",
				ErrFlowRouteAlreadyExists, route.FlowID, route.LogicalAccountID)
		}
		return fmt.Errorf("flow_anchor_route: insert: %w", err)
	}
	return nil
}

// Insert 不带 tx 的快捷方法（routing 写入是与 anchor 写入分离的步骤）。
func (r *flowAnchorRouteRepository) Insert(ctx context.Context, route *model.FlowAnchorRoute) error {
	if err := r.validateInput(route); err != nil {
		return err
	}
	_, gtblIdx := r.RouteByFlowID(route.FlowID)
	db, err := r.dbForShard(gtblIdx)
	if err != nil {
		return err
	}
	return r.InsertInTx(ctx, db, route, gtblIdx)
}

func (r *flowAnchorRouteRepository) GetByFlowAndLogical(
	ctx context.Context, flowID string, logicalAccountID int64,
) (*model.FlowAnchorRoute, error) {
	if flowID == "" {
		return nil, errors.New("flow_anchor_route: empty flow_id")
	}
	_, gtblIdx := r.RouteByFlowID(flowID)
	tableName, err := r.shardTableName(ctx, gtblIdx)
	if err != nil {
		return nil, err
	}
	db, err := r.dbForShard(gtblIdx)
	if err != nil {
		return nil, err
	}
	var row model.FlowAnchorRoute
	res := db.WithContext(ctx).Table(tableName).
		Where("flow_id = ? AND logical_account_id = ?", flowID, logicalAccountID).
		Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("flow_anchor_route: get: %w", res.Error)
	}
	return &row, nil
}

func (r *flowAnchorRouteRepository) UpdateAccountNoInTx(
	ctx context.Context, tx *gorm.DB, globalTableIndex int,
	routeID int64, expectedVersion int64, newAccountNo string,
) error {
	if newAccountNo == "" {
		return errors.New("flow_anchor_route: empty new_account_no")
	}
	tableName, err := r.shardTableName(ctx, globalTableIndex)
	if err != nil {
		return err
	}
	res := tx.WithContext(ctx).Table(tableName).
		Where("id = ? AND version = ?", routeID, expectedVersion).
		Updates(map[string]any{
			"account_no":            newAccountNo,
			"migration_chain_depth": gorm.Expr("migration_chain_depth + 1"),
			"version":               gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("flow_anchor_route: update account_no: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id=%d expected_version=%d",
			ErrFlowRouteVersionConflict, routeID, expectedVersion)
	}
	return nil
}
