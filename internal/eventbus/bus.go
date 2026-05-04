// Package eventbus —— 风控事件总线。
//
// risk-manage 内部事件（Screen 决策、Report 入账、规则命中、外部 webhook 触发）
// 走统一发布接口；订阅方可以是同进程 worker（feature 预计算、metrics 累加），
// 也可以是跨进程消费者（ML 训练、审计、外部告警）。
//
// 设计目标：
//
//  1. 上游业务路径（Screen / Report）只调 Publisher.Publish；不感知订阅者。
//  2. 订阅方故障不阻断 Publish（fail-open；事件丢就丢，由 Stream 自身的
//     consumer-group ack 重试机制承担可靠性）。
//  3. Mem 实现给单测 / dev / 单进程部署用；Redis Stream 实现给生产用。
//
// 事件 schema：Event 是个 envelope，data 是 []byte 由 publisher 序列化。
// 用 JSON 而不是 protobuf 让外部消费者（python ML pipeline / 运维脚本）
// 不必跟 risk-manage 拉同款 .proto 编译。
package eventbus

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Topic 事件主题。一对一映射 Redis Stream 名（前缀 + topic）。
type Topic string

const (
	TopicScreen Topic = "screen"  // Screen 决策结果
	TopicReport Topic = "report"  // Report 上报（成功 / 失败 / chargeback）
	TopicHit    Topic = "rulehit" // 规则命中（每条命中规则一条事件）
)

// Event 总线 envelope。Data 由 publisher 序列化，consumer 反序列化。
type Event struct {
	Topic     Topic     `json:"topic"`
	OccurAt   time.Time `json:"occur_at"`
	// Key 路由键。Redis Stream 不分片此字段，但 worker 可以按 key 做 consistent
	// 哈希到本地 partition；也作为可观测维度（per-merchant / per-customer 流量）。
	Key  string          `json:"key,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Publisher 上游写。
type Publisher interface {
	Publish(ctx context.Context, ev Event) error
}

// Handler 事件处理函数。返回 error 表示处理失败 → consumer-group 不 ACK，
// Stream 实现会重投递（按 implementation 决定 backoff）。
type Handler func(ctx context.Context, ev Event) error

// Subscriber 下游订。Subscribe 启动后台 goroutine 拉事件并调 Handler。
// 一次性接口：cancel 由调用方持有的 ctx 控制。
type Subscriber interface {
	Subscribe(ctx context.Context, topic Topic, group, consumer string, h Handler) error
}

// Bus 同时支持发布 + 订阅。
type Bus interface {
	Publisher
	Subscriber
}

// ─── Noop ───────────────────────────────────────────────────────────────────

// NoopBus 关掉总线时使用：Publish 直接 nil，Subscribe 永远 block 在 ctx。
type NoopBus struct{}

func (NoopBus) Publish(_ context.Context, _ Event) error { return nil }
func (NoopBus) Subscribe(ctx context.Context, _ Topic, _ string, _ string, _ Handler) error {
	<-ctx.Done()
	return ctx.Err()
}

// ─── Mem 实现（dev / 单测 / 单进程） ─────────────────────────────────────────

// MemBus 进程内 channel-fanout 总线。订阅者掉一条丢一条；Publisher 不阻塞。
//
// 不保证 ack 重试；不持久化。生产请用 Redis Stream 实现。
type MemBus struct {
	mu   sync.RWMutex
	subs map[Topic][]chan Event
	// bufSize per-subscriber channel buffer 大小。0 = unbuffered（slow consumer
	// 会阻塞所有人 → 通常不用）。默认 1024。
	bufSize int
}

func NewMemBus(bufSize int) *MemBus {
	if bufSize <= 0 {
		bufSize = 1024
	}
	return &MemBus{subs: make(map[Topic][]chan Event), bufSize: bufSize}
}

func (b *MemBus) Publish(_ context.Context, ev Event) error {
	if ev.OccurAt.IsZero() {
		ev.OccurAt = time.Now()
	}
	b.mu.RLock()
	chs := append([]chan Event(nil), b.subs[ev.Topic]...)
	b.mu.RUnlock()
	for _, ch := range chs {
		select {
		case ch <- ev:
		default:
			// 满 → 丢；不阻塞 publisher。生产用 Redis Stream 时由 stream 自身
			// 的 max-len 实现背压。
		}
	}
	return nil
}

func (b *MemBus) Subscribe(ctx context.Context, topic Topic, _ string, _ string, h Handler) error {
	ch := make(chan Event, b.bufSize)
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		out := b.subs[topic][:0]
		for _, c := range b.subs[topic] {
			if c != ch {
				out = append(out, c)
			}
		}
		b.subs[topic] = out
		b.mu.Unlock()
		close(ch)
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-ch:
			// Handler 错误只 log（caller 可在 h 内自己处理重试）；Mem 实现不做 ack。
			_ = h(ctx, ev)
		}
	}
}
