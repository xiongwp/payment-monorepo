package grpcutil

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func okHandler(_ context.Context, _ interface{}) (interface{}, error) { return "ok", nil }

// captureCallerHandler 把 ctx 里的 caller label 通过 closure 捕获出来给测试断言。
func captureCallerHandler(out *string) grpc.UnaryHandler {
	return func(ctx context.Context, _ interface{}) (interface{}, error) {
		*out = CallerFromContext(ctx)
		return "ok", nil
	}
}

func TestAuthInterceptor_NoTokensConfiguredAllowsAll(t *testing.T) {
	intc := AuthInterceptor(map[string]string{}, zap.NewNop())
	resp, err := intc(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/svc.v1/Foo"}, okHandler)
	if err != nil || resp != "ok" {
		t.Fatalf("expected pass-through, got resp=%v err=%v", resp, err)
	}
}

func TestAuthInterceptor_HealthExempt(t *testing.T) {
	intc := AuthInterceptor(map[string]string{"good": "caller"}, zap.NewNop())
	_, err := intc(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}, okHandler)
	if err != nil {
		t.Fatalf("expected health exempt, got %v", err)
	}
}

func TestAuthInterceptor_RejectsMissingBearer(t *testing.T) {
	intc := AuthInterceptor(map[string]string{"good": "caller"}, zap.NewNop())
	_, err := intc(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/svc.v1/Foo"}, okHandler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestAuthInterceptor_RejectsWrongToken(t *testing.T) {
	intc := AuthInterceptor(map[string]string{"good": "caller"}, zap.NewNop())
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer wrong"))
	_, err := intc(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/svc.v1/Foo"}, okHandler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestAuthInterceptor_AcceptedTokenInjectsCallerLabel(t *testing.T) {
	intc := AuthInterceptor(map[string]string{
		"alpha-token":   "svc-a",
		"beta-token":    "svc-b",
		"correct-token": "admin-web",
	}, zap.NewNop())
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer correct-token"))

	var captured string
	resp, err := intc(ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/svc.v1/Foo"},
		captureCallerHandler(&captured))
	if err != nil || resp != "ok" {
		t.Fatalf("expected accept, got resp=%v err=%v", resp, err)
	}
	if captured != "admin-web" {
		t.Fatalf("expected ctx caller=admin-web, got %q", captured)
	}
}

func TestCallerFromContext_EmptyWithoutAuth(t *testing.T) {
	// ctx 上没值 → ""
	if got := CallerFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
