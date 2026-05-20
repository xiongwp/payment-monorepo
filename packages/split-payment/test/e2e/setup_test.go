// Package e2e holds end-to-end integration tests covering the split-payment
// engine across MySQL repo + accounting stub + kafka stub.
//
// Run:
//
//	SPLIT_PAYMENT_E2E=1 \
//	SPLIT_PAYMENT_TEST_DSN='root:testpass@tcp(127.0.0.1:3306)/split_payment_test?parseTime=true' \
//	go test ./test/e2e/... -count=1 -tags=e2e
//
// CI brings up a sidecar MySQL via .github/workflows/ci.yml and exports
// SPLIT_PAYMENT_TEST_DSN.
//
// Skips: tests skip when SPLIT_PAYMENT_E2E env not set so unit-test runs
// stay hermetic.
//
//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/split-payment/internal/domain"
	"github.com/xiongwp/split-payment/internal/repo"
	"github.com/xiongwp/split-payment/internal/workflow"

	_ "github.com/go-sql-driver/mysql"
)

// e2eEnv 共享 fixture — 启动期跑一次, 多个测试复用.
type e2eEnv struct {
	db       *sql.DB
	engine   *workflow.Engine
	graphs   *repo.MySQLGraphRepo
	runs     *repo.MySQLRunRepo
	acct     *stubAccounting
	events   *captureEventPublisher
	log      *zap.Logger
	cleanups []func()
}

var (
	envOnce sync.Once
	envInst *e2eEnv
	envErr  error
)

// loadEnv 启 MySQL + 建 engine + stub clients. 第一次调用初始化, 之后返同实例.
func loadEnv(t *testing.T) *e2eEnv {
	t.Helper()
	if os.Getenv("SPLIT_PAYMENT_E2E") == "" {
		t.Skip("set SPLIT_PAYMENT_E2E=1 to run e2e tests")
	}
	envOnce.Do(func() {
		envInst, envErr = buildEnv()
	})
	if envErr != nil {
		t.Fatalf("e2e env init failed: %v", envErr)
	}
	return envInst
}

func buildEnv() (*e2eEnv, error) {
	dsn := os.Getenv("SPLIT_PAYMENT_TEST_DSN")
	if dsn == "" {
		dsn = "root:testpass@tcp(127.0.0.1:3306)/split_payment_test?parseTime=true&charset=utf8mb4"
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)

	// retry ping — 给 sidecar MySQL up 时间
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		if err := db.PingContext(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	// 建表
	ensureCtx, ensureCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ensureCancel()
	if err := repo.EnsureSchema(ensureCtx, db); err != nil {
		return nil, err
	}

	// truncate so test 之间没有 state 漏
	for _, tbl := range []string{"moneyflow_runs", "moneyflow_graphs"} {
		if _, err := db.ExecContext(ensureCtx, "TRUNCATE TABLE "+tbl); err != nil {
			return nil, err
		}
	}

	log, _ := zap.NewDevelopment()
	graphs := repo.NewMySQLGraphRepo(db)
	runs := repo.NewMySQLRunRepo(db)
	acct := &stubAccounting{}
	events := &captureEventPublisher{}
	engine := &workflow.Engine{
		GraphRepo:      graphs,
		RunRepo:        runs,
		AccountingMeta: acct,
		Events:         events,
		Audit:          logAudit{log: log},
		Log:            log,
	}

	return &e2eEnv{
		db: db, engine: engine, graphs: graphs, runs: runs,
		acct: acct, events: events, log: log,
		cleanups: []func(){
			func() { db.Close() },
		},
	}, nil
}

// stubAccounting — 测试用 AccountingMetaCaller, 回 voucher + 记录所有 req.
type stubAccounting struct {
	mu      sync.Mutex
	calls   []*domain.TransactionRequest
	respErr error
	respFn  func(req *domain.TransactionRequest) (*workflow.AccountingTxResp, error)
}

func (s *stubAccounting) CreateTransaction(_ context.Context, req *domain.TransactionRequest) (*workflow.AccountingTxResp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.respFn != nil {
		return s.respFn(req)
	}
	if s.respErr != nil {
		return nil, s.respErr
	}
	return &workflow.AccountingTxResp{
		VoucherNo: "vch-" + req.OrderNo,
		Status:    2, // success
	}, nil
}

func (s *stubAccounting) Calls() []*domain.TransactionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*domain.TransactionRequest, len(s.calls))
	copy(out, s.calls)
	return out
}

func (s *stubAccounting) Reset() {
	s.mu.Lock()
	s.calls = nil
	s.respErr = nil
	s.respFn = nil
	s.mu.Unlock()
}

// captureEventPublisher 收 engine 发出的事件用于 assert.
type captureEventPublisher struct {
	mu     sync.Mutex
	events []capturedEvent
}

type capturedEvent struct {
	Event string
	Plan  *domain.RunPlan
}

func (c *captureEventPublisher) Publish(_ context.Context, ev string, p *domain.RunPlan) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, capturedEvent{Event: ev, Plan: p})
	return nil
}

func (c *captureEventPublisher) Events() []capturedEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedEvent, len(c.events))
	copy(out, c.events)
	return out
}

func (c *captureEventPublisher) Reset() {
	c.mu.Lock()
	c.events = nil
	c.mu.Unlock()
}

type logAudit struct{ log *zap.Logger }

func (l logAudit) Write(_ context.Context, ev map[string]any) error {
	l.log.Sugar().Infow("AUDIT", "event", ev)
	return nil
}
