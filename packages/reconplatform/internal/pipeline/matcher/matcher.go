// Package matcher — 无状态匹配引擎.
//
// 流水线第三段:
//
//	candidate.Layer (Pop trigger) -> Matcher (本包) -> MatchResult -> Kafka result
//
// 设计:
//   - 无状态: 输入 (TriggerKey + 同 biz_key 桶里全部 Event), 输出 MatchResult.
//             同一 input 跑多次结果一致 (除时间戳),便于水平扩展 + 重试.
//   - 可插拔规则: 内置 Go 规则 + Starlark 规则两条路径.
//                 Go 规则编译期快;Starlark 规则运行时热加载,生产改规则不重启.
//   - 锁: Pop trigger -> Lock(trigger) -> 跑规则 -> AckMatch + Unlock.
//        防止并发 worker 处理同一 trigger 出双倍结果.
//   - 兜底: 规则执行超时 / panic -> 出 ResultError 结果,不丢消息.
package matcher

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"reconcile-system/internal/pipeline/candidate"
	"reconcile-system/internal/store"
)

// Verdict 匹配裁定.
type Verdict string

const (
	// VerdictMatched 全部预期方都到齐且数据一致.
	VerdictMatched Verdict = "matched"
	// VerdictMismatched 各方到齐但数据不一致 (e.g. 金额对不上).
	VerdictMismatched Verdict = "mismatched"
	// VerdictOrphan 只有一方到 (e.g. payment-channel 有 charge,order-core 没 PI).
	VerdictOrphan Verdict = "orphan"
	// VerdictPending 还在等其他方 (TTL 内). 通常由 Sweep 触发的 trigger 出此结果.
	VerdictPending Verdict = "pending"
	// VerdictError 规则执行出错 (脚本 panic / 超时). 运维介入.
	VerdictError Verdict = "error"
)

// MatchResult 单次匹配输出.
type MatchResult struct {
	// 触发本次匹配的 (biz_key, value)
	TriggerKey candidate.TriggerKey `json:"trigger_key"`
	// 裁定
	Verdict Verdict `json:"verdict"`
	// 命中的规则名 (用于追溯)
	RuleName string `json:"rule_name"`
	// 详情:任意 JSON,各规则自定义 (金额对不上时填 "want": x, "got": y)
	Detail map[string]any `json:"detail,omitempty"`
	// 涉及的事件 (审计 / 调试)
	Events []*store.Event `json:"events,omitempty"`
	// 出错时填
	Error string `json:"error,omitempty"`
	// 元信息
	MatchedAt   time.Time     `json:"matched_at"`
	DurationMS  int64         `json:"duration_ms"`
	WorkerID    string        `json:"worker_id"`
}

// Rule 匹配规则接口. 内置 Go 规则 / Starlark 规则都实现这个.
type Rule interface {
	// Name 规则名 (唯一,用于注册 + 日志).
	Name() string
	// Match 输入 trigger + 桶里所有事件 -> 输出 MatchResult.
	// 同一 input 必须输出同一 verdict (无副作用 + 幂等).
	Match(ctx context.Context, t candidate.TriggerKey, events []*store.Event) (MatchResult, error)
}

// Registry 规则注册表. 一个 trigger 可能被多条规则评估.
type Registry struct {
	mu    sync.RWMutex
	rules []Rule
}

// NewRegistry 空注册表.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register 注册规则 (重复名报错防误操作).
func (r *Registry) Register(rule Rule) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.rules {
		if x.Name() == rule.Name() {
			return fmt.Errorf("rule %q already registered", rule.Name())
		}
	}
	r.rules = append(r.rules, rule)
	return nil
}

// MustRegister panic 版,启动期 init 用.
func (r *Registry) MustRegister(rule Rule) {
	if err := r.Register(rule); err != nil {
		panic(err)
	}
}

// List 返当前所有规则名 (admin / metric 用).
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.rules))
	for _, x := range r.rules {
		out = append(out, x.Name())
	}
	return out
}

// removeByName DynamicRegistry 用 — 把某 name 的规则从 rules 切片移除.
// 返是否真删了.
func (r *Registry) removeByName(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, x := range r.rules {
		if x.Name() == name {
			r.rules = append(r.rules[:i], r.rules[i+1:]...)
			return true
		}
	}
	return false
}

// EvalAll 对一个 trigger 跑所有规则,聚合 MatchResult 切片.
func (r *Registry) EvalAll(ctx context.Context, t candidate.TriggerKey, events []*store.Event) []MatchResult {
	r.mu.RLock()
	rules := make([]Rule, len(r.rules))
	copy(rules, r.rules)
	r.mu.RUnlock()

	out := make([]MatchResult, 0, len(rules))
	for _, rule := range rules {
		started := time.Now()
		res, err := safeMatch(ctx, rule, t, events)
		res.DurationMS = time.Since(started).Milliseconds()
		res.RuleName = rule.Name()
		res.MatchedAt = started.UTC()
		if err != nil {
			res.Verdict = VerdictError
			res.Error = err.Error()
		}
		out = append(out, res)
	}
	return out
}

// safeMatch 带 panic 兜底.
func safeMatch(ctx context.Context, rule Rule, t candidate.TriggerKey, events []*store.Event) (res MatchResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return rule.Match(ctx, t, events)
}

// ─── Worker: 从候选层 Pop → 跑规则 → Publish 结果 ──────────────

// Publisher 把 MatchResult 推下游 (实现见 publisher 包).
type Publisher interface {
	Publish(ctx context.Context, result MatchResult) error
}

// WorkerConfig worker 行为参数.
type WorkerConfig struct {
	WorkerID     string        // 标识 (Stats / log)
	BatchSize    int           // 单次 Pop 拿多少 trigger,默认 10
	PollTimeout  time.Duration // 阻塞 Pop 多久,默认 5s
	MatchTimeout time.Duration // 单条 trigger 跑规则超时,默认 10s
	KeepHistory  bool          // 匹配完是否保留桶 (审计),默认 false

	// Concurrency 一个 batch 内并发处理 trigger 的 goroutine 数.
	//   <= 1: 顺序处理 (旧行为)
	//   >= 2: 同 batch 内并发跑 processOne, 适合规则慢的场景 (e.g. Starlark 重 IO)
	// 默认 4. 注意:不同 batch 之间仍是 sequential (Pop 是阻塞的).
	Concurrency int
}

// DefaultWorkerConfig 默认.
func DefaultWorkerConfig(workerID string) WorkerConfig {
	return WorkerConfig{
		WorkerID: workerID, BatchSize: 10,
		PollTimeout: 5 * time.Second, MatchTimeout: 10 * time.Second,
		KeepHistory: false,
		Concurrency: 4,
	}
}

// Worker 拉 trigger 跑规则的实例. 多 worker 并发安全.
type Worker struct {
	cfg       WorkerConfig
	layer     candidate.Layer
	reg       *Registry
	publisher Publisher
	logger    *zap.Logger

	matched      atomic.Int64
	mismatched   atomic.Int64
	orphan       atomic.Int64
	pending      atomic.Int64
	errs         atomic.Int64
	skippedLocks atomic.Int64
}

// NewWorker 构造.
func NewWorker(cfg WorkerConfig, layer candidate.Layer, reg *Registry, pub Publisher, logger *zap.Logger) *Worker {
	if cfg.WorkerID == "" {
		cfg.WorkerID = "worker-" + numStr(int(time.Now().UnixNano()&0xfffff))
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 10
	}
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 5 * time.Second
	}
	if cfg.MatchTimeout <= 0 {
		cfg.MatchTimeout = 10 * time.Second
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Worker{
		cfg: cfg, layer: layer, reg: reg, publisher: pub,
		logger: logger.With(zap.String("worker", cfg.WorkerID)),
	}
}

// Run 阻塞循环.
func (w *Worker) Run(ctx context.Context) error {
	if w.layer == nil || w.reg == nil || w.publisher == nil {
		return errors.New("matcher worker: nil dependency")
	}
	w.logger.Info("worker started",
		zap.Int("batch", w.cfg.BatchSize),
		zap.Strings("rules", w.reg.List()))
	defer w.logger.Info("worker stopped")

	for {
		if ctx.Err() != nil {
			return nil
		}
		triggers, err := w.layer.Pop(ctx, w.cfg.BatchSize, w.cfg.PollTimeout)
		if err != nil {
			w.logger.Warn("layer.Pop failed", zap.Error(err))
			time.Sleep(time.Second)
			continue
		}
		// 并发处理 batch: 用 semaphore 控制 Concurrency goroutines.
		// 同 batch 内并发 → CPU + Redis I/O 重叠;
		// batch 与 batch 之间仍 sequential (Pop 阻塞控速).
		w.processBatch(ctx, triggers)
	}
}

// processBatch 在 Concurrency 限制下并发跑 processOne.
//
// 比 sequential 在规则 IO-bound (e.g. Starlark 调 ctx.scan 多次) 时
// 吞吐提升 ~Concurrency 倍; CPU-bound 规则提升 < Concurrency 倍 (受 GOMAXPROCS 限).
func (w *Worker) processBatch(ctx context.Context, triggers []candidate.TriggerKey) {
	if len(triggers) == 0 {
		return
	}
	if w.cfg.Concurrency <= 1 || len(triggers) == 1 {
		// 单线程路径 (避免 goroutine 开销)
		for _, t := range triggers {
			w.processOne(ctx, t)
		}
		return
	}
	sem := make(chan struct{}, w.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, t := range triggers {
		wg.Add(1)
		sem <- struct{}{}
		go func(tk candidate.TriggerKey) {
			defer wg.Done()
			defer func() { <-sem }()
			w.processOne(ctx, tk)
		}(t)
	}
	wg.Wait()
}

// processOne 锁 + Get + Eval + Publish + Ack.
func (w *Worker) processOne(ctx context.Context, t candidate.TriggerKey) {
	unlock, ok, err := w.layer.Lock(ctx, t)
	if err != nil {
		w.logger.Warn("lock failed", zap.Stringer("trigger", t), zap.Error(err))
		return
	}
	if !ok {
		w.skippedLocks.Add(1)
		return // 别人在处理
	}
	defer unlock()

	matchCtx, cancel := context.WithTimeout(ctx, w.cfg.MatchTimeout)
	defer cancel()

	events, err := w.layer.Get(matchCtx, t.BizKey, t.Value)
	if err != nil {
		w.logger.Warn("layer.Get failed", zap.Stringer("trigger", t), zap.Error(err))
		return
	}

	results := w.reg.EvalAll(matchCtx, t, events)
	for i := range results {
		results[i].TriggerKey = t
		results[i].WorkerID = w.cfg.WorkerID
		// 没 Events 时填上, 给下游审计:
		if results[i].Events == nil {
			results[i].Events = events
		}
		switch results[i].Verdict {
		case VerdictMatched:
			w.matched.Add(1)
		case VerdictMismatched:
			w.mismatched.Add(1)
		case VerdictOrphan:
			w.orphan.Add(1)
		case VerdictPending:
			w.pending.Add(1)
		case VerdictError:
			w.errs.Add(1)
		}
		if pErr := w.publisher.Publish(matchCtx, results[i]); pErr != nil {
			w.logger.Warn("publisher.Publish failed",
				zap.Stringer("trigger", t),
				zap.String("rule", results[i].RuleName),
				zap.Error(pErr))
		}
	}
	// pending 状态保留桶 (等其他方);其它状态可 ack 清掉
	allPending := true
	for _, r := range results {
		if r.Verdict != VerdictPending {
			allPending = false
			break
		}
	}
	if !allPending {
		if err := w.layer.AckMatch(matchCtx, t, w.cfg.KeepHistory); err != nil {
			w.logger.Warn("AckMatch failed", zap.Stringer("trigger", t), zap.Error(err))
		}
	}
}

// Stats 暴露给 Prometheus.
type WorkerStats struct {
	Matched, Mismatched, Orphan, Pending, Errors, SkippedLocks int64
}

// Stats 取计数.
func (w *Worker) Stats() WorkerStats {
	return WorkerStats{
		Matched: w.matched.Load(), Mismatched: w.mismatched.Load(),
		Orphan: w.orphan.Load(), Pending: w.pending.Load(),
		Errors: w.errs.Load(), SkippedLocks: w.skippedLocks.Load(),
	}
}

// ─── 内置 Go 规则示例 ─────────────────────────────────────────

// CrossServicePresenceRule 通用规则: 检查 trigger 桶里是否所有期望服务都到齐.
//
// 例: pi_id 桶必须有 order-core + payment-channel + accounting-system 三方.
// 缺任一 -> Orphan;到齐 -> Matched.
type CrossServicePresenceRule struct {
	RuleName        string
	BizKey          string   // e.g. "pi_id"
	ExpectedServices []string // e.g. ["order-core", "payment-channel", "accounting-system"]
}

// Name impl.
func (r *CrossServicePresenceRule) Name() string { return r.RuleName }

// Match impl.
func (r *CrossServicePresenceRule) Match(_ context.Context, t candidate.TriggerKey, events []*store.Event) (MatchResult, error) {
	if t.BizKey != r.BizKey {
		return MatchResult{Verdict: VerdictPending}, nil
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Service] = true
	}
	missing := []string{}
	for _, svc := range r.ExpectedServices {
		if !seen[svc] {
			missing = append(missing, svc)
		}
	}
	if len(missing) == 0 {
		return MatchResult{Verdict: VerdictMatched, Detail: map[string]any{
			"services_present": r.ExpectedServices,
		}}, nil
	}
	if len(seen) == 0 {
		return MatchResult{Verdict: VerdictOrphan, Detail: map[string]any{
			"missing": missing, "present": []string{},
		}}, nil
	}
	return MatchResult{Verdict: VerdictPending, Detail: map[string]any{
		"missing": missing, "present_count": len(seen),
	}}, nil
}

// AmountEqualityRule 通用规则: 在跨服务 events 里抽 amount 字段比较.
//
// 例: order-core.pi.amount == payment-channel.tx.amount == accounting.le.amount
type AmountEqualityRule struct {
	RuleName    string
	BizKey      string
	AmountField string                       // 默认 "amount"
	Pairs       []AmountPair                 // 哪些 (svc, table) 上抽 amount
}

// AmountPair 单方 amount 取值描述.
type AmountPair struct {
	Service string
	Table   string
}

// Name impl.
func (r *AmountEqualityRule) Name() string { return r.RuleName }

// Match impl.
func (r *AmountEqualityRule) Match(_ context.Context, t candidate.TriggerKey, events []*store.Event) (MatchResult, error) {
	if t.BizKey != r.BizKey {
		return MatchResult{Verdict: VerdictPending}, nil
	}
	field := r.AmountField
	if field == "" {
		field = "amount"
	}
	amounts := map[string]int64{} // "<svc>:<table>" -> amount
	for _, e := range events {
		for _, p := range r.Pairs {
			if e.Service == p.Service && e.Table == p.Table {
				if v, ok := e.After[field]; ok {
					if n, ok := toInt64(v); ok {
						amounts[p.Service+":"+p.Table] = n
					}
				}
			}
		}
	}
	if len(amounts) < len(r.Pairs) {
		return MatchResult{Verdict: VerdictPending, Detail: map[string]any{
			"reason": "not all pairs present", "got": amounts,
		}}, nil
	}
	// 全部相等?
	var first int64
	allEqual := true
	i := 0
	for _, v := range amounts {
		if i == 0 {
			first = v
		} else if v != first {
			allEqual = false
		}
		i++
	}
	if allEqual {
		return MatchResult{Verdict: VerdictMatched, Detail: map[string]any{
			"amount": first, "pairs": amounts,
		}}, nil
	}
	return MatchResult{Verdict: VerdictMismatched, Detail: map[string]any{
		"amounts": amounts,
	}}, nil
}

// toInt64 把 any (JSON number / int / float) 转 int64.
func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case float32:
		return int64(x), true
	}
	return 0, false
}

// numStr 极简 int → string (避免循环 import).
func numStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
