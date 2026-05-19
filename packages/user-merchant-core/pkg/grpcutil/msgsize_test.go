package grpcutil

import (
	"context"
	"testing"

	usermerchantv1 "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMsgSize_NoLimit_NoOp(t *testing.T) {
	iv := MsgSizeLimitInterceptor(MsgSizeLimits{})
	_, err := iv(context.Background(), &usermerchantv1.GetMerchantRequest{Id: "x"}, &grpc.UnaryServerInfo{FullMethod: "/a"}, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestMsgSize_DefaultEnforced(t *testing.T) {
	iv := MsgSizeLimitInterceptor(MsgSizeLimits{Default: 1})
	req := &usermerchantv1.GetMerchantRequest{Id: "a-longer-than-one-byte-id"}
	_, err := iv(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/a"}, func(ctx context.Context, _ interface{}) (interface{}, error) {
		t.Fatal("handler should not run")
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected err")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %s", st.Code())
	}
}

func TestMsgSize_PerMethodOverride(t *testing.T) {
	iv := MsgSizeLimitInterceptor(MsgSizeLimits{
		Default:  1,
		ByMethod: map[string]int{"/generous": 1 << 20},
	})
	req := &usermerchantv1.GetMerchantRequest{Id: "normally-too-big"}
	_, err := iv(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/generous"}, func(ctx context.Context, _ interface{}) (interface{}, error) {
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("per-method override should allow: %v", err)
	}
}
