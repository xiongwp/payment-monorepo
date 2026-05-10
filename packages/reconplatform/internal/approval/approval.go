// Package approval — 双人复核（four-eyes）工作流。
//
// 高风险 diff 状态迁移（resolved / false_positive 大额）需要 2 个 approver
// 同意才能落库。SOX / PCI-DSS / 内部 risk policy 必备。
//
// 流程:
//
//	op_A POST /api/v1/diffs/{id}/transition?to=resolved
//	  → 检查 amount 是否触发 approval policy
//	  → 触发 → 写 recon:approval:pending:<diff_id>，diffstate 不动
//	  → 返 {pending: true, approval_id, required_approvers: 2, current: 0}
//
//	op_B POST /api/v1/approvals/{approval_id}/approve
//	  → 验证 op_B != op_A（不能自批自）
//	  → current++
//	  → current >= required → 真正调 diffStore.Transition(...)
//	  → 删 pending key
//
//	op_C POST /api/v1/approvals/{approval_id}/reject
//	  → 删 pending key + 写 audit_log "rejected by op_C"
//
// Policy（config-center key=reconplatform/approval.policy）:
//
//	thresholds:
//	  - condition: "transition_to=resolved AND amount_minor > 100000"   # >$1000
//	    required_approvers: 2
//	  - condition: "transition_to=false_positive AND amount_minor > 1000000"  # >$10000
//	    required_approvers: 3
//	approver_groups:
//	  diff_resolver: ["alice@", "bob@", "carol@"]
//	  finance:       ["dave@"]

package approval

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"reconcile-system/internal/diffstate"
)

// PendingApproval 一笔待批准的状态迁移。
type PendingApproval struct {
	ID                string    `json:"id"`           // sha1(diff_id|to|requested_by|ts)
	DiffID            string    `json:"diff_id"`
	From              string    `json:"from_state"`
	To                string    `json:"to_state"`
	Note              string    `json:"note"`
	RequestedBy       string    `json:"requested_by"`
	RequestedAt       time.Time `json:"requested_at"`
	RequiredApprovers int       `json:"required_approvers"`
	Approvers         []string  `json:"approvers"`     // 已点赞的 op email
	Reason            string    `json:"reason,omitempty"`  // 为什么需要审批
}

// Manager 双人复核管理器。
type Manager struct {
	rdb       redis.UniversalClient
	diffStore *diffstate.Store
	policy    Policy
}

func New(rdb redis.UniversalClient, diffStore *diffstate.Store, policy Policy) *Manager {
	return &Manager{rdb: rdb, diffStore: diffStore, policy: policy}
}

// Policy 决定一笔迁移是否需要审批 + 需要几个人。
type Policy struct {
	// 简化版 — 触发条件用 hard-coded 规则；生产换 cel/expr 表达式引擎。
	Thresholds []Threshold `yaml:"thresholds" json:"thresholds"`
}

// Threshold 一条触发条件。
type Threshold struct {
	ToState           string `yaml:"transition_to" json:"transition_to"`
	MinAmountMinor    int64  `yaml:"min_amount_minor" json:"min_amount_minor"`
	RequiredApprovers int    `yaml:"required_approvers" json:"required_approvers"`
	Reason            string `yaml:"reason" json:"reason"`
}

// DefaultPolicy 一组安全默认值（dev / 没配 policy 时用）。
func DefaultPolicy() Policy {
	return Policy{
		Thresholds: []Threshold{
			// resolved + > $1000 → 2 个人审
			{ToState: "resolved", MinAmountMinor: 100_000, RequiredApprovers: 2,
				Reason: "Resolving diff > $1000 requires 4-eyes"},
			// false_positive + > $1000 → 2 个人审
			{ToState: "false_positive", MinAmountMinor: 100_000, RequiredApprovers: 2,
				Reason: "False positive > $1000 requires 4-eyes"},
			// false_positive + > $10000 → 3 个人审
			{ToState: "false_positive", MinAmountMinor: 1_000_000, RequiredApprovers: 3,
				Reason: "False positive > $10000 requires 3 approvers"},
		},
	}
}

// RequiresApproval 检查迁移是否需要 approval。返 (need, requiredCount, reason)。
func (m *Manager) RequiresApproval(d *diffstate.Diff, to string) (bool, int, string) {
	amt := extractAmount(d.Detail)
	maxRequired := 0
	reason := ""
	for _, t := range m.policy.Thresholds {
		if t.ToState != to {
			continue
		}
		if amt < t.MinAmountMinor {
			continue
		}
		if t.RequiredApprovers > maxRequired {
			maxRequired = t.RequiredApprovers
			reason = t.Reason
		}
	}
	return maxRequired > 0, maxRequired, reason
}

// Request 发起一笔审批请求。返 PendingApproval 给 caller 展示。
func (m *Manager) Request(ctx context.Context, diffID, to, note, requestedBy string,
	required int, reason string) (*PendingApproval, error) {
	d, err := m.diffStore.Get(ctx, diffID)
	if err != nil {
		return nil, err
	}
	approvalID := approvalIDFor(diffID, to, requestedBy)
	pa := &PendingApproval{
		ID:                approvalID,
		DiffID:            diffID,
		From:              string(d.State),
		To:                to,
		Note:              note,
		RequestedBy:       requestedBy,
		RequestedAt:       time.Now().UTC(),
		RequiredApprovers: required,
		Approvers:         []string{},
		Reason:            reason,
	}
	body, _ := json.Marshal(pa)
	key := "recon:approval:pending:" + approvalID
	if err := m.rdb.SetNX(ctx, key, body, 7*24*time.Hour).Err(); err != nil {
		return nil, err
	}
	// 索引 diff_id → approval_id（admin 查 diff 详情时显示 pending）
	m.rdb.Set(ctx, "recon:approval:by_diff:"+diffID, approvalID, 7*24*time.Hour)
	// ZSET 列待办（admin /pending 看板）
	m.rdb.ZAdd(ctx, "recon:approval:queue",
		redis.Z{Score: float64(pa.RequestedAt.UnixMilli()), Member: approvalID})
	return pa, nil
}

// Approve op 点赞。返 (committed, pending) — committed=true 表示批够数已落库。
func (m *Manager) Approve(ctx context.Context, approvalID, by string) (bool, *PendingApproval, error) {
	key := "recon:approval:pending:" + approvalID
	body, err := m.rdb.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return false, nil, fmt.Errorf("approval %q not found", approvalID)
	}
	if err != nil {
		return false, nil, err
	}
	var pa PendingApproval
	if err := json.Unmarshal(body, &pa); err != nil {
		return false, nil, err
	}
	if by == pa.RequestedBy {
		return false, &pa, fmt.Errorf("requester cannot self-approve")
	}
	for _, a := range pa.Approvers {
		if a == by {
			return false, &pa, fmt.Errorf("already approved by %s", by)
		}
	}
	pa.Approvers = append(pa.Approvers, by)
	body2, _ := json.Marshal(&pa)
	if len(pa.Approvers) < pa.RequiredApprovers {
		// 还差人，update 后等
		m.rdb.Set(ctx, key, body2, 7*24*time.Hour)
		return false, &pa, nil
	}
	// 够人了 → 真正落 transition
	if err := m.diffStore.Transition(ctx, pa.DiffID,
		diffstate.State(pa.To), pa.RequestedBy+"+"+strings.Join(pa.Approvers, "+"), pa.Note); err != nil {
		return false, &pa, err
	}
	// 清掉 pending key
	m.rdb.Del(ctx, key, "recon:approval:by_diff:"+pa.DiffID)
	m.rdb.ZRem(ctx, "recon:approval:queue", approvalID)
	return true, &pa, nil
}

// Reject 拒绝（任一 approver 都可以）。
func (m *Manager) Reject(ctx context.Context, approvalID, by, reason string) error {
	key := "recon:approval:pending:" + approvalID
	body, err := m.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return err
	}
	var pa PendingApproval
	json.Unmarshal(body, &pa)
	// 写到 audit hash chain 留痕
	auditKey := "recon:approval:audit:" + pa.DiffID
	auditEntry, _ := json.Marshal(map[string]any{
		"approval_id": approvalID, "rejected_by": by,
		"reason": reason, "at": time.Now().UTC().Format(time.RFC3339),
	})
	m.rdb.RPush(ctx, auditKey, auditEntry)
	m.rdb.Del(ctx, key, "recon:approval:by_diff:"+pa.DiffID)
	m.rdb.ZRem(ctx, "recon:approval:queue", approvalID)
	return nil
}

// ListPending admin /api/v1/approvals/pending —— 待办看板。
func (m *Manager) ListPending(ctx context.Context, limit int) ([]*PendingApproval, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	ids, err := m.rdb.ZRevRange(ctx, "recon:approval:queue", 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*PendingApproval, 0, len(ids))
	for _, id := range ids {
		body, err := m.rdb.Get(ctx, "recon:approval:pending:"+id).Bytes()
		if err != nil {
			continue
		}
		var pa PendingApproval
		if err := json.Unmarshal(body, &pa); err != nil {
			continue
		}
		out = append(out, &pa)
	}
	return out, nil
}

// extractAmount 从 diff.Detail 里挖金额（minor unit）。多种字段名兜底。
func extractAmount(d map[string]any) int64 {
	for _, k := range []string{"amount_minor", "amount", "delta_minor"} {
		v, ok := d[k]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case int:
			return int64(x)
		case int64:
			return x
		case float64:
			return int64(x)
		}
	}
	return 0
}

func approvalIDFor(diffID, to, by string) string {
	h := sha1.New()
	h.Write([]byte(diffID))
	h.Write([]byte{0})
	h.Write([]byte(to))
	h.Write([]byte{0})
	h.Write([]byte(by))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", time.Now().UnixNano())
	return hex.EncodeToString(h.Sum(nil)[:12])
}
