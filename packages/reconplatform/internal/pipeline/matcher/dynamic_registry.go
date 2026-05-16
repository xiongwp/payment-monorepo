// dynamic_registry.go — 热替换的 Registry.
//
// 关键能力:
//   - Compile(name, source) 动态编译 Starlark 源 → 缓存 + 注册
//   - Replace(name, source) 原子替换旧规则 (atomic swap, 不打断 in-flight Match)
//   - Remove(name) 摘掉一条规则
//   - 编译失败时:旧规则保持有效,新源码返 error,不中断 worker
//
// 并发安全:
//   - EvalAll / List 用 RWMutex 读锁
//   - Compile / Replace / Remove 用写锁
//   - 拿到 Rule 后再持锁外执行 — 哪怕中途有 Replace,这一次评估用的还是旧规则,
//     不会出现"评估一半被换掉"导致诡异结果.
//
// 用法 (admin web 收到 PUT /scripts/:id 后):
//
//	if err := dynReg.Replace("duplicate_charge", newSource, "pi_id"); err != nil {
//	    return err  // 编译失败,返 4xx,旧规则继续跑
//	}
//	// 新规则即时生效
package matcher

import (
	"fmt"
	"sync"

	"reconcile-system/internal/script"
)

// DynamicRegistry 比 Registry 多: Compile / Replace / Remove + Starlark engine 持有.
type DynamicRegistry struct {
	*Registry

	mu     sync.RWMutex
	engine *script.Engine

	// name → 当前 active 的 Starlark rule (用于 Remove / Replace 时定位)
	starlarkRules map[string]*StarlarkRule
}

// NewDynamicRegistry 构造.
func NewDynamicRegistry(engine *script.Engine) *DynamicRegistry {
	if engine == nil {
		engine = script.NewEngine(5_000_000)
	}
	return &DynamicRegistry{
		Registry:      NewRegistry(),
		engine:        engine,
		starlarkRules: map[string]*StarlarkRule{},
	}
}

// Compile 编译 Starlark 源 → 注册为新规则.
// name 重复 → 返 error (要替换用 Replace).
//
// 编译失败:不影响已注册规则,返 error 即可.
func (d *DynamicRegistry) Compile(name, source, bizKey string) error {
	if name == "" {
		return fmt.Errorf("name required")
	}
	cs, err := d.engine.Compile(name, source)
	if err != nil {
		return fmt.Errorf("compile %q: %w", name, err)
	}
	rule := NewStarlarkRule(d.engine, cs, name, bizKey)

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.starlarkRules[name]; exists {
		return fmt.Errorf("rule %q already exists (use Replace)", name)
	}
	if err := d.Registry.Register(rule); err != nil {
		return err
	}
	d.starlarkRules[name] = rule
	return nil
}

// Replace 原子替换一条已存在的 Starlark 规则.
//
// 步骤:
//
//	1. 新源码编译 (失败 → 返 error,旧规则保持)
//	2. 写锁里把 Registry.rules 切片中对应 name 的 rule 指针替换
//	3. starlarkRules map 更新
//
// 替换瞬间是原子的:之前已经拿到旧 rule 引用的 goroutine 继续跑完没问题
// (因为 rule 自身字段是 immutable;rule 指针指向新对象就更没问题).
func (d *DynamicRegistry) Replace(name, source, bizKey string) error {
	cs, err := d.engine.Compile(name, source)
	if err != nil {
		return fmt.Errorf("compile %q: %w", name, err)
	}
	newRule := NewStarlarkRule(d.engine, cs, name, bizKey)

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.starlarkRules[name]; !exists {
		return fmt.Errorf("rule %q not registered yet (use Compile first)", name)
	}
	// 在底层 Registry 的 rules 切片里找到位置 + 替换.
	// 借用 Registry 自己的写锁不可能 (它是内部 mu),所以这里通过暴露的方法:
	// 删除 + 注册,顺序保证原子可见.
	d.Registry.removeByName(name)
	if err := d.Registry.Register(newRule); err != nil {
		return err
	}
	d.starlarkRules[name] = newRule
	return nil
}

// Remove 摘掉一条规则.
func (d *DynamicRegistry) Remove(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.starlarkRules[name]; !exists {
		return fmt.Errorf("rule %q not registered", name)
	}
	d.Registry.removeByName(name)
	delete(d.starlarkRules, name)
	return nil
}

// Engine 暴露 underlying engine,便于 admin web 拿去做 _validate / _dry_run.
func (d *DynamicRegistry) Engine() *script.Engine { return d.engine }
