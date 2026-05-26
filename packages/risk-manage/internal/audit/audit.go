// Package audit 风控决策证据链。每笔 Screen 落一条 DecisionAudit：
// rule_version + 输入快照 + verdict + score + reasons + 时间戳。
//
// 用途：
//  1. **合规取证**：监管 / 法务要求"为什么这笔被拒/通过"，能 by decision_id 还原全过程
//  2. **离线分析**：导出给 ML 团队当 label / 训练样本
//  3. **回放**：上线新规则前用历史 decision 回放，对比新旧 verdict
//
// 接口分两层：
//   - Sink：异步落 audit 记录的存储后端（默认 ring buffer + zap 日志；
//     生产可换成 Kafka / S3 append / DB write-once 表）
//   - Recorder：service 层调一次 .Record(decision)；内部走 sink，不阻塞主流程
package audit

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"
)

// DecisionAudit 一次 Screen 决策的完整快照。JSON 兼容，便于落库 / Kafka。
type DecisionAudit struct {
	DecisionID string    `json:"decision_id"` // 全局唯一
	OccurredAt time.Time `json:"occurred_at"`
	// RuleVersion 当前规则集稳定 hash 的低 32 位（fnv32a over 排序后的
	// (rule_id, active_version) 元组集合）。
	// 历史上是 engine.RuleCount() — 即"规则总数当版本号"，不能反查 / 不能
	// 回滚；切到 RuleSetHash 后同一规则集任意切换都能由该字段精确定位到
	// engine 的状态快照。clickhouse_sink 还在用 int32(RuleVersion)，所以
	// 保持 int 类型，只截断到 32 位。
	RuleVersion int `json:"rule_version"`
	// RuleSetHash 同上的 64-bit-friendly hex 表示（"a3f2"…）；新字段方便
	// 在 admin UI / kafka 消费者直接 string 比对，不必担心负数 / 截断。
	RuleSetHash string `json:"rule_set_hash,omitempty"`
	// 输入快照（不含敏感字段；卡号 / token 由 service 层提前脱敏）
	Input AuditInput `json:"input"`
	// 输出
	Verdict   string     `json:"verdict"` // ALLOW / DENY / REVIEW
	RiskScore int        `json:"risk_score"`
	RiskLevel string     `json:"risk_level"`
	Hits      []AuditHit `json:"hits"`
	// ShadowHits shadow 模式规则命中（仅观察，不影响 verdict）。
	ShadowHits []AuditHit `json:"shadow_hits,omitempty"`
	// MLScore + MLModelVer ML 推理结果（service.Screen 在 Evaluate 前填充）。
	// 审计重现决策必须知道用的哪个模型版本 → ModelVer 是关键字段。
	MLScore    float64 `json:"ml_score,omitempty"`
	MLModelVer string  `json:"ml_model_ver,omitempty"`
	// EvalDurationMs 端到端引擎执行耗时
	EvalDurationMs float64 `json:"eval_duration_ms"`
}

// AuditInput 入参快照子集。**不包含**卡号 / token / passwd 这类敏感字段。
type AuditInput struct {
	PaymentIntentID string            `json:"payment_intent_id"`
	MerchantID      string            `json:"merchant_id"`
	CustomerID      string            `json:"customer_id"`
	Amount          int64             `json:"amount"`
	Currency        string            `json:"currency"`
	PaymentMethod   string            `json:"payment_method"`
	Country         string            `json:"country"`
	IPAddress       string            `json:"ip_address,omitempty"`
	DeviceID        string            `json:"device_id,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// AuditHit 单条规则命中明细。
type AuditHit struct {
	RuleID   string `json:"rule_id"`
	RuleName string `json:"rule_name"`
	Decision string `json:"decision"` // ALLOW / DENY / REVIEW
	Detail   string `json:"detail"`
}

// Sink 落 audit 记录的存储后端。实现需要是无阻塞的（< 1ms）；
// 慢后端（Kafka / DB）应在内部用 channel buffer + worker。
type Sink interface {
	Write(ctx context.Context, a *DecisionAudit)
}

// LogSink 把每条决策写到 zap.Info，带 audit_event=risk_decision 标签。
// log shipping 配置规则把这些行路由到独立审计 sink。生产推荐配合 KafkaSink
// 之类的持久化存储（本包不实现，调用方插）。
type LogSink struct {
	Logger *zap.Logger
}

func (s *LogSink) Write(_ context.Context, a *DecisionAudit) {
	if s == nil || s.Logger == nil {
		return
	}
	// json.Marshal 单次约 1-2µs，远低于决策本身（~ms 级）；不在主路径上瓶颈。
	body, err := json.Marshal(a)
	if err != nil {
		s.Logger.Warn("audit marshal failed", zap.Error(err))
		return
	}
	s.Logger.Info("risk_decision_audit",
		zap.String("audit_event", "risk_decision"),
		zap.String("decision_id", a.DecisionID),
		zap.String("verdict", a.Verdict),
		zap.Int("score", a.RiskScore),
		zap.ByteString("body", body),
	)
}

// MemSink 进程内 ring buffer，给 admin /admin/audit/decisions 端点查最近 N 条。
// 不是持久化方案 —— 进程重启清空。生产用 LogSink + 外部存储为准。
//
// 同时维护一个 PI → decision_id 的 LRU 索引，给 dispute 自动反馈用：
// order-core 的 webhook 只知道 payment_intent_id，需要在这里反查到风控
// 当时给这笔交易打的 decision_id 才能落 outcome。索引上限跟 buf 一致，
// 老 decision 被 ring 覆写时同步从 index 移除（保证不内存泄漏）。
type MemSink struct {
	mu      sync.RWMutex
	cap     int
	buf     []*DecisionAudit
	cursor  int
	full    bool
	piIndex map[string]string // payment_intent_id → decision_id
}

// NewMemSink 容量 cap 的 ring buffer。cap <= 0 时回落 1024。
func NewMemSink(cap int) *MemSink {
	if cap <= 0 {
		cap = 1024
	}
	return &MemSink{cap: cap, buf: make([]*DecisionAudit, cap), piIndex: make(map[string]string)}
}

func (s *MemSink) Write(_ context.Context, a *DecisionAudit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 被覆写的旧条目从 PI 索引清掉（避免无限增长）。
	if old := s.buf[s.cursor]; old != nil && old.Input.PaymentIntentID != "" {
		// 仅当 index 仍指向这个老 decision_id 时才删（防止 PI 复用场景误删）。
		if existing, ok := s.piIndex[old.Input.PaymentIntentID]; ok && existing == old.DecisionID {
			delete(s.piIndex, old.Input.PaymentIntentID)
		}
	}
	s.buf[s.cursor] = a
	if a != nil && a.Input.PaymentIntentID != "" && a.DecisionID != "" {
		s.piIndex[a.Input.PaymentIntentID] = a.DecisionID
	}
	s.cursor = (s.cursor + 1) % s.cap
	if s.cursor == 0 {
		s.full = true
	}
}

// SearchFilter MemSink 范围查询条件。零值字段视作 "不过滤"。
//
// 前端常见用例：
//   - "查商户 X 最近 24h 的所有 DENY"  → MerchantID + Verdict + Since
//   - "查客户 Y 所有触发 review 的"    → CustomerID + Verdict
//   - "查 IP 192.168.1.1 的近期决策"  → IPAddress
type SearchFilter struct {
	MerchantID string
	CustomerID string
	IPAddress  string
	Verdict    string    // ALLOW / REVIEW / DENY；空 = 全部
	Since      time.Time // OccurredAt >= Since；零值 = 不限
	Until      time.Time // OccurredAt <= Until；零值 = 不限
	Limit      int       // 返回上限；<= 0 → 200
}

// Search 按 SearchFilter 扫 ring buffer 返回匹配。O(N) 每次（ring 上限 ~4096
// 条问题不大）；生产 ClickHouse / PG sink 应直接 SELECT WHERE...
//
// 返回结果按 OccurredAt 降序（最新在前）。
func (s *MemSink) Search(f SearchFilter) []*DecisionAudit {
	if f.Limit <= 0 {
		f.Limit = 200
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	size := s.cursor
	if s.full {
		size = s.cap
	}
	out := make([]*DecisionAudit, 0, f.Limit)
	for i := 0; i < size; i++ {
		idx := (s.cursor - 1 - i + s.cap) % s.cap
		a := s.buf[idx]
		if a == nil {
			continue
		}
		if f.MerchantID != "" && a.Input.MerchantID != f.MerchantID {
			continue
		}
		if f.CustomerID != "" && a.Input.CustomerID != f.CustomerID {
			continue
		}
		if f.IPAddress != "" && a.Input.IPAddress != f.IPAddress {
			continue
		}
		if f.Verdict != "" && a.Verdict != f.Verdict {
			continue
		}
		if !f.Since.IsZero() && a.OccurredAt.Before(f.Since) {
			continue
		}
		if !f.Until.IsZero() && a.OccurredAt.After(f.Until) {
			continue
		}
		out = append(out, a)
		if len(out) >= f.Limit {
			break
		}
	}
	return out
}

// LookupByPaymentIntent 按 PI 反查 decision_id。命中 → ("decision_id", true)；
// 不命中（决策已经被 ring buffer 覆写 / 或本进程没处理过该 PI）→ ("", false)。
//
// 给 /admin/feedback/dispute 用：order-core dispute webhook 只知道 PI，需要
// 这个反向索引才能定位到风控当时的决策记录。生产应该用 ClickHouse / PG
// 长期存储 (decision_id, payment_intent_id) 索引代替本地 LRU。
func (s *MemSink) LookupByPaymentIntent(pi string) (string, bool) {
	if pi == "" {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.piIndex[pi]
	return id, ok
}

// LookupByDecisionID 按 decision_id 找回完整 audit 条目（O(N) 扫 ring buffer）。
// 给 outcome lag 计算用：拿到 decision OccurredAt 才能算 (now - then)。
// 找不到 → nil（决策已被 ring 覆写或不存在）。
func (s *MemSink) LookupByDecisionID(decisionID string) *DecisionAudit {
	if decisionID == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.buf {
		if a != nil && a.DecisionID == decisionID {
			return a
		}
	}
	return nil
}

// Recent 返回最近 limit 条决策（按 OccurredAt 降序）。limit > cap 时返回全量。
func (s *MemSink) Recent(limit int) []*DecisionAudit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	size := s.cursor
	if s.full {
		size = s.cap
	}
	if limit <= 0 || limit > size {
		limit = size
	}
	out := make([]*DecisionAudit, 0, limit)
	// 从 cursor-1 往回走 limit 步
	for i := 0; i < limit; i++ {
		idx := (s.cursor - 1 - i + s.cap) % s.cap
		if s.buf[idx] != nil {
			out = append(out, s.buf[idx])
		}
	}
	return out
}

// MultiSink 把一条决策同时投递到多个 sink（如 LogSink + MemSink + KafkaSink）。
// 所有 sink 必须是无阻塞实现（依次 Write）。
type MultiSink []Sink

func (m MultiSink) Write(ctx context.Context, a *DecisionAudit) {
	for _, s := range m {
		s.Write(ctx, a)
	}
}
