// starlark_api.go — 把 recon.Context / EventList / Event 包成 starlark.Value，
// 让脚本里 ctx.scan_index("pi_id", "", 5000) 这种链式调用可读。
//
// 设计思路：
//   - 每种 Go 对象写一个 starlark wrapper（实现 starlark.Value + HasAttrs）
//   - 字段访问（ctx.now / event.service）走 .Attr(name)
//   - 方法调用（ctx.scan_index(...)）也走 .Attr → 返一个 *Builtin 让脚本 call
//   - 安全：所有 wrapper 都是 frozen（脚本不能 mutate Go 数据结构）

package script

import (
	"context"
	"fmt"
	"strings"

	"go.starlark.net/starlark"
	"reconcile-system/internal/store"
)

// ─── Context wrapper ─────────────────────────────────────────────────

type starlarkContext struct {
	inner *Context
}

// wrapContext 创建一个 starlark.Value 包裹 *Context。
func wrapContext(c *Context) *starlarkContext { return &starlarkContext{inner: c} }

func (c *starlarkContext) String() string        { return "<recon.Context>" }
func (c *starlarkContext) Type() string          { return "recon.Context" }
func (c *starlarkContext) Freeze()               {}
func (c *starlarkContext) Truth() starlark.Bool  { return starlark.Bool(c.inner != nil) }
func (c *starlarkContext) Hash() (uint32, error) { return 0, fmt.Errorf("recon.Context unhashable") }

// AttrNames 给 reflect / dir() 用，决定哪些属性能补齐。
// **autocomplete 关键**：editor 通过 /symbols 端点拿这个清单。
func (c *starlarkContext) AttrNames() []string {
	return []string{
		"now", "params",
		"scan_index", "get_by_index", "get",
		"http_get",
		"log_info", "log_warn", "log_error",
	}
}

func (c *starlarkContext) Attr(name string) (starlark.Value, error) {
	if c.inner == nil {
		return nil, fmt.Errorf("nil ctx")
	}
	switch name {
	case "now":
		return starlark.String(c.inner.Now.Format("2006-01-02T15:04:05Z07:00")), nil
	case "params":
		return goToStarlark(c.inner.Params), nil
	case "scan_index":
		return starlark.NewBuiltin("ctx.scan_index", c.scanIndex), nil
	case "get_by_index":
		return starlark.NewBuiltin("ctx.get_by_index", c.getByIndex), nil
	case "get":
		return starlark.NewBuiltin("ctx.get", c.get), nil
	case "http_get":
		return starlark.NewBuiltin("ctx.http_get", c.httpGet), nil
	case "log_info":
		return starlark.NewBuiltin("ctx.log_info", c.logBuiltin("info")), nil
	case "log_warn":
		return starlark.NewBuiltin("ctx.log_warn", c.logBuiltin("warn")), nil
	case "log_error":
		return starlark.NewBuiltin("ctx.log_error", c.logBuiltin("error")), nil
	}
	return nil, nil // attr not found：starlark 会抛 AttributeError
}

func (c *starlarkContext) scanIndex(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var idxName, prefix string
	limit := 1000
	if err := starlark.UnpackArgs("scan_index", args, nil, "idx_name", &idxName, "prefix?", &prefix, "limit?", &limit); err != nil {
		return nil, err
	}
	keys := c.inner.ScanIndex(idxName, prefix, limit)
	out := starlark.NewList(make([]starlark.Value, 0, len(keys)))
	for _, k := range keys {
		_ = out.Append(starlark.String(k))
	}
	return out, nil
}

func (c *starlarkContext) getByIndex(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var idxName, value string
	if err := starlark.UnpackArgs("get_by_index", args, nil, "idx_name", &idxName, "value", &value); err != nil {
		return nil, err
	}
	events := c.inner.GetByIndex(idxName, value)
	return wrapEventList(events), nil
}

func (c *starlarkContext) get(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var service, table, pk string
	if err := starlark.UnpackArgs("get", args, nil, "service", &service, "table", &table, "pk", &pk); err != nil {
		return nil, err
	}
	e := c.inner.Get(service, table, pk)
	if e == nil {
		return starlark.None, nil
	}
	return wrapEvent(e), nil
}

// httpGet 与 modules 里的 recon.http_get 相同语义；放 ctx 上是因为业务作者更习惯
// ctx.http_get(...) 而不是 load("@recon", "http_get") 然后 http_get(...)。
func (c *starlarkContext) httpGet(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	return reconHTTPGet(thread, nil, args, nil)
}

func (c *starlarkContext) logBuiltin(level string) func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
	return func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var msg string
		if err := starlark.UnpackArgs("log", args[:1], nil, "msg", &msg); err != nil {
			return nil, err
		}
		// kwargs 转 key-value pairs
		kv := make([]any, 0, len(kwargs)*2)
		for _, kw := range kwargs {
			ks, _ := kw[0].(starlark.String)
			kv = append(kv, string(ks), starlarkToGo(kw[1]))
		}
		// args[1:] 当 positional kv (pair-wise)
		extras := args[1:]
		for i := 0; i+1 < len(extras); i += 2 {
			ks, _ := extras[i].(starlark.String)
			kv = append(kv, string(ks), starlarkToGo(extras[i+1]))
		}
		switch level {
		case "info":
			c.inner.logger.Info(msg, kv...)
		case "warn":
			c.inner.logger.Warn(msg, kv...)
		case "error":
			c.inner.logger.Error(msg, kv...)
		}
		return starlark.None, nil
	}
}

// ─── EventList wrapper ────────────────────────────────────────────────

type starlarkEventList struct {
	inner store.EventList
}

func wrapEventList(es store.EventList) *starlarkEventList { return &starlarkEventList{inner: es} }

func (l *starlarkEventList) String() string {
	return fmt.Sprintf("<EventList len=%d>", len(l.inner))
}
func (l *starlarkEventList) Type() string          { return "recon.EventList" }
func (l *starlarkEventList) Freeze()               {}
func (l *starlarkEventList) Truth() starlark.Bool  { return starlark.Bool(len(l.inner) > 0) }
func (l *starlarkEventList) Hash() (uint32, error) { return 0, fmt.Errorf("EventList unhashable") }
func (l *starlarkEventList) Len() int              { return len(l.inner) }

func (l *starlarkEventList) AttrNames() []string {
	return []string{"find", "find_all", "len", "__iter__"}
}

func (l *starlarkEventList) Attr(name string) (starlark.Value, error) {
	switch name {
	case "find":
		return starlark.NewBuiltin("EventList.find", l.find), nil
	case "find_all":
		return starlark.NewBuiltin("EventList.find_all", l.findAll), nil
	case "len":
		return starlark.MakeInt(len(l.inner)), nil
	}
	return nil, nil
}

func (l *starlarkEventList) find(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var service, table string
	if err := starlark.UnpackArgs("find", args, nil, "service", &service, "table", &table); err != nil {
		return nil, err
	}
	e := l.inner.Find(service, table)
	if e == nil {
		return starlark.None, nil
	}
	return wrapEvent(e), nil
}

func (l *starlarkEventList) findAll(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var service, table string
	if err := starlark.UnpackArgs("find_all", args, nil, "service", &service, "table", &table); err != nil {
		return nil, err
	}
	matches := l.inner.FindAll(service, table)
	return wrapEventList(matches), nil
}

// EventList 也要可迭代（for e in events:）
func (l *starlarkEventList) Iterate() starlark.Iterator {
	return &eventListIter{inner: l.inner, idx: 0}
}

type eventListIter struct {
	inner store.EventList
	idx   int
}

func (it *eventListIter) Next(p *starlark.Value) bool {
	if it.idx >= len(it.inner) {
		return false
	}
	*p = wrapEvent(it.inner[it.idx])
	it.idx++
	return true
}

func (it *eventListIter) Done() {}

// ─── Event wrapper ────────────────────────────────────────────────────

type starlarkEvent struct {
	inner *store.Event
}

func wrapEvent(e *store.Event) *starlarkEvent { return &starlarkEvent{inner: e} }

func (e *starlarkEvent) String() string {
	if e.inner == nil {
		return "<Event nil>"
	}
	return fmt.Sprintf("<Event %s.%s pk=%s>", e.inner.Service, e.inner.Table, e.inner.PK)
}
func (e *starlarkEvent) Type() string         { return "recon.Event" }
func (e *starlarkEvent) Freeze()              {}
func (e *starlarkEvent) Truth() starlark.Bool { return starlark.Bool(e.inner != nil && !e.inner.IsZero()) }
func (e *starlarkEvent) Hash() (uint32, error) {
	if e.inner == nil {
		return 0, nil
	}
	// 简单 hash：service/table/pk 拼接
	return uint32(len(e.inner.Service) + len(e.inner.Table)*7 + len(e.inner.PK)*31), nil
}

func (e *starlarkEvent) AttrNames() []string {
	return []string{"service", "table", "pk", "timestamp", "int", "str", "get", "data"}
}

func (e *starlarkEvent) Attr(name string) (starlark.Value, error) {
	if e.inner == nil {
		return starlark.None, nil
	}
	switch name {
	case "service":
		return starlark.String(e.inner.Service), nil
	case "table":
		return starlark.String(e.inner.Table), nil
	case "pk":
		return starlark.String(e.inner.PK), nil
	case "timestamp":
		return starlark.String(e.inner.Timestamp.Format("2006-01-02T15:04:05Z07:00")), nil
	case "int":
		return starlark.NewBuiltin("Event.int", e.intCol), nil
	case "str":
		return starlark.NewBuiltin("Event.str", e.strCol), nil
	case "get":
		return starlark.NewBuiltin("Event.get", e.getCol), nil
	case "data":
		// 把 raw 列字典原样返
		d := starlark.NewDict(len(e.inner.Cols))
		for k, v := range e.inner.Cols {
			_ = d.SetKey(starlark.String(k), starlark.String(v))
		}
		return d, nil
	}
	return nil, nil
}

func (e *starlarkEvent) intCol(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var col string
	if err := starlark.UnpackArgs("int", args, nil, "col", &col); err != nil {
		return nil, err
	}
	return starlark.MakeInt64(e.inner.Int(col)), nil
}

func (e *starlarkEvent) strCol(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var col string
	if err := starlark.UnpackArgs("str", args, nil, "col", &col); err != nil {
		return nil, err
	}
	return starlark.String(e.inner.Str(col)), nil
}

func (e *starlarkEvent) getCol(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var col string
	def := ""
	if err := starlark.UnpackArgs("get", args, nil, "col", &col, "default?", &def); err != nil {
		return nil, err
	}
	if v, ok := e.inner.Cols[col]; ok {
		return starlark.String(v), nil
	}
	return starlark.String(def), nil
}

// ─── ContextKeys 给 /symbols 端点用 ───────────────────────────────────

// CtxAttrNames 暴露 context 上的可补齐属性，给 admin web autocomplete provider 调。
func CtxAttrNames() []string {
	return (&starlarkContext{}).AttrNames()
}

// EventAttrNames Event 对象上的可补齐属性。
func EventAttrNames() []string {
	return (&starlarkEvent{}).AttrNames()
}

// EventListAttrNames EventList 对象上的可补齐属性。
func EventListAttrNames() []string {
	return (&starlarkEventList{}).AttrNames()
}

// _ 强制保留 store 包 import 不被 IDE 误移除（Cols / Service 用）
var _ = strings.HasPrefix

// _ 同样保留 context 包的逻辑显式可见
var _ context.Context
