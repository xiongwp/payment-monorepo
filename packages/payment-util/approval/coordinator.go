// coordinator.go — Approval 主编排 API.

package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Config 默认值
type Config struct {
	DefaultRequiredApprovals int           // 默认 2
	DefaultExpireAfter       time.Duration // 默认 7d
	CancelWindow             time.Duration // approved → executed 之间允许 cancel 的窗口, 默认 30min
}

func DefaultConfig() Config {
	return Config{
		DefaultRequiredApprovals: 2,
		DefaultExpireAfter:       7 * 24 * time.Hour,
		CancelWindow:             30 * time.Minute,
	}
}

// AuditSink 由调用方注入 — 集成 audit-log
type AuditSink interface {
	Emit(ctx context.Context, e AuditEvent) error
}

type AuditEvent struct {
	Action     string                 // request / approve / reject / execute / cancel / expire
	ActionID   string
	Actor      string
	ResourceID string
	Details    map[string]interface{}
}

// NoOpSink 不发任何审计 (默认; dev 用)
type NoOpSink struct{}

func (NoOpSink) Emit(_ context.Context, _ AuditEvent) error { return nil }

// Coordinator 唯一对外入口
type Coordinator struct {
	store Store
	audit AuditSink
	cfg   Config
}

func New(s Store, audit AuditSink, cfg Config) *Coordinator {
	if audit == nil {
		audit = NoOpSink{}
	}
	if cfg.DefaultRequiredApprovals == 0 {
		cfg = DefaultConfig()
	}
	return &Coordinator{store: s, audit: audit, cfg: cfg}
}

// Request 创建一个待审批 action.
//
// 必填: Type, Resource, Requester. 其他可空 (默认值).
func (c *Coordinator) Request(ctx context.Context, in Action) (Action, error) {
	if in.Type == "" || in.Resource == "" || in.Requester == "" {
		return Action{}, ErrInvalidState
	}
	now := time.Now().UTC()
	if in.ID == "" {
		in.ID = "ap_" + randHex(16)
	}
	in.RequesterAt = now
	in.State = StatePending
	if in.RequiredApprovals == 0 {
		in.RequiredApprovals = c.cfg.DefaultRequiredApprovals
	}
	if in.ExpiresAt.IsZero() {
		in.ExpiresAt = now.Add(c.cfg.DefaultExpireAfter)
	}
	if err := c.store.Create(in); err != nil {
		return Action{}, err
	}
	_ = c.audit.Emit(ctx, AuditEvent{
		Action:     "request",
		ActionID:   in.ID,
		Actor:      in.Requester,
		ResourceID: in.Resource,
		Details: map[string]interface{}{
			"type":               in.Type,
			"required_approvals": in.RequiredApprovals,
		},
	})
	return in, nil
}

// Approve 给 action 加一票 approve.
// 自动推进状态机: pending → reviewing → approved
func (c *Coordinator) Approve(ctx context.Context, actionID, reviewer, note string) (Action, error) {
	return c.review(ctx, actionID, reviewer, note, DecisionApprove)
}

// Reject 任一 reject 即终态.
func (c *Coordinator) Reject(ctx context.Context, actionID, reviewer, note string) (Action, error) {
	return c.review(ctx, actionID, reviewer, note, DecisionReject)
}

func (c *Coordinator) review(ctx context.Context, actionID, reviewer, note string, dec Decision) (Action, error) {
	a, err := c.store.Get(actionID)
	if err != nil {
		return Action{}, err
	}
	now := time.Now().UTC()
	if now.After(a.ExpiresAt) && a.State == StatePending {
		a.State = StateExpired
		_ = c.store.Update(a)
		return a, ErrInvalidState
	}
	if a.State != StatePending && a.State != StateReviewing {
		return a, ErrAlreadyDecided
	}
	if reviewer == a.Requester {
		return a, ErrSelfApproval
	}
	if a.HasReviewer(reviewer) {
		return a, ErrDuplicateReviewer
	}

	a.Approvals = append(a.Approvals, Approval{
		Reviewer:   reviewer,
		Decision:   dec,
		Note:       note,
		ReviewedAt: now,
	})

	// 状态推进
	if dec == DecisionReject {
		a.State = StateRejected
	} else {
		if a.ApproveCount() >= a.RequiredApprovals {
			a.State = StateApproved
		} else {
			a.State = StateReviewing
		}
	}

	if err := c.store.Update(a); err != nil {
		return Action{}, err
	}

	_ = c.audit.Emit(ctx, AuditEvent{
		Action:     string(dec),
		ActionID:   a.ID,
		Actor:      reviewer,
		ResourceID: a.Resource,
		Details: map[string]interface{}{
			"new_state":      string(a.State),
			"approve_count":  a.ApproveCount(),
			"required":       a.RequiredApprovals,
			"note":           note,
		},
	})
	return a, nil
}

// MarkExecuted 业务侧执行完后回写; 防 cancel 反悔
func (c *Coordinator) MarkExecuted(ctx context.Context, actionID, executor string) error {
	a, err := c.store.Get(actionID)
	if err != nil {
		return err
	}
	if a.State != StateApproved {
		return ErrInvalidState
	}
	a.State = StateExecuted
	a.ExecutedAt = time.Now().UTC()
	a.ExecutedBy = executor
	if err := c.store.Update(a); err != nil {
		return err
	}
	_ = c.audit.Emit(ctx, AuditEvent{
		Action:     "execute",
		ActionID:   a.ID,
		Actor:      executor,
		ResourceID: a.Resource,
	})
	return nil
}

// Cancel approved 状态 + 30min 内 requester 可撤回.
// 注意: 不能 reject 后 cancel (已是终态).
func (c *Coordinator) Cancel(ctx context.Context, actionID, requester, reason string) error {
	a, err := c.store.Get(actionID)
	if err != nil {
		return err
	}
	if a.Requester != requester {
		return ErrSelfApproval // 复用错误: 不是 requester 不能 cancel
	}
	now := time.Now().UTC()
	switch a.State {
	case StatePending, StateReviewing:
		// 任何时候可撤
	case StateApproved:
		// 30min 内才能
		approvedAt := a.Approvals[len(a.Approvals)-1].ReviewedAt
		if now.Sub(approvedAt) > c.cfg.CancelWindow {
			return ErrInvalidState
		}
	default:
		return ErrInvalidState
	}
	a.State = StateCancelled
	a.CancelledAt = now
	if err := c.store.Update(a); err != nil {
		return err
	}
	_ = c.audit.Emit(ctx, AuditEvent{
		Action:     "cancel",
		ActionID:   a.ID,
		Actor:      requester,
		ResourceID: a.Resource,
		Details:    map[string]interface{}{"reason": reason},
	})
	return nil
}

// GetState 给业务侧判断
func (c *Coordinator) GetState(actionID string) (State, error) {
	a, err := c.store.Get(actionID)
	if err != nil {
		return "", err
	}
	return a.State, nil
}

// Get 全量取
func (c *Coordinator) Get(actionID string) (Action, error) {
	return c.store.Get(actionID)
}

// List
func (c *Coordinator) List(filter ListFilter) ([]Action, error) {
	return c.store.List(filter)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
