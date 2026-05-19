package service

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	channelv1 "reconcile-system/packages/payment-channel/kitex_gen/channel/v1"
	"github.com/xiongwp/payment-core/internal/channel"
	"github.com/xiongwp/payment-core/internal/routing"
)

// fakeChannelClient 记录最后一次请求并返回预设响应。
type fakeChannelClient struct {
	lastCharge *channelv1.ChargeRequest
	lastRefund *channelv1.RefundRequest
	chargeResp *channelv1.ChargeResponse
	refundResp *channelv1.OpResponse
	err        error
}

func (f *fakeChannelClient) Charge(_ context.Context, in *channelv1.ChargeRequest, _ ...grpc.CallOption) (*channelv1.ChargeResponse, error) {
	f.lastCharge = in
	return f.chargeResp, f.err
}
func (f *fakeChannelClient) Capture(context.Context, *channelv1.CaptureRequest, ...grpc.CallOption) (*channelv1.OpResponse, error) {
	return &channelv1.OpResponse{Result: "succeeded"}, nil
}
func (f *fakeChannelClient) Void(context.Context, *channelv1.VoidRequest, ...grpc.CallOption) (*channelv1.OpResponse, error) {
	return &channelv1.OpResponse{Result: "succeeded"}, nil
}
func (f *fakeChannelClient) Refund(_ context.Context, in *channelv1.RefundRequest, _ ...grpc.CallOption) (*channelv1.OpResponse, error) {
	f.lastRefund = in
	return f.refundResp, f.err
}
func (f *fakeChannelClient) Query(context.Context, *channelv1.QueryRequest, ...grpc.CallOption) (*channelv1.QueryResponse, error) {
	return &channelv1.QueryResponse{Result: "succeeded"}, nil
}
func (f *fakeChannelClient) Close() error { return nil }

// fakeChannelClient needs to implement channelclient.Client; in this test
// we bind it directly via a local wrapper so we don't bring the client pkg.
type clientWrapper struct{ *fakeChannelClient }

func (c *clientWrapper) Charge(ctx context.Context, in *channelv1.ChargeRequest) (*channelv1.ChargeResponse, error) {
	return c.fakeChannelClient.Charge(ctx, in)
}
func (c *clientWrapper) Capture(ctx context.Context, in *channelv1.CaptureRequest) (*channelv1.OpResponse, error) {
	return c.fakeChannelClient.Capture(ctx, in)
}
func (c *clientWrapper) Void(ctx context.Context, in *channelv1.VoidRequest) (*channelv1.OpResponse, error) {
	return c.fakeChannelClient.Void(ctx, in)
}
func (c *clientWrapper) Refund(ctx context.Context, in *channelv1.RefundRequest) (*channelv1.OpResponse, error) {
	return c.fakeChannelClient.Refund(ctx, in)
}
func (c *clientWrapper) Query(ctx context.Context, in *channelv1.QueryRequest) (*channelv1.QueryResponse, error) {
	return c.fakeChannelClient.Query(ctx, in)
}
func (c *clientWrapper) Close() error { return nil }

func newTestSvc(t *testing.T, cli *fakeChannelClient) *PaymentService {
	t.Helper()
	r := routing.NewRouter([]routing.Rule{
		{Priority: 100, Country: "PH", PaymentMethod: "GCASH", Adapter: "gcash"},
		{Priority: 100, Country: "PH", PaymentMethod: "MAYA", Adapter: "maya"},
		{Priority: 999, Country: "PH", Adapter: "gcash"},
	})
	return NewPaymentService(r, &clientWrapper{fakeChannelClient: cli}, nil, zap.NewNop())
}

func TestCharge_RoutesByPaymentMethod(t *testing.T) {
	cli := &fakeChannelClient{chargeResp: &channelv1.ChargeResponse{Result: "succeeded", ExternalRefNo: "ref_1"}}
	svc := newTestSvc(t, cli)
	resp, err := svc.Charge(context.Background(), &channel.PaymentRequest{
		PaymentIntentID: "pi_1",
		Amount:          10000,
		Currency:        "PHP",
		Country:         "PH",
		PaymentMethod:   "MAYA",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ResultType != channel.ResultSucceeded || resp.ExternalRefNo != "ref_1" {
		t.Fatalf("bad resp: %+v", resp)
	}
	if cli.lastCharge.GetAdapter() != "maya" {
		t.Fatalf("want adapter=maya got %q", cli.lastCharge.GetAdapter())
	}
	if cli.lastCharge.GetIdempotencyKey() == "" {
		t.Fatal("idem key must be stamped")
	}
}

func TestRefund_UsesAdapterFromExtra(t *testing.T) {
	cli := &fakeChannelClient{refundResp: &channelv1.OpResponse{Result: "succeeded", ExternalRefNo: "rfd_1"}}
	svc := newTestSvc(t, cli)
	resp, err := svc.Refund(context.Background(), &channel.RefundChannelRequest{
		PaymentIntentID: "pi_1",
		RefundID:        "re_1",
		ExternalRefNo:   "ref_1",
		Amount:          5000,
		Extra:           map[string]string{"adapter": "maya"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cli.lastRefund.GetAdapter() != "maya" {
		t.Fatalf("want adapter=maya got %q", cli.lastRefund.GetAdapter())
	}
	if resp.ExternalRefundRefNo != "rfd_1" {
		t.Fatalf("bad refund resp: %+v", resp)
	}
}

// TestRefund_RoutingFallbackRejected verifies P2-1: 只给 country/payment_method 没有
// 显式 adapter 必须被拒绝（避免 routing 热更新后 refund 找错渠道）。
func TestRefund_RoutingFallbackRejected(t *testing.T) {
	cli := &fakeChannelClient{refundResp: &channelv1.OpResponse{Result: "succeeded"}}
	svc := newTestSvc(t, cli)
	_, err := svc.Refund(context.Background(), &channel.RefundChannelRequest{
		PaymentIntentID: "pi_1",
		RefundID:        "re_1",
		ExternalRefNo:   "ref_1",
		Amount:          5000,
		Extra:           map[string]string{"country": "PH", "payment_method": "GCASH"},
	})
	if err == nil {
		t.Fatal("expected error: routing-based fallback must be rejected")
	}
	if cli.lastRefund != nil {
		t.Fatal("must not call payment-channel without explicit adapter")
	}
}

func TestRefund_NoContextIsRejected(t *testing.T) {
	cli := &fakeChannelClient{}
	svc := newTestSvc(t, cli)
	_, err := svc.Refund(context.Background(), &channel.RefundChannelRequest{
		PaymentIntentID: "pi_1",
		RefundID:        "re_1",
		ExternalRefNo:   "ref_1",
		Amount:          5000,
		// no Extra -> must fail, never silently pick a random adapter
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if cli.lastRefund != nil {
		t.Fatal("should not have called payment-channel")
	}
}

func TestIdempotencyKey_Stability(t *testing.T) {
	a := IdempotencyKey("pi_1", "charge", 10000, "PHP")
	b := IdempotencyKey("pi_1", "charge", 10000, "PHP")
	if a != b {
		t.Fatal("idem key not deterministic")
	}
	c := IdempotencyKey("pi_1", "refund", 10000, "PHP")
	if a == c {
		t.Fatal("different action should yield different key")
	}
	if len(a) != 64 {
		t.Fatalf("want hex sha256 length 64 got %d", len(a))
	}
	// 金额 / 币种纳入 hash：任何一个不同都得到新 key
	d := IdempotencyKey("pi_1", "charge", 20000, "PHP")
	if a == d {
		t.Fatal("amount must affect idem key")
	}
	e := IdempotencyKey("pi_1", "charge", 10000, "USD")
	if a == e {
		t.Fatal("currency must affect idem key")
	}
}

func TestRefundIdempotencyKey_Stability(t *testing.T) {
	a := RefundIdempotencyKey("pi_1", "re_1", 5000, "PHP")
	b := RefundIdempotencyKey("pi_1", "re_1", 5000, "PHP")
	if a != b {
		t.Fatal("refund idem key not deterministic")
	}
	// 不同 refund_id 应当不同
	c := RefundIdempotencyKey("pi_1", "re_2", 5000, "PHP")
	if a == c {
		t.Fatal("different refund_id must yield different key")
	}
	// 不同金额应当不同
	d := RefundIdempotencyKey("pi_1", "re_1", 6000, "PHP")
	if a == d {
		t.Fatal("amount must affect refund idem key")
	}
	// 与 charge 区分
	if a == IdempotencyKey("pi_1", "re_1", 5000, "PHP") {
		t.Fatal("refund key must differ from charge key")
	}
}

// TestRefund_NoRefundIDRejected verifies P0-3: 缺 refund_id 必须拒绝。
func TestRefund_NoRefundIDRejected(t *testing.T) {
	cli := &fakeChannelClient{}
	svc := newTestSvc(t, cli)
	_, err := svc.Refund(context.Background(), &channel.RefundChannelRequest{
		PaymentIntentID: "pi_1",
		// no RefundID
		ExternalRefNo: "ref_1",
		Amount:        5000,
		Extra:         map[string]string{"adapter": "maya"},
	})
	if err == nil {
		t.Fatal("expected error: refund_id required")
	}
	if cli.lastRefund != nil {
		t.Fatal("must not call payment-channel without refund_id")
	}
}

var _ = errors.New
