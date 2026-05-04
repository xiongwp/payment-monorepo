package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xiongwp/order-core/internal/domain"
)

// fakeAccountingOutboxRepo in-memory 实现，用 request_id 去重。
type fakeAccountingOutboxRepo struct {
	rows []*domain.AccountingOutbox
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
func (f *fakeAccountingOutboxRepo) MarkSent(_ context.Context, row *domain.AccountingOutbox) error {
	for _, existing := range f.rows {
		if existing.ID == row.ID {
			existing.Status = domain.AccountingOutboxSent
			return nil
		}
	}
	return nil
}
func (f *fakeAccountingOutboxRepo) MarkRetry(context.Context, *domain.AccountingOutbox, time.Time, string) error {
	return nil
}
func (f *fakeAccountingOutboxRepo) MarkFailed(context.Context, *domain.AccountingOutbox, string) error {
	return nil
}
func (f *fakeAccountingOutboxRepo) PurgeSentBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}
func (f *fakeAccountingOutboxRepo) CountDeadLetters(context.Context) (int64, error) { return 0, nil }

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
