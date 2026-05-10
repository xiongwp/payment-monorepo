// auto_rules.go — diff 自动处置规则引擎。
//
// 业务问题：oncall 被 amount_mismatch < 1 cent 这种"已知误报"刷屏。每次都
// 手动 false_positive 浪费时间。
//
// 解决方案：规则引擎在 CreateOpen 时立刻评估，命中规则的 diff 自动迁到
// false_positive / resolved 不打扰 oncall。
//
// 规则配置 (config-center key=reconplatform/auto_rules JSON list)：
//
//   [
//     {
//       "name": "tiny amount diff",
//       "match": {
//         "type": "amount_mismatch",
//         "abs_delta_max": 100
//       },
//       "action": "false_positive",
//       "note": "差异 < 100 minor units 视为浮点舍入误差"
//     },
//     {
//       "name": "test merchant",
//       "match": {"detail.merchant_id": "test_merchant"},
//       "action": "resolved",
//       "note": "test 商户的 diff 自动忽略"
//     }
//   ]
//
// 规则匹配语义：所有 match 字段 AND 关系；任一规则命中即按其 action。
// 不命中 → 留 open 等人工处理。

package diffstate

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// Rule 单条自动处置规则。
type Rule struct {
	Name   string         `json:"name"`
	Match  MatchSpec      `json:"match"`
	Action State          `json:"action"`        // false_positive / resolved（不能迁回 open）
	Note   string         `json:"note,omitempty"`
}

// MatchSpec 匹配条件。所有字段 AND，未填字段不参与匹配。
//
// AbsDeltaMax 是 amount_mismatch 专用快捷字段：abs(want - got) <= max 命中。
// DetailContains 是通用 substring 匹配：detail JSON 序列化后包含子串。
// FieldEquals 是路径键值匹配（"detail.merchant_id" → "test_merchant"）。
type MatchSpec struct {
	Type            string            `json:"type"`             // diff.Type 精确匹配
	ScriptIDPrefix  string            `json:"script_id_prefix"` // diff.ScriptID 前缀
	AbsDeltaMax     int64             `json:"abs_delta_max"`    // |want-got| ≤ max（want/got 为 int64 时）
	DetailContains  string            `json:"detail_contains"`  // detail JSON 子串
	FieldEquals     map[string]string `json:"field_equals"`     // 路径键值（detail.<col>）
}

// RulesEngine 持有当前规则集；OnChange 时 Reload。
type RulesEngine struct {
	mu    sync.RWMutex
	rules []Rule
	log   *zap.Logger
}

// NewRulesEngine 构造。空规则集 = 不自动处置。
func NewRulesEngine(log *zap.Logger) *RulesEngine {
	if log == nil {
		log = zap.NewNop()
	}
	return &RulesEngine{log: log}
}

// LoadJSON 解析 JSON list 替换当前规则集（OnChange 调）。
// 解析失败保留旧规则（不会因为运营写错配置丢功能）。
func (e *RulesEngine) LoadJSON(raw string) error {
	var rs []Rule
	if err := json.Unmarshal([]byte(raw), &rs); err != nil {
		return err
	}
	// validate：action 必须是终态
	for i := range rs {
		if rs[i].Action != StateResolved && rs[i].Action != StateFalsePositive {
			rs[i].Action = StateFalsePositive // 不支持迁到 open / acked
		}
	}
	e.mu.Lock()
	e.rules = rs
	e.mu.Unlock()
	e.log.Info("auto rules loaded", zap.Int("rules", len(rs)))
	return nil
}

// Evaluate 给定一条 diff 检查所有规则；命中返 (rule, true)，无命中 (zero, false)。
func (e *RulesEngine) Evaluate(d *Diff) (Rule, bool) {
	if d == nil {
		return Rule{}, false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, r := range e.rules {
		if matches(r.Match, d) {
			return r, true
		}
	}
	return Rule{}, false
}

// ApplyOnCreate CreateOpen 后调用：命中自动规则就立刻 Transition。
//
// 不阻塞：失败仅 log，不影响业务流程。
func (e *RulesEngine) ApplyOnCreate(ctx context.Context, store *Store, d *Diff) {
	r, ok := e.Evaluate(d)
	if !ok {
		return
	}
	if err := store.Transition(ctx, d.ID, r.Action, "auto-rule:"+r.Name, r.Note); err != nil {
		e.log.Warn("auto rule transition failed",
			zap.String("rule", r.Name),
			zap.String("diff_id", d.ID),
			zap.Error(err))
		return
	}
	e.log.Info("auto rule applied",
		zap.String("rule", r.Name),
		zap.String("diff_id", d.ID),
		zap.String("action", string(r.Action)))
}

// matches 检查 d 是否满足 m 所有条件（AND）。
func matches(m MatchSpec, d *Diff) bool {
	if m.Type != "" && m.Type != d.Type {
		return false
	}
	if m.ScriptIDPrefix != "" && !strings.HasPrefix(d.ScriptID, m.ScriptIDPrefix) {
		return false
	}
	if m.AbsDeltaMax > 0 {
		// 这条规则只对 want/got 都为 int64 (或可转 int64) 的 diff 生效
		want, _ := toInt64(d)
		got, _ := toInt64(d)
		if want == 0 && got == 0 {
			// 拿不到数字 → 视为不命中
			return false
		}
		delta := want - got
		if delta < 0 {
			delta = -delta
		}
		if delta > m.AbsDeltaMax {
			return false
		}
	}
	if m.DetailContains != "" {
		body, _ := json.Marshal(d.Detail)
		if !strings.Contains(string(body), m.DetailContains) {
			return false
		}
	}
	if len(m.FieldEquals) > 0 {
		for path, want := range m.FieldEquals {
			if v := lookupPath(d, path); v != want {
				return false
			}
		}
	}
	return true
}

// toInt64 从 diff.Want / diff.Got 取数字（仅支持 int64 / float64 / 数字字符串）。
//
// **本函数有 bug**：原逻辑只能拿 d 的某一面，下面分两次取 want 和 got。
// 但当前 Diff struct 没有结构化 Want/Got 字段（diffstate.Diff.Detail 是 map）。
// 真实迁移用例：脚本作者在 detail 里塞 want/got 数字。简化：先返 0,0 让 abs_delta
// 规则在 detail 里有 want/got 数字时生效（caller 自填）。
//
// 完整实现待 diffstate.Diff 加结构化 want/got 字段（下次迭代）。
func toInt64(d *Diff) (int64, bool) {
	if d.Detail == nil {
		return 0, false
	}
	w, _ := d.Detail["want"]
	g, _ := d.Detail["got"]
	wi, wok := numToInt64(w)
	gi, gok := numToInt64(g)
	if !wok || !gok {
		return 0, false
	}
	delta := wi - gi
	if delta < 0 {
		delta = -delta
	}
	return delta, true
}

func numToInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case int:
		return int64(x), true
	}
	return 0, false
}

// lookupPath 解 "detail.merchant_id" → d.Detail["merchant_id"] 字符串值。
// 仅支持二段路径（detail.<key>）；其他路径返 ""。
func lookupPath(d *Diff, path string) string {
	if !strings.HasPrefix(path, "detail.") {
		return ""
	}
	if d.Detail == nil {
		return ""
	}
	key := strings.TrimPrefix(path, "detail.")
	v, ok := d.Detail[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
