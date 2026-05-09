// postgres_review.go: review.Store / feedback.Recorder 的 Postgres 参考实现。
//
// **未编译进默认 build**：避免给 risk-manage 强制加 pgx 依赖。
// 接入时：
//
//  1. go.mod 加：require github.com/jackc/pgx/v5 v5.x.x
//  2. 删除文件顶部的 //go:build pg 标签
//  3. main.go newReviewStore 切到这里：
//     pool, _ := pgxpool.New(ctx, dsn)
//     reviewStore := store.NewPGReviewStore(pool)
//
// Schema（Postgres 17，UTC tz）：
//
//	CREATE TABLE risk_review (
//	    id              TEXT PRIMARY KEY,
//	    merchant_id     TEXT NOT NULL,
//	    customer_id     TEXT NOT NULL,
//	    payment_intent_id TEXT NOT NULL,
//	    amount          BIGINT NOT NULL,
//	    currency        TEXT NOT NULL,
//	    risk_score      INT NOT NULL,
//	    reasons         JSONB NOT NULL,
//	    status          TEXT NOT NULL,             -- pending|in_review|escalated|approved|rejected
//	    sla_deadline    TIMESTAMPTZ NOT NULL,
//	    assigned_to     TEXT,
//	    assigned_at     TIMESTAMPTZ,
//	    escalate_level  INT NOT NULL DEFAULT 0,
//	    notes           JSONB NOT NULL DEFAULT '[]',
//	    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
//	    decided_at      TIMESTAMPTZ,
//	    decided_by      TEXT,
//	    decide_reason   TEXT
//	);
//	CREATE INDEX risk_review_status_created ON risk_review (status, created_at DESC);
//	CREATE INDEX risk_review_assignee       ON risk_review (assigned_to, sla_deadline);
//	CREATE INDEX risk_review_sla            ON risk_review (sla_deadline) WHERE status IN ('pending','in_review','escalated');
//	CREATE INDEX risk_review_merchant       ON risk_review (merchant_id, created_at DESC);
//
//	CREATE TABLE risk_outcome (
//	    id           BIGSERIAL PRIMARY KEY,
//	    decision_id  TEXT NOT NULL REFERENCES risk_review(id),
//	    source       TEXT NOT NULL,
//	    is_fraud     BOOLEAN NOT NULL,
//	    at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
//	    actor        TEXT,
//	    notes        TEXT
//	);
//	CREATE INDEX risk_outcome_did ON risk_outcome (decision_id, at DESC);
//
// 一致性 / 并发：
//   - Push: INSERT ... ON CONFLICT (id) DO NOTHING 幂等
//   - Decide: UPDATE WHERE id=? AND status IN ('pending','in_review','escalated')
//   - Claim: UPDATE WHERE status='pending' OR (status='in_review' AND assigned_to=actor) — 防双 actor 抢
//   - Escalate / Release: UPDATE WHERE assigned_to=actor — 防越权

//go:build pg

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiongwp/risk-manage/internal/feedback"
	"github.com/xiongwp/risk-manage/internal/review"
)

// 默认 SLA：每条 case Push 时按 24h 算 deadline；用 SetDefaultSLA 调。
const pgDefaultSLA = 24 * time.Hour

type PGReviewStore struct {
	pool       *pgxpool.Pool
	defaultSLA time.Duration
}

func NewPGReviewStore(pool *pgxpool.Pool) *PGReviewStore {
	return &PGReviewStore{pool: pool, defaultSLA: pgDefaultSLA}
}

func (s *PGReviewStore) SetDefaultSLA(d time.Duration) {
	if d <= 0 {
		d = pgDefaultSLA
	}
	s.defaultSLA = d
}

// ── case 增删改查 ────────────────────────────────────────────────────

func (s *PGReviewStore) Push(item review.Item) error {
	if item.ID == "" {
		return errors.New("review: id required")
	}
	if item.Status == "" {
		item.Status = review.StatusPending
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	if item.SLADeadline.IsZero() {
		item.SLADeadline = item.CreatedAt.Add(s.defaultSLA)
	}
	reasonsJSON, _ := json.Marshal(item.Reasons)
	notesJSON, _ := json.Marshal(item.Notes)
	if len(notesJSON) == 0 {
		notesJSON = []byte("[]")
	}
	const q = `
INSERT INTO risk_review
  (id, merchant_id, customer_id, payment_intent_id, amount, currency,
   risk_score, reasons, status, sla_deadline, escalate_level, notes, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (id) DO NOTHING`
	_, err := s.pool.Exec(context.Background(), q,
		item.ID, item.MerchantID, item.CustomerID, item.PaymentIntentID,
		item.Amount, item.Currency, item.RiskScore, reasonsJSON,
		string(item.Status), item.SLADeadline, item.EscalateLevel, notesJSON,
		item.CreatedAt)
	return err
}

func (s *PGReviewStore) Decide(id string, action review.Action, actor, reason string) (*review.Item, error) {
	var newStatus review.Status
	switch action {
	case review.ActionApprove:
		newStatus = review.StatusApproved
	case review.ActionReject:
		newStatus = review.StatusRejected
	default:
		return nil, errors.New("review: invalid action")
	}
	const q = `
UPDATE risk_review
   SET status=$2, decided_at=NOW(), decided_by=$3, decide_reason=$4
 WHERE id=$1 AND status IN ('pending','in_review','escalated')
RETURNING ` + reviewCols
	row := s.pool.QueryRow(context.Background(), q, id, string(newStatus), actor, reason)
	it, err := scanReviewRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, review.ErrNotPending
		}
		return nil, err
	}
	return it, nil
}

func (s *PGReviewStore) Claim(id, actor string) (*review.Item, error) {
	if actor == "" {
		return nil, errors.New("review: actor required")
	}
	// 允许 (pending → in_review) 或 (in_review by 同 actor 重复 claim 幂等)
	const q = `
UPDATE risk_review
   SET status='in_review', assigned_to=$2, assigned_at=NOW()
 WHERE id=$1 AND (
        status='pending'
     OR (status='in_review' AND assigned_to=$2)
 )
RETURNING ` + reviewCols
	row := s.pool.QueryRow(context.Background(), q, id, actor)
	it, err := scanReviewRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 区分"不存在"vs"已被他人 claim"
			cur := s.Get(id)
			if cur == nil {
				return nil, review.ErrNotPending
			}
			if cur.Status == review.StatusInReview && cur.AssignedTo != actor {
				return nil, review.ErrAlreadyClaimed
			}
			return nil, review.ErrNotPending
		}
		return nil, err
	}
	return it, nil
}

func (s *PGReviewStore) Release(id, actor string) (*review.Item, error) {
	const q = `
UPDATE risk_review
   SET status='pending', assigned_to=NULL, assigned_at=NULL
 WHERE id=$1 AND status='in_review' AND assigned_to=$2
RETURNING ` + reviewCols
	row := s.pool.QueryRow(context.Background(), q, id, actor)
	it, err := scanReviewRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			cur := s.Get(id)
			if cur != nil && cur.Status == review.StatusInReview && cur.AssignedTo != actor {
				return nil, review.ErrNotAssignee
			}
			return nil, review.ErrNotPending
		}
		return nil, err
	}
	return it, nil
}

func (s *PGReviewStore) Escalate(id, actor, reason string) (*review.Item, error) {
	noteRow := review.Note{Actor: actor, Body: "ESCALATED: " + reason, CreatedAt: time.Now().UTC()}
	noteJSON, _ := json.Marshal(noteRow)
	// jsonb_insert append 到末尾
	const q = `
UPDATE risk_review
   SET status='escalated',
       escalate_level=escalate_level+1,
       assigned_to=NULL,
       assigned_at=NULL,
       notes=notes || $3::jsonb
 WHERE id=$1 AND status='in_review' AND assigned_to=$2
RETURNING ` + reviewCols
	row := s.pool.QueryRow(context.Background(), q, id, actor, "["+string(noteJSON)+"]")
	it, err := scanReviewRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			cur := s.Get(id)
			if cur != nil && cur.Status == review.StatusInReview && cur.AssignedTo != actor {
				return nil, review.ErrNotAssignee
			}
			return nil, review.ErrNotPending
		}
		return nil, err
	}
	return it, nil
}

func (s *PGReviewStore) AddNote(id, actor, body string) (*review.Item, error) {
	if actor == "" || body == "" {
		return nil, errors.New("review: actor + body required")
	}
	noteRow := review.Note{Actor: actor, Body: body, CreatedAt: time.Now().UTC()}
	noteJSON, _ := json.Marshal(noteRow)
	const q = `
UPDATE risk_review
   SET notes=notes || $2::jsonb
 WHERE id=$1
RETURNING ` + reviewCols
	row := s.pool.QueryRow(context.Background(), q, id, "["+string(noteJSON)+"]")
	it, err := scanReviewRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, review.ErrNotPending
		}
		return nil, err
	}
	return it, nil
}

// ── 查询 ────────────────────────────────────────────────────────────

func (s *PGReviewStore) List(status review.Status, limit, offset int) []*review.Item {
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var (
		rows pgx.Rows
		err  error
	)
	if status == "" {
		rows, err = s.pool.Query(context.Background(),
			`SELECT `+reviewCols+` FROM risk_review ORDER BY created_at DESC LIMIT $1 OFFSET $2`,
			limit, offset)
	} else {
		rows, err = s.pool.Query(context.Background(),
			`SELECT `+reviewCols+` FROM risk_review WHERE status=$1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
			string(status), limit, offset)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanRows(rows)
}

func (s *PGReviewStore) Get(id string) *review.Item {
	row := s.pool.QueryRow(context.Background(),
		`SELECT `+reviewCols+` FROM risk_review WHERE id=$1`, id)
	it, err := scanReviewRow(row)
	if err != nil {
		return nil
	}
	return it
}

func (s *PGReviewStore) CountByStatus(status review.Status) int {
	var n int
	var err error
	if status == "" {
		err = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM risk_review`).Scan(&n)
	} else {
		err = s.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM risk_review WHERE status=$1`, string(status)).Scan(&n)
	}
	if err != nil {
		return 0
	}
	return n
}

// OldestPendingAge 走 risk_review_status_created 索引，O(log N)。
// 队列空 → 返 0。错误（DB 抖动）也返 0，避免 metric 瞎抖；同时由 OutcomeLag /
// 健康检查路径独立反馈数据库问题。
func (s *PGReviewStore) OldestPendingAge(now time.Time) time.Duration {
	var createdAt time.Time
	err := s.pool.QueryRow(context.Background(),
		`SELECT created_at FROM risk_review
		  WHERE status = 'pending'
		  ORDER BY created_at ASC LIMIT 1`).Scan(&createdAt)
	if err != nil {
		return 0
	}
	if createdAt.IsZero() {
		return 0
	}
	d := now.Sub(createdAt)
	if d < 0 {
		return 0
	}
	return d
}

func (s *PGReviewStore) ListByAssignee(actor string, statuses []review.Status, limit int) []*review.Item {
	if limit <= 0 {
		limit = 100
	}
	args := []any{actor, limit}
	q := `SELECT ` + reviewCols + ` FROM risk_review WHERE assigned_to=$1`
	if len(statuses) > 0 {
		placeholders := make([]string, 0, len(statuses))
		for i, st := range statuses {
			placeholders = append(placeholders, "$"+itoa(3+i))
			args = append(args, string(st))
			_ = i
		}
		q += " AND status IN (" + strings.Join(placeholders, ",") + ")"
	}
	q += ` ORDER BY sla_deadline ASC LIMIT $2`
	rows, err := s.pool.Query(context.Background(), q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanRows(rows)
}

func (s *PGReviewStore) OverdueSLA(now time.Time, limit int) []*review.Item {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(context.Background(),
		`SELECT `+reviewCols+`
           FROM risk_review
          WHERE sla_deadline < $1
            AND status IN ('pending','in_review','escalated')
          ORDER BY sla_deadline ASC LIMIT $2`,
		now, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanRows(rows)
}

// ── helpers ────────────────────────────────────────────────────────

const reviewCols = `id, merchant_id, customer_id, payment_intent_id, amount, currency,
risk_score, reasons, status, sla_deadline, assigned_to, assigned_at, escalate_level,
notes, created_at, decided_at, decided_by, decide_reason`

type scannable interface {
	Scan(...any) error
}

func scanReviewRow(s scannable) (*review.Item, error) {
	var (
		it             review.Item
		reasonsJSON    []byte
		notesJSON      []byte
		statusStr      string
		assignedTo     *string
		assignedAt     *time.Time
		decidedAt      *time.Time
		decidedBy      *string
		decideReason   *string
	)
	if err := s.Scan(
		&it.ID, &it.MerchantID, &it.CustomerID, &it.PaymentIntentID,
		&it.Amount, &it.Currency, &it.RiskScore, &reasonsJSON,
		&statusStr, &it.SLADeadline,
		&assignedTo, &assignedAt, &it.EscalateLevel,
		&notesJSON, &it.CreatedAt,
		&decidedAt, &decidedBy, &decideReason,
	); err != nil {
		return nil, err
	}
	it.Status = review.Status(statusStr)
	_ = json.Unmarshal(reasonsJSON, &it.Reasons)
	_ = json.Unmarshal(notesJSON, &it.Notes)
	if assignedTo != nil {
		it.AssignedTo = *assignedTo
	}
	if assignedAt != nil {
		it.AssignedAt = assignedAt
	}
	if decidedAt != nil {
		it.DecidedAt = decidedAt
	}
	if decidedBy != nil {
		it.DecidedBy = *decidedBy
	}
	if decideReason != nil {
		it.DecideReason = *decideReason
	}
	return &it, nil
}

func scanRows(rows pgx.Rows) []*review.Item {
	var out []*review.Item
	for rows.Next() {
		it, err := scanReviewRow(rows)
		if err == nil {
			out = append(out, it)
		}
	}
	return out
}

// itoa 轻量 int→string，避免 strconv import 在 build 标签下不必要。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ── feedback.Recorder ────────────────────────────────────────────────

type PGFeedbackRecorder struct {
	pool *pgxpool.Pool
}

func NewPGFeedbackRecorder(pool *pgxpool.Pool) *PGFeedbackRecorder {
	return &PGFeedbackRecorder{pool: pool}
}

func (r *PGFeedbackRecorder) Record(o feedback.Outcome) error {
	if o.DecisionID == "" {
		return errors.New("feedback: decision_id required")
	}
	if o.Source == "" {
		return errors.New("feedback: source required")
	}
	if o.At.IsZero() {
		o.At = time.Now().UTC()
	}
	_, err := r.pool.Exec(context.Background(),
		`INSERT INTO risk_outcome (decision_id, source, is_fraud, at, actor, notes)
           VALUES ($1,$2,$3,$4,$5,$6)`,
		o.DecisionID, string(o.Source), o.IsFraud, o.At, o.Actor, o.Notes)
	return err
}

func (r *PGFeedbackRecorder) Get(decisionID string) []*feedback.Outcome {
	rows, err := r.pool.Query(context.Background(),
		`SELECT decision_id, source, is_fraud, at, actor, notes
           FROM risk_outcome WHERE decision_id=$1 ORDER BY at`,
		decisionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanOutcomes(rows)
}

func (r *PGFeedbackRecorder) Recent(limit int) []*feedback.Outcome {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(context.Background(),
		`SELECT decision_id, source, is_fraud, at, actor, notes
           FROM risk_outcome ORDER BY at DESC LIMIT $1`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanOutcomes(rows)
}

func scanOutcomes(rows pgx.Rows) []*feedback.Outcome {
	var out []*feedback.Outcome
	for rows.Next() {
		o := &feedback.Outcome{}
		var src string
		var actor, notes *string
		if err := rows.Scan(&o.DecisionID, &src, &o.IsFraud, &o.At, &actor, &notes); err != nil {
			continue
		}
		o.Source = feedback.Source(src)
		if actor != nil {
			o.Actor = *actor
		}
		if notes != nil {
			o.Notes = *notes
		}
		out = append(out, o)
	}
	return out
}
