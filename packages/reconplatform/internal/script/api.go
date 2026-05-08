// Package script 定义对账脚本的运行时 API（Context + Diff + Result）。
//
// 设计原则：
//   - 暴露给脚本作者的接口尽量"业务化"——他们关心的是 order_id / pi_id /
//     transaction_id 这些业务 key，不是 Redis key 长啥样。
//   - 重 helper（Find / FindAll / Int / Str）放 EventList / Event 上，让
//     脚本可以链式调用：
//        events := ctx.GetByIndex("pi_id", piID)
//        order := events.Find("order-core", "payment_intents")
//        if order.Int("amount") != events.Find("...").Int("amount") { ... }
//   - Result 结构尽量简单：脚本只关心 AddDiff(type, key, detail)。
//
// 运行时由 yaegi 解释器把脚本源码加载进来，调用 Check(ctx) 函数。
package script

import (
	"context"
	"fmt"
	"sync"
	"time"

	"reconcile-system/internal/store"
)

// Context 单次脚本运行的上下文。脚本通过它访问数据 + 记录 diff。
//
// 不要把 Context 长期持有；每次 Run 时由 engine 构造新的实例。
type Context struct {
	// 内部依赖
	searcher *store.Searcher
	logger   Logger
	sqlDBs   map[string]*sqlDBInternal // service/shard → DB（advanced API）

	// 调用环境
	Ctx       context.Context // Go context（含 deadline / trace_id）
	StartedAt time.Time

	// 给脚本读的元信息
	Now    time.Time // 脚本开始执行时的 wall clock；脚本里所有"今天"判断都用这个
	Params map[string]string // 调用方传的参数（cron / 手动触发可填）
	Logger Logger // 暴露给脚本（脚本里 ctx.Logger.Info(...) 使用）

	// 累计 diff（输出）
	mu      sync.Mutex
	diffs   []Diff
	stats   Stats
}

// sqlDBInternal 跟 api_advanced.go 的 SQLDB 配套。
// 让 advanced API 能访问 sqlDBs 而不破坏 Context 字段大小写。
type sqlDBInternal = SQLDB

// Logger 给脚本用的日志接口（默认包 zap，但脚本侧只看到 Info / Warn / Error 三个方法）。
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// NewContext engine 调用，给脚本运行时构造。
func NewContext(ctx context.Context, searcher *store.Searcher, logger Logger, params map[string]string) *Context {
	if logger == nil {
		logger = noopLogger{}
	}
	return &Context{
		searcher:  searcher,
		logger:    logger,
		Logger:    logger, // 同步导出给脚本
		Ctx:       ctx,
		StartedAt: time.Now(),
		Now:       time.Now(),
		Params:    params,
	}
}

// ─── 数据访问 API（脚本主要用这些）──────────────────────────────

// GetByIndex 用业务 key 跨服务关联：返回所有引用 (idxName, value) 的事件。
//
// 例：events := ctx.GetByIndex("pi_id", "pi_xxx")
//     order := events.Find("order-core", "payment_intents")
//     txn   := events.Find("accounting-system", "account_transaction")
//     chg   := events.Find("payment-channel", "card_charges")
func (c *Context) GetByIndex(idxName, value string) store.EventList {
	c.stats.IndexLookups++
	out, err := c.searcher.SearchByIndex(c.Ctx, idxName, value)
	if err != nil {
		c.logger.Warn("GetByIndex failed", "idx", idxName, "value", value, "err", err.Error())
		c.stats.RedisErrors++
		return nil
	}
	return out
}

// Get 按 (svc, table, pk) 拿一条事件；找不到返 nil。
func (c *Context) Get(service, table, pk string) *store.Event {
	c.stats.PointLookups++
	e, err := c.searcher.GetEvent(c.Ctx, service, table, pk)
	if err != nil {
		c.logger.Warn("Get failed", "svc", service, "table", table, "pk", pk, "err", err.Error())
		c.stats.RedisErrors++
		return nil
	}
	return e
}

// ScanIndex 列出某 idxName 下所有 value（带前缀过滤）。
//
// 例：piIDs := ctx.ScanIndex("pi_id", "pi_", 1000)
// 适合"扫最近 N 条"这种全表对账场景；prefix 留空 = 扫所有。
func (c *Context) ScanIndex(idxName, prefix string, limit int) []string {
	c.stats.Scans++
	out, err := c.searcher.ScanIndex(c.Ctx, idxName, prefix, limit)
	if err != nil {
		c.logger.Warn("ScanIndex failed", "idx", idxName, "err", err.Error())
		c.stats.RedisErrors++
		return nil
	}
	return out
}

// Schema 拿表的列定义（脚本里动态决定取哪个列时用，不常见）。
func (c *Context) Schema(service, table string) string {
	s, err := c.searcher.GetSchema(c.Ctx, service, table)
	if err != nil {
		return ""
	}
	return s
}

// ─── Diff 输出 API ────────────────────────────────────────────

// Diff 一条对账差异。Type 是脚本作者自定义的分类 tag（"missing" / "amount_mismatch" 等）。
type Diff struct {
	Type   string `json:"type"`
	Key    string `json:"key"`             // 关联的业务 key（pi_id / order_id / ...）
	Want   any    `json:"want,omitempty"`  // 期望值
	Got    any    `json:"got,omitempty"`   // 实际值
	Detail any    `json:"detail,omitempty"` // 任意附加信息
}

// AddDiff 累计一条 diff。脚本里反复调，最后由 engine 收集到 Result。
func (c *Context) AddDiff(diffType, key string, detail any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.diffs = append(c.diffs, Diff{Type: diffType, Key: key, Detail: detail})
}

// AddCompare 对比类 diff 的快捷方式：amount 不一致等场景。
func (c *Context) AddCompare(diffType, key string, want, got any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.diffs = append(c.diffs, Diff{Type: diffType, Key: key, Want: want, Got: got})
}

// Diffs 返回所有累计的 diff（engine 调）。
func (c *Context) Diffs() []Diff {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Diff, len(c.diffs))
	copy(out, c.diffs)
	return out
}

// Stats 内部计数（运行结束后写到 result 给 SLO / debug 用）。
type Stats struct {
	IndexLookups int `json:"index_lookups"`
	PointLookups int `json:"point_lookups"`
	Scans        int `json:"scans"`
	RedisErrors  int `json:"redis_errors"`
	SQLQueries   int `json:"sql_queries"`
	HTTPCalls    int `json:"http_calls"`
}

// GetStats 拿当前累计统计（engine 调）。
func (c *Context) GetStats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// ─── Result 结构（engine 写入 Redis 持久化）────────────────────

// Result 一次脚本运行的完整结果。
type Result struct {
	ScriptID    string    `json:"script_id"`
	RunID       string    `json:"run_id"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Status      string    `json:"status"` // "success" / "error" / "timeout"
	Error       string    `json:"error,omitempty"`
	Diffs       []Diff    `json:"diffs"`
	Stats       Stats     `json:"stats"`
	TriggeredBy string    `json:"triggered_by"` // "manual" / "cron" / "stream:<svc>:<table>"
}

// ─── time helpers ──────────────────────────────────────────────

// Last24Hours 给脚本作者用的 helper（"我要扫最近 24h"）。
// 返一个 (since, until) 对，脚本可以用来过滤 ctx.Now / event.Timestamp。
func Last24Hours(now time.Time) (since, until time.Time) {
	return now.Add(-24 * time.Hour), now
}

// LastNHours 同上，但指定 N。
func LastNHours(now time.Time, n int) (since, until time.Time) {
	return now.Add(time.Duration(-n) * time.Hour), now
}

// ─── helpers ────────────────────────────────────────────────────

type noopLogger struct{}

func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

// formatKV 把 ["k1","v1","k2","v2"] 拼成 "k1=v1 k2=v2"，给 stdout / 脚本 dump 看。
func formatKV(kv []any) string {
	if len(kv) == 0 {
		return ""
	}
	out := ""
	for i := 0; i+1 < len(kv); i += 2 {
		if out != "" {
			out += " "
		}
		out += fmt.Sprintf("%v=%v", kv[i], kv[i+1])
	}
	return out
}
