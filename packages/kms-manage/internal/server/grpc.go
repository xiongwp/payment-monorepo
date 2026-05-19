// Package server 把 service 层挂到 kmsv1.KMSServiceServer 上。
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/xiongwp/payment-util/serviceregistry"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	kmsv1 "reconcile-system/packages/kms-manage/kitex_gen/kms/v1"
	"github.com/xiongwp/kms-manage/internal/service"
)

type Server struct {
	kmsv1.UnimplementedKMSServiceServer

	svc        *service.KMSService
	auth       map[string]string
	allowedIDs ClientIdentityAllowList
	tlsCfg     *tls.Config
	// rateLimiter 持有引用方便 SetRateLimit 热更新（config-center OnChange 调）。
	// nil = 不限流；rps <= 0 时构造为 nil。
	rateLimiter *rate.Limiter
	logger      *zap.Logger
}

// TLSPaths server 端 mTLS 配置。三个都必填才启用 mTLS；任一空 →
// 走 insecure listener（dev / 本地开发）。
type TLSPaths struct {
	ServerCert string
	ServerKey  string
	ClientCA   string // 校验调用方 cert 用，必填启用 RequireAndVerifyClientCert
}

type Deps struct {
	KMSSvc       *service.KMSService
	AuthTokens   map[string]string
	AllowedIDs   []string  // mTLS client cert CN / SAN 白名单
	TLS          TLSPaths  // 三个都填 → mTLS-only listener
	RateLimitRPS float64
	RateBurst    int
	Logger       *zap.Logger
}

// NewServer 装配 Server；TLS 路径任一缺省走 insecure（dev only）。
// prod 路径要求三件齐全 + AllowedIDs 至少 1 项，由 main.go assertProdSafety 拦。
func NewServer(d Deps) (*Server, error) {
	tlsCfg, err := buildServerTLS(d.TLS)
	if err != nil {
		return nil, err
	}
	var lim *rate.Limiter
	if d.RateLimitRPS > 0 {
		burst := d.RateBurst
		if burst <= 0 {
			burst = int(d.RateLimitRPS)
			if burst < 1 {
				burst = 1
			}
		}
		lim = rate.NewLimiter(rate.Limit(d.RateLimitRPS), burst)
	}
	return &Server{
		svc:         d.KMSSvc,
		auth:        d.AuthTokens,
		allowedIDs:  NewClientIdentityAllowList(d.AllowedIDs),
		tlsCfg:      tlsCfg,
		rateLimiter: lim,
		logger:      d.Logger,
	}, nil
}

// SetRateLimit 热更新 rps + burst（config-center OnChange 回调里调）。
func (s *Server) SetRateLimit(rps float64, burst int) {
	if rps <= 0 {
		s.rateLimiter = nil
		return
	}
	if burst <= 0 {
		burst = int(rps)
		if burst < 1 {
			burst = 1
		}
	}
	if s.rateLimiter == nil {
		s.rateLimiter = rate.NewLimiter(rate.Limit(rps), burst)
		return
	}
	s.rateLimiter.SetLimit(rate.Limit(rps))
	s.rateLimiter.SetBurst(burst)
}

// buildServerTLS 三件齐全 → 加载 cert + 信任 client CA + ClientAuth=Require。
// 任一空 → 返 nil 表示 insecure listener。
func buildServerTLS(p TLSPaths) (*tls.Config, error) {
	if p.ServerCert == "" || p.ServerKey == "" || p.ClientCA == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(p.ServerCert, p.ServerKey)
	if err != nil {
		return nil, fmt.Errorf("kms server keypair: %w", err)
	}
	pool := x509.NewCertPool()
	caBytes, err := os.ReadFile(p.ClientCA)
	if err != nil {
		return nil, fmt.Errorf("kms client CA: %w", err)
	}
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("kms client CA: PEM parse failed")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		// **关键**：必须验证调用方 cert，否则 mTLS 退化为单向 TLS，纵深防御=0。
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS12,
	}, nil
}

// ListenAndServe 开 gRPC 监听。ctx 关闭时 GracefulStop。
//
// **mTLS 双层鉴权（P0-4 完成）**：
//
//	层 1 — TLS 握手期：grpc.Creds(NewTLS) + ClientAuth=RequireAndVerifyClientCert
//	         调用方没合法 client cert → 握手期就被踢，进不来 interceptor。
//	层 2 — ClientIdentityInterceptor：cert 合法仍要 CN/SAN 命中白名单。
//	         即使 CA 误签了一个 cert，没在 allowed_client_ids 里照样 deny。
//	层 3 — AuthInterceptor (Bearer)：保留作 break-glass / 老客户兼容。
//
// 三层 AND，全过才放行 handler。
func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	// HardenedServerOptions 加 KeepaliveEnforcementPolicy{MinTime:5s, PermitWithoutStream:true}
	// —— 必须挂，否则配套 client（serviceregistry.DialDirect 默认 10s/3s ping）会被
	// grpc-go 默认 EnforcementPolicy{MinTime:5min, PermitWithoutStream:false} 当 abuse
	// 用 GOAWAY "ENHANCE_YOUR_CALM / too_many_pings" 踢回去，导致 client 反复重连永远建不稳。
	srvOpts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(
			RecoverInterceptor(s.logger),
			trace.UnaryServerInterceptor(s.logger), // 从 metadata 取 x-trace-id 注入 ctx/logger
			LoggingInterceptor(s.logger),
			MetricsInterceptor(),
			RateLimitInterceptor(s.rateLimiter),
			ClientIdentityInterceptor(s.allowedIDs, s.logger),
			AuthInterceptor(s.auth, s.logger),
		),
	}
	if s.tlsCfg != nil {
		srvOpts = append(srvOpts, grpc.Creds(credentials.NewTLS(s.tlsCfg)))
		s.logger.Info("kms-manage TLS enabled (mTLS RequireAndVerifyClientCert)")
	} else {
		s.logger.Warn("kms-manage running INSECURE (no TLS) — dev mode only")
	}
	srvOpts = append(srvOpts, serviceregistry.HardenedServerOptions()...)
	srv := grpc.NewServer(srvOpts...)
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
