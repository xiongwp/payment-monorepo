// Package feedback 收集"决策实际结果"反馈，给 ML 训练 + 规则迭代提供
// label。架构图里的"反馈闭环"。
//
// 来源：
//
//  1. **review 决议**：人工 approve / reject 直接当 label
//  2. **chargeback / dispute**：商户后续被拒付，回写为 fraud=true
//  3. **业务 confirm**：商户主动确认这笔是 / 不是欺诈
//
// 数据 schema：每条 Outcome 关联到一个 decision_id（audit）：
//
//	decision_id    audit 链的关联键
//	source         review_human / dispute / merchant_confirm
//	is_fraud       true / false
//	at             UTC ts
//	actor          who reported
//	notes          free text
//
// 离线消费：导出全部 outcome × audit join → CSV / parquet 给 ML 团队训练
// 下一版模型；同时统计每条规则的 precision / recall（命中且 is_fraud=true
// 的占比 = precision）。
package feedback

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// Source 反馈来源。
type Source string

const (
	SourceReviewHuman     Source = "review_human"
	SourceDispute         Source = "dispute"
	SourceMerchantConfirm Source = "merchant_confirm"
)

// Outcome 单条反馈。
type Outcome struct {
	DecisionID string    `json:"decision_id"`
	Source     Source    `json:"source"`
	IsFraud    bool      `json:"is_fraud"`
	At         time.Time `json:"at"`
	Actor      string    `json:"actor,omitempty"`
	Notes      string    `json:"notes,omitempty"`
}

// Recorder 反馈持久化接口。
type Recorder interface {
	Record(o Outcome) error
	Get(decisionID string) []*Outcome
	Recent(limit int) []*Outcome
}

// MemRecorder 内存版。生产 PG 实现：append-only `risk_outcome` 表 +
// (decision_id, at) 索引；同 decision_id 可有多个 outcome（review human
// + dispute + merchant_confirm 是相互独立的反馈源）。
type MemRecorder struct {
	mu     sync.RWMutex
	byID   map[string][]*Outcome
	all    []*Outcome
	maxAll int
}

func NewMemRecorder(maxAll int) *MemRecorder {
	if maxAll <= 0 {
		maxAll = 100_000
	}
	return &MemRecorder{
		byID:   make(map[string][]*Outcome),
		maxAll: maxAll,
	}
}

func (m *MemRecorder) Record(o Outcome) error {
	if o.DecisionID == "" {
		return errors.New("feedback: decision_id required")
	}
	if o.Source == "" {
		return errors.New("feedback: source required")
	}
	if o.At.IsZero() {
		o.At = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := o
	m.byID[o.DecisionID] = append(m.byID[o.DecisionID], &cp)
	m.all = append(m.all, &cp)
	// 简单上限：超过 maxAll 时把最老的从 all 里裁掉（byID 仍保留，便于
	// audit-by-id 查询）；生产 DB 用 retention policy。
	if len(m.all) > m.maxAll {
		m.all = m.all[len(m.all)-m.maxAll:]
	}
	return nil
}

func (m *MemRecorder) Get(decisionID string) []*Outcome {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.byID[decisionID]
	if len(src) == 0 {
		return nil
	}
	out := make([]*Outcome, len(src))
	for i, o := range src {
		cp := *o
		out[i] = &cp
	}
	return out
}

func (m *MemRecorder) Recent(limit int) []*Outcome {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 || limit > len(m.all) {
		limit = len(m.all)
	}
	out := make([]*Outcome, 0, limit)
	// 最近的在前
	for i := len(m.all) - 1; i >= 0 && len(out) < limit; i-- {
		cp := *m.all[i]
		out = append(out, &cp)
	}
	// 二次保险：按 At DESC 排序（all 大体上是 append 顺序但保险起见）
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}
