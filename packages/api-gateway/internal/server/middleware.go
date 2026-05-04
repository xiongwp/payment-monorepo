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
					metrics.ShadowHeaderRejected.WithLabelValues(r.URL.Path).Inc()
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

// APIKeyMiddleware 校验请求头 X-API-Key。tokens 为空时退化为 warn-only：放行
// 所有请求，启动时一次 WARN 提示生产应配置。健康检查路径豁免。
//
// 比较走 subtle.ConstantTimeCompare 防 timing attack。原 token 列表在启动时
// 物化为 [][]byte，避免每条请求 range map 引入额外 timing 信号。
func APIKeyMiddleware(tokens []string, logger *zap.Logger) func(http.Handler) http.Handler {
	expected := make([][]byte, 0, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		expected = append(expected, []byte(t))
	}
	if len(expected) == 0 {
		logger.Warn("api-gateway: API KEY AUTH DISABLED — set auth.tokens in config or auth.enabled=false explicitly for dev")
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
			var match int
			for _, e := range expected {
				match |= subtle.ConstantTimeCompare(got, e)
			}
			if match != 1 {
				logger.Warn("api-key rejected",
					zap.String("path", r.URL.Path),
					zap.String("remote", r.RemoteAddr))
				http.Error(w, `{"error":"invalid X-API-Key"}`, http.StatusUnauthorized)
				return
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

// RateLimitMiddleware 双层限流：per-IP（防匿名扫描） + per-merchant
// （X-Merchant-ID 取值，防一个商户耗尽全局额度）。
//
// 实现：per_key_limiter 内部按 key 维护独立 rate.Limiter，惰性创建 + LRU 淘汰，
// 防 map 无限增长。
func RateLimitMiddleware(
	ipRPS, ipBurst, merchantRPS, merchantBurst int,
	logger *zap.Logger,
) func(http.Handler) http.Handler {
	ipLimiter := ratelimit.NewKeyed(rate.Limit(ipRPS), ipBurst)
	mchLimiter := ratelimit.NewKeyed(rate.Limit(merchantRPS), merchantBurst)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isRateLimitExemptPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			ip := clientIP(r)
			if ipRPS > 0 && !ipLimiter.Allow(ip) {
				logger.Warn("rate limit hit",
					zap.String("dim", "ip"),
					zap.String("key", ip),
					zap.String("path", r.URL.Path))
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
			// 资安：per-merchant 限流之前直接读 X-Merchant-ID header，header 是
			// 客户端可控的——攻击者拿一把合法 API key 不停换 merchant_id 头，
			// per-merchant 桶每次都新鲜，等同没限流。
			//
			// 临时修复：merchant rate limit 仅在 ctx 里有由 APIKeyMiddleware
			// 注入的 merchantID（即真正的 server-side authoritative 值）才生效。
			// 当前 APIKey 还没把 API key 反查到 merchant_id（kms-manage 那条路
			// 暂未接），所以这块先空跑——比信任客户端 header 安全。
			//
			// follow-up：APIKeyMiddleware 拿到 token 后查 merchant 注册表把
			// merchant_id 注入 ctx，这里改读 ctx.Value("merchant_id")。
			if mch := merchantIDFromContext(r); mch != "" && merchantRPS > 0 {
				if !mchLimiter.Allow(mch) {
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
