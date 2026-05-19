// Package server Kitex 适配层 (multi-service, 6 services 注册一个端口).
package server

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	kitexserver "github.com/cloudwego/kitex/server"
	"go.uber.org/zap"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"

	auditservice "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1/auditservice"
	merchantsecretservice "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1/merchantsecretservice"
	merchantservice "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1/merchantservice"
	usercardinternalservice "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1/usercardinternalservice"
	usercardservice "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1/usercardservice"
	userservice "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1/userservice"

	"github.com/xiongwp/user-merchant-core/internal/cache"
	"github.com/xiongwp/user-merchant-core/internal/repo"
	"github.com/xiongwp/user-merchant-core/internal/service"
	"github.com/xiongwp/user-merchant-core/pkg/grpcutil"
)

// Server 汇聚 merchant + merchant secret + user gRPC 适配层。
type Server struct {
	merchantSvc        service.MerchantService
	merchantSecretSvc  service.MerchantSecretService
	userSvc            *service.UserService
	userCardSvc        *service.UserCardService // 卡 (UserCardService gRPC) — 接入 PAN 单跳后的 stored_token
	auditRepo          repo.AuditRepository
	merchantCache      *cache.MerchantCache
	merchantDefaultRPS float64
	authTokens         map[string]string
	rateLimit          float64
	rateBurst          int
	perKey             grpcutil.PerKeyLimitOptions
	timeouts           grpcutil.TimeoutConfig
	idempotencyStore   grpcutil.IdempotencyStore
	mutationMethods    map[string]struct{}
	auditStore         grpcutil.AuditStore
	logger             *zap.Logger
}

// Deps gRPC server 的依赖
type Deps struct {
	MerchantSvc       service.MerchantService
	MerchantSecretSvc service.MerchantSecretService
	UserSvc           *service.UserService
	UserCardSvc       *service.UserCardService
	AuditRepo         repo.AuditRepository
	// MerchantCache 用来在限流 resolver 里快速查商户配置；nil 时自动退化
	// 到全局 DefaultRPS。
	MerchantCache    *cache.MerchantCache
	MerchantDefaultRPS float64
	// 可选：interceptor 配置
	AuthTokens   map[string]string // 非空开启 bearer 鉴权
	RateLimitRPS float64           // 全局限流 0 关闭
	RateBurst    int
	// Per-key 限流：按 metadata header 分桶，防某商户滥用影响他人。
	PerKey       grpcutil.PerKeyLimitOptions
	// 每 RPC 默认 context timeout；0 关闭
	Timeouts     grpcutil.TimeoutConfig
	// Idempotency：Stripe 风格 Idempotency-Key header；仅对 MutationMethods 启用。
	IdempotencyStore   grpcutil.IdempotencyStore
	MutationMethods    map[string]struct{}
	// Audit：每个 mutation 写 append-only 链式日志。
	AuditStore grpcutil.AuditStore
	Logger     *zap.Logger
}

// NewServer 构造
func NewServer(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = zap.NewNop()
	}
	return &Server{
		merchantSvc:        d.MerchantSvc,
		merchantSecretSvc:  d.MerchantSecretSvc,
		userSvc:            d.UserSvc,
		userCardSvc:        d.UserCardSvc,
		auditRepo:          d.AuditRepo,
		merchantCache:      d.MerchantCache,
		merchantDefaultRPS: d.MerchantDefaultRPS,
		authTokens:         d.AuthTokens,
		rateLimit:          d.RateLimitRPS,
		rateBurst:          d.RateBurst,
		perKey:             d.PerKey,
		timeouts:           d.Timeouts,
		idempotencyStore:   d.IdempotencyStore,
		mutationMethods:    d.MutationMethods,
		auditStore:         d.AuditStore,
		logger:             d.Logger,
	}
}

// resolveMerchantLimit —— MerchantLimitResolver 的本服务实现。
//
// 解析顺序：
//   1. metadata["x-merchant-id"]：显式携带（admin-backend 代理时填）
//   2. Authorization: Bearer sk_live_/sk_test_xxx → hash → MerchantCache 反查
//
// 拿到 merchant.ID 之后读 RateLimitRPS；字段为 0 走 DefaultRPS。
// 不会回源 DB —— 限流不值得为此多一次 RPC；warmup 覆盖了大部分活跃商户。
func (s *Server) resolveMerchantLimit(ctx context.Context, _ string) (string, float64) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", 0
	}
	// Path 1: explicit header
	if vals := md.Get("x-merchant-id"); len(vals) > 0 && vals[0] != "" {
		id := vals[0]
		if s.merchantCache != nil {
			if m, ok := s.merchantCache.GetByID(id); ok {
				return id, float64(m.RateLimitRPS)
			}
		}
		return id, 0 // cache miss → 走 DefaultRPS
	}
	// Path 2: bearer API key
	auth := strings.TrimSpace(strings.Join(md.Get("authorization"), ""))
	if !strings.HasPrefix(auth, "Bearer ") || s.merchantCache == nil {
		return "", 0
	}
	// 这里直接用 service.hashKey 太重；限流路径不回 DB，只查 cache：
	// LookupByKeyHash 入参是 sha256(plaintext)。共享函数单独抽在 pkg/grpcutil 里太跨层，
	// 这里 inline 一个 sha256 避免对 service 包产生反向依赖。
	h := sha256Hex(strings.TrimPrefix(auth, "Bearer "))
	if m, ok := s.merchantCache.LookupByKeyHash(h); ok {
		return m.ID, float64(m.RateLimitRPS)
	}
	return "", 0
}

// ListenAndServe 启动 Kitex multi-service server (阻塞).
//
// 注册 6 个 service 到同一端口 (Kitex 0.10+ MultiService):
//   MerchantService / MerchantSecretService / AuditService / UserService /
//   UserCardService / UserCardInternalService
//
// TODO: kitexutil MW (Recover/OTel/Trace/Shadow/Timeout/Logging/Metrics/MsgSizeLimit/
// RateLimit/PerKeyRateLimit/MerchantRateLimit/Auth/Idempotency/Audit 共 14 条) 全标 TODO
// 待 kitexutil port 完成后接 server.WithMiddleware(...).
func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("resolve :%d: %w", port, err)
	}
	gs := kitexserver.NewServer(kitexserver.WithServiceAddr(addr))

	if s.merchantSvc != nil {
		merchantservice.RegisterService(gs, NewMerchantServer(s.merchantSvc))
	}
	if s.merchantSecretSvc != nil {
		merchantsecretservice.RegisterService(gs, NewMerchantSecretServer(s.merchantSecretSvc))
	}
	if s.auditRepo != nil {
		auditservice.RegisterService(gs, NewAuditServer(s.auditRepo))
	}
	if s.userSvc != nil {
		userservice.RegisterService(gs, NewUserServer(s.userSvc))
	}
	// PAN 单跳后: UserCardService Kitex 暴露给 api-gateway / order-core 内部调用.
	if s.userCardSvc != nil {
		ucServer := NewUserCardServer(s.userCardSvc)
		usercardservice.RegisterService(gs, ucServer)
		// UserCardInternalService (GetStoredTokenForPayment) 同一 listener;
		// 生产应该通过另一个 internal-only 端口暴露 (TODO).
		usercardinternalservice.RegisterService(gs, ucServer)
	}

	// 健康检查 / reflection 由 Kitex 自带, 不再手动注册.
	_ = health.NewServer
	_ = grpc_health_v1.HealthCheckResponse_SERVING
	_ = reflection.Register

	go func() { <-ctx.Done(); _ = gs.Stop() }()
	s.logger.Info("user-merchant-core Kitex listening", zap.String("addr", addr.String()))
	return gs.Run()
}
