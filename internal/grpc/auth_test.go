package grpc

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeHandler 总是返回固定字符串，用于断言"是否进入 handler"。
func fakeHandler(_ context.Context, _ interface{}) (interface{}, error) {
	return "ok", nil
}

func TestServiceTokenInterceptor_EmptyTokenIsWarnOnly(t *testing.T) {
	// token == "" → 任何请求（甚至无 metadata）都放行
	intc := serviceTokenInterceptor("", zap.NewNop())
	resp, err := intc(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/accounting.v1.AccountingService/DoubleEntryBooking"},
		fakeHandler)
	if err != nil {
		t.Fatalf("expected pass-through, got err: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("expected handler reached, got %v", resp)
	}
}

func TestServiceTokenInterceptor_StrictRejectsMissing(t *testing.T) {
	intc := serviceTokenInterceptor("secret", zap.NewNop())
	_, err := intc(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/accounting.v1.AccountingService/DoubleEntryBooking"},
		fakeHandler)
	if err == nil {
		t.Fatal("expected Unauthenticated for missing metadata, got nil")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestServiceTokenInterceptor_StrictRejectsWrong(t *testing.T) {
	intc := serviceTokenInterceptor("secret", zap.NewNop())
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(MetadataServiceTokenKey, "wrong"))
	_, err := intc(ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/accounting.v1.AccountingService/DoubleEntryBooking"},
		fakeHandler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v (err=%v)", status.Code(err), err)
	}
}

func TestServiceTokenInterceptor_StrictAcceptsValid(t *testing.T) {
	intc := serviceTokenInterceptor("secret", zap.NewNop())
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(MetadataServiceTokenKey, "secret"))
	resp, err := intc(ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/accounting.v1.AccountingService/DoubleEntryBooking"},
		fakeHandler)
	if err != nil {
		t.Fatalf("expected pass, got err: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("expected handler reached, got %v", resp)
	}
}

func TestServiceTokenInterceptor_HealthExempt(t *testing.T) {
	intc := serviceTokenInterceptor("secret", zap.NewNop())
	// 无 metadata 但是 health method → 仍放行
	resp, err := intc(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"},
		fakeHandler)
	if err != nil {
		t.Fatalf("expected health exempt, got err: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("expected handler reached, got %v", resp)
	}
}

func TestServiceTokenInterceptor_ReflectionExempt(t *testing.T) {
	intc := serviceTokenInterceptor("secret", zap.NewNop())
	resp, err := intc(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo"},
		fakeHandler)
	if err != nil {
		t.Fatalf("expected reflection exempt, got err: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("expected handler reached, got %v", resp)
	}
}
