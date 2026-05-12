package scaffold

import (
	"context"
	"testing"
)

func TestTraceID_RoundTrip(t *testing.T) {
	ctx := WithTraceID(context.Background(), "trace_abc123")
	if got := TraceIDFromCtx(ctx); got != "trace_abc123" {
		t.Errorf("trace_id round-trip = %q", got)
	}
}

func TestTraceID_EmptyCtx(t *testing.T) {
	if got := TraceIDFromCtx(context.Background()); got != "" {
		t.Errorf("empty ctx returned %q, want empty", got)
	}
}

func TestNewTraceID_Unique(t *testing.T) {
	a := newTraceID()
	b := newTraceID()
	if a == b {
		t.Error("collision in newTraceID")
	}
	if len(a) != 32 { // 16 bytes hex
		t.Errorf("trace_id len = %d, want 32", len(a))
	}
}
