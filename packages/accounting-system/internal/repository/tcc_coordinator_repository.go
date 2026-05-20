package repository

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
	"github.com/xiongwp/accounting-system/internal/metrics"
	"gorm.io/gorm"
)

// recordTransition 打 coordinator 状态机指标。
func recordTransition(from, to, outcome string) {
	metrics.TccCoordinatorTransitionsTotal.WithLabelValues(from, to, outcome).Inc()
}

// TccCoordinatorRepository TCC 全局协调者仓储
//
// 存储：分库分表（tcc_coordinator_00 ... tcc_coordinator_99），共 100 张表，分散在
// 10 个记账库（accounting_db_0 ~ accounting_db_9）中。路由规则与 account_transaction /
// tcc_transaction 一致：RouteByNumericStr(tccID) → (dbIndex, globalTableIndex)。
//
// 这样设计保证同一笔 booking 的 coordinator + tcc_transaction + account 都落在同一
// 分片，ListStuck / 状态机转换不会产生跨分片事务。
type TccCoordinatorRepository interface {
	// Create 创建协调者。cutDate / currency 由 booking 入口处决定，propagate
	// 到这里写入。同一 voucher 的所有 entry 必然共享此 cutDate 和 currency。
	// Recovery 路径补 confirm 时读 coord.Currency 写入流水，避免与原 booking 币种
	// 不一致 → trial balance 按 currency 过滤时单边出现 → 不平。
	Create(ctx context.Context, tccID, businessNo, cutDate, currency string, branchCount int) error
	TransitionToConfirming(ctx context.Context, tccID string) error
	// TransitionToConfirmed 把 phase 从 CONFIRMING 推进到 CONFIRMED。幂等。
	// 不再需要 finalize timestamp（cut_date 已经在 Create 时写入）。
	TransitionToConfirmed(ctx context.Context, tccID string) error
	TransitionToCancelled(ctx context.Context, tccID string) error
	// GetByTccID 读取协调者记录，不存在返回 (nil, nil)。
	GetByTccID(ctx context.Context, tccID string) (*model.TccCoordinator, error)
	// ListStuck 并发扫全 100 张分表，汇总所有仍处于 TRYING 或 CONFIRMING 且
	// updated_at < before 的记录；最多返回 limit 条（按 updated_at 升序）。
	ListStuck(ctx context.Context, before time.Time, limit int) ([]*model.TccCoordinator, error)
	// CountActiveByCutDate 统计 cut_date <= maxCutDate AND phase IN (TRYING, CONFIRMING)
	// 的 coord 行数（跨所有分片）。日切 drain 等待用：值 = 0 时所有 cut_date <= X 的
	// voucher 已 final（CONFIRMED 或 CANCELLED），扫描 account_transaction 安全。
	CountActiveByCutDate(ctx context.Context, maxCutDate string) (int64, error)
}

type tccCoordinatorRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

func NewTccCoordinatorRepository(dbManager *database.Manager, router *sharding.Router) TccCoordinatorRepository {
	return &tccCoordinatorRepository{dbManager: dbManager, router: router}
}

// shardTable 按 tccID 数字路由，返回对应分片的 (db, 物理表名)。
// tableName 形如 "tcc_coordinator_07"。
func (r *tccCoordinatorRepository) shardTable(ctx context.Context, tccID string) (*gorm.DB, string, error) {
	dbIndex, tblIdx := r.router.RouteByNumericStr(tccID)
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, "", err
	}
	// shadow 路由：raw Exec 不经过 callback，所以这里必须显式按 ctx 拼影子后缀。
	tableName := r.router.TableName(ctx, "tcc_coordinator", tblIdx)
	return db.WithContext(ctx), tableName, nil
}

func (r *tccCoordinatorRepository) Create(ctx context.Context, tccID, businessNo, cutDate, currency string, branchCount int) error {
	db, tableName, err := r.shardTable(ctx, tccID)
	if err != nil {
		return err
	}
	// INSERT IGNORE：重试场景下可能重复创建，忽略重复键错误。cut_date / currency 在
	// booking 入口已确定，propagate 到这里 — 同一 voucher 的所有分录的 cut_date 和
	// currency 必然 == 此值（recovery 路径读 coord 兜底，避免 hardcode "CNY"）。
	sql := fmt.Sprintf("INSERT IGNORE INTO %s (tcc_id, phase, business_no, cut_date, currency, branch_count) VALUES (?, ?, ?, ?, ?, ?)", tableName)
	return db.Exec(sql, tccID, model.TccPhaseTrying, businessNo, cutDate, currency, branchCount).Error
}

// TransitionToConfirming: TRYING → CONFIRMING。
// WHERE phase = TRYING：阻止 CANCELLED/CONFIRMED 被回滚到 CONFIRMING（状态机不允许回退）。
// RowsAffected = 0 视为异常：Recovery Worker 可能已判 Cancel，本次 Confirm 决不能继续，
// 否则"释放冻结后又应用余额" = 凭空多钱。返回显式错误让上层拒绝进入 Confirm。
func (r *tccCoordinatorRepository) TransitionToConfirming(ctx context.Context, tccID string) error {
	db, tableName, err := r.shardTable(ctx, tccID)
	if err != nil {
		recordTransition("TRYING", "CONFIRMING", "error")
		return err
	}
	res := db.Table(tableName).
		Where("tcc_id = ? AND phase = ?", tccID, model.TccPhaseTrying).
		Update("phase", model.TccPhaseConfirming)
	if res.Error != nil {
		recordTransition("TRYING", "CONFIRMING", "error")
		return res.Error
	}
	if res.RowsAffected == 0 {
		recordTransition("TRYING", "CONFIRMING", "rejected_illegal")
		return fmt.Errorf("tcc coordinator %s not in TRYING phase (likely cancelled or concurrent transition)", tccID)
	}
	recordTransition("TRYING", "CONFIRMING", "ok")
	return nil
}

// TransitionToConfirmed: CONFIRMING → CONFIRMED。幂等。
// cut_date 已在 Create 时写入，无需此处处理。
func (r *tccCoordinatorRepository) TransitionToConfirmed(ctx context.Context, tccID string) error {
	db, tableName, err := r.shardTable(ctx, tccID)
	if err != nil {
		recordTransition("CONFIRMING", "CONFIRMED", "error")
		return err
	}
	res := db.Table(tableName).
		Where("tcc_id = ? AND phase IN ?",
			tccID, []model.TccCoordinatorPhase{model.TccPhaseConfirming, model.TccPhaseConfirmed}).
		Update("phase", model.TccPhaseConfirmed)
	if res.Error != nil {
		recordTransition("CONFIRMING", "CONFIRMED", "error")
		return res.Error
	}
	if res.RowsAffected == 0 {
		recordTransition("CONFIRMING", "CONFIRMED", "rejected_illegal")
		return fmt.Errorf("tcc coordinator %s cannot transition to CONFIRMED (not in CONFIRMING/CONFIRMED phase; possible concurrent cancel)", tccID)
	}
	recordTransition("CONFIRMING", "CONFIRMED", "ok")
	return nil
}

// CountActiveByCutDate 见接口注释。跨所有 100 分表并行 SELECT COUNT(*) WHERE cut_date<=?
// AND phase IN (TRYING, CONFIRMING)，求和返回。
//
// 日切 drain wait 调用：循环至返回 0 或超时。0 → 所有 cut_date<=X 的 voucher 已落定，
// 此时 account_transaction WHERE cut_date=X 是 final state，扫描结果稳定 + 试算平衡。
func (r *tccCoordinatorRepository) CountActiveByCutDate(ctx context.Context, maxCutDate string) (int64, error) {
	activePhases := []model.TccCoordinatorPhase{model.TccPhaseTrying, model.TccPhaseConfirming}
	shards := r.router.GetAllShards()

	type shardResult struct {
		count int64
		err   error
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
				results[i].err = err
				return
			}
			tableName := r.router.GetTableName("tcc_coordinator", shard.TableIndex)
			var n int64
			if err := db.WithContext(ctx).Table(tableName).
				Where("cut_date <= ? AND phase IN ?", maxCutDate, activePhases).
				Count(&n).Error; err != nil {
				results[i].err = err
				return
			}
			results[i].count = n
		}()
	}
	wg.Wait()

	var total int64
	for _, r := range results {
		if r.err != nil {
			return 0, fmt.Errorf("count active coords for cut_date<=%s: %w", maxCutDate, r.err)
		}
		total += r.count
	}
	return total, nil
}

// GetByTccID 见接口注释。
func (r *tccCoordinatorRepository) GetByTccID(ctx context.Context, tccID string) (*model.TccCoordinator, error) {
	db, tableName, err := r.shardTable(ctx, tccID)
	if err != nil {
		return nil, err
	}
	var c model.TccCoordinator
	res := db.Table(tableName).Where("tcc_id = ?", tccID).Take(&c)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, res.Error
	}
	return &c, nil
}

// TransitionToCancelled: 仅在 TRYING 阶段允许取消。CANCELLED → CANCELLED 幂等。
func (r *tccCoordinatorRepository) TransitionToCancelled(ctx context.Context, tccID string) error {
	db, tableName, err := r.shardTable(ctx, tccID)
	if err != nil {
		recordTransition("TRYING", "CANCELLED", "error")
		return err
	}
	res := db.Table(tableName).
		Where("tcc_id = ? AND phase IN ?",
			tccID, []model.TccCoordinatorPhase{model.TccPhaseTrying, model.TccPhaseCancelled}).
		Update("phase", model.TccPhaseCancelled)
	if res.Error != nil {
		recordTransition("TRYING", "CANCELLED", "error")
		return res.Error
	}
	if res.RowsAffected == 0 {
		recordTransition("TRYING", "CANCELLED", "rejected_illegal")
		return fmt.Errorf("tcc coordinator %s cannot transition to CANCELLED (likely already CONFIRMING/CONFIRMED)", tccID)
	}
	recordTransition("TRYING", "CANCELLED", "ok")
	return nil
}

// ListStuck 并发扫全量 100 张分表（10 库 × 10 表/库），汇总 stuck 协调者。
// 全局 limit：每分片各自用 limit 取 top-limit，最终合并后按 updated_at 升序截断。
// 对齐 TccRecoveryWorker 的扫描语义（老版本扫 10 库 × 1 表，分表后扩 10 倍扫描基数但
// 单表数据量也缩 10 倍，整体延迟相当）。
func (r *tccCoordinatorRepository) ListStuck(ctx context.Context, before time.Time, limit int) ([]*model.TccCoordinator, error) {
	stuckPhases := []model.TccCoordinatorPhase{model.TccPhaseTrying, model.TccPhaseConfirming}
	shards := r.router.GetAllShards()

	type shardResult struct {
		coords []*model.TccCoordinator
		err    error
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
				results[i].err = err
				return
			}
			tableName := r.router.GetTableName("tcc_coordinator", shard.TableIndex)
			var coords []*model.TccCoordinator
			qErr := db.WithContext(ctx).Table(tableName).
				Where("phase IN ? AND updated_at < ?", stuckPhases, before).
				Order("updated_at ASC").
				Limit(limit).
				Find(&coords).Error
			if qErr != nil && !errors.Is(qErr, gorm.ErrRecordNotFound) {
				results[i].err = qErr
				return
			}
			results[i].coords = coords
		}()
	}
	wg.Wait()

	var all []*model.TccCoordinator
	for _, res := range results {
		// 单个分片查询失败不阻断（其余分片继续工作）
		all = append(all, res.coords...)
	}
	// 分片各自取 top-limit 后合并，结果并非全局有序 → 统一按 updated_at 升序
	// 再截断到 limit，得到近似全局 top-limit（最老的 stuck 优先被 recover）。
	sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt.Before(all[j].UpdatedAt) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}
