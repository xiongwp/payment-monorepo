// Loader 用 Yaegi 解释器动态加载 Go 对账脚本。
//
// 为啥不用 plugin / .so？
//   - plugin 包在 macOS dev 上几乎不可用；交叉编译 + go module hash 不一致 → import not found
//   - 不能热卸载（plugin 加载后无法 unload）
//   - 安全 surface 太大（脚本可以做任何事）
//
// Yaegi 是 traefik 写的 Go interpreter：
//   - 同一进程内解释执行 Go 源码，无 cgo / 无外部依赖
//   - 可白名单 import（脚本只能用我们暴露的 recon 包 + 部分标准库）
//   - 真热更新：admin web 保存 → loader.Replace(id, source) → 立即生效
//
// 脚本契约：
//
//	package main
//	import "recon"
//	func Check(ctx *recon.Context) (*recon.Result, error) { ... }
//
// loader 找 main.Check 函数，调用一次拿 *Result。
package script

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"
)

// Script 已加载的单条脚本：源码 + 编译后的 entry function（reflect.Value）。
type Script struct {
	ID         string
	Name       string
	Code       string
	UpdatedAt  time.Time
	UpdatedBy  string
	Schedule   string   // cron 表达式（"*/5 * * * *"）；空 = 不走定时
	Triggers   []string // ["order-core:payment_intents", "*"]；空 = 不走事件驱动

	// reflectFn 解析后保存的入口函数；invoke 时直接 Call。
	// 真实类型是 reflect.Value (kind=Func)，调用签名：
	//   func(ctx *Context) (*Result, error)
	reflectFn reflect.Value
}

// Loader 管理一组已加载的脚本。线程安全。
type Loader struct {
	mu      sync.RWMutex
	scripts map[string]*Script

	// yaegiNew 真实是 yaegi.Interp 工厂；测试 / 骨架阶段用 nil 跑过编译。
	// 第一阶段：Yaegi 包还没引入，loader 用 stub fn（直接 panic）让接口先稳。
	yaegiNew func() interpreter
}

// NewLoader 构造空 loader，CRUD 由 Add / Replace / Remove 控制。
func NewLoader() *Loader {
	return &Loader{
		scripts: make(map[string]*Script),
		yaegiNew: func() interpreter {
			// 真接 yaegi 时换成 yaegi.New(yaegi.Options{...})
			return &stubInterpreter{}
		},
	}
}

// Add 加载一条新脚本（id 不能已存在）。
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

// Validate 只做语法 + 入口函数签名检查，不真注册。
//
// admin web 编辑器右上角"语法检查"按钮调本接口；运营写到一半保存草稿
// 也用这个验。返 nil 表示 OK，否则 err.Error() 是给运营看的可读错误。
//
// Yaegi 真接进来后这一步等价于 interp.Eval(src) + 找 main.Check 函数 +
// 验签名 func(*Context) (*Result, error)。
func (l *Loader) Validate(code string) error {
	if l.yaegiNew == nil {
		return errors.New("loader: interpreter factory not set")
	}
	if code == "" {
		return errors.New("empty script code")
	}
	interp := l.yaegiNew()
	if err := interp.Eval(code); err != nil {
		return fmt.Errorf("syntax error: %w", err)
	}
	v, err := interp.Get("main.Check")
	if err != nil {
		return fmt.Errorf("entry function 'main.Check' not found: %w", err)
	}
	if !v.IsValid() || v.Kind() != reflect.Func {
		return errors.New("'main.Check' must be a function")
	}
	t := v.Type()
	// 期望签名：func(*Context) (*Result, error)
	if t.NumIn() != 1 {
		return fmt.Errorf("main.Check must take 1 argument (*recon.Context), got %d", t.NumIn())
	}
	if t.NumOut() != 2 {
		return fmt.Errorf("main.Check must return 2 values (*recon.Result, error), got %d", t.NumOut())
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

// List 列出所有已加载脚本（按 id 排序由 caller 决定）。
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

	// 调用脚本入口：func(ctx *Context) (*Result, error)
	res, err := callCheck(s.reflectFn, ctx)
	r.FinishedAt = time.Now()
	r.Diffs = ctx.Diffs()
	r.Stats = ctx.GetStats()
	if err != nil {
		r.Status = "error"
		r.Error = err.Error()
		return r
	}
	r.Status = "success"
	if res != nil {
		// 脚本可能也直接构造一份 Result 返回；合并 diff（脚本通过 ctx.AddDiff 添加的已经在 r.Diffs）
		if len(res.Diffs) > 0 {
			r.Diffs = append(r.Diffs, res.Diffs...)
		}
	}
	return r
}

// ─── 内部：编译 / 调用 ────────────────────────────────────────

// compile 解析脚本源码，定位 main.Check 函数，存到 s.reflectFn。
//
// 真接 Yaegi 时：
//
//	interp := yaegi.New(yaegi.Options{})
//	interp.Use(stdlib.Symbols)             // 标准库白名单
//	interp.Use(reconSymbols)               // 注入 recon 包（store / Diff / Context 等）
//	_, err := interp.Eval(s.Code)
//	v, err := interp.Eval("main.Check")
//	s.reflectFn = v
//
// 当前为骨架：直接拿一个不会被调用的 stub。Run 时会因 fn.IsValid()=false 走错误分支。
func (l *Loader) compile(s *Script) error {
	if l.yaegiNew == nil {
		return errors.New("loader: yaegi factory not set")
	}
	if s == nil || s.Code == "" {
		return errors.New("loader: empty script code")
	}
	interp := l.yaegiNew()
	if err := interp.Eval(s.Code); err != nil {
		return fmt.Errorf("eval source: %w", err)
	}
	v, err := interp.Get("main.Check")
	if err != nil {
		return fmt.Errorf("locate main.Check: %w", err)
	}
	if !v.IsValid() || v.Kind() != reflect.Func {
		return fmt.Errorf("main.Check is not a function")
	}
	s.reflectFn = v
	return nil
}

// callCheck 反射调用 main.Check(ctx) (*Result, error)。
func callCheck(fn reflect.Value, ctx *Context) (*Result, error) {
	if !fn.IsValid() {
		return nil, errors.New("script not compiled")
	}
	out := fn.Call([]reflect.Value{reflect.ValueOf(ctx)})
	if len(out) != 2 {
		return nil, fmt.Errorf("main.Check must return (*Result, error), got %d return values", len(out))
	}
	var res *Result
	if !out[0].IsNil() {
		r, ok := out[0].Interface().(*Result)
		if !ok {
			return nil, fmt.Errorf("main.Check first return is not *Result")
		}
		res = r
	}
	var err error
	if !out[1].IsNil() {
		e, ok := out[1].Interface().(error)
		if !ok {
			return nil, fmt.Errorf("main.Check second return is not error")
		}
		err = e
	}
	return res, err
}

// ─── interpreter abstraction（方便测试 + 延迟引入 yaegi 包）──

type interpreter interface {
	// Eval 解析并执行一段 Go 源码（package main 起头）。
	Eval(src string) error
	// Get 按 "pkg.Name" 拿到一个符号（函数 / 变量）的 reflect.Value。
	Get(qualifiedName string) (reflect.Value, error)
}

// stubInterpreter 占位（让骨架 build 通过）。一旦 yaegi 包引入就换掉。
//
// 真接 yaegi 后：
//
//	type yaegiInterp struct{ i *interp.Interpreter }
//	func (y *yaegiInterp) Eval(src string) error      { _, e := y.i.Eval(src); return e }
//	func (y *yaegiInterp) Get(name string) (reflect.Value, error) { return y.i.Eval(name) }
type stubInterpreter struct{}

func (stubInterpreter) Eval(src string) error {
	return errors.New("stubInterpreter: yaegi not yet wired (loader is skeleton)")
}

func (stubInterpreter) Get(name string) (reflect.Value, error) {
	return reflect.Value{}, errors.New("stubInterpreter: yaegi not yet wired")
}

// 让 context 包的 ctx 类型导入不被 IDE 标"未使用"——本文件不直接用，
// 但 Run 接收的 *Context 来自 api.go 同 package，所以无需额外 import。
var _ = context.Background
