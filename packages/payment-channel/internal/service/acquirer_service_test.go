package service

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/domain"
)

func newAcquirerSvcForTest(t *testing.T, ad *fakeAdapter) (*AcquirerService, *memTxRepo) {
	t.Helper()
	reg := newFakeRegistry(ad)
	txRepo := newMemTxRepo()
	svc := NewAcquirerService(reg, txRepo, &memIDIssuer{}, zap.NewNop())
	return svc, txRepo
}

func TestAcquirer_Charge_HappyPath(t *testing.T) {
	ad := newFakeAdapter("gcash")
	svc, txRepo := newAcquirerSvcForTest(t, ad)

	resp, err := svc.Charge(context.Background(), "gcash", &channel.ChargeRequest{
		PiID:           "pi_1",
		IdempotencyKey: "idem-1",
		Amount:         10000,
		Currency:       "PHP",
	})
	if err != nil {
		t.Fatalf("charge err: %v", err)
	}
	if resp.Result != channel.ResultSucceeded {
		t.Fatalf("want succeeded got %s", resp.Result)
	}
	if resp.ExternalRefNo != "ref_pi_1" {
		t.Fatalf("bad ref: %s", resp.ExternalRefNo)
	}
	if charges, _, _, _, _ := ad.counts(); charges != 1 {
		t.Fatalf("want 1 charge got %d", charges)
	}
	rows := txRepo.rowsByAdapter("gcash")
	if len(rows) != 1 {
		t.Fatalf("want 1 tx row got %d", len(rows))
	}
	if rows[0].State != domain.AcquirerTxSucceeded {
		t.Fatalf("want succeeded got %s", rows[0].State)
	}
	if rows[0].ExternalRefNo != "ref_pi_1" {
		t.Fatalf("tx external_ref not persisted: %s", rows[0].ExternalRefNo)
	}
}

func TestAcquirer_Charge_IdempotentReplay(t *testing.T) {
	ad := newFakeAdapter("gcash")
	svc, txRepo := newAcquirerSvcForTest(t, ad)

	req := &channel.ChargeRequest{PiID: "pi_1", IdempotencyKey: "idem-1", Amount: 10000, Currency: "PHP"}
	if _, err := svc.Charge(context.Background(), "gcash", req); err != nil {
		t.Fatal(err)
	}
	// 第二次同 idem，应该不再调 adapter，直接回放首次响应
	resp2, err := svc.Charge(context.Background(), "gcash", req)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.ExternalRefNo != "ref_pi_1" {
		t.Fatalf("replay ref mismatch: %s", resp2.ExternalRefNo)
	}
	if charges, _, _, _, _ := ad.counts(); charges != 1 {
		t.Fatalf("want exactly 1 adapter call (replay), got %d", charges)
	}
	if len(txRepo.rowsByAdapter("gcash")) != 1 {
		t.Fatal("expected only 1 tx row after replay")
	}
}

func TestAcquirer_Charge_RequiresAction_StillSucceededRow(t *testing.T) {
	ad := newFakeAdapter("maya")
	ad.nextChargeResp = &channel.ChargeResponse{
		Result:        channel.ResultRequiresAction,
		ExternalRefNo: "co_1",
		RequiredAction: &channel.RequiredAction{
			Type:        "app_redirect",
			RedirectURL: "https://paymaya/checkout?id=co_1",
		},
	}
	svc, txRepo := newAcquirerSvcForTest(t, ad)

	resp, err := svc.Charge(context.Background(), "maya", &channel.ChargeRequest{
		PiID: "pi_2", IdempotencyKey: "k2", Amount: 5000, Currency: "PHP",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result != channel.ResultRequiresAction {
		t.Fatalf("want requires_action got %s", resp.Result)
	}
	if resp.RequiredAction == nil || resp.RequiredAction.RedirectURL == "" {
		t.Fatal("required action should carry redirect_url")
	}
	rows := txRepo.rowsByAdapter("maya")
	// RequiresAction 视为"调用完成"，行状态为 succeeded（代表"HTTP 调用成功"）
	if rows[0].State != domain.AcquirerTxSucceeded {
		t.Fatalf("tx row state want succeeded got %s", rows[0].State)
	}
}

func TestAcquirer_Charge_AdapterError_MarksUnknown_NotFailed(t *testing.T) {
	// 资损修复回归：callErr（网络超时 / 5xx / ctx canceled）必须落 unknown，
	// 不能落 failed —— 否则 CallRetryWorker 会按原 idempotency_key 重发原请求，
	// 而第一次很可能已在渠道侧落账，造成双扣 / 双退。
	ad := newFakeAdapter("gcash")
	ad.nextChargeErr = errors.New("upstream 502")
	svc, txRepo := newAcquirerSvcForTest(t, ad)

	_, err := svc.Charge(context.Background(), "gcash", &channel.ChargeRequest{
		PiID: "pi_3", IdempotencyKey: "k3", Amount: 100, Currency: "PHP",
	})
	if err == nil {
		t.Fatal("want error from adapter")
	}
	rows := txRepo.rowsByAdapter("gcash")
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].State != domain.AcquirerTxUnknown {
		t.Fatalf("want state=unknown got %s (regression: callErr → failed reintroduces 重复扣款 风险)", rows[0].State)
	}
	if rows[0].FailureCode != channel.FailChannelUnavailable {
		t.Fatalf("want channel_unavailable on network err, got %s", rows[0].FailureCode)
	}
	// CallRetryWorker 的 listing 必须是空 —— unknown 不该被它重发。
	pending, _ := txRepo.ListPendingRetries(context.Background(), 10)
	if len(pending) != 0 {
		t.Fatalf("unknown rows must NOT be in ListPendingRetries (would cause re-issue), got %d", len(pending))
	}
	// 但应该出现在 ListUnknownTxs 里 —— PendingQueryWorker 会用 Query 推进。
	unk, _ := txRepo.ListUnknownTxs(context.Background(), 10, 0, 0)
	if len(unk) != 1 {
		t.Fatalf("want 1 unknown row visible to PendingQueryWorker, got %d", len(unk))
	}
}

func TestAcquirer_Charge_ChannelReturnsUnknown_StaysUnknown(t *testing.T) {
	// 渠道明示「请走 Query 推进」（部分钱包同步只回 ack）：resp.Result=Unknown
	// 不能当 succeeded 也不能当 failed。
	ad := newFakeAdapter("gcash")
	ad.nextChargeResp = &channel.ChargeResponse{
		Result:        channel.ResultUnknown,
		ExternalRefNo: "ack_xyz",
	}
	svc, txRepo := newAcquirerSvcForTest(t, ad)
	_, err := svc.Charge(context.Background(), "gcash", &channel.ChargeRequest{
		PiID: "pi_u", IdempotencyKey: "ku", Amount: 100, Currency: "PHP",
	})
	if err != nil {
		t.Fatalf("no err expected, got %v", err)
	}
	rows := txRepo.rowsByAdapter("gcash")
	if len(rows) != 1 || rows[0].State != domain.AcquirerTxUnknown {
		t.Fatalf("want 1 unknown row, got %+v", rows)
	}
}

func TestAcquirer_Refund_IdempotentReplay(t *testing.T) {
	ad := newFakeAdapter("gcash")
	svc, _ := newAcquirerSvcForTest(t, ad)

	req := &channel.RefundRequest{
		PiID: "pi_1", ExternalRefNo: "ref_pi_1", Amount: 5000, IdempotencyKey: "rfd-1",
	}
	r1, err := svc.Refund(context.Background(), "gcash", req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.Refund(context.Background(), "gcash", req)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, refunds, _ := ad.counts()
	if refunds != 1 {
		t.Fatalf("want 1 refund (second is replay), got %d", refunds)
	}
	if r1.ExternalRefNo != r2.ExternalRefNo {
		t.Fatalf("replay ref mismatch: %s vs %s", r1.ExternalRefNo, r2.ExternalRefNo)
	}
}

func TestAcquirer_Query_NoPersistence(t *testing.T) {
	ad := newFakeAdapter("gcash")
	ad.nextQueryResp = &channel.QueryResponse{Result: channel.ResultSucceeded, ExternalRefNo: "ref_x"}
	svc, txRepo := newAcquirerSvcForTest(t, ad)

	resp, err := svc.Query(context.Background(), "gcash", &channel.QueryRequest{
		PiID: "pi_9", ExternalRefNo: "ref_x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExternalRefNo != "ref_x" {
		t.Fatalf("bad ref: %s", resp.ExternalRefNo)
	}
	// Query 不写 acquirer_tx（只读操作）
	if got := len(txRepo.rowsByAdapter("gcash")); got != 0 {
		t.Fatalf("Query should not persist rows, got %d", got)
	}
	_, _, _, _, queries := ad.counts()
	if queries != 1 {
		t.Fatalf("want 1 query, got %d", queries)
	}
}

func TestAcquirer_UnknownAdapter_Errors(t *testing.T) {
	svc, _ := newAcquirerSvcForTest(t, newFakeAdapter("gcash"))
	_, err := svc.Charge(context.Background(), "unknown", &channel.ChargeRequest{
		PiID: "pi_1", IdempotencyKey: "idem", Amount: 1, Currency: "PHP",
	})
	if err == nil {
		t.Fatal("want error")
	}
}
