package rule

import (
	"sync"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

type Engine struct {
	mu    sync.RWMutex
	rules map[string]*vm.Program
}

func New() *Engine {
	return &Engine{rules: make(map[string]*vm.Program)}
}

func (e *Engine) Update(name, exprStr string) error {

	prog, err := expr.Compile(exprStr, expr.Env(map[string]interface{}{
		"order":   map[string]interface{}{},
		"payment": map[string]interface{}{},
	}))
	if err != nil {
		return err
	}

	e.mu.Lock()
	e.rules[name] = prog
	e.mu.Unlock()
	return nil
}

func (e *Engine) EvalAll(env map[string]interface{}) map[string]string {

	e.mu.RLock()
	defer e.mu.RUnlock()

	res := make(map[string]string)

	for name, prog := range e.rules {

		out, err := expr.Run(prog, env)

		if err != nil {
			res[name] = "ERROR"
			continue
		}
		// 资损修复：rule expression 必须返 bool。之前 out.(bool) 无 comma-ok →
		// 任何返非 bool 的规则（写错的 expr、未来扩展返金额 / 字符串）会 panic
		// 整个消费 goroutine → reconcile 静默崩溃 → 资金缺漏不告警。
		matched, ok := out.(bool)
		if !ok {
			res[name] = "ERROR"
			continue
		}
		if matched {
			res[name] = "MATCH"
		} else {
			res[name] = "MISMATCH"
		}
	}
	return res
}
