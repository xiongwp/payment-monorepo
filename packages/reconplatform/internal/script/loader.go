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
type Script struct {
	ID        string
	Name      string
	Code      string
	UpdatedAt time.Time
	UpdatedBy string
	Schedule  string   // cron 表达式（"*/5 * * * *"）；空 = 不走定时
	Triggers  []string // ["order-core:payment_intents", "*"]；空 = 不走事件驱动

	// compiled Starlark 编译后产物，Run 时复用。
	compiled *CompiledScript
}

// Loader 管理一组已加载的 Starlark 脚本。线程安全。
type Loader struct {
	mu      sync.RWMutex
	scripts map[string]*Script
	engine  *Engine
}

// NewLoader 构造 Loader。engine 默认是 NewEngine(0) — 用 maxSteps 默认值。
//
// 测试 / dev 可以传 nil，loader 会自己 NewEngine。
func NewLoader(e *Engine) *Loader {
	if e == nil {
		e = NewEngine(0)
	}
	return &Loader{
		scripts: make(map[string]*Script),
		engine:  e,
	}
}

// Engine 暴露给 caller（main.go）做动态 RegisterModule / 调 /symbols 端点。
func (l *Loader) Engine() *Engine { return l.engine }

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
