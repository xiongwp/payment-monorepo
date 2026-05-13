// Package errcode — 全平台统一错误码 + 多语言文案.
//
// 设计:
//   - Code 是稳定的字符串枚举 (snake_case),不会变;HTTP / gRPC code 是映射出来的
//   - Message 走 i18n,通过 LangFromContext / WithLang 决定
//   - Detail 由调用方运行时填充 (不入 i18n,因含动态数据)
//
// 用法:
//
//	return errcode.New(errcode.CodeIdempotencyConflict, "pi_id mismatch")
//	// 或带数据:
//	return errcode.New(errcode.CodeAmountTooSmall).WithDetail("min=100")
//	// 在 HTTP handler 出口:
//	if e := errcode.As(err); e != nil {
//	    w.WriteHeader(e.HTTPStatus())
//	    json.NewEncoder(w).Encode(e.ToProblem(ctx))   // RFC 7807
//	}
package errcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/grpc/codes"
)

// Code 全平台错误码 (稳定 string 枚举).
type Code string

const (
	// 公共
	CodeInternal          Code = "internal"
	CodeInvalidArgument   Code = "invalid_argument"
	CodeUnauthenticated   Code = "unauthenticated"
	CodePermissionDenied  Code = "permission_denied"
	CodeNotFound          Code = "not_found"
	CodeAlreadyExists     Code = "already_exists"
	CodeFailedPrecondition Code = "failed_precondition"
	CodeDeadlineExceeded  Code = "deadline_exceeded"
	CodeUnavailable       Code = "unavailable"
	CodeRateLimited       Code = "rate_limited"

	// 支付域
	CodeIdempotencyConflict Code = "idempotency_conflict"
	CodeAmountTooSmall      Code = "amount_too_small"
	CodeAmountTooLarge      Code = "amount_too_large"
	CodeCurrencyUnsupported Code = "currency_unsupported"
	CodeMerchantSuspended   Code = "merchant_suspended"
	CodeAccountFrozen       Code = "account_frozen"
	CodeInsufficientFunds   Code = "insufficient_funds"
	CodeCardDeclined        Code = "card_declined"
	CodeDoNotHonor          Code = "do_not_honor"
	CodeChannelUnavailable  Code = "channel_unavailable"
	CodeChannelTimeout      Code = "channel_timeout"
	CodeNoFallbackAdapter   Code = "no_fallback_adapter"
	Code3DSRequired         Code = "three_ds_required"

	// 风控
	CodeRiskBlocked    Code = "risk_blocked"
	CodeRiskReview     Code = "risk_review"
	CodeAMLHit         Code = "aml_hit"

	// PCI
	CodeTokenExpired   Code = "token_expired"
	CodeTokenInvalid   Code = "token_invalid"

	// 资金 / 清算
	CodeRefundExceedsCharge Code = "refund_exceeds_charge"
	CodeChargebackInProgress Code = "chargeback_in_progress"
)

// LangKey 是 ctx 里取语言的 key.
type LangKey struct{}

// LangFromContext 从 ctx 取语言 (en / zh / ja),空 → en.
func LangFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(LangKey{}).(string); ok && v != "" {
		return v
	}
	return "en"
}

// WithLang 把语言塞 ctx.
func WithLang(ctx context.Context, lang string) context.Context {
	return context.WithValue(ctx, LangKey{}, lang)
}

// Error 错误对象.
type Error struct {
	code   Code
	detail string
}

// New 构造.
func New(code Code, detail ...string) *Error {
	e := &Error{code: code}
	if len(detail) > 0 {
		e.detail = detail[0]
	}
	return e
}

// WithDetail 链式.
func (e *Error) WithDetail(format string, args ...any) *Error {
	if len(args) > 0 {
		e.detail = fmt.Sprintf(format, args...)
	} else {
		e.detail = format
	}
	return e
}

// Error implements error.
func (e *Error) Error() string {
	if e.detail == "" {
		return string(e.code)
	}
	return string(e.code) + ": " + e.detail
}

// Code returns the code.
func (e *Error) Code() Code { return e.code }

// HTTPStatus maps to HTTP status code.
func (e *Error) HTTPStatus() int {
	switch e.code {
	case CodeInvalidArgument, CodeAmountTooSmall, CodeAmountTooLarge, CodeCurrencyUnsupported:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodePermissionDenied, CodeRiskBlocked, CodeAMLHit, CodeMerchantSuspended, CodeAccountFrozen:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeAlreadyExists, CodeIdempotencyConflict:
		return http.StatusConflict
	case CodeFailedPrecondition, CodeRefundExceedsCharge:
		return http.StatusUnprocessableEntity
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeDeadlineExceeded, CodeChannelTimeout:
		return http.StatusGatewayTimeout
	case CodeUnavailable, CodeChannelUnavailable, CodeNoFallbackAdapter:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// GRPCCode maps to gRPC code.
func (e *Error) GRPCCode() codes.Code {
	switch e.code {
	case CodeInvalidArgument, CodeAmountTooSmall, CodeAmountTooLarge, CodeCurrencyUnsupported:
		return codes.InvalidArgument
	case CodeUnauthenticated:
		return codes.Unauthenticated
	case CodePermissionDenied, CodeRiskBlocked, CodeAMLHit:
		return codes.PermissionDenied
	case CodeNotFound:
		return codes.NotFound
	case CodeAlreadyExists, CodeIdempotencyConflict:
		return codes.AlreadyExists
	case CodeFailedPrecondition, CodeRefundExceedsCharge:
		return codes.FailedPrecondition
	case CodeRateLimited:
		return codes.ResourceExhausted
	case CodeDeadlineExceeded, CodeChannelTimeout:
		return codes.DeadlineExceeded
	case CodeUnavailable, CodeChannelUnavailable:
		return codes.Unavailable
	default:
		return codes.Internal
	}
}

// Problem 序列化为 RFC 7807 application/problem+json.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Code     string `json:"code"`
	TraceID  string `json:"trace_id,omitempty"`
}

// ToProblem builds an RFC 7807 representation.
func (e *Error) ToProblem(ctx context.Context) Problem {
	return Problem{
		Type:   "https://errors.payment.example.com/" + string(e.code),
		Title:  Message(string(e.code), LangFromContext(ctx)),
		Status: e.HTTPStatus(),
		Detail: e.detail,
		Code:   string(e.code),
	}
}

// MarshalJSON 默认 marshal 走 Problem.
func (e *Error) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.ToProblem(context.Background()))
}

// As 把 error 解析为 *Error。非匹配时返 nil。
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
