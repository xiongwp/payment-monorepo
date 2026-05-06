// hub.go：进程内 fan-out — admin PutConfig 通知所有订阅本 namespace 的
// watcher。多 server 副本时单 hub 跨不到对端实例；v1 简化为单实例 + 客户端
// since_version resume 兜底跨 server 的最终一致。
package service

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"sync"
	"sync/atomic"
)

// EventType watch 流事件类型，跟 SDK / proto 对齐。
type EventType int

const (
	EventUnknown EventType = 0
	EventSnapshot EventType = 1
	EventUpdate  EventType = 2
	EventDelete  EventType = 3
)

// Event 单个变更事件。
type Event struct {
	Type   EventType
	Config *ConfigRow
}

// matchFn 给 hub.Subscribe 的过滤器：判断该 event 是否要推给该 instance。
type matchFn func(*ConfigRow, string) bool

// watcherHub 单 namespace 的订阅管理。
type watcherHub struct {
	mu        sync.RWMutex
	subs      map[string]map[*subscriber]struct{} // namespace → set of subscribers
	closed    atomic.Bool
}

type subscriber struct {
	instanceID string
	out        chan *Event
	match      matchFn
}

func newWatcherHub() *watcherHub {
	return &watcherHub{subs: make(map[string]map[*subscriber]struct{})}
}

// Subscribe 注册一个 watcher；ctx 取消自动 unregister。
// 调用方负责把 events 从 out 读出来。
func (h *watcherHub) Subscribe(ctx context.Context, namespace, instanceID string, out chan *Event, match matchFn) {
	if h.closed.Load() {
		return
	}
	sub := &subscriber{instanceID: instanceID, out: out, match: match}
	h.mu.Lock()
	if h.subs[namespace] == nil {
		h.subs[namespace] = make(map[*subscriber]struct{})
	}
	h.subs[namespace][sub] = struct{}{}
	h.mu.Unlock()

	// 等 ctx Done 后清理
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		if set := h.subs[namespace]; set != nil {
			delete(set, sub)
			if len(set) == 0 {
				delete(h.subs, namespace)
			}
		}
		h.mu.Unlock()
	}()
}

// Publish 发一个事件给本 namespace 所有订阅；非阻塞（subscriber chan 满直接丢
// 该订阅，由客户端 since_version 重连时补齐）。
func (h *watcherHub) Publish(namespace string, ev *Event) {
	if h.closed.Load() {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.subs[namespace] {
		// 策略过滤：CANARY / TARGETED 时只推命中的 instance
		if sub.match != nil && ev.Config != nil && !sub.match(ev.Config, sub.instanceID) {
			continue
		}
		select {
		case sub.out <- ev:
		default:
			// channel full → drop。客户端断开重连时 since_version > 0 会
			// 让 server 拉漏掉的 version 增量补上。
		}
	}
}

// Close 停止所有 publish。
func (h *watcherHub) Close() { h.closed.Store(true) }

// ─── strategy match helpers ─────────────────────────────────────────────

// CanarySpec admin 写 strategy=CANARY 时的 spec 反序列化目标。
//
// percent: 0..100 — instance_id fnv64 mod 100 < percent 命中（稳定 hash，
//          同 instance 在多个 canary 间命中关系稳定）
// target_instance_ids: 显式名单，命中即放行（与 percent 并列 OR 关系）
//
// 同 namespace 多个并存的 CANARY 配置时：server 取最新一条匹配（priority 高优先），
// 不命中则继续找；都不命中走兜底 active。
type CanarySpec struct {
	Percent           int      `json:"percent"`
	TargetInstanceIds []string `json:"target_instance_ids"`
	Priority          int      `json:"priority"`
}

// TargetedSpec strategy=TARGETED 时的 spec。严格白名单，不在内的 instance 不收。
type TargetedSpec struct {
	InstanceIds []string `json:"instance_ids"`
}

// matchCanary StrategySpec JSON ↔ CanarySpec → 命中判定。
//
// 解析失败 → 不命中（保守）。生产 admin 写入时会先 json.Unmarshal 校验过，
// 这里再校一次防 DB 直改 / 旧 schema 漂移。
func matchCanary(specJSON, instanceID string) bool {
	if specJSON == "" {
		return false
	}
	var spec CanarySpec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		return false
	}
	for _, id := range spec.TargetInstanceIds {
		if id == instanceID {
			return true
		}
	}
	if spec.Percent > 0 && spec.Percent <= 100 {
		h := fnv.New64a()
		_, _ = h.Write([]byte(instanceID))
		bucket := int(h.Sum64() % 100)
		if bucket < spec.Percent {
			return true
		}
	}
	return false
}

// matchTargeted strategy=TARGETED 严格白名单。
func matchTargeted(specJSON, instanceID string) bool {
	if specJSON == "" {
		return false
	}
	var spec TargetedSpec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		return false
	}
	for _, id := range spec.InstanceIds {
		if id == instanceID {
			return true
		}
	}
	return false
}

