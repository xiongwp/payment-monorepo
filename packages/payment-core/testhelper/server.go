// Package testhelper 给其它仓库做端到端测试时"启动一个真实的 payment-core
// gRPC 服务器"用。路由规则可自定义，上游 payment-channel 由调用方传入
// bufconn 连接。
//
// 本包路径在 repo 根外（非 internal/），跨模块可以 import。
package testhelper

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
	paymentcorev1 "github.com/xiongwp/payment-core/api/proto/paymentcore/v1"
	"github.com/xiongwp/payment-core/internal/channelclient"
	"github.com/xiongwp/payment-core/internal/routing"
	"github.com/xiongwp/payment-core/internal/server"
	"github.com/xiongwp/payment-core/internal/service"
)

// Rule 是可跨模块使用的简化版路由规则（字段完全对齐 internal/routing.Rule）。
type Rule struct {
	Priority      int
	Merchant      string
	Country       string
	PaymentMethod string
	AmountMin     int64
	AmountMax     int64
	Adapter       string
}

// StartConfig 启动一个 payment-core Server 所需的最小配置。
type StartConfig struct {
	// ChannelConn 已建好、指向 payment-channel gRPC（bufconn 或真实）的连接。
	ChannelConn *grpc.ClientConn
	// Rules 路由规则集。空则什么都不路由（测试多半会失败，建议至少一条兜底）。
	Rules []Rule
	// RPCTimeout 调下游 payment-channel 的超时；默认 10s。
	RPCTimeout time.Duration
}

// Server 是启动后的 payment-core 句柄。
type Server struct {
	Client   paymentcorev1.PaymentCoreServiceClient
	Conn     *grpc.ClientConn
	Listener *bufconn.Listener
	cleanup  func()
}

// Stop 优雅停止。
func (s *Server) Stop() { s.cleanup() }

// Start 启动真实的 payment-core 服务（真 routing + service + server 代码），
// 监听一个 bufconn，返回一个已 dialed 的 gRPC client。t 接受 *testing.T / *testing.B。
func Start(t testing.TB, cfg StartConfig) *Server {
	t.Helper()
	logger := zap.NewNop()

	if cfg.ChannelConn == nil {
		t.Fatal("StartConfig.ChannelConn is required")
	}
	if cfg.RPCTimeout == 0 {
		cfg.RPCTimeout = 10 * time.Second
	}

	router := routing.NewRouter(toInternalRules(cfg.Rules))
	channelCli := &bufconnChannelClient{api: channelv1.NewAcquirerServiceClient(cfg.ChannelConn)}
	paymentSvc := service.NewPaymentService(router, channelCli, nil, logger)
	webhookSvc := service.NewWebhookService(logger)
	handler := server.NewServer(server.Deps{
		PaymentSvc: paymentSvc,
		WebhookSvc: webhookSvc,
		Logger:     logger,
	})

	lis := bufconn.Listen(1 << 20)
	grpcSrv := grpc.NewServer()
	paymentcorev1.RegisterPaymentCoreServiceServer(grpcSrv, handler)
	go func() {
		if err := grpcSrv.Serve(lis); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Logf("payment-core testhelper serve: %v", err)
		}
	}()

	conn, err := grpc.NewClient("passthrough://paymentcore",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_ = conn.Close()
		grpcSrv.GracefulStop()
		_ = lis.Close()
	}
	t.Cleanup(cleanup)
	return &Server{
		Client:   paymentcorev1.NewPaymentCoreServiceClient(conn),
		Conn:     conn,
		Listener: lis,
		cleanup:  cleanup,
	}
}

// ─── 内部：导出 rule → internal rule ─────────────────────────

func toInternalRules(rs []Rule) []routing.Rule {
	out := make([]routing.Rule, len(rs))
	for i, r := range rs {
		out[i] = routing.Rule{
			Priority:      r.Priority,
			Merchant:      r.Merchant,
			Country:       r.Country,
			PaymentMethod: r.PaymentMethod,
			AmountMin:     r.AmountMin,
			AmountMax:     r.AmountMax,
			Adapter:       r.Adapter,
		}
	}
	return out
}

// bufconnChannelClient 把 channelv1 client 包成 channelclient.Client 接口。
type bufconnChannelClient struct{ api channelv1.AcquirerServiceClient }

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
func (c *bufconnChannelClient) Close() error { return nil }

var _ channelclient.Client = (*bufconnChannelClient)(nil)
