// starlark_rule.go — 把已有 internal/script.Engine 编译产物适配成 matcher.Rule.
//
// 让 Starlark 规则也走 stateless pipeline:
//
//	*.star 源码 -> script.Engine.Compile() -> *CompiledScript -> StarlarkRule (本类) -> matcher.Rule
//
// 关键:Compile 在注册时跑一次,生成 AST + 字节码缓存 (*CompiledScript);
// Match() 只调 Engine.Run(cs, ctx),不再编译 — 即"动态编译,执行时零开销"。
package matcher

import (
	"context"
	"fmt"

	"reconcile-system/internal/pipeline/candidate"
	"reconcile-system/internal/script"
	"reconcile-system/internal/store"
)

// StarlarkRule 把一个已编译的 Starlark 脚本包成 Rule.
//
// 工作流:
//
//	1. compiled = engine.Compile("rule_name", source)   // 一次性编译,生成字节码
//	2. rule = NewStarlarkRule(compiled, engine, "rule_name", "pi_id")
//	3. matcher.Worker.processOne 时调 rule.Match()      // 每次 Run 都是 O(脚本长度)
//	4. 热更新 (源码改):重新 Compile 得新 *CompiledScript,
//	   通过 DynamicRegistry.Replace() 原子换掉旧 rule。
type StarlarkRule struct {
	engine   *script.Engine
	compiled *script.CompiledScript
	ruleName string
	bizKey   string // 该规则期望的 trigger.BizKey;不匹配则返 VerdictPending
}

// NewStarlarkRule 构造.
//
// bizKey 决定本规则关心哪一类 trigger (e.g. "pi_id" 表示只跑 pi_id 桶);
// 空表示所有 trigger 都跑 (不推荐:浪费算力).
func NewStarlarkRule(engine *script.Engine, compiled *script.CompiledScript, ruleName, bizKey string) *StarlarkRule {
	return &StarlarkRule{
		engine:   engine,
		compiled: compiled,
		ruleName: ruleName,
		bizKey:   bizKey,
	}
}

// Name impl.
func (r *StarlarkRule) Name() string { return r.ruleName }

// Match impl.
//
// 把候选层的事件包成 StaticSearcher (script.SearcherIface),让脚本可以 ctx.scan / get_by_index.
// 注意脚本里 return list[dict] 是 script.Diff 列表;这里转成 matcher.MatchResult.
func (r *StarlarkRule) Match(ctx context.Context, t candidate.TriggerKey, events []*store.Event) (MatchResult, error) {
	if r.bizKey != "" && t.BizKey != r.bizKey {
		return MatchResult{Verdict: VerdictPending}, nil
	}
	if r.compiled == nil {
		return MatchResult{Verdict: VerdictError}, fmt.Errorf("starlark rule %q: nil compiled", r.ruleName)
	}

	// 把候选层的 events 喂进一个临时 FixtureSearcher,让脚本 API 不变.
	fs := store.NewFixtureSearcher(toEvents(events))
	sctx := script.NewContext(ctx, fs, nil, map[string]string{
		"trigger.biz_key": t.BizKey,
		"trigger.value":   t.Value,
	})

	diffs, err := r.engine.Run(ctx, r.compiled, sctx)
	if err != nil {
		return MatchResult{Verdict: VerdictError, Error: err.Error()}, err
	}

	// 转换:
	//   - 0 diff       → VerdictMatched
	//   - 1+ diff      → VerdictMismatched
	//   - 脚本可在 Detail 里写 "verdict": "orphan" / "pending" 等覆盖默认裁定
	res := MatchResult{
		Detail: map[string]any{
			"diffs": diffs,
			"count": len(diffs),
		},
	}
	if len(diffs) == 0 {
		res.Verdict = VerdictMatched
	} else {
		// 脚本可以显式声明 verdict (第一条 diff 的 detail.verdict 覆盖)
		if d0Detail, ok := diffs[0].Detail.(map[string]any); ok {
			if v, ok := d0Detail["verdict"].(string); ok {
				res.Verdict = Verdict(v)
			}
		}
		if res.Verdict == "" {
			res.Verdict = VerdictMismatched
		}
	}
	return res, nil
}

// toEvents []*store.Event → []store.Event (FixtureSearcher 取值类型).
func toEvents(es []*store.Event) []store.Event {
	out := make([]store.Event, 0, len(es))
	for _, e := range es {
		if e != nil {
			out = append(out, *e)
		}
	}
	return out
}
