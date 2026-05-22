// mysql.go — MoneyFlow Graph + RunPlan MySQL 仓储 (MF-1, DB-split).
//
// DB-split Batch 3 重构：
//   moneyflow_graphs   → split_payment_meta 库（不分片，meta）
//   moneyflow_runs     → split_payment_db_0..9 × 10 张子表/库 = 100 张全局子表
//                        分片 key = charge_id（同一 charge 的 run 都在同 shard）
//
// 用 stdlib database/sql + mysql driver, 不引 GORM, 跟现有 monorepo 风格对齐.
//
// 启动期 EnsureSchema 不再用（DDL 已挪到 database/{metadb,shardb}/init/*.sql，由
// docker entrypoint 灌入）。本文件只剩 CRUD 代码。
package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/xiongwp/split-payment/internal/database"
	"github.com/xiongwp/split-payment/internal/domain"
	"github.com/xiongwp/split-payment/internal/sharding"
)

const (
	familyRuns = "moneyflow_runs" // family 前缀，跟 DDL gen.sh 对齐
)

// ─── Graph repo (走 metaDB, 表名 moneyflow_graphs 不变) ────────────────

// MySQLGraphRepo MySQL 实现.
type MySQLGraphRepo struct {
	mgr *database.Manager
}

// NewMySQLGraphRepo 构造. 走 metaDB.
func NewMySQLGraphRepo(mgr *database.Manager) *MySQLGraphRepo {
	return &MySQLGraphRepo{mgr: mgr}
}

// Save 创建或更新.
func (r *MySQLGraphRepo) Save(ctx context.Context, g *domain.Graph) (int64, error) {
	if g.Key == "" {
		return 0, errors.New("graph.key required")
	}
	specJSON, err := json.Marshal(g.Spec)
	if err != nil {
		return 0, fmt.Errorf("marshal spec: %w", err)
	}
	now := time.Now().UTC()
	if g.Version == "" {
		g.Version = "1.0.0"
	}
	if g.Status == "" {
		g.Status = "draft"
	}

	db := r.mgr.Meta()
	var existingID int64
	err = db.QueryRowContext(ctx, "SELECT id FROM moneyflow_graphs WHERE `key`=? LIMIT 1", g.Key).Scan(&existingID)
	switch {
	case err == sql.ErrNoRows:
		res, err := db.ExecContext(ctx, `
			INSERT INTO moneyflow_graphs (`+"`key`"+`, name, version, status, owner_type, owner_id, spec_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			g.Key, g.Name, g.Version, g.Status,
			nullable(g.OwnerType), nullable(g.OwnerID),
			specJSON, now, now)
		if err != nil {
			return 0, fmt.Errorf("insert graph: %w", err)
		}
		id, _ := res.LastInsertId()
		g.ID = id
		g.CreatedAt = now
		g.UpdatedAt = now
		return id, nil
	case err != nil:
		return 0, fmt.Errorf("lookup graph by key: %w", err)
	}

	g.ID = existingID
	g.Version = bumpPatch(g.Version)
	g.UpdatedAt = now
	_, err = db.ExecContext(ctx, `
		UPDATE moneyflow_graphs
		   SET name=?, version=?, status=?, owner_type=?, owner_id=?, spec_json=?, updated_at=?
		 WHERE id=?`,
		g.Name, g.Version, g.Status,
		nullable(g.OwnerType), nullable(g.OwnerID),
		specJSON, now, existingID)
	if err != nil {
		return 0, fmt.Errorf("update graph: %w", err)
	}
	return existingID, nil
}

// GetByKey 按业务键查.
func (r *MySQLGraphRepo) GetByKey(ctx context.Context, key string) (*domain.Graph, error) {
	row := r.mgr.Meta().QueryRowContext(ctx, `
		SELECT id, `+"`key`"+`, name, version, status, owner_type, owner_id, spec_json, created_at, updated_at
		  FROM moneyflow_graphs WHERE `+"`key`"+`=? LIMIT 1`, key)
	return scanGraph(row)
}

// FindByTrigger 列 status='active' 且 spec 里 triggers 含此 event 的 graph.
func (r *MySQLGraphRepo) FindByTrigger(ctx context.Context, event string) ([]*domain.Graph, error) {
	rows, err := r.mgr.Meta().QueryContext(ctx, `
		SELECT id, `+"`key`"+`, name, version, status, owner_type, owner_id, spec_json, created_at, updated_at
		  FROM moneyflow_graphs
		 WHERE status='active'
		   AND JSON_CONTAINS(spec_json->'$.triggers[*].event', JSON_QUOTE(?))`, event)
	if err != nil {
		return nil, fmt.Errorf("find by trigger: %w", err)
	}
	defer rows.Close()
	return collectGraphs(rows)
}

// Delete soft-delete: archive.
func (r *MySQLGraphRepo) Delete(ctx context.Context, key string) error {
	_, err := r.mgr.Meta().ExecContext(ctx,
		"UPDATE moneyflow_graphs SET status='archived', updated_at=NOW() WHERE `key`=?", key)
	if err != nil {
		return fmt.Errorf("delete (archive) graph %q: %w", key, err)
	}
	return nil
}

// List 按状态列.
func (r *MySQLGraphRepo) List(ctx context.Context, status string) ([]*domain.Graph, error) {
	db := r.mgr.Meta()
	var rows *sql.Rows
	var err error
	if status == "" || status == "all" {
		rows, err = db.QueryContext(ctx, `
			SELECT id, `+"`key`"+`, name, version, status, owner_type, owner_id, spec_json, created_at, updated_at
			  FROM moneyflow_graphs ORDER BY updated_at DESC`)
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT id, `+"`key`"+`, name, version, status, owner_type, owner_id, spec_json, created_at, updated_at
			  FROM moneyflow_graphs WHERE status=? ORDER BY updated_at DESC`, status)
	}
	if err != nil {
		return nil, fmt.Errorf("list graphs: %w", err)
	}
	defer rows.Close()
	return collectGraphs(rows)
}

// ─── Run repo (走 shardDB, 分片 key=charge_id, 子表 moneyflow_runs_NN) ──

// MySQLRunRepo MySQL 实现.
type MySQLRunRepo struct {
	mgr    *database.Manager
	router *sharding.Router
}

// NewMySQLRunRepo 构造. 路由由 router 提供。
func NewMySQLRunRepo(mgr *database.Manager, router *sharding.Router) *MySQLRunRepo {
	return &MySQLRunRepo{mgr: mgr, router: router}
}

// shardOf 按 ctx + chargeID 解析出 (shard db, 全局 table name).
// 从 ctx 读 shadow flag，自动拼 _shadow 后缀（全链路压测影子流量）.
// chargeID 空 → 退化到 db_0 / table_00（兼容 caller 不传时的旧行为；prod 应该总传 chargeID）.
func (r *MySQLRunRepo) shardOf(ctx context.Context, chargeID string) (*sql.DB, string) {
	dbIdx, tblIdx := r.router.RouteByString(chargeID)
	return r.mgr.Shard(dbIdx), r.router.TableNameCtx(ctx, familyRuns, tblIdx)
}

// Save 插入新 run plan. 必须先填 p.ChargeID 用来路由.
func (r *MySQLRunRepo) Save(ctx context.Context, p *domain.RunPlan) (int64, error) {
	attrJSON, _ := json.Marshal(p.Attributes)
	movJSON, _ := json.Marshal(p.Movements)
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if p.Status == "" {
		p.Status = "created"
	}
	db, tbl := r.shardOf(ctx, p.ChargeID)
	res, err := db.ExecContext(ctx, `
		INSERT INTO `+tbl+`
		(graph_id, graph_version, trigger_event, charge_id, merchant_id,
		 amount_minor, currency, attributes_json, movements_json,
		 status, voucher_no, error_msg, trace_id, created_at)
		VALUES (?, ?, ?, ?, ?,  ?, ?, ?, ?,  ?, ?, ?, ?, ?)`,
		p.GraphID, p.GraphVersion, p.TriggerEvent, nullable(p.ChargeID), nullable(p.MerchantID),
		p.AmountMinor, nullable(p.Currency), attrJSON, movJSON,
		p.Status, nullable(p.VoucherNo), nullable(p.ErrorMsg), nullable(p.TraceID), p.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert run (tbl=%s): %w", tbl, err)
	}
	id, _ := res.LastInsertId()
	p.ID = id
	return id, nil
}

// Update 更新已存 run. 入参用 *RunPlan（必须含 ChargeID 用来路由 + ID 用来定位行）.
func (r *MySQLRunRepo) Update(ctx context.Context, p *domain.RunPlan) error {
	movJSON, _ := json.Marshal(p.Movements)
	db, tbl := r.shardOf(ctx, p.ChargeID)
	_, err := db.ExecContext(ctx, `
		UPDATE `+tbl+`
		   SET status=?, voucher_no=?, error_msg=?, movements_json=?
		 WHERE id=?`,
		p.Status, nullable(p.VoucherNo), nullable(p.ErrorMsg), movJSON, p.ID)
	if err != nil {
		return fmt.Errorf("update run (tbl=%s): %w", tbl, err)
	}
	return nil
}

// GetByCharge 拉同一 charge 关联的所有 run plan. chargeID 路由到唯一 shard.
func (r *MySQLRunRepo) GetByCharge(ctx context.Context, chargeID string) ([]*domain.RunPlan, error) {
	db, tbl := r.shardOf(ctx, chargeID)
	rows, err := db.QueryContext(ctx, `
		SELECT id, graph_id, graph_version, trigger_event, charge_id, merchant_id,
		       amount_minor, currency, attributes_json, movements_json,
		       status, voucher_no, error_msg, trace_id, created_at
		  FROM `+tbl+`
		 WHERE charge_id=?
		 ORDER BY created_at DESC`, chargeID)
	if err != nil {
		return nil, fmt.Errorf("get runs by charge (tbl=%s): %w", tbl, err)
	}
	defer rows.Close()
	return collectRuns(rows)
}

// MarkHoldReleased 标 hold_released=1. 加 chargeID 参数用来路由.
func (r *MySQLRunRepo) MarkHoldReleased(ctx context.Context, runID int64, chargeID string) error {
	db, tbl := r.shardOf(ctx, chargeID)
	res, err := db.ExecContext(ctx, `
		UPDATE `+tbl+`
		   SET hold_released = 1
		 WHERE id = ?
		   AND hold_released = 0`, runID)
	if err != nil {
		return fmt.Errorf("mark hold released (tbl=%s): %w", tbl, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetHoldUntil 给 run plan 设 hold 到期时间. 加 chargeID 参数用来路由.
func (r *MySQLRunRepo) SetHoldUntil(ctx context.Context, runID int64, chargeID string, holdUntil time.Time) error {
	db, tbl := r.shardOf(ctx, chargeID)
	_, err := db.ExecContext(ctx, `
		UPDATE `+tbl+` SET hold_until = ? WHERE id = ?`,
		holdUntil.UTC(), runID)
	if err != nil {
		return fmt.Errorf("set hold_until (tbl=%s): %w", tbl, err)
	}
	return nil
}

// GetByID 单条查。不知道 chargeID 时退化到全 shard 扫（性能差但兼容）.
// 推荐 caller 用 GetByChargeAndID 直接定位 shard.
func (r *MySQLRunRepo) GetByID(ctx context.Context, id int64) (*domain.RunPlan, error) {
	// 全 shard 扫 — 每个 shard 的 100 / shardCount 张表都查一遍
	for _, st := range r.router.AllTables(ctx, familyRuns) {
		db := r.mgr.Shard(st.DBIdx)
		rows, err := db.QueryContext(ctx, `
			SELECT id, graph_id, graph_version, trigger_event, charge_id, merchant_id,
			       amount_minor, currency, attributes_json, movements_json,
			       status, voucher_no, error_msg, trace_id, created_at
			  FROM `+st.TableName+` WHERE id=? LIMIT 1`, id)
		if err != nil {
			continue
		}
		list, err := collectRuns(rows)
		rows.Close()
		if err == nil && len(list) > 0 {
			return list[0], nil
		}
	}
	return nil, ErrNotFound
}

// GetByChargeAndID 推荐使用：chargeID 路由 + id 定位，O(1) 查询.
func (r *MySQLRunRepo) GetByChargeAndID(ctx context.Context, chargeID string, id int64) (*domain.RunPlan, error) {
	db, tbl := r.shardOf(ctx, chargeID)
	rows, err := db.QueryContext(ctx, `
		SELECT id, graph_id, graph_version, trigger_event, charge_id, merchant_id,
		       amount_minor, currency, attributes_json, movements_json,
		       status, voucher_no, error_msg, trace_id, created_at
		  FROM `+tbl+` WHERE id=? LIMIT 1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := collectRuns(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return list[0], nil
}

// ─── Cross-shard scan 方法（worker / admin 用，性能差但要全扫）────────

// ListExpiredHolds 跨所有 shard 拉到期未释放的 hold.
func (r *MySQLRunRepo) ListExpiredHolds(ctx context.Context, now time.Time, limit int) ([]*domain.RunPlan, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return r.scanAllShards(ctx, `
		SELECT id, graph_id, graph_version, trigger_event, charge_id, merchant_id,
		       amount_minor, currency, attributes_json, movements_json,
		       status, voucher_no, error_msg, trace_id, created_at
		  FROM %s
		 WHERE hold_released = 0
		   AND hold_until IS NOT NULL
		   AND hold_until <= ?
		   AND status = 'completed'
		 ORDER BY hold_until ASC
		 LIMIT ?`, []any{now.UTC(), limit}, limit)
}

// ListByStatus 跨所有 shard 列指定 status.
func (r *MySQLRunRepo) ListByStatus(ctx context.Context, status string, limit int) ([]*domain.RunPlan, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return r.scanAllShards(ctx, `
		SELECT id, graph_id, graph_version, trigger_event, charge_id, merchant_id,
		       amount_minor, currency, attributes_json, movements_json,
		       status, voucher_no, error_msg, trace_id, created_at
		  FROM %s
		 WHERE status=?
		 ORDER BY created_at DESC LIMIT ?`, []any{status, limit}, limit)
}

// Search adminhttp 用. eventLike 在 trigger_event LIKE %x%.
func (r *MySQLRunRepo) Search(ctx context.Context, eventLike string, limit int) ([]*domain.RunPlan, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if eventLike == "" {
		return r.scanAllShards(ctx, `
			SELECT id, graph_id, graph_version, trigger_event, charge_id, merchant_id,
			       amount_minor, currency, attributes_json, movements_json,
			       status, voucher_no, error_msg, trace_id, created_at
			  FROM %s
			 ORDER BY created_at DESC LIMIT ?`, []any{limit}, limit)
	}
	return r.scanAllShards(ctx, `
		SELECT id, graph_id, graph_version, trigger_event, charge_id, merchant_id,
		       amount_minor, currency, attributes_json, movements_json,
		       status, voucher_no, error_msg, trace_id, created_at
		  FROM %s
		 WHERE trigger_event LIKE ?
		 ORDER BY created_at DESC LIMIT ?`, []any{"%" + eventLike + "%", limit}, limit)
}

// scanAllShards 并发扫 100 张全局表（10 shard × 10 表/shard），合并结果再按 limit 截断.
// queryFmt 必须含一个 %s（替换成 table name），后续 args 给 ? 占位符填值.
func (r *MySQLRunRepo) scanAllShards(ctx context.Context, queryFmt string, args []any, limit int) ([]*domain.RunPlan, error) {
	tables := r.router.AllTables(ctx, familyRuns)
	results := make([][]*domain.RunPlan, len(tables))
	errs := make([]error, len(tables))

	var wg sync.WaitGroup
	for i, st := range tables {
		wg.Add(1)
		go func(i int, st sharding.ShardTable) {
			defer wg.Done()
			q := fmt.Sprintf(queryFmt, st.TableName)
			rows, err := r.mgr.Shard(st.DBIdx).QueryContext(ctx, q, args...)
			if err != nil {
				errs[i] = err
				return
			}
			defer rows.Close()
			list, err := collectRuns(rows)
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = list
		}(i, st)
	}
	wg.Wait()

	// 任一 shard 错就报错（保守）
	for _, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("scan all shards: %w", err)
		}
	}

	// 合并 + 按 created_at desc 排序 + limit 截断
	out := make([]*domain.RunPlan, 0)
	for _, list := range results {
		out = append(out, list...)
	}
	// 简单倒序 by CreatedAt
	for i := 0; i < len(out)-1; i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.After(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ─── scan helpers ──────────────────────────────────────────────────────

func scanGraph(row interface{ Scan(...any) error }) (*domain.Graph, error) {
	var (
		g         domain.Graph
		ownerType sql.NullString
		ownerID   sql.NullString
		specJSON  []byte
	)
	err := row.Scan(&g.ID, &g.Key, &g.Name, &g.Version, &g.Status,
		&ownerType, &ownerID, &specJSON, &g.CreatedAt, &g.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan graph: %w", err)
	}
	g.OwnerType = ownerType.String
	g.OwnerID = ownerID.String
	if len(specJSON) > 0 {
		if err := json.Unmarshal(specJSON, &g.Spec); err != nil {
			return nil, fmt.Errorf("unmarshal spec: %w", err)
		}
	}
	return &g, nil
}

func collectGraphs(rows *sql.Rows) ([]*domain.Graph, error) {
	out := []*domain.Graph{}
	for rows.Next() {
		g, err := scanGraph(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func collectRuns(rows *sql.Rows) ([]*domain.RunPlan, error) {
	out := []*domain.RunPlan{}
	for rows.Next() {
		var (
			p          domain.RunPlan
			chargeID   sql.NullString
			merchantID sql.NullString
			currency   sql.NullString
			voucher    sql.NullString
			errMsg     sql.NullString
			traceID    sql.NullString
			attrJSON   []byte
			movJSON    []byte
		)
		if err := rows.Scan(
			&p.ID, &p.GraphID, &p.GraphVersion, &p.TriggerEvent,
			&chargeID, &merchantID, &p.AmountMinor, &currency,
			&attrJSON, &movJSON,
			&p.Status, &voucher, &errMsg, &traceID, &p.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		p.ChargeID = chargeID.String
		p.MerchantID = merchantID.String
		p.Currency = currency.String
		p.VoucherNo = voucher.String
		p.ErrorMsg = errMsg.String
		p.TraceID = traceID.String
		if len(attrJSON) > 0 {
			_ = json.Unmarshal(attrJSON, &p.Attributes)
		}
		if len(movJSON) > 0 {
			_ = json.Unmarshal(movJSON, &p.Movements)
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// nullable string 转 sql.NullString. 空 → NULL.
func nullable(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// bumpPatch 升级 semver 的 patch 段.
func bumpPatch(v string) string {
	// 简化：1.0.0 → 1.0.1（不解析 semver，最后段 +1）
	idx := -1
	for i := len(v) - 1; i >= 0; i-- {
		if v[i] == '.' {
			idx = i
			break
		}
	}
	if idx == -1 || idx == len(v)-1 {
		return v + ".1"
	}
	patch := 0
	for i := idx + 1; i < len(v); i++ {
		c := v[i]
		if c < '0' || c > '9' {
			return v + ".1"
		}
		patch = patch*10 + int(c-'0')
	}
	return fmt.Sprintf("%s.%d", v[:idx], patch+1)
}

// ErrNotFound 定义在 memory.go（同包共用 sentinel）。
