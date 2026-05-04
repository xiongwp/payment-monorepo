package store

import (
	"sync"
	"time"
)

// PredebitCommits 缓存 Screen Allow 时落地的预扣 key（按 IdempotencyKey 索引），
// 给 Report 阶段查询「这条 IdempotencyKey 在 Screen 阶段是否已经把 counter
// 加过了」用。命中即跳过 Report 的 Incr，避免 double counting。
//
// 资损修复（PR #37 follow-up）：之前 Screen Allow 路径会通过 IncrIfBelow*
// 把 daily/monthly/velocity counter 加 1 次（atomic 预扣），Report 又把同
// counter 加 1 次（不感知 Screen 已经加过）。在 MemCounter 部署下两次累加
// → 后续 amount_limit 误判超额。生产 RedisCounter 不实现 AtomicCounter
// 不进预扣分支，所以暂时未受影响——但本机制把语义补齐，将来 Redis 版本
// 接入 AtomicCounter 时不用回头改 Report。
//
// 缓存为进程内：跨实例不共享。当一个 Screen 在实例 A 落地、Report 落到实
// 例 B 时，B 看不到 A 的 commit，仍会 Incr → 双计。多副本部署下接受这条
// 边角，理由：(1) 同 IdempotencyKey 的 Screen + Report 通常被 LB sticky
// 到同实例（payment-core 走 channelclient gRPC 复用同一连接）；(2) 即便
// 漂移 → 双计只放大限额阈值，是 false positive 不是 fund loss；(3) 解决
// 方案要走 Redis 共享缓存，不值得本 PR 的复杂度。
//
// TTL 5 分钟覆盖典型 Screen → Charge → Report 链路；超时则缓存逐出，
// 后续 Report 走原始 Incr 路径（少数边角双计可容忍）。
type PredebitCommits struct {
	mu      sync.RWMutex
	entries map[string]predebitEntry
}

type predebitEntry struct {
	keys      map[string]struct{}
	expiresAt time.Time
}

const predebitTTL = 5 * time.Minute

func NewPredebitCommits() *PredebitCommits {
	p := &PredebitCommits{entries: make(map[string]predebitEntry, 256)}
	go p.gcLoop()
	return p
}

// Commit 把 tracker 里所有 Reservation 的 counter key 索引到 idemKey 下。
// idemKey 为空时 no-op（caller 没传 idempotency_key → 没法查询）。
func (p *PredebitCommits) Commit(idemKey string, items []Reservation) {
	if idemKey == "" || len(items) == 0 {
		return
	}
	keys := make(map[string]struct{}, len(items))
	for _, r := range items {
		if r.Key != "" {
			keys[r.Key] = struct{}{}
		}
	}
	if len(keys) == 0 {
		return
	}
	p.mu.Lock()
	p.entries[idemKey] = predebitEntry{
		keys:      keys,
		expiresAt: time.Now().Add(predebitTTL),
	}
	p.mu.Unlock()
}

// WasReserved 报告 (idemKey, counterKey) 是否在 Commit 时落过。
// idemKey 空 → 永远 false（Report 走原行为）。命中过期条目 → false。
func (p *PredebitCommits) WasReserved(idemKey, counterKey string) bool {
	if idemKey == "" || counterKey == "" {
		return false
	}
	p.mu.RLock()
	e, ok := p.entries[idemKey]
	p.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return false
	}
	_, hit := e.keys[counterKey]
	return hit
}

func (p *PredebitCommits) gcLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for now := range t.C {
		p.mu.Lock()
		for k, e := range p.entries {
			if now.After(e.expiresAt) {
				delete(p.entries, k)
			}
		}
		p.mu.Unlock()
	}
}
