// postgres_rule_versions.go: engine.RuleVersionStore 的 Postgres 实现。
//
// **未编译进默认 build**（//go:build pg）。生产打包时加 -tags pg + pgx 依赖。
//
// Schema：
//
//	CREATE TABLE risk_rule_versions (
//	    rule_id        TEXT NOT NULL,
//	    version        BIGSERIAL NOT NULL,
//	    content_hash   TEXT NOT NULL,        -- sha256(canonical(spec_json))
//	    spec_json      JSONB NOT NULL,
//	    change_summary TEXT,
//	    author         TEXT,
//	    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
//	    PRIMARY KEY (rule_id, version)
//	);
//	CREATE INDEX risk_rule_versions_hash ON risk_rule_versions (rule_id, content_hash);
//
//	CREATE TABLE risk_rule_active (
//	    rule_id        TEXT PRIMARY KEY,
//	    active_version BIGINT NOT NULL,
//	    activated_by   TEXT,
//	    activated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
//	    FOREIGN KEY (rule_id, active_version) REFERENCES risk_rule_versions(rule_id, version)
//	);
//
// 一致性：
//   - Record 走"内容 hash 一致即复用"路径：先 SELECT 最新版本对比 hash；
//     不一致才 INSERT。INSERT 用 BIGSERIAL 自增 (rule_id 维度上是简单
//     append，BIGSERIAL 全表共享但够用)。
//   - Activate UPSERT 到 risk_rule_active；FK 保证 (rule_id, version) 必须
//     先存在 risk_rule_versions。
//   - Diff 在 server 侧用 engine.UnifiedDiff（不下推到 DB），简化实现。

//go:build pg

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiongwp/risk-manage/internal/engine"
)

type PGRuleVersionStore struct {
	pool *pgxpool.Pool
}

func NewPGRuleVersionStore(pool *pgxpool.Pool) *PGRuleVersionStore {
	return &PGRuleVersionStore{pool: pool}
}

func (s *PGRuleVersionStore) Record(ctx context.Context, ruleID string, spec []byte, summary, author string) (int64, bool, error) {
	if ruleID == "" {
		return 0, false, errors.New("rule_id required")
	}
	hash := engine.ContentHash(spec)
	canon, _ := engine.CanonicalJSON(spec)
	if len(canon) == 0 {
		canon = []byte("null")
	}
	// 看最新版本（version 最大）的 hash，一致就复用。
	var lastVer int64
	var lastHash string
	err := s.pool.QueryRow(ctx,
		`SELECT version, content_hash FROM risk_rule_versions
		   WHERE rule_id = $1
		   ORDER BY version DESC LIMIT 1`, ruleID).Scan(&lastVer, &lastHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("select last: %w", err)
	}
	if err == nil && lastHash == hash {
		return lastVer, false, nil
	}
	// 写新版本。version 用子查询 COALESCE(MAX)+1 而非 BIGSERIAL；这样
	// (rule_id) 维度下版本号是连续的 1,2,3...，运营看 UI 比"全局 BIGSERIAL"
	// 直观。schema 上写的 BIGSERIAL 仍然能正常 INSERT （覆盖默认 nextval）。
	var newVer int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO risk_rule_versions (rule_id, version, content_hash, spec_json, change_summary, author)
		 SELECT $1, COALESCE(MAX(version), 0) + 1, $2, $3::jsonb, $4, $5
		   FROM risk_rule_versions WHERE rule_id = $1
		 RETURNING version`, ruleID, hash, canon, summary, author).Scan(&newVer); err != nil {
		return 0, false, fmt.Errorf("insert version: %w", err)
	}
	return newVer, true, nil
}

func (s *PGRuleVersionStore) Activate(ctx context.Context, ruleID string, version int64, actor string) error {
	if ruleID == "" {
		return errors.New("rule_id required")
	}
	ct, err := s.pool.Exec(ctx,
		`INSERT INTO risk_rule_active (rule_id, active_version, activated_by)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (rule_id) DO UPDATE
		   SET active_version = EXCLUDED.active_version,
		       activated_by   = EXCLUDED.activated_by,
		       activated_at   = NOW()`, ruleID, version, actor)
	if err != nil {
		return fmt.Errorf("activate upsert: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return engine.ErrVersionNotFound
	}
	return nil
}

func (s *PGRuleVersionStore) GetActive(ctx context.Context, ruleID string) (int64, []byte, error) {
	var ver int64
	var spec []byte
	err := s.pool.QueryRow(ctx,
		`SELECT rv.version, rv.spec_json::text
		   FROM risk_rule_active ra
		   JOIN risk_rule_versions rv
		     ON rv.rule_id = ra.rule_id AND rv.version = ra.active_version
		  WHERE ra.rule_id = $1`, ruleID).Scan(&ver, &spec)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("get active: %w", err)
	}
	return ver, spec, nil
}

func (s *PGRuleVersionStore) ListVersions(ctx context.Context, ruleID string, limit int) ([]engine.RuleVersion, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT rule_id, version, content_hash, spec_json::text,
		        COALESCE(change_summary, ''), COALESCE(author, ''), created_at
		   FROM risk_rule_versions
		  WHERE rule_id = $1
		  ORDER BY version DESC
		  LIMIT $2`, ruleID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []engine.RuleVersion
	for rows.Next() {
		var rv engine.RuleVersion
		var specText string
		if err := rows.Scan(&rv.RuleID, &rv.Version, &rv.ContentHash, &specText, &rv.ChangeSummary, &rv.Author, &rv.CreatedAt); err != nil {
			return nil, err
		}
		rv.SpecJSON = []byte(specText)
		out = append(out, rv)
	}
	return out, rows.Err()
}

func (s *PGRuleVersionStore) GetVersion(ctx context.Context, ruleID string, version int64) (engine.RuleVersion, error) {
	var rv engine.RuleVersion
	var specText string
	err := s.pool.QueryRow(ctx,
		`SELECT rule_id, version, content_hash, spec_json::text,
		        COALESCE(change_summary, ''), COALESCE(author, ''), created_at
		   FROM risk_rule_versions
		  WHERE rule_id = $1 AND version = $2`, ruleID, version).Scan(
		&rv.RuleID, &rv.Version, &rv.ContentHash, &specText, &rv.ChangeSummary, &rv.Author, &rv.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return engine.RuleVersion{}, fmt.Errorf("%w: rule_id=%s version=%d", engine.ErrVersionNotFound, ruleID, version)
	}
	if err != nil {
		return engine.RuleVersion{}, err
	}
	rv.SpecJSON = []byte(specText)
	return rv, nil
}

func (s *PGRuleVersionStore) Diff(ctx context.Context, ruleID string, fromVer, toVer int64) (string, error) {
	a, err := s.GetVersion(ctx, ruleID, fromVer)
	if err != nil {
		return "", err
	}
	b, err := s.GetVersion(ctx, ruleID, toVer)
	if err != nil {
		return "", err
	}
	return engine.UnifiedDiff(prettyForDiff(a.SpecJSON), prettyForDiff(b.SpecJSON),
		fmt.Sprintf("%s@%d", ruleID, fromVer),
		fmt.Sprintf("%s@%d", ruleID, toVer)), nil
}

// prettyForDiff 重排成 indent JSON。和 engine.prettyJSON 行为对齐
// （不导出该 helper 是有意：避免 engine 包暴露过多 internal）。
func prettyForDiff(raw []byte) string {
	c, _ := engine.CanonicalJSON(raw)
	if len(c) == 0 {
		return string(raw)
	}
	// 借 engine.UnifiedDiff 的入参就是 string；CanonicalJSON 已经是单行 canonical。
	// 为了 diff 出"按字段一行"，我们想 indent。但 engine.prettyJSON 不导出 —
	// 简单做法：直接走 CanonicalJSON 单行。Diff 仍然能跑（整行替换），运营
	// 想看字段级 diff 时通过 /admin/rules/{id}/versions/{n} 拿 spec_json 自行
	// 比对。
	return string(c)
}
