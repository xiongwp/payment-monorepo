// rule_audit.go: 规则变更审计日志（谁 / 什么时候 / 改了什么 / 旧值 → 新值）。
//
// 跟 DecisionAudit 不同：
//   - DecisionAudit 记每笔交易的风控决策（合规取证）
//   - RuleAudit 记每次规则配置变更（运营审计；防"昨天好好的怎么今天就误杀了"）
//
// 持久化：跟 DecisionAudit 复用同一套 sink 抽象（MemSink + ClickHouse / PG）；
// 但用独立 stream 名（"rule_audit"），下游消费者按 stream 区分。
//
// 触发点（main.go 各 admin endpoint 调）：
//   - POST /admin/rules/reload  → action=reload, payload={loaded_count, ...}
//   - POST /admin/rules/mode    → action=mode_change, payload={rule_id, old, new}
//   - POST /admin/rules/update  → action=rule_update, payload={rule_id, before, after}
//
// 失败行为：审计写不进去时**继续执行**业务（fail-open）。如果合规要求
// fail-close（写不动审计就回滚），改 sink.Write 返 error 时 abort。
package audit

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// RuleAuditEntry 一次规则变更。
type RuleAuditEntry struct {
	OccurredAt time.Time         `json:"occurred_at"`
	Action     string            `json:"action"` // "reload" / "mode_change" / "rule_update" / "rule_disable"
	Actor      string            `json:"actor"`  // 操作者 id（admin SSO / token sub）
	RuleID     string            `json:"rule_id,omitempty"`
	Before     json.RawMessage   `json:"before,omitempty"`
	After      json.RawMessage   `json:"after,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// RuleAuditStore 规则审计日志存储。Mem / ClickHouse / PG 可换。
type RuleAuditStore interface {
	Write(ctx context.Context, entry RuleAuditEntry) error
	Recent(limit int) []*RuleAuditEntry
	ByRuleID(ruleID string, limit int) []*RuleAuditEntry
}

// MemRuleAuditStore 进程内 ring buffer。重启清零；生产应配 PG / ClickHouse。
type MemRuleAuditStore struct {
	mu     sync.RWMutex
	buf    []*RuleAuditEntry
	cap    int
	cursor int
}

func NewMemRuleAuditStore(cap int) *MemRuleAuditStore {
	if cap <= 0 {
		cap = 4096
	}
	return &MemRuleAuditStore{cap: cap, buf: make([]*RuleAuditEntry, 0, cap)}
}

func (s *MemRuleAuditStore) Write(_ context.Context, entry RuleAuditEntry) error {
	if entry.OccurredAt.IsZero() {
		entry.OccurredAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := entry
	if len(s.buf) < s.cap {
		s.buf = append(s.buf, &cp)
	} else {
		s.buf[s.cursor] = &cp
		s.cursor = (s.cursor + 1) % s.cap
	}
	return nil
}

func (s *MemRuleAuditStore) Recent(limit int) []*RuleAuditEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := len(s.buf)
	if n == 0 {
		return nil
	}
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]*RuleAuditEntry, 0, limit)
	// reverse chronological：先尾后头（如果环已满）
	if len(s.buf) < s.cap {
		for i := n - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, s.buf[i])
		}
		return out
	}
	// 环已满，从 cursor-1 倒着读
	for i := 0; i < limit; i++ {
		idx := (s.cursor - 1 - i + s.cap) % s.cap
		out = append(out, s.buf[idx])
	}
	return out
}

func (s *MemRuleAuditStore) ByRuleID(ruleID string, limit int) []*RuleAuditEntry {
	if ruleID == "" {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*RuleAuditEntry{}
	// 跟 Recent 一样倒序遍历，但过滤 ruleID
	n := len(s.buf)
	scan := func(idx int) {
		if e := s.buf[idx]; e != nil && e.RuleID == ruleID {
			out = append(out, e)
		}
	}
	if len(s.buf) < s.cap {
		for i := n - 1; i >= 0 && len(out) < limit; i-- {
			scan(i)
		}
	} else {
		for i := 0; i < s.cap && len(out) < limit; i++ {
			idx := (s.cursor - 1 - i + s.cap) % s.cap
			scan(idx)
		}
	}
	return out
}
