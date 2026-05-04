// Package server_test 下的三层端到端联调测试。
//
// 链路：order-core（模拟）→ payment-core（真）→ payment-channel（fake 服务端，
// 外部渠道响应 mock 在 adapter 层）。
//
// 本测试不需要 MySQL / Docker；用 bufconn 把两段 gRPC 全部跑在进程内：
//
//	[fake order-core caller]
//	        │ paymentcorev1 RPC
//	        ▼
//	[real payment-core Server]  ← internal/server/grpc.go
//	        │ channelv1 RPC
//	        ▼
//	[fake payment-channel Server] ← 实现 channelv1.AcquirerServiceServer，
//	                                  canned responses 模拟外部渠道
package server_test

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	channelv1 "github.com/xiongwp/payment-channel/api/proto/channel/v1"
	paymentcorev1 "github.com/xiongwp/payment-core/api/proto/paymentcore/v1"
	"github.com/xiongwp/payment-core/internal/channelclient"
	"github.com/xiongwp/payment-core/internal/routing"
	"github.com/xiongwp/payment-core/internal/server"
	"github.com/xiongwp/payment-core/internal/service"
)

// ─── fake payment-channel server（mocks the whole downstream tier） ──────

type fakeChannelServer struct {
	channelv1.UnimplementedAcquirerServiceServer

	mu           sync.Mutex
	chargeReqs   []*channelv1.ChargeRequest
	refundReqs   []*channelv1.RefundRequest
	queryReqs    []*channelv1.QueryRequest
	// scripted：模拟特定 adapter / external channel 的不同结果
	chargeResp   *channelv1.ChargeResponse
	refundResp   *channelv1.OpResponse
	queryResp    *channelv1.QueryResponse
	// callCount：全局调用计数，验证"请求确实打到 channel 层"
	totalCalls atomic.Int32
}

func (f *fakeChannelServer) Charge(ctx context.Context, req *channelv1.ChargeRequest) (*channelv1.ChargeResponse, error) {
	f.totalCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chargeReqs = append(f.chargeReqs, req)
	if f.chargeResp != nil {
		return f.chargeResp, nil
	}
	// 默认：按 adapter 给不同的 canned 响应，模拟外部渠道
	switch req.GetAdapter() {
	case "gcash":
		return &channelv1.ChargeResponse{
			Result:        "requires_action",
			ExternalRefNo: "gcash_" + req.GetPiId(),
			RequiredAction: &channelv1.RequiredAction{
				Type:        "app_redirect",
				ExpiresAt:   time.Now().Add(15 * time.Minute).Unix(),
				RedirectUrl: "gcash://pay?token=xxx",
				Scheme:      "universal",
				ReturnUrl:   req.GetReturnUrl(),
			},
		}, nil
	case "maya":
		return &channelv1.ChargeResponse{
			Result:        "requires_action",
			ExternalRefNo: "maya_co_" + req.GetPiId(),
			RequiredAction: &channelv1.RequiredAction{
				Type:        "app_redirect",
				ExpiresAt:   time.Now().Add(30 * time.Minute).Unix(),
				RedirectUrl: "https://payments.paymaya.com/checkout?id=co_" + req.GetPiId(),
			},
		}, nil
	case "instapay":
		// 银行转账同步成功
		return &channelv1.ChargeResponse{
			Result:        "succeeded",
			ExternalRefNo: "IP" + req.GetPiId(),
		}, nil
	case "pesonet":
		return &channelv1.ChargeResponse{Result: "processing", ExternalRefNo: "PN" + req.GetPiId()}, nil
	default:
		// 模拟外部渠道拒绝
		return &channelv1.ChargeResponse{
			Result:         "failed",
			FailureCode:    "channel_unavailable",
			RawFailureCode: "ADAPTER_NOT_CONFIGURED",
			FailureMessage: "adapter " + req.GetAdapter() + " not configured",
		}, nil
	}
}

func (f *fakeChannelServer) Capture(_ context.Context, req *channelv1.CaptureRequest) (*channelv1.OpResponse, error) {
	f.totalCalls.Add(1)
	return &channelv1.OpResponse{Result: "succeeded", ExternalRefNo: req.GetExternalRefNo()}, nil
}
func (f *fakeChannelServer) Void(_ context.Context, req *channelv1.VoidRequest) (*channelv1.OpResponse, error) {
	f.totalCalls.Add(1)
	return &channelv1.OpResponse{Result: "succeeded", ExternalRefNo: req.GetExternalRefNo()}, nil
}
func (f *fakeChannelServer) Refund(ctx context.Context, req *channelv1.RefundRequest) (*channelv1.OpResponse, error) {
	f.totalCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refundReqs = append(f.refundReqs, req)
	if f.refundResp != nil {
		return f.refundResp, nil
	}
	return &channelv1.OpResponse{Result: "succeeded", ExternalRefNo: "rfd_" + req.GetExternalRefNo()}, nil
}
func (f *fakeChannelServer) Query(_ context.Context, req *channelv1.QueryRequest) (*channelv1.QueryResponse, error) {
	f.totalCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queryReqs = append(f.queryReqs, req)
	if f.queryResp != nil {
		return f.queryResp, nil
	}
	return &channelv1.QueryResponse{Result: "succeeded", ExternalRefNo: req.GetExternalRefNo()}, nil
}

// lock for testutils; Go vet 不会挑，但显式声明更清楚
type mutex interface {
	Lock()
	Unlock()
}

var _ mutex = (*sync.Mutex)(nil)

// ─── workspace helpers ────────────────────────────────────────────────

// spinUpFakeChannel 启动一个本地 channel gRPC 服务器并返回一个可 dial 的
// bufconn。测试函数结束时自动 graceful stop。
func spinUpFakeChannel(t *testing.T, fake *fakeChannelServer) *bufconn.Listener {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	channelv1.RegisterAcquirerServiceServer(srv, fake)
	go func() {
		if err := srv.Serve(lis); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Logf("fake channel serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		srv.GracefulStop()
		_ = lis.Close()
	})
	return lis
}

// bufconnClient 把 bufconn 包成 channelclient.Client。
type bufconnChannelClient struct {
	conn *grpc.ClientConn
	api  channelv1.AcquirerServiceClient
}

func newBufconnChannelClient(t *testing.T, lis *bufconn.Listener) channelclient.Client {
	t.Helper()
	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &bufconnChannelClient{conn: conn, api: channelv1.NewAcquirerServiceClient(conn)}
}

func (c *bufconnChannelClient) Charge(ctx context.Context, in *channelv1.ChargeRequest) (*channelv1.ChargeResponse, error) {
	return c.api.Charge(ctx, in)
}
func (c *bufconnChannelClient) Capture(ctx context.Context, in *channelv1.CaptureRequest) (*channelv1.OpResponse, error) {
	return c.api.Capture(ctx, in)
}
func (c *bufconnChannelClient) Void(ctx context.Context, in *channelv1.VoidRequest) (*channelv1.OpResponse, error) {
	return c.api.Void(ctx, in)
}
func (c *bufconnChannelClient) Refund(ctx context.Context, in *channelv1.RefundRequest) (*channelv1.OpResponse, error) {
	return c.api.Refund(ctx, in)
}
func (c *bufconnChannelClient) Query(ctx context.Context, in *channelv1.QueryRequest) (*channelv1.QueryResponse, error) {
	return c.api.Query(ctx, in)
}
func (c *bufconnChannelClient) Close() error { return c.conn.Close() }

// spinUpPaymentCore 启动真实的 payment-core Server（routing + service 全走完）。
// 上游 payment-channel 用 fake bufconn 顶替，外部渠道响应 = fake 的 scripted。
func spinUpPaymentCore(t *testing.T, channelCli channelclient.Client, rules []routing.Rule) paymentcorev1.PaymentCoreServiceClient {
	t.Helper()
	logger := zap.NewNop()

	router := routing.NewRouter(rules)
	paymentSvc := service.NewPaymentService(router, channelCli, nil, logger)
	webhookSvc := service.NewWebhookService(logger)
	s := server.NewServer(server.Deps{
		PaymentSvc: paymentSvc,
		WebhookSvc: webhookSvc,
		Logger:     logger,
	})

	lis := bufconn.Listen(1 << 20)
	grpcSrv := grpc.NewServer()
	paymentcorev1.RegisterPaymentCoreServiceServer(grpcSrv, s)
	go func() {
		if err := grpcSrv.Serve(lis); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Logf("payment-core serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		grpcSrv.GracefulStop()
		_ = lis.Close()
	})

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return paymentcorev1.NewPaymentCoreServiceClient(conn)
}

// defaultPHRules 和 config/config.yaml 里的 routing 保持一致。
func defaultPHRules() []routing.Rule {
	return []routing.Rule{
		{Priority: 100, Country: "PH", PaymentMethod: "GCASH", Adapter: "gcash"},
		{Priority: 100, Country: "PH", PaymentMethod: "MAYA", Adapter: "maya"},
		{Priority: 100, Country: "PH", PaymentMethod: "GRABPAY", Adapter: "grabpay"},
		{Priority: 100, Country: "PH", PaymentMethod: "INSTAPAY", AmountMax: 5000000, Adapter: "instapay"},
		{Priority: 110, Country: "PH", PaymentMethod: "INSTAPAY", AmountMin: 5000001, Adapter: "pesonet"},
		{Priority: 999, Country: "PH", Adapter: "gcash"},
	}
}

// ─── E2E tests ────────────────────────────────────────────────────────

func TestE2E_OrderCoreToPaymentCoreToChannel_GCashRedirect(t *testing.T) {
	// 1. 起 fake payment-channel（外部渠道完全 mock 在它的 canned 响应里）
	fakeCh := &fakeChannelServer{}
	chLis := spinUpFakeChannel(t, fakeCh)

	// 2. 起 real payment-core，client 指到 fake channel
	chCli := newBufconnChannelClient(t, chLis)
	pcCli := spinUpPaymentCore(t, chCli, defaultPHRules())

	// 3. 模拟 order-core 下单（通过 paymentcorev1 gRPC）
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := pcCli.Charge(ctx, &paymentcorev1.ChargeRequest{
		PaymentIntentId: "pi_4471234560001",
		ChargeId:        "ch_4471234560001",
		Amount:          10000,
		Currency:        "PHP",
		Country:         "PH",
		PaymentMethod:   "GCASH",
		ReturnUrl:       "https://cashier.example.com/ret",
	})
	if err != nil {
		t.Fatalf("order-core → payment-core Charge failed: %v", err)
	}

	// 校验：payment-core 返回 order-core 期望的形状
	if resp.GetResultType() != "requires_action" {
		t.Fatalf("GCash should return requires_action, got %s", resp.GetResultType())
	}
	if resp.GetExternalRefNo() != "gcash_pi_4471234560001" {
		t.Fatalf("bad external_ref: %q", resp.GetExternalRefNo())
	}
	ra := resp.GetRequiredAction()
	if ra == nil {
		t.Fatal("expected required_action")
	}
	if ra.GetType() != "app_redirect" {
		t.Fatalf("want app_redirect got %s", ra.GetType())
	}
	if got := ra.GetDetails()["redirect_url"]; got != "gcash://pay?token=xxx" {
		t.Fatalf("redirect_url not flattened: %q", got)
	}
	if got := ra.GetDetails()["return_url"]; got != "https://cashier.example.com/ret" {
		t.Fatalf("return_url should come from order-core input: %q", got)
	}

	// 校验：payment-channel 那边收到的 request 是 payment-core 加工后的
	if len(fakeCh.chargeReqs) != 1 {
		t.Fatalf("want 1 channel charge, got %d", len(fakeCh.chargeReqs))
	}
	call := fakeCh.chargeReqs[0]
	if call.GetAdapter() != "gcash" {
		t.Fatalf("router should pick gcash adapter, got %q", call.GetAdapter())
	}
	if call.GetIdempotencyKey() == "" {
		t.Fatal("payment-core must stamp idempotency_key")
	}
	if call.GetAmount() != 10000 || call.GetCurrency() != "PHP" {
		t.Fatalf("amount/currency wrong: %d %s", call.GetAmount(), call.GetCurrency())
	}
}

func TestE2E_RoutingByAmount_BankTransfer(t *testing.T) {
	fakeCh := &fakeChannelServer{}
	chLis := spinUpFakeChannel(t, fakeCh)
	chCli := newBufconnChannelClient(t, chLis)
	pcCli := spinUpPaymentCore(t, chCli, defaultPHRules())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// ≤₱50,000 走 InstaPay 实时
	small, err := pcCli.Charge(ctx, &paymentcorev1.ChargeRequest{
		PaymentIntentId: "pi_1171234560001",
		Amount:          4000000, // ₱40,000
		Currency:        "PHP",
		Country:         "PH",
		PaymentMethod:   "INSTAPAY",
	})
	if err != nil {
		t.Fatal(err)
	}
	if small.GetResultType() != "succeeded" {
		t.Fatalf("small-amount InstaPay should succeed synchronously, got %s", small.GetResultType())
	}

	// >₱50,000 自动降级到 PESONet（配置里 priority 更高的具体规则命中）
	big, err := pcCli.Charge(ctx, &paymentcorev1.ChargeRequest{
		PaymentIntentId: "pi_2271234560001",
		Amount:          6000000, // ₱60,000
		Currency:        "PHP",
		Country:         "PH",
		PaymentMethod:   "INSTAPAY",
	})
	if err != nil {
		t.Fatal(err)
	}
	if big.GetResultType() != "processing" {
		t.Fatalf("big-amount should degrade to PESONet (processing), got %s", big.GetResultType())
	}

	// 校验 channel 层收到的 adapter 是不同的
	if len(fakeCh.chargeReqs) != 2 {
		t.Fatalf("want 2 charges at channel, got %d", len(fakeCh.chargeReqs))
	}
	if fakeCh.chargeReqs[0].GetAdapter() != "instapay" {
		t.Fatalf("first should be instapay got %s", fakeCh.chargeReqs[0].GetAdapter())
	}
	if fakeCh.chargeReqs[1].GetAdapter() != "pesonet" {
		t.Fatalf("second should be pesonet got %s", fakeCh.chargeReqs[1].GetAdapter())
	}
}

func TestE2E_Refund_RoutesByExtra(t *testing.T) {
	fakeCh := &fakeChannelServer{}
	chLis := spinUpFakeChannel(t, fakeCh)
	chCli := newBufconnChannelClient(t, chLis)
	pcCli := spinUpPaymentCore(t, chCli, defaultPHRules())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Refund 必须由调用方（order-core）把 adapter / payment_method 透传过来，
	// payment-core 不会猜测。
	resp, err := pcCli.Refund(ctx, &paymentcorev1.RefundRequest{
		PaymentIntentId: "pi_4471234560001",
		ChargeId:        "ch_4471234560001",
		RefundId:        "re_4471234560001",
		ExternalRefNo:   "gcash_ref_1",
		Amount:          5000,
		Currency:        "PHP",
		Extra:           map[string]string{"adapter": "gcash"},
	})
	if err != nil {
		t.Fatalf("refund err: %v", err)
	}
	if resp.GetResultType() != "succeeded" {
		t.Fatalf("want succeeded refund got %s", resp.GetResultType())
	}
	if len(fakeCh.refundReqs) != 1 {
		t.Fatalf("want 1 channel refund got %d", len(fakeCh.refundReqs))
	}
	if fakeCh.refundReqs[0].GetAdapter() != "gcash" {
		t.Fatalf("refund should route to gcash via extra[adapter], got %q", fakeCh.refundReqs[0].GetAdapter())
	}
	if fakeCh.refundReqs[0].GetIdempotencyKey() == "" {
		t.Fatal("refund must have idempotency key")
	}
}

func TestE2E_Refund_FailsWithoutRoutingHint(t *testing.T) {
	fakeCh := &fakeChannelServer{}
	chLis := spinUpFakeChannel(t, fakeCh)
	chCli := newBufconnChannelClient(t, chLis)
	pcCli := spinUpPaymentCore(t, chCli, defaultPHRules())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 不传 extra.adapter 也不传 payment_method → payment-core 必须拒绝，
	// 绝不允许猜测 adapter（防止钱跑错渠道）
	_, err := pcCli.Refund(ctx, &paymentcorev1.RefundRequest{
		PaymentIntentId: "pi_4471234560001",
		ChargeId:        "ch_4471234560001",
		RefundId:        "re_4471234560001",
		ExternalRefNo:   "gcash_ref_1",
		Amount:          5000,
	})
	if err == nil {
		t.Fatal("refund without routing hint should be rejected")
	}
	if fakeCh.totalCalls.Load() != 0 {
		t.Fatal("should not have hit payment-channel at all")
	}
}

func TestE2E_Query_PassesThrough(t *testing.T) {
	fakeCh := &fakeChannelServer{}
	chLis := spinUpFakeChannel(t, fakeCh)
	chCli := newBufconnChannelClient(t, chLis)
	pcCli := spinUpPaymentCore(t, chCli, defaultPHRules())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := pcCli.Query(ctx, &paymentcorev1.QueryRequest{
		PaymentIntentId: "pi_4471234560001",
		ExternalRefNo:   "ref_x",
		Extra:           map[string]string{"adapter": "maya"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResultType() != "succeeded" {
		t.Fatalf("want succeeded got %s", resp.GetResultType())
	}
	if len(fakeCh.queryReqs) != 1 {
		t.Fatalf("want 1 query got %d", len(fakeCh.queryReqs))
	}
	if fakeCh.queryReqs[0].GetAdapter() != "maya" {
		t.Fatalf("query should route to maya got %q", fakeCh.queryReqs[0].GetAdapter())
	}
}

func TestE2E_Webhook_Normalization(t *testing.T) {
	// Webhook 是 payment-channel → payment-core → order-core 方向，
	// payment-core 只做协议归一，不调下游。所以这里 fakeChannelServer 不被触达。
	fakeCh := &fakeChannelServer{}
	chLis := spinUpFakeChannel(t, fakeCh)
	chCli := newBufconnChannelClient(t, chLis)
	pcCli := spinUpPaymentCore(t, chCli, defaultPHRules())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	evt, err := pcCli.ParseWebhook(ctx, &paymentcorev1.ParseWebhookRequest{
		Adapter: "gcash",
		Headers: map[string]string{"Signature": "ok"},
		Body:    []byte(`{"paymentRequestId":"pi_4471234560001","paymentId":"pay_abc","paymentStatus":"SUCCESS"}`),
	})
	if err != nil {
		t.Fatalf("parse webhook: %v", err)
	}
	if evt.GetEventType() != "charge.succeeded" {
		t.Fatalf("want canonical charge.succeeded, got %q", evt.GetEventType())
	}
	if evt.GetPaymentIntentId() != "pi_4471234560001" {
		t.Fatalf("pi_id extraction wrong: %q", evt.GetPaymentIntentId())
	}
	if evt.GetExternalRefNo() != "pay_abc" {
		t.Fatalf("external_ref extraction wrong: %q", evt.GetExternalRefNo())
	}
	if fakeCh.totalCalls.Load() != 0 {
		t.Fatal("ParseWebhook must not touch payment-channel")
	}
}
