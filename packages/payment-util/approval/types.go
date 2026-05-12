package approval

import "time"

// State 状态机
type State string

const (
	StatePending   State = "pending"   // 创建后, 0 approve
	StateReviewing State = "reviewing" // 已有 ≥1 approve, 但还没集齐
	StateApproved  State = "approved"  // 集齐 RequiredApprovals
	StateRejected  State = "rejected"  // 任一 reject 即终态
	StateExecuted  State = "executed"  // 业务侧已执行
	StateExpired   State = "expired"   // 超过 ExpireAfter 没集齐 → 自动 expire
	StateCancelled State = "cancelled" // requester 撤回 (approved 后 30min 内可)
)

// Decision approver 给的决议
type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionReject  Decision = "reject"
)

// Action 一个待审批的动作
type Action struct {
	ID                string                 `json:"id"`               // ap_<32hex>
	Type              string                 `json:"type"`             // payout_high_value / rtbf_erase / sanction_unfreeze / ...
	Resource          string                 `json:"resource"`         // 业务侧 ID, e.g. payout_xxx
	Requester         string                 `json:"requester"`        // ops 账号
	RequesterIP       string                 `json:"requester_ip,omitempty"`
	RequesterAt       time.Time              `json:"requester_at"`
	RequestNote       string                 `json:"request_note,omitempty"`
	RequiredApprovals int                    `json:"required_approvals"` // 默认 2
	State             State                  `json:"state"`
	Approvals         []Approval             `json:"approvals"`
	Payload           map[string]interface{} `json:"payload,omitempty"`   // 业务上下文 (金额 / 受影响范围)
	ExpiresAt         time.Time              `json:"expires_at"`          // 默认 +7d
	ExecutedAt        time.Time              `json:"executed_at,omitempty"`
	ExecutedBy        string                 `json:"executed_by,omitempty"`
	CancelledAt       time.Time              `json:"cancelled_at,omitempty"`
}

// Approval 单次 approve/reject 记录 (append-only)
type Approval struct {
	Reviewer   string    `json:"reviewer"`
	ReviewerIP string    `json:"reviewer_ip,omitempty"`
	Decision   Decision  `json:"decision"`
	Note       string    `json:"note,omitempty"`
	ReviewedAt time.Time `json:"reviewed_at"`
}

// HasReviewer 看 reviewer 之前是否审过 — 防 reviewer 多次 approve.
func (a *Action) HasReviewer(reviewer string) bool {
	for _, ap := range a.Approvals {
		if ap.Reviewer == reviewer {
			return true
		}
	}
	return false
}

// ApproveCount 当前 approve 数 (reject 不算)
func (a *Action) ApproveCount() int {
	n := 0
	for _, ap := range a.Approvals {
		if ap.Decision == DecisionApprove {
			n++
		}
	}
	return n
}

// HasReject 是否任一 reject
func (a *Action) HasReject() bool {
	for _, ap := range a.Approvals {
		if ap.Decision == DecisionReject {
			return true
		}
	}
	return false
}
