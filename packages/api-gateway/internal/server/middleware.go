// Package server: HTTP middleware chain（trace / recovery / logging / auth /
// rate-limit）。每一层关心单一职责，按外→内顺序在 NewHTTPServer 里 wrap。
//
// 设计要点：
//   - 所有 middleware 通过 context 传递元信息（trace-id / merchant-id），不
//     用全局 / package-level 变量。
//   - panic 必须被 RecoverMiddleware 兜底，转 500，避免一条坏请求拖死进程。
//   - logging 只记 metadata（method / path / status / duration / size），不记
//     request / response body（敏感字段防泄露）。
package server

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/xiongwp/payment-util/shadow"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/xiongwp/api-gateway/internal/metrics"
	"github.com/xiongwp/api-gateway/internal/ratelimit"
)

// ─── shadow ──────────────────────────────────────────────────────────────────

// ShadowConfig 控制 X-Shadow header 的可信源策略。
//
// **资损 / 安全风险**：X-Shadow=1 让请求走 _shadow 表 + redis _shadow namespace +
// payment-channel adapter 短路（mock 成功不真扣款）。如果外部攻击者能伪造此
// header，可以用真实商户配置发起免费"成功"交易，制造对账假象 + 把测试数据落
// 影子库审计；更严重的是某些路径下 risk-manage 短路 ALLOW，盗卡能借此绕风控。
//
// 默认严格策略：只有可信源（内网 CIDR / 经过 LB 的 X-Internal-Source: trusted
// header）才允许 X-Shadow 透传；其余请求该 header 被 strip 掉。
type ShadowConfig struct {
	// TrustedCIDRs 可信来源 CIDR 列表（IDC 内网、压测平台 IP 段）。
	// 例：["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]。
	// **空列表（默认）= 全部 strip**——最安全。
	TrustedCIDRs []string
	// TrustedHeaderName 经过授信反代会注入的 header 名（如 LB 的内部专属 header）。
	// 若 LB 把外部请求的同名 header 自动剥离再注入自己的版本，这是最强的可信判定。
	// 空 = 不启用此通道。与 TrustedCIDRs 是 OR 关系（任一通过即信任）。
	TrustedHeaderName  string
	TrustedHeaderValue string
}

// parsedCIDRs 启动期一次性解析；运行期每个请求只比较网段。
type parsedCIDRs []*net.IPNet

func (p parsedCIDRs) contains(ip net.IP) bool {
	for _, n := range p {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func parseCIDRs(cidrs []string, logger *zap.Logger) parsedCIDRs {
	out := make(parsedCIDRs, 0, len(cidrs))
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			if logger != nil {
				logger.Warn("ShadowMiddleware: invalid CIDR, ignored",
					zap.String("cidr", c), zap.Error(err))
			}
			continue
		}
		out = append(out, n)
	}
	return out
}

// requestIP 从 RemoteAddr 拿调用方 IP（http.Server 已经把 host:port 形式填进去）。
// 不信任 X-Forwarded-For / X-Real-IP——它们可被外部任意伪造（除非 LB 已校验后注入）。
func requestIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

// sourceTrusted 判断该请求是否可信，可允许携带 X-Shadow。
func sourceTrusted(r *http.Request, cidrs parsedCIDRs, cfg ShadowConfig) bool {
	if cfg.TrustedHeaderName != "" && cfg.TrustedHeaderValue != "" {
		// 1 == subtle.ConstantTimeCompare 时为 1
		got := r.Header.Get(cfg.TrustedHeaderName)
		if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(cfg.TrustedHeaderValue)) == 1 {
			return true
		}
	}
	if len(cidrs) == 0 {
		return false
	}
	ip := requestIP(r)
	if ip == nil {
		return false
	}
	return cidrs.contains(ip)
}

// ShadowMiddleware 把 X-Shadow HTTP header 翻进 request ctx——**只对可信源生效**。
// 不可信源的 X-Shadow header 在 r.Header 上被显式 Del() 掉，
// shadow.HTTPHeaderToContext 看不见就不会标 shadow=true。
//
// 后续 handler 调下游 gRPC 时，shadow.UnaryClientInterceptor 会自动把
// x-shadow=1 metadata 透传给下游服务（仅当 ctx 真带 shadow=true）。
func ShadowMiddleware(cfg ShadowConfig, logger *zap.Logger) func(http.Handler) http.Handler {
	cidrs := parseCIDRs(cfg.TrustedCIDRs, logger)
	if len(cidrs) == 0 && cfg.TrustedHeaderName == "" && logger != nil {
		logger.Warn("ShadowMiddleware: no trusted source configured; all X-Shadow headers will be stripped (safe default)")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(shadow.MetadataKey) != "" && !sourceTrusted(r, cidrs, cfg) {
				if logger != nil {
					logger.Warn("ShadowMiddleware: rejected X-Shadow from untrusted source",
						zap.String("remote", r.RemoteAddr),
						zap.String("path", r.URL.Path),
						zap.String("method", r.Method))
				}
				if metrics.ShadowHeaderRejected != nil {
					// **不能用 r.URL.Path** — 含 user_id / order_id / mch_id 的路径
					// 会让 prometheus label 基数爆表（10K user × 5 path = 50K series，
					// 内存 + scrape 都炸）。改用 path "桶"（first 2 segments），
					// 把 /api/v1/merchants/123/orders/456 归一成 /api/v1/merchants。
					metrics.ShadowHeaderRejected.WithLabelValues(pathBucket(r.URL.Path)).Inc()
				}
				r.Header.Del(shadow.MetadataKey)
			}
			ctx := shadow.HTTPHeaderToContext(r.Context(), r.Header)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ─── recovery ────────────────────────────────────────────────────────────────

// RecoverMiddleware 兜底 handler panic，避免整进程崩溃。返回 500 + 空 body。
// 同时打 ERROR 日志带 method / path / 调用栈，便于事后定位。
func RecoverMiddleware(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic in handler",
						zap.String("method", r.Method),
						zap.String("path", r.URL.Path),
						zap.Any("recover", rec),
						zap.String("stack", string(debug.Stack())))
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ─── logging ─────────────────────────────────────────────────────────────────

// statusRecorder 包一层 ResponseWriter 记录最终 status code + bytes。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// LoggingMiddleware 记录 request 元信息 + 耗时 + status。不打 body。
func LoggingMiddleware(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			logger.Info("http",
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Int("status", rec.status),
				zap.Int("bytes", rec.bytes),
				zap.Duration("duration", time.Since(start)),
				zap.String("remote", r.RemoteAddr),
				zap.String("ua", r.UserAgent()))
		})
	}
}

// ─── auth ────────────────────────────────────────────────────────────────────

// APIKeyMiddleware 校验请求头 X-API-Key。
//
// apiKeys: token → merchant_id（server-trusted）。token 为空时退化为 warn-only：
// 放行所有请求，启动时一次 WARN 提示生产应配置。健康检查路径豁免。
//
// **P1-10 解锁 per-merchant rate limit**：之前 token 是 []string，无法把 merchant_id
// 注入 ctx，per-merchant 限流空跑。现在改成 map，token 校验通过后把对应的 merchant_id
// 写到 ctx (WithMerchantID)，下游 RateLimitMiddleware 直接读 ctx 拿到 server-trusted
// merchant_id，per-merchant 限流真正生效。
//
// 比较走 subtle.ConstantTimeCompare 防 timing attack。token 列表在启动时物化为
// [][]byte + 平行 [merchantID 切片，避免每条请求 range map 引入额外 timing 信号。
func APIKeyMiddleware(apiKeys map[string]string, logger *zap.Logger) func(http.Handler) http.Handler {
	expected := make([][]byte, 0, len(apiKeys))
	merchantByIdx := make([]string, 0, len(apiKeys))
	for tok, mch := range apiKeys {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		expected = append(expected, []byte(tok))
		merchantByIdx = append(merchantByIdx, strings.TrimSpace(mch))
	}
	if len(expected) == 0 {
		logger.Warn("api-gateway: API KEY AUTH DISABLED — set auth.api_keys in config or auth.enabled=false explicitly for dev")
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isAuthExemptPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			got := []byte(r.Header.Get("X-API-Key"))
			if len(got) == 0 {
				http.Error(w, `{"error":"missing X-API-Key"}`, http.StatusUnauthorized)
				return
			}
			// 全扫描求 match（constant-time 防 timing leak）；同时记录命中位置
			// 用于反查 merchant_id。命中位置在 ctx 注入前不会暴露给攻击者，
			// 因为只有真命中才走到 next handler。
			var match int
			matchedIdx := -1
			for i, e := range expected {
				eq := subtle.ConstantTimeCompare(got, e)
				match |= eq
				if eq == 1 {
					matchedIdx = i
				}
			}
			if match != 1 {
				logger.Warn("api-key rejected",
					zap.String("path", r.URL.Path),
					zap.String("remote", r.RemoteAddr))
				http.Error(w, `{"error":"invalid X-API-Key"}`, http.StatusUnauthorized)
				return
			}
			if matchedIdx >= 0 {
				if mch := merchantByIdx[matchedIdx]; mch != "" {
					r = WithMerchantID(r, mch)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isAuthExemptPath 跳过 X-API-Key 校验（这些路径用 JWT/Cookie 自己鉴权或本就
// 是匿名 health probe）。**注意**：跳过 API-key 不等于跳过限流——
// 限流逻辑请用 isRateLimitExemptPath 单独判断。
func isAuthExemptPath(p string) bool {
	switch p {
	case "/", "/health", "/healthz", "/readyz",
		"/signup", "/login", "/verify-otp", "/logout", "/me", "/forgot":
		return true
	}
	return strings.HasPrefix(p, "/health/")
}

// isRateLimitExemptPath 仅 health probe / 静态根路径豁免限流。
// 资安修复：之前 RateLimitMiddleware 与 APIKey 共用 isAuthExemptPath，导致
// /login / /signup / /verify-otp / /forgot 这些**认证入口完全没限流**——
// 攻击者可以跨多个 login_id 走 credential stuffing，绕过 user-merchant-core
// per-account 5 次锁的防御。这些路径必须留在 IP 限流里。
func isRateLimitExemptPath(p string) bool {
	switch p {
	case "/", "/health", "/healthz", "/readyz":
		return true
	}
	return strings.HasPrefix(p, "/health/")
}

// ─── rate limit ──────────────────────────────────────────────────────────────

// RateLimitParams 双层限流参数；config-center 推送时整体替换。
type RateLimitParams struct {
	IPRPS         int
	IPBurst       int
	MerchantRPS   int
	MerchantBurst int
}

// RateLimitHub 持有一组 Keyed limiter；config-center 推送变更时调
// ApplyParams 在线热更新（保留已存在的子 limiter 状态，不丢 token bucket）。
type RateLimitHub struct {
	ipLim  *ratelimit.Keyed
	mchLim *ratelimit.Keyed
	logger *zap.Logger
}

// NewRateLimitHub 构造；initial 由 caller 提供（main.go 启动期从 config-center
// 拉一次；不可达走 yaml bootstrap 默认值兜底）。
func NewRateLimitHub(initial RateLimitParams, logger *zap.Logger) *RateLimitHub {
	return &RateLimitHub{
		ipLim:  ratelimit.NewKeyed(rate.Limit(initial.IPRPS), initial.IPBurst),
		mchLim: ratelimit.NewKeyed(rate.Limit(initial.MerchantRPS), initial.MerchantBurst),
		logger: logger,
	}
}

// ApplyParams config-center watch 推送 / OnChange 回调里调；零拷贝热更新。
func (h *RateLimitHub) ApplyParams(p RateLimitParams) {
	h.ipLim.SetLimit(rate.Limit(p.IPRPS), p.IPBurst)
	h.mchLim.SetLimit(rate.Limit(p.MerchantRPS), p.MerchantBurst)
	if h.logger != nil {
		h.logger.Info("rate_limit params hot-reloaded",
			zap.Int("ip_rps", p.IPRPS), zap.Int("ip_burst", p.IPBurst),
			zap.Int("mch_rps", p.MerchantRPS), zap.Int("mch_burst", p.MerchantBurst))
	}
}

// RateLimitMiddleware 双层限流（per-IP + per-merchant），走 hub 接 config-center。
func RateLimitMiddleware(hub *RateLimitHub, logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isRateLimitExemptPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			ip := clientIP(r)
			if !hub.ipLim.Allow(ip) {
				logger.Warn("rate limit hit",
					zap.String("dim", "ip"),
					zap.String("key", ip),
					zap.String("path", r.URL.Path))
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
			// per-merchant：merchant_id 由 APIKeyMiddleware 在 token 校验后注入
			// ctx（server-trusted）；匿名路径返空 → 退化 per-IP only。
			if mch := merchantIDFromContext(r); mch != "" {
				if !hub.mchLim.Allow(mch) {
					logger.Warn("rate limit hit",
						zap.String("dim", "merchant"),
						zap.String("key", mch),
						zap.String("path", r.URL.Path))
					http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP 从 X-Forwarded-For / X-Real-IP / RemoteAddr 提取最近一跳 IP。
// 信任 LB / CDN 的头；如果不信任，应在 LB 处剥离再交给 gateway。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return strings.TrimSpace(xrip)
	}
	// RemoteAddr 形如 "ip:port"，取 ip 部分
	if i := strings.LastIndexByte(r.RemoteAddr, ':'); i >= 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}

// merchantCtxKey 用于把 server-trusted merchant_id 从 APIKeyMiddleware
// （或未来的 OAuth2 middleware）传到下游 handler / 限流器，避免从客户端
// header 读 X-Merchant-ID 的伪造问题。
type merchantCtxKey struct{}

// isAdminCtxKey P1-21：admin HTTP 入口在 ctx 显式 mark "is_admin"，让下游 handler
// 区分公网用户面 vs admin 调用——避免某个 RPC 同时挂在两个 mux 时被滥用。
type isAdminCtxKey struct{}

// merchantIDFromContext 从 ctx 提取 server-trusted merchant_id。
// 返回空字符串 = 没有 authoritative merchant_id（匿名 / API key 还没反查到
// merchant，限流跳过 per-merchant 维度，只走 per-IP）。
func merchantIDFromContext(r *http.Request) string {
	v, _ := r.Context().Value(merchantCtxKey{}).(string)
	return v
}

// WithMerchantID 在 ctx 注入 server-trusted merchant_id。
// APIKeyMiddleware（或后续 OAuth2）拿到 token 后查 merchant 注册表得到
// merchant_id，再用本函数注入 ctx；下游限流 / 审计读 ctx 而非 header。
func WithMerchantID(r *http.Request, mch string) *http.Request {
	if mch == "" {
		return r
	}
	return r.WithContext(contextWithMerchantID(r.Context(), mch))
}

func contextWithMerchantID(parent context.Context, mch string) context.Context {
	return context.WithValue(parent, merchantCtxKey{}, mch)
}

// IsAdminFromContext 是否经 admin token middleware 校验过的请求。
// 业务 handler 入口可拿来加二次门禁（"这条 RPC 必须 admin 身份"）。
func IsAdminFromContext(r *http.Request) bool {
	v, _ := r.Context().Value(isAdminCtxKey{}).(bool)
	return v
}

// WithIsAdmin 在 ctx 标记 admin 身份；admin token middleware 通过校验后调用。
func WithIsAdmin(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), isAdminCtxKey{}, true))
}

// pathBucket 把 url path 归一成 prometheus label 用的低基数桶：
// 取头 2 段（含 /api/v1）。例：
//
//	/api/v1/merchants/123/orders/456 → /api/v1/merchants
//	/api/v1/cards                    → /api/v1/cards
//	/healthz                         → /healthz
//	(empty / "/")                    → /
//
// 上限 2 段保证：常见 ~20 个业务 path 集合不会因 user_id / mch_id / order_id
// 在路径里而爆基数。生产经 grafana 仍能看到核心路径的 X-Shadow 拒绝分布。
func pathBucket(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	parts := strings.SplitN(p, "/", 5) // ["", "api", "v1", "merchants", "..."]
	switch {
	case len(parts) <= 1:
		return "/"
	case len(parts) == 2:
		return "/" + parts[1]
	case len(parts) == 3:
		return "/" + parts[1] + "/" + parts[2]
	default:
		return "/" + parts[1] + "/" + parts[2] + "/" + parts[3]
	}
}
