// chain.go: review.Store 的 tamper-evident wrapper。
//
// 在每次 state-changing op (Claim/Release/Escalate/AddNote/Decide) 成功后，
// 同步往 audit.ChainWriter 追加一条 chain record。原 Store 接口完全不破坏：
// chain 失败 / cfg 关闭都走 noop，业务流程不阻塞。
//
// 为什么 review 也要 chain：监管 (SOC2 CC7.2 / PCI 10.5) 要求"人工干预记录
// 不可篡改"。review notes / decided_by / decide_reason 都是 mutable row，
// 不上 chain 一个流氓 DBA 可以随手 UPDATE risk_review SET decided_by='xxx'
// WHERE id=Y。
//
// chain 是**独立链**（stream="review"），跟 decisions / rule_audit / outcomes
// 互不影响——见 audit/chain_stream.go 包注释。
//
// record schema（snake_case JSON，跟 DecisionAudit 风格一致）：
//
//	{
//	  "type": "review",
//	  "op":   "claim" | "release" | "escalate" | "note" | "decide",
//	  "case_id":    "<decision_id>",
//	  "actor":      "<analyst-id>",
//	  "occurred_at":"<RFC3339-utc>",
//	  "before":     {"status":"pending",  "assigned_to":""},
//	  "after":      {"status":"in_review","assigned_to":"alice"},
//	  "extra":      {"reason":"...", "body":"..."}    // op-specific
//	}
package review

import (
	"context"
	"time"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/metrics"
)

// chainStore wraps a Store and append-chains every state-changing op.
// Push 不上链（每次 Screen 都 push，量太大；DecisionAudit 已经在 chain 里）。
type chainStore struct {
	Store
	cw *audit.ChainWriter
}

// WrapWithChain 把 Store 包成 chain-aware。cw==nil → 返回原 store 不变（cfg
// 关闭时零开销）。
func WrapWithChain(s Store, cw *audit.ChainWriter) Store {
	if cw == nil || s == nil {
		return s
	}
	return &chainStore{Store: s, cw: cw}
}

func (c *chainStore) appendChain(op string, item *Item, before, extra map[string]any) {
	if c.cw == nil || item == nil {
		return
	}
	body := map[string]any{
		"type":        "review",
		"op":          op,
		"case_id":     item.ID,
		"actor":       item.AssignedTo,
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
		"before":      before,
		"after": map[string]any{
			"status":         string(item.Status),
			"assigned_to":    item.AssignedTo,
			"escalate_level": item.EscalateLevel,
			"decided_by":     item.DecidedBy,
		},
	}
	if extra != nil {
		body["extra"] = extra
	}
	if err := c.cw.Append(context.Background(), body); err != nil {
		metrics.AuditChainWriteTotal.WithLabelValues("review", "err").Inc()
		return
	}
	metrics.AuditChainWriteTotal.WithLabelValues("review", "ok").Inc()
}

func (c *chainStore) Claim(id, actor string) (*Item, error) {
	before := snapshotBefore(c.Store, id)
	it, err := c.Store.Claim(id, actor)
	if err == nil {
		c.appendChain("claim", it, before, map[string]any{"actor": actor})
	}
	return it, err
}

func (c *chainStore) Release(id, actor string) (*Item, error) {
	before := snapshotBefore(c.Store, id)
	it, err := c.Store.Release(id, actor)
	if err == nil {
		c.appendChain("release", it, before, map[string]any{"actor": actor})
	}
	return it, err
}

func (c *chainStore) Escalate(id, actor, reason string) (*Item, error) {
	before := snapshotBefore(c.Store, id)
	it, err := c.Store.Escalate(id, actor, reason)
	if err == nil {
		c.appendChain("escalate", it, before, map[string]any{"actor": actor, "reason": reason})
	}
	return it, err
}

func (c *chainStore) AddNote(id, actor, body string) (*Item, error) {
	before := snapshotBefore(c.Store, id)
	it, err := c.Store.AddNote(id, actor, body)
	if err == nil {
		c.appendChain("note", it, before, map[string]any{"actor": actor, "body": body})
	}
	return it, err
}

func (c *chainStore) Decide(id string, action Action, actor, reason string) (*Item, error) {
	before := snapshotBefore(c.Store, id)
	it, err := c.Store.Decide(id, action, actor, reason)
	if err == nil {
		c.appendChain("decide", it, before, map[string]any{
			"action": string(action), "actor": actor, "reason": reason,
		})
	}
	return it, err
}

// snapshotBefore 取 Store 当前状态做 "before" 快照。Get 返回 nil（不存在）
// 时返回 nil 让 chain record before 字段直接缺省。
func snapshotBefore(s Store, id string) map[string]any {
	it := s.Get(id)
	if it == nil {
		return nil
	}
	return map[string]any{
		"status":         string(it.Status),
		"assigned_to":    it.AssignedTo,
		"escalate_level": it.EscalateLevel,
		"decided_by":     it.DecidedBy,
	}
}
