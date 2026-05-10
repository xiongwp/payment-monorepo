// Package invariant — 声明式金额恒等检查（不写脚本）。
//
// 80% 对账场景就是"sum(A) ?= sum(B) ± tolerance"。让运营写 YAML 比写
// Starlark 脚本简单 10 倍。
//
// 配置形如（config-center key=reconplatform/invariants 或 yaml）：
//
//	invariants:
//	  - name: pi_amount_eq_account_tx_sum
//	    description: PaymentIntent.amount == sum(account_transaction.amount where related_pi_id=pi.id)
//	    schedule: "*/15 * * * *"
//	    severity: P0
//	    sources:
//	      A:
//	        service: order-core
//	        table: payment_intent
//	        amount_column: amount
//	        filter: "status='succeeded'"
//	        group_by: id            # PI 一行一组（PK）
//	      B:
//	        service: accounting-system
//	        table: account_transaction
#         amount_column: amount
#         filter: ""               # 全收
#         group_by: related_pi_id  # 按 PI 关联拉取
//	    tolerance_minor: 0
//	    skew_direction: any         # any/positive/negative
//
// Engine 跑：
//   1. 拉 sources.A 全集（按 group_by 字段做 dict[group_key]→ amount_sum）
//   2. 拉 sources.B 同样形式
//   3. diff = A[k] - B[k]，超 tolerance_minor 报 diff
//   4. 不存在的 key（A 有 B 无 / 反之）也报（缺失 leg）
//
// 限制：sources 只支持 Redis 里已经索引化的数据（CDC 摄入的 binlog 或 external
// 文件源）。复杂 SQL JOIN 还是写脚本。

package invariant

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"reconcile-system/internal/diffstate"
)

// Spec 一条恒等检查的声明（YAML / JSON 反序列化目标）。
type Spec struct {
	Name           string  `yaml:"name" json:"name"`
	Description    string  `yaml:"description" json:"description"`
	Schedule       string  `yaml:"schedule" json:"schedule"`
	Severity       string  `yaml:"severity" json:"severity"`
	Sources        struct {
		A SourceRef `yaml:"A" json:"A"`
		B SourceRef `yaml:"B" json:"B"`
	} `yaml:"sources" json:"sources"`
	ToleranceMinor int64  `yaml:"tolerance_minor" json:"tolerance_minor"`
	SkewDirection  string `yaml:"skew_direction" json:"skew_direction"` // any / positive / negative
}

// SourceRef 一边的数据来源声明。
type SourceRef struct {
	Service      string `yaml:"service" json:"service"`
	Table        string `yaml:"table" json:"table"`
	AmountColumn string `yaml:"amount_column" json:"amount_column"`
	Filter       string `yaml:"filter,omitempty" json:"filter,omitempty"`            // 简化：仅支持 col=val 形式
	GroupBy      string `yaml:"group_by" json:"group_by"`
}

// Engine 单例，注入 redis + diffStore；启动期 LoadSpecs() 装载，跑 cron。
type Engine struct {
	rdb       redis.UniversalClient
	diffStore *diffstate.Store
	log       *zap.Logger
	cron      *cron.Cron
	mu        sync.Mutex
	specs     map[string]Spec
	cronIDs   map[string]cron.EntryID
}

func New(rdb redis.UniversalClient, diffStore *diffstate.Store, log *zap.Logger) *Engine {
	if log == nil {
		log = zap.NewNop()
	}
	return &Engine{
		rdb: rdb, diffStore: diffStore, log: log,
		cron:    cron.New(),
		specs:   map[string]Spec{},
		cronIDs: map[string]cron.EntryID{},
	}
}

func (e *Engine) Start() { e.cron.Start() }
func (e *Engine) Stop(ctx context.Context) {
	stopped := e.cron.Stop()
	select {
	case <-stopped.Done():
	case <-ctx.Done():
	}
}

// LoadSpecs 替换全部 specs，重排 cron。
func (e *Engine) LoadSpecs(ctx context.Context, specs []Spec) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	// 清旧 cron entries
	for _, id := range e.cronIDs {
		e.cron.Remove(id)
	}
	e.cronIDs = map[string]cron.EntryID{}
	e.specs = map[string]Spec{}

	for _, s := range specs {
		if s.Name == "" {
			continue
		}
		e.specs[s.Name] = s
		if s.Schedule == "" {
			continue
		}
		spec := s // capture
		id, err := e.cron.AddFunc(s.Schedule, func() {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := e.RunOne(rctx, spec); err != nil {
				e.log.Warn("invariant run failed",
					zap.String("name", spec.Name), zap.Error(err))
			}
		})
		if err != nil {
			return fmt.Errorf("invariant %q schedule %q: %w", s.Name, s.Schedule, err)
		}
		e.cronIDs[s.Name] = id
	}
	e.log.Info("invariants loaded", zap.Int("count", len(specs)))
	return nil
}

// RunOne 跑一条 Invariant：扫 A、扫 B、对比、写 diff。
func (e *Engine) RunOne(ctx context.Context, spec Spec) error {
	t0 := time.Now()
	a, err := e.aggregate(ctx, spec.Sources.A)
	if err != nil {
		return fmt.Errorf("aggregate A: %w", err)
	}
	b, err := e.aggregate(ctx, spec.Sources.B)
	if err != nil {
		return fmt.Errorf("aggregate B: %w", err)
	}

	diffsCount := 0
	allKeys := mergeKeys(a, b)
	runID := fmt.Sprintf("inv_%s_%d", spec.Name, t0.Unix())
	for i, key := range allKeys {
		va, oka := a[key]
		vb, okb := b[key]
		if !oka {
			diffsCount++
			e.writeDiff(ctx, spec, runID, i, key, "missing_in_A", 0, vb, "B has but A missing")
			continue
		}
		if !okb {
			diffsCount++
			e.writeDiff(ctx, spec, runID, i, key, "missing_in_B", va, 0, "A has but B missing")
			continue
		}
		delta := va - vb
		if absInt64(delta) > spec.ToleranceMinor {
			if !skewAllowed(delta, spec.SkewDirection) {
				continue
			}
			diffsCount++
			e.writeDiff(ctx, spec, runID, i, key, "amount_mismatch", va, vb,
				fmt.Sprintf("delta=%d (tolerance=%d)", delta, spec.ToleranceMinor))
		}
	}
	e.log.Info("invariant run done",
		zap.String("name", spec.Name),
		zap.Int("a_groups", len(a)),
		zap.Int("b_groups", len(b)),
		zap.Int("diffs", diffsCount),
		zap.Duration("dur", time.Since(t0)))
	return nil
}

// aggregate 扫 source 全集，按 group_by 字段累加 amount_column。
//
// 实现：SCAN recon:event:<svc>:<table>:* → 拉 JSON → group_by + amount。
// 这里走 N+1 SCAN+GET，对 100 万行级别 OK；千万级建议落 ClickHouse 走 SQL。
func (e *Engine) aggregate(ctx context.Context, ref SourceRef) (map[string]int64, error) {
	pattern := fmt.Sprintf("recon:event:%s:%s*:*", ref.Service, ref.Table)
	out := map[string]int64{}
	iter := e.rdb.Scan(ctx, 0, pattern, 1000).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		raw, err := e.rdb.Get(ctx, key).Bytes()
		if err != nil {
			continue
		}
		var ev struct {
			After map[string]any `json:"After"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		row := ev.After
		if row == nil {
			continue
		}
		if !matchFilter(row, ref.Filter) {
			continue
		}
		gk := stringField(row, ref.GroupBy)
		if gk == "" {
			continue
		}
		amt := numField(row, ref.AmountColumn)
		out[gk] += amt
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// writeDiff 把违例写进 diffstate.Store。
func (e *Engine) writeDiff(ctx context.Context, spec Spec, runID string, idx int,
	key, vtype string, va, vb int64, msg string) {
	if e.diffStore == nil {
		return
	}
	d := diffstate.Diff{
		ID:       diffstate.IDFor("invariant_"+spec.Name, runID, vtype, key, idx),
		ScriptID: "invariant_" + spec.Name,
		RunID:    runID,
		Type:     vtype,
		Key:      key,
		Detail: map[string]any{
			"invariant":   spec.Name,
			"description": spec.Description,
			"severity":    spec.Severity,
			"a_amount":    va,
			"b_amount":    vb,
			"message":     msg,
		},
	}
	_ = e.diffStore.CreateOpen(ctx, d)
}

// ─── helpers ───────────────────────────────────────────────────────

// matchFilter 极简过滤器：只支持 col=val 形式（一条等值），否则全允。
// 真实生产换更强表达式（cel/expr）。
func matchFilter(row map[string]any, filter string) bool {
	if filter == "" {
		return true
	}
	for i := 0; i < len(filter); i++ {
		if filter[i] == '=' {
			col := trim(filter[:i])
			val := trim(filter[i+1:])
			val = trim2(val, '\'')
			actual := stringField(row, col)
			return actual == val
		}
	}
	return true
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func trim2(s string, c byte) string {
	for len(s) > 0 && s[0] == c {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == c {
		s = s[:len(s)-1]
	}
	return s
}

func stringField(row map[string]any, col string) string {
	v, ok := row[col]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func numField(row map[string]any, col string) int64 {
	v, ok := row[col]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

func mergeKeys(a, b map[string]int64) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(a)+len(b))
	for k := range a {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	return out
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func skewAllowed(delta int64, dir string) bool {
	switch dir {
	case "", "any":
		return true
	case "positive":
		return delta > 0
	case "negative":
		return delta < 0
	}
	return true
}
