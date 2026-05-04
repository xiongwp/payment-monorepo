package eventbus

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestMemBus_PubSubBasic(t *testing.T) {
	b := NewMemBus(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan Event, 4)
	go func() {
		_ = b.Subscribe(ctx, TopicScreen, "g", "c1", func(_ context.Context, ev Event) error {
			got <- ev
			return nil
		})
	}()

	// 给 subscribe goroutine 一点时间注册
	time.Sleep(20 * time.Millisecond)

	if err := b.Publish(ctx, Event{Topic: TopicScreen, Key: "k1", Data: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(ctx, Event{Topic: TopicScreen, Key: "k2"}); err != nil {
		t.Fatal(err)
	}
	// 不订阅这个 topic 的事件应该不被收到
	if err := b.Publish(ctx, Event{Topic: TopicReport, Key: "irrelevant"}); err != nil {
		t.Fatal(err)
	}

	count := 0
	for count < 2 {
		select {
		case ev := <-got:
			if ev.Topic != TopicScreen {
				t.Errorf("wrong topic: %v", ev.Topic)
			}
			count++
		case <-time.After(time.Second):
			t.Fatalf("only got %d events", count)
		}
	}
	// 等 100ms 确保 TopicReport 事件没有偷溜进来
	select {
	case ev := <-got:
		t.Errorf("unexpected leak: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestMemBus_FanoutMultipleSubscribers(t *testing.T) {
	b := NewMemBus(4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	gotCounts := map[string]int{}
	for _, name := range []string{"sub1", "sub2"} {
		n := name
		go func() {
			_ = b.Subscribe(ctx, TopicReport, "g", n, func(_ context.Context, _ Event) error {
				mu.Lock()
				gotCounts[n]++
				mu.Unlock()
				return nil
			})
		}()
	}
	time.Sleep(20 * time.Millisecond)

	for i := 0; i < 3; i++ {
		_ = b.Publish(ctx, Event{Topic: TopicReport, Key: "k"})
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"sub1", "sub2"} {
		if gotCounts[name] != 3 {
			t.Errorf("%s: expected 3, got %d", name, gotCounts[name])
		}
	}
}

func TestMemBus_PublishOccurAtFilledIfZero(t *testing.T) {
	b := NewMemBus(4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Event, 1)
	go func() {
		_ = b.Subscribe(ctx, TopicHit, "g", "c", func(_ context.Context, ev Event) error {
			got <- ev
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	before := time.Now()
	_ = b.Publish(ctx, Event{Topic: TopicHit})
	ev := <-got
	if ev.OccurAt.IsZero() || ev.OccurAt.Before(before.Add(-time.Second)) {
		t.Errorf("OccurAt not set: %v", ev.OccurAt)
	}
}

func TestNoopBus_PublishNeverErrors(t *testing.T) {
	var b Bus = NoopBus{}
	if err := b.Publish(context.Background(), Event{Topic: TopicScreen}); err != nil {
		t.Fatal(err)
	}
}
