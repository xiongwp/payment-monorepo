// sse.go — Server-Sent Events 实时事件流给 admin web。
//
// /api/v1/events/stream 让浏览器用 EventSource 订阅 reconplatform 实时事件：
//   - binlog event 涌入（recon:stream:events）
//   - 脚本运行产出 diff（diff_created）
//   - diff 状态迁移（diff_transitioned）
//
// 价值：运营打开 admin web 主面板 → 直接看到数据流"在动"，不用刷新。
// 比 Redis MONITOR 命令安全（限定 channel）+ 比 polling 省（push）。
//
// 实现：
//   - HTTP 长连接，Content-Type: text/event-stream
//   - 后台 goroutine XREAD recon:stream:events $（最新事件）
//   - 客户端每 25s 收一个 :keepalive 注释保活防 LB 切
//   - 客户端断开 → request ctx 取消 → goroutine 退出
//
// 不接 PubSub：Redis Stream 的 XREAD blocking 比 PubSub 更可靠（消费者断开
// 重连时不丢事件，能从上次 ID 续读）。
//
// admin web 用法：
//   const ev = new EventSource('/api/v1/events/stream');
//   ev.addEventListener('binlog', e => console.log(JSON.parse(e.data)));
//   ev.addEventListener('diff', e => console.log(JSON.parse(e.data)));

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// SSEHub 持有 Redis 连接 + 给 server.go 路由用。
type SSEHub struct {
	r   redis.UniversalClient
	log *zap.Logger
}

// NewSSEHub 构造。
func NewSSEHub(r redis.UniversalClient, log *zap.Logger) *SSEHub {
	if log == nil {
		log = zap.NewNop()
	}
	return &SSEHub{r: r, log: log}
}

// Handle GET /api/v1/events/stream — SSE 长连接处理函数。
//
// Query params:
//   from=<stream_id>  从指定 ID 起读（断线重连用），默认 $（最新）
//
// 协议:
//   event: binlog
//   data: {"svc":"order-core","table":"payment_intents","pk":"pi_xxx","op":"INSERT","ts":...}
//   id: 1715000000000-0
func (h *SSEHub) Handle(w http.ResponseWriter, r *http.Request) {
	// SSE 必备 header
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 关 nginx 缓冲

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	from := r.URL.Query().Get("from")
	if from == "" {
		from = "$" // 仅订阅未来事件
	}

	// 立刻冲一条 connect 事件让客户端知道连上了
	_, _ = fmt.Fprintf(w, "event: connect\ndata: %s\n\n",
		mustMarshal(map[string]any{"server_time": time.Now().Format(time.RFC3339)}))
	flusher.Flush()

	// 发送 keepalive 注释 防 LB 切（25s < typical 60s LB idle）
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		default:
		}

		// XREAD 阻塞 5s 等新事件（短 block 让 keepalive 也能被调度）
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		streams, err := h.r.XRead(readCtx, &redis.XReadArgs{
			Streams: []string{"recon:stream:events", from},
			Count:   50,
			Block:   5 * time.Second,
		}).Result()
		cancel()

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if err == redis.Nil {
				continue // 5s 内没新事件，继续 block
			}
			h.log.Warn("SSE XRead error", zap.Error(err))
			time.Sleep(2 * time.Second)
			continue
		}

		for _, st := range streams {
			for _, msg := range st.Messages {
				payload := mustMarshal(msg.Values)
				_, err := fmt.Fprintf(w, "id: %s\nevent: binlog\ndata: %s\n\n",
					msg.ID, payload)
				if err != nil {
					return
				}
				from = msg.ID
			}
		}
		flusher.Flush()
	}
}

func mustMarshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
