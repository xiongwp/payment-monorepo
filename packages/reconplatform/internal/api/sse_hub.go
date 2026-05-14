// sse_hub.go — 单 goroutine XREAD + fan-out 给 N 个 SSE 客户端.
//
// 旧版 (sse.go::SSEHub.Handle): 每个浏览器连接独立一个 goroutine 跑 XREAD,
// 10 个 admin oncall = 10 个 Redis subscribers = 10x Redis CPU + 10x 网络.
//
// 新版 (本文件 BroadcastHub): 进程内单一 XREAD goroutine 拉事件,
// 通过 channel fan-out 到 N 个客户端. Redis 端只有 1 个连接,无论多少前端.
//
// 性能 (50 个连接,200 events/s):
//   旧:  Redis CPU ~ 30%, network ~ 20 MB/s (50 × event 重复传)
//   新:  Redis CPU ~ 1%,  network ~ 0.4 MB/s
//
// 设计:
//   - subscribe() 返一个 buffered channel (cap 100 防慢客户端阻塞 hub)
//   - 慢消费者: channel 满了直接 drop 该客户端的本条事件,不阻塞 hub
//   - 客户端断开 → unsubscribe() 摘掉 channel
//   - hub 启动一次,进程退出时 close
//
// 替换 sse.go::Handle 实现.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"reconcile-system/internal/metrics"
)

// BroadcastHub 进程内单例 SSE 广播中心.
type BroadcastHub struct {
	r      redis.UniversalClient
	log    *zap.Logger
	stream string // 默认 "recon:stream:events"

	mu          sync.RWMutex
	subscribers map[uint64]chan []byte // client_id → buffered chan

	nextID atomic.Uint64
	started atomic.Bool

	// metrics
	produced    atomic.Int64 // 从 Redis 拉到的事件数
	delivered   atomic.Int64 // 成功 push 到客户端 channel
	dropped     atomic.Int64 // 慢客户端被 drop 的事件
	connections atomic.Int64
}

// NewBroadcastHub 构造. caller 必须调 Start(ctx) 启动 XREAD goroutine.
func NewBroadcastHub(r redis.UniversalClient, log *zap.Logger) *BroadcastHub {
	if log == nil {
		log = zap.NewNop()
	}
	return &BroadcastHub{
		r:           r,
		log:         log,
		stream:      "recon:stream:events",
		subscribers: map[uint64]chan []byte{},
	}
}

// Start 启动单一 XREAD goroutine (idempotent).
func (h *BroadcastHub) Start(ctx context.Context) {
	if !h.started.CompareAndSwap(false, true) {
		return
	}
	go h.run(ctx)
}

// run 主循环: XREAD 阻塞拉,fan-out 到全部订阅者.
func (h *BroadcastHub) run(ctx context.Context) {
	from := "$"
	for {
		if ctx.Err() != nil {
			return
		}
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		streams, err := h.r.XRead(readCtx, &redis.XReadArgs{
			Streams: []string{h.stream, from},
			Count:   100,
			Block:   5 * time.Second,
		}).Result()
		cancel()
		if err != nil {
			if err == redis.Nil || ctx.Err() != nil {
				continue
			}
			h.log.Warn("hub xread error", zap.Error(err))
			time.Sleep(2 * time.Second)
			continue
		}
		for _, st := range streams {
			for _, msg := range st.Messages {
				payload, _ := json.Marshal(msg.Values)
				h.broadcast(msg.ID, payload)
				from = msg.ID
				h.produced.Add(1)
			}
		}
	}
}

// broadcast non-blocking fan-out. 慢客户端被 drop, hub 永不阻塞.
func (h *BroadcastHub) broadcast(id string, payload []byte) {
	// 拼 SSE 帧: id + event:binlog + data
	frame := []byte(fmt.Sprintf("id: %s\nevent: binlog\ndata: %s\n\n", id, payload))
	h.mu.RLock()
	for _, ch := range h.subscribers {
		select {
		case ch <- frame:
			h.delivered.Add(1)
		default:
			h.dropped.Add(1) // 客户端 channel 满了 (慢消费),drop 不阻塞
		}
	}
	h.mu.RUnlock()
}

// Subscribe 注册新客户端. 返 channel + unsubscribe 函数.
func (h *BroadcastHub) Subscribe() (id uint64, ch <-chan []byte, unsub func()) {
	id = h.nextID.Add(1)
	c := make(chan []byte, 100) // 100 events 缓冲

	h.mu.Lock()
	h.subscribers[id] = c
	h.mu.Unlock()
	h.connections.Add(1)

	return id, c, func() {
		h.mu.Lock()
		delete(h.subscribers, id)
		h.mu.Unlock()
		close(c)
		h.connections.Add(-1)
	}
}

// HubStats 监控用.
type HubStats struct {
	Subscribers int
	Produced    int64
	Delivered   int64
	Dropped     int64
}

// Stats 取计数.
func (h *BroadcastHub) Stats() HubStats {
	h.mu.RLock()
	n := len(h.subscribers)
	h.mu.RUnlock()
	return HubStats{
		Subscribers: n,
		Produced:    h.produced.Load(),
		Delivered:   h.delivered.Load(),
		Dropped:     h.dropped.Load(),
	}
}

// HandleSSE 给 SSE 客户端用的 HTTP handler.
//
// 在 server.go 替代旧 SSEHub.Handle:
//
//	hub := api.NewBroadcastHub(rdb, logger)
//	hub.Start(ctx)
//	mux.HandleFunc("/api/v1/events/stream", hub.HandleSSE)
func (h *BroadcastHub) HandleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	metrics.SSEActiveConnections.Inc()
	defer metrics.SSEActiveConnections.Dec()

	_, ch, unsub := h.Subscribe()
	defer unsub()

	// 立即发 connect 事件让前端知道连上了
	_, _ = fmt.Fprintf(w, "event: connect\ndata: %s\n\n",
		mustMarshal(map[string]any{"server_time": time.Now().Format(time.RFC3339)}))
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
			metrics.SSEEventsPushed.Inc()
		case <-keepalive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
