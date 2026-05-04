package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/domain"
)

func newPendingQueryWorkerForTest(t *testing.T, ad *fakeAdapter, repo *memTxRepo) *PendingQueryWorker {
	t.Helper()
	reg := newFakeRegistry(ad)
	w := NewPendingQueryWorker(reg, repo,
		1*time.Second, /* interval, irrelevant for Tick test */
		0, 0,          /* olderThan + queryThrottle: memTxRepo ignores */
		100, 30,
		zap.NewNop())
	return w
}

func TestPendingQueryWorker_Promotes_UnknownToSucceeded(t *testing.T) {
	ad := newFakeAdapter("gcash")
	svc, repo := newAcquirerSvcForTest(t, ad)
	// 让 Charge 报错制造一行 unknown
	ad.nextChargeErr = errors.New("upstream timeout")
	_, _ = svc.Charge(context.Background(), "gcash", &channel.ChargeRequest{
		PiID: "pi_q1", IdempotencyKey: "ikq1", Amount: 100, Currency: "PHP",
	})

	// adapter Query 下次返成功 + 真实 ext_ref_no
	ad.nextQueryResp = &channel.QueryResponse{
		Result:        channel.ResultSucceeded,
		ExternalRefNo: "ref_q1",
	}

	w := newPendingQueryWorkerForTest(t, ad, repo)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	rows := repo.rowsByAdapter("gcash")
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].State != domain.AcquirerTxSucceeded {
		t.Fatalf("want state=succeeded after Query promotes, got %s", rows[0].State)
	}
	if rows[0].ExternalRefNo != "ref_q1" {
		t.Fatalf("want ext_ref_no backfilled by Query, got %q", rows[0].ExternalRefNo)
	}
	if rows[0].QueryCount != 1 {
		t.Fatalf("want query_count=1, got %d", rows[0].QueryCount)
	}
	if rows[0].LastQueryAt == nil {
		t.Fatal("want last_query_at set")
	}
}

func TestPendingQueryWorker_Demotes_UnknownToFailed(t *testing.T) {
	ad := newFakeAdapter("gcash")
	svc, repo := newAcquirerSvcForTest(t, ad)
	ad.nextChargeErr = errors.New("upstream 502")
	_, _ = svc.Charge(context.Background(), "gcash", &channel.ChargeRequest{
		PiID: "pi_q2", IdempotencyKey: "ikq2", Amount: 100, Currency: "PHP",
	})
	// Query 明示 failed —— 渠道侧实际没扣成功
	ad.nextQueryResp = &channel.QueryResponse{Result: channel.ResultFailed}

	w := newPendingQueryWorkerForTest(t, ad, repo)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	rows := repo.rowsByAdapter("gcash")
	if len(rows) != 1 || rows[0].State != domain.AcquirerTxFailed {
		t.Fatalf("want state=failed after Query says failed, got %+v", rows)
	}
}

func TestPendingQueryWorker_StaysUnknown_WhenQueryProcessing(t *testing.T) {
	ad := newFakeAdapter("gcash")
	svc, repo := newAcquirerSvcForTest(t, ad)
	ad.nextChargeErr = errors.New("timeout")
	_, _ = svc.Charge(context.Background(), "gcash", &channel.ChargeRequest{
		PiID: "pi_q3", IdempotencyKey: "ikq3", Amount: 100, Currency: "PHP",
	})
	ad.nextQueryResp = &channel.QueryResponse{Result: channel.ResultProcessing}

	w := newPendingQueryWorkerForTest(t, ad, repo)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	rows := repo.rowsByAdapter("gcash")
	if rows[0].State != domain.AcquirerTxUnknown {
		t.Fatalf("processing → still unknown, got %s", rows[0].State)
	}
	if rows[0].QueryCount != 1 {
		t.Fatalf("query_count should bump to 1 even on processing, got %d", rows[0].QueryCount)
	}
}

func TestPendingQueryWorker_StaysUnknown_WhenQueryItselfErrors(t *testing.T) {
	// 用一个 adapter Query 总是失败的实现
	ad := &fakeAdapter{name: "gcash", webhookSigOK: true}
	svc, repo := newAcquirerSvcForTest(t, ad)
	ad.nextChargeErr = errors.New("upstream timeout")
	_, _ = svc.Charge(context.Background(), "gcash", &channel.ChargeRequest{
		PiID: "pi_q4", IdempotencyKey: "ikq4", Amount: 100, Currency: "PHP",
	})

	// 用一个会注入 Query 错误的包装：直接覆盖 nextQueryResp 为 nil 也不能让 Query 返错；
	// fakeAdapter.Query 没有 nextQueryErr 字段。换法：用一个 stub adapter 包一层。
	w := NewPendingQueryWorker(newFakeRegistry(&queryErrAdapter{name: "gcash"}), repo,
		1*time.Second, 0, 0, 100, 30, zap.NewNop())
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	rows := repo.rowsByAdapter("gcash")
	if rows[0].State != domain.AcquirerTxUnknown {
		t.Fatalf("Query err → still unknown, got %s", rows[0].State)
	}
	if rows[0].QueryCount != 1 {
		t.Fatalf("query_count should bump on Query err, got %d", rows[0].QueryCount)
	}
}

// queryErrAdapter Query 总报错；其余方法 panic（测试不会用到）。
type queryErrAdapter struct{ name string }

func (a *queryErrAdapter) Name() string { return a.name }
func (a *queryErrAdapter) Charge(_ context.Context, _ *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	panic("not used")
}
func (a *queryErrAdapter) Capture(_ context.Context, _ *channel.CaptureRequest) (*channel.OpResponse, error) {
	panic("not used")
}
func (a *queryErrAdapter) Void(_ context.Context, _ *channel.VoidRequest) (*channel.OpResponse, error) {
	panic("not used")
}
func (a *queryErrAdapter) Refund(_ context.Context, _ *channel.RefundRequest) (*channel.OpResponse, error) {
	panic("not used")
}
func (a *queryErrAdapter) Query(_ context.Context, _ *channel.QueryRequest) (*channel.QueryResponse, error) {
	return nil, errors.New("query upstream down")
}
func (a *queryErrAdapter) ParseWebhook(_ map[string]string, _ []byte) (*channel.WebhookEvent, error) {
	panic("not used")
}
