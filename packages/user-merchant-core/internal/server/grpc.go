// Package server Kitex 适配层 (multi-service, 6 services 注册一个端口).
package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"

	kitexserver "github.com/cloudwego/kitex/server"
	"github.com/xiongwp/payment-util/kitexutil"
	"go.uber.org/zap"
	usermerchantv1 "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1"
	auditservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/auditservice"
	merchantsecretservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/merchantsecretservice"
	merchantservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/merchantservice"
	usercardinternalservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/usercardinternalservice"
	usercardservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/usercardservice"
	userservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/userservice"

	"github.com/xiongwp/user-merchant-core/internal/auditstore"
	"github.com/xiongwp/user-merchant-core/internal/cache"
	"github.com/xiongwp/user-merchant-core/internal/repo"
	"github.com/xiongwp/user-merchant-core/internal/service"
)

// Server 汇聚 merchant + merchant secret + user 6 个 Kitex service 适配层.
// 老 gRPC interceptor 配置 (PerKey/Timeouts/RateLimit/AuthTokens 等) 已删 —
// Kitex 切换后中间件通过 server.WithMiddleware 接入, 不再走 server 结构体字段.
type Server struct {
	merchantSvc        service.MerchantService
	merchantSecretSvc  service.MerchantSecretService
	userSvc            *service.UserService
	userCardSvc        *service.UserCardService // 卡 (UserCardService gRPC) — 接入 PAN 单跳后的 stored_token
	auditRepo          repo.AuditRepository
	merchantCache      *cache.MerchantCache
	merchantDefaultRPS float64
	idempotencyStore   auditstore.IdempotencyStore // 业务路径 (Service 层) 自取做幂等; 不再做 interceptor 兜底
	mutationMethods    map[string]struct{}
	auditStore         auditstore.AuditStore
	logger             *zap.Logger
}

// Deps gRPC server 的依赖
type Deps struct {
	MerchantSvc        service.MerchantService
	MerchantSecretSvc  service.MerchantSecretService
	UserSvc            *service.UserService
	UserCardSvc        *service.UserCardService
	AuditRepo          repo.AuditRepository
	MerchantCache      *cache.MerchantCache
	MerchantDefaultRPS float64
	// Idempotency / Audit: 业务路径自取.
	IdempotencyStore auditstore.IdempotencyStore
	MutationMethods  map[string]struct{}
	AuditStore       auditstore.AuditStore
	Logger           *zap.Logger
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
		idempotencyStore:   d.IdempotencyStore,
		mutationMethods:    d.MutationMethods,
		auditStore:         d.AuditStore,
		logger:             d.Logger,
	}
}

// resolveMerchantLimit 已删 — 老 gRPC MerchantLimitResolver 实现, 0 caller.
// Kitex 切换后 MerchantRateLimit MW 需重新设计 (从 TTHeader metainfo 读
// x-merchant-id / Authorization), 由 kitexutil 提供.

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
	// etcd 自注册 — REGISTRY_ENDPOINTS env 非空时生效, 注册到 "user-merchant-core" 名下.
	advHost := os.Getenv("ADVERTISE_HOST")
	if advHost == "" {
		advHost = "user-merchant-core"
	}
	srvOpts := []kitexserver.Option{kitexserver.WithServiceAddr(addr)}
	srvOpts = append(srvOpts, kitexutil.DefaultServerOptions("user-merchant-core", fmt.Sprintf("%s:%d", advHost, port))...)
	gs := kitexserver.NewServer(srvOpts...)

	// MULTISVC: 6 个 service 共用 9191 端口. 多个 service 都有 List 方法
	// (Merchant.List / User.List / Audit.List / UserCard.List), Kitex 启动期
	// 检测到方法冲突会 ERROR exit: "method name [List] is conflicted between
	// services but no fallback service is specified".
	// 解法: 一个 service 用 WithFallbackService() 标 fallback. 选 merchantservice
	// 作主 service. 当 client 的 metadata 里没带 service-name (旧 wire 或某些 mux 路径)
	// 时, Kitex 默认路由到 fallback. 现代 client 带 service-name 时仍按名字路由.
	if s.merchantSvc != nil {
		merchantservice.RegisterService(gs, NewMerchantServer(s.merchantSvc), kitexserver.WithFallbackService())
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
	// P0-PCI-1: UserCardInternalService 拆到独立 internal-only listener,
	// 不跟公开 service 共端口 (防横向越权: GetStoredTokenForPayment 返存储 token,
	// 只能让 order-core / api-gateway 在内部网络调).
	// 类型用 handler interface (usermerchantv1.UserCardInternalService) —
	// kitex_gen/usercardinternalservice 包只导出 NewServer/RegisterService 函数,
	// 没有 Server 类型. UserCardServer 实现该 interface (GetStoredTokenForPayment).
	var ucInternal usermerchantv1.UserCardInternalService
	if s.userCardSvc != nil {
		ucServer := NewUserCardServer(s.userCardSvc)
		usercardservice.RegisterService(gs, ucServer)
		ucInternal = ucServer // 内部 service 用同 handler, 但不在公开端口注册
	}

	// 健康检查 / reflection 由 Kitex 自带, 不再手动注册.

	// 启动 internal-only listener (P0-PCI-1): 默认 :9192, env INTERNAL_GRPC_PORT 可改.
	// 仅 UserCardInternalService 暴露在这个端口; etcd 注册名 "user-merchant-core-internal"
	// 让 caller 显式区分 — order-core 拨号 stored token 用 internal 名,
	// 其他公开 RPC 拨 user-merchant-core. clientCN 白名单 / mTLS 在 ingress 层做.
	if ucInternal != nil {
		go s.serveInternal(ctx, ucInternal)
	}

	go func() { <-ctx.Done(); _ = gs.Stop() }()
	s.logger.Info("user-merchant-core Kitex listening", zap.String("addr", addr.String()))
	return gs.Run()
}

// serveInternal 启动 internal-only Kitex listener (UserCardInternalService 独占).
// 拆出来防止跟公开 6 个 service 共用端口 → PCI 横向越权.
func (s *Server) serveInternal(ctx context.Context, ucInternal usermerchantv1.UserCardInternalService) {
	intPort := 9192
	if v := os.Getenv("INTERNAL_GRPC_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			intPort = p
		}
	}
	addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", intPort))
	if err != nil {
		s.logger.Error("internal listener resolve failed", zap.Error(err))
		return
	}
	advHost := os.Getenv("ADVERTISE_HOST")
	if advHost == "" {
		advHost = "user-merchant-core"
	}
	srvOpts := []kitexserver.Option{kitexserver.WithServiceAddr(addr)}
	// 单独 etcd 注册名: 让 caller 显式选 internal endpoint, 公开 caller 拨不到.
	srvOpts = append(srvOpts,
		kitexutil.DefaultServerOptions("user-merchant-core-internal",
			fmt.Sprintf("%s:%d", advHost, intPort))...)
	gs := kitexserver.NewServer(srvOpts...)
	usercardinternalservice.RegisterService(gs, ucInternal)

	go func() { <-ctx.Done(); _ = gs.Stop() }()
	s.logger.Info("user-merchant-core INTERNAL Kitex listening (PCI restricted)",
		zap.String("addr", addr.String()),
		zap.Int("port", intPort),
		zap.String("etcd_name", "user-merchant-core-internal"))
	if err := gs.Run(); err != nil {
		s.logger.Error("internal Kitex Run exited", zap.Error(err))
	}
}
