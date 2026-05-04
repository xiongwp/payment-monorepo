// 鉴权拦截器单测：覆盖安全默认 + dev 显式放行 + 正常 token 校验。
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

// 简单 handler，用于检测是否被放行。
func okHandler(_ context.Context, _ interface{}) (interface{}, error) {
	return "ok", nil
}

func callAuth(t *testing.T, validTokens map[string]string, allowUnauth bool, method string, mdAuth string) (interface{}, error) {
	t.Helper()
	ic := AuthInterceptor(validTokens, allowUnauth, zap.NewNop())
	ctx := context.Background()
	if mdAuth != "" {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", mdAuth))
	}
	info := &grpc.UnaryServerInfo{FullMethod: method}
	return ic(ctx, nil, info, okHandler)
}

// 1. 安全默认：tokens 空 + allow_unauthenticated=false → 业务 RPC 拒绝。
func TestAuthInterceptor_EmptyTokensRejectsByDefault(t *testing.T) {
	resp, err := callAuth(t, nil, false, "/order.v1.PaymentIntentService/Create", "")
	if err == nil {
		t.Fatalf("want Unauthenticated, got resp=%v err=nil", resp)
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

// 2. health/reflection 例外：tokens 空 + allow_unauthenticated=false 也仍放行（健康检查不能挂）。
func TestAuthInterceptor_HealthCheckAlwaysAllowed(t *testing.T) {
	resp, err := callAuth(t, nil, false, "/grpc.health.v1.Health/Check", "")
	if err != nil || resp != "ok" {
		t.Fatalf("want ok, got resp=%v err=%v", resp, err)
	}
	resp2, err2 := callAuth(t, nil, false, "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo", "")
	if err2 != nil || resp2 != "ok" {
		t.Fatalf("want ok, got resp=%v err=%v", resp2, err2)
	}
}

// 3. dev 开关：tokens 空 + allow_unauthenticated=true → 放行。
func TestAuthInterceptor_AllowUnauthenticated(t *testing.T) {
	resp, err := callAuth(t, nil, true, "/order.v1.PaymentIntentService/Create", "")
	if err != nil || resp != "ok" {
		t.Fatalf("want ok, got resp=%v err=%v", resp, err)
	}
}

// 4. 正常 token 校验：缺 header / 错 token / 对 token。
func TestAuthInterceptor_TokenValidation(t *testing.T) {
	tokens := map[string]string{"good-token": "ok"}

	// 缺 header
	if _, err := callAuth(t, tokens, false, "/svc/Method", ""); err == nil {
		t.Fatal("want Unauthenticated for missing header")
	}

	// 错误格式
	if _, err := callAuth(t, tokens, false, "/svc/Method", "bad-format"); err == nil {
		t.Fatal("want Unauthenticated for missing Bearer prefix")
	}

	// 错 token
	if _, err := callAuth(t, tokens, false, "/svc/Method", "Bearer wrong"); err == nil {
		t.Fatal("want Unauthenticated for wrong token")
	}

	// 对 token
	resp, err := callAuth(t, tokens, false, "/svc/Method", "Bearer good-token")
	if err != nil || resp != "ok" {
		t.Fatalf("want ok with valid token, got resp=%v err=%v", resp, err)
	}
}

// 5. NewServer 启动期兜底：tokens 空 + allow_unauthenticated=false 必须返回 error，
// 避免运维忘配置导致裸奔上线。
func TestNewServer_RefusesIfUnconfigured(t *testing.T) {
	_, err := NewServer(Deps{
		AuthTokens:               nil,
		AuthAllowUnauthenticated: false,
		Logger:                   zap.NewNop(),
	})
	if err == nil {
		t.Fatal("NewServer should refuse when AuthTokens empty and allow_unauthenticated=false")
	}
}

// 6. NewServer dev 显式开启：可启动。
func TestNewServer_AllowsDevModeExplicit(t *testing.T) {
	srv, err := NewServer(Deps{
		AuthTokens:               nil,
		AuthAllowUnauthenticated: true,
		Logger:                   zap.NewNop(),
	})
	if err != nil || srv == nil {
		t.Fatalf("NewServer with dev mode failed: %v", err)
	}
}

// 7. NewServer 配了 token 即可启动。
func TestNewServer_AllowsWithTokens(t *testing.T) {
	srv, err := NewServer(Deps{
		AuthTokens:               map[string]string{"t": "ok"},
		AuthAllowUnauthenticated: false,
		Logger:                   zap.NewNop(),
	})
	if err != nil || srv == nil {
		t.Fatalf("NewServer with tokens failed: %v", err)
	}
}

