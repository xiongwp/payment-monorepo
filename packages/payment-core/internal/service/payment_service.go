// Package service 把 order-core 的 PaymentChannel 6 方法翻译到
// payment-channel 的 AcquirerService。
//
// 约束：
//   - payment-core 自身无状态、无 DB
//   - 幂等键统一由 service.IdempotencyKey 产生，透传给下游
//   - 失败码先用 channel 侧归一结果，缺失时兜底 channel.NormalizeFailure
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	channelv1 "github.com/xiongwp/payment-channel/api/proto/channel/v1"
	"github.com/xiongwp/payment-core/internal/channel"
	"github.com/xiongwp/payment-core/internal/channelclient"
	"github.com/xiongwp/payment-core/internal/channelops"
	"github.com/xiongwp/payment-core/internal/circuitbreaker"
	"github.com/xiongwp/payment-core/internal/metrics"
	"github.com/xiongwp/payment-core/internal/riskclient"
	"github.com/xiongwp/payment-core/internal/routing"
)

// callerIsProbeAuthorized 判定 caller 是否被允许传 probe=true。
//
// 检查 incoming gRPC metadata：
//   - x-source = "qa" / "admin"
//   - x-role  包含 "admin" / "qa"
//
// admin-web BFF / QA 工具调用方在 outbound 时会显式注入这两个 header；
// 普通商户调用方拿不到这套 metadata（gateway 层会清洗自定义 header），
// 故能拿到 probe 权限的只可能是受信内部组件。
func callerIsProbeAuthorized(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	if v := strings.ToLower(strings.Join(md.Get("x-source"), ",")); v != "" {
		if strings.Contains(v, "qa") || strings.Contains(v, "admin") {
			return true
		}
	}
	if v := strings.ToLower(strings.Join(md.Get("x-role"), ",")); v != "" {
		if strings.Contains(v, "admin") || strings.Contains(v, "qa") {
			return true
		}
	}
	return false
}

// isUnknownResultErr 判断下游 gRPC 错误是否属于"结果未知"类——超时 / 取消 / 不可达。
//
// 资损背景：原实现把这些错误直接 `return nil, err`，order-core 看到 err 即认为
// 本次扣款 / 退款"失败"，会按业务策略二次发起。下游可能其实已经扣款成功，
// 重复发起将造成实际重复扣款 / 重复退款。统一识别后改回 ResultProcessing
// + FailureCode="result_unknown" 让 order-core 走"结果未知 → 主动 Query
// 对账"的安全路径，并由 payment-channel 侧 UNIQUE(idempotency_key) 兜底。
//
// 命中条件：
//   - context.DeadlineExceeded / context.Canceled（caller ctx 自行超时）
//   - gRPC status 是 DEADLINE_EXCEEDED / UNAVAILABLE / Canceled
func isUnknownResultErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.DeadlineExceeded, codes.Unavailable, codes.Canceled:
			return true
		}
	}
	return false
}

type PaymentService struct {
	router        *routing.Router
	fallback      *routing.FallbackRouter // P0-3：备用渠道路由
	client        channelclient.Client
	risk          riskclient.Client
	riskFailClose bool // true = risk 不可达时拒绝交易；false = fail-open 放行
	// riskReviewStepUp 收到 verdict=REVIEW 时，是否对支持 3DS 的渠道（CARD）触发
	// step-up：返回 requires_action(three_d_secure) 让前端 redirect 到银行 3DS。
	// false（默认）= 继续路由，等同 ALLOW（兼容老行为）。
	riskReviewStepUp bool
	// riskReviewStepUpMethods 哪些 payment_method 支持 step-up；默认 ["CARD"]。
	// GCash / Maya 等钱包没 3DS 通用机制，命中只能继续路由 + 监控。
	riskReviewStepUpMethods map[string]bool
	// riskTimeoutDefault 风控调用兜底超时（rpc 客户端层也有 dial timeout，但这是 per-call 上限）。
	// 默认走全局 risk.rpc_timeout；下面 riskTimeoutByMerchant 命中时按商户覆盖。
	riskTimeoutDefault   time.Duration
	riskTimeoutByMerchant map[string]time.Duration // merchant_id → timeout；任何配置都会被 minRiskTimeoutFloor 兜底（不允许 < 3s 让风控形同虚设）
	riskTimeoutMu         sync.RWMutex             // 保护 riskTimeoutByMerchant 的并发热更新
	maintenance           int32                    // atomic: 1 = maintenance mode, 0 = normal
	breakers              *circuitbreaker.Registry
	channelOps            *channelops.Manager
	retryQueue            routing.RetryQueue       // P0-3：重试队列（outbox pattern）
	logger                *zap.Logger
}

func NewPaymentService(r *routing.Router, c channelclient.Client, risk riskclient.Client, logger *zap.Logger) *PaymentService {
	return &PaymentService{
		router:                  r,
		fallback:                routing.NewFallbackRouter(logger),
		client:                  c,
		risk:                    risk,
		riskTimeoutByMerchant:   map[string]time.Duration{},
		riskReviewStepUpMethods: map[string]bool{"CARD": true},
		breakers:                circuitbreaker.NewRegistry(circuitbreaker.DefaultConfig),
		channelOps:              channelops.NewManager(),
		logger:                  logger,
	}
}

// SetRiskReviewStepUp 切换 verdict=REVIEW 时的处理策略。
//   - enabled=false（默认）：REVIEW 等同 ALLOW 继续路由
//   - enabled=true + payment_method 在 methods 里：返回 requires_action(three_d_secure)
//     让前端发起 3DS 挑战；通过 → 重新调用 Charge（业务侧带 step-up token），不通过 → 失败
//
// methods 为空 → 沿用默认 ["CARD"]。
func (s *PaymentService) SetRiskReviewStepUp(enabled bool, methods []string) {
	s.riskReviewStepUp = enabled
	if len(methods) == 0 {
		return
	}
	m := make(map[string]bool, len(methods))
	for _, x := range methods {
		m[strings.ToUpper(strings.TrimSpace(x))] = true
	}
	s.riskReviewStepUpMethods = m
}

// minRiskTimeoutFloor 风控调用兜底超时的"地板"：
// 即使 yaml 配 0（不限）或非常小的值（误配 1ms），也强制至少 3s。
// 资损背景：如果配为 0（不限），caller ctx 一旦极短（order-core 误配 50ms）会
// 触发风控 fail-open 直接放行——盗卡攻击者可借此跳过风控。给一个硬地板能保证
// 至少在 3s 窗口内 risk-manage 能给出明确判定。
const minRiskTimeoutFloor = 3 * time.Second

// SetRiskTimeoutDefault 设置全局兜底超时；通常在 server 启动时由 config 注入。
// 资损保护：若入参 < minRiskTimeoutFloor（含 0 = 不限），强制使用 floor 并打警告。
func (s *PaymentService) SetRiskTimeoutDefault(d time.Duration) {
	if d < minRiskTimeoutFloor {
		if s.logger != nil {
			s.logger.Warn("risk timeout default below floor; clamping",
				zap.Duration("requested", d),
				zap.Duration("floor", minRiskTimeoutFloor))
		}
		d = minRiskTimeoutFloor
	}
	s.riskTimeoutDefault = d
}

// SetRiskTimeoutByMerchant 替换 merchant_id → timeout 映射（原子）。
// 典型用法：admin /admin/risk-timeout 端点 reload yaml 配置后调用，热生效。
//
// 资损保护：每个商户级超时也应用 minRiskTimeoutFloor 地板——避免某个商户
// 单独配 0/1ms 导致 fail-open 绕过风控。
func (s *PaymentService) SetRiskTimeoutByMerchant(m map[string]time.Duration) {
	cp := make(map[string]time.Duration, len(m))
	for k, v := range m {
		if v < minRiskTimeoutFloor {
			if s.logger != nil {
				s.logger.Warn("merchant risk timeout below floor; clamping",
					zap.String("merchant_id", k),
					zap.Duration("requested", v),
					zap.Duration("floor", minRiskTimeoutFloor))
			}
			v = minRiskTimeoutFloor
		}
		cp[k] = v
	}
	s.riskTimeoutMu.Lock()
	s.riskTimeoutByMerchant = cp
	s.riskTimeoutMu.Unlock()
}

// riskTimeoutFor 返回该商户的 risk Screen 超时；命中商户级配置返回它，否则
// 返回全局 default（可能是 0 = 不限）。
func (s *PaymentService) riskTimeoutFor(merchantID string) time.Duration {
	if merchantID != "" {
		s.riskTimeoutMu.RLock()
		d, ok := s.riskTimeoutByMerchant[merchantID]
		s.riskTimeoutMu.RUnlock()
		if ok {
			return d
		}
	}
	return s.riskTimeoutDefault
}

// Breakers exposes the per-adapter circuit breaker registry so the admin
// metrics HTTP server can list states + reset stuck breakers. Returning the
// concrete type is fine: the metrics server only holds it behind a narrow
// BreakerRegistry interface.
func (s *PaymentService) Breakers() *circuitbreaker.Registry { return s.breakers }

// CircuitStates 暴露给 admin / healthcheck 用
func (s *PaymentService) CircuitStates() map[string]string {
	return s.breakers.States()
}

// SetRiskFailClose 运行时切换 fail-open / fail-close
func (s *PaymentService) SetRiskFailClose(v bool) { s.riskFailClose = v }

// SetMaintenance 维护模式：1=拒绝新支付, 0=正常
func (s *PaymentService) SetMaintenance(on bool) {
	if on {
		atomic.StoreInt32(&s.maintenance, 1)
	} else {
		atomic.StoreInt32(&s.maintenance, 0)
	}
}

// InMaintenance 当前是否维护模式
func (s *PaymentService) InMaintenance() bool {
	return atomic.LoadInt32(&s.maintenance) == 1
}

// ChannelOps 暴露渠道运维管理器给 admin 用
func (s *PaymentService) ChannelOps() *channelops.Manager {
	return s.channelOps
}

// ─── Charge ─────────────────────────────────────────────────────────

func (s *PaymentService) Charge(ctx context.Context, req *channel.PaymentRequest) (*channel.PaymentResponse, error) {
	start := time.Now()

	// ── 维护模式检查 ─────────────────────────────────────────
	if s.InMaintenance() {
		return &channel.PaymentResponse{
			ResultType:     channel.ResultFailed,
			FailureCode:    "maintenance",
			FailureMessage: "system is in maintenance mode, please retry later",
		}, nil
	}

	// ── 风控判定（risk-manage.Screen）──────────────────────────
	if s.risk != nil {
		screenReq := &riskclient.ScreenRequest{
			PaymentIntentID: req.PaymentIntentID,
			MerchantID:      req.Metadata["merchant_id"],
			CustomerID:      req.CustomerID,
			Amount:          req.Amount,
			Currency:        req.Currency,
			PaymentMethod:   req.PaymentMethod,
			Country:         req.Country,
			IPAddress:       req.Metadata["ip_address"],
			DeviceID:        req.Metadata["device_id"],
			UserAgent:       req.Metadata["user_agent"],
			// risk_session_id 由 checkout SDK 创建，业务侧塞到 metadata 透传过来。
			// 拿不到时 risk-manage 不能做 fingerprint / behavior 关联，但其它规则
			// 仍按 IP / amount / 历史限额跑，不阻断。
			RiskSessionID: req.Metadata["risk_session_id"],
			// 用 PaymentIntentID 作幂等键：order-core 重试 / 双数据中心 fallback
			// 都拿同一 PI，risk-manage 只 evaluate / audit 一次。
			IdempotencyKey: req.PaymentIntentID,
		}
		// per-merchant risk timeout：高流量商户配 500ms 强制快速决策（fail-open
		// 兜底，不阻塞支付主路径）。0 表示不限，走 caller ctx。
		callCtx := ctx
		if d := s.riskTimeoutFor(screenReq.MerchantID); d > 0 {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		// P0-6 防雪崩：risk-manage 在前面调用持续失败时熔断保护，避免本服务
		// 把 goroutine 全部 hang 在 3s 超时上，连带把 channel 调用也阻塞。
		// breaker open 时按 fail-open/close 策略走，跟 RPC 真挂等价处理。
		riskBreaker := s.breakers.Get("risk-manage")
		if !riskBreaker.Allow() {
			metrics.RiskScreenTotal.WithLabelValues("circuit_open").Inc()
			s.logger.Warn("risk circuit breaker open; treating as risk-unavailable",
				zap.String("pi_id", req.PaymentIntentID))
			if s.riskFailClose {
				return &channel.PaymentResponse{
					ResultType:     channel.ResultFailed,
					FailureCode:    "risk_unavailable",
					FailureMessage: "risk engine breaker open; transaction denied (fail-close policy)",
				}, nil
			}
			// fail-open: 跳过 risk 直接进路由（与 sr=nil err=nil 等价）
			goto skipRiskDecision
		}
		screenStart := time.Now()
		sr, err := s.risk.Screen(callCtx, screenReq)
		screenLat := time.Since(screenStart).Seconds()
		if err != nil {
			riskBreaker.RecordFailure()
			// I4: fail-open vs fail-close 可配。当前默认 fail-open（risk 挂了不阻塞支付）。
			// 高风险场景可以改为 fail-close：返回 channel_unavailable 拒绝交易。
			if s.riskFailClose {
				metrics.RiskScreenTotal.WithLabelValues("fail_close").Inc()
				metrics.RiskScreenDuration.WithLabelValues("fail_close").Observe(screenLat)
				s.logger.Error("risk screen failed, DENYING (fail-close mode)",
					zap.String("pi_id", req.PaymentIntentID), zap.Error(err))
				return &channel.PaymentResponse{
					ResultType:     channel.ResultFailed,
					FailureCode:    "risk_unavailable",
					FailureMessage: "risk engine unreachable; transaction denied (fail-close policy)",
				}, nil
			}
			metrics.RiskScreenTotal.WithLabelValues("fail_open").Inc()
			metrics.RiskScreenDuration.WithLabelValues("fail_open").Observe(screenLat)
			s.logger.Warn("risk screen failed, allowing (fail-open)",
				zap.String("pi_id", req.PaymentIntentID), zap.Error(err))
		} else {
			riskBreaker.RecordSuccess()
			outcome := "review"
			switch sr.Decision {
			case riskclient.Allow:
				outcome = "allow"
			case riskclient.Deny:
				outcome = "deny"
			}
			metrics.RiskScreenTotal.WithLabelValues(outcome).Inc()
			metrics.RiskScreenDuration.WithLabelValues(outcome).Observe(screenLat)
			s.logger.Info("risk screen",
				zap.String("pi_id", req.PaymentIntentID),
				zap.String("decision_id", sr.DecisionID),
				zap.Int("decision", int(sr.Decision)),
				zap.Int("score", sr.RiskScore),
				zap.String("level", sr.RiskLevel),
				zap.String("reason", sr.Reason),
				zap.String("recommended_action", sr.RecommendedAction))
			if sr.Decision == riskclient.Deny {
				metrics.ChargeTotal.WithLabelValues(req.PaymentMethod, "risk", "denied").Inc()
				// P1-1: 风控拒绝也要 Report，让 risk 限额计数器纳入被拒交易，
				// 防止盗卡通过"故意触发 deny"轮询绕过限频。
				s.reportRiskAsync(ctx, req, "payment.denied", "risk_blocked", sr.DecisionID)
				return &channel.PaymentResponse{
					ResultType:     channel.ResultFailed,
					FailureCode:    "risk_blocked",
					FailureMessage: "transaction denied by risk engine: " + sr.Reason,
					RiskLevel:      sr.RiskLevel,
					RiskScore:      sr.RiskScore,
					RawResponse: map[string]string{
						"risk_decision_id": sr.DecisionID,
					},
				}, nil
			}
			// REVIEW + step-up 启用 + 该 payment_method 支持 3DS → 返回 requires_action
			// 让前端发起 3DS 挑战。挑战成功后业务侧重新调 Charge（带 step-up token），
			// 此时 Screen 走幂等命中（同 IdempotencyKey），仍然 REVIEW，但 metadata.step_up_done=1
			// 让规则层放行。这套语义需要规则配合（暂未实现，留作后续）。
			if sr.Decision == riskclient.Review && s.riskReviewStepUp &&
				s.riskReviewStepUpMethods[strings.ToUpper(req.PaymentMethod)] {
				metrics.ChargeTotal.WithLabelValues(req.PaymentMethod, "risk", "review_step_up").Inc()
				// P1-1: REVIEW 走 step-up 也要 Report，理由同 Deny：
				// 限额计数器必须包含"被风控触达"的次数，否则盗卡可绕过限频。
				s.reportRiskAsync(ctx, req, "payment.review", "", sr.DecisionID)
				return &channel.PaymentResponse{
					ResultType: channel.ResultRequiresAction,
					RequiredAction: &channel.RequiredAction{
						Type: channel.ActionTypeThreeDS,
						Details: map[string]string{
							"reason":           "risk_review_step_up",
							"risk_decision_id": sr.DecisionID,
							"risk_level":       sr.RiskLevel,
						},
					},
					RiskLevel: sr.RiskLevel,
					RiskScore: sr.RiskScore,
					RawResponse: map[string]string{
						"risk_decision_id": sr.DecisionID,
					},
				}, nil
			}
		}
	}
skipRiskDecision:

	// ── 路由 ──────────────────────────────────────────────────
	adapter, err := s.router.Route(routing.MatchInput{
		Country:       req.Country,
		PaymentMethod: req.PaymentMethod,
		Amount:        req.Amount,
	})
	if err != nil {
		metrics.RouteTotal.WithLabelValues(req.PaymentMethod, "no_match").Inc()
		return nil, fmt.Errorf("route: %w", err)
	}
	metrics.RouteTotal.WithLabelValues(req.PaymentMethod, adapter).Inc()

	// ── 渠道启停 + 限流 ──────────────────────────────────────
	if !s.channelOps.IsEnabled(adapter) {
		s.logger.Warn("channel disabled",
			zap.String("adapter", adapter), zap.String("pi_id", req.PaymentIntentID))
		metrics.ChargeTotal.WithLabelValues(req.PaymentMethod, adapter, "channel_disabled").Inc()
		return &channel.PaymentResponse{
			ResultType:     channel.ResultFailed,
			FailureCode:    "channel_disabled",
			FailureMessage: fmt.Sprintf("adapter %s is disabled by ops", adapter),
		}, nil
	}
	if !s.channelOps.Allow(adapter) {
		s.logger.Warn("channel rate limited",
			zap.String("adapter", adapter), zap.String("pi_id", req.PaymentIntentID))
		metrics.ChargeTotal.WithLabelValues(req.PaymentMethod, adapter, "channel_rate_limited").Inc()
		return &channel.PaymentResponse{
			ResultType:     channel.ResultFailed,
			FailureCode:    "channel_rate_limited",
			FailureMessage: fmt.Sprintf("adapter %s rate limit exceeded", adapter),
		}, nil
	}

	// ── 熔断检查 + P0-3 备用渠道降级 ──────────────────────────────────────────────
	// Probe / admin-simulated charges (from PH 渠道联测 页 or similar QA tools)
	// bypass the circuit breaker entirely: their failure scenarios are
	// intentional and must not count toward tripping real traffic. The
	// admin-web BFF marks these with Extra["probe"]="true".
	//
	// P1-3 资损/可用性保护：probe=true 仅在调用方携带 admin / qa 身份时才生效——
	// 否则普通商户传 probe=true 可以绕过 circuit breaker 持续打挂的渠道，
	// 既掩盖故障又把熔断保护变成摆设。
	isProbe := req.Extra != nil && req.Extra["probe"] == "true" && callerIsProbeAuthorized(ctx)
	if !isProbe && req.Extra != nil && req.Extra["probe"] == "true" {
		s.logger.Warn("probe=true ignored: caller not admin/qa",
			zap.String("pi_id", req.PaymentIntentID),
			zap.String("payment_method", req.PaymentMethod))
		metrics.ChargeTotal.WithLabelValues(req.PaymentMethod, "probe", "unauthorized").Inc()
	}
	cb := s.breakers.Get(adapter)
	failReason := ""

	if !isProbe && !cb.Allow() {
		failReason = "circuit_open"
		s.logger.Warn("circuit breaker open, trying fallback",
			zap.String("adapter", adapter), zap.String("pi_id", req.PaymentIntentID))
		metrics.ChargeTotal.WithLabelValues(req.PaymentMethod, adapter, "circuit_open").Inc()

		// P0-3：尝试 fallback 渠道
		fromState := cb.State().String()
		adapter, err = s.tryFallbackAdapter(ctx, adapter, req)
		if err != nil {
			// fallback 也失败，写 outbox 异步重试，返回 processing
			s.enqueueRetry(ctx, req, adapter, failReason)
			metrics.RoutingRetryEnqueuedTotal.WithLabelValues(failReason).Inc()
			return &channel.PaymentResponse{
				ResultType:     channel.ResultProcessing,
				FailureCode:    "processing",
				FailureMessage: "primary adapter unavailable, enqueued for async retry",
			}, nil
		}
		metrics.RoutingFallbackTotal.WithLabelValues(fromState, adapter).Inc()
		// fallback 成功继续路由到新 adapter
	}

	in := &channelv1.ChargeRequest{
		Adapter:          adapter,
		PiId:             req.PaymentIntentID,
		IdempotencyKey:   IdempotencyKey(req.PaymentIntentID, "charge", req.Amount, req.Currency),
		Amount:           req.Amount,
		Currency:         req.Currency,
		Description:      req.Description,
		ReturnUrl:        req.ReturnURL,
		NotifyUrl:        req.NotifyURL,
		CaptureImmediate: !strings.EqualFold(req.CaptureMethod, "manual"),
		Metadata:         req.Metadata,
	}
	// 路由便捷映射：payment_method=GCASH_MINIPROGRAM 自动注入
	// metadata[gcash_flow]=miniprogram，让商户 SDK 仅指定 payment_method
	// 即可触发小程序模式，无需显式传 metadata 标志位。
	// 调用方已显式提供 metadata 时尊重原值。
	if strings.EqualFold(req.PaymentMethod, "GCASH_MINIPROGRAM") {
		if in.Metadata == nil {
			in.Metadata = map[string]string{}
		}
		if _, ok := in.Metadata["gcash_flow"]; !ok {
			in.Metadata["gcash_flow"] = "miniprogram"
		}
	}
	if req.CustomerID != "" {
		if in.Metadata == nil {
			in.Metadata = map[string]string{}
		}
		in.Metadata["customer_id"] = req.CustomerID
	}

	resp, err := s.client.Charge(ctx, in)
	lat := time.Since(start).Seconds()
	metrics.ChargeDuration.WithLabelValues(req.PaymentMethod, adapter).Observe(lat)
	if err != nil {
		if !isProbe {
			cb.RecordFailure()
		}
		// 超时 / 不可达：结果未知。绝不能向上游返回 err（order-core 会当成"明确失败"
		// 二次发起，可能造成重复扣款）。改为 ResultProcessing + result_unknown，
		// 让 order-core 走 Query 对账路径；payment-channel 侧 UNIQUE(idempotency_key)
		// 兜底重放安全。
		if isUnknownResultErr(err) {
			metrics.ChargeTotal.WithLabelValues(req.PaymentMethod, adapter, "unknown").Inc()
			s.logger.Warn("charge result unknown (timeout/unavailable), returning processing",
				zap.String("pi_id", req.PaymentIntentID),
				zap.String("adapter", adapter),
				zap.Error(err))
			return &channel.PaymentResponse{
				ResultType:     channel.ResultProcessing,
				FailureCode:    "result_unknown",
				FailureMessage: "downstream timeout/unavailable; result unknown, query to reconcile: " + err.Error(),
			}, nil
		}
		metrics.ChargeTotal.WithLabelValues(req.PaymentMethod, adapter, "error").Inc()
		return nil, err
	}
	// 渠道返回 succeeded / requires_action / processing 都算 "能通"
	// 只有明确 5xx 类错误（体现为 channel_unavailable failure code）才 RecordFailure
	// Probe 请求完全不计入 CB 统计（成功失败都不算）。
	result := resp.GetResult()
	if !isProbe {
		if resp.GetFailureCode() == "channel_unavailable" {
			cb.RecordFailure()
		} else {
			cb.RecordSuccess()
		}
	}
	metrics.ChargeTotal.WithLabelValues(req.PaymentMethod, adapter, result).Inc()

	s.logger.Info("charge done",
		zap.String("pi_id", req.PaymentIntentID),
		zap.String("payment_method", req.PaymentMethod),
		zap.String("adapter", adapter),
		zap.String("result", result))

	// ── 交易后上报给 risk-manage（更新限额计数器） ─────────────
	//
	// 关键点：用 detached ctx + 自己的 timeout，**不**继承 caller ctx。
	// 原实现直接传 ctx 进去，order-core 一断开本调用就被 cancel，导致 risk 限额计数器丢失，
	// 高频商户 sliding-window 数据出现毛刺。WithoutCancel 保留 trace metadata 但脱离 cancel
	// 信号；2s timeout 兜底防止 risk 慢拖死后置 goroutine。
	if s.risk != nil {
		evtType := "payment.processing"
		failCode := ""
		switch {
		case result == "succeeded":
			evtType = "payment.succeeded"
		case result == "failed":
			evtType = "payment.failed"
			failCode = resp.GetFailureCode()
		}
		report := &riskclient.ReportRequest{
			PaymentIntentID: req.PaymentIntentID,
			MerchantID:      req.Metadata["merchant_id"],
			CustomerID:      req.CustomerID,
			Amount:          req.Amount,
			Currency:        req.Currency,
			PaymentMethod:   req.PaymentMethod,
			EventType:       evtType,
			FailureCode:     failCode,
			IPAddress:       req.Metadata["ip_address"],
			DeviceID:        req.Metadata["device_id"],
		}
		go func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), riskReportTimeout)
			defer cancel()
			if err := s.risk.Report(rctx, report); err != nil {
				metrics.RiskReportTotal.WithLabelValues("error").Inc()
				s.logger.Warn("risk.Report failed (post-charge stat only)",
					zap.String("pi_id", report.PaymentIntentID),
					zap.String("event", report.EventType),
					zap.Error(err))
				return
			}
			metrics.RiskReportTotal.WithLabelValues("ok").Inc()
		}()
	}

	return mapChargeResp(req, resp), nil
}

// riskReportTimeout 后置 risk.Report 的硬上限。Report 是统计后置（更新限额计数器），
// 不阻塞用户支付主路径——故 detached ctx + 短 timeout：风控慢/挂了也不会拖累 goroutine。
// 2s 来自 risk.rpc_timeout=3s 的合理收紧（统计场景容忍度高于 Screen）。
const riskReportTimeout = 2 * time.Second

// reportRiskAsync 把一笔 Charge 上报给 risk-manage（更新限额计数器 / 风控行为画像）。
// 用 detached ctx + 自带 timeout，不阻塞主流程。Deny / Review / 成功失败都应当 Report，
// 否则盗卡可以反复触发 deny 但不被风控计数，进而绕过 sliding-window 限频。
func (s *PaymentService) reportRiskAsync(ctx context.Context, req *channel.PaymentRequest, eventType, failCode, decisionID string) {
	if s.risk == nil {
		return
	}
	report := &riskclient.ReportRequest{
		PaymentIntentID: req.PaymentIntentID,
		MerchantID:      req.Metadata["merchant_id"],
		CustomerID:      req.CustomerID,
		Amount:          req.Amount,
		Currency:        req.Currency,
		PaymentMethod:   req.PaymentMethod,
		EventType:       eventType,
		FailureCode:     failCode,
		IPAddress:       req.Metadata["ip_address"],
		DeviceID:        req.Metadata["device_id"],
	}
	go func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), riskReportTimeout)
		defer cancel()
		if err := s.risk.Report(rctx, report); err != nil {
			metrics.RiskReportTotal.WithLabelValues("error").Inc()
			s.logger.Warn("risk.Report failed",
				zap.String("pi_id", report.PaymentIntentID),
				zap.String("event", report.EventType),
				zap.String("decision_id", decisionID),
				zap.Error(err))
			return
		}
		metrics.RiskReportTotal.WithLabelValues("ok").Inc()
	}()
}

// ─── Capture / Void / Refund / Query ────────────────────────────────

func (s *PaymentService) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.CaptureResponse, error) {
	adapter, err := s.adapterForOp(ctx, req.PaymentIntentID, req.ExternalRefNo, req.Extra)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Capture(ctx, &channelv1.CaptureRequest{
		Adapter:        adapter,
		PiId:           req.PaymentIntentID,
		ExternalRefNo:  req.ExternalRefNo,
		Amount:         req.Amount,
		IdempotencyKey: IdempotencyKey(req.PaymentIntentID, "capture", req.Amount, ""),
	})
	if err != nil {
		// 超时/不可达：结果未知，返回 processing 而非 err，避免上游重发造成重复 capture。
		if isUnknownResultErr(err) {
			s.logger.Warn("capture result unknown (timeout/unavailable), returning processing",
				zap.String("pi_id", req.PaymentIntentID), zap.String("adapter", adapter), zap.Error(err))
			return &channel.CaptureResponse{
				ResultType:     channel.ResultProcessing,
				FailureCode:    "result_unknown",
				FailureMessage: "downstream timeout/unavailable; result unknown, query to reconcile: " + err.Error(),
			}, nil
		}
		return nil, err
	}
	return &channel.CaptureResponse{
		ResultType:     channel.PaymentResultType(resp.GetResult()),
		ExternalRefNo:  resp.GetExternalRefNo(),
		AmountCaptured: req.Amount,
		FailureCode:    channel.NormalizeFailure(resp.GetFailureCode()),
		FailureMessage: resp.GetFailureMessage(),
	}, nil
}

func (s *PaymentService) Void(ctx context.Context, req *channel.VoidRequest) (*channel.VoidResponse, error) {
	adapter, err := s.adapterForOp(ctx, req.PaymentIntentID, req.ExternalRefNo, req.Extra)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Void(ctx, &channelv1.VoidRequest{
		Adapter:        adapter,
		PiId:           req.PaymentIntentID,
		ExternalRefNo:  req.ExternalRefNo,
		IdempotencyKey: IdempotencyKey(req.PaymentIntentID, "void", 0, ""),
	})
	if err != nil {
		// 超时/不可达：结果未知，返回 processing。
		if isUnknownResultErr(err) {
			s.logger.Warn("void result unknown (timeout/unavailable), returning processing",
				zap.String("pi_id", req.PaymentIntentID), zap.String("adapter", adapter), zap.Error(err))
			return &channel.VoidResponse{
				ResultType:     channel.ResultProcessing,
				FailureCode:    "result_unknown",
				FailureMessage: "downstream timeout/unavailable; result unknown, query to reconcile: " + err.Error(),
			}, nil
		}
		return nil, err
	}
	return &channel.VoidResponse{
		ResultType:     channel.PaymentResultType(resp.GetResult()),
		ExternalRefNo:  resp.GetExternalRefNo(),
		FailureCode:    channel.NormalizeFailure(resp.GetFailureCode()),
		FailureMessage: resp.GetFailureMessage(),
	}, nil
}

func (s *PaymentService) Refund(ctx context.Context, req *channel.RefundChannelRequest) (*channel.RefundChannelResponse, error) {
	// P0-3: refund_id 必须非空——否则同一 pi 的多次部分退款会因 idempotency_key 一致
	// 在 payment-channel 侧被合并/丢弃，造成"少退"。
	if req.RefundID == "" {
		return nil, status.Error(codes.InvalidArgument, "refund_id required")
	}
	adapter, err := s.adapterForOp(ctx, req.PaymentIntentID, req.ExternalRefNo, req.Extra)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Refund(ctx, &channelv1.RefundRequest{
		Adapter:        adapter,
		PiId:           req.PaymentIntentID,
		ExternalRefNo:  req.ExternalRefNo,
		Amount:         req.Amount,
		Reason:         req.Reason,
		IdempotencyKey: RefundIdempotencyKey(req.PaymentIntentID, req.RefundID, req.Amount, req.Currency),
	})
	if err != nil {
		metrics.RefundTotal.WithLabelValues(adapter, "error").Inc()
		// 超时/不可达：结果未知。绝不能向上游返回 err（业务侧极易二次退款，造成多退资损）。
		// 改为 ResultProcessing + result_unknown，order-core 应走 Query 对账路径；
		// payment-channel 侧 UNIQUE(refund idempotency_key) 兜底重放安全。
		if isUnknownResultErr(err) {
			metrics.RefundTotal.WithLabelValues(adapter, "unknown").Inc()
			s.logger.Warn("refund result unknown (timeout/unavailable), returning processing",
				zap.String("pi_id", req.PaymentIntentID),
				zap.String("refund_id", req.RefundID),
				zap.String("adapter", adapter),
				zap.Error(err))
			return &channel.RefundChannelResponse{
				ResultType:     channel.ResultProcessing,
				FailureCode:    "result_unknown",
				FailureMessage: "downstream timeout/unavailable; result unknown, query to reconcile: " + err.Error(),
			}, nil
		}
		return nil, err
	}
	metrics.RefundTotal.WithLabelValues(adapter, resp.GetResult()).Inc()
	return &channel.RefundChannelResponse{
		ResultType:          channel.PaymentResultType(resp.GetResult()),
		ExternalRefundRefNo: resp.GetExternalRefNo(),
		FailureCode:         channel.NormalizeFailure(resp.GetFailureCode()),
		FailureMessage:      resp.GetFailureMessage(),
	}, nil
}

func (s *PaymentService) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	adapter, err := s.adapterForOp(ctx, req.PaymentIntentID, req.ExternalRefNo, req.Extra)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Query(ctx, &channelv1.QueryRequest{
		Adapter:       adapter,
		PiId:          req.PaymentIntentID,
		ExternalRefNo: req.ExternalRefNo,
	})
	if err != nil {
		return nil, err
	}
	return &channel.QueryResponse{
		ResultType:     channel.PaymentResultType(resp.GetResult()),
		ExternalRefNo:  resp.GetExternalRefNo(),
		AmountCaptured: resp.GetAmountCaptured(),
		AmountRefunded: resp.GetAmountRefunded(),
		RawResponse:    resp.GetRaw(),
	}, nil
}

// adapterForOp 为 Capture/Void/Refund/Query 解析出当初 Charge 用的 adapter 名。
//
// P2-1 资损保护：**强制要求** extra["adapter"] 非空。
//
// 历史实现允许在 extra[adapter] 缺失时，用 country / payment_method 反查路由表。
// 但路由规则可热更新（`POST /admin/routing/reload`）—— 一旦运营调整了 PH/GCASH 的
// adapter 指向（例如灰度切换 gcash → gcash_v2），存量的 Refund/Query 反推就会
// 路由到**新渠道**，而原 charge 在旧渠道。结果：在 v2 上找不到这笔交易，
// "退款失败" → 客服重发到对账系统 → 实际原渠道 v1 的钱永远退不出去。
//
// 正确约定：order-core 在 Charge 成功时把 adapter_name 落到自己的 charge 表，
// Capture/Void/Refund/Query 时从持久化记录里原样透传到 extra["adapter"]。
// 缺失就 InvalidArgument 拒绝——绝不允许哑吞或猜路由。
func (s *PaymentService) adapterForOp(_ context.Context, piID, _ string, extra map[string]string) (string, error) {
	if a := extra["adapter"]; a != "" {
		return a, nil
	}
	return "", status.Errorf(codes.InvalidArgument,
		"adapterForOp: extra[adapter] is required for refund/query/capture/void (pi=%s); "+
			"router-based fallback removed to avoid mis-routing after routing rule hot reload", piID)
}

// ─── proto mapping ────────────────────────────────────────────────

func mapChargeResp(req *channel.PaymentRequest, resp *channelv1.ChargeResponse) *channel.PaymentResponse {
	r := &channel.PaymentResponse{
		ResultType:     channel.PaymentResultType(resp.GetResult()),
		ExternalRefNo:  resp.GetExternalRefNo(),
		FailureCode:    channel.NormalizeFailure(resp.GetFailureCode()),
		FailureMessage: resp.GetFailureMessage(),
		RawResponse:    resp.GetRaw(),
	}
	if ra := resp.GetRequiredAction(); ra != nil {
		r.RequiredAction = proto2Action(ra, req.ReturnURL)
	}
	return r
}

func proto2Action(a *channelv1.RequiredAction, returnURL string) *channel.RequiredAction {
	details := map[string]string{}
	for k, v := range a.GetExtra() {
		details[k] = v
	}
	t := channel.RequiredActionType(a.GetType())
	switch t {
	case channel.ActionTypeAppRedirect:
		details["redirect_url"] = a.GetRedirectUrl()
		if a.GetScheme() != "" {
			details["scheme"] = a.GetScheme()
		}
		if ru := orDefault(a.GetReturnUrl(), returnURL); ru != "" {
			details["return_url"] = ru
		}
	case channel.ActionTypeQRCode:
		if u := a.GetQrCodeUrl(); u != "" {
			details["qrcode_url"] = u
		}
		if img := a.GetQrImageB64(); img != "" {
			details["qrcode_image_base64"] = img
		}
		if p := a.GetPollMs(); p > 0 {
			details["poll_interval_ms"] = fmt.Sprintf("%d", p)
		}
	case channel.ActionTypeOTP:
		if m := a.GetOtpMasked(); m != "" {
			details["recipient_masked"] = m
		}
		if c := a.GetOtpChannel(); c != "" {
			details["channel"] = c
		}
		if l := a.GetOtpLength(); l > 0 {
			details["length"] = fmt.Sprintf("%d", l)
		}
	case channel.ActionTypeThreeDS:
		details["redirect_url"] = a.GetRedirectUrl()
		if ru := orDefault(a.GetReturnUrl(), returnURL); ru != "" {
			details["return_url"] = ru
		}
	}
	exp := time.Unix(a.GetExpiresAt(), 0)
	if a.GetExpiresAt() == 0 {
		exp = time.Now().Add(15 * time.Minute)
	}
	return &channel.RequiredAction{
		Type:         t,
		Details:      details,
		ChallengeRef: a.GetRedirectUrl(), // 大部分 adapter 没单独 challenge_ref，复用 redirect_url
		ExpiresAt:    exp,
	}
}

func orDefault(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

// ─── P0-3 备用渠道降级 ──────────────────────────────────

// tryFallbackAdapter 尝试 fallback 渠道链中的下一个 adapter。
// 返回可用的 adapter 名；全部失败返 error。
func (s *PaymentService) tryFallbackAdapter(ctx context.Context, failedAdapter string, req *channel.PaymentRequest) (string, error) {
	chain := s.fallback.GetFallbackChain(failedAdapter, routing.FallbackInput{
		Country:       req.Country,
		PaymentMethod: req.PaymentMethod,
		BIN:           req.Metadata["bin"],
		Currency:      req.Currency,
	})

	// 跳过已失败的 adapter，尝试 chain 里的下一个
	for _, adapter := range chain {
		if adapter == failedAdapter {
			continue
		}
		cb := s.breakers.Get(adapter)
		if !cb.Allow() {
			s.logger.Debug("fallback adapter also unavailable",
				zap.String("adapter", adapter), zap.String("pi_id", req.PaymentIntentID))
			continue
		}
		if !s.channelOps.IsEnabled(adapter) {
			continue
		}
		s.logger.Info("fallback adapter available",
			zap.String("from", failedAdapter), zap.String("to", adapter),
			zap.String("pi_id", req.PaymentIntentID))
		return adapter, nil
	}

	// 无可用 fallback
	s.logger.Warn("no fallback adapter available",
		zap.String("primary", failedAdapter), zap.String("pi_id", req.PaymentIntentID))
	return "", fmt.Errorf("no fallback adapter available for %s", failedAdapter)
}

// enqueueRetry 把失败的 charge 写入 retry queue（outbox pattern）
func (s *PaymentService) enqueueRetry(ctx context.Context, req *channel.PaymentRequest, adapter, reason string) {
	if s.retryQueue == nil {
		s.logger.Warn("retry queue not configured, dropping retry task",
			zap.String("pi_id", req.PaymentIntentID))
		return
	}

	task := &routing.RetryTask{
		ID:              fmt.Sprintf("retry_%s_%d", req.PaymentIntentID, time.Now().UnixNano()),
		PaymentIntentID: req.PaymentIntentID,
		IdempotencyKey:  IdempotencyKey(req.PaymentIntentID, "charge", req.Amount, req.Currency),
		Amount:          req.Amount,
		Currency:        req.Currency,
		PaymentMethod:   req.PaymentMethod,
		Country:         req.Country,
		BIN:             req.Metadata["bin"],
		FailedAdapter:   adapter,
		Reason:          reason,
		Attempt:         0,
		NextRetryAt:     (&routing.RetryScheduler{}).NextRetryTime(0),
		Metadata:        req.Metadata,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}

	if err := s.retryQueue.Enqueue(ctx, task); err != nil {
		s.logger.Error("enqueue retry task failed",
			zap.String("pi_id", req.PaymentIntentID), zap.Error(err))
	}
}
