// rule_audit_chain.go: RuleAuditStore 的 tamper-evident wrapper。
//
// 每条 RuleAuditEntry 写入时同步追加一条 chain record（stream="rule_audit"）。
// 跟 review chain.go 同样思路；这里因为 RuleAuditStore + ChainWriter 都在 audit
// 包里，直接放同包不需要导入。
//
// 为什么 rule_audit 必须 chain：rule_audit 本身就是 "谁改了规则" 的取证日志，
// 是合规审查的重点对象。如果 rule_audit 表本身可篡改，那监管来查的时候相当
// 于"防贼的锁没上"。
//
// chain 独立链（stream="rule_audit"），跟其它 stream 互不影响。
//
// record schema：
//
//	{
//	  "type":        "rule_audit",
//	  "rule_id":     "<id>",
//	  "op":          "reload" | "mode_change" | "rule_update" | "rule_disable" | ...
//	  "actor":       "<who>",
//	  "occurred_at": "<RFC3339-utc>",
//	  "before":      <raw JSON or null>,
//	  "after":       <raw JSON or null>,
//	  "reason":      "<free text>"
//	}
//
// 失败兜底：inner.Write 返 err 时 ChainWriter 已经推过 prev_hash；我们仍把
// inner 的 err 返回给调用方（保持 Write 语义），chain 失败不会让 inner 也失败。
package audit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xiongwp/risk-manage/internal/metrics"
)

// chainRuleAuditStore wraps RuleAuditStore，每条 Write 同步上链。
type chainRuleAuditStore struct {
	RuleAuditStore
	cw *ChainWriter
}

// WrapRuleAuditWithChain 把 RuleAuditStore 包成 chain-aware。cw==nil 或 inner
// ==nil → 返回原 store（noop 路径，cfg 关闭时零开销）。
func WrapRuleAuditWithChain(s RuleAuditStore, cw *ChainWriter) RuleAuditStore {
	if cw == nil || s == nil {
		return s
	}
	return &chainRuleAuditStore{RuleAuditStore: s, cw: cw}
}

func (c *chainRuleAuditStore) Write(ctx context.Context, entry RuleAuditEntry) error {
	// 先写 inner（业务数据落库），再上链。chain 失败不影响 inner write 结果。
	innerErr := c.RuleAuditStore.Write(ctx, entry)

	// inner 内部会把 zero OccurredAt 填成 now.UTC()，但 entry 是 by-value
	// 拷贝传入的，本地这份还可能是 zero → chain record 用本地版本，独立填。
	occurredAt := entry.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	body := map[string]any{
		"type":        "rule_audit",
		"op":          entry.Action,
		"rule_id":     entry.RuleID,
		"actor":       entry.Actor,
		"reason":      entry.Reason,
		"occurred_at": occurredAt.UTC().Format(time.RFC3339Nano),
	}
	if len(entry.Before) > 0 {
		body["before"] = jsonRawToAny(entry.Before)
	}
	if len(entry.After) > 0 {
		body["after"] = jsonRawToAny(entry.After)
	}
	if err := c.cw.Append(ctx, body); err != nil {
		metrics.AuditChainWriteTotal.WithLabelValues("rule_audit", "err").Inc()
	} else {
		metrics.AuditChainWriteTotal.WithLabelValues("rule_audit", "ok").Inc()
	}
	return innerErr
}

// jsonRawToAny 把 json.RawMessage 反序列化成 any（map/slice/scalar），让它
// 进入 chain body 后能跟 sortedAnyMap / encoding/json 一起得到稳定输出。
// 解析失败 → 退化成 string（原 raw bytes），永远不丢字段。
func jsonRawToAny(raw json.RawMessage) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}
