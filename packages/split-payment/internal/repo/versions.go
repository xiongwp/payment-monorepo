// versions.go — SP-7 Graph versioning + immutable snapshot.
//
// 表:
//   moneyflow_graphs (id, key UNIQUE, name, active_version_id, status, owner_type, owner_id, ...)
//   moneyflow_graph_versions (id PK, graph_id, version VARCHAR, spec_json, immutable_at,
//                             created_by, change_summary, INDEX (graph_id, immutable_at))
//
// 行为:
//   - Save(graph) → 创建 graph (若不存在) + 新增 version 行 (draft, 不立即激活)
//   - Activate(graph_id, version_id, by) → 把 graph.active_version_id 指过去
//   - RunPlan.GraphVersionID 锁定本次 run 用的 version, 之后激活变化不影响 in-flight
//   - 已 immutable 的 version 永远只读
//
// Phase 1 的 SaveDefWithMeta 是 "重写最新版" 模式, Phase 2 升级为 "新增 version + 待激活" 模式.
// 老 SaveDef API 保留, 内部走 SaveVersion 兼容路径.

package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"
)

// EnsureVersioningSchema SP-7 表.
func EnsureVersioningSchema(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS moneyflow_graph_versions (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			graph_id BIGINT NOT NULL,
			version VARCHAR(32) NOT NULL,
			spec_json JSON NOT NULL,
			immutable_at DATETIME,
			created_by VARCHAR(128),
			change_summary VARCHAR(512),
			created_at DATETIME NOT NULL,
			UNIQUE KEY uk_graph_version (graph_id, version),
			KEY idx_graph_created (graph_id, created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		// 老 moneyflow_graphs 表加 active_version_id 列 (若没有). 用 SHOW COLUMNS + 动态 ALTER 避免重复加.
		`ALTER TABLE moneyflow_graphs
			ADD COLUMN IF NOT EXISTS active_version_id BIGINT DEFAULT NULL`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			// MySQL < 8.0.29 不支持 ADD COLUMN IF NOT EXISTS; 忽略 "Duplicate column" 错误
			if isDupColumnErr(err) {
				continue
			}
			return fmt.Errorf("ensure versioning schema: %w", err)
		}
	}
	return nil
}

func isDupColumnErr(err error) bool {
	return err != nil && (containsAny(err.Error(),
		"Duplicate column name", "1060", "ER_DUP_FIELDNAME"))
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) <= len(s) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// VersionRepo SP-7 graph versioning.
type VersionRepo struct{ db *sql.DB }

// NewVersionRepo 构造.
func NewVersionRepo(db *sql.DB) *VersionRepo { return &VersionRepo{db: db} }

// SaveVersion 新增 graph_version (draft 状态, 不立即激活).
//
// 调用方:
//   1. 已有 graph: 传 graph_id + 新 version + 新 spec
//   2. 全新 graph: 先 INSERT moneyflow_graphs (key/name/status='draft'), 拿到 id, 再调 SaveVersion
//
// 返回 version_id (新行 PK).
func (r *VersionRepo) SaveVersion(
	ctx context.Context,
	graphID int64,
	version string,
	spec domain.GraphSpec,
	createdBy, changeSummary string,
) (int64, error) {
	if graphID == 0 {
		return 0, errors.New("graph_id required")
	}
	if version == "" {
		return 0, errors.New("version required")
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return 0, fmt.Errorf("marshal spec: %w", err)
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO moneyflow_graph_versions
		(graph_id, version, spec_json, created_by, change_summary, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE spec_json=VALUES(spec_json),
		                       change_summary=VALUES(change_summary)`,
		graphID, version, specJSON, createdBy, changeSummary, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("insert version: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// Activate 把指定 graph 的 active_version_id 改成 versionID, 并把该 version 标 immutable.
//
// 一旦 immutable 不可修改 (SaveVersion 会拒掉同 (graph_id, version)).
// 老 active version 不动 (历史 RunPlan 可能引用).
func (r *VersionRepo) Activate(ctx context.Context, graphID, versionID int64, by string) error {
	// 1) 标 immutable_at
	res, err := r.db.ExecContext(ctx, `
		UPDATE moneyflow_graph_versions
		   SET immutable_at = IFNULL(immutable_at, ?)
		 WHERE id=? AND graph_id=?`,
		time.Now().UTC(), versionID, graphID)
	if err != nil {
		return fmt.Errorf("mark immutable: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("version %d not found for graph %d", versionID, graphID)
	}
	// 2) graph.active_version_id 切换
	_, err = r.db.ExecContext(ctx, `
		UPDATE moneyflow_graphs
		   SET active_version_id=?, status='active', updated_at=?
		 WHERE id=?`,
		versionID, time.Now().UTC(), graphID)
	if err != nil {
		return fmt.Errorf("update active_version: %w", err)
	}
	_ = by // audit 留下次接入
	return nil
}

// GetActiveSpec 拿 graph 当前激活的 spec (engine 触发时用).
//
// graph.active_version_id 为 nil → graph 没激活, 返 ErrNotFound (engine 跳过此 graph).
func (r *VersionRepo) GetActiveSpec(ctx context.Context, graphID int64) (*domain.GraphSpec, int64, string, error) {
	var (
		activeID sql.NullInt64
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT active_version_id FROM moneyflow_graphs WHERE id=?`, graphID).Scan(&activeID)
	if err == sql.ErrNoRows {
		return nil, 0, "", ErrNotFound
	}
	if err != nil {
		return nil, 0, "", err
	}
	if !activeID.Valid {
		return nil, 0, "", ErrNotFound
	}
	var (
		specJSON []byte
		version  string
	)
	err = r.db.QueryRowContext(ctx, `
		SELECT version, spec_json FROM moneyflow_graph_versions WHERE id=?`,
		activeID.Int64).Scan(&version, &specJSON)
	if err != nil {
		return nil, 0, "", err
	}
	var spec domain.GraphSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return nil, 0, "", fmt.Errorf("unmarshal spec: %w", err)
	}
	return &spec, activeID.Int64, version, nil
}

// GetVersion 拿历史 version 的 spec (RunPlan 用 graph_version_id 锁定时用).
func (r *VersionRepo) GetVersion(ctx context.Context, versionID int64) (*domain.GraphSpec, string, error) {
	var (
		specJSON []byte
		version  string
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT version, spec_json FROM moneyflow_graph_versions WHERE id=?`,
		versionID).Scan(&version, &specJSON)
	if err == sql.ErrNoRows {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	var spec domain.GraphSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return nil, "", fmt.Errorf("unmarshal spec: %w", err)
	}
	return &spec, version, nil
}

// ListVersions 拉 graph 的所有 version 历史 (按 created_at desc).
func (r *VersionRepo) ListVersions(ctx context.Context, graphID int64, limit int) ([]GraphVersion, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, graph_id, version, immutable_at, created_by, change_summary, created_at
		  FROM moneyflow_graph_versions
		 WHERE graph_id=?
		 ORDER BY created_at DESC LIMIT ?`, graphID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GraphVersion{}
	for rows.Next() {
		var v GraphVersion
		var imm sql.NullTime
		var by, sum sql.NullString
		if err := rows.Scan(&v.ID, &v.GraphID, &v.Version, &imm, &by, &sum, &v.CreatedAt); err != nil {
			return nil, err
		}
		if imm.Valid {
			v.ImmutableAt = imm.Time
		}
		v.CreatedBy = by.String
		v.ChangeSummary = sum.String
		out = append(out, v)
	}
	return out, rows.Err()
}

// GraphVersion DB 行 (HTTP list 用, 不返 spec_json 减小响应).
type GraphVersion struct {
	ID            int64     `json:"id"`
	GraphID       int64     `json:"graph_id"`
	Version       string    `json:"version"`
	ImmutableAt   time.Time `json:"immutable_at,omitempty"`
	CreatedBy     string    `json:"created_by"`
	ChangeSummary string    `json:"change_summary"`
	CreatedAt     time.Time `json:"created_at"`
}
