package shadow

import (
	"context"
	"net/http"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestIsShadow_Default(t *testing.T) {
	if IsShadow(context.Background()) {
		t.Fatal("default ctx should not be shadow")
	}
	if IsShadow(nil) {
		t.Fatal("nil ctx should not panic and should return false")
	}
}

func TestWithShadow_RoundTrip(t *testing.T) {
	ctx := WithShadow(context.Background(), true)
	if !IsShadow(ctx) {
		t.Fatal("WithShadow(ctx, true) → IsShadow false")
	}
	ctx = WithShadow(ctx, false)
	if IsShadow(ctx) {
		t.Fatal("WithShadow(ctx, false) → IsShadow true")
	}
}

func TestSuffixHelpers(t *testing.T) {
	plain := context.Background()
	shadowed := WithShadow(plain, true)

	cases := []struct {
		fn    func(context.Context, string) string
		name  string
		base  string
		main  string
		shdw  string
	}{
		{TableName, "TableName", "payment_intent_42", "payment_intent_42", "payment_intent_42_shadow"},
		{RedisKey, "RedisKey", "balance:acct123", "balance:acct123", "balance:acct123_shadow"},
		{KafkaTopic, "KafkaTopic", "ledger.entry", "ledger.entry", "ledger.entry_shadow"},
	}
	for _, c := range cases {
		if got := c.fn(plain, c.base); got != c.main {
			t.Errorf("%s(plain, %q) = %q, want %q", c.name, c.base, got, c.main)
		}
		if got := c.fn(shadowed, c.base); got != c.shdw {
			t.Errorf("%s(shadow, %q) = %q, want %q", c.name, c.base, got, c.shdw)
		}
	}
}

func TestFromMetadata(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true}, {"true", true}, {"on", true}, {"TRUE", true}, {" 1 ", true},
		{"0", false}, {"false", false}, {"", false}, {"yes", false},
	}
	for _, c := range cases {
		md := metadata.Pairs(MetadataKey, c.val)
		if got := FromMetadata(md); got != c.want {
			t.Errorf("FromMetadata(%q) = %v, want %v", c.val, got, c.want)
		}
	}
	if FromMetadata(nil) {
		t.Error("FromMetadata(nil) should be false")
	}
}

func TestUnaryServerInterceptor_PromotesMetadata(t *testing.T) {
	ic := UnaryServerInterceptor()
	md := metadata.Pairs(MetadataKey, "1")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	called := false
	_, err := ic(ctx, "req", &grpc.UnaryServerInfo{FullMethod: "/svc/M"},
		func(ctx context.Context, _ interface{}) (interface{}, error) {
			called = true
			if !IsShadow(ctx) {
				t.Error("handler ctx should carry shadow=true")
			}
			return "ok", nil
		})
	if err != nil || !called {
		t.Fatalf("handler not invoked: err=%v", err)
	}
}

func TestUnaryClientInterceptor_AppendsHeader(t *testing.T) {
	ic := UnaryClientInterceptor()
	ctx := WithShadow(context.Background(), true)
	captured := metadata.MD{}
	invoker := func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		md, _ := metadata.FromOutgoingContext(ctx)
		captured = md
		return nil
	}
	_ = ic(ctx, "/svc/M", nil, nil, nil, invoker)
	if vs := captured.Get(MetadataKey); len(vs) != 1 || vs[0] != "1" {
		t.Fatalf("outgoing %s = %v, want [1]", MetadataKey, vs)
	}
}

func TestUnaryClientInterceptor_NoShadowNoHeader(t *testing.T) {
	ic := UnaryClientInterceptor()
	captured := metadata.MD{}
	invoker := func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		md, _ := metadata.FromOutgoingContext(ctx)
		captured = md
		return nil
	}
	_ = ic(context.Background(), "/svc/M", nil, nil, nil, invoker)
	if vs := captured.Get(MetadataKey); len(vs) != 0 {
		t.Fatalf("non-shadow ctx should not append header, got %v", vs)
	}
}

func TestHTTPHeaderToContext(t *testing.T) {
	h := http.Header{}
	h.Set(MetadataKey, "1")
	ctx := HTTPHeaderToContext(context.Background(), h)
	if !IsShadow(ctx) {
		t.Error("http x-shadow=1 should promote to ctx")
	}

	h2 := http.Header{}
	h2.Set(MetadataKey, "0")
	if IsShadow(HTTPHeaderToContext(context.Background(), h2)) {
		t.Error("http x-shadow=0 should not promote")
	}

	if IsShadow(HTTPHeaderToContext(context.Background(), nil)) {
		t.Error("nil header should not promote")
	}
}

func TestWithoutCancel_PreservesShadow(t *testing.T) {
	parent, cancel := context.WithCancel(WithShadow(context.Background(), true))
	cancel() // immediately cancel parent
	detached := WithoutCancel(parent)
	if !IsShadow(detached) {
		t.Fatal("detached ctx should preserve shadow flag")
	}
	if detached.Err() != nil {
		t.Fatalf("detached ctx should not propagate cancel; got %v", detached.Err())
	}
	select {
	case <-detached.Done():
		t.Fatal("detached ctx Done() should never fire")
	default:
	}
}
