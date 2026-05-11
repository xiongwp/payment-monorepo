// Package problem — RFC 7807 Problem Details for HTTP APIs.
//
// 统一所有服务的错误响应格式 → 客户端 SDK 一份解析逻辑搞定:
//
//   {
//     "type":     "https://errors.payment.example.com/insufficient-balance",
//     "title":    "Insufficient balance",
//     "status":   402,
//     "detail":   "Account cust_wallet/u123/USD has 50.00 but needs 100.00",
//     "instance": "/api/v1/payments/pay/abc",
//     "trace_id": "abc123def...",
//     "code":     "insufficient_balance",
//     "errors":   [{ "field": "amount", "msg": "..." }]
//   }
//
// 标准: RFC 7807 https://datatracker.ietf.org/doc/html/rfc7807
//
// 用:
//   // 业务代码
//   if balance < amount {
//       problem.Write(w, r, problem.InsufficientBalance(account, balance, amount))
//       return
//   }
//
//   // 自定义
//   problem.Write(w, r, problem.New(problem.WithStatus(409),
//       problem.WithCode("idempotency_conflict"),
//       problem.WithTitle("Idempotency key already used"),
//       problem.WithDetail("key=abc has been used with different request body")))

package problem

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ContentType RFC 7807 媒体类型.
const ContentType = "application/problem+json"

// Problem RFC 7807 + 业务扩展字段.
type Problem struct {
	Type     string         `json:"type,omitempty"`
	Title    string         `json:"title"`
	Status   int            `json:"status"`
	Detail   string         `json:"detail,omitempty"`
	Instance string         `json:"instance,omitempty"`
	// ── 业务扩展 (RFC 7807 §3.2 允许任意扩展字段) ──
	Code     string         `json:"code,omitempty"`     // 机器可读错误码, 客户端用它分支处理
	TraceID  string         `json:"trace_id,omitempty"` // 客户支持要这个查日志
	Errors   []FieldError   `json:"errors,omitempty"`   // 字段级错误 (常见 validation)
	Meta     map[string]any `json:"meta,omitempty"`     // 额外上下文
}

// FieldError 字段级 validation 错误.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
	Got     any    `json:"got,omitempty"`
}

// Error 让 Problem 实现 error 接口.
func (p *Problem) Error() string {
	if p.Detail != "" {
		return fmt.Sprintf("%s: %s", p.Code, p.Detail)
	}
	return p.Title
}

// Option ...
type Option func(*Problem)

// WithStatus ...
func WithStatus(s int) Option { return func(p *Problem) { p.Status = s } }

// WithCode ...
func WithCode(c string) Option { return func(p *Problem) { p.Code = c } }

// WithTitle ...
func WithTitle(t string) Option { return func(p *Problem) { p.Title = t } }

// WithDetail ...
func WithDetail(d string, args ...any) Option {
	return func(p *Problem) {
		if len(args) > 0 {
			p.Detail = fmt.Sprintf(d, args...)
		} else {
			p.Detail = d
		}
	}
}

// WithType type URI (一般指向 docs/errors/...).
func WithType(t string) Option { return func(p *Problem) { p.Type = t } }

// WithInstance ...
func WithInstance(i string) Option { return func(p *Problem) { p.Instance = i } }

// WithField 添加一条字段错.
func WithField(field, msg string, got any) Option {
	return func(p *Problem) {
		p.Errors = append(p.Errors, FieldError{Field: field, Message: msg, Got: got})
	}
}

// WithMeta ...
func WithMeta(k string, v any) Option {
	return func(p *Problem) {
		if p.Meta == nil {
			p.Meta = map[string]any{}
		}
		p.Meta[k] = v
	}
}

// New 构造一个 Problem.
func New(opts ...Option) *Problem {
	p := &Problem{Status: http.StatusInternalServerError, Title: "Internal Error"}
	for _, o := range opts {
		o(p)
	}
	if p.Type == "" && p.Code != "" {
		p.Type = "https://errors.payment.example.com/" + p.Code
	}
	return p
}

// Write 把 problem 写到 ResponseWriter (自动从 ctx 提 trace_id).
func Write(w http.ResponseWriter, r *http.Request, p *Problem) {
	if p == nil {
		p = New()
	}
	if p.TraceID == "" {
		p.TraceID = traceIDFromCtx(r.Context())
	}
	if p.Instance == "" {
		p.Instance = r.URL.Path
	}
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// ─── 预定义常用 Problem (各服务复用) ──────────────────────────────

// InvalidArgument 400 — 请求参数错.
func InvalidArgument(detail string, fields ...FieldError) *Problem {
	p := New(WithStatus(400), WithCode("invalid_argument"),
		WithTitle("Invalid argument"), WithDetail(detail))
	p.Errors = fields
	return p
}

// Unauthorized 401 — 未鉴权.
func Unauthorized(detail string) *Problem {
	return New(WithStatus(401), WithCode("unauthorized"),
		WithTitle("Unauthorized"), WithDetail(detail))
}

// Forbidden 403 — 缺 scope.
func Forbidden(scope string) *Problem {
	return New(WithStatus(403), WithCode("insufficient_scope"),
		WithTitle("Insufficient scope"),
		WithDetail("missing scope %q", scope),
		WithMeta("required_scope", scope))
}

// NotFound 404.
func NotFound(resource string) *Problem {
	return New(WithStatus(404), WithCode("not_found"),
		WithTitle("Not found"), WithDetail("resource %q not found", resource))
}

// Conflict 409 — 幂等冲突 / 状态冲突.
func Conflict(detail string) *Problem {
	return New(WithStatus(409), WithCode("conflict"),
		WithTitle("Conflict"), WithDetail(detail))
}

// IdempotencyConflict 409 (常见).
func IdempotencyConflict(key string) *Problem {
	return New(WithStatus(409), WithCode("idempotency_conflict"),
		WithTitle("Idempotency key already used"),
		WithDetail("key=%q has been used with different request body", key),
		WithMeta("idempotency_key", key))
}

// InsufficientBalance 402 — 业务自定义典型例.
func InsufficientBalance(account string, have, need int64, currency string) *Problem {
	return New(WithStatus(402), WithCode("insufficient_balance"),
		WithTitle("Insufficient balance"),
		WithDetail("account %s has %d %s but needs %d %s", account, have, currency, need, currency),
		WithMeta("account", account),
		WithMeta("have", have),
		WithMeta("need", need),
		WithMeta("currency", currency))
}

// RateLimited 429.
func RateLimited(retryAfterSec int) *Problem {
	return New(WithStatus(429), WithCode("rate_limited"),
		WithTitle("Rate limited"),
		WithDetail("retry after %d seconds", retryAfterSec),
		WithMeta("retry_after", retryAfterSec))
}

// UpstreamError 502 — 下游故障.
func UpstreamError(service string, err error) *Problem {
	return New(WithStatus(502), WithCode("upstream_error"),
		WithTitle("Upstream service error"),
		WithDetail("%s: %v", service, err),
		WithMeta("upstream", service))
}

// Internal 500 — 兜底 (生产应该 sanitize, 不暴露 stack).
func Internal(detail string) *Problem {
	return New(WithStatus(500), WithCode("internal_error"),
		WithTitle("Internal server error"), WithDetail(detail))
}

// ─── ctx trace id 提取 (跟 payment-mw 兼容) ──────────────────────────

// 用 string 而不是 type-typed key 以保持 zero-dep.
const traceCtxKey = "trace_id"

func traceIDFromCtx(ctx context.Context) string {
	v := ctx.Value(traceCtxKey)
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// FromError 把任意 error 转成 Problem (兜底).
//   - 如果 err 本身是 *Problem, 直接返
//   - 否则包成 500 Internal (sanitize detail 避免泄密)
func FromError(err error) *Problem {
	if err == nil {
		return nil
	}
	if p, ok := err.(*Problem); ok {
		return p
	}
	return Internal(err.Error())
}

// WrapHTTPError 把 net/http style error 转成 RFC 7807 (middleware 用).
//   先尝试 detect 是否已经是 problem+json, 是则透传; 否则升级。
func WrapHTTPError(w http.ResponseWriter, r *http.Request, status int, msg string, code string) {
	Write(w, r, New(WithStatus(status), WithCode(code),
		WithTitle(http.StatusText(status)), WithDetail(msg)))
}
