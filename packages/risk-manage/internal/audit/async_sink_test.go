package audit

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// asyncRecordingSink 记录被写过的 audit + 是否调过 WriteBatch。用于验证
// AsyncBatchSink 把 record 正确传给 inner。
type asyncRecordingSink struct {
	mu          sync.Mutex
	got         []*DecisionAudit
	batchCalls  int
	writeCalls  int
	writeDelay  time.Duration
}

func (s *asyncRecordingSink) Write(_ context.Context, a *DecisionAudit) {
	if s.writeDelay > 0 {
		time.Sleep(s.writeDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, a)
	s.writeCalls++
}

func (s *asyncRecordingSink) snapshot() []*DecisionAudit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*DecisionAudit, len(s.got))
	copy(out, s.got)
	return out
}

// asyncBatchingSink 实现 BatchSink，让 AsyncBatchSink 走批量路径
type asyncBatchingSink struct {
	asyncRecordingSink
}

func (s *asyncBatchingSink) WriteBatch(_ context.Context, batch []*DecisionAudit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, batch...)
	s.batchCalls++
}

func TestAsyncBatchSink_Forwards(t *testing.T) {
	inner := &asyncRecordingSink{}
	s := NewAsyncBatchSink(inner, 100, 10, 50*time.Millisecond, zap.NewNop())
	defer s.Stop()
	for i := 0; i < 5; i++ {
		s.Write(context.Background(), mkAudit(string(rune('a'+i))))
	}
	// 等 flush_interval 触发
	time.Sleep(100 * time.Millisecond)
	if got := inner.snapshot(); len(got) != 5 {
		t.Fatalf("expected 5 forwarded; got %d", len(got))
	}
}

func TestAsyncBatchSink_BatchSinkInterfaceUsed(t *testing.T) {
	inner := &asyncBatchingSink{}
	s := NewAsyncBatchSink(inner, 100, 5, 200*time.Millisecond, zap.NewNop())
	defer s.Stop()
	for i := 0; i < 5; i++ {
		s.Write(context.Background(), mkAudit(string(rune('a'+i))))
	}
	// batchSize=5 触发立即 flush
	time.Sleep(50 * time.Millisecond)
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if inner.batchCalls != 1 {
		t.Fatalf("expected 1 batch call (size match); got %d batch + %d write", inner.batchCalls, inner.writeCalls)
	}
	if len(inner.got) != 5 {
		t.Fatalf("expected 5 records; got %d", len(inner.got))
	}
}

func TestAsyncBatchSink_QueueFullDrops(t *testing.T) {
	// inner 慢 → queue 撑满 → drop
	inner := &asyncRecordingSink{writeDelay: 50 * time.Millisecond}
	s := NewAsyncBatchSink(inner, 5, 100, 1*time.Second, zap.NewNop())
	defer s.Stop()
	for i := 0; i < 100; i++ {
		s.Write(context.Background(), mkAudit("x"))
	}
	if s.Dropped() == 0 {
		t.Fatal("expected drops when queue is full + inner is slow")
	}
}

func TestAsyncBatchSink_StopFlushesQueue(t *testing.T) {
	inner := &asyncRecordingSink{}
	s := NewAsyncBatchSink(inner, 100, 1000, 10*time.Second, zap.NewNop())
	for i := 0; i < 10; i++ {
		s.Write(context.Background(), mkAudit("x"))
	}
	s.Stop() // 应该 flush 剩余 10 条
	if got := inner.snapshot(); len(got) != 10 {
		t.Fatalf("Stop should flush queue; got %d", len(got))
	}
}

func TestAsyncBatchSink_NilSafe(t *testing.T) {
	var s *AsyncBatchSink
	s.Write(context.Background(), mkAudit("x"))
	if s.Dropped() != 0 {
		t.Fatal("nil sink dropped should be 0")
	}
	if s.QueueLen() != 0 {
		t.Fatal("nil sink queue len should be 0")
	}
	s.Stop() // 不该 panic
}

// inner panic 不影响后续 batch
func TestAsyncBatchSink_InnerPanicRecovered(t *testing.T) {
	panicSink := &panickingSink{}
	s := NewAsyncBatchSink(panicSink, 10, 2, 50*time.Millisecond, zap.NewNop())
	defer s.Stop()
	s.Write(context.Background(), mkAudit("a"))
	s.Write(context.Background(), mkAudit("b"))
	// 等 flush
	time.Sleep(100 * time.Millisecond)
	// 还能继续写不死
	s.Write(context.Background(), mkAudit("c"))
	s.Write(context.Background(), mkAudit("d"))
	time.Sleep(100 * time.Millisecond)
}

type panickingSink struct{}

func (panickingSink) Write(_ context.Context, _ *DecisionAudit) {
	panic("simulated inner sink failure")
}
func (panickingSink) WriteBatch(_ context.Context, _ []*DecisionAudit) {
	panic("simulated batch failure")
}
