package server

import (
	"context"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	cardpaymentv1 "reconcile-system/packages/card-payment/kitex_gen/cardpayment/v1"

	"github.com/xiongwp/card-payment/internal/processor"
)

// Server 实现 Kitex cardpaymentservice.Server 接口 (跟 gRPC 同方法签名).
// 切 Kitex 后不再 embed UnimplementedCardPaymentServer.
type Server struct {
	proc   *processor.Processor
	logger *zap.Logger
}

func NewServer(p *processor.Processor, logger *zap.Logger) *Server {
	return &Server{proc: p, logger: logger}
}

// Register no-op 兼容老接口; Kitex cmd/server/main.go 在构造时即完成 service 注册.
func (s *Server) Register() {}

// Authorize 入口
func (s *Server) Authorize(ctx context.Context, req *cardpaymentv1.AuthorizeRequest) (*cardpaymentv1.AuthorizeResponse, error) {
	if req.GetPaymentToken() == "" || req.GetPiId() == "" {
		return nil, status.Error(codes.InvalidArgument, "payment_token / pi_id required")
	}
	in := &processor.AuthorizeInput{
		PaymentToken:       req.GetPaymentToken(),
		PIID:               req.GetPiId(),
		Amount:             req.GetAmount(),
		Currency:           req.GetCurrency(),
		Network:            req.GetNetwork(),
		MerchantDescriptor: req.GetMerchantDescriptor(),
		IdempotencyKey:     "",
	}
	if td := req.GetThreeDsData(); td != nil {
		in.ThreeDS = &processor.ThreeDSData{
			Version:   td.GetVersion(),
			ECI:       td.GetEci(),
			CAVV:      td.GetCavv(),
			XID:       td.GetXid(),
			DSTransID: td.GetDsTransId(),
		}
	}
	out, err := s.proc.Authorize(ctx, in)
	if err != nil {
		return nil, status.Error(codes.Internal, "authorize failed")
	}
	return &cardpaymentv1.AuthorizeResponse{
		NetworkRefNo:  out.NetworkRefNo,
		Status:        out.Status,
		DeclineCode:   out.DeclineCode,
		DeclineReason: out.DeclineReason,
		MaskedPan:     out.MaskedPAN,
		Network:       out.Network,
		Arn:           out.ARN,
	}, nil
}

// Capture / Refund / Void / Query 暂用 Unimplemented；processor 后续扩展。
func (s *Server) Capture(ctx context.Context, req *cardpaymentv1.CaptureRequest) (*cardpaymentv1.CaptureResponse, error) {
	return nil, status.Error(codes.Unimplemented, "capture not implemented yet")
}
func (s *Server) Refund(ctx context.Context, req *cardpaymentv1.RefundRequest) (*cardpaymentv1.RefundResponse, error) {
	return nil, status.Error(codes.Unimplemented, "refund not implemented yet")
}
func (s *Server) Void(ctx context.Context, req *cardpaymentv1.VoidRequest) (*cardpaymentv1.VoidResponse, error) {
	return nil, status.Error(codes.Unimplemented, "void not implemented yet")
}
func (s *Server) Query(ctx context.Context, req *cardpaymentv1.QueryRequest) (*cardpaymentv1.QueryResponse, error) {
	return nil, status.Error(codes.Unimplemented, "query not implemented yet")
}

// ─── Client CN allowlist interceptor ──────────────────────────────────────

type ClientCNAllowList map[string]struct{}

func NewClientCNAllowList(cns []string) ClientCNAllowList {
	out := make(ClientCNAllowList, len(cns))
	for _, cn := range cns {
		out[cn] = struct{}{}
	}
	return out
}

// PeerCN 取 mTLS client CN
func PeerCN(ctx context.Context) (string, string) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", ""
	}
	ip := p.Addr.String()
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", ip
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return "", ip
	}
	return tlsInfo.State.PeerCertificates[0].Subject.CommonName, ip
}

// UnaryClientCNInterceptor 拒绝不在白名单的客户端
func UnaryClientCNInterceptor(allow ClientCNAllowList) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		// health check / reflection 跳过
		if strings.HasPrefix(info.FullMethod, "/grpc.health.") ||
			strings.HasPrefix(info.FullMethod, "/grpc.reflection.") {
			return handler(ctx, req)
		}
		cn, _ := PeerCN(ctx)
		if cn == "" {
			return nil, status.Error(codes.Unauthenticated, "missing client cert CN")
		}
		if _, ok := allow[cn]; !ok {
			return nil, status.Errorf(codes.PermissionDenied, "CN %q not allowed", cn)
		}
		return handler(ctx, req)
	}
}
