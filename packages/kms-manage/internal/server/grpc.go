// Package server 把 service 层挂到 kmsv1.KMSServiceServer 上。
package server

import (
	"context"
	"fmt"
	"net"

	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	kmsv1 "github.com/xiongwp/kms-manage/api/proto/kms/v1"
	"github.com/xiongwp/kms-manage/internal/service"
)

type Server struct {
	kmsv1.UnimplementedKMSServiceServer

	svc    *service.KMSService
	auth   map[string]string
	rps    float64
	burst  int
	logger *zap.Logger
}

type Deps struct {
	KMSSvc       *service.KMSService
	AuthTokens   map[string]string
	RateLimitRPS float64
	RateBurst    int
	Logger       *zap.Logger
}

func NewServer(d Deps) *Server {
	return &Server{
		svc:    d.KMSSvc,
		auth:   d.AuthTokens,
		rps:    d.RateLimitRPS,
		burst:  d.RateBurst,
		logger: d.Logger,
	}
}

// ListenAndServe 开 gRPC 监听。ctx 关闭时 GracefulStop。
//
// **TODO(P1-5 长期，需要 ops 配合)**：升级到 mTLS / SPIFFE workload identity。
// 当前 AuthInterceptor 只用静态 Bearer token，宿主机 / k8s pod 拿到 token 文件
// 都能调 Decrypt / GenerateDataKey 解密任意业务密文。生产期目标：
//   - server-side: 用 grpc.Creds(credentials.NewTLS(...)) 替代默认 insecure
//   - client-side: 每个调用方 service 持自己的 client cert（cert-manager 自动续期）
//   - AuthInterceptor 改读 peer.FromContext + tls.ConnectionState.PeerCertificates
//     → 校验 cert 的 spiffe:// URI 与白名单的 service identity 匹配
// 切换前需要先在 staging 跑通端到端 mTLS 链路，runbook 见 docs/KMS_MTLS.md。
func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(
		RecoverInterceptor(s.logger),
		trace.UnaryServerInterceptor(s.logger), // 从 metadata 取 x-trace-id 注入 ctx/logger
		LoggingInterceptor(s.logger),
		MetricsInterceptor(),
		RateLimitInterceptor(s.rps, s.burst),
		AuthInterceptor(s.auth, s.logger),
	))
	kmsv1.RegisterKMSServiceServer(srv, s)
	s.logger.Info("kms-manage grpc listening", zap.Int("port", port))
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	return srv.Serve(lis)
}

// ─── RPC handlers ──────────────────────────────

func (s *Server) Encrypt(ctx context.Context, req *kmsv1.EncryptRequest) (*kmsv1.EncryptResponse, error) {
	if len(req.GetPlaintext()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "plaintext required")
	}
	out, err := s.svc.Encrypt(ctx, service.EncryptIn{
		KeyID:     req.GetKeyId(),
		Plaintext: req.GetPlaintext(),
		Context:   req.GetContext(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &kmsv1.EncryptResponse{Ciphertext: out.Ciphertext, KeyId: out.KeyID}, nil
}

func (s *Server) Decrypt(ctx context.Context, req *kmsv1.DecryptRequest) (*kmsv1.DecryptResponse, error) {
	if req.GetCiphertext() == "" {
		return nil, status.Error(codes.InvalidArgument, "ciphertext required")
	}
	if req.GetContext() == "" {
		return nil, status.Error(codes.InvalidArgument, "context (AAD) required for decrypt — use 'svc:<service>:<field>' format")
	}
	out, err := s.svc.Decrypt(ctx, service.DecryptIn{
		Ciphertext: req.GetCiphertext(),
		Context:    req.GetContext(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &kmsv1.DecryptResponse{Plaintext: out.Plaintext, KeyId: out.KeyID}, nil
}

func (s *Server) GenerateDataKey(ctx context.Context, req *kmsv1.GenerateDataKeyRequest) (*kmsv1.GenerateDataKeyResponse, error) {
	out, err := s.svc.GenerateDataKey(ctx, service.GenerateDataKeyIn{
		KeyID:   req.GetKeyId(),
		Context: req.GetContext(),
		Bytes:   int(req.GetBytes()),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &kmsv1.GenerateDataKeyResponse{
		PlaintextKey: out.Plaintext,
		EncryptedKey: out.Encrypted,
		KeyId:        out.KeyID,
	}, nil
}

func (s *Server) DescribeKey(_ context.Context, req *kmsv1.DescribeKeyRequest) (*kmsv1.DescribeKeyResponse, error) {
	m, ok := s.svc.DescribeKey(req.GetKeyId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "key %q not found", req.GetKeyId())
	}
	_, active := s.svc.ListKeys()
	return &kmsv1.DescribeKeyResponse{
		KeyId:     m.ID,
		Algorithm: m.Algorithm,
		CreatedAt: m.CreatedAt.Unix(),
		Active:    m.ID == active,
	}, nil
}

func (s *Server) ListKeys(_ context.Context, _ *kmsv1.ListKeysRequest) (*kmsv1.ListKeysResponse, error) {
	metas, active := s.svc.ListKeys()
	out := &kmsv1.ListKeysResponse{ActiveKeyId: active}
	for _, m := range metas {
		out.Keys = append(out.Keys, &kmsv1.DescribeKeyResponse{
			KeyId:     m.ID,
			Algorithm: m.Algorithm,
			CreatedAt: m.CreatedAt.Unix(),
			Active:    m.ID == active,
		})
	}
	return out, nil
}

// ─── helpers ────────────────────────────────

func toStatus(err error) error {
	if err == nil {
		return nil
	}
	return status.Error(codes.InvalidArgument, err.Error())
}
