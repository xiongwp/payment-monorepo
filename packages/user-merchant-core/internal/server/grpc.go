// Package server gRPC 适配层。
package server

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"

	"github.com/xiongwp/payment-util/shadow"
	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"
	"github.com/xiongwp/user-merchant-core/internal/cache"
	"github.com/xiongwp/user-merchant-core/internal/repo"
	"github.com/xiongwp/user-merchant-core/internal/service"
	"github.com/xiongwp/user-merchant-core/internal/trace"
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

// ListenAndServe 启动 gRPC（阻塞）
func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	addr := fmt.Sprintf(":%d", port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			RecoverInterceptor(s.logger),
			// OTel 先于 tracex：OTel 先建 span，tracex 从 span 里抽 trace_id 作为
			// log 字段；没配 OTel 就是 no-op 拦截器，tracex 退回自生 id。
			tracexOTelInterceptor(),
			trace.UnaryServerInterceptor(s.logger),
			// Shadow 标识：把 metadata x-shadow 翻进 ctx，后续 repo / 出站 client 自动按 ctx 选主/影路径。
			shadow.UnaryServerInterceptor(),
			// Timeout 在日志之前注入：所有后续拦截器 + handler 都能拿到新 ctx，
			// 超时后 handler goroutine 里的 DB / 下游 RPC 会立即被取消。
			grpcutil.TimeoutInterceptor(s.timeouts),
			LoggingInterceptor(s.logger, LoggingOptions{
				// 热路径（AuthenticateByAPIKey / Get）跳过 body 渲染，
				// 其余方法按 2KB 默认截断。
				SkipMethods: map[string]struct{}{
					"/usermerchant.v1.MerchantService/AuthenticateByAPIKey": {},
				},
			}),
			MetricsInterceptor(),
			// 请求体 size 限制：per-method，防 DoS。默认 4KB，Create/AddDocument 可以放宽。
			grpcutil.MsgSizeLimitInterceptor(grpcutil.MsgSizeLimits{
				Default: 4 * 1024,
				ByMethod: map[string]int{
					"/usermerchant.v1.MerchantService/Create":      64 * 1024, // metadata + KYC 初始
					"/usermerchant.v1.MerchantService/AddDocument": 16 * 1024,
					"/usermerchant.v1.MerchantService/BatchGet":    32 * 1024, // 500 ids × 64B
				},
			}),
			RateLimitInterceptor(s.rateLimit, s.rateBurst),
			grpcutil.PerKeyRateLimitInterceptor(s.perKey),
			// 每商户动态限流：resolver 解析 metadata → cache → merchant.rate_limit_rps。
			// resolver 返回空 key 或 rps=0（且无 DefaultRPS）都是 no-op；商户的 rate_limit_rps
			// 改了之后下一次桶重建自动生效，不需要 restart 服务。
			grpcutil.MerchantRateLimitInterceptor(grpcutil.MerchantRateLimitOptions{
				Resolver:   s.resolveMerchantLimit,
				DefaultRPS: s.merchantDefaultRPS,
			}),
			AuthInterceptor(s.authTokens, s.logger),
			// Idempotency：必须在 Auth 后面（先认 bearer，再决定要不要幂等）。
			// 仅对 mutation methods 生效，未传 header 直通。
			//
			// TTL 1h：原 24h 留下"凭 API key + 老幂等键 24h 内重放仍命中"的窗口；
			// 攻击者一旦泄露 token 可在一整天里复用旧请求。Stripe 给的是 24h
			// 但他们额外校验请求体哈希；我们没做请求体哈希，把 TTL 收紧到 1h
			// 是更保守的折中。
			grpcutil.IdempotencyInterceptor(grpcutil.IdempotencyOptions{
				HeaderName:   "idempotency-key",
				MethodFilter: s.mutationMethods,
				Store:        s.idempotencyStore,
				TTL:          time.Hour,
			}),
			// Audit：最接近 handler，这样 handler 的最终 status 才是被记录的。
			grpcutil.AuditInterceptor(grpcutil.AuditOptions{
				Store:         s.auditStore,
				MethodFilter:  s.mutationMethods,
				ActorFromCtx:  actorFromAuth,
				TargetFromReq: targetFromRequest,
				TraceIDHeader: trace.MetadataKey,
			}),
		),
		// keepalive: 内部服务间长连接；5 分钟没请求发 ping，15 秒没回就 GOAWAY。
		// EnforcementPolicy 限制客户端 ping 频率，防御 CPU 耗尽攻击。
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    5 * time.Minute,
			Timeout: 15 * time.Second,
		}),
		// MinTime=5s（之前 30s）：和 serviceregistry.hardenedKeepalive 的 client
		// Time=10s 配合，留 2× 裕度。原 30s 会把 monorepo 内统一升级后的 client
		// （serviceregistry.DialDirect 默认 10s ping）当 abuse 用 GOAWAY
		// "ENHANCE_YOUR_CALM/too_many_pings" 踢回，client 反复重连永远建不稳。
		// 防 CPU 耗尽攻击改靠 RateLimitInterceptor + 上游 LB，不再依赖 ping 频率。
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		// wave N: 请求体上限 4MB。merchant/secret 类请求数 KB 就够了，
		// 拉高只会帮到 DoS。这个值等 payment-admin-web 大批量导入时再调。
		grpc.MaxRecvMsgSize(4*1024*1024),
	)
	if s.merchantSvc != nil {
		usermerchantv1.RegisterMerchantServiceServer(gs, NewMerchantServer(s.merchantSvc))
	}
	if s.merchantSecretSvc != nil {
		usermerchantv1.RegisterMerchantSecretServiceServer(gs, NewMerchantSecretServer(s.merchantSecretSvc))
	}
	if s.auditRepo != nil {
		usermerchantv1.RegisterAuditServiceServer(gs, NewAuditServer(s.auditRepo))
	}
	if s.userSvc != nil {
		usermerchantv1.RegisterUserServiceServer(gs, NewUserServer(s.userSvc))
	}
	// PAN 单跳后：UserCardService gRPC 暴露给 api-gateway / order-core 内部调用
	if s.userCardSvc != nil {
		ucServer := NewUserCardServer(s.userCardSvc)
		usermerchantv1.RegisterUserCardServiceServer(gs, ucServer)
		// UserCardInternalService（GetStoredTokenForPayment）也注册到同一 listener；
		// 生产应该通过另一个 internal-only 端口 + 独立 mTLS clientCN 白名单暴露，
		// 当前 dev 暂复用 public listener，待独立 listener 切分后挪走。
		usermerchantv1.RegisterUserCardInternalServiceServer(gs, ucServer)
	}

	h := health.NewServer()
	h.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(gs, h)
	reflection.Register(gs)

	go func() { <-ctx.Done(); gs.GracefulStop() }()
	s.logger.Info("grpc listening", zap.String("addr", addr))
	return gs.Serve(lis)
}
