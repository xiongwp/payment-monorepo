// mysql.go — AML store MySQL 实现.
//
// 表设计:
//   - list_entries     名单 (sanction / pep / 内部黑名单), 索引 first-letter + source
//   - screen_results   screen 调用历史 (按 request_id)
//   - hit_records      screen 命中详情 (复核工单)
//
// 70w+ 条名单 (OFAC + EU + UK + UN + 商用 PEP) 全表扫成本: ~3s; 用 first-letter
// + source + nationality 复合索引可降到 50ms.
//
// 真生产 scale 到 700w 条 (含 adverse media), 按 first-letter 分 26 shard.

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"reconcile-system/packages/aml-screening/internal/domain"
)

// MySQLStore 用单 DB 简单实现; shard 版本另起一个 ShardedMySQLStore.
type MySQLStore struct {
	DB *sql.DB
}

func NewMySQLStore(db *sql.DB) *MySQLStore {
	return &MySQLStore{DB: db}
}

func (s *MySQLStore) UpsertEntry(e domain.ListEntry) error {
	aliasesJSON, _ := json.Marshal(e.Aliases)
	nationalityJSON, _ := json.Marshal(e.Nationality)
	addressesJSON, _ := json.Marshal(e.Addresses)
	idDocsJSON, _ := json.Marshal(e.IDDocuments)
	firstLetter := ""
	if e.PrimaryName != "" {
		firstLetter = strings.ToLower(string([]rune(e.PrimaryName)[0]))
	}

	_, err := s.DB.Exec(`
		INSERT INTO list_entries
		  (entry_id, source, entity_type, primary_name, first_letter, dob, birth_place,
		   aliases_json, nationality_json, addresses_json, id_docs_json,
		   program, remarks, listed_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		  primary_name = VALUES(primary_name),
		  first_letter = VALUES(first_letter),
		  dob = VALUES(dob),
		  aliases_json = VALUES(aliases_json),
		  nationality_json = VALUES(nationality_json),
		  addresses_json = VALUES(addresses_json),
		  id_docs_json = VALUES(id_docs_json),
		  program = VALUES(program),
		  updated_at = VALUES(updated_at)`,
		e.ID, e.Source, e.EntityType, e.PrimaryName, firstLetter,
		e.DOB, e.BirthPlace,
		aliasesJSON, nationalityJSON, addressesJSON, idDocsJSON,
		e.Program, e.Remarks, e.ListedAt, e.UpdatedAt,
	)
	return err
}

func (s *MySQLStore) GetEntry(src domain.ListSource, id string) (domain.ListEntry, error) {
	var e domain.ListEntry
	var aliasesJSON, nationalityJSON, addressesJSON, idDocsJSON []byte
	err := s.DB.QueryRow(`
		SELECT entry_id, source, entity_type, primary_name, dob, birth_place,
		       aliases_json, nationality_json, addresses_json, id_docs_json,
		       program, remarks, listed_at, updated_at
		  FROM list_entries WHERE source=? AND entry_id=? LIMIT 1`,
		src, id).Scan(&e.ID, &e.Source, &e.EntityType, &e.PrimaryName, &e.DOB, &e.BirthPlace,
		&aliasesJSON, &nationalityJSON, &addressesJSON, &idDocsJSON,
		&e.Program, &e.Remarks, &e.ListedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ListEntry{}, ErrNotFound
	}
	if err != nil {
		return domain.ListEntry{}, err
	}
	_ = json.Unmarshal(aliasesJSON, &e.Aliases)
	_ = json.Unmarshal(nationalityJSON, &e.Nationality)
	_ = json.Unmarshal(addressesJSON, &e.Addresses)
	_ = json.Unmarshal(idDocsJSON, &e.IDDocuments)
	return e, nil
}

func (s *MySQLStore) Candidates(prefix string, sources []domain.ListSource, countryISO string) ([]domain.ListEntry, error) {
	args := []interface{}{prefix}
	sql := `SELECT entry_id, source, entity_type, primary_name, dob, birth_place,
	               aliases_json, nationality_json, addresses_json, id_docs_json, program
	          FROM list_entries WHERE first_letter = ?`
	if len(sources) > 0 {
		placeholders := strings.Repeat("?,", len(sources))
		placeholders = placeholders[:len(placeholders)-1]
		sql += " AND source IN (" + placeholders + ")"
		for _, src := range sources {
			args = append(args, src)
		}
	}
	if countryISO != "" {
		// nationality JSON LIKE 模糊匹配 — fine for prefix scan, perf 一般 OK
		sql += " AND nationality_json LIKE ?"
		args = append(args, "%\""+countryISO+"\"%")
	}
	sql += " LIMIT 5000"
	rows, err := s.DB.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.ListEntry, 0, 100)
	for rows.Next() {
		var e domain.ListEntry
		var aliasesJSON, nationalityJSON, addressesJSON, idDocsJSON []byte
		if err := rows.Scan(&e.ID, &e.Source, &e.EntityType, &e.PrimaryName, &e.DOB, &e.BirthPlace,
			&aliasesJSON, &nationalityJSON, &addressesJSON, &idDocsJSON, &e.Program); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(aliasesJSON, &e.Aliases)
		_ = json.Unmarshal(nationalityJSON, &e.Nationality)
		_ = json.Unmarshal(addressesJSON, &e.Addresses)
		_ = json.Unmarshal(idDocsJSON, &e.IDDocuments)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *MySQLStore) SaveResult(res domain.ScreenResult, req domain.ScreenRequest) error {
	resJSON, _ := json.Marshal(res)
	reqJSON, _ := json.Marshal(req)
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
		INSERT INTO screen_results (request_id, action, highest_hit, screened_at, latency_ms, result_json, request_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE action=VALUES(action), highest_hit=VALUES(highest_hit), result_json=VALUES(result_json)`,
		res.RequestID, res.Action, res.HighestHit, res.ScreenedAt, res.Latency_ms, resJSON, reqJSON)
	if err != nil {
		return err
	}
	// hits: per-hit row 给 ops 复核 queue 用
	for _, h := range res.Hits {
		entryJSON, _ := json.Marshal(h.ListEntry)
		matchedJSON, _ := json.Marshal(h.MatchedOn)
		_, err = tx.Exec(`
			INSERT INTO hit_records
			  (hit_id, request_id, source, confidence, matched_on_json, algorithm, state, entry_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			h.HitID, res.RequestID, h.ListEntry.Source, h.Confidence,
			matchedJSON, h.Algorithm, h.State, entryJSON, time.Now().UTC())
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *MySQLStore) GetResult(requestID string) (domain.ScreenResult, error) {
	var resJSON []byte
	err := s.DB.QueryRow(`SELECT result_json FROM screen_results WHERE request_id=?`, requestID).Scan(&resJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ScreenResult{}, ErrNotFound
	}
	if err != nil {
		return domain.ScreenResult{}, err
	}
	var out domain.ScreenResult
	_ = json.Unmarshal(resJSON, &out)
	return out, nil
}

func (s *MySQLStore) UpdateHitState(hitID string, decision domain.HitResolution) error {
	_, err := s.DB.Exec(`
		UPDATE hit_records SET state=?, reviewer=?, reason=?, evidence=?, resolved_at=?
		 WHERE hit_id=?`,
		decision.Decision, decision.Reviewer, decision.Reason, decision.Evidence, decision.ResolvedAt, hitID)
	return err
}

func (s *MySQLStore) ListPendingHits(limit, offset int) ([]domain.HitInfo, error) {
	rows, err := s.DB.Query(`
		SELECT hit_id, source, confidence, matched_on_json, algorithm, state, entry_json
		  FROM hit_records WHERE state='pending_review' ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.HitInfo, 0, limit)
	for rows.Next() {
		var h domain.HitInfo
		var src string
		var matchedJSON, entryJSON []byte
		if err := rows.Scan(&h.HitID, &src, &h.Confidence, &matchedJSON, &h.Algorithm, &h.State, &entryJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(matchedJSON, &h.MatchedOn)
		_ = json.Unmarshal(entryJSON, &h.ListEntry)
		out = append(out, h)
	}
	return out, nil
}

func (s *MySQLStore) EntryCount(src domain.ListSource) (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM list_entries WHERE source=?`, src).Scan(&n)
	return n, err
}

func (s *MySQLStore) PurgeStaleEntries(src domain.ListSource, before time.Time) (int, error) {
	res, err := s.DB.Exec(`DELETE FROM list_entries WHERE source=? AND updated_at < ?`, src, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// 避免 unused import (fmt)
var _ = fmt.Sprintf
