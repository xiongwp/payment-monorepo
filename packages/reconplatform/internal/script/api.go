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

// SearcherIface 是 Context 对底层数据源的最小依赖。
//
// 生产由 *store.Searcher 实现 (Redis-backed);
// 本地 CLI 测试由 store.FixtureSearcher 实现 (内存 fixture-backed)。
//
// 抽接口后 Context 可在不起 Redis 的情况下被单测 / CLI 直接用。
type SearcherIface interface {
	SearchByIndex(ctx context.Context, idxName, value string) (store.EventList, error)
	GetEvent(ctx context.Context, service, table, pk string) (*store.Event, error)
	ScanService(ctx context.Context, service, table string, limit int) (store.EventList, error)
	ScanIndex(ctx context.Context, idxName, prefix string, limit int) ([]string, error)
}

// LogEntry 一次脚本运行中通过 ctx.log_info/warn/error 产生的日志条目.
//
// 同时包含 print() 输出 (Level="print", KV nil) — 给 dry-run / 编辑器 UI 显示用,
// 用户写 print(...) 调试时能直接在右侧 console 面板看到.
type LogEntry struct {
	Level string         `json:"level"`        // info / warn / error / print
	Msg   string         `json:"msg"`
	KV    map[string]any `json:"kv,omitempty"`
}

// Context 单次脚本运行的上下文。脚本通过 starlark_api.go 的 wrapContext 暴露给脚本侧。
//
// 不要把 Context 长期持有；每次 Run 时由 engine 构造新的实例。
type Context struct {
	// 内部依赖
	searcher SearcherIface
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

	// print() / ctx.log_* 输出的捕获缓冲. dry-run 端点会读出来塞 Result.Logs,
	// 让前端 console 面板能展示;生产路径 logger 仍接 zap 不丢日志.
	logs []LogEntry
}

// Logger 给脚本用的日志接口（默认包 zap，但脚本侧只看到 Info / Warn / Error 三个方法）。
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// NewContext engine 调用，给脚本运行时构造。
//
// searcher 接受 *store.Searcher (生产) 或 store.FixtureSearcher (CLI/单测)。
func NewContext(ctx context.Context, searcher SearcherIface, logger Logger, params map[string]string) *Context {
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

// AppendLog 追加一条日志条目到本次运行的捕获缓冲. 由 engine 的 Print hook + ctx.log_* 调.
//
// 限制 buffer 上限 (1000 条) 防爆内存:超过即丢弃新的,确保单脚本死循环 print() 不会拖死进程.
func (c *Context) AppendLog(level, msg string, kv map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.logs) >= 1000 {
		return
	}
	c.logs = append(c.logs, LogEntry{Level: level, Msg: msg, KV: kv})
}

// GetLogs 拿走捕获的日志快照 (deep copy).
func (c *Context) GetLogs() []LogEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.logs) == 0 {
		return nil
	}
	out := make([]LogEntry, len(c.logs))
	copy(out, c.logs)
	return out
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
	ScriptID    string     `json:"script_id"`
	RunID       string     `json:"run_id"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  time.Time  `json:"finished_at"`
	Status      string     `json:"status"` // "success" / "error" / "timeout"
	Error       string     `json:"error,omitempty"`
	Diffs       []Diff     `json:"diffs"`
	Stats       Stats      `json:"stats"`
	Logs        []LogEntry `json:"logs,omitempty"`        // 脚本里 print() / ctx.log_* 的捕获
	TriggeredBy string     `json:"triggered_by"`          // "manual" / "cron" / "stream:<svc>:<table>"
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
