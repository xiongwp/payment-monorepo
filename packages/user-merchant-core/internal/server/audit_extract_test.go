package server

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/xiongwp/user-merchant-core/pkg/grpcutil"
)

func TestActorFromAuth_PrefersXAdminActor(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(
			"x-admin-actor", "alice@corp",
			"authorization", "Bearer some-token-that-should-be-ignored",
		))
	if got := actorFromAuth(ctx); got != "alice@corp" {
		t.Fatalf("expected alice@corp, got %q", got)
	}
}

// 验证：bearer token 校验通过后 AuthInterceptor 把 caller label 写入 ctx，
// actorFromAuth 能读到。
func TestActorFromAuth_FallsBackToCallerFromCtx(t *testing.T) {
	intc := grpcutil.AuthInterceptor(map[string]string{
		"sk-admin": "admin-web",
	}, zap.NewNop())

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer sk-admin"))

	var actor string
	_, err := intc(ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/svc.v1/Mutation"},
		func(c context.Context, _ interface{}) (interface{}, error) {
			actor = actorFromAuth(c)
			return nil, nil
		})
	if err != nil {
		t.Fatalf("auth failed: %v", err)
	}
	if actor != "admin-web" {
		t.Fatalf("expected admin-web, got %q", actor)
	}
}

func TestActorFromAuth_AnonymousWhenNothing(t *testing.T) {
	if got := actorFromAuth(context.Background()); got != "anonymous" {
		t.Fatalf("expected anonymous, got %q", got)
	}
}

// 关键回归：旧实现 "Bearer admin___fake" → 返回 "admin___" 可伪造。
// 现在不再回落到 token 前缀，ctx 里也没有 caller label → "anonymous"。
func TestActorFromAuth_BearerPrefixNoLongerLeaks(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer admin___fake_attacker"))
	if got := actorFromAuth(ctx); got != "anonymous" {
		t.Fatalf("expected anonymous (no spoof), got %q", got)
	}
}
