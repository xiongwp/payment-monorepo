// gRPC + zap integration helpers for piiredact.
//
// LoggingInterceptor is the canonical entry point: drop it into a gRPC server
// chain and access logs will have PII redacted automatically.

package piiredact

import (
	"context"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// LoggingOptions controls what the interceptor logs.
type LoggingOptions struct {
	// Redactor overrides the default; nil → GetDefault().
	Redactor *Redactor

	// LogPayload — log full req/resp (redacted) in addition to metadata.
	// Off by default; turning on costs CPU + log volume.
	LogPayload bool

	// SkipMethods — exact gRPC method paths to skip (e.g. health checks).
	SkipMethods map[string]struct{}
}

// LoggingInterceptor returns a gRPC unary server interceptor that emits an
// "access log" with redacted payloads.
//
// Output (zap fields):
//   - method:    "/svc.Foo/Bar"
//   - code:      gRPC status code
//   - duration:  request handling time
//   - req:       (optional) redacted request map
//   - resp:      (optional) redacted response map  — only on success
//   - error:     (on failure) status.Message (passed through RedactString)
func LoggingInterceptor(log *zap.Logger, opts LoggingOptions) grpc.UnaryServerInterceptor {
	if log == nil {
		log = zap.NewNop()
	}
	r := opts.Redactor
	if r == nil {
		r = GetDefault()
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if _, skip := opts.SkipMethods[info.FullMethod]; skip {
			return handler(ctx, req)
		}
		start := time.Now()
		resp, err := handler(ctx, req)
		dur := time.Since(start)

		code := codes.OK
		errMsg := ""
		if err != nil {
			st, _ := status.FromError(err)
			code = st.Code()
			errMsg = r.RedactString(st.Message())
		}

		fields := []zap.Field{
			zap.String("method", info.FullMethod),
			zap.String("code", code.String()),
			zap.Duration("duration", dur),
		}
		if opts.LogPayload {
			fields = append(fields, zap.Any("req", r.Redact(req)))
			if err == nil {
				fields = append(fields, zap.Any("resp", r.Redact(resp)))
			}
		}
		if errMsg != "" {
			fields = append(fields, zap.String("error", errMsg))
		}

		// Pick level by code: OK → Info, client errors (InvalidArgument..) → Warn,
		// server errors (Internal..) → Error.
		level := zapcore.InfoLevel
		switch code {
		case codes.OK, codes.Canceled, codes.NotFound, codes.AlreadyExists:
			level = zapcore.InfoLevel
		case codes.InvalidArgument, codes.FailedPrecondition,
			codes.OutOfRange, codes.Unauthenticated, codes.PermissionDenied,
			codes.ResourceExhausted, codes.Aborted, codes.Unavailable:
			level = zapcore.WarnLevel
		default:
			level = zapcore.ErrorLevel
		}
		if ce := log.Check(level, "grpc.access"); ce != nil {
			ce.Write(fields...)
		}
		return resp, err
	}
}

// ZapField returns a zap.Field whose value is the redacted form of v.
// Convenient for ad-hoc log sites that don't go through the interceptor.
//
// Example:
//
//	log.Info("user submitted form", piiredact.ZapField("form", form))
func ZapField(key string, v any) zap.Field {
	return zap.Any(key, GetDefault().Redact(v))
}

// ZapStringField returns a zap.Field whose string value has been Luhn-scrubbed
// (for free-form text like raw response bodies).
func ZapStringField(key, s string) zap.Field {
	return zap.String(key, GetDefault().RedactString(s))
}
