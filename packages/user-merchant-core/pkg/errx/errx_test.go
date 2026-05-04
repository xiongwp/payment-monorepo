package errx

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	errNotFound = errors.New("not found")
	errInvalid  = errors.New("invalid")
)

func TestMap_NilPassesThrough(t *testing.T) {
	Clear()
	if Map(nil) != nil {
		t.Fatal("nil should map to nil")
	}
}

func TestMap_UnregisteredFallsBackToInternal(t *testing.T) {
	Clear()
	err := errors.New("boom")
	got := Map(err)
	st, _ := status.FromError(got)
	if st.Code() != codes.Internal {
		t.Fatalf("want Internal, got %s", st.Code())
	}
}

func TestMap_DirectMatch(t *testing.T) {
	Clear()
	Register(errNotFound, codes.NotFound)
	got := Map(errNotFound)
	st, _ := status.FromError(got)
	if st.Code() != codes.NotFound {
		t.Fatalf("want NotFound, got %s", st.Code())
	}
}

func TestMap_WrappedByFmtErrorf(t *testing.T) {
	Clear()
	Register(errInvalid, codes.InvalidArgument)
	wrapped := fmt.Errorf("validate name: %w", errInvalid)
	got := Map(wrapped)
	st, _ := status.FromError(got)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %s (msg=%s)", st.Code(), st.Message())
	}
	if st.Message() != wrapped.Error() {
		t.Fatalf("message should retain wrap, got %s", st.Message())
	}
}

func TestMap_FirstMatchWins(t *testing.T) {
	// 先注册的命中。注册顺序代表"具体 → 通用"，调用方应当按此顺序注册。
	Clear()
	Register(errNotFound, codes.NotFound)
	Register(errors.New("never reached"), codes.Unknown) // 匹配不上，无影响
	got := Map(errNotFound)
	st, _ := status.FromError(got)
	if st.Code() != codes.NotFound {
		t.Fatalf("want NotFound, got %s", st.Code())
	}
}

func TestRegister_NilTargetIgnored(t *testing.T) {
	Clear()
	Register(nil, codes.NotFound) // no panic, no entry
	got := Map(errors.New("x"))
	st, _ := status.FromError(got)
	if st.Code() != codes.Internal {
		t.Fatalf("want Internal (fallback), got %s", st.Code())
	}
}
