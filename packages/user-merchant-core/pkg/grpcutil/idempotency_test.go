package grpcutil

import (
	"context"
	"sync"
	"testing"
	"time"

	usermerchantv1 "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type memStore struct {
	mu   sync.Mutex
	data map[string]*IdempotencyRecord
}

func newMem() *memStore { return &memStore{data: map[string]*IdempotencyRecord{}} }

func (m *memStore) key(k, method string) string { return k + "|" + method }

func (m *memStore) Get(_ context.Context, k, method string) (*IdempotencyRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[m.key(k, method)]
	return v, ok, nil
}

func (m *memStore) TryInsert(_ context.Context, r *IdempotencyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data[m.key(r.Key, r.Method)]; ok {
		return ErrIdempotencyExists
	}
	m.data[m.key(r.Key, r.Method)] = r
	return nil
}

func ctxWithKey(key string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("idempotency-key", key))
}

func TestIdempotency_NoHeaderPassThrough(t *testing.T) {
	iv := IdempotencyInterceptor(IdempotencyOptions{HeaderName: "idempotency-key", Store: newMem(), TTL: time.Hour})
	calls := 0
	req := &usermerchantv1.GetMerchantRequest{Id: "x"}
	handler := func(context.Context, interface{}) (interface{}, error) {
		calls++
		return &usermerchantv1.GetMerchantResponse{}, nil
	}
	_, _ = iv(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/m"}, handler)
	if calls != 1 {
		t.Fatalf("expected handler to run once, got %d", calls)
	}
}

func TestIdempotency_MethodFilter(t *testing.T) {
	iv := IdempotencyInterceptor(IdempotencyOptions{
		HeaderName:   "idempotency-key",
		Store:        newMem(),
		MethodFilter: map[string]struct{}{"/want": {}},
		TTL:          time.Hour,
	})
	calls := 0
	ctx := ctxWithKey("k1")
	req := &usermerchantv1.GetMerchantRequest{Id: "x"}
	handler := func(context.Context, interface{}) (interface{}, error) {
		calls++
		return &usermerchantv1.GetMerchantResponse{}, nil
	}
	_, _ = iv(ctx, req, &grpc.UnaryServerInfo{FullMethod: "/other"}, handler)
	_, _ = iv(ctx, req, &grpc.UnaryServerInfo{FullMethod: "/other"}, handler)
	if calls != 2 {
		t.Fatalf("non-whitelisted method should run both times, got %d", calls)
	}
}

func TestIdempotency_ReplayReturnsAlreadyExists(t *testing.T) {
	store := newMem()
	iv := IdempotencyInterceptor(IdempotencyOptions{HeaderName: "idempotency-key", Store: store, TTL: time.Hour})
	ctx := ctxWithKey("k1")
	info := &grpc.UnaryServerInfo{FullMethod: "/m"}
	req := &usermerchantv1.GetMerchantRequest{Id: "x"}
	calls := 0
	handler := func(context.Context, interface{}) (interface{}, error) {
		calls++
		return &usermerchantv1.GetMerchantResponse{}, nil
	}
	if _, err := iv(ctx, req, info, handler); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := iv(ctx, req, info, handler)
	if err == nil {
		t.Fatal("expected replay error")
	}
	if s, _ := status.FromError(err); s.Code() != codes.AlreadyExists {
		t.Fatalf("want AlreadyExists, got %s", s.Code())
	}
	if calls != 1 {
		t.Fatalf("handler should only run once, got %d", calls)
	}
}

func TestIdempotency_FailedPreconditionOnBodyMismatch(t *testing.T) {
	store := newMem()
	iv := IdempotencyInterceptor(IdempotencyOptions{HeaderName: "idempotency-key", Store: store, TTL: time.Hour})
	ctx := ctxWithKey("k1")
	info := &grpc.UnaryServerInfo{FullMethod: "/m"}
	handler := func(context.Context, interface{}) (interface{}, error) {
		return &usermerchantv1.GetMerchantResponse{}, nil
	}
	// Seed with body {Id: "a"}
	if _, err := iv(ctx, &usermerchantv1.GetMerchantRequest{Id: "a"}, info, handler); err != nil {
		t.Fatal(err)
	}
	// Second call with same key but different Id → body hash mismatch → FailedPrecondition.
	_, err := iv(ctx, &usermerchantv1.GetMerchantRequest{Id: "b"}, info, handler)
	if err == nil {
		t.Fatal("expected body-mismatch error")
	}
	if s, _ := status.FromError(err); s.Code() != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %s (%v)", s.Code(), err)
	}
}
