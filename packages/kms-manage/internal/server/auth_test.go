package server

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func ok(_ context.Context, _ interface{}) (interface{}, error) { return "ok", nil }

func TestAuthInterceptor_NoTokensConfiguredAllowsAll(t *testing.T) {
	intc := AuthInterceptor(map[string]string{}, zap.NewNop())
	resp, err := intc(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/kms.v1.KMS/Decrypt"}, ok)
	if err != nil || resp != "ok" {
		t.Fatalf("expected pass-through, got resp=%v err=%v", resp, err)
	}
}

func TestAuthInterceptor_HealthExempt(t *testing.T) {
	intc := AuthInterceptor(map[string]string{"good": "caller"}, zap.NewNop())
	_, err := intc(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}, ok)
	if err != nil {
		t.Fatalf("expected health exempt, got %v", err)
	}
}

func TestAuthInterceptor_RejectsMissingBearer(t *testing.T) {
	intc := AuthInterceptor(map[string]string{"good": "caller"}, zap.NewNop())
	_, err := intc(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/kms.v1.KMS/Decrypt"}, ok)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestAuthInterceptor_RejectsWrongToken(t *testing.T) {
	intc := AuthInterceptor(map[string]string{"good": "caller"}, zap.NewNop())
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer wrong"))
	_, err := intc(ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/kms.v1.KMS/Decrypt"}, ok)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestAuthInterceptor_AcceptsValidTokenAmongMany(t *testing.T) {
	intc := AuthInterceptor(map[string]string{
		"alpha":   "svc-a",
		"beta":    "svc-b",
		"correct": "svc-c",
	}, zap.NewNop())
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer correct"))
	resp, err := intc(ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/kms.v1.KMS/Decrypt"}, ok)
	if err != nil || resp != "ok" {
		t.Fatalf("expected accept, got resp=%v err=%v", resp, err)
	}
}
