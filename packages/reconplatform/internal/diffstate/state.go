// Package diffstate — 对账差异处置工作流。
//
// 一条 diff 不只"被发现"就完了，oncall / 运营要给它一个**结论**：
//
//	open                初始状态：刚被脚本写出来
//	acked               值班看到了，正在处理（不再发新告警）
//	resolved            已确认是真问题且修了（如 webhook 漏推已补发）
//	false_positive      规则误报，关下次同形 diff 不再告警
//	expired             N 天内没人理 → 自动 expire（默认 30d）
//
// 状态机:
//
//	open ──ack──→ acked ──resolve──→ resolved
//	   │             │
//	   │             └──flag_fp──→ false_positive
//	   │
//	   └──flag_fp──→ false_positive
//	   │
//	   └──(N 天 GC)──→ expired
//
// 所有状态迁移落 audit_log（who/when/note），合规月底导 CSV 给财务看。
//
// 存储：
//
//	recon:diff:state:<diff_id>           HASH  当前状态 + 元信息
//	recon:diff:audit:<diff_id>           LIST  操作历史（RPUSH JSON）
//	recon:diff:by_state:<state>           ZSET  member=diff_id score=updated_ms
//	                                            （admin web "我的待办" 列表）
//
// diff_id 由 publisher 生成（首次写入时）：sha1(script_id|run_id|type|key|index)。
// 同一 diff 二次重复出现也是同一 ID（脚本本来就该幂等）。

package diffstate

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// State 处置状态。
type State string

const (
	StateOpen          State = "open"
	StateAcked         State = "acked"
	StateResolved      State = "resolved"
	StateFalsePositive State = "false_positive"
	StateExpired       State = "expired"
)

// IsTerminal 终态判定 — 终态不再生成新告警 / 不参与 dedup 入库。
func (s State) IsTerminal() bool {
	return s == StateResolved || s == StateFalsePositive || s == StateExpired
}

// Diff 一条对账差异在状态化存储里的视图。
type Diff struct {
	ID        string         `json:"id"`         // sha1 三元组
	ScriptID  string         `json:"script_id"`
	RunID     string         `json:"run_id"`
	Type      string         `json:"type"`
	Key       string         `json:"key"`
	Detail    map[string]any `json:"detail"`
	State     State          `json:"state"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	UpdatedBy string         `json:"updated_by"`
	Note      string         `json:"note,omitempty"`
}

// AuditEntry 状态迁移审计记录。
type AuditEntry struct {
	From      State     `json:"from"`
	To        State     `json:"to"`
	By        string    `json:"by"`
	At        time.Time `json:"at"`
	Note      string    `json:"note,omitempty"`
}

// Store Redis 实现的状态存储 + audit log。
type Store struct {
	r redis.UniversalClient
}

// New 构造。
func New(r redis.UniversalClient) *Store { return &Store{r: r} }

// IDFor 给 (script_id, run_id, type, key, idx) 生成稳定 diff_id。
//
// 同一脚本 run 的同 type+key 重复（罕见 — 脚本作者写错产生）会有不同 idx。
// idx 推荐用 enumerate(diffs) 的下标。
func IDFor(scriptID, runID, diffType, key string, idx int) string {
	h := sha1.New()
	h.Write([]byte(scriptID))
	h.Write([]byte{0})
	h.Write([]byte(runID))
	h.Write([]byte{0})
	h.Write([]byte(diffType))
	h.Write([]byte{0})
	h.Write([]byte(key))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", idx)
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// CreateOpen 一条新发现的 diff 落 store，初始 state=open。
// 已存在（同 ID）时不动 — 状态由人工或 GC 流转。
func (s *Store) CreateOpen(ctx context.Context, d Diff) error {
	d.State = StateOpen
	now := time.Now()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now

	key := "recon:diff:state:" + d.ID
	exists, err := s.r.Exists(ctx, key).Result()
	if err != nil {
		return err
	}
	if exists > 0 {
		// 不覆盖：已有状态可能是 acked 等
		return nil
	}
	body, _ := json.Marshal(d)
	pipe := s.r.Pipeline()
	pipe.Set(ctx, key, body, 90*24*time.Hour) // 90d TTL，防 Redis 暴涨
	pipe.ZAdd(ctx, "recon:diff:by_state:"+string(StateOpen),
		redis.Z{Score: float64(now.UnixMilli()), Member: d.ID})
	pipe.RPush(ctx, "recon:diff:audit:"+d.ID, mustJSON(AuditEntry{
		From: "", To: StateOpen, By: "system", At: now,
		Note: "first detection",
	}))
	_, err = pipe.Exec(ctx)
	return err
}

// Transition 改 diff 状态（admin web 调）。
//   - from→to 不合法 → 返 error（拒动）
//   - 写 audit_log，更新 by_state ZSET 索引
func (s *Store) Transition(ctx context.Context, diffID string, to State, by, note string) error {
	if !validTransition(to) {
		return fmt.Errorf("unknown target state %q", to)
	}
	key := "recon:diff:state:" + diffID
	body, err := s.r.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return fmt.Errorf("diff %q not found", diffID)
	}
	if err != nil {
		return err
	}
	var d Diff
	if err := json.Unmarshal(body, &d); err != nil {
		return fmt.Errorf("diff %q corrupt: %w", diffID, err)
	}
	if !canTransition(d.State, to) {
		return fmt.Errorf("invalid transition %s → %s", d.State, to)
	}
	prev := d.State
	now := time.Now()
	d.State = to
	d.UpdatedAt = now
	d.UpdatedBy = by
	if note != "" {
		d.Note = note
	}
	body2, _ := json.Marshal(d)

	pipe := s.r.Pipeline()
	pipe.Set(ctx, key, body2, 90*24*time.Hour)
	pipe.ZRem(ctx, "recon:diff:by_state:"+string(prev), diffID)
	pipe.ZAdd(ctx, "recon:diff:by_state:"+string(to),
		redis.Z{Score: float64(now.UnixMilli()), Member: diffID})
	pipe.RPush(ctx, "recon:diff:audit:"+diffID, mustJSON(AuditEntry{
		From: prev, To: to, By: by, At: now, Note: note,
	}))
	_, err = pipe.Exec(ctx)
	return err
}

// Get 取一条 diff 当前状态 + 详情。
func (s *Store) Get(ctx context.Context, diffID string) (*Diff, error) {
	body, err := s.r.Get(ctx, "recon:diff:state:"+diffID).Bytes()
	if err == redis.Nil {
		return nil, fmt.Errorf("diff %q not found", diffID)
	}
	if err != nil {
		return nil, err
	}
	var d Diff
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ListByState 列某状态下的 diff（admin web "我的待办"）。倒序按 updated_at。
func (s *Store) ListByState(ctx context.Context, state State, limit int) ([]*Diff, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ids, err := s.r.ZRevRange(ctx, "recon:diff:by_state:"+string(state), 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*Diff, 0, len(ids))
	for _, id := range ids {
		d, err := s.Get(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// Audit 拿一条 diff 的全量 audit_log（按时间正序）。
func (s *Store) Audit(ctx context.Context, diffID string) ([]AuditEntry, error) {
	rows, err := s.r.LRange(ctx, "recon:diff:audit:"+diffID, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]AuditEntry, 0, len(rows))
	for _, raw := range rows {
		var e AuditEntry
		if err := json.Unmarshal([]byte(raw), &e); err == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// ExpireOld 后台 worker 周期调，把 N 天前仍 open 的 diff 自动迁 expired。
// 默认 30 天，dev 调短到 1 天测试。
func (s *Store) ExpireOld(ctx context.Context, age time.Duration) (int, error) {
	cutoff := time.Now().Add(-age).UnixMilli()
	ids, err := s.r.ZRangeByScore(ctx, "recon:diff:by_state:"+string(StateOpen),
		&redis.ZRangeBy{Min: "-inf", Max: fmt.Sprintf("%d", cutoff), Count: 500}).Result()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		if err := s.Transition(ctx, id, StateExpired, "system", "auto-expired by GC"); err == nil {
			count++
		}
	}
	return count, nil
}

// validTransition 目标状态白名单。
func validTransition(s State) bool {
	switch s {
	case StateOpen, StateAcked, StateResolved, StateFalsePositive, StateExpired:
		return true
	}
	return false
}

// canTransition 状态机迁移规则。
//
//	open  → acked / resolved / false_positive / expired
//	acked → resolved / false_positive / expired
//	终态 → 不可迁出（resolved / false_positive / expired 不能再改）
func canTransition(from, to State) bool {
	if from.IsTerminal() {
		return false
	}
	switch from {
	case StateOpen:
		return to == StateAcked || to == StateResolved || to == StateFalsePositive || to == StateExpired
	case StateAcked:
		return to == StateResolved || to == StateFalsePositive || to == StateExpired
	}
	return false
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
