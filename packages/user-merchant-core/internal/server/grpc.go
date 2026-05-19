// Package server Kitex 适配层 (multi-service, 6 services 注册一个端口).
package server

import (
	"context"
	"fmt"
	"net"
	"os"

	kitexserver "github.com/cloudwego/kitex/server"
	"github.com/xiongwp/payment-util/kitexutil"
	"go.uber.org/zap"
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

	go func() { <-ctx.Done(); _ = gs.Stop() }()
	s.logger.Info("user-merchant-core Kitex listening", zap.String("addr", addr.String()))
	return gs.Run()
}
