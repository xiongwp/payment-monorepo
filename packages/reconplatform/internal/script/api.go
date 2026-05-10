// Package script 定义 Starlark 对账脚本的运行时 API（Context + Diff + Result）。
//
// 设计原则：
//   - 暴露给脚本作者的接口尽量"业务化" — 关心的是 order_id / pi_id /
//     transaction_id 这些业务 key，不是 Redis key 长啥样
//   - 重 helper（find / int / str）放 EventList / Event 上，让脚本可链式调用
//   - 输出风格：def check(ctx) → return list[dict]
//     脚本不用 ctx.add_diff(...) 副作用（虽然 Context 上仍保留这接口给老 yaegi 兼容）
//
// 运行时由 starlark_engine.go 的 Engine 把脚本源码编译 + 调 check(ctx)。

package script

import (
	"context"
	"fmt"
	"sync"
	"time"

	"reconcile-system/internal/store"
)

// Context 单次脚本运行的上下文。脚本通过 starlark_api.go 的 wrapContext 暴露给脚本侧。
//
// 不要把 Context 长期持有；每次 Run 时由 engine 构造新的实例。
type Context struct {
	// 内部依赖
	searcher *store.Searcher
	logger   Logger

	// 调用环境
	Ctx       context.Context // Go context（含 deadline / trace_id）
	StartedAt time.Time

	// 给脚本读的元信息
	Now    time.Time         // 脚本开始执行时的 wall clock；脚本里所有"今天"判断都用这个
	Params map[string]string // 调用方传的参数（cron / 手动触发可填）

	// 累计 diff（输出，老风格 ctx.add_diff 兼容；新风格 def check return list 走 engine）
	mu    sync.Mutex
	diffs []Diff
	stats Stats
}

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
		Ctx:       ctx,
		StartedAt: time.Now(),
		Now:       time.Now(),
		Params:    params,
	}
}

// ─── 数据访问 API（脚本主要用这些） ──────────────────────────────

// GetByIndex 用业务 key 跨服务关联：返回所有引用 (idxName, value) 的事件。
//
// 脚本里通过 starlark wrapper：events = ctx.get_by_index("pi_id", "pi_xxx")
// 然后 events.find("order-core", "payment_intents") / events.find("...") 等等。
func (c *Context) GetByIndex(idxName, value string) store.EventList {
	c.stats.IndexLookups++
	out, err := c.searcher.SearchByIndex(c.Ctx, idxName, value)
	if err != nil {
		c.logger.Warn("get_by_index failed", "idx", idxName, "value", value, "err", err.Error())
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
		c.logger.Warn("get failed", "svc", service, "table", table, "pk", pk, "err", err.Error())
		c.stats.RedisErrors++
		return nil
	}
	return e
}

// ScanIndex 列出某 idxName 下所有 value（带前缀过滤）。
//
// 脚本里：pi_ids = ctx.scan_index("pi_id", "pi_", 1000)
// 适合"扫最近 N 条"全表对账场景；prefix 留空 = 扫所有。
func (c *Context) ScanIndex(idxName, prefix string, limit int) []string {
	c.stats.Scans++
	out, err := c.searcher.ScanIndex(c.Ctx, idxName, prefix, limit)
	if err != nil {
		c.logger.Warn("scan_index failed", "idx", idxName, "err", err.Error())
		c.stats.RedisErrors++
		return nil
	}
	return out
}

// ScanService 列出某 (service, table) 下的全部 row（最多 limit 条）。
//
// 脚本里：rows = ctx.scan("order-core", "payment_intent")
// 实现：SCAN recon:event:<svc>:<table>*:* → GET 每条；O(n)。大表慎用，
// 优先 get_by_index 缩范围。
func (c *Context) ScanService(service, table string, limit int) store.EventList {
	c.stats.Scans++
	out, err := c.searcher.ScanService(c.Ctx, service, table, limit)
	if err != nil {
		c.logger.Warn("scan failed", "svc", service, "table", table, "err", err.Error())
		c.stats.RedisErrors++
		return nil
	}
	return out
}

// ─── Diff 输出 API（兼容 yaegi 老风格 ctx.add_diff） ──────────────

// Diff 一条对账差异。Type 是脚本作者自定义的分类 tag（"missing" / "amount_mismatch" 等）。
type Diff struct {
	Type   string `json:"type"`
	Key    string `json:"key"`              // 关联的业务 key（pi_id / order_id / ...）
	Want   any    `json:"want,omitempty"`   // 期望值
	Got    any    `json:"got,omitempty"`    // 实际值
	Detail any    `json:"detail,omitempty"` // 任意附加信息
}

// AddDiff 累计一条 diff。Starlark 新风格脚本通过 return list 输出，
// 这个 API 留给少数副作用风格脚本兼容（不推荐）。
func (c *Context) AddDiff(diffType, key string, detail any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.diffs = append(c.diffs, Diff{Type: diffType, Key: key, Detail: detail})
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
	IndexLookups int   `json:"index_lookups"`
	PointLookups int   `json:"point_lookups"`
	Scans        int   `json:"scans"`
	RedisErrors  int   `json:"redis_errors"`
	HTTPCalls    int   `json:"http_calls"`
	ExecMillis   int64 `json:"exec_ms"`
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

// ─── helpers ────────────────────────────────────────────────────

type noopLogger struct{}

func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

// formatKV 留给将来若需要 grpc-style key=val log 拼接。
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

var _ = formatKV
