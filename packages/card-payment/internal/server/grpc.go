package server

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	cardpaymentv1 "github.com/xiongwp/card-payment/kitex_gen/cardpayment/v1"

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
		return nil, fmt.Errorf("payment_token / pi_id required")
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
		return nil, fmt.Errorf("authorize failed")
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

// Capture / Refund / Void / Query 暂用 Unimplemented;processor 后续扩展。
func (s *Server) Capture(ctx context.Context, req *cardpaymentv1.CaptureRequest) (*cardpaymentv1.CaptureResponse, error) {
	return nil, fmt.Errorf("capture not implemented yet")
}
func (s *Server) Refund(ctx context.Context, req *cardpaymentv1.RefundRequest) (*cardpaymentv1.RefundResponse, error) {
	return nil, fmt.Errorf("refund not implemented yet")
}
func (s *Server) Void(ctx context.Context, req *cardpaymentv1.VoidRequest) (*cardpaymentv1.VoidResponse, error) {
	return nil, fmt.Errorf("void not implemented yet")
}
func (s *Server) Query(ctx context.Context, req *cardpaymentv1.QueryRequest) (*cardpaymentv1.QueryResponse, error) {
	return nil, fmt.Errorf("query not implemented yet")
}

// ─── Client CN allowlist (data-only stub) ──────────────────────────────────
//
// 历史: 这里曾持有 PeerCN + UnaryClientCNInterceptor (gRPC mTLS). Kitex 切换 +
// mTLS 废弃后, interceptor 0 caller, 已删. 仅保留 ClientCNAllowList 数据结构
// + NewClientCNAllowList ctor — 等 kitexutil.MTLSClientCNMW 实现后接回.

type ClientCNAllowList map[string]struct{}

func NewClientCNAllowList(cns []string) ClientCNAllowList {
	out := make(ClientCNAllowList, len(cns))
	for _, cn := range cns {
		out[cn] = struct{}{}
	}
	return out
}
