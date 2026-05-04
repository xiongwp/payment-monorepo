package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/xiongwp/order-core/internal/domain"
)

// fakeAccountingOutboxRepo in-memory 实现，用 request_id 去重。
//
// 模拟 ClaimBatch 语义：把全部 pending 行染上同一 token，next_attempt_at 推到远未来。
// 用于覆盖 worker 单测；真实并发安全靠 SQL 行锁，单测不验证锁本身。
type fakeAccountingOutboxRepo struct {
	rows  []*domain.AccountingOutbox
	clock int64 // 自增 token 序号
}

func (f *fakeAccountingOutboxRepo) Insert(_ context.Context, row *domain.AccountingOutbox) (*domain.AccountingOutbox, error) {
	for _, existing := range f.rows {
		if existing.RequestID == row.RequestID {
			return existing, domain.ErrAccountingOutboxDuplicate
		}
	}
	cp := *row
	f.rows = append(f.rows, &cp)
	return &cp, nil
}
func (f *fakeAccountingOutboxRepo) ListPending(context.Context, time.Time, int) ([]*domain.AccountingOutbox, error) {
	return f.rows, nil
}
func (f *fakeAccountingOutboxRepo) ClaimBatch(_ context.Context, _ time.Time, perTableLimit int, _ time.Duration) (string, int, error) {
	f.clock++
	tok := fakeClaimToken(f.clock)
	claimed := 0
	for _, r := range f.rows {
		if r.Status != domain.AccountingOutboxPending || r.ClaimToken != "" {
			continue
		}
		r.ClaimToken = tok
		claimed++
		if perTableLimit > 0 && claimed >= perTableLimit*100 { // 模拟 100 张表上限
			break
		}
	}
	return tok, claimed, nil
}
func (f *fakeAccountingOutboxRepo) ListByClaimToken(_ context.Context, tok string) ([]*domain.AccountingOutbox, error) {
	if tok == "" {
		return nil, nil
	}
	var out []*domain.AccountingOutbox
	for _, r := range f.rows {
		if r.ClaimToken == tok {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeAccountingOutboxRepo) MarkSent(_ context.Context, row *domain.AccountingOutbox) error {
	for _, existing := range f.rows {
		if existing.ID == row.ID && (row.ClaimToken == "" || existing.ClaimToken == row.ClaimToken) {
			existing.Status = domain.AccountingOutboxSent
			existing.ClaimToken = ""
			return nil
		}
	}
	return nil
}
func (f *fakeAccountingOutboxRepo) MarkRetry(_ context.Context, row *domain.AccountingOutbox, _ time.Time, _ string) error {
	for _, existing := range f.rows {
		if existing.ID == row.ID && (row.ClaimToken == "" || existing.ClaimToken == row.ClaimToken) {
			existing.ClaimToken = ""
			existing.Attempts++
			return nil
		}
	}
	return nil
}
func (f *fakeAccountingOutboxRepo) MarkFailed(_ context.Context, row *domain.AccountingOutbox, _ string) error {
	for _, existing := range f.rows {
		if existing.ID == row.ID && (row.ClaimToken == "" || existing.ClaimToken == row.ClaimToken) {
			existing.Status = domain.AccountingOutboxFailed
			existing.ClaimToken = ""
			return nil
		}
	}
	return nil
}
func (f *fakeAccountingOutboxRepo) PurgeSentBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}
func (f *fakeAccountingOutboxRepo) CountDeadLetters(context.Context) (int64, error) { return 0, nil }

func fakeClaimToken(seq int64) string {
	return fmt.Sprintf("test-claim-%d", seq)
}

type fakeIDGen struct{ next int64 }

func (f *fakeIDGen) NextID(context.Context, string) (int64, error) { f.next++; return f.next, nil }
func (f *fakeIDGen) Register(context.Context, string, int64, int, string) error {
	return nil
}
func (f *fakeIDGen) Preload(context.Context) error { return nil }

func TestAccountingOutboxService_EnqueueUserCharge(t *testing.T) {
	repo := &fakeAccountingOutboxRepo{}
	svc := NewAccountingOutboxService(repo, &fakeIDGen{}, nil)
	pi := &domain.PaymentIntent{
		ID:            "pi_115abc",
		CustomerID:    "cus_42",
		MchID:         "mch_1",
		Amount:        10000,
		Currency:      "PHP",
		PaymentMethod: "gcash",
	}
	if err := svc.EnqueueChargeSucceeded(context.Background(), pi, "ch_7", 10000); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(repo.rows))
	}
	row := repo.rows[0]
	if row.OwnerType != domain.AccountingOwnerUser || row.OwnerID != "cus_42" {
		t.Errorf("owner = %s/%s", row.OwnerType, row.OwnerID)
	}
	if row.PaymentMethod != "gcash" {
		t.Errorf("payment_method = %s", row.PaymentMethod)
	}
	if row.RequestID != "pi_115abc:charge_succeeded:ch_7" {
		t.Errorf("request_id = %s", row.RequestID)
	}
}

func TestAccountingOutboxService_EnqueueMerchantCharge(t *testing.T) {
	// CustomerID empty, MchID set → routes to merchant path.
	repo := &fakeAccountingOutboxRepo{}
	svc := NewAccountingOutboxService(repo, &fakeIDGen{}, nil)
	pi := &domain.PaymentIntent{
		ID:            "pi_200b",
		MchID:         "mch_42",
		Currency:      "PHP",
		PaymentMethod: "shopeepay",
	}
	if err := svc.EnqueueChargeSucceeded(context.Background(), pi, "ch_1", 2000); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(repo.rows))
	}
	row := repo.rows[0]
	if row.OwnerType != domain.AccountingOwnerMerchant || row.OwnerID != "mch_42" {
		t.Errorf("owner = %s/%s", row.OwnerType, row.OwnerID)
	}
}

func TestAccountingOutboxService_Idempotent(t *testing.T) {
	repo := &fakeAccountingOutboxRepo{}
	svc := NewAccountingOutboxService(repo, &fakeIDGen{}, nil)
	pi := &domain.PaymentIntent{ID: "pi_100a", CustomerID: "cus_1", Currency: "PHP", PaymentMethod: "gcash"}
	for i := 0; i < 3; i++ {
		if err := svc.EnqueueChargeSucceeded(context.Background(), pi, "ch_x", 500); err != nil {
			t.Fatalf("enqueue #%d: %v", i, err)
		}
	}
	if len(repo.rows) != 1 {
		t.Fatalf("row count = %d, want 1 (idempotent)", len(repo.rows))
	}
}

func TestAccountingOutboxService_SkipNoOwner(t *testing.T) {
	repo := &fakeAccountingOutboxRepo{}
	svc := NewAccountingOutboxService(repo, &fakeIDGen{}, nil)
	pi := &domain.PaymentIntent{ID: "pi_200b", Currency: "PHP"} // no Customer, no Mch
	if err := svc.EnqueueChargeSucceeded(context.Background(), pi, "ch_1", 100); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if len(repo.rows) != 0 {
		t.Fatalf("want 0 rows, got %d", len(repo.rows))
	}
}

type fakeAccountingClient struct {
	err  error
	seen []string
}

func (f *fakeAccountingClient) DoubleEntryBooking(_ context.Context, row *domain.AccountingOutbox) error {
	f.seen = append(f.seen, row.RequestID)
	return f.err
}

func TestAccountingOutboxService_InlineDeliverySuccessMarksSent(t *testing.T) {
	repo := &fakeAccountingOutboxRepo{}
	svc := NewAccountingOutboxService(repo, &fakeIDGen{}, nil).(*accountingOutboxService)
	client := &fakeAccountingClient{}
	svc.SetInlineClient(client)

	pi := &domain.PaymentIntent{ID: "pi_inline_ok", Currency: "PHP", CustomerID: "100000001", MchID: "m1"}
	if err := svc.EnqueueChargeSucceeded(context.Background(), pi, "ch_1", 10000); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if len(client.seen) != 1 || client.seen[0] != "pi_inline_ok:charge_succeeded:ch_1" {
		t.Fatalf("want inline client called once with request_id, got %v", client.seen)
	}
	if len(repo.rows) != 1 || repo.rows[0].Status != domain.AccountingOutboxSent {
		t.Fatalf("want row marked sent, got %+v", repo.rows)
	}
}

func TestAccountingOutboxService_InlineDeliveryFailureLeavesPending(t *testing.T) {
	repo := &fakeAccountingOutboxRepo{}
	svc := NewAccountingOutboxService(repo, &fakeIDGen{}, nil).(*accountingOutboxService)
	client := &fakeAccountingClient{err: errors.New("accounting-system down")}
	svc.SetInlineClient(client)

	pi := &domain.PaymentIntent{ID: "pi_inline_fail", Currency: "PHP", CustomerID: "100000001", MchID: "m1"}
	if err := svc.EnqueueChargeSucceeded(context.Background(), pi, "ch_1", 10000); err != nil {
		t.Fatalf("enqueue: %v (inline failure must not surface to caller)", err)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("want 1 pending row, got %d", len(repo.rows))
	}
	if repo.rows[0].Status == domain.AccountingOutboxSent {
		t.Fatalf("row must stay non-sent so worker retries; got status=%s", repo.rows[0].Status)
	}
}

func TestAccountingOutboxWorker_TickSkipsWhenClientNil(t *testing.T) {
	repo := &fakeAccountingOutboxRepo{rows: []*domain.AccountingOutbox{
		{ID: "aob_1", RequestID: "r1", PaymentIntentID: "pi_1", Status: domain.AccountingOutboxPending},
	}}
	w := NewAccountingOutboxWorker(repo, nil, nil, AccountingOutboxWorkerConfig{})
	w.Tick(context.Background())
	// stays pending
	if len(repo.rows) != 1 {
		t.Fatalf("rows changed")
	}
}

func TestAccountingOutboxWorker_DeliversOnTick(t *testing.T) {
	repo := &fakeAccountingOutboxRepo{rows: []*domain.AccountingOutbox{
		{ID: "aob_1", RequestID: "r1", PaymentIntentID: "pi_1", Status: domain.AccountingOutboxPending},
	}}
	cli := &fakeAccountingClient{}
	w := NewAccountingOutboxWorker(repo, cli, nil, AccountingOutboxWorkerConfig{})
	w.Tick(context.Background())
	if len(cli.seen) != 1 || cli.seen[0] != "r1" {
		t.Fatalf("client call = %v", cli.seen)
	}
}

func TestAccountingOutboxWorker_RetriesOnError(t *testing.T) {
	repo := &fakeAccountingOutboxRepo{rows: []*domain.AccountingOutbox{
		{ID: "aob_1", RequestID: "r1", PaymentIntentID: "pi_1", Status: domain.AccountingOutboxPending, Attempts: 0},
	}}
	cli := &fakeAccountingClient{err: errors.New("temporary failure")}
	w := NewAccountingOutboxWorker(repo, cli, nil, AccountingOutboxWorkerConfig{MaxAttempts: 5})
	w.Tick(context.Background())
	if len(cli.seen) != 1 {
		t.Fatalf("client calls = %d", len(cli.seen))
	}
}

// 同一 batch 不会被同一 worker 重复处理：第二次 Tick 已无 claim_token=NULL 的行。
// 模拟两个 pod 顺序进入 Tick——第二个看到的是第一个 claim 走的行（next_attempt_at 在未来 + claim_token 已写）。
func TestAccountingOutboxWorker_SecondTickDoesNotReclaim(t *testing.T) {
	repo := &fakeAccountingOutboxRepo{rows: []*domain.AccountingOutbox{
		{ID: "aob_1", RequestID: "r1", PaymentIntentID: "pi_1", Status: domain.AccountingOutboxPending},
		{ID: "aob_2", RequestID: "r2", PaymentIntentID: "pi_2", Status: domain.AccountingOutboxPending},
	}}
	cli := &fakeAccountingClient{}
	w := NewAccountingOutboxWorker(repo, cli, nil, AccountingOutboxWorkerConfig{})
	first := w.Tick(context.Background())
	if first != 2 {
		t.Fatalf("first tick claimed = %d, want 2", first)
	}
	if len(cli.seen) != 2 {
		t.Fatalf("client calls after first tick = %d", len(cli.seen))
	}

	// 第二次 Tick：两行已在第一次内被 process 标 sent + 释放 token。再 Tick 应 claim 0 行。
	second := w.Tick(context.Background())
	if second != 0 {
		t.Fatalf("second tick claimed = %d, want 0 (rows already sent)", second)
	}
}

// MarkSent 仅在 ClaimToken 仍持有时生效：模拟 worker A claim 了行，但 worker B 在 lease 过期后接管，
// A 完成后再尝试 MarkSent 应被 fake 实现拒绝（行的 ClaimToken 已变成 B 的 token）。
// 注意：fake 实现里 MarkSent 比对 token 不一致时静默 return；这是和真实 SQL 路径不同的简化，
// 真实路径返回 "claim lost" error。这里测的是 MarkSent 不会破坏 B 的状态。
func TestAccountingOutboxRepo_MarkSentRespectsClaimToken(t *testing.T) {
	repo := &fakeAccountingOutboxRepo{rows: []*domain.AccountingOutbox{
		{ID: "aob_x", RequestID: "rx", PaymentIntentID: "pi_x", Status: domain.AccountingOutboxPending},
	}}
	// A claim
	tokA, _, _ := repo.ClaimBatch(context.Background(), time.Now(), 10, 0)
	// 模拟 lease 过期 → B 抢回。fake 不真模拟时间，但我们手动清空 token 再让 B claim
	repo.rows[0].ClaimToken = ""
	tokB, _, _ := repo.ClaimBatch(context.Background(), time.Now(), 10, 0)
	if tokA == tokB {
		t.Fatalf("tokens collided: %s", tokA)
	}
	// A 现在试图 MarkSent（持有过期 tokA）
	rowA := *repo.rows[0]
	rowA.ClaimToken = tokA
	_ = repo.MarkSent(context.Background(), &rowA)
	// 真实 row 仍属于 B，状态不应被 A 改成 sent
	if repo.rows[0].Status == domain.AccountingOutboxSent {
		t.Fatalf("row status was overwritten by stale claim holder")
	}
	if repo.rows[0].ClaimToken != tokB {
		t.Fatalf("row claim_token changed to %s, want %s", repo.rows[0].ClaimToken, tokB)
	}
}
