package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	kitexserver "github.com/cloudwego/kitex/server"
	"github.com/xiongwp/payment-util/kitexutil"
	"github.com/xiongwp/payment-util/shadow"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	riskv1 "github.com/xiongwp/risk-manage/kitex_gen/risk/v1"
	riskservice "github.com/xiongwp/risk-manage/kitex_gen/risk/v1/riskservice"

	"github.com/xiongwp/risk-manage/internal/auth"
	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/metrics"
	"github.com/xiongwp/risk-manage/internal/reliability"
	"github.com/xiongwp/risk-manage/internal/service"
	"github.com/xiongwp/risk-manage/internal/store"
)

// Server 实现 Kitex riskservice.Server 接口 (跟 gRPC 同形态 — 方法签名 ctx + *pbReq → *pbResp + error).
//
// 切 Kitex 后不再 embed UnimplementedRiskServiceServer (gRPC 兼容性兜底);
// 接口完整实现见 Screen / BulkScreen / Report / ErasePersonalData / ListRules /
// ReloadRules / AddBlacklist / RemoveBlacklist / ListBlacklist 9 个方法.
type Server struct {
	svc             *service.RiskService
	bl              store.Blacklist
	auth            map[string]string
	apiKeys         auth.APIKeyStore             // nil = 不启用 per-merchant API key (兼容老 AuthTokens 模式)
	limiter         *reliability.MerchantLimiter // nil = 不限流 (dev / 单测)
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
	addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	// Kitex server — middleware 链走 kitexutil (跟老 grpc interceptor 等价).
	// TODO: shadow MW (从 metainfo 取 x-shadow 翻 ctx) — 等 kitexutil.ShadowMW port 完成
	// TODO: putil.KitexMW (trace) — 等 payment-util/trace 提 KitexMW
	// TODO: authInterceptor port → kitexutil.MultiAuthMW(tokens, apiKeys)
	advHost := os.Getenv("ADVERTISE_HOST")
	if advHost == "" {
		advHost = "risk-manage"
	}
	srvOpts := []kitexserver.Option{kitexserver.WithServiceAddr(addr)}
	srvOpts = append(srvOpts, kitexutil.DefaultServerOptions("risk-manage", fmt.Sprintf("%s:%d", advHost, port))...)
	// TODO 接 kitexutil MW 三件套 + shadow / trace / auth port 完成后取消注释:
	// srvOpts = append(srvOpts, kitexserver.WithMiddleware(kitexutil.RecoverMW(s.logger)))
	// srvOpts = append(srvOpts, kitexserver.WithMiddleware(kitexutil.LogMW(s.logger)))
	// srvOpts = append(srvOpts, kitexserver.WithMiddleware(kitexutil.MetricsMW()))
	srv := riskservice.NewServer(s, srvOpts...)
	// gRPC 占位 _ = ... 已删 (Kitex 不再需要 grpc.ServerOption / keepalive / metadata).
	_ = kitexutil.LogMW // 留 (Kitex MW 接通后用)
	s.logger.Info("risk-manage Kitex listening", zap.Int("port", port))
	go func() {
		<-ctx.Done()
		// Kitex Stop 是 graceful: 等 in-flight RPC 跑完再退.
		// 跟老 GracefulStop 不同: Kitex 没有强制 Stop fallback API, 整个 ctx 超时由
		// fx.Lifecycle OnStop 的 stopCtx 控制 (默认 15s, 跟老 shutdownTimeout 对齐).
		timeout := s.shutdownTimeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		done := make(chan struct{})
		go func() {
			if err := srv.Stop(); err != nil {
				s.logger.Warn("kitex stop error", zap.Error(err))
			}
			close(done)
		}()
		select {
		case <-done:
			s.logger.Info("kitex graceful stop complete")
		case <-time.After(timeout):
			s.logger.Warn("kitex graceful stop timeout (Kitex doesn't expose force-Stop; consider raising fx.Lifecycle StopTimeout)",
				zap.Duration("timeout", timeout))
		}
	}()
	return srv.Run()
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
		return nil, fmt.Errorf("%s", err.Error())
	}
	merchantID := auth.FillMerchantID(ctx, req.GetMerchantId())
	// per-merchant 限流：超额返 ResourceExhausted，order-core 应识别为可重试。
	if s.limiter != nil && !s.limiter.Allow(merchantID) {
		return nil, fmt.Errorf("merchant %s exceeded screen QPS quota", merchantID)
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
		return nil, fmt.Errorf("%s", err.Error())
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
		return fmt.Errorf("merchant scope cannot access admin endpoints")
	}
	return nil
}

// ─── Blacklist 管理 ──────────────────────────────

// validBlacklistDimensions MED-FIX-2: blacklist rule (rules/blacklist.go pickDimension)
// 只识别 merchant/customer/ip/device 这 4 个 dimension; 之前 handler 不校验, 写进 "garbage_dim"
// 也成功 → 占内存但永远不会被 Contains() 命中 = 隐性故障. 入口拒绝.
var validBlacklistDimensions = map[string]struct{}{
	"merchant": {}, "customer": {}, "ip": {}, "device": {},
}

// requireAdminOrInternal blacklist 写操作不让商户调; admin / internal 放行.
func requireAdminOrInternal(ctx context.Context) error {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil // dev / 未鉴权模式放行
	}
	if p.Scope == auth.ScopeMerchant {
		return fmt.Errorf("merchant scope cannot manage blacklist")
	}
	return nil
}

// operatorID 取 caller KeyID 做 audit log 关联. 没鉴权时返 "anonymous".
func operatorID(ctx context.Context) string {
	if p, ok := auth.PrincipalFrom(ctx); ok && p != nil {
		return p.KeyID
	}
	return "anonymous"
}

func (s *Server) AddBlacklist(ctx context.Context, req *riskv1.BlacklistEntryMsg) (*riskv1.BlacklistOpResponse, error) {
	if err := requireAdminOrInternal(ctx); err != nil {
		return nil, err
	}
	dim := req.GetDimension()
	val := req.GetValue()
	if dim == "" || val == "" {
		return nil, fmt.Errorf("dimension and value required")
	}
	if _, ok := validBlacklistDimensions[dim]; !ok {
		return nil, fmt.Errorf("invalid dimension %q (allowed: merchant / customer / ip / device)", dim)
	}
	s.bl.Add(ctx, dim, val, req.GetReason())
	s.logger.Info("blacklist_admin_add",
		zap.String("audit_event", "blacklist_add"),
		zap.String("operator", operatorID(ctx)),
		zap.String("dim", dim),
		zap.String("val", val),
		zap.String("reason", req.GetReason()))
	return &riskv1.BlacklistOpResponse{Ok: true}, nil
}

func (s *Server) RemoveBlacklist(ctx context.Context, req *riskv1.BlacklistEntryMsg) (*riskv1.BlacklistOpResponse, error) {
	if err := requireAdminOrInternal(ctx); err != nil {
		return nil, err
	}
	dim := req.GetDimension()
	val := req.GetValue()
	if dim == "" || val == "" {
		return nil, fmt.Errorf("dimension and value required")
	}
	if _, ok := validBlacklistDimensions[dim]; !ok {
		return nil, fmt.Errorf("invalid dimension %q (allowed: merchant / customer / ip / device)", dim)
	}
	s.bl.Remove(ctx, dim, val)
	s.logger.Info("blacklist_admin_remove",
		zap.String("audit_event", "blacklist_remove"),
		zap.String("operator", operatorID(ctx)),
		zap.String("dim", dim),
		zap.String("val", val))
	return &riskv1.BlacklistOpResponse{Ok: true}, nil
}

func (s *Server) ListBlacklist(ctx context.Context, req *riskv1.ListBlacklistRequest) (*riskv1.ListBlacklistResponse, error) {
	if err := requireAdminOrInternal(ctx); err != nil {
		return nil, err
	}
	dim := req.GetDimension()
	if dim != "" {
		if _, ok := validBlacklistDimensions[dim]; !ok {
			return nil, fmt.Errorf("invalid dimension %q (allowed: merchant / customer / ip / device; or empty for all)", dim)
		}
	}
	entries := s.bl.List(ctx, dim)
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
		return nil, fmt.Errorf("erase requires internal scope")
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
		return nil, fmt.Errorf("%s", err.Error())
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

// ─── interceptors deleted — gRPC UnaryServerInterceptor 不再适用 Kitex.
// 等价 Kitex middleware 在 kitexutil.{RecoverMW,LogMW,MetricsMW,AuthMW} 提供,
// 由 cmd/server/main.go 通过 server.WithMiddleware(...) 接入.

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
