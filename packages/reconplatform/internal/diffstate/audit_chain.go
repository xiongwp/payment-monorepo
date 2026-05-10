// audit_chain.go — diff 状态迁移防篡改哈希链。
//
// 合规背景（PCI DSS Req 10 / SOX）：
// 任何敏感数据状态变更要有"不可篡改"的审计链路。运营事后改 Redis LIST
// 或者 attacker 拿到 root 改 audit log 都能"洗掉"操作历史。哈希链让
// 检查方能离线验证："给我一段 audit_log，我自己重算 hash 确认没人改过"。
//
// 设计：
//
//	每条 audit entry 写入时算：
//	    chain_hash[N] = sha256(chain_hash[N-1] || canonical_json(entry))
//
//	entry 字段固定包含 from / to / by / at / note；chain_hash 跟 entry
//	一起 RPUSH 持久化；下一条 RPUSH 时读上一条算 hash 续接。
//
// 验证：VerifyChain(diff_id) 重新跑一遍 hash 链，任一不匹配返 mismatch index。
//
// 攻击场景：
//
//	(a) 改某条历史 audit 内容 → 它的 chain_hash 还能伪造，但下一条续接的
//	    输入变了，发现匹配不上 → mismatch
//	(b) 直接删某条 → LIST 长度对不上预期，前后 chain_hash 断开
//	(c) 加假 audit → 算法对入侵者不可预测（Redis 服务端不算）
//
// 不防：拿到 Redis 写权限的 attacker 可以从某点往后**整段重写**一个
// 自洽的链。完全防需要把 chain_head 周期性 anchor 到 append-only 媒介
// （区块链 / S3 Object Lock / Git）— 留作 Phase 2。

package diffstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// chainHashOf 给定 prev_hash + entry 算下一条 chain_hash。
//
// canonical_json: json.Marshal 的 entry，固定字段顺序由 struct 字段顺序决定
// （不用 map[string]any —— 那是无序的）。
func chainHashOf(prevHash string, entry AuditEntry) string {
	payload, _ := json.Marshal(struct {
		Prev string     `json:"prev"`
		E    AuditEntry `json:"entry"`
	}{
		Prev: prevHash,
		E:    entry,
	})
	h := sha256.Sum256(payload)
	return hex.EncodeToString(h[:])
}

// ChainedAuditEntry = AuditEntry + chain_hash 字段。
// 落 Redis 的实际存储是这个结构（合规需要时用 AuditWithChain 拿原始记录）。
type ChainedAuditEntry struct {
	AuditEntry
	ChainHash string `json:"chain_hash"`
}

// AuditWithChain 取审计 log 并保留每条的 chain_hash 字段（合规审计用）。
func (s *Store) AuditWithChain(ctx context.Context, diffID string) ([]ChainedAuditEntry, error) {
	rows, err := s.r.LRange(ctx, "recon:diff:audit:"+diffID, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]ChainedAuditEntry, 0, len(rows))
	for _, raw := range rows {
		var c ChainedAuditEntry
		if err := json.Unmarshal([]byte(raw), &c); err == nil {
			out = append(out, c)
		}
	}
	return out, nil
}

// VerifyChain 重新跑哈希链，校验所有 chain_hash 都对得上。
//
//   ok=true             → 链完整无篡改
//   ok=false + badIndex → 第 badIndex 条出现 hash 不匹配
//
// caller 通常在合规审计 / 月度报告时调；不在 hot path。
func (s *Store) VerifyChain(ctx context.Context, diffID string) (ok bool, badIndex int, err error) {
	entries, err := s.AuditWithChain(ctx, diffID)
	if err != nil {
		return false, -1, err
	}
	prev := ""
	for i, e := range entries {
		expected := chainHashOf(prev, e.AuditEntry)
		if e.ChainHash != expected {
			return false, i, fmt.Errorf(
				"audit chain mismatch at index %d: stored=%s expected=%s",
				i, e.ChainHash, expected)
		}
		prev = e.ChainHash
	}
	return true, -1, nil
}

// pushAudit 把 entry 加 chain_hash 后 RPUSH。
//
// 替代 state.go 里直接 RPUSH(mustJSON(AuditEntry))。CreateOpen / Transition
// 改成调本函数。
func (s *Store) pushAudit(ctx context.Context, diffID string, entry AuditEntry) error {
	// 读上一条 chain_hash（链尾）
	prevRaw, err := s.r.LIndex(ctx, "recon:diff:audit:"+diffID, -1).Result()
	prevHash := ""
	if err == nil && prevRaw != "" {
		var prev ChainedAuditEntry
		if json.Unmarshal([]byte(prevRaw), &prev) == nil {
			prevHash = prev.ChainHash
		}
	}
	// （err == redis.Nil → 链空，prevHash="" 是正确初始值）
	chained := ChainedAuditEntry{
		AuditEntry: entry,
		ChainHash:  chainHashOf(prevHash, entry),
	}
	body, _ := json.Marshal(chained)
	return s.r.RPush(ctx, "recon:diff:audit:"+diffID, body).Err()
}
