// Package script — Starlark 对账脚本引擎。
//
// 替代 yaegi（v0.x 已下线）。Starlark 是 Google 设计的 Python 子集 + 沙箱：
//
//   - 类型安全：编译期就拒绝 int + str
//   - 默认无 IO（无 file / net / os.Exec / unsafe），即使脚本作者乱写也搞不出资损
//   - 确定性：no random / no while-true（只 for-in 受控迭代）
//   - 性能：编译 IR 后纯解释，无 reflect call 开销，比 yaegi 快 5-10×
//
// 脚本入口约定：
//
//	def check(ctx):
//	    """返 list[dict]，每条 dict 是一条 diff (type/key/want/got/detail)"""
//	    diffs = []
//	    for pi in ctx.scan_index("pi_id", "", 5000):
//	        events = ctx.get_by_index("pi_id", pi)
//	        ...
//	        if mismatch:
//	            diffs.append({"type": "amount_mismatch", "key": pi, "want": ..., "got": ...})
//	    return diffs
//
// 引入新包：
//
//	load("@json", "encode", "decode")
//	load("@time", "now", "parse_time")
//	load("@strings", "split", "to_lower")
//
// 详见 starlark_modules.go 的 builtin module 列表。运行时可以通过
// engine.RegisterModule(name, dict) 动态新加 host 包不重启。

package script

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// Engine 单进程级 Starlark 引擎。线程安全：内部 modules 表用 RWMutex 保护，
// thread 实例每次 Run 新建（Starlark thread 不可复用 across calls）。
type Engine struct {
	mu      sync.RWMutex
	modules map[string]starlark.StringDict // name → exported symbols
	// maxSteps 单次 Run 最大执行步数（防 runaway 循环）。0 = 不限。
	maxSteps int64
}

// NewEngine 构造默认 Engine，自动加载 starlark stdlib (json / time / math / strings)。
//
// maxSteps: 单次 Check 调用允许的最大解释器步数；过大可能拖死整个 reconplatform。
// 默认 1e8 步（约相当于 ~1 秒纯计算）。
func NewEngine(maxSteps int64) *Engine {
	if maxSteps <= 0 {
		maxSteps = 100_000_000
	}
	e := &Engine{
		modules:  make(map[string]starlark.StringDict),
		maxSteps: maxSteps,
	}
	registerBuiltinModules(e)
	return e
}

// RegisterModule 运行时动态注册 host 包。脚本通过 load("@<name>", "func1", ...) 调用。
//
// 重复注册同名 module 直接覆盖（admin 修改 host 实现后热更新）。
// dict 的 key 是脚本侧可见的符号名；value 是 starlark.Value（典型 *starlark.Builtin
// / starlark.NewDict / starlark.Tuple 等）。
//
// **线程安全**：可以在脚本运行期间被并发调用；新加的 module 立刻对后续 Run 可见，
// 已在跑的 Run 不影响（thread.Load 是按需查询，下次 load() 取新值）。
func (e *Engine) RegisterModule(name string, dict starlark.StringDict) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.modules[name] = dict
}

// ModuleNames 列出所有已注册 module（给 /symbols 端点 / admin web 自动补齐用）。
func (e *Engine) ModuleNames() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.modules))
	for k := range e.modules {
		out = append(out, k)
	}
	return out
}

// ModuleSymbols 返回单个 module 暴露的所有顶层符号 + 类型。
// 给 /symbols 端点用，编辑器自动补齐 load("@<mod>", ...) 第二个参数。
func (e *Engine) ModuleSymbols(name string) []SymbolInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()
	dict, ok := e.modules[name]
	if !ok {
		return nil
	}
	out := make([]SymbolInfo, 0, len(dict))
	for k, v := range dict {
		out = append(out, SymbolInfo{
			Name: k,
			Type: v.Type(), // "builtin_function_or_method" / "string" / etc.
		})
	}
	return out
}

// loadModule 实现 starlark.Thread.Load 钩子。
// 脚本写 load("@json", "encode", ...) → name="@json"。
// 脚本写 load("helper.star", "f") → name="helper.star"（暂不支持脚本互引；
// 留作后续 helper.star 注册到 Loader 的 follow-up）。
func (e *Engine) loadModule(_ *starlark.Thread, module string) (starlark.StringDict, error) {
	if len(module) > 1 && module[0] == '@' {
		key := module[1:]
		e.mu.RLock()
		defer e.mu.RUnlock()
		if d, ok := e.modules[key]; ok {
			return d, nil
		}
		return nil, fmt.Errorf("unknown builtin module %q (registered: %v)", key, e.modulesNamesUnlocked())
	}
	return nil, fmt.Errorf("script-to-script load() not yet supported: %q", module)
}

// modulesNamesUnlocked 不加锁的版本（caller 已持有锁）。
func (e *Engine) modulesNamesUnlocked() []string {
	out := make([]string, 0, len(e.modules))
	for k := range e.modules {
		out = append(out, k)
	}
	return out
}

// Compile 解析 + frozen module（不执行 def 之外的逻辑），返回脚本对象供 Run 复用。
//
// errors:
//   - 语法错误（解析失败）
//   - 顶层执行错误（脚本里有不安全的顶层语句）
//   - 找不到 def check 函数 / 签名错
func (e *Engine) Compile(scriptID, code string) (*CompiledScript, error) {
	if code == "" {
		return nil, errors.New("empty script code")
	}
	thread := &starlark.Thread{
		Name: "compile:" + scriptID,
		Load: e.loadModule,
	}
	// FileOptions 可关一些 corner-case feature；用默认即可。
	opts := &syntax.FileOptions{}
	globals, err := starlark.ExecFileOptions(opts, thread, scriptID+".star", code, nil)
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	checkFn, ok := globals["check"]
	if !ok {
		return nil, errors.New("script must define a top-level 'check(ctx)' function")
	}
	if _, ok := checkFn.(starlark.Callable); !ok {
		return nil, fmt.Errorf("'check' is not callable: got %s", checkFn.Type())
	}
	// Freeze the globals so concurrent Run cannot mutate shared state.
	globals.Freeze()
	return &CompiledScript{
		ID:      scriptID,
		Code:    code,
		Globals: globals,
		Check:   checkFn.(starlark.Callable),
	}, nil
}

// Run 执行脚本 check(ctx)；返 diffs + error。
//
// 超时：通过 ctx.Done() + thread.SetMaxExecutionSteps 双重保护：
//   - I/O 调用（http_get / redis 索引）走 Go ctx，到时立刻返
//   - 纯计算 / 死循环走 maxSteps 上限，超了 Starlark 自己抛 ExecutionError
//
// 不可恢复 panic：用 recover 兜底，避免脚本 bug 把 reconplatform 整个进程拖死。
func (e *Engine) Run(ctx context.Context, cs *CompiledScript, sctx *Context) (diffs []Diff, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("script panic: %v", r)
		}
	}()
	if cs == nil || cs.Check == nil {
		return nil, errors.New("nil compiled script")
	}

	thread := &starlark.Thread{
		Name: "run:" + cs.ID,
		Load: e.loadModule,
		Print: func(_ *starlark.Thread, msg string) {
			// 脚本里的 print() 走两路:
			//  1) logger.Info → zap (生产容器能 grep)
			//  2) ctx.AppendLog → 本次 Run 的捕获缓冲, dry-run 接口会读出给编辑器 console 面板.
			if sctx == nil {
				return
			}
			if sctx.logger != nil {
				sctx.logger.Info("script.print", "script_id", cs.ID, "msg", msg)
			}
			sctx.AppendLog("print", msg, nil)
		},
	}
	// MaxExecutionSteps 限制纯计算上限。Starlark 每条语句 ~ 1-100 step。
	thread.SetMaxExecutionSteps(uint64(e.maxSteps))

	// 把 Go ctx 塞 thread.local，让 builtin (http_get / redis) 能拿到
	thread.SetLocal(threadLocalCtx, sctx)
	thread.SetLocal(threadLocalGoCtx, ctx)

	scv := wrapContext(sctx)
	startedAt := time.Now()
	out, callErr := starlark.Call(thread, cs.Check, starlark.Tuple{scv}, nil)
	elapsed := time.Since(startedAt)

	if sctx != nil {
		sctx.stats.ExecMillis = elapsed.Milliseconds()
	}
	if callErr != nil {
		return nil, fmt.Errorf("script.check error: %w", callErr)
	}

	// 期望 check 返 list[dict]
	parsed, perr := parseDiffsReturn(out)
	if perr != nil {
		return nil, fmt.Errorf("script.check return: %w", perr)
	}
	return parsed, nil
}

// CompiledScript 一次编译的产物，可重复 Run。
type CompiledScript struct {
	ID      string
	Code    string
	Globals starlark.StringDict
	Check   starlark.Callable
}

// SymbolInfo 给 /api/v1/script/symbols 端点用，前端 Monaco 自动补齐消费。
type SymbolInfo struct {
	Name string `json:"name"`
	Type string `json:"type"` // "builtin_function_or_method" / "function" / "string" / ...
}

// thread-local 键
const (
	threadLocalCtx   = "recon.ctx"
	threadLocalGoCtx = "recon.goctx"
)

// parseDiffsReturn 把 starlark return 值转成 []Diff。
//
// 期待结构 (def check(ctx) -> list[dict]):
//
//	[{"type": "...", "key": "...", "want": ..., "got": ..., "detail": ...}, ...]
//
// None / 空 list 视为空 diff。
func parseDiffsReturn(v starlark.Value) ([]Diff, error) {
	if v == nil || v == starlark.None {
		return nil, nil
	}
	list, ok := v.(*starlark.List)
	if !ok {
		return nil, fmt.Errorf("expected list, got %s", v.Type())
	}
	out := make([]Diff, 0, list.Len())
	iter := list.Iterate()
	defer iter.Done()
	var item starlark.Value
	for iter.Next(&item) {
		d, err := parseDiffDict(item)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func parseDiffDict(v starlark.Value) (Diff, error) {
	dict, ok := v.(*starlark.Dict)
	if !ok {
		return Diff{}, fmt.Errorf("diff must be dict, got %s", v.Type())
	}
	d := Diff{}
	if t, ok, _ := dict.Get(starlark.String("type")); ok {
		d.Type = string(t.(starlark.String))
	}
	if k, ok, _ := dict.Get(starlark.String("key")); ok {
		d.Key = string(k.(starlark.String))
	}
	if w, ok, _ := dict.Get(starlark.String("want")); ok {
		d.Want = starlarkToGo(w)
	}
	if g, ok, _ := dict.Get(starlark.String("got")); ok {
		d.Got = starlarkToGo(g)
	}
	if det, ok, _ := dict.Get(starlark.String("detail")); ok {
		d.Detail = starlarkToGo(det)
	}
	return d, nil
}

// starlarkToGo 递归把 starlark 值转成 Go any（diff.want/got/detail 用）。
func starlarkToGo(v starlark.Value) any {
	switch x := v.(type) {
	case starlark.String:
		return string(x)
	case starlark.Int:
		if i64, ok := x.Int64(); ok {
			return i64
		}
		return x.String()
	case starlark.Float:
		return float64(x)
	case starlark.Bool:
		return bool(x)
	case starlark.NoneType:
		return nil
	case *starlark.List:
		out := make([]any, 0, x.Len())
		iter := x.Iterate()
		defer iter.Done()
		var item starlark.Value
		for iter.Next(&item) {
			out = append(out, starlarkToGo(item))
		}
		return out
	case *starlark.Dict:
		out := make(map[string]any, x.Len())
		for _, k := range x.Keys() {
			val, _, _ := x.Get(k)
			ks, _ := k.(starlark.String)
			out[string(ks)] = starlarkToGo(val)
		}
		return out
	default:
		return v.String()
	}
}

// goToStarlark 反向：Go any → starlark value，给 ctx.params / event 字段返脚本用。
// nil → None；map[string]string → dict；string/int/bool → 对应 starlark.Value。
// 复杂结构按需扩展。
func goToStarlark(v any) starlark.Value {
	switch x := v.(type) {
	case nil:
		return starlark.None
	case string:
		return starlark.String(x)
	case int:
		return starlark.MakeInt(x)
	case int64:
		return starlark.MakeInt64(x)
	case float64:
		return starlark.Float(x)
	case bool:
		return starlark.Bool(x)
	case map[string]string:
		d := starlark.NewDict(len(x))
		for k, val := range x {
			_ = d.SetKey(starlark.String(k), starlark.String(val))
		}
		return d
	case map[string]any:
		d := starlark.NewDict(len(x))
		for k, val := range x {
			_ = d.SetKey(starlark.String(k), goToStarlark(val))
		}
		return d
	case []string:
		l := starlark.NewList(make([]starlark.Value, 0, len(x)))
		for _, s := range x {
			_ = l.Append(starlark.String(s))
		}
		return l
	default:
		return starlark.String(fmt.Sprintf("%v", v))
	}
}
