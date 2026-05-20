// mysql.go — MoneyFlow Graph + RunPlan MySQL 仓储 (MF-1).
//
// 表:
//   moneyflow_graphs  (id PK, key UNIQUE, spec_json JSON, ...)
//   moneyflow_runs    (id PK, graph_id FK, charge_id INDEX, ...)
//
// 用 stdlib database/sql + mysql driver, 不引 GORM, 跟现有 monorepo 风格对齐.
//
// 启动期 EnsureSchema() 自动建表 (dev/staging); 生产用 migrate 工具.
package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiongwp/split-payment/internal/domain"
)

// ─── Graph repo ────────────────────────────────────────────────────────

// MySQLGraphRepo MySQL 实现.
type MySQLGraphRepo struct {
	db *sql.DB
}

// NewMySQLGraphRepo 构造. db 由 caller 准备 (dsn=user:pass@tcp(host:3306)/dbname).
func NewMySQLGraphRepo(db *sql.DB) *MySQLGraphRepo { return &MySQLGraphRepo{db: db} }

// EnsureSchema 启动期自建表 (idempotent). dev/staging 用; 生产建议 migrate 工具.
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS moneyflow_graphs (
			id          BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			` + "`key`" + `       VARCHAR(128) NOT NULL,
			name        VARCHAR(256) NOT NULL,
			version     VARCHAR(32)  NOT NULL DEFAULT '1.0.0',
			status      VARCHAR(32)  NOT NULL DEFAULT 'draft',
			owner_type  VARCHAR(32)  DEFAULT NULL,
			owner_id    VARCHAR(128) DEFAULT NULL,
			spec_json   JSON         NOT NULL,
			created_at  DATETIME     NOT NULL,
			updated_at  DATETIME     NOT NULL,
			UNIQUE KEY uk_key (` + "`key`" + `),
			KEY idx_status (status)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS moneyflow_runs (
			id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			graph_id        BIGINT NOT NULL,
			graph_version   VARCHAR(32),
			trigger_event   VARCHAR(64)  NOT NULL,
			charge_id       VARCHAR(128) DEFAULT NULL,
			merchant_id     VARCHAR(128) DEFAULT NULL,
			amount_minor    BIGINT NOT NULL DEFAULT 0,
			currency        VARCHAR(8)   DEFAULT NULL,
			attributes_json JSON         DEFAULT NULL,
			movements_json  JSON         DEFAULT NULL,
			status          VARCHAR(32)  NOT NULL DEFAULT 'created',
			voucher_no      VARCHAR(64)  DEFAULT NULL,
			error_msg       TEXT         DEFAULT NULL,
			trace_id        VARCHAR(64)  DEFAULT NULL,
			-- SP-AC-7 PH3-7: hold-period 字段供 HoldUnstickWorker 用.
			hold_until      DATETIME     DEFAULT NULL,
			hold_released   TINYINT(1)   NOT NULL DEFAULT 0,
			created_at      DATETIME     NOT NULL,
			KEY idx_charge (charge_id),
			KEY idx_graph (graph_id),
			KEY idx_event_created (trigger_event, created_at),
			KEY idx_hold_expired (hold_released, hold_until)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	// 老库升级 (新加列): IF NOT EXISTS 需要 MySQL 8.0.29+ / MariaDB 10.0.2+;
	// 兼容更老版本: try ALTER + 忽略 1060 duplicate column 错.
	for _, alter := range []string{
		`ALTER TABLE moneyflow_runs ADD COLUMN hold_until DATETIME DEFAULT NULL`,
		`ALTER TABLE moneyflow_runs ADD COLUMN hold_released TINYINT(1) NOT NULL DEFAULT 0`,
	} {
		if _, err := db.ExecContext(ctx, alter); err != nil {
			// 1060 = "Duplicate column name" → 列已存在, 忽略.
			// 其它错 → 真问题, 报出来.
			if !isDuplicateColumnErr(err) {
				return fmt.Errorf("ensure schema (alter): %w", err)
			}
		}
	}
	return nil
}

// isDuplicateColumnErr 检测 MySQL/MariaDB error 1060 (Duplicate column name).
// 不依赖 go-sql-driver/mysql.MySQLError 类型断言, 用字符串匹配兼容多 driver.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1060") ||
		strings.Contains(msg, "Duplicate column name") ||
		strings.Contains(msg, "duplicate column")
}

// Save 创建或更新. key 已存在 → 升 version 字段 (按 semver) + UPDATE; 否则 INSERT.
//
// 调用方控制 status (draft / active / archived). 同 key 永远只一行 (用 versions 表
// 做历史快照可后续加).
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

	// upsert: 优先按 key 找
	var existingID int64
	err = r.db.QueryRowContext(ctx, "SELECT id FROM moneyflow_graphs WHERE `key`=? LIMIT 1", g.Key).Scan(&existingID)
	switch {
	case err == sql.ErrNoRows:
		// insert
		res, err := r.db.ExecContext(ctx, `
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

	// update
	g.ID = existingID
	g.Version = bumpPatch(g.Version)
	g.UpdatedAt = now
	_, err = r.db.ExecContext(ctx, `
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
	row := r.db.QueryRowContext(ctx, `
		SELECT id, `+"`key`"+`, name, version, status, owner_type, owner_id, spec_json, created_at, updated_at
		  FROM moneyflow_graphs WHERE `+"`key`"+`=? LIMIT 1`, key)
	return scanGraph(row)
}

// FindByTrigger 列 status='active' 且 spec 里 triggers 含此 event 的 graph.
//
// 用 JSON_CONTAINS 跑 MySQL 端过滤. 老 MySQL (<5.7) 没 JSON 函数时退化为
// 拉全部 active 再代码侧过滤 (没接 fallback, 默认 MySQL ≥ 8).
func (r *MySQLGraphRepo) FindByTrigger(ctx context.Context, event string) ([]*domain.Graph, error) {
	rows, err := r.db.QueryContext(ctx, `
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

// Delete soft-delete: 把 graph 状态置为 "archived". 物理删会破坏历史 run_plan
// 关联, 因此只做软删. key 不存在 → 返 nil (idempotent), 与 grpcsvc 注释对齐.
func (r *MySQLGraphRepo) Delete(ctx context.Context, key string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE moneyflow_graphs SET status='archived', updated_at=NOW() WHERE `key`=?", key)
	if err != nil {
		return fmt.Errorf("delete (archive) graph %q: %w", key, err)
	}
	return nil
}

// List 按状态列. status="" 或 "all" 返全部.
func (r *MySQLGraphRepo) List(ctx context.Context, status string) ([]*domain.Graph, error) {
	var rows *sql.Rows
	var err error
	if status == "" || status == "all" {
		rows, err = r.db.QueryContext(ctx, `
			SELECT id, `+"`key`"+`, name, version, status, owner_type, owner_id, spec_json, created_at, updated_at
			  FROM moneyflow_graphs ORDER BY updated_at DESC`)
	} else {
		rows, err = r.db.QueryContext(ctx, `
			SELECT id, `+"`key`"+`, name, version, status, owner_type, owner_id, spec_json, created_at, updated_at
			  FROM moneyflow_graphs WHERE status=? ORDER BY updated_at DESC`, status)
	}
	if err != nil {
		return nil, fmt.Errorf("list graphs: %w", err)
	}
	defer rows.Close()
	return collectGraphs(rows)
}

// ─── Run repo ──────────────────────────────────────────────────────────

// MySQLRunRepo MySQL 实现.
type MySQLRunRepo struct {
	db *sql.DB
}

// NewMySQLRunRepo 构造.
func NewMySQLRunRepo(db *sql.DB) *MySQLRunRepo { return &MySQLRunRepo{db: db} }

// Save 插入新 run plan.
func (r *MySQLRunRepo) Save(ctx context.Context, p *domain.RunPlan) (int64, error) {
	attrJSON, _ := json.Marshal(p.Attributes)
	movJSON, _ := json.Marshal(p.Movements)
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if p.Status == "" {
		p.Status = "created"
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO moneyflow_runs
		(graph_id, graph_version, trigger_event, charge_id, merchant_id,
		 amount_minor, currency, attributes_json, movements_json,
		 status, voucher_no, error_msg, trace_id, created_at)
		VALUES (?, ?, ?, ?, ?,  ?, ?, ?, ?,  ?, ?, ?, ?, ?)`,
		p.GraphID, p.GraphVersion, p.TriggerEvent, nullable(p.ChargeID), nullable(p.MerchantID),
		p.AmountMinor, nullable(p.Currency), attrJSON, movJSON,
		p.Status, nullable(p.VoucherNo), nullable(p.ErrorMsg), nullable(p.TraceID), p.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert run: %w", err)
	}
	id, _ := res.LastInsertId()
	p.ID = id
	return id, nil
}

// Update 更新已存 run (status / voucher_no / movements / error_msg 常变).
func (r *MySQLRunRepo) Update(ctx context.Context, p *domain.RunPlan) error {
	movJSON, _ := json.Marshal(p.Movements)
	_, err := r.db.ExecContext(ctx, `
		UPDATE moneyflow_runs
		   SET status=?, voucher_no=?, error_msg=?, movements_json=?
		 WHERE id=?`,
		p.Status, nullable(p.VoucherNo), nullable(p.ErrorMsg), movJSON, p.ID)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	return nil
}

// GetByCharge 拉同一 charge 关联的所有 run plan (按 created_at desc).
func (r *MySQLRunRepo) GetByCharge(ctx context.Context, chargeID string) ([]*domain.RunPlan, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, graph_id, graph_version, trigger_event, charge_id, merchant_id,
		       amount_minor, currency, attributes_json, movements_json,
		       status, voucher_no, error_msg, trace_id, created_at
		  FROM moneyflow_runs
		 WHERE charge_id=?
		 ORDER BY created_at DESC`, chargeID)
	if err != nil {
		return nil, fmt.Errorf("get runs by charge: %w", err)
	}
	defer rows.Close()
	return collectRuns(rows)
}

// SP-AC-7 PH3-7: ListExpiredHolds 拉到期未释放的 hold (hold_released=0 AND hold_until<=now),
// HoldUnstickWorker 用 — 把 unsettled 资金搬到正式账户.
//
// 只查 status=completed 的 plan (失败/进行中的 plan 不会有真 unsettled 资金).
// idx_hold_expired(hold_released, hold_until) 走索引扫描, limit 默认 100.
func (r *MySQLRunRepo) ListExpiredHolds(ctx context.Context, now time.Time, limit int) ([]*domain.RunPlan, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, graph_id, graph_version, trigger_event, charge_id, merchant_id,
		       amount_minor, currency, attributes_json, movements_json,
		       status, voucher_no, error_msg, trace_id, created_at
		  FROM moneyflow_runs
		 WHERE hold_released = 0
		   AND hold_until IS NOT NULL
		   AND hold_until <= ?
		   AND status = 'completed'
		 ORDER BY hold_until ASC
		 LIMIT ?`, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list expired holds: %w", err)
	}
	defer rows.Close()
	return collectRuns(rows)
}

// SP-AC-7 PH3-7: MarkHoldReleased 标 hold_released=1, 防 worker 重复扫.
// CAS 风格 — 只翻 0→1, 防多副本竞争 (虽然外层已有 lease, 仍是双保险).
func (r *MySQLRunRepo) MarkHoldReleased(ctx context.Context, runID int64) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE moneyflow_runs
		   SET hold_released = 1
		 WHERE id = ?
		   AND hold_released = 0`, runID)
	if err != nil {
		return fmt.Errorf("mark hold released: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// 别的 worker 已经处理或行不存在 — 当作非错(幂等).
		return ErrNotFound
	}
	return nil
}

// SP-AC-7 PH3-7: SetHoldUntil 给 run plan 设 hold 到期时间.
// 用法: engine.Handle 在创建 plan 时如果场景有 hold 期 (e.g. marketplace_split
// 配 hold_days=7), 调本方法填 hold_until = now + N days.
//
// 不动 hold_released — 默认为 0 (新行).
func (r *MySQLRunRepo) SetHoldUntil(ctx context.Context, runID int64, holdUntil time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE moneyflow_runs SET hold_until = ? WHERE id = ?`,
		holdUntil.UTC(), runID)
	if err != nil {
		return fmt.Errorf("set hold_until: %w", err)
	}
	return nil
}

// ListByStatus SP-FIN-4 4-eyes approval 列表用 — 拉指定 status 的 plan, created_at desc.
func (r *MySQLRunRepo) ListByStatus(ctx context.Context, status string, limit int) ([]*domain.RunPlan, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, graph_id, graph_version, trigger_event, charge_id, merchant_id,
		       amount_minor, currency, attributes_json, movements_json,
		       status, voucher_no, error_msg, trace_id, created_at
		  FROM moneyflow_runs
		 WHERE status=?
		 ORDER BY created_at DESC LIMIT ?`, status, limit)
	if err != nil {
		return nil, fmt.Errorf("list by status: %w", err)
	}
	defer rows.Close()
	return collectRuns(rows)
}

// GetByID 单条.
func (r *MySQLRunRepo) GetByID(ctx context.Context, id int64) (*domain.RunPlan, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, graph_id, graph_version, trigger_event, charge_id, merchant_id,
		       amount_minor, currency, attributes_json, movements_json,
		       status, voucher_no, error_msg, trace_id, created_at
		  FROM moneyflow_runs WHERE id=? LIMIT 1`, id)
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

// Search adminhttp 用 — eventLike 在 trigger_event LIKE %x%, limit 默认 100.
func (r *MySQLRunRepo) Search(ctx context.Context, eventLike string, limit int) ([]*domain.RunPlan, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `
		SELECT id, graph_id, graph_version, trigger_event, charge_id, merchant_id,
		       amount_minor, currency, attributes_json, movements_json,
		       status, voucher_no, error_msg, trace_id, created_at
		  FROM moneyflow_runs `
	args := []any{}
	if eventLike != "" {
		q += "WHERE trigger_event LIKE ? "
		args = append(args, "%"+eventLike+"%")
	}
	q += "ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("search runs: %w", err)
	}
	defer rows.Close()
	return collectRuns(rows)
}

// ─── scan helpers ──────────────────────────────────────────────────────

func scanGraph(row interface{ Scan(...any) error }) (*domain.Graph, error) {
	var (
		g          domain.Graph
		ownerType  sql.NullString
		ownerID    sql.NullString
		specJSON   []byte
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

// bumpPatch semver 1.2.3 → 1.2.4. 解析失败保留原值.
//
// 简化:不处理 prerelease (1.0.0-rc1) / build metadata.
func bumpPatch(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return v
	}
	patch := 0
	if _, err := fmt.Sscanf(parts[2], "%d", &patch); err != nil {
		return v
	}
	return fmt.Sprintf("%s.%s.%d", parts[0], parts[1], patch+1)
}
