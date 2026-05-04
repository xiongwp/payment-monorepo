package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/xiongwp/payment-util/shadow"
	putil "github.com/xiongwp/payment-util/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	riskv1 "github.com/xiongwp/risk-manage/api/proto/risk/v1"
	"github.com/xiongwp/risk-manage/internal/auth"
	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/metrics"
	"github.com/xiongwp/risk-manage/internal/reliability"
	"github.com/xiongwp/risk-manage/internal/service"
	"github.com/xiongwp/risk-manage/internal/store"
)

type Server struct {
	riskv1.UnimplementedRiskServiceServer
	svc             *service.RiskService
	bl              store.Blacklist
	auth            map[string]string
	apiKeys         auth.APIKeyStore             // nil = 不启用 per-merchant API key（兼容老 AuthTokens 模式）
	limiter         *reliability.MerchantLimiter // nil = 不限流（dev / 单测）
	shutdownTimeout time.Duration                // 0 = 默认 15s
	logger          *zap.Logger
}

type Deps struct {
	RiskSvc    *service.RiskService
	Blacklist  store.Blacklist
	AuthTokens map[string]string // 老的 flat-token 模式：通过校验 = ScopeInternal 全权
	APIKeys    auth.APIKeyStore  // 新模式：per-merchant API key + scope
	Limiter    *reliability.MerchantLimiter
	Logger     *zap.Logger
}

func NewServer(d Deps) *Server {
	return &Server{
		svc:     d.RiskSvc,
		bl:      d.Blacklist,
		auth:    d.AuthTokens,
		apiKeys: d.APIKeys,
		limiter: d.Limiter,
		logger:  d.Logger,
	}
}

func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			recoverInterceptor(s.logger),
			putil.UnaryServerInterceptor(s.logger), // 从 metadata 取 x-trace-id 注入 ctx/logger
			shadow.UnaryServerInterceptor(),        // 把 metadata x-shadow 翻进 ctx；后续 RPC handler 短路放行 shadow 流量
			loggingInterceptor(s.logger),
			metricsInterceptor(),
			authInterceptor(s.auth, s.apiKeys, s.logger),
		),
		// Keepalive 配置：让 server 主动检测 idle 客户端 + 拒绝过激 PING。
		// payment-core 走长连接 (HTTP/2 stream)；客户端死链 / 防火墙吃包时
		// 不发现的话连接句柄会泄漏。
		// MaxConnectionIdle: 5min 没流量就 GOAWAY 让 client 重连
		// Time / Timeout: 每 30s 主动 PING，10s 没回响就断
		// PermitWithoutStream: 允许客户端在没活跃 stream 时也保活
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           10 * time.Second,
		}),
		// EnforcementPolicy: 防客户端 keepalive 风暴 (DoS) — 至少 10s 间隔
		// 内同一连接不能 PING > 1 次，否则 server 主动断
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	riskv1.RegisterRiskServiceServer(srv, s)
	s.logger.Info("risk-manage grpc listening", zap.Int("port", port))
	go func() {
		<-ctx.Done()
		// Graceful shutdown with timeout：等 in-flight Screen 跑完再退；
		// 超 ShutdownTimeout 强制 Stop 防止部署窗口被卡死。
		// 默认 15s（payment-core 调 Screen 超时 3s，留 5x 余量；可调）。
		timeout := s.shutdownTimeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		done := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
			s.logger.Info("grpc graceful stop complete")
		case <-time.After(timeout):
			s.logger.Warn("grpc graceful stop timeout; forcing Stop",
				zap.Duration("timeout", timeout))
			srv.Stop()
		}
	}()
	return srv.Serve(lis)
}

// SetShutdownTimeout 配置 GracefulStop 等待 in-flight RPC 的最大时间。
// 默认 15s；超过强制 Stop。生产 0 = disabled (永久等)，仅 dev 用。
func (s *Server) SetShutdownTimeout(d time.Duration) {
	if s == nil {
		return
	}
	s.shutdownTimeout = d
}

// ─── RPC handlers ──────────────────────────────

// shadowAllowResponse 构造 shadow 流量的统一 ALLOW 响应。
//
// 压测流量在 risk-manage 这一步永远放行：不查黑名单、不查 Redis 频控、不调外部
// 反欺诈、不写 ML 训练样本。返回的 reason 用 "shadow:bypass" 让上游日志 / 看板
// 能区分（与真实 ALLOW 决策区分开）。
func shadowAllowResponse() *riskv1.ScreenResponse {
	return &riskv1.ScreenResponse{
		Decision:  riskv1.Decision_ALLOW,
		RiskScore: 0,
		RiskLevel: "shadow",
		Reason:    "shadow:bypass",
	}
}

func (s *Server) Screen(ctx context.Context, req *riskv1.ScreenRequest) (*riskv1.ScreenResponse, error) {
	// Shadow 短路：压测流量直接 ALLOW，不消耗任何风控资源、不污染统计。
	// 必须放在 RequireMerchantMatch 之前 — shadow 流量可能用通用压测租户身份，
	// merchant_id 校验对它不适用。
	if shadow.IsShadow(ctx) {
		metrics.ScreenTotal.WithLabelValues("shadow_bypass").Inc()
		return shadowAllowResponse(), nil
	}
	// Tenant 隔离：商户 key 调 Screen 必须 merchant_id 匹配；空时自动注入 principal 的 merchant_id。
	if err := auth.RequireMerchantMatch(ctx, req.GetMerchantId()); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	merchantID := auth.FillMerchantID(ctx, req.GetMerchantId())
	// per-merchant 限流：超额返 ResourceExhausted，order-core 应识别为可重试。
	if s.limiter != nil && !s.limiter.Allow(merchantID) {
		return nil, status.Errorf(codes.ResourceExhausted,
			"merchant %s exceeded screen QPS quota", merchantID)
	}
	resp, _ := s.screenOne(ctx, req, merchantID)
	return resp, nil
}

// screenOne 把 ScreenRequest → svc.Screen → ScreenResponse 抽出来给
// BulkScreen 复用。merchantID 已经鉴权过传进来；返 (resp, nil) 或 (nil, err)。
func (s *Server) screenOne(ctx context.Context, req *riskv1.ScreenRequest, merchantID string) (*riskv1.ScreenResponse, error) {
	txn := &engine.TxnContext{
		PaymentIntentID: req.GetPaymentIntentId(),
		MerchantID:      merchantID,
		CustomerID:      req.GetCustomerId(),
		Amount:          req.GetAmount(),
		Currency:        req.GetCurrency(),
		PaymentMethod:   req.GetPaymentMethod(),
		Country:         req.GetCountry(),
		IPAddress:       req.GetIpAddress(),
		DeviceID:        req.GetDeviceId(),
		UserAgent:       req.GetUserAgent(),
		RiskSessionID:   req.GetRiskSessionId(),
		IdempotencyKey:  req.GetIdempotencyKey(),
		EventType:       req.GetEventType(),
		FingerprintHash: req.GetFingerprintHash(),
		Metadata:        req.GetMetadata(),
	}
	start := time.Now()
	res := s.svc.Screen(ctx, txn)
	metrics.ScreenDuration.WithLabelValues().Observe(time.Since(start).Seconds())
	metrics.ScreenTotal.WithLabelValues(res.Decision.String()).Inc()

	resp := &riskv1.ScreenResponse{
		Decision:          toProtoDecision(res.Decision),
		RiskScore:         int32(res.RiskScore),
		RiskLevel:         res.RiskLevel,
		DecisionId:        res.DecisionID,
		RecommendedAction: res.RecommendedAction,
	}
	if res.Decision != engine.Allow {
		reasons := make([]string, 0, len(res.Hits))
		for _, h := range res.Hits {
			reasons = append(reasons, h.RuleName+": "+h.Detail)
		}
		resp.Reason = strings.Join(reasons, "; ")
	}
	for _, h := range res.Hits {
		resp.Hits = append(resp.Hits, &riskv1.RuleHit{
			RuleId:   h.RuleID,
			RuleName: h.RuleName,
			Decision: h.Decision.String(),
			Detail:   h.Detail,
		})
	}
	// shadow_hits 透出来给 caller 看"如果切 enforce 会触发什么"。
	// 不影响 decision / score；caller 应仅用于运营 dashboard / log，不要拿
	// 来做业务决策。
	for _, h := range res.ShadowHits {
		resp.ShadowHits = append(resp.ShadowHits, &riskv1.RuleHit{
			RuleId:   h.RuleID,
			RuleName: h.RuleName,
			Decision: h.Decision.String(),
			Detail:   h.Detail,
		})
	}
	return resp, nil
}

// bulkScreenMaxBatch 单次 BulkScreen 最大笔数（防 OOM / 单 RPC 拖死服务）。
const bulkScreenMaxBatch = 500

// bulkScreenConcurrency 服务端并发 worker 数（不超过 batch 大小）。
const bulkScreenConcurrency = 10

func (s *Server) BulkScreen(ctx context.Context, req *riskv1.BulkScreenRequest) (*riskv1.BulkScreenResponse, error) {
	in := req.GetRequests()
	if len(in) == 0 {
		return &riskv1.BulkScreenResponse{}, nil
	}
	if len(in) > bulkScreenMaxBatch {
		in = in[:bulkScreenMaxBatch]
	}
	// Shadow 短路：所有 row 都 ALLOW，不调底层 svc。和 Screen 单笔一致语义。
	if shadow.IsShadow(ctx) {
		metrics.ScreenTotal.WithLabelValues("shadow_bypass").Add(float64(len(in)))
		results := make([]*riskv1.BulkScreenResult, len(in))
		for i := range in {
			results[i] = &riskv1.BulkScreenResult{
				Index:    int32(i),
				Response: shadowAllowResponse(),
			}
		}
		return &riskv1.BulkScreenResponse{Results: results, TotalMs: 0}, nil
	}
	// principal 鉴权一次：批内每笔再 RequireMerchantMatch 防混租
	merchantID := auth.FillMerchantID(ctx, "")
	start := time.Now()
	results := make([]*riskv1.BulkScreenResult, len(in))
	sem := make(chan struct{}, bulkScreenConcurrency)
	var wg sync.WaitGroup
	for i, r := range in {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, r *riskv1.ScreenRequest) {
			defer wg.Done()
			defer func() { <-sem }()
			out := &riskv1.BulkScreenResult{Index: int32(i)}
			// Per-row tenant check：merchant_id 不为空时必须跟 principal 匹配
			if err := auth.RequireMerchantMatch(ctx, r.GetMerchantId()); err != nil {
				out.Error = "permission_denied: " + err.Error()
				results[i] = out
				return
			}
			rowMerchant := merchantID
			if r.GetMerchantId() != "" {
				rowMerchant = r.GetMerchantId()
			}
			// 批内不复用单笔限流（限流给单 RPC 流量场景；批接口走 batch 配额，
			// 后续可加 BulkLimiter，本次先简化）
			resp, err := s.screenOne(ctx, r, rowMerchant)
			if err != nil {
				out.Error = err.Error()
			} else {
				out.Response = resp
			}
			results[i] = out
		}(i, r)
	}
	wg.Wait()
	return &riskv1.BulkScreenResponse{
		Results: results,
		TotalMs: time.Since(start).Milliseconds(),
	}, nil
}

func (s *Server) Report(ctx context.Context, req *riskv1.ReportRequest) (*riskv1.ReportResponse, error) {
	// Shadow 短路：不写任何 outcome，避免压测样本污染 ML 训练 / 反馈环。
	if shadow.IsShadow(ctx) {
		// metric 单独打一个 label 区分 — 不计入正常 Report 总量
		// （ScreenTotal 复用即可：shadow 流量根本没经过 Screen，所以这里 noop log）
		return &riskv1.ReportResponse{}, nil
	}
	if err := auth.RequireMerchantMatch(ctx, req.GetMerchantId()); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	merchantID := auth.FillMerchantID(ctx, req.GetMerchantId())
	txn := &engine.TxnContext{
		PaymentIntentID: req.GetPaymentIntentId(),
		MerchantID:      merchantID,
		CustomerID:      req.GetCustomerId(),
		Amount:          req.GetAmount(),
		Currency:        req.GetCurrency(),
		PaymentMethod:   req.GetPaymentMethod(),
		IPAddress:       req.GetIpAddress(),
		DeviceID:        req.GetDeviceId(),
	}
	s.svc.Report(ctx, txn, req.GetEventType())
	return &riskv1.ReportResponse{}, nil
}

func (s *Server) ListRules(ctx context.Context, _ *riskv1.ListRulesRequest) (*riskv1.ListRulesResponse, error) {
	if err := requireNonMerchant(ctx); err != nil {
		return nil, err
	}
	rules := s.svc.Engine().Rules()
	resp := &riskv1.ListRulesResponse{}
	for _, r := range rules {
		resp.Rules = append(resp.Rules, &riskv1.Rule{
			Id:      r.ID(),
			Name:    r.Name(),
			Type:    r.Type(),
			Enabled: r.Enabled(),
		})
	}
	return resp, nil
}

func (s *Server) ReloadRules(ctx context.Context, _ *riskv1.ReloadRulesRequest) (*riskv1.ReloadRulesResponse, error) {
	if err := requireNonMerchant(ctx); err != nil {
		return nil, err
	}
	return &riskv1.ReloadRulesResponse{Loaded: int32(s.svc.Engine().RuleCount())}, nil
}

// requireNonMerchant 拒绝 ScopeMerchant 访问平台级 admin 接口（规则 / 黑名单 list）。
// 没鉴权（dev 模式）/ admin / internal 一律放行。
func requireNonMerchant(ctx context.Context) error {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil
	}
	if p.Scope == auth.ScopeMerchant {
		return status.Error(codes.PermissionDenied, "merchant scope cannot access admin endpoints")
	}
	return nil
}

// ─── Blacklist 管理 ──────────────────────────────

func (s *Server) AddBlacklist(ctx context.Context, req *riskv1.BlacklistEntryMsg) (*riskv1.BlacklistOpResponse, error) {
	if req.GetDimension() == "" || req.GetValue() == "" {
		return nil, status.Error(codes.InvalidArgument, "dimension and value required")
	}
	s.bl.Add(ctx, req.GetDimension(), req.GetValue(), req.GetReason())
	s.logger.Info("blacklist added",
		zap.String("dim", req.GetDimension()), zap.String("val", req.GetValue()))
	return &riskv1.BlacklistOpResponse{Ok: true}, nil
}

func (s *Server) RemoveBlacklist(ctx context.Context, req *riskv1.BlacklistEntryMsg) (*riskv1.BlacklistOpResponse, error) {
	if req.GetDimension() == "" || req.GetValue() == "" {
		return nil, status.Error(codes.InvalidArgument, "dimension and value required")
	}
	s.bl.Remove(ctx, req.GetDimension(), req.GetValue())
	s.logger.Info("blacklist removed",
		zap.String("dim", req.GetDimension()), zap.String("val", req.GetValue()))
	return &riskv1.BlacklistOpResponse{Ok: true}, nil
}

func (s *Server) ListBlacklist(ctx context.Context, req *riskv1.ListBlacklistRequest) (*riskv1.ListBlacklistResponse, error) {
	entries := s.bl.List(ctx, req.GetDimension())
	resp := &riskv1.ListBlacklistResponse{}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &riskv1.BlacklistEntryMsg{
			Dimension: e.Dimension,
			Value:     e.Value,
			Reason:    e.Reason,
		})
	}
	return resp, nil
}

// ErasePersonalData GDPR right-to-erasure。要 ScopeInternal（合规操作不让商户调）。
func (s *Server) ErasePersonalData(ctx context.Context, req *riskv1.EraseRequest) (*riskv1.EraseResponse, error) {
	if p, ok := auth.PrincipalFrom(ctx); !ok || p == nil || p.Scope != auth.ScopeInternal {
		return nil, status.Error(codes.PermissionDenied, "erase requires internal scope")
	}
	res, err := s.svc.ErasePersonalData(ctx, &service.EraseInput{
		CustomerID:  req.GetCustomerId(),
		DeviceID:    req.GetDeviceId(),
		IPAddress:   req.GetIpAddress(),
		Fingerprint: req.GetFingerprint(),
		EmailHash:   req.GetEmailHash(),
		PhoneHash:   req.GetPhoneHash(),
		Reason:      req.GetReason(),
		RequestedBy: req.GetRequestedBy(),
	})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &riskv1.EraseResponse{
		LinkEdgesPurged: int32(res.LinkEdgesPurged),
		CountersPurged:  int32(res.CountersPurged),
		FeaturesPurged:  int32(res.FeaturesPurged),
		AuditsMasked:    int32(res.AuditsMasked),
	}, nil
}

// ─── helpers ──────────────────────────────────

func toProtoDecision(d engine.Decision) riskv1.Decision {
	switch d {
	case engine.Allow:
		return riskv1.Decision_ALLOW
	case engine.Deny:
		return riskv1.Decision_DENY
	case engine.Review:
		return riskv1.Decision_REVIEW
	}
	return riskv1.Decision_ALLOW
}

// ─── interceptors (same pattern as other services) ────────────────

func recoverInterceptor(logger *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic", zap.String("method", info.FullMethod), zap.Any("recover", r), zap.String("stack", string(debug.Stack())))
				err = status.Errorf(codes.Internal, "internal panic")
			}
		}()
		return handler(ctx, req)
	}
}

func loggingInterceptor(logger *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if strings.HasPrefix(info.FullMethod, "/grpc.health.") {
			return handler(ctx, req)
		}
		start := time.Now()
		logger.Info("grpc IN", zap.String("method", info.FullMethod), zap.String("req", renderProto(req)))
		resp, err := handler(ctx, req)
		dur := time.Since(start)
		if err != nil {
			st, _ := status.FromError(err)
			logger.Warn("grpc OUT", zap.String("method", info.FullMethod), zap.Duration("dur", dur), zap.String("code", st.Code().String()), zap.String("err", err.Error()))
		} else {
			logger.Info("grpc OUT", zap.String("method", info.FullMethod), zap.Duration("dur", dur), zap.String("code", "OK"), zap.String("resp", renderProto(resp)))
		}
		return resp, err
	}
}

func metricsInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := codes.OK.String()
		if err != nil {
			if st, ok := status.FromError(err); ok {
				code = st.Code().String()
			}
		}
		metrics.GRPCRequestTotal.WithLabelValues(info.FullMethod, code).Inc()
		metrics.GRPCRequestDuration.WithLabelValues(info.FullMethod).Observe(time.Since(start).Seconds())
		return resp, err
	}
}

// authInterceptor 双模式：
//
//  1. apiKeys != nil 时优先走 per-merchant API key store，解析 → 注入 Principal 到 ctx；
//     不在 store 里再退到 legacyTokens（map）继续兼容；
//  2. legacyTokens 命中按 ScopeInternal 注入（payment-core 等内部服务）；
//  3. 都为空 = dev 模式，所有调用放行（无 Principal）。
//
// 下游 handler 通过 auth.PrincipalFrom(ctx) 决定 tenant 隔离 / 商户匹配。
func authInterceptor(legacyTokens map[string]string, apiKeys auth.APIKeyStore, logger *zap.Logger) grpc.UnaryServerInterceptor {
	skip := func(method string) bool {
		return strings.HasPrefix(method, "/grpc.health.") || strings.HasPrefix(method, "/grpc.reflection.")
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if skip(info.FullMethod) {
			return handler(ctx, req)
		}
		// 没启用任何鉴权 → dev 模式
		if len(legacyTokens) == 0 && apiKeys == nil {
			return handler(ctx, req)
		}
		md, _ := metadata.FromIncomingContext(ctx)
		hdr := strings.TrimSpace(strings.Join(md.Get("authorization"), ""))
		if !strings.HasPrefix(hdr, "Bearer ") {
			return nil, status.Error(codes.Unauthenticated, "missing bearer token")
		}
		tok := strings.TrimPrefix(hdr, "Bearer ")

		// 1) API key store
		if apiKeys != nil {
			if p, err := apiKeys.Lookup(ctx, tok); err == nil {
				ctx = auth.WithPrincipal(ctx, p)
				return handler(ctx, req)
			}
		}
		// 2) legacy flat-token
		if _, ok := legacyTokens[tok]; ok {
			ctx = auth.WithPrincipal(ctx, &auth.Principal{Scope: auth.ScopeInternal, KeyID: "legacy"})
			return handler(ctx, req)
		}
		return nil, status.Error(codes.Unauthenticated, "invalid token")
	}
}

func renderProto(v interface{}) string {
	if m, ok := v.(proto.Message); ok {
		b, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(m)
		if err == nil {
			return string(b)
		}
	}
	b, _ := json.Marshal(v)
	return string(b)
}
