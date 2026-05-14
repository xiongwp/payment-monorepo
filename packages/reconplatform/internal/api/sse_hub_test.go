package api

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 不需要真 Redis: 直接 push 到 broadcast 测 fan-out 逻辑.
//
// 假设场景: 100 个订阅者, 1000 events 全部 broadcast.

func TestBroadcastHub_FanOut(t *testing.T) {
	h := NewBroadcastHub(nil, nil)
	// 启动多个订阅者
	const N = 10
	chans := make([]<-chan []byte, N)
	unsubs := make([]func(), N)
	for i := 0; i < N; i++ {
		_, c, u := h.Subscribe()
		chans[i] = c
		unsubs[i] = u
	}

	// 直接 broadcast (绕过 Redis)
	const M = 50
	for i := 0; i < M; i++ {
		h.broadcast("id_"+itoa(i), []byte(`{"k":"v"}`))
	}

	// 每个订阅者应收到 M 条
	for i, ch := range chans {
		got := 0
		for j := 0; j < M; j++ {
			select {
			case <-ch:
				got++
			case <-time.After(100 * time.Millisecond):
				t.Fatalf("subscriber %d only got %d/%d", i, got, M)
			}
		}
	}
	for _, u := range unsubs {
		u()
	}
}

func TestBroadcastHub_SlowConsumerDoesNotBlock(t *testing.T) {
	h := NewBroadcastHub(nil, nil)
	_, _, unsub := h.Subscribe() // 不消费此 channel,模拟慢客户端
	defer unsub()

	// 发 1000 条,channel 容量只有 100,后 900 条应被 drop
	start := time.Now()
	for i := 0; i < 1000; i++ {
		h.broadcast("id", []byte("x"))
	}
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("broadcast blocked: %v", elapsed)
	}
	stats := h.Stats()
	if stats.Dropped == 0 {
		t.Error("slow consumer should cause drops")
	}
}

func TestBroadcastHub_UnsubscribeReleasesChannel(t *testing.T) {
	h := NewBroadcastHub(nil, nil)
	id, ch, unsub := h.Subscribe()
	if h.Stats().Subscribers != 1 {
		t.Errorf("expect 1 subscriber")
	}
	unsub()
	// channel 应已 close
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed after unsub")
		}
	case <-time.After(50 * time.Millisecond):
		t.Error("channel not closed after unsub")
	}
	if h.Stats().Subscribers != 0 {
		t.Errorf("expect 0 subscribers, got %d", h.Stats().Subscribers)
	}
	_ = id
}

func TestBroadcastHub_ConcurrentSubscribeUnsubscribe(t *testing.T) {
	h := NewBroadcastHub(nil, nil)
	var active atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, unsub := h.Subscribe()
			active.Add(1)
			time.Sleep(time.Duration(i%5+1) * time.Millisecond)
			unsub()
			active.Add(-1)
		}()
	}
	// 同时 broadcast
	stopBroadcast := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopBroadcast:
				return
			default:
				h.broadcast("id", []byte("x"))
			}
		}
	}()
	wg.Wait()
	close(stopBroadcast)
	if h.Stats().Subscribers != 0 {
		t.Errorf("subscribers leak: %d", h.Stats().Subscribers)
	}
}

// helpers
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
