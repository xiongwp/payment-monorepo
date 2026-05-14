// loader.go — Starlark 脚本加载器。
//
// 替代之前的 Yaegi 版（v0.x 已下线，见 commit history）。
//
// 脚本契约：
//
//	def check(ctx):
//	    """返 list[dict]，每条 dict 是一条 diff (type/key/want/got/detail)"""
//	    diffs = []
//	    ...
//	    return diffs
//
// 引入新包：load("@json", "encode") / load("@time", "now") / 等
//
// 热更新：admin web 保存 → loader.Replace(id, source) → engine.Compile 立即生效
package script

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Script 已加载的单条脚本：源码 + Starlark 编译产物。
//
// JSON tag 保证 detail 端点 (/api/v1/scripts/:id) 返回小写字段名,
// 与 list 端点 (/api/v1/scripts) 手工 map 出的字段名一致, 前端写一套即可.
type Script struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Code      string    `json:"code"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
	Schedule  string    `json:"schedule"`   // cron 表达式（"*/5 * * * *"）；空 = 不走定时
	Triggers  []string  `json:"triggers"`   // ["order-core:payment_intents", "*"]；空 = 不走事件驱动

	// compiled Starlark 编译后产物，Run 时复用。
	compiled *CompiledScript `json:"-"`
}

// PostRunHook 脚本运行结束后的回调（main.go 注 notifier + diffstate.CreateOpen）。
//
// 不让 script 包直接 import notifier / diffstate（会反向依赖），用 interface
// 让 caller 注入。Hook 失败不阻塞 Run 主流程，仅 log。
type PostRunHook func(ctx *Context, result *Result)

// Loader 管理一组已加载的 Starlark 脚本。线程安全。
type Loader struct {
	mu        sync.RWMutex
	scripts   map[string]*Script
	engine    *Engine
	postHooks []PostRunHook

	// compileCache LRU cache for ad-hoc Compile (RunCode / Validate).
	// 已注册脚本的 compiled 字段仍用 Script.compiled 持有(永驻);
	// cache 只服务 dry-run / batch 等"一次性 code 但可能重复"的路径,
	// 命中典型节省 5-50ms/次.
	compileCache *CompileCache
}

// NewLoader 构造 Loader。engine 默认是 NewEngine(0) — 用 maxSteps 默认值。
//
// 测试 / dev 可以传 nil，loader 会自己 NewEngine。
func NewLoader(e *Engine) *Loader {
	if e == nil {
		e = NewEngine(0)
	}
	return &Loader{
		scripts:      make(map[string]*Script),
		engine:       e,
		compileCache: NewCompileCache(128),
	}
}

// CompileCache 暴露给 caller 抓 metrics (Prometheus).
func (l *Loader) CompileCache() *CompileCache { return l.compileCache }

// Engine 暴露给 caller（main.go）做动态 RegisterModule / 调 /symbols 端点。
func (l *Loader) Engine() *Engine { return l.engine }

// AddPostRunHook 注册脚本运行结束后的回调。多次调可叠加多个 hook。
//
// 典型 caller (main.go)：
//
//	loader.AddPostRunHook(func(ctx *script.Context, r *script.Result) {
//	    // 1) diffs 落 diffstate（每条 diff 用 IDFor 算稳定 ID）
//	    for i, d := range r.Diffs { diffstateStore.CreateOpen(...) }
//	    // 2) 通过 dispatcher 分发告警（含 dedup 抑制）
//	    notifier.Dispatch(ctx.Ctx, &notifier.RunResult{...})
//	})
func (l *Loader) AddPostRunHook(h PostRunHook) {
	l.mu.Lock()
	l.postHooks = append(l.postHooks, h)
	l.mu.Unlock()
}

// Add 加载新脚本（id 不能已存在）。
func (l *Loader) Add(id string, s *Script) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.scripts[id]; exists {
		return fmt.Errorf("script %q already exists", id)
	}
	if err := l.compile(s); err != nil {
		return fmt.Errorf("compile %q: %w", id, err)
	}
	s.ID = id
	l.scripts[id] = s
	return nil
}

// Replace 热更新（id 必须已存在）。
func (l *Loader) Replace(id string, s *Script) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.scripts[id]; !exists {
		return fmt.Errorf("script %q not found", id)
	}
	if err := l.compile(s); err != nil {
		return fmt.Errorf("recompile %q: %w", id, err)
	}
	s.ID = id
	l.scripts[id] = s
	return nil
}

// Upsert 不存在就 Add，存在就 Replace。loader-from-Redis 启动场景常用。
func (l *Loader) Upsert(id string, s *Script) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.compile(s); err != nil {
		return fmt.Errorf("compile %q: %w", id, err)
	}
	s.ID = id
	l.scripts[id] = s
	return nil
}

// Validate 仅做语法 + check 函数签名检查，不真注册。
//
// admin web 编辑器右上角"语法检查"按钮调本接口；运营写到一半保存草稿
// 也用这个验。返 nil 表示 OK，否则 err.Error() 是给运营看的可读错误。
//
// 不含 lint warnings。需要 lint 走 ValidateWithLint。
func (l *Loader) Validate(code string) error {
	if code == "" {
		return errors.New("empty script code")
	}
	_, err := l.engine.Compile("validate", code)
	if err != nil {
		return err
	}
	return nil
}

// ValidateWithLint 编译 + Lint，返 (compileErr, lintIssues)。
//
// 设计：
//
//	(1) 编译错（语法 / 缺 check / 签名错）→ 返 err，issues 仍尽量给（lint 用 parse 后的 AST）
//	(2) 编译 OK → 返 nil err + lint issues（包含 error/warn/info 三档）
//	(3) lint error 不阻塞 caller —— 由 caller 决定是否拒绝保存（生产 admin 拒
//	    error 通过，warn / info 仅展示）
//
// 给 admin web 编辑器侧栏 marker 用：把 issues 转 monaco.editor.IMarkerData
// 在编辑器左侧显示红/黄/蓝小条。
func (l *Loader) ValidateWithLint(code string) (error, []LintIssue) {
	if code == "" {
		return errors.New("empty script code"), nil
	}
	// 先 lint：即使编译失败 lint 也尽量跑（parse 即可，不需要 compile 全部 OK）
	issues, _ := Lint("validate", code)
	// 然后 compile
	_, err := l.engine.Compile("validate", code)
	return err, issues
}

// Remove 卸载。
func (l *Loader) Remove(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.scripts, id)
}

// Get 拿一条脚本（只读 view）。
func (l *Loader) Get(id string) (*Script, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, ok := l.scripts[id]
	return s, ok
}

// List 列出所有已加载脚本。
func (l *Loader) List() []*Script {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*Script, 0, len(l.scripts))
	for _, s := range l.scripts {
		out = append(out, s)
	}
	return out
}

// Run 同步执行一条脚本，返完整 Result。
//
// trigger 字段记录到 Result.TriggeredBy，便于审计："manual" / "cron" /
// "stream:order-core:payment_intents".
func (l *Loader) Run(ctx *Context, scriptID, trigger string) *Result {
	l.mu.RLock()
	s, ok := l.scripts[scriptID]
	l.mu.RUnlock()

	r := &Result{
		ScriptID:    scriptID,
		RunID:       fmt.Sprintf("%d", time.Now().UnixNano()),
		StartedAt:   time.Now(),
		TriggeredBy: trigger,
	}
	if !ok {
		r.Status = "error"
		r.Error = "script not loaded"
		r.FinishedAt = time.Now()
		return r
	}
	if s.compiled == nil {
		r.Status = "error"
		r.Error = "script not compiled"
		r.FinishedAt = time.Now()
		return r
	}

	goCtx := ctx.Ctx
	if goCtx == nil {
		goCtx = context.Background()
	}
	diffs, err := l.engine.Run(goCtx, s.compiled, ctx)
	r.FinishedAt = time.Now()
	r.Stats = ctx.GetStats()
	// 收集脚本运行期通过 ctx.AddDiff 写入的 diff（兼容老风格）+ return 的 diffs
	r.Diffs = append(r.Diffs, ctx.Diffs()...)
	r.Diffs = append(r.Diffs, diffs...)
	if err != nil {
		r.Status = "error"
		r.Error = err.Error()
	} else {
		r.Status = "success"
	}
	// post-run hooks (notifier dispatch / diffstate.CreateOpen 等)
	// 任何 hook panic 都不能拖死 Run；defer recover 兜底。
	l.runPostHooks(ctx, r)
	return r
}

// runPostHooks 顺序调所有注册的 hook；panic 各自隔离不影响其他。
func (l *Loader) runPostHooks(ctx *Context, r *Result) {
	l.mu.RLock()
	hooks := make([]PostRunHook, len(l.postHooks))
	copy(hooks, l.postHooks)
	l.mu.RUnlock()
	for _, h := range hooks {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					if ctx != nil && ctx.logger != nil {
						ctx.logger.Warn("post-run hook panic",
							"script_id", r.ScriptID, "recover", fmt.Sprintf("%v", rec))
					}
				}
			}()
			h(ctx, r)
		}()
	}
}

// RunCode 临时编译 + 执行任意脚本代码，不写 loader / scripts 表 / 历史结果。
//
// 用途：admin web 编辑器 "Dry Run" 按钮 — 让运营在保存之前先试跑当前
// 编辑器里的代码，看看 diff 结果是否符合预期。
//
//	显示名 (用于日志 / Result.ScriptID)：传 _scratch / _dryrun 之类
//	失败：编译错 / 运行错都包成 Result 返回，不 panic
//
// 与 Run 的差异：
//   - 不读 l.scripts 缓存（直接用入参 code 编译）
//   - 编译产物 throw-away（运行完即丢，不污染内存）
//   - caller 决定是否调 SaveResult；本方法不写 Redis
func (l *Loader) RunCode(ctx *Context, displayID, code, trigger string) *Result {
	r := &Result{
		ScriptID:    displayID,
		RunID:       fmt.Sprintf("dryrun-%d", time.Now().UnixNano()),
		StartedAt:   time.Now(),
		TriggeredBy: trigger,
	}
	// 优先查 cache: hit 命中省 5-50ms (Starlark Compile 是热路径瓶颈).
	var cs *CompiledScript
	if cached, ok := l.compileCache.Get(displayID, code); ok {
		cs = cached
	} else {
		var err error
		cs, err = l.engine.Compile(displayID, code)
		if err != nil {
			r.Status = "error"
			r.Error = "compile: " + err.Error()
			r.FinishedAt = time.Now()
			return r
		}
		l.compileCache.Put(displayID, code, cs)
	}
	goCtx := ctx.Ctx
	if goCtx == nil {
		goCtx = context.Background()
	}
	diffs, runErr := l.engine.Run(goCtx, cs, ctx)
	r.FinishedAt = time.Now()
	r.Stats = ctx.GetStats()
	r.Diffs = append(r.Diffs, ctx.Diffs()...)
	r.Diffs = append(r.Diffs, diffs...)
	// 把脚本里 print() / ctx.log_* 的捕获日志带回, dry-run 端点会显示给编辑器 Console 面板.
	// 即使为 nil 也兜底为 [] (JSON 字段不缺失,前端 r.logs.length 不报错).
	r.Logs = ctx.GetLogs()
	if r.Logs == nil {
		r.Logs = []LogEntry{}
	}
	if runErr != nil {
		r.Status = "error"
		r.Error = runErr.Error()
		return r
	}
	r.Status = "success"
	return r
}

// compile 编译脚本，存到 s.compiled。
//
// 调用方必须持有 l.mu.Lock（Add / Replace / Upsert 已经拿了锁）。
func (l *Loader) compile(s *Script) error {
	if s == nil || s.Code == "" {
		return errors.New("loader: empty script code")
	}
	cs, err := l.engine.Compile(s.ID, s.Code)
	if err != nil {
		return err
	}
	s.compiled = cs
	return nil
}
