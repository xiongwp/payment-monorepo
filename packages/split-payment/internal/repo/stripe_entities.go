// stripe_entities.go — SP-4: ConnectedAccount / Transfer / ApplicationFee / Payout / Reversal MySQL repos.
//
// 表结构 (跟 mysql.go 的 moneyflow_* 同库):
//
//   connected_accounts (id PK, type, country, default_currency, status,
//                       capabilities_json, business_profile_json, payout_destination_json,
//                       payout_schedule_json, metadata_json, created_at, updated_at)
//
//   transfers (id PK, transfer_group INDEX, source_account, destination_account,
//              amount_minor, currency, status, reversed_amount, source_transaction INDEX,
//              application_fee, graph_run_id INDEX, idempotency_key UNIQUE,
//              metadata_json, created_at, posted_at)
//
//   application_fees (id PK, charge INDEX, account, amount_minor, currency,
//                     status, refunded_amount, graph_run_id, idempotency_key UNIQUE,
//                     metadata_json, created_at)
//
//   payouts (id PK, account INDEX, amount_minor, currency, method, status,
//            arrival_date, failure_code, failure_message, statement_descriptor,
//            destination_json, graph_run_id, idempotency_key UNIQUE, metadata_json, created_at)
//
//   reversals (id PK, transfer INDEX, amount_minor, currency, reason, status,
//              failure_message, idempotency_key UNIQUE, graph_run_id,
//              metadata_json, created_at)
//
// idempotency_key UNIQUE 防止 translator 二次跑产生重复对象 (engine 失败重试场景).
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

// EnsureStripeSchema — 启动期自建 SP-4 五张表.
//
// Idempotent: CREATE IF NOT EXISTS. 生产用 migrate 工具替代.
func EnsureStripeSchema(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS connected_accounts (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			type VARCHAR(16) NOT NULL,
			country VARCHAR(8) NOT NULL,
			default_currency VARCHAR(8) NOT NULL,
			status VARCHAR(16) NOT NULL DEFAULT 'pending',
			capabilities_json JSON,
			business_profile_json JSON,
			payout_destination_json JSON,
			payout_schedule_json JSON,
			metadata_json JSON,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			KEY idx_status (status),
			KEY idx_country (country)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS transfers (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			transfer_group VARCHAR(64),
			source_account VARCHAR(64) NOT NULL,
			destination_account VARCHAR(64) NOT NULL,
			amount_minor BIGINT NOT NULL,
			currency VARCHAR(8) NOT NULL,
			description VARCHAR(256),
			source_transaction VARCHAR(64),
			application_fee VARCHAR(64),
			status VARCHAR(32) NOT NULL DEFAULT 'created',
			reversed_amount BIGINT NOT NULL DEFAULT 0,
			graph_run_id BIGINT,
			idempotency_key VARCHAR(128),
			metadata_json JSON,
			created_at DATETIME NOT NULL,
			posted_at DATETIME,
			UNIQUE KEY uk_idem (idempotency_key),
			KEY idx_group (transfer_group),
			KEY idx_src_tx (source_transaction),
			KEY idx_dest (destination_account),
			KEY idx_run (graph_run_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS application_fees (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			charge VARCHAR(64) NOT NULL,
			account VARCHAR(64),
			amount_minor BIGINT NOT NULL,
			currency VARCHAR(8) NOT NULL,
			status VARCHAR(32) NOT NULL DEFAULT 'pending',
			refunded_amount BIGINT NOT NULL DEFAULT 0,
			graph_run_id BIGINT,
			idempotency_key VARCHAR(128),
			metadata_json JSON,
			created_at DATETIME NOT NULL,
			UNIQUE KEY uk_idem (idempotency_key),
			KEY idx_charge (charge),
			KEY idx_account (account)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS payouts (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			account VARCHAR(64) NOT NULL,
			amount_minor BIGINT NOT NULL,
			currency VARCHAR(8) NOT NULL,
			method VARCHAR(16) NOT NULL DEFAULT 'standard',
			status VARCHAR(32) NOT NULL DEFAULT 'pending',
			arrival_date DATETIME,
			failure_code VARCHAR(64),
			failure_message TEXT,
			statement_descriptor VARCHAR(128),
			destination_json JSON,
			graph_run_id BIGINT,
			idempotency_key VARCHAR(128),
			metadata_json JSON,
			created_at DATETIME NOT NULL,
			UNIQUE KEY uk_idem (idempotency_key),
			KEY idx_account (account),
			KEY idx_status_arrival (status, arrival_date)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS reversals (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			transfer VARCHAR(64) NOT NULL,
			amount_minor BIGINT NOT NULL,
			currency VARCHAR(8) NOT NULL,
			reason VARCHAR(64),
			status VARCHAR(32) NOT NULL DEFAULT 'pending',
			failure_message TEXT,
			idempotency_key VARCHAR(128),
			graph_run_id BIGINT,
			metadata_json JSON,
			created_at DATETIME NOT NULL,
			UNIQUE KEY uk_idem (idempotency_key),
			KEY idx_transfer (transfer)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("ensure stripe schema: %w", err)
		}
	}
	return nil
}

// ─── ConnectedAccount repo ────────────────────────────────────────────

// AccountRepo 接口 + MySQL 实现.
type AccountRepo struct{ db *sql.DB }

// NewAccountRepo 构造.
func NewAccountRepo(db *sql.DB) *AccountRepo { return &AccountRepo{db: db} }

// Upsert 插入或更新 ConnectedAccount.
func (r *AccountRepo) Upsert(ctx context.Context, a *domain.ConnectedAccount) error {
	if a.ID == "" {
		return errors.New("account.id required")
	}
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	caps, _ := json.Marshal(a.Capabilities)
	bp, _ := json.Marshal(a.BusinessProfile)
	pd, _ := json.Marshal(a.PayoutDestination)
	ps, _ := json.Marshal(a.PayoutSchedule)
	meta, _ := json.Marshal(a.Metadata)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO connected_accounts
		(id, type, country, default_currency, status, capabilities_json,
		 business_profile_json, payout_destination_json, payout_schedule_json,
		 metadata_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		  type=VALUES(type), country=VALUES(country),
		  default_currency=VALUES(default_currency), status=VALUES(status),
		  capabilities_json=VALUES(capabilities_json),
		  business_profile_json=VALUES(business_profile_json),
		  payout_destination_json=VALUES(payout_destination_json),
		  payout_schedule_json=VALUES(payout_schedule_json),
		  metadata_json=VALUES(metadata_json),
		  updated_at=VALUES(updated_at)`,
		a.ID, a.Type, a.Country, a.DefaultCurrency, a.Status,
		caps, bp, pd, ps, meta, a.CreatedAt, a.UpdatedAt)
	return err
}

// Get 按 ID 拿.
func (r *AccountRepo) Get(ctx context.Context, id string) (*domain.ConnectedAccount, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, type, country, default_currency, status,
		       capabilities_json, business_profile_json, payout_destination_json,
		       payout_schedule_json, metadata_json, created_at, updated_at
		  FROM connected_accounts WHERE id=?`, id)
	return scanAccount(row)
}

// List 按 status 过滤 ("" / "all" 全部).
func (r *AccountRepo) List(ctx context.Context, status string, limit int) ([]*domain.ConnectedAccount, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	if status == "" || status == "all" {
		rows, err = r.db.QueryContext(ctx, `
			SELECT id, type, country, default_currency, status,
			       capabilities_json, business_profile_json, payout_destination_json,
			       payout_schedule_json, metadata_json, created_at, updated_at
			  FROM connected_accounts ORDER BY updated_at DESC LIMIT ?`, limit)
	} else {
		rows, err = r.db.QueryContext(ctx, `
			SELECT id, type, country, default_currency, status,
			       capabilities_json, business_profile_json, payout_destination_json,
			       payout_schedule_json, metadata_json, created_at, updated_at
			  FROM connected_accounts WHERE status=? ORDER BY updated_at DESC LIMIT ?`, status, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*domain.ConnectedAccount{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanAccount(row interface{ Scan(...any) error }) (*domain.ConnectedAccount, error) {
	var a domain.ConnectedAccount
	var caps, bp, pd, ps, meta []byte
	err := row.Scan(&a.ID, &a.Type, &a.Country, &a.DefaultCurrency, &a.Status,
		&caps, &bp, &pd, &ps, &meta, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(caps, &a.Capabilities)
	_ = json.Unmarshal(bp, &a.BusinessProfile)
	_ = json.Unmarshal(pd, &a.PayoutDestination)
	_ = json.Unmarshal(ps, &a.PayoutSchedule)
	_ = json.Unmarshal(meta, &a.Metadata)
	return &a, nil
}

// ─── Transfer repo ────────────────────────────────────────────────────

// TransferRepo MySQL 实现.
type TransferRepo struct{ db *sql.DB }

// NewTransferRepo 构造.
func NewTransferRepo(db *sql.DB) *TransferRepo { return &TransferRepo{db: db} }

// Insert 写一条 Transfer. idempotency_key 冲突 → 忽略 (DUPLICATE KEY UPDATE no-op).
func (r *TransferRepo) Insert(ctx context.Context, t *domain.Transfer) error {
	if t.ID == "" {
		return errors.New("transfer.id required")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	meta, _ := json.Marshal(t.Metadata)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO transfers
		(id, transfer_group, source_account, destination_account,
		 amount_minor, currency, description, source_transaction, application_fee,
		 status, reversed_amount, graph_run_id, idempotency_key, metadata_json,
		 created_at, posted_at)
		VALUES (?, ?, ?, ?,  ?, ?, ?, ?, ?,  ?, ?, ?, ?, ?,  ?, ?)
		ON DUPLICATE KEY UPDATE id=id`, // no-op: idempotency_key 已存在不再覆写
		t.ID, t.TransferGroup, t.SourceAccount, t.DestinationAccount,
		t.AmountMinor, t.Currency, t.Description, t.SourceTransaction, t.ApplicationFee,
		t.Status, t.ReversedAmount, t.GraphRunID, t.IdempotencyKey, meta,
		t.CreatedAt, nullTime(t.PostedAt))
	return err
}

// UpdateStatus 更新 status / posted_at / reversed_amount.
func (r *TransferRepo) UpdateStatus(ctx context.Context, id, status string, postedAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE transfers SET status=?, posted_at=? WHERE id=?`,
		status, nullTime(postedAt), id)
	return err
}

// AddReversedAmount 累加 reversed_amount; 若达 amount 自动转 reversed 状态.
func (r *TransferRepo) AddReversedAmount(ctx context.Context, id string, delta int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE transfers
		   SET reversed_amount = reversed_amount + ?,
		       status = CASE
		         WHEN reversed_amount + ? >= amount_minor THEN 'reversed'
		         WHEN reversed_amount + ? > 0 THEN 'partially_reversed'
		         ELSE status
		       END
		 WHERE id=?`, delta, delta, delta, id)
	return err
}

// Get 按 ID 拿单条.
func (r *TransferRepo) Get(ctx context.Context, id string) (*domain.Transfer, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, transfer_group, source_account, destination_account,
		       amount_minor, currency, description, source_transaction, application_fee,
		       status, reversed_amount, graph_run_id, idempotency_key, metadata_json,
		       created_at, posted_at
		  FROM transfers WHERE id=?`, id)
	return scanTransfer(row)
}

// ListByGroup 按 transfer_group 拉同组 transfers (退款侧 reverse 时用).
func (r *TransferRepo) ListByGroup(ctx context.Context, group string) ([]*domain.Transfer, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, transfer_group, source_account, destination_account,
		       amount_minor, currency, description, source_transaction, application_fee,
		       status, reversed_amount, graph_run_id, idempotency_key, metadata_json,
		       created_at, posted_at
		  FROM transfers WHERE transfer_group=? ORDER BY created_at`, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*domain.Transfer{}
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanTransfer(row interface{ Scan(...any) error }) (*domain.Transfer, error) {
	var t domain.Transfer
	var meta []byte
	var posted sql.NullTime
	var desc, srcTx, appFee, idem sql.NullString
	err := row.Scan(&t.ID, &t.TransferGroup, &t.SourceAccount, &t.DestinationAccount,
		&t.AmountMinor, &t.Currency, &desc, &srcTx, &appFee,
		&t.Status, &t.ReversedAmount, &t.GraphRunID, &idem, &meta,
		&t.CreatedAt, &posted)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Description = desc.String
	t.SourceTransaction = srcTx.String
	t.ApplicationFee = appFee.String
	t.IdempotencyKey = idem.String
	if posted.Valid {
		t.PostedAt = posted.Time
	}
	_ = json.Unmarshal(meta, &t.Metadata)
	return &t, nil
}

// ─── ApplicationFee repo ──────────────────────────────────────────────

// AppFeeRepo MySQL 实现.
type AppFeeRepo struct{ db *sql.DB }

// NewAppFeeRepo 构造.
func NewAppFeeRepo(db *sql.DB) *AppFeeRepo { return &AppFeeRepo{db: db} }

// Insert.
func (r *AppFeeRepo) Insert(ctx context.Context, f *domain.ApplicationFee) error {
	if f.ID == "" {
		return errors.New("application_fee.id required")
	}
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now().UTC()
	}
	meta, _ := json.Marshal(f.Metadata)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO application_fees
		(id, charge, account, amount_minor, currency, status, refunded_amount,
		 graph_run_id, idempotency_key, metadata_json, created_at)
		VALUES (?, ?, ?, ?, ?,  ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE id=id`,
		f.ID, f.Charge, f.Account, f.AmountMinor, f.Currency,
		f.Status, f.RefundedAmount, f.GraphRunID, f.IdempotencyKey, meta, f.CreatedAt)
	return err
}

// AddRefundedAmount 累加 refunded_amount + 转状态.
func (r *AppFeeRepo) AddRefundedAmount(ctx context.Context, id string, delta int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE application_fees
		   SET refunded_amount = refunded_amount + ?,
		       status = CASE
		         WHEN refunded_amount + ? >= amount_minor THEN 'refunded'
		         WHEN refunded_amount + ? > 0 THEN 'partially_refunded'
		         ELSE status
		       END
		 WHERE id=?`, delta, delta, delta, id)
	return err
}

// ListByCharge 按 charge_id 拉所有 fee.
func (r *AppFeeRepo) ListByCharge(ctx context.Context, charge string) ([]*domain.ApplicationFee, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, charge, account, amount_minor, currency, status, refunded_amount,
		       graph_run_id, idempotency_key, metadata_json, created_at
		  FROM application_fees WHERE charge=? ORDER BY created_at`, charge)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*domain.ApplicationFee{}
	for rows.Next() {
		var f domain.ApplicationFee
		var meta []byte
		var idem, acct sql.NullString
		if err := rows.Scan(&f.ID, &f.Charge, &acct, &f.AmountMinor, &f.Currency,
			&f.Status, &f.RefundedAmount, &f.GraphRunID, &idem, &meta, &f.CreatedAt); err != nil {
			return nil, err
		}
		f.Account = acct.String
		f.IdempotencyKey = idem.String
		_ = json.Unmarshal(meta, &f.Metadata)
		out = append(out, &f)
	}
	return out, rows.Err()
}

// ─── Payout repo ──────────────────────────────────────────────────────

// PayoutRepo MySQL 实现.
type PayoutRepo struct{ db *sql.DB }

// NewPayoutRepo 构造.
func NewPayoutRepo(db *sql.DB) *PayoutRepo { return &PayoutRepo{db: db} }

// Insert.
func (r *PayoutRepo) Insert(ctx context.Context, p *domain.Payout) error {
	if p.ID == "" {
		return errors.New("payout.id required")
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	dest, _ := json.Marshal(p.Destination)
	meta, _ := json.Marshal(p.Metadata)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO payouts
		(id, account, amount_minor, currency, method, status, arrival_date,
		 failure_code, failure_message, statement_descriptor, destination_json,
		 graph_run_id, idempotency_key, metadata_json, created_at)
		VALUES (?, ?, ?, ?, ?,  ?, ?, ?, ?, ?,  ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE id=id`,
		p.ID, p.Account, p.AmountMinor, p.Currency, p.Method, p.Status, nullTime(p.ArrivalDate),
		p.FailureCode, p.FailureMessage, p.StatementDescriptor, dest,
		p.GraphRunID, p.IdempotencyKey, meta, p.CreatedAt)
	return err
}

// UpdateStatus 更新状态 + 失败信息 + arrival_date.
func (r *PayoutRepo) UpdateStatus(ctx context.Context, id, status, failureCode, failureMsg string, arrivalAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE payouts
		   SET status=?, failure_code=?, failure_message=?, arrival_date=?
		 WHERE id=?`,
		status, failureCode, failureMsg, nullTime(arrivalAt), id)
	return err
}

// ListPending 拉 status='pending' 的 payout (SP-FIN-2 dispatch worker 用), 按 created_at 升序.
func (r *PayoutRepo) ListPending(ctx context.Context, limit int) ([]*domain.Payout, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, account, amount_minor, currency, method, status, arrival_date,
		       failure_code, failure_message, statement_descriptor, destination_json,
		       graph_run_id, idempotency_key, metadata_json, created_at
		  FROM payouts
		 WHERE status='pending'
		 ORDER BY created_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending payouts: %w", err)
	}
	defer rows.Close()
	return scanPayoutRows(rows)
}

// scanPayoutRows 共用 rows → []*Payout (ListPending + ListByAccount).
func scanPayoutRows(rows *sql.Rows) ([]*domain.Payout, error) {
	out := []*domain.Payout{}
	for rows.Next() {
		var p domain.Payout
		var dest, meta []byte
		var arrival sql.NullTime
		var failCode, failMsg, stmt, idem sql.NullString
		if err := rows.Scan(&p.ID, &p.Account, &p.AmountMinor, &p.Currency, &p.Method, &p.Status, &arrival,
			&failCode, &failMsg, &stmt, &dest,
			&p.GraphRunID, &idem, &meta, &p.CreatedAt); err != nil {
			return nil, err
		}
		if arrival.Valid {
			p.ArrivalDate = arrival.Time
		}
		p.FailureCode = failCode.String
		p.FailureMessage = failMsg.String
		p.StatementDescriptor = stmt.String
		p.IdempotencyKey = idem.String
		_ = json.Unmarshal(dest, &p.Destination)
		_ = json.Unmarshal(meta, &p.Metadata)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// ListByAccount 按 account 拉 payout 历史.
func (r *PayoutRepo) ListByAccount(ctx context.Context, account string, limit int) ([]*domain.Payout, error) {
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, account, amount_minor, currency, method, status, arrival_date,
		       failure_code, failure_message, statement_descriptor, destination_json,
		       graph_run_id, idempotency_key, metadata_json, created_at
		  FROM payouts WHERE account=? ORDER BY created_at DESC LIMIT ?`, account, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayoutRows(rows)
}

// ─── Reversal repo ────────────────────────────────────────────────────

// ReversalRepo MySQL 实现.
type ReversalRepo struct{ db *sql.DB }

// NewReversalRepo 构造.
func NewReversalRepo(db *sql.DB) *ReversalRepo { return &ReversalRepo{db: db} }

// Insert.
func (r *ReversalRepo) Insert(ctx context.Context, rv *domain.Reversal) error {
	if rv.ID == "" {
		return errors.New("reversal.id required")
	}
	if rv.CreatedAt.IsZero() {
		rv.CreatedAt = time.Now().UTC()
	}
	meta, _ := json.Marshal(rv.Metadata)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO reversals
		(id, transfer, amount_minor, currency, reason, status, failure_message,
		 idempotency_key, graph_run_id, metadata_json, created_at)
		VALUES (?, ?, ?, ?, ?,  ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE id=id`,
		rv.ID, rv.Transfer, rv.AmountMinor, rv.Currency, rv.Reason,
		rv.Status, rv.FailureMessage, rv.IdempotencyKey, rv.GraphRunID, meta, rv.CreatedAt)
	return err
}

// ListByTransfer 按 transfer_id 拉 reversal 历史.
func (r *ReversalRepo) ListByTransfer(ctx context.Context, transferID string) ([]*domain.Reversal, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, transfer, amount_minor, currency, reason, status, failure_message,
		       idempotency_key, graph_run_id, metadata_json, created_at
		  FROM reversals WHERE transfer=? ORDER BY created_at`, transferID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*domain.Reversal{}
	for rows.Next() {
		var rv domain.Reversal
		var meta []byte
		var failMsg, idem sql.NullString
		if err := rows.Scan(&rv.ID, &rv.Transfer, &rv.AmountMinor, &rv.Currency, &rv.Reason,
			&rv.Status, &failMsg, &idem, &rv.GraphRunID, &meta, &rv.CreatedAt); err != nil {
			return nil, err
		}
		rv.FailureMessage = failMsg.String
		rv.IdempotencyKey = idem.String
		_ = json.Unmarshal(meta, &rv.Metadata)
		out = append(out, &rv)
	}
	return out, rows.Err()
}

// ─── helpers ──────────────────────────────────────────────────────────

// nullTime: zero time → SQL NULL.
func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}
