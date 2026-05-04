// Package review 是 Radar-style 人工 review 队列。
//
// 流程：
//
//	service.Screen → verdict=REVIEW 时 push 一条到 review.Queue
//	admin UI    GET /admin/review/list?status=pending      列表
//	            POST /admin/review/decide {id, action, actor, reason}
//	  action ∈ approve / reject
//	  approve: 该笔实际放过；后续业务侧用此结论
//	  reject:  该笔实际拒；后续业务侧拒绝放款
//
// 决议结果通过 OutcomeRecorder（feedback 包）回写到 audit，让 ML 离线训练
// 拿到"风控判 review，运营人工判定真实结果"的 label。
//
// 当前 Mem 实现是内存版（重启丢失；生产换 PG / MySQL append-only 表）。
package review

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// Status 队列项的状态。
type Status string

const (
	StatusPending  Status = "pending"
	StatusInReview Status = "in_review" // 已被 analyst Claim 还没决议
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
	StatusEscalated Status = "escalated" // 升级到上级 analyst
)

// Action admin 决议时的输入动作。
type Action string

const (
	ActionApprove Action = "approve"
	ActionReject  Action = "reject"
)

// Item 一个 review 任务（"case"）。Created / DecidedAt 都是 UTC。
//
// 工作流：pending → (Claim) → in_review → (Decide) → approved|rejected
//                              ↓ Escalate
//                              escalated → (Decide by senior) → approved|rejected
//
// Notes 给 analyst 之间留协作上下文（"已联系商户" / "等待对方回邮"）。
// SLADeadline 由 Push 时按 Store 配置算（默认 24h）。
type Item struct {
	ID              string     `json:"id"` // 等于 audit 的 decision_id，便于关联
	MerchantID      string     `json:"merchant_id"`
	CustomerID      string     `json:"customer_id"`
	PaymentIntentID string     `json:"payment_intent_id"`
	Amount          int64      `json:"amount"`
	Currency        string     `json:"currency"`
	RiskScore       int        `json:"risk_score"`
	Reasons         []string   `json:"reasons"`
	Status          Status     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	SLADeadline     time.Time  `json:"sla_deadline"`
	AssignedTo      string     `json:"assigned_to,omitempty"`
	AssignedAt      *time.Time `json:"assigned_at,omitempty"`
	EscalateLevel   int        `json:"escalate_level,omitempty"`
	DecidedAt       *time.Time `json:"decided_at,omitempty"`
	DecidedBy       string     `json:"decided_by,omitempty"`
	DecideReason    string     `json:"decide_reason,omitempty"`
	Notes           []Note     `json:"notes,omitempty"`
}

// Note case-level 备注。analyst 协作 / 调查留痕。
type Note struct {
	Actor     string    `json:"actor"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Store 持久化接口。Mem 实现见下；生产 PG 实现：append-only `risk_review`
// 表，UPDATE WHERE id=? AND status=expected_status 保证状态机原子。
type Store interface {
	// Push 加一条 pending 任务。已存在 id 视为幂等（Screen 重试时不重复 push）。
	Push(item Item) error
	// Decide 把 pending / in_review / escalated 改成 approved / rejected。
	// 已 decided / 不存在 → ErrNotPending。
	Decide(id string, action Action, actor, reason string) (*Item, error)
	// List 按 status 过滤；status == "" 返全量。limit / offset 给 admin 分页。
	List(status Status, limit, offset int) []*Item
	// Get 单条详情。
	Get(id string) *Item
	// CountByStatus 给 SLO 监控 / queue depth gauge 用。status="" 返总数。
	CountByStatus(status Status) int
	// Claim 把 pending 抢占为 in_review，记录 analyst id。重复 claim 同 actor
	// 视为幂等（不报错），不同 actor 抢已 claim → ErrAlreadyClaimed。
	Claim(id, actor string) (*Item, error)
	// Release 把 in_review 退回 pending（analyst 暂不审）。只 actor 自己能放。
	Release(id, actor string) (*Item, error)
	// Escalate 把 in_review 升到 escalated，bumping escalate_level。
	Escalate(id, actor, reason string) (*Item, error)
	// AddNote 追加一条备注。任何状态都能加（包括已 decided，留事后调查痕）。
	AddNote(id, actor, body string) (*Item, error)
	// ListByAssignee 当前 analyst 的工作台视图。
	ListByAssignee(actor string, statuses []Status, limit int) []*Item
	// OverdueSLA 已过 SLADeadline 仍未决议的 case。给监控 alert 用。
	OverdueSLA(now time.Time, limit int) []*Item
}

// 状态机 / 抢占错误。
var (
	ErrNotPending     = errors.New("review: item not in modifiable state")
	ErrAlreadyClaimed = errors.New("review: already claimed by another actor")
	ErrNotAssignee    = errors.New("review: caller is not the current assignee")
)

// MemStore 内存版。线程安全；O(N) list（N 是历史项数；生产用 DB index）。
type MemStore struct {
	mu        sync.RWMutex
	items     map[string]*Item
	defaultSLA time.Duration // Push 时 SLADeadline = CreatedAt + this；默认 24h
}

func NewMemStore() *MemStore {
	return &MemStore{items: make(map[string]*Item), defaultSLA: 24 * time.Hour}
}

// SetDefaultSLA 配置默认 SLA 期限。<=0 用 24h。
func (m *MemStore) SetDefaultSLA(d time.Duration) {
	if d <= 0 {
		d = 24 * time.Hour
	}
	m.mu.Lock()
	m.defaultSLA = d
	m.mu.Unlock()
}

func (m *MemStore) Push(item Item) error {
	if item.ID == "" {
		return errors.New("review: id is required")
	}
	if item.Status == "" {
		item.Status = StatusPending
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if item.SLADeadline.IsZero() {
		sla := m.defaultSLA
		if sla <= 0 {
			sla = 24 * time.Hour
		}
		item.SLADeadline = item.CreatedAt.Add(sla)
	}
	if _, exists := m.items[item.ID]; exists {
		return nil // 幂等
	}
	cp := item
	m.items[item.ID] = &cp
	return nil
}

func (m *MemStore) Decide(id string, action Action, actor, reason string) (*Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return nil, ErrNotPending
	}
	switch it.Status {
	case StatusPending, StatusInReview, StatusEscalated:
	default:
		return nil, ErrNotPending
	}
	switch action {
	case ActionApprove:
		it.Status = StatusApproved
	case ActionReject:
		it.Status = StatusRejected
	default:
		return nil, errors.New("review: invalid action")
	}
	now := time.Now().UTC()
	it.DecidedAt = &now
	it.DecidedBy = actor
	it.DecideReason = reason
	cp := *it
	return &cp, nil
}

func (m *MemStore) Claim(id, actor string) (*Item, error) {
	if actor == "" {
		return nil, errors.New("review: actor required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return nil, ErrNotPending
	}
	switch it.Status {
	case StatusPending:
		// 第一次 claim
	case StatusInReview:
		if it.AssignedTo == actor {
			cp := *it
			return &cp, nil // 同 actor 重复 claim 幂等
		}
		return nil, ErrAlreadyClaimed
	default:
		return nil, ErrNotPending
	}
	now := time.Now().UTC()
	it.Status = StatusInReview
	it.AssignedTo = actor
	it.AssignedAt = &now
	cp := *it
	return &cp, nil
}

func (m *MemStore) Release(id, actor string) (*Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return nil, ErrNotPending
	}
	if it.Status != StatusInReview {
		return nil, ErrNotPending
	}
	if it.AssignedTo != actor {
		return nil, ErrNotAssignee
	}
	it.Status = StatusPending
	it.AssignedTo = ""
	it.AssignedAt = nil
	cp := *it
	return &cp, nil
}

func (m *MemStore) Escalate(id, actor, reason string) (*Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return nil, ErrNotPending
	}
	if it.Status != StatusInReview {
		return nil, ErrNotPending
	}
	if it.AssignedTo != actor {
		return nil, ErrNotAssignee
	}
	now := time.Now().UTC()
	it.Status = StatusEscalated
	it.EscalateLevel++
	it.AssignedTo = "" // 等待上级 claim
	it.AssignedAt = nil
	it.Notes = append(it.Notes, Note{
		Actor: actor, Body: "ESCALATED: " + reason, CreatedAt: now,
	})
	cp := *it
	return &cp, nil
}

func (m *MemStore) AddNote(id, actor, body string) (*Item, error) {
	if actor == "" || body == "" {
		return nil, errors.New("review: actor + body required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return nil, ErrNotPending
	}
	it.Notes = append(it.Notes, Note{
		Actor: actor, Body: body, CreatedAt: time.Now().UTC(),
	})
	cp := *it
	return &cp, nil
}

func (m *MemStore) ListByAssignee(actor string, statuses []Status, limit int) []*Item {
	m.mu.RLock()
	defer m.mu.RUnlock()
	want := make(map[Status]struct{}, len(statuses))
	for _, s := range statuses {
		want[s] = struct{}{}
	}
	var out []*Item
	for _, it := range m.items {
		if it.AssignedTo != actor {
			continue
		}
		if len(want) > 0 {
			if _, ok := want[it.Status]; !ok {
				continue
			}
		}
		cp := *it
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SLADeadline.Before(out[j].SLADeadline) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (m *MemStore) OverdueSLA(now time.Time, limit int) []*Item {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Item
	for _, it := range m.items {
		// 已决议的不算 overdue
		if it.Status == StatusApproved || it.Status == StatusRejected {
			continue
		}
		if !it.SLADeadline.IsZero() && now.After(it.SLADeadline) {
			cp := *it
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SLADeadline.Before(out[j].SLADeadline) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (m *MemStore) List(status Status, limit, offset int) []*Item {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var all []*Item
	for _, it := range m.items {
		if status != "" && it.Status != status {
			continue
		}
		cp := *it
		all = append(all, &cp)
	}
	// 最新的在前（CreatedAt DESC）
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(all) {
		return nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end]
}

func (m *MemStore) Get(id string) *Item {
	m.mu.RLock()
	defer m.mu.RUnlock()
	it, ok := m.items[id]
	if !ok {
		return nil
	}
	cp := *it
	return &cp
}

func (m *MemStore) CountByStatus(status Status) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if status == "" {
		return len(m.items)
	}
	n := 0
	for _, it := range m.items {
		if it.Status == status {
			n++
		}
	}
	return n
}
