// hub.go：进程内 fan-out — admin PutConfig 通知所有订阅本 namespace 的
// watcher。多 server 副本时单 hub 跨不到对端实例；v1 简化为单实例 + 客户端
// since_version resume 兜底跨 server 的最终一致。
package service

import (
	"context"
	"hash/fnv"
	"strings"
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

// matchCanary StrategySpec 是 CanarySpec 的 JSON：{"percent":10,"target_instance_ids":["a","b"]}
//
//	percent > 0：用 fnv 哈希 instance_id mod 100 < percent 命中
//	target_instance_ids 非空：包含 instance_id 时命中
//	两者 OR，任一即放行
func matchCanary(spec, instanceID string) bool {
	if spec == "" {
		return false
	}
	c := parseCanary(spec)
	for _, id := range c.targetIDs {
		if id == instanceID {
			return true
		}
	}
	if c.percent > 0 && c.percent <= 100 {
		h := fnv.New64a()
		_, _ = h.Write([]byte(instanceID))
		bucket := int(h.Sum64() % 100)
		if bucket < c.percent {
			return true
		}
	}
	return false
}

func matchTargeted(spec, instanceID string) bool {
	if spec == "" {
		return false
	}
	t := parseTargeted(spec)
	for _, id := range t.ids {
		if id == instanceID {
			return true
		}
	}
	return false
}

// 简化 JSON 解析（避免拉 encoding/json 大头到热路径）。spec 形态可控，
// admin 写入时已校验。这里 lenient 解析，不抛 error 便于 hot path。
type canarySpec struct {
	percent   int
	targetIDs []string
}

type targetedSpec struct {
	ids []string
}

func parseCanary(s string) canarySpec {
	out := canarySpec{}
	// 极简解析：找 "percent": N, "target_instance_ids": ["a","b"]
	if i := strings.Index(s, `"percent":`); i >= 0 {
		out.percent = parseIntAfter(s[i+len(`"percent":`):])
	}
	if i := strings.Index(s, `"target_instance_ids":`); i >= 0 {
		out.targetIDs = parseStringSliceAfter(s[i+len(`"target_instance_ids":`):])
	}
	return out
}

func parseTargeted(s string) targetedSpec {
	if i := strings.Index(s, `"instance_ids":`); i >= 0 {
		return targetedSpec{ids: parseStringSliceAfter(s[i+len(`"instance_ids":`):])}
	}
	return targetedSpec{}
}

func parseIntAfter(s string) int {
	n := 0
	started := false
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
			started = true
		} else if started {
			break
		}
	}
	return n
}

func parseStringSliceAfter(s string) []string {
	// 找 [ ... ]
	start := strings.Index(s, "[")
	end := strings.Index(s, "]")
	if start < 0 || end < 0 || end <= start {
		return nil
	}
	inner := s[start+1 : end]
	out := []string{}
	for _, part := range strings.Split(inner, ",") {
		p := strings.TrimSpace(part)
		p = strings.Trim(p, `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

