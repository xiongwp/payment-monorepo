// saga_store.go — SP-3A SagaInstance MySQL 持久化.
//
// 表:
//   moneyflow_sagas (saga_id PK, graph_run_id, correlation_id, state,
//                    current_step, steps_json, started_at, completed_at,
//                    updated_at)
//
// 重启 resume: ListUnfinished WHERE state IN ('started','forwarding') ORDER BY started_at LIMIT N.
package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"reconcile-system/packages/split-payment/internal/workflow"
)

// EnsureSagaSchema 启动期建表.
func EnsureSagaSchema(ctx context.Context, db *sql.DB) error {
	stmt := `CREATE TABLE IF NOT EXISTS moneyflow_sagas (
		saga_id        VARCHAR(64) NOT NULL PRIMARY KEY,
		graph_run_id   BIGINT,
		correlation_id VARCHAR(128),
		state          VARCHAR(32) NOT NULL,
		current_step   INT NOT NULL DEFAULT 0,
		steps_json     JSON NOT NULL,
		started_at     DATETIME NOT NULL,
		completed_at   DATETIME,
		updated_at     DATETIME NOT NULL,
		KEY idx_state (state),
		KEY idx_run (graph_run_id),
		KEY idx_started (started_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("ensure saga schema: %w", err)
	}
	return nil
}

// MySQLSagaStore 实现 workflow.SagaStore.
type MySQLSagaStore struct{ db *sql.DB }

// NewMySQLSagaStore.
func NewMySQLSagaStore(db *sql.DB) *MySQLSagaStore { return &MySQLSagaStore{db: db} }

// Save UPSERT.
func (s *MySQLSagaStore) Save(ctx context.Context, inst *workflow.SagaInstance) error {
	if inst.SagaID == "" {
		return errors.New("saga_id required")
	}
	// steps_json 只持序列化字段 (Execute/Compensate 闭包不序列化, 标 `json:"-"`)
	stepsJSON, err := json.Marshal(inst.Steps)
	if err != nil {
		return fmt.Errorf("marshal steps: %w", err)
	}
	now := time.Now().UTC()
	if inst.StartedAt.IsZero() {
		inst.StartedAt = now
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO moneyflow_sagas
		(saga_id, graph_run_id, correlation_id, state, current_step,
		 steps_json, started_at, completed_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		  state=VALUES(state),
		  current_step=VALUES(current_step),
		  steps_json=VALUES(steps_json),
		  completed_at=VALUES(completed_at),
		  updated_at=VALUES(updated_at)`,
		inst.SagaID, inst.GraphRunID, inst.CorrelationID,
		string(inst.State), inst.CurrentStep,
		stepsJSON, inst.StartedAt, nullTime(inst.CompletedAt), now)
	if err != nil {
		return fmt.Errorf("saga save: %w", err)
	}
	return nil
}

// Load.
func (s *MySQLSagaStore) Load(ctx context.Context, sagaID string) (*workflow.SagaInstance, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT saga_id, graph_run_id, correlation_id, state, current_step,
		       steps_json, started_at, completed_at
		  FROM moneyflow_sagas WHERE saga_id=?`, sagaID)
	return scanSaga(row)
}

// ListUnfinished — state ∈ {started, forwarding} 的 saga, 按 started_at 升序.
func (s *MySQLSagaStore) ListUnfinished(ctx context.Context, limit int) ([]*workflow.SagaInstance, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT saga_id, graph_run_id, correlation_id, state, current_step,
		       steps_json, started_at, completed_at
		  FROM moneyflow_sagas
		 WHERE state IN ('started', 'forwarding')
		 ORDER BY started_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("saga list: %w", err)
	}
	defer rows.Close()
	out := []*workflow.SagaInstance{}
	for rows.Next() {
		inst, err := scanSaga(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

func scanSaga(row interface{ Scan(...any) error }) (*workflow.SagaInstance, error) {
	var (
		inst         workflow.SagaInstance
		graphRunID   sql.NullInt64
		corrID       sql.NullString
		stepsJSON    []byte
		completedAt  sql.NullTime
		state        string
	)
	err := row.Scan(&inst.SagaID, &graphRunID, &corrID, &state, &inst.CurrentStep,
		&stepsJSON, &inst.StartedAt, &completedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	inst.GraphRunID = graphRunID.Int64
	inst.CorrelationID = corrID.String
	inst.State = workflow.SagaState(state)
	if completedAt.Valid {
		inst.CompletedAt = completedAt.Time
	}
	if len(stepsJSON) > 0 {
		if err := json.Unmarshal(stepsJSON, &inst.Steps); err != nil {
			return nil, fmt.Errorf("unmarshal steps: %w", err)
		}
	}
	return &inst, nil
}
