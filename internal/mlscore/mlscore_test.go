package mlscore

import (
	"context"
	"testing"
)

func TestNoopService_AlwaysZero(t *testing.T) {
	r, err := NoopService{}.Score(context.Background(), Features{Amount: 100})
	if err != nil {
		t.Fatal(err)
	}
	if r.Score != 0 {
		t.Fatalf("expected 0 score, got %f", r.Score)
	}
	if r.ModelVer != "noop" {
		t.Fatalf("expected modelVer noop, got %s", r.ModelVer)
	}
}

func TestFixedService_ReturnsConfigured(t *testing.T) {
	s := NewFixedService(0.85, "test-v1")
	r, err := s.Score(context.Background(), Features{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Score != 0.85 || r.ModelVer != "test-v1" {
		t.Fatalf("got %+v", r)
	}
}

func TestFixedService_CallsCount(t *testing.T) {
	s := NewFixedService(0.5, "v1")
	for i := 0; i < 5; i++ {
		s.Score(context.Background(), Features{})
	}
	if got := s.Calls(); got != 5 {
		t.Fatalf("expected 5 calls, got %d", got)
	}
}
