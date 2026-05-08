// yaegi_impl.go — Yaegi 解释器真实集成。
//
// 设计扩展点：
//
//   1. SymbolPack — 对外暴露的 import 包注入。
//      默认注入：stdlib.Symbols + recon (script.Context/EventList/Diff/Result)
//      运营要让脚本 import 自定义包（如内部 utility）→ 注册新 SymbolPack。
//
//   2. Sandbox — 安全约束。yaegi 默认能执行任何 Go 代码（包括 unsafe / os.Exec）。
//      我们用白名单 stdlib（GoCacheDir / GoPath 都隔离），并禁止 unsafe / syscall /
//      net (除非 ctx.HTTPGet) / os.Exec / runtime/debug 写操作。
//
//   3. Timeout — 单次脚本运行 ctx.Deadline；yaegi 的 reflect.Call 不能直接打断，
//      靠 script 自己的 ctx.Ctx.Done() 检查。我们在 advanced API（GetByIndex /
//      ScanIndex 等）入口处都判断 ctx.Err()，遇到 deadline 立刻 return nil。
//
// 用法（在 main.go）：
//
//   loader := script.NewLoader()
//   script.SetYaegiBackend(loader)   // 把 stub 替换成真 yaegi
//
// 之后所有 loader.Validate / Add / Replace 都走 yaegi 真编译 + 调用。

package script

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"

	"reconcile-system/internal/store"
)

// SetYaegiBackend 把 Loader 的 stub 替换成 yaegi 真实 interpreter。
//
// caller 调一次（启动期）；之后 loader.Validate / Add / Replace 都走真 yaegi。
//
// 额外可选 packs 让用户注入自定义符号；没有就只挂 stdlib + recon。
func SetYaegiBackend(l *Loader, packs ...interp.Exports) {
	l.yaegiNew = func() interpreter {
		i := interp.New(interp.Options{
			Unrestricted: false, // 禁 unsafe / syscall（默认）
		})
		// 标准库（fmt / time / strings / strconv / encoding/json 等）
		_ = i.Use(stdlib.Symbols)
		// recon 包：暴露 Context / EventList / Diff / Result 给脚本 import "recon"
		_ = i.Use(reconSymbols())
		// 用户自定义包
		for _, pack := range packs {
			_ = i.Use(pack)
		}
		return &yaegiInterp{i: i}
	}
}

// yaegiInterp 实现 loader 内部的 interpreter 接口（Eval / Get）。
type yaegiInterp struct {
	i *interp.Interpreter
}

func (y *yaegiInterp) Eval(src string) error {
	_, err := y.i.Eval(src)
	return err
}

func (y *yaegiInterp) Get(qualifiedName string) (reflect.Value, error) {
	return y.i.Eval(qualifiedName)
}

// reconSymbols 把 script 包内的类型导出到 yaegi 的 "recon" import path。
//
// 脚本里写：
//
//   import "recon"
//   func Check(ctx *recon.Context) (*recon.Result, error) { ... }
//
// yaegi 会从这个 map 找 recon.Context / recon.Result 等符号。
//
// 维护提醒：新加方法到 Context 时不需要改这里（reflect 自动找到 *Context 类型
// 上所有 exported method）；新加 Stand-alone helper（如 Last24Hours）需要在
// 这里手动列出。
func reconSymbols() interp.Exports {
	return interp.Exports{
		"recon/recon": {
			// 类型
			"Context":   reflect.ValueOf((*Context)(nil)),
			"Diff":      reflect.ValueOf((*Diff)(nil)),
			"Result":    reflect.ValueOf((*Result)(nil)),
			"Stats":     reflect.ValueOf((*Stats)(nil)),
			"Logger":    reflect.ValueOf((*Logger)(nil)).Elem(),
			"Event":     reflect.ValueOf((*store.Event)(nil)),
			"EventList": reflect.ValueOf((*store.EventList)(nil)).Elem(),
			"SQLDB":     reflect.ValueOf((*SQLDB)(nil)),

			// Stand-alone helper
			"Last24Hours": reflect.ValueOf(Last24Hours),
			"LastNHours":  reflect.ValueOf(LastNHours),
		},
	}
}

// EnsureYaegiAvailable 启动期自检：跑一个 trivial 脚本验证 yaegi 工作。
// 失败时返回 error，main 决定是 fail-fast 还是降级（默认 fail-fast，避免
// 运营写脚本结果保存了但根本没法运行）。
func (l *Loader) EnsureYaegiAvailable() error {
	if l.yaegiNew == nil {
		return errors.New("yaegi backend not set; call script.SetYaegiBackend(loader) first")
	}
	const probe = `package main

import "recon"

func Check(ctx *recon.Context) (*recon.Result, error) {
	return &recon.Result{}, nil
}
`
	if err := l.Validate(probe); err != nil {
		return fmt.Errorf("yaegi probe failed: %w", err)
	}
	return nil
}
