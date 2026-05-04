// dlq.go: 死信队列（Dead Letter Queue）— webhook 重试耗尽后落 DLQ。
//
// 重试 N 次还失败的 event：
//   - 商户 URL 持续 5xx (商户后端挂了 / 配错了)
//   - 网络分区
//   - 商户 secret 改了但 risk-manage 还没刷
//   - 商户限流 / 流控
//
// 这些 event 不能直接 drop（合规 / 商户体验）。落 DLQ → admin 端点显示
// 失败列表 + reason，运营可：
//   1. fix 配置后调 /admin/webhook/dlq/replay?id=X 手动重发
//   2. 批量 replay 整个商户：/admin/webhook/dlq/replay-merchant?merchant_id=Y
//   3. 不可恢复的 → /admin/webhook/dlq/discard 标已知不可送达
//
// 当前实现 MemDLQStore (ring buffer，进程内)；生产应换 PG-backed store
// （schema：webhook_dlq(event_id PRIMARY KEY, merchant_id, event_type,
// body BYTEA, last_error TEXT, attempts INT, first_failed_at, last_attempt_at,
// status enum(pending/discarded))）。
package webhook

import (
	"sync"
	"time"
)

// DLQEntry 一条死信记录。
type DLQEntry struct {
	EventID       string    `json:"event_id"`
	MerchantID    string    `json:"merchant_id"`
	EventType     EventType `json:"event_type"`
	Body          []byte    `json:"body"` // 原始 JSON event；replay 时用
	LastError     string    `json:"last_error"`
	Attempts      int       `json:"attempts"`
	FirstFailedAt time.Time `json:"first_failed_at"`
	LastAttemptAt time.Time `json:"last_attempt_at"`
	Status        string    `json:"status"` // pending / discarded
}

// DLQStore 死信存储抽象。生产 PG 实现把 record 落库。
type DLQStore interface {
	Put(entry DLQEntry) error
	Get(eventID string) (*DLQEntry, bool)
	List(merchantID string, status string, limit int) []DLQEntry
	Discard(eventID string, actor, reason string) error
	Delete(eventID string) error
}

// MemDLQStore 进程内 ring + map 索引。重启清空。
type MemDLQStore struct {
	mu      sync.RWMutex
	cap     int
	byID    map[string]*DLQEntry
	order   []string // 插入顺序，给 List + ring 删用
}

// NewMemDLQStore cap <= 0 → 4096。
func NewMemDLQStore(cap int) *MemDLQStore {
	if cap <= 0 {
		cap = 4096
	}
	return &MemDLQStore{
		cap:   cap,
		byID:  make(map[string]*DLQEntry, cap),
		order: make([]string, 0, cap),
	}
}

func (s *MemDLQStore) Put(e DLQEntry) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byID[e.EventID]; ok {
		// 同 event 二次失败 → 更新 attempts / last_error，保留 first_failed_at
		existing.Attempts = e.Attempts
		existing.LastError = e.LastError
		existing.LastAttemptAt = e.LastAttemptAt
		return nil
	}
	cp := e
	if cp.Status == "" {
		cp.Status = "pending"
	}
	s.byID[e.EventID] = &cp
	s.order = append(s.order, e.EventID)
	// ring 限制：超过 cap 删最老
	for len(s.order) > s.cap {
		oldID := s.order[0]
		s.order = s.order[1:]
		delete(s.byID, oldID)
	}
	return nil
}

func (s *MemDLQStore) Get(eventID string) (*DLQEntry, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byID[eventID]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// List 按 merchant + status 过滤，返最近 limit 条（最新在前）。
func (s *MemDLQStore) List(merchantID string, status string, limit int) []DLQEntry {
	if s == nil {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]DLQEntry, 0, limit)
	// 倒序（最新在前）
	for i := len(s.order) - 1; i >= 0 && len(out) < limit; i-- {
		e := s.byID[s.order[i]]
		if e == nil {
			continue
		}
		if merchantID != "" && e.MerchantID != merchantID {
			continue
		}
		if status != "" && e.Status != status {
			continue
		}
		out = append(out, *e)
	}
	return out
}

func (s *MemDLQStore) Discard(eventID string, actor, reason string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byID[eventID]; ok {
		e.Status = "discarded"
		e.LastError = "discarded by " + actor + ": " + reason
	}
	return nil
}

func (s *MemDLQStore) Delete(eventID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[eventID]; !ok {
		return nil
	}
	delete(s.byID, eventID)
	for i, id := range s.order {
		if id == eventID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return nil
}

// Size 当前 DLQ 条目数。给 metric / health 用。
func (s *MemDLQStore) Size() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID)
}
