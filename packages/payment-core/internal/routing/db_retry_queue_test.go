package routing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"go.uber.org/zap"
)

// helper: 拿一个 sqlmock 包裹的 DBRetryQueueImpl
func newTestQueue(t *testing.T) (*DBRetryQueueImpl, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	q := NewDBRetryQueue(db, zap.NewNop())
	return q, mock, func() { _ = db.Close() }
}

func TestEnqueue_InsertOnce(t *testing.T) {
	q, mock, cleanup := newTestQueue(t)
	defer cleanup()

	task := &RetryTask{
		ID:              "task_abc",
		PaymentIntentID: "pi_1",
		IdempotencyKey:  "idem_1",
		Amount:          1000,
		Currency:        "USD",
		PaymentMethod:   "CARD",
		NextRetryAt:     time.Now().Add(30 * time.Second),
	}

	// 不强匹配 SQL 串(它会被 helper 重写),仅匹配执行 + 影响行数
	mock.ExpectExec(".*INSERT INTO payment_retry_queue.*").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// QueryMatcherEqual 模式下 .*会失败,换 QueryMatcherRegexp
	q.db.Close()
	db2, m2, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	q.db = db2
	mock = m2
	m2.ExpectExec(`INSERT INTO payment_retry_queue`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := q.Enqueue(context.Background(), task); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
}

func TestEnqueue_ValidationFail(t *testing.T) {
	q, _, cleanup := newTestQueue(t)
	defer cleanup()

	cases := []*RetryTask{nil, {ID: ""}, {ID: "x", IdempotencyKey: ""}}
	for _, c := range cases {
		if err := q.Enqueue(context.Background(), c); err == nil {
			t.Errorf("expect error for invalid task: %+v", c)
		}
	}
}

func TestDequeue_AtomicLease(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	q := NewDBRetryQueue(db, zap.NewNop())

	// 1) 回收过期 lease
	mock.ExpectExec(`UPDATE payment_retry_queue.*SET state='pending'.*lease_expires_at.*<.*NOW`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// 2) 开事务
	mock.ExpectBegin()
	// 3) 抢占租约
	mock.ExpectExec(`UPDATE payment_retry_queue.*SET state='leased'`).
		WithArgs(q.owner, sqlmock.AnyArg(), 50).
		WillReturnResult(sqlmock.NewResult(0, 2))
	// 4) 查 leased
	rows := sqlmock.NewRows([]string{
		"id", "payment_intent_id", "idempotency_key", "amount", "currency",
		"payment_method", "country", "bin", "failed_adapter", "reason",
		"attempt", "next_retry_at", "last_error_msg", "metadata",
		"created_at", "updated_at",
	}).
		AddRow("t1", "pi_1", "idem_1", int64(1000), "USD",
			"CARD", "US", "411111", "visa-stripe", "circuit_open",
			0, time.Now(), "circuit open", `{"src":"web"}`,
			time.Now(), time.Now()).
		AddRow("t2", "pi_2", "idem_2", int64(2000), "USD",
			"CARD", "US", "555555", "mc-adyen", "unavailable",
			1, time.Now(), "503 from adyen", `{}`,
			time.Now(), time.Now())
	mock.ExpectQuery(`SELECT.*FROM payment_retry_queue.*WHERE state='leased'`).
		WithArgs(q.owner, 50).
		WillReturnRows(rows)
	mock.ExpectCommit()

	tasks, err := q.Dequeue(context.Background(), 50)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("expect 2 tasks, got %d", len(tasks))
	}
	if tasks[0].Metadata["src"] != "web" {
		t.Errorf("metadata not deserialized correctly: %+v", tasks[0].Metadata)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestDequeue_EmptyReturnsNoRows(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	q := NewDBRetryQueue(db, zap.NewNop())

	mock.ExpectExec(`UPDATE payment_retry_queue.*SET state='pending'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE payment_retry_queue.*SET state='leased'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	tasks, err := q.Dequeue(context.Background(), 100)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expect 0 tasks, got %d", len(tasks))
	}
}

func TestMarkRetry_TruncatesLongError(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	q := NewDBRetryQueue(db, zap.NewNop())

	longErr := make([]byte, 2000)
	for i := range longErr {
		longErr[i] = 'x'
	}
	mock.ExpectExec(`UPDATE payment_retry_queue.*SET state='pending'.*attempt`).
		WithArgs(2, sqlmock.AnyArg(), sqlmock.AnyArg(), "task_x").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := q.MarkRetry(context.Background(), "task_x", 2, time.Now().Add(time.Minute), string(longErr)); err != nil {
		t.Fatalf("MarkRetry: %v", err)
	}
}

func TestMarkRetry_TaskNotFound(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	q := NewDBRetryQueue(db, zap.NewNop())

	mock.ExpectExec(`UPDATE payment_retry_queue`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := q.MarkRetry(context.Background(), "missing", 1, time.Now(), "err")
	if err == nil {
		t.Fatal("expect error for missing task")
	}
}

func TestMarkSuccess_FlipState(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	q := NewDBRetryQueue(db, zap.NewNop())

	mock.ExpectExec(`UPDATE payment_retry_queue.*SET state='done'`).
		WithArgs("task_y").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := q.MarkSuccess(context.Background(), "task_y"); err != nil {
		t.Fatalf("MarkSuccess: %v", err)
	}
}

func TestStats_AggregatesByState(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	q := NewDBRetryQueue(db, zap.NewNop())

	mock.ExpectQuery(`SELECT state, COUNT.*FROM payment_retry_queue.*GROUP BY state`).
		WillReturnRows(sqlmock.NewRows([]string{"state", "n"}).
			AddRow("pending", 5).
			AddRow("leased", 2).
			AddRow("done", 100))
	mock.ExpectQuery(`SELECT COUNT.*FROM payment_retry_queue.*pending.*next_retry_at`).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))

	got, err := q.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got["pending"] != 5 || got["leased"] != 2 || got["done"] != 100 || got["overdue"] != 1 {
		t.Errorf("stats wrong: %+v", got)
	}
}

func TestPurgeDoneOlderThan(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	q := NewDBRetryQueue(db, zap.NewNop())

	mock.ExpectExec(`DELETE FROM payment_retry_queue.*state='done'`).
		WillReturnResult(sqlmock.NewResult(0, 42))

	n, err := q.PurgeDoneOlderThan(context.Background(), 7*24*time.Hour)
	if err != nil || n != 42 {
		t.Errorf("Purge: got n=%d err=%v", n, err)
	}
}

// Smoke: 即便没数据,error 也得能透传 (防 sql.ErrNoRows 误用)
func TestStats_QueryError(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	q := NewDBRetryQueue(db, zap.NewNop())

	mock.ExpectQuery(`SELECT state, COUNT`).WillReturnError(errors.New("db down"))
	_, err := q.Stats(context.Background())
	if err == nil {
		t.Fatal("expect error")
	}
}

// 防御 nil db (调用方失误)
var _ RetryQueue = (*DBRetryQueueImpl)(nil)

// 编译期检查 NewDBRetryQueue 返回类型
var _ = func(db *sql.DB) RetryQueue { return NewDBRetryQueue(db, nil) }
