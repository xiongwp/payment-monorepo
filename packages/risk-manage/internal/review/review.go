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
	"context"
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
	// ReasonCode 结构化决议 enum；见 reason_code.go。Decide 后写入。
	// 旧 case 历史回填值可能为空 → 前端展示时 fallback 到 DecideReason free text。
	ReasonCode string `json:"reason_code,omitempty"`
	Notes      []Note `json:"notes,omitempty"`

	// Level 当前 review 级别：1 = L1 一线，2 = L2 高级。默认 1。
	// 升级走 EscalateTo（手动 or SLA 自动）。区别于旧 EscalateLevel：
	//   EscalateLevel = 累计升级次数（兼容老代码 / chain audit）
	//   Level         = 当前应该由谁审（路由依据；L2 队列 = level=2 且 status=pending|escalated）
	Level int `json:"level,omitempty"`

	// TransferHist 转接历史（A 分析师 → B 分析师，level 不变）。
	TransferHist []TransferLog `json:"transfer_hist,omitempty"`
	// EscalateHist 升级历史（L1 → L2），含触发原因（manual / sla_timeout）。
	EscalateHist []EscalateLog `json:"escalate_hist,omitempty"`
	// SLAEscalated 标记此 case 已被 SLA 自动升级，避免重复触发（即使 deadline 持续过）。
	SLAEscalated bool `json:"sla_escalated,omitempty"`
}

// TransferLog 一条 case 转接记录（不改 level）。
type TransferLog struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// EscalateTrigger 触发升级的来源。
type EscalateTrigger string

const (
	EscalateTriggerManual     EscalateTrigger = "manual"      // 分析师主动升
	EscalateTriggerSLATimeout EscalateTrigger = "sla_timeout" // SLA 自动升
)

// EscalateLog 一条 case 升级记录（L1 → L2）。
type EscalateLog struct {
	FromLevel int             `json:"from_level"`
	ToLevel   int             `json:"to_level"`
	Trigger   EscalateTrigger `json:"trigger"`
	Actor     string          `json:"actor,omitempty"` // manual 时是 analyst id；sla_timeout 时为 "system"
	Reason    string          `json:"reason,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
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
	//
	// 注意：新代码请优先用 DecideWithCode（带结构化 reason_code）。
	// Decide 仍保留是为了向后兼容（chain.go / 历史调用方）。
	Decide(id string, action Action, actor, reason string) (*Item, error)
	// DecideWithCode 同 Decide 但接受结构化 ReasonCode。code 非法 → ErrInvalidReason。
	// 是 v2 推荐路径；HTTP handler 走这条。
	DecideWithCode(id string, action Action, actor, reasonCode, reason string) (*Item, error)
	// List 按 status 过滤；status == "" 返全量。limit / offset 给 admin 分页。
	List(status Status, limit, offset int) []*Item
	// Get 单条详情。
	Get(id string) *Item
	// CountByStatus 给 SLO 监控 / queue depth gauge 用。status="" 返总数。
	CountByStatus(status Status) int
	// OldestPendingAge 返回当前 pending 队列里"最早 CreatedAt"的年龄。
	// 没有 pending 项时返回 0。给 oncall alert 用："队列堆积超过 X 分钟"。
	// 单次扫描 O(N)；建议由 cmd/server 后台 goroutine 周期采样到 gauge，
	// 不要每次请求都调（生产 N 可能上千）。
	OldestPendingAge(now time.Time) time.Duration
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
	// Transfer 把 case 从 fromActor 转给 toActor（不改 level / status，等于
	// 重新分派）。fromActor 必须当前持有该 case；状态机：in_review → in_review。
	// 追加一条 TransferLog；旧 AssignedAt 重置为 now。
	Transfer(caseID, fromActor, toActor, reason string) (*Item, error)
	// EscalateTo 显式把 case 升到 targetLevel（必须 > 当前 Level）；
	// trigger 区分 manual / sla_timeout；reason 留痕。L2 升级后 AssignedTo
	// 清空，等待 L2 队列里的资深 analyst Claim。
	//
	// 业务约束（v1）：只支持 L1 → L2 (targetLevel == 2)。targetLevel > 2 报错。
	EscalateTo(caseID string, targetLevel int, actor, reason string, trigger EscalateTrigger) (*Item, error)
	// AutoEscalateOverdue 扫一遍所有 status ∈ {pending, in_review} 且 level==1
	// 且 SLADeadline < now 的 case，把它们升到 L2（trigger=sla_timeout）。
	// 返回被升级的 case 数。已被自动升过的（SLAEscalated=true）跳过避免重复。
	//
	// 由 sla_escalator goroutine 定时调用；调用方 ctx 取消即返。
	AutoEscalateOverdue(ctx context.Context, now time.Time) (escalated int, err error)
}

// 状态机 / 抢占错误。
var (
	ErrNotPending      = errors.New("review: item not in modifiable state")
	ErrAlreadyClaimed  = errors.New("review: already claimed by another actor")
	ErrNotAssignee     = errors.New("review: caller is not the current assignee")
	ErrInvalidReason   = errors.New("review: invalid or missing reason_code")
	ErrInvalidLevel    = errors.New("review: invalid target level (only L1→L2 supported)")
	ErrAlreadyAtLevel  = errors.New("review: case already at or above target level")
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
	if item.Level <= 0 {
		item.Level = 1 // 新 case 默认 L1
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

// DecideWithCode 是 Decide 的 v2 入口：必须传合法的结构化 reasonCode；
// reason 是可选的 free-text 详述。code 非法 → ErrInvalidReason。
//
// 实现复用 Decide 的状态机，校验通过后顺带写 it.ReasonCode。
func (m *MemStore) DecideWithCode(id string, action Action, actor, reasonCode, reason string) (*Item, error) {
	if !ValidReasonCode(reasonCode) {
		return nil, ErrInvalidReason
	}
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
	it.ReasonCode = reasonCode
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

// Transfer 把 case 从 fromActor 重新分派给 toActor。
// 约束：
//   - case 必须存在
//   - 当前 status == in_review 才允许 transfer（pending 没人持有；escalated 等待上级 claim）
//   - fromActor == AssignedTo（防越权改派）
//   - toActor 非空、!= fromActor
//
// 状态机：(in_review, A) → (in_review, B)。Level 不变。追加 TransferLog。
// AssignedAt 重置为 now（让 toActor 的 SLA 计算从转接点起算 — 但 SLADeadline 不变）。
func (m *MemStore) Transfer(caseID, fromActor, toActor, reason string) (*Item, error) {
	if toActor == "" || fromActor == "" {
		return nil, errors.New("review: from + to actor required")
	}
	if toActor == fromActor {
		return nil, errors.New("review: cannot transfer to self")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[caseID]
	if !ok {
		return nil, ErrNotPending
	}
	if it.Status != StatusInReview {
		return nil, ErrNotPending
	}
	if it.AssignedTo != fromActor {
		return nil, ErrNotAssignee
	}
	now := time.Now().UTC()
	it.TransferHist = append(it.TransferHist, TransferLog{
		From: fromActor, To: toActor, Reason: reason, CreatedAt: now,
	})
	it.AssignedTo = toActor
	it.AssignedAt = &now
	cp := *it
	return &cp, nil
}

// EscalateTo L1 → L2 显式升级。区别于旧 Escalate(actor, reason)：
//   - 旧 Escalate：状态 in_review → escalated；不变 Level（实际就是 free-form bump
//     EscalateLevel 计数器）。等于"我审不了，丢回队列让别人抢"。
//   - 新 EscalateTo：明确改 Level（路由依据），追加 EscalateHist 含 trigger。
//
// 业务约束（v1）：targetLevel 只支持 2。targetLevel > 2 → ErrInvalidLevel。
//
// 状态机：(level=1, status ∈ {pending, in_review}) → (level=2, status=escalated, assigned_to="")
// trigger=sla_timeout 时同时设 SLAEscalated=true，防 sla_escalator 反复触发。
func (m *MemStore) EscalateTo(caseID string, targetLevel int, actor, reason string, trigger EscalateTrigger) (*Item, error) {
	if targetLevel != 2 {
		return nil, ErrInvalidLevel
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[caseID]
	if !ok {
		return nil, ErrNotPending
	}
	// 已决议的不能再升级
	if it.Status == StatusApproved || it.Status == StatusRejected {
		return nil, ErrNotPending
	}
	curLevel := it.Level
	if curLevel <= 0 {
		curLevel = 1 // 历史 case 兼容
	}
	if curLevel >= targetLevel {
		return nil, ErrAlreadyAtLevel
	}
	now := time.Now().UTC()
	it.EscalateHist = append(it.EscalateHist, EscalateLog{
		FromLevel: curLevel, ToLevel: targetLevel,
		Trigger: trigger, Actor: actor, Reason: reason, CreatedAt: now,
	})
	it.Level = targetLevel
	it.Status = StatusEscalated
	it.EscalateLevel++ // 兼容旧字段（chain audit / 老 dashboard）
	it.AssignedTo = "" // 等待 L2 claim
	it.AssignedAt = nil
	if trigger == EscalateTriggerSLATimeout {
		it.SLAEscalated = true
	}
	// 加一条 note 留协作可读痕迹（除了结构化 EscalateHist）
	it.Notes = append(it.Notes, Note{
		Actor:     actorOrSystem(actor),
		Body:      "ESCALATED L" + itoaSimple(curLevel) + "→L" + itoaSimple(targetLevel) + " [" + string(trigger) + "]: " + reason,
		CreatedAt: now,
	})
	cp := *it
	return &cp, nil
}

// AutoEscalateOverdue 扫一遍所有 level=1 + status ∈ {pending, in_review} + SLA 已过期 +
// SLAEscalated=false 的 case，自动升 L2。
//
// 复杂度 O(N)；MemStore 适用百级 N，PG 实现走索引可上千。
// ctx 取消时尽量在两次升级之间停下；不强求精确（已升的不回滚）。
func (m *MemStore) AutoEscalateOverdue(ctx context.Context, now time.Time) (int, error) {
	// 第一步：snapshot 候选 id（持读锁），第二步：单个升级（持写锁）。
	// 避免持写锁太久阻塞 claim / decide。
	m.mu.RLock()
	candidates := make([]string, 0, 16)
	for id, it := range m.items {
		if it.SLAEscalated {
			continue
		}
		if it.Level > 1 { // 已经 L2 不动
			continue
		}
		if it.Status != StatusPending && it.Status != StatusInReview {
			continue
		}
		if it.SLADeadline.IsZero() || !now.After(it.SLADeadline) {
			continue
		}
		candidates = append(candidates, id)
	}
	m.mu.RUnlock()

	count := 0
	for _, id := range candidates {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		if _, err := m.EscalateTo(id, 2, "system", "sla_timeout", EscalateTriggerSLATimeout); err == nil {
			count++
		}
		// 单条失败不影响其它（如已被人工 decide 掉了 → ErrNotPending，跳过）。
	}
	return count, nil
}

// itoaSimple 小整数 → 字符串，避免引入 strconv（review.go 已轻量）。
func itoaSimple(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func actorOrSystem(a string) string {
	if a == "" {
		return "system"
	}
	return a
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

// OldestPendingAge 扫描 in-memory 全表，挑 status==pending 的 CreatedAt 最小值。
// 没有 pending 返回 0。生产 N≤几千足够 O(N) 扫；超大量级请用 PGReviewStore（走索引）。
func (m *MemStore) OldestPendingAge(now time.Time) time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var oldest time.Time
	for _, it := range m.items {
		if it.Status != StatusPending {
			continue
		}
		if oldest.IsZero() || it.CreatedAt.Before(oldest) {
			oldest = it.CreatedAt
		}
	}
	if oldest.IsZero() {
		return 0
	}
	d := now.Sub(oldest)
	if d < 0 {
		return 0
	}
	return d
}
