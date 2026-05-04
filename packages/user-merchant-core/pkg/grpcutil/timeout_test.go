package grpcutil

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
)

func TestTimeout_NoConfig_NoOp(t *testing.T) {
	iv := TimeoutInterceptor(TimeoutConfig{})
	called := false
	_, err := iv(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x"}, func(ctx context.Context, _ interface{}) (interface{}, error) {
		called = true
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("no timeout config → no deadline injected")
		}
		return nil, nil
	})
	if err != nil || !called {
		t.Fatal("should pass through")
	}
}

func TestTimeout_DefaultInjected(t *testing.T) {
	iv := TimeoutInterceptor(TimeoutConfig{Default: 10 * time.Millisecond})
	_, err := iv(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x"}, func(ctx context.Context, _ interface{}) (interface{}, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Fatal("deadline should be present")
		}
		if time.Until(d) > 10*time.Millisecond {
			t.Fatalf("deadline too far: %s", time.Until(d))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTimeout_PerMethodOverridesDefault(t *testing.T) {
	iv := TimeoutInterceptor(TimeoutConfig{
		Default:  5 * time.Second,
		ByMethod: map[string]time.Duration{"/fast": 10 * time.Millisecond},
	})
	_, _ = iv(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/fast"}, func(ctx context.Context, _ interface{}) (interface{}, error) {
		d, _ := ctx.Deadline()
		if time.Until(d) > 30*time.Millisecond {
			t.Fatalf("per-method should win; got deadline %s away", time.Until(d))
		}
		return nil, nil
	})
}

func TestTimeout_CallerTighterDeadlineKept(t *testing.T) {
	iv := TimeoutInterceptor(TimeoutConfig{Default: time.Hour})
	parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _ = iv(parent, nil, &grpc.UnaryServerInfo{FullMethod: "/x"}, func(ctx context.Context, _ interface{}) (interface{}, error) {
		d, _ := ctx.Deadline()
		if time.Until(d) > 50*time.Millisecond {
			t.Fatalf("caller 20ms should dominate over 1h default; got %s", time.Until(d))
		}
		return nil, nil
	})
}
