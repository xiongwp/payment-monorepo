// payment-channel 端 bufconn e2e 测试。
//
// 链路：[order-core 模拟者] → [real AcquirerService gRPC Server]
//                              └→ real AcquirerService service
//                                   └→ fake adapter（外部渠道响应 mock）
//
// 外部渠道（GCash / Maya / 银行 API）全部由 fakeAdapter 模拟。本测试不依赖
// MySQL，仓储用内存实现。
package server_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	channelv1 "github.com/xiongwp/payment-channel/api/proto/channel/v1"
	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/server"
	"github.com/xiongwp/payment-channel/internal/service"
)

// ─── adapter：模拟外部渠道（GCash 等的 HTTP 响应都通过它返回）────────────

type scriptedAdapter struct {
	name string

	chargeResp *channel.ChargeResponse
	chargeErr  error
	refundResp *channel.OpResponse
	queryResp  *channel.QueryResponse
}

func (a *scriptedAdapter) Name() string { return a.name }

func (a *scriptedAdapter) Charge(_ context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	if a.chargeErr != nil {
		return nil, a.chargeErr
	}
	if a.chargeResp != nil {
		return a.chargeResp, nil
	}
	return &channel.ChargeResponse{Result: channel.ResultSucceeded, ExternalRefNo: a.name + "_" + req.PiID}, nil
}
func (a *scriptedAdapter) Capture(_ context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}
func (a *scriptedAdapter) Void(_ context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}
func (a *scriptedAdapter) Refund(_ context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	if a.refundResp != nil {
		return a.refundResp, nil
	}
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: "rfd_" + req.ExternalRefNo}, nil
}
func (a *scriptedAdapter) Query(_ context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	if a.queryResp != nil {
		return a.queryResp, nil
	}
	return &channel.QueryResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}
func (a *scriptedAdapter) ParseWebhook(_ map[string]string, _ []byte) (*channel.WebhookEvent, error) {
	return &channel.WebhookEvent{EventID: "e", PiID: "pi_1", EventType: "charge.succeeded", SignatureOK: true}, nil
}

// ─── 本测试用的内存仓储：内联 in-memory 版本 ──────────────────────────

// 注：internal/service/fakes_test.go 里的 memTxRepo 是 _test 内部符号，
// 从 server_test 包里访问不到。我们在这里重新定义最小可用集。

type idIssuer struct{ n int64 }

func (i *idIssuer) Next(prefix, piID string) (string, error) {
	i.n++
	return prefix + "_" + piID, nil
}

// ─── harness ──────────────────────────────────────────────────────

func spinUpChannelServer(t *testing.T, ad channel.Adapter) channelv1.AcquirerServiceClient {
	t.Helper()
	logger := zap.NewNop()
	reg := channel.NewRegistry()
	reg.Register(ad)
	txRepo := newInProcTxRepo()
	svc := service.NewAcquirerService(reg, txRepo, &idIssuer{}, logger)
	s := server.NewServer(server.Deps{AcquirerSvc: svc, Logger: logger})

	lis := bufconn.Listen(1 << 20)
	grpcSrv := grpc.NewServer()
	channelv1.RegisterAcquirerServiceServer(grpcSrv, s)
	go func() {
		if err := grpcSrv.Serve(lis); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Logf("paychan serve: %v", err)
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
	return channelv1.NewAcquirerServiceClient(conn)
}

// ─── E2E tests ─────────────────────────────────────────────────────

func TestPaychanE2E_Charge_HappyPath(t *testing.T) {
	ad := &scriptedAdapter{name: "gcash"}
	cli := spinUpChannelServer(t, ad)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := cli.Charge(ctx, &channelv1.ChargeRequest{
		Adapter:        "gcash",
		PiId:           "pi_4471234560001",
		IdempotencyKey: "idem-1",
		Amount:         10000,
		Currency:       "PHP",
	})
	if err != nil {
		t.Fatalf("charge err: %v", err)
	}
	if resp.GetResult() != "succeeded" {
		t.Fatalf("want succeeded got %s", resp.GetResult())
	}
	if resp.GetExternalRefNo() != "gcash_pi_4471234560001" {
		t.Fatalf("bad ref: %s", resp.GetExternalRefNo())
	}
}

func TestPaychanE2E_Charge_RequiresActionRedirect(t *testing.T) {
	ad := &scriptedAdapter{
		name: "maya",
		chargeResp: &channel.ChargeResponse{
			Result:        channel.ResultRequiresAction,
			ExternalRefNo: "co_1",
			RequiredAction: &channel.RequiredAction{
				Type:        "app_redirect",
				ExpiresAt:   time.Now().Add(30 * time.Minute),
				RedirectURL: "https://payments.paymaya.com/checkout?id=co_1",
			},
		},
	}
	cli := spinUpChannelServer(t, ad)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := cli.Charge(ctx, &channelv1.ChargeRequest{
		Adapter:        "maya",
		PiId:           "pi_5571234560001",
		IdempotencyKey: "k-maya",
		Amount:         20000,
		Currency:       "PHP",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != "requires_action" {
		t.Fatalf("want requires_action got %s", resp.GetResult())
	}
	ra := resp.GetRequiredAction()
	if ra == nil || ra.GetRedirectUrl() == "" {
		t.Fatal("required_action should carry redirect_url")
	}
	if !strings.Contains(ra.GetRedirectUrl(), "paymaya.com") {
		t.Fatalf("bad redirect_url: %q", ra.GetRedirectUrl())
	}
}

func TestPaychanE2E_Charge_UnknownAdapter_Rejected(t *testing.T) {
	cli := spinUpChannelServer(t, &scriptedAdapter{name: "gcash"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cli.Charge(ctx, &channelv1.ChargeRequest{
		Adapter:        "unregistered",
		PiId:           "pi_9",
		IdempotencyKey: "k",
		Amount:         100,
		Currency:       "PHP",
	})
	if err == nil {
		t.Fatal("unregistered adapter should fail")
	}
}

func TestPaychanE2E_Charge_IdempotencyReplay_NoDoubleCall(t *testing.T) {
	// 通过一个计数 adapter 验证：同 idem 第二次不会真的调 adapter
	ad := &scriptedAdapter{name: "gcash"}
	counting := &callCountingAdapter{inner: ad}
	cli := spinUpChannelServer(t, counting)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := &channelv1.ChargeRequest{
		Adapter:        "gcash",
		PiId:           "pi_0071234560001",
		IdempotencyKey: "dup-idem",
		Amount:         100,
		Currency:       "PHP",
	}
	r1, err := cli.Charge(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := cli.Charge(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if r1.GetExternalRefNo() != r2.GetExternalRefNo() {
		t.Fatalf("idempotent replay returned different ref: %q vs %q", r1.GetExternalRefNo(), r2.GetExternalRefNo())
	}
	if n := counting.count(); n != 1 {
		t.Fatalf("expected 1 adapter call (second is replay), got %d", n)
	}
}

func TestPaychanE2E_Refund_HappyPath(t *testing.T) {
	cli := spinUpChannelServer(t, &scriptedAdapter{name: "gcash"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := cli.Refund(ctx, &channelv1.RefundRequest{
		Adapter:        "gcash",
		PiId:           "pi_4471234560001",
		ExternalRefNo:  "gcash_pi_4471234560001",
		Amount:         5000,
		IdempotencyKey: "rfd-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != "succeeded" {
		t.Fatalf("want succeeded refund got %s", resp.GetResult())
	}
}

func TestPaychanE2E_Query_HappyPath(t *testing.T) {
	cli := spinUpChannelServer(t, &scriptedAdapter{name: "gcash"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := cli.Query(ctx, &channelv1.QueryRequest{
		Adapter:       "gcash",
		PiId:          "pi_4471234560001",
		ExternalRefNo: "ref_x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != "succeeded" {
		t.Fatalf("want succeeded got %s", resp.GetResult())
	}
}
