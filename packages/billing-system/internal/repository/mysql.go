// mysql.go — billing 数据持久化的 MySQL 实现。
//
// 接口完全兼容 feecalc.Repository + statement.Repository (memory.go 同形)。
// caller 切换只需要 main.go 改:
//   repo := repository.NewMemoryRepo()                 → 改成 ↓
//   repo, err := repository.NewMySQLRepo(dsn)
//
// DSN 示例:
//   billing_app:billing_app_pwd@tcp(billing-mysql:3306)/billing_db?parseTime=true&charset=utf8mb4

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"reconcile-system/packages/billing-system/internal/domain"
)

// MySQLRepo *sql.DB 包装。
type MySQLRepo struct {
	db *sql.DB
}

// NewMySQLRepo 构造 — 调用方 import _ "github.com/go-sql-driver/mysql"。
func NewMySQLRepo(db *sql.DB) (*MySQLRepo, error) {
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("mysql ping: %w", err)
	}
	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &MySQLRepo{db: db}, nil
}

// Close 关连接。
func (r *MySQLRepo) Close() error { return r.db.Close() }

// ─── feecalc.Repository ────────────────────────────────────────────

func (r *MySQLRepo) SaveFeeEvent(ctx context.Context, ev *domain.FeeEvent) error {
	const q = `INSERT INTO fee_event (
		merchant_id, event_type, ref_id, ref_service,
		gross_amount_minor, currency, fee_minor, fee_minor_base, fx_rate,
		rule_id, rule_name, product, channel_adapter, region,
		status, statement_id, trace_id, occurred_at, created_at
	) VALUES (?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,NULL,?,?,?)`
	res, err := r.db.ExecContext(ctx, q,
		ev.MerchantID, ev.EventType, ev.RefID, ev.RefService,
		ev.GrossAmountMinor, ev.Currency, ev.FeeMinor, ev.FeeMinorBase, ev.FXRate,
		ev.RuleID, ev.RuleName, ev.Product, ev.ChannelAdapter, ev.Region,
		ev.Status, ev.TraceID, ev.OccurredAt, ev.CreatedAt,
	)
	if err != nil {
		// uk_idempotent 撞 = 重复事件，作幂等忽略
		if strings.Contains(err.Error(), "uk_idempotent") || strings.Contains(err.Error(), "Duplicate") {
			return nil
		}
		return fmt.Errorf("insert fee_event: %w", err)
	}
	id, _ := res.LastInsertId()
	ev.ID = id
	return nil
}

func (r *MySQLRepo) LoadOriginalCharge(ctx context.Context, merchantID, refID string) (*domain.FeeEvent, error) {
	const q = `SELECT id, merchant_id, event_type, ref_id, ref_service,
		gross_amount_minor, currency, fee_minor, fee_minor_base, fx_rate,
		rule_id, rule_name, product, channel_adapter, region,
		status, COALESCE(statement_id, 0), trace_id, occurred_at, created_at
		FROM fee_event WHERE merchant_id=? AND ref_id=? AND event_type='charge' LIMIT 1`
	row := r.db.QueryRowContext(ctx, q, merchantID, refID)
	ev := &domain.FeeEvent{}
	err := row.Scan(&ev.ID, &ev.MerchantID, &ev.EventType, &ev.RefID, &ev.RefService,
		&ev.GrossAmountMinor, &ev.Currency, &ev.FeeMinor, &ev.FeeMinorBase, &ev.FXRate,
		&ev.RuleID, &ev.RuleName, &ev.Product, &ev.ChannelAdapter, &ev.Region,
		&ev.Status, &ev.StatementID, &ev.TraceID, &ev.OccurredAt, &ev.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query original charge: %w", err)
	}
	return ev, nil
}

func (r *MySQLRepo) ExistsByRef(ctx context.Context, merchantID, refID string, eventType domain.EventType) (bool, error) {
	const q = `SELECT 1 FROM fee_event WHERE merchant_id=? AND ref_id=? AND event_type=? LIMIT 1`
	var dummy int
	err := r.db.QueryRowContext(ctx, q, merchantID, refID, eventType).Scan(&dummy)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ─── statement.Repository ──────────────────────────────────────────

func (r *MySQLRepo) ListPendingEvents(ctx context.Context, merchantID string, from, to time.Time) ([]*domain.FeeEvent, error) {
	const q = `SELECT id, merchant_id, event_type, ref_id, ref_service,
		gross_amount_minor, currency, fee_minor, fee_minor_base, fx_rate,
		rule_id, rule_name, product, channel_adapter, region,
		status, COALESCE(statement_id, 0), trace_id, occurred_at, created_at
		FROM fee_event
		WHERE merchant_id=? AND status='pending' AND occurred_at >= ? AND occurred_at < ?
		ORDER BY occurred_at ASC`
	rows, err := r.db.QueryContext(ctx, q, merchantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("query pending events: %w", err)
	}
	defer rows.Close()
	var out []*domain.FeeEvent
	for rows.Next() {
		ev := &domain.FeeEvent{}
		err := rows.Scan(&ev.ID, &ev.MerchantID, &ev.EventType, &ev.RefID, &ev.RefService,
			&ev.GrossAmountMinor, &ev.Currency, &ev.FeeMinor, &ev.FeeMinorBase, &ev.FXRate,
			&ev.RuleID, &ev.RuleName, &ev.Product, &ev.ChannelAdapter, &ev.Region,
			&ev.Status, &ev.StatementID, &ev.TraceID, &ev.OccurredAt, &ev.CreatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (r *MySQLRepo) SaveStatement(ctx context.Context, s *domain.Statement) (int64, error) {
	const q = `INSERT INTO statement (
		merchant_id, period_start, period_end, currency,
		total_gross_minor, total_fee_minor, total_refund_minor, total_chargeback_minor,
		net_payout_minor, event_count, status, pdf_url, issued_at, payout_id
	) VALUES (?,?,?,?, ?,?,?,?, ?,?,?,?,?,0)
	ON DUPLICATE KEY UPDATE
		total_gross_minor = VALUES(total_gross_minor),
		total_fee_minor = VALUES(total_fee_minor),
		total_refund_minor = VALUES(total_refund_minor),
		total_chargeback_minor = VALUES(total_chargeback_minor),
		net_payout_minor = VALUES(net_payout_minor),
		event_count = VALUES(event_count),
		status = VALUES(status)`
	res, err := r.db.ExecContext(ctx, q,
		s.MerchantID, s.PeriodStart, s.PeriodEnd, s.Currency,
		s.TotalGrossMinor, s.TotalFeeMinor, s.TotalRefundMinor, s.TotalChargebackMinor,
		s.NetPayoutMinor, s.EventCount, s.Status, s.PDFURL, s.IssuedAt)
	if err != nil {
		return 0, fmt.Errorf("save statement: %w", err)
	}
	id, _ := res.LastInsertId()
	if id == 0 {
		// ON DUPLICATE 时 LastInsertId 是 0；再查一次
		err := r.db.QueryRowContext(ctx,
			`SELECT id FROM statement WHERE merchant_id=? AND period_start=? AND period_end=? AND currency=?`,
			s.MerchantID, s.PeriodStart, s.PeriodEnd, s.Currency).Scan(&id)
		if err != nil {
			return 0, err
		}
	}
	return id, nil
}

func (r *MySQLRepo) MarkEventsSettled(ctx context.Context, eventIDs []int64, statementID int64) error {
	if len(eventIDs) == 0 {
		return nil
	}
	// 批量 update 用 IN clause
	placeholders := strings.Repeat("?,", len(eventIDs))
	placeholders = strings.TrimSuffix(placeholders, ",")
	q := fmt.Sprintf(`UPDATE fee_event SET status='settled', statement_id=?
		WHERE id IN (%s)`, placeholders)
	args := make([]any, 0, len(eventIDs)+1)
	args = append(args, statementID)
	for _, id := range eventIDs {
		args = append(args, id)
	}
	_, err := r.db.ExecContext(ctx, q, args...)
	return err
}

func (r *MySQLRepo) ListMerchantIDs(ctx context.Context, from, to time.Time) ([]string, error) {
	const q = `SELECT DISTINCT merchant_id FROM fee_event
		WHERE status='pending' AND occurred_at >= ? AND occurred_at < ?`
	rows, err := r.db.QueryContext(ctx, q, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var mid string
		if err := rows.Scan(&mid); err != nil {
			return nil, err
		}
		out = append(out, mid)
	}
	return out, rows.Err()
}

// ─── admin web 查询 ─────────────────────────────────────────────────

func (r *MySQLRepo) ListStatementsByMerchant(ctx context.Context, merchantID string, limit int) ([]*domain.Statement, error) {
	if limit <= 0 || limit > 200 {
		limit = 12
	}
	const q = `SELECT id, merchant_id, period_start, period_end, currency,
		total_gross_minor, total_fee_minor, total_refund_minor, total_chargeback_minor,
		net_payout_minor, event_count, status, pdf_url, issued_at, payout_id
		FROM statement WHERE merchant_id=? ORDER BY period_start DESC LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, merchantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Statement
	for rows.Next() {
		s := &domain.Statement{}
		err := rows.Scan(&s.ID, &s.MerchantID, &s.PeriodStart, &s.PeriodEnd, &s.Currency,
			&s.TotalGrossMinor, &s.TotalFeeMinor, &s.TotalRefundMinor, &s.TotalChargebackMinor,
			&s.NetPayoutMinor, &s.EventCount, &s.Status, &s.PDFURL, &s.IssuedAt, &s.PayoutID)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *MySQLRepo) GetStatement(ctx context.Context, id int64) (*domain.Statement, error) {
	const q = `SELECT id, merchant_id, period_start, period_end, currency,
		total_gross_minor, total_fee_minor, total_refund_minor, total_chargeback_minor,
		net_payout_minor, event_count, status, pdf_url, issued_at, payout_id
		FROM statement WHERE id=?`
	row := r.db.QueryRowContext(ctx, q, id)
	s := &domain.Statement{}
	err := row.Scan(&s.ID, &s.MerchantID, &s.PeriodStart, &s.PeriodEnd, &s.Currency,
		&s.TotalGrossMinor, &s.TotalFeeMinor, &s.TotalRefundMinor, &s.TotalChargebackMinor,
		&s.NetPayoutMinor, &s.EventCount, &s.Status, &s.PDFURL, &s.IssuedAt, &s.PayoutID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (r *MySQLRepo) ListStatementEvents(ctx context.Context, statementID int64) ([]*domain.FeeEvent, error) {
	const q = `SELECT id, merchant_id, event_type, ref_id, ref_service,
		gross_amount_minor, currency, fee_minor, fee_minor_base, fx_rate,
		rule_id, rule_name, product, channel_adapter, region,
		status, COALESCE(statement_id, 0), trace_id, occurred_at, created_at
		FROM fee_event WHERE statement_id=? ORDER BY occurred_at ASC`
	rows, err := r.db.QueryContext(ctx, q, statementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.FeeEvent
	for rows.Next() {
		ev := &domain.FeeEvent{}
		err := rows.Scan(&ev.ID, &ev.MerchantID, &ev.EventType, &ev.RefID, &ev.RefService,
			&ev.GrossAmountMinor, &ev.Currency, &ev.FeeMinor, &ev.FeeMinorBase, &ev.FXRate,
			&ev.RuleID, &ev.RuleName, &ev.Product, &ev.ChannelAdapter, &ev.Region,
			&ev.Status, &ev.StatementID, &ev.TraceID, &ev.OccurredAt, &ev.CreatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
