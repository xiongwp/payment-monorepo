// Package featurestore —— ML 特征快照存储。
//
// 用途：每次 Screen 时把 Features + decision_id + outcome_label（最终 dispute /
// chargeback 结果，由 feedback.Recorder 后续 join 上）落到一个 append-only
// 流，给以下场景用：
//
//   - 离线 retraining：把所有 (features, label) 拉出来训新模型
//   - 在线 replay / A-B：拿历史样本喂给新模型评估 champion vs challenger
//   - 数据科学探索：分析哪些特征最有预测力
//   - SOX/合规：还原"为什么这笔交易当时被判 X"的完整证据链
//
// 设计原则：
//
//  1. **不阻塞 Screen 主路径**：Save 异步、fail-open
//  2. **最小 schema 耦合**：Features blob 用 JSON，未来加新字段无需 migration
//  3. **可插拔后端**：Mem (dev) / Eventbus (kafka/stream 流式) / Postgres
//     (low-throughput 直接落库)
//
// 后续配合 cmd/retrain 一个独立 worker 离线消费这条流出新模型。
package featurestore

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/xiongwp/risk-manage/internal/eventbus"
	"github.com/xiongwp/risk-manage/internal/mlscore"
)

// Snapshot 一次 Screen 的特征 + 决策快照。
//
// outcome_label 字段在快照时为空；OutcomeRecorder（feedback 包）拿到 dispute /
// chargeback 信号后通过 decision_id 反查 + 异步 Update。当前 Mem 实现支持
// in-place update；流式后端（Eventbus）改成发"补丁事件"由消费侧 left-join。
type Snapshot struct {
	DecisionID string             `json:"decision_id"`
	OccurAt    time.Time          `json:"occur_at"`
	Features   *mlscore.Features  `json:"features"`
	Verdict    string             `json:"verdict"`     // ALLOW / REVIEW / DENY
	RiskScore  int                `json:"risk_score"`
	MLScore    float64            `json:"ml_score,omitempty"`
	MLModelVer string             `json:"ml_model_ver,omitempty"`
	// OutcomeLabel 由 OutcomeRecorder 反向 join：true=fraud, false=legit, nil=unknown
	OutcomeLabel *bool `json:"outcome_label,omitempty"`
	OutcomeAt    *time.Time `json:"outcome_at,omitempty"`
}

// Store 特征快照存储抽象。
type Store interface {
	// Save 写一条 snapshot。重复 decision_id 视调用方语义；Mem 实现允许覆盖。
	// 失败只 log，不返 error 给主路径。
	Save(ctx context.Context, snap Snapshot)
	// SetOutcome 给历史 snapshot 打标签。decision_id 不存在 → no-op。
	SetOutcome(ctx context.Context, decisionID string, isFraud bool, at time.Time)
	// Get 单条；调试 / 离线脚本用。
	Get(decisionID string) *Snapshot
	// PurgeByCustomer 按 customer_id 清掉所有 snapshot（GDPR right-to-erasure）。
	// 返回 purge 掉的条数。重复调幂等。
	PurgeByCustomer(ctx context.Context, customerID string) int
}

// ─── Mem 实现 ─────────────────────────────────────────────────────────

// MemStore 单进程 in-memory；重启清零。给 dev / 单测用，生产替换 EventbusStore
// 或 PostgresStore。容量上限 256k 条，超出按 FIFO 淘汰最旧的。
type MemStore struct {
	mu  sync.RWMutex
	m   map[string]*Snapshot
	max int
}

func NewMemStore(maxEntries int) *MemStore {
	if maxEntries <= 0 {
		maxEntries = 256 * 1024
	}
	return &MemStore{m: make(map[string]*Snapshot, 1024), max: maxEntries}
}

func (s *MemStore) Save(_ context.Context, snap Snapshot) {
	if snap.DecisionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := snap
	s.m[snap.DecisionID] = &cp
	// FIFO 兜底淘汰：超过 max 时清掉最早的 1/4（粗粒度，不维护严格 LRU）
	if len(s.m) > s.max {
		i := 0
		drop := s.max / 4
		for k := range s.m {
			if i >= drop {
				break
			}
			delete(s.m, k)
			i++
		}
	}
}

func (s *MemStore) SetOutcome(_ context.Context, decisionID string, isFraud bool, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap, ok := s.m[decisionID]; ok {
		snap.OutcomeLabel = &isFraud
		snap.OutcomeAt = &at
	}
}

func (s *MemStore) Get(decisionID string) *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if snap, ok := s.m[decisionID]; ok {
		cp := *snap
		return &cp
	}
	return nil
}

// PurgeByCustomer 删所有 features.CustomerID == customerID 的 snapshot。
// O(N)；GDPR 用，频率低，acceptable。
func (s *MemStore) PurgeByCustomer(_ context.Context, customerID string) int {
	if customerID == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	purged := 0
	for k, snap := range s.m {
		if snap.Features != nil && snap.Features.CustomerID == customerID {
			delete(s.m, k)
			purged++
		}
	}
	return purged
}

// ─── Eventbus 实现 ──────────────────────────────────────────────────

// EventbusStore 把每条 snapshot 序列化发到 eventbus（Topic="ml.feature_snapshot"）。
// 离线 retrain worker 用 consumer-group 消费这条流写到 parquet / clickhouse。
//
// 优势：跨进程持久化、可重放、外部消费者（python ML pipeline）零侵入。
// 局限：Get 拿不回来（事件流只能 forward，不能随机访问）；想拿单条要 join 别处。
type EventbusStore struct {
	pub eventbus.Publisher
}

func NewEventbusStore(pub eventbus.Publisher) *EventbusStore {
	return &EventbusStore{pub: pub}
}

// FeatureSnapshotTopic 离线消费者订这个 topic。
const FeatureSnapshotTopic eventbus.Topic = "ml.feature_snapshot"

// FeatureOutcomeTopic SetOutcome 写到这条流；消费者侧 left-join 到主流。
const FeatureOutcomeTopic eventbus.Topic = "ml.feature_outcome"

func (s *EventbusStore) Save(ctx context.Context, snap Snapshot) {
	if s.pub == nil {
		return
	}
	body, err := json.Marshal(snap)
	if err != nil {
		return
	}
	_ = s.pub.Publish(ctx, eventbus.Event{
		Topic:   FeatureSnapshotTopic,
		Key:     snap.DecisionID,
		OccurAt: snap.OccurAt,
		Data:    body,
	})
}

type outcomePatch struct {
	DecisionID string    `json:"decision_id"`
	IsFraud    bool      `json:"is_fraud"`
	At         time.Time `json:"at"`
}

func (s *EventbusStore) SetOutcome(ctx context.Context, decisionID string, isFraud bool, at time.Time) {
	if s.pub == nil {
		return
	}
	body, _ := json.Marshal(outcomePatch{DecisionID: decisionID, IsFraud: isFraud, At: at})
	_ = s.pub.Publish(ctx, eventbus.Event{
		Topic: FeatureOutcomeTopic,
		Key:   decisionID,
		Data:  body,
	})
}

// EventbusStore 不支持 Get（流式后端拿不回单条）；离线侧用 join 实现。
func (s *EventbusStore) Get(string) *Snapshot { return nil }

// PurgeByCustomer 在事件流里发一条 "erase" 事件，consumer 在自己侧实现物理删除。
func (s *EventbusStore) PurgeByCustomer(ctx context.Context, customerID string) int {
	if s.pub == nil || customerID == "" {
		return 0
	}
	body, _ := json.Marshal(map[string]any{"customer_id": customerID, "erase": true})
	_ = s.pub.Publish(ctx, eventbus.Event{
		Topic: FeatureSnapshotTopic,
		Key:   customerID,
		Data:  body,
	})
	return 0 // 流式后端此处无法精确 count；返 0 是契约
}

// ─── Noop ─────────────────────────────────────────────────────────

// NoopStore 关掉 feature snapshot 时用。
type NoopStore struct{}

func (NoopStore) Save(context.Context, Snapshot)                       {}
func (NoopStore) SetOutcome(context.Context, string, bool, time.Time)  {}
func (NoopStore) Get(string) *Snapshot                                 { return nil }
func (NoopStore) PurgeByCustomer(context.Context, string) int          { return 0 }
