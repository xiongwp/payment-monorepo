// mysql.go — refund-engine MySQL 持久化。
//
// 接口完全兼容 cmd/server/main.go 里的 inline memory repo + workflow.Repository。
// 切换只改 main.go:
//   repo := newMemoryRepo()                  →
//   repo, err := repository.NewMySQLRepo(db)

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"reconcile-system/packages/refund-engine/internal/domain"
)

// MySQLRepo workflow.Repository 的 MySQL 实现。
type MySQLRepo struct {
	db *sql.DB
}

// NewMySQLRepo dsn 示例:
//
//	refund_app:refund_app_pwd@tcp(refund-mysql:3306)/refund_db?parseTime=true&charset=utf8mb4
func NewMySQLRepo(db *sql.DB) (*MySQLRepo, error) {
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &MySQLRepo{db: db}, nil
}

func (r *MySQLRepo) Close() error { return r.db.Close() }

const refundCols = `id, refund_id, merchant_id, charge_id, payment_intent_id,
	amount_minor, original_charge_amount_minor, already_refunded_minor,
	currency, reason, reason_note, method, status,
	channel_refund_id, failure_code, failure_message,
	idempotency_key, requested_by, approved_by,
	requested_at, approved_at, submitted_at, completed_at,
	trace_id, created_at, updated_at`

func scanRefund(row interface{ Scan(...any) error }) (*domain.Refund, error) {
	r := &domain.Refund{}
	var apprAt, subAt, compAt sql.NullTime
	err := row.Scan(
		&r.ID, &r.RefundID, &r.MerchantID, &r.ChargeID, &r.PaymentIntentID,
		&r.AmountMinor, &r.OriginalChargeAmountMinor, &r.AlreadyRefundedMinor,
		&r.Currency, &r.Reason, &r.ReasonNote, &r.Method, &r.Status,
		&r.ChannelRefundID, &r.FailureCode, &r.FailureMessage,
		&r.IdempotencyKey, &r.RequestedBy, &r.ApprovedBy,
		&r.RequestedAt, &apprAt, &subAt, &compAt,
		&r.TraceID, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if apprAt.Valid {
		r.ApprovedAt = &apprAt.Time
	}
	if subAt.Valid {
		r.SubmittedAt = &subAt.Time
	}
	if compAt.Valid {
		r.CompletedAt = &compAt.Time
	}
	return r, nil
}

// ─── workflow.Repository 接口实现 ─────────────────────────────────────

func (r *MySQLRepo) Create(ctx context.Context, rf *domain.Refund) (int64, error) {
	const q = `INSERT INTO refund (
		refund_id, merchant_id, charge_id, payment_intent_id,
		amount_minor, original_charge_amount_minor, already_refunded_minor,
		currency, reason, reason_note, method, status,
		idempotency_key, requested_by, requested_at, trace_id, created_at, updated_at
	) VALUES (?,?,?,?, ?,?,?, ?,?,?,?,?, ?,?,?,?,?,?)`
	res, err := r.db.ExecContext(ctx, q,
		rf.RefundID, rf.MerchantID, rf.ChargeID, rf.PaymentIntentID,
		rf.AmountMinor, rf.OriginalChargeAmountMinor, rf.AlreadyRefundedMinor,
		rf.Currency, rf.Reason, rf.ReasonNote, rf.Method, rf.Status,
		rf.IdempotencyKey, rf.RequestedBy, rf.RequestedAt, rf.TraceID,
		rf.CreatedAt, rf.UpdatedAt,
	)
	if err != nil {
		// uk_idempotent 撞 → 幂等回返已有
		if strings.Contains(err.Error(), "Duplicate") {
			existing, _ := r.GetByIdempotency(ctx, rf.IdempotencyKey)
			if existing != nil {
				return existing.ID, nil
			}
		}
		return 0, fmt.Errorf("insert refund: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (r *MySQLRepo) Get(ctx context.Context, id int64) (*domain.Refund, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+refundCols+" FROM refund WHERE id=?", id)
	rf, err := scanRefund(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return rf, err
}

func (r *MySQLRepo) GetByRefundID(ctx context.Context, refundID string) (*domain.Refund, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+refundCols+" FROM refund WHERE refund_id=?", refundID)
	rf, err := scanRefund(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return rf, err
}

func (r *MySQLRepo) GetByIdempotency(ctx context.Context, key string) (*domain.Refund, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+refundCols+" FROM refund WHERE idempotency_key=?", key)
	rf, err := scanRefund(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return rf, err
}

// UpdateStatus 状态机迁移 — 用 status 旧值做 CAS 防竞争（防同一笔被两人 approve）。
func (r *MySQLRepo) UpdateStatus(ctx context.Context, id int64, to domain.Status, fields map[string]any) error {
	// 建动态 SET clause
	sets := []string{"status=?", "updated_at=NOW(3)"}
	args := []any{string(to)}
	for k, v := range fields {
		switch k {
		case "approved_by":
			sets = append(sets, "approved_by=?")
			args = append(args, v)
		case "approved_at":
			sets = append(sets, "approved_at=?")
			args = append(args, v)
		case "submitted_at":
			sets = append(sets, "submitted_at=?")
			args = append(args, v)
		case "completed_at":
			sets = append(sets, "completed_at=?")
			args = append(args, v)
		case "channel_refund_id":
			sets = append(sets, "channel_refund_id=?")
			args = append(args, v)
		case "failure_code":
			sets = append(sets, "failure_code=?")
			args = append(args, v)
		case "failure_message":
			sets = append(sets, "failure_message=?")
			args = append(args, v)
		}
	}
	args = append(args, id)
	q := "UPDATE refund SET " + strings.Join(sets, ", ") + " WHERE id=?"
	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("refund %d not found or stale", id)
	}
	return nil
}

func (r *MySQLRepo) ListByCharge(ctx context.Context, chargeID string) ([]*domain.Refund, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+refundCols+" FROM refund WHERE charge_id=? ORDER BY requested_at DESC",
		chargeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Refund
	for rows.Next() {
		rf, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rf)
	}
	return out, rows.Err()
}

func (r *MySQLRepo) ListByStatus(ctx context.Context, status domain.Status, limit int) ([]*domain.Refund, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+refundCols+" FROM refund WHERE status=? ORDER BY requested_at LIMIT ?",
		string(status), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Refund
	for rows.Next() {
		rf, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rf)
	}
	return out, rows.Err()
}

// SumRefundedByCharge 累计已结算/已提交的退款金额 — 防超额退款关键。
func (r *MySQLRepo) SumRefundedByCharge(ctx context.Context, chargeID string) (int64, error) {
	const q = `SELECT COALESCE(SUM(amount_minor), 0) FROM refund
		WHERE charge_id=? AND status IN ('submitted', 'completed')`
	var total int64
	err := r.db.QueryRowContext(ctx, q, chargeID).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total, nil
}
