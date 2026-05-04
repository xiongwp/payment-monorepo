// Package ruleio 规则集 YAML 导入 / 导出。给运营跨环境推规则用：
//
//   1. 在 staging 把规则跑出来 → /admin/rules/export 拿到 YAML
//   2. 在 prod /admin/rules/import?dry_run=true 跑校验 + 看 diff
//   3. 确认无误 → import?dry_run=false 真正落地
//
// 比直接改 ConfigMap 安全：每条 rule 都过 BuildRule schema 校验，失败时
// 整个 import 拒绝（不会半套生效）。
//
// YAML schema 跟 config/config.yaml 的 rules: 段一致：
//
//	rules:
//	  - id: limit_per_txn_50k
//	    name: "单笔上限"
//	    type: amount_limit
//	    decision: DENY
//	    enabled: true
//	    mode: enforce
//	    weight: 0
//	    config: '{"max_per_txn": 5000000}'
//	    rollout:
//	      enable_pct: 100
package ruleio

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// yamlRule 镜像 config.yaml 里的 rule 项格式。注意 Config 字段是 string
// (raw JSON)，跟 main.go reload 路径解出的 RuleDef 一致 — 让 export 的
// YAML 直接能 reload。
type yamlRule struct {
	ID       string               `yaml:"id"`
	Name     string               `yaml:"name,omitempty"`
	Type     string               `yaml:"type"`
	Decision string               `yaml:"decision,omitempty"`
	Enabled  bool                 `yaml:"enabled"`
	Mode     string               `yaml:"mode,omitempty"`
	Weight   int                  `yaml:"weight,omitempty"`
	Config   string               `yaml:"config,omitempty"`
	Rollout  engine.RolloutConfig `yaml:"rollout,omitempty"`
}

type yamlBundle struct {
	Rules []yamlRule `yaml:"rules"`
}

// Marshal 把当前 RuleDef[] 序列化成 YAML bytes。给 /admin/rules/export 用。
//
// config_json 字段以 string 落 YAML（保持跟 config.yaml 一致格式 →
// 圆括号里 raw JSON）。空 config_json 字段省略。
func Marshal(defs []engine.RuleDef) ([]byte, error) {
	b := yamlBundle{Rules: make([]yamlRule, 0, len(defs))}
	for _, d := range defs {
		yr := yamlRule{
			ID:       d.ID,
			Name:     d.Name,
			Type:     d.Type,
			Decision: d.Decision,
			Enabled:  d.Enabled,
			Mode:     d.Mode,
			Weight:   d.Weight,
			Rollout:  d.Rollout,
		}
		if len(d.ConfigJSON) > 0 && string(d.ConfigJSON) != "null" {
			yr.Config = string(d.ConfigJSON)
		}
		b.Rules = append(b.Rules, yr)
	}
	return yaml.Marshal(b)
}

// Unmarshal 把 YAML bytes 解成 RuleDef[]。失败 → error；不做 schema 校验
// （留给 caller 用 engine.BuildRule 验证）。
func Unmarshal(data []byte) ([]engine.RuleDef, error) {
	var b yamlBundle
	if err := yaml.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("yaml parse: %w", err)
	}
	out := make([]engine.RuleDef, 0, len(b.Rules))
	for i, yr := range b.Rules {
		if yr.ID == "" {
			return nil, fmt.Errorf("rule %d: id required", i)
		}
		if yr.Type == "" {
			return nil, fmt.Errorf("rule %s: type required", yr.ID)
		}
		var cfg json.RawMessage
		if yr.Config != "" {
			// 验证是合法 JSON
			if err := json.Unmarshal([]byte(yr.Config), new(any)); err != nil {
				return nil, fmt.Errorf("rule %s: config not valid JSON: %w", yr.ID, err)
			}
			cfg = json.RawMessage(yr.Config)
		}
		out = append(out, engine.RuleDef{
			ID: yr.ID, Name: yr.Name, Type: yr.Type, Decision: yr.Decision,
			Enabled: yr.Enabled, Mode: yr.Mode, Weight: yr.Weight,
			ConfigJSON: cfg, Rollout: yr.Rollout,
		})
	}
	return out, nil
}

// DiffEntry 单条 import diff 行。
type DiffEntry struct {
	RuleID string `json:"rule_id"`
	Action string `json:"action"` // create / update / delete / unchanged
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
}

// Diff 对比 current vs imported，返回每条规则的变更动作。给 dry-run 用，
// 让运营在真正 apply 前看清楚会改什么。
//
//	import 里有的 + current 没有 → create
//	import 里有的 + current 也有 + 内容不同 → update
//	import 里没有 + current 有 → delete
//	import 里有的 + current 也有 + 内容相同 → unchanged
func Diff(current, imported []engine.RuleDef) []DiffEntry {
	curMap := make(map[string]engine.RuleDef, len(current))
	for _, d := range current {
		curMap[d.ID] = d
	}
	impMap := make(map[string]engine.RuleDef, len(imported))
	for _, d := range imported {
		impMap[d.ID] = d
	}
	out := make([]DiffEntry, 0, len(imported)+len(current))
	// 按 imported 顺序输出 create / update / unchanged
	for _, imp := range imported {
		cur, ok := curMap[imp.ID]
		if !ok {
			out = append(out, DiffEntry{RuleID: imp.ID, Action: "create", After: imp})
			continue
		}
		if ruleDefEqual(cur, imp) {
			out = append(out, DiffEntry{RuleID: imp.ID, Action: "unchanged"})
		} else {
			out = append(out, DiffEntry{RuleID: imp.ID, Action: "update", Before: cur, After: imp})
		}
	}
	// current 里有但 imported 没有的 → delete
	for _, cur := range current {
		if _, ok := impMap[cur.ID]; !ok {
			out = append(out, DiffEntry{RuleID: cur.ID, Action: "delete", Before: cur})
		}
	}
	return out
}

func ruleDefEqual(a, b engine.RuleDef) bool {
	if a.ID != b.ID || a.Name != b.Name || a.Type != b.Type ||
		a.Decision != b.Decision || a.Enabled != b.Enabled ||
		a.Mode != b.Mode || a.Weight != b.Weight {
		return false
	}
	if a.Rollout != b.Rollout {
		return false
	}
	// ConfigJSON：比较反序列化后的结构（避免 whitespace / key order 差异）
	var av, bv any
	_ = json.Unmarshal(a.ConfigJSON, &av)
	_ = json.Unmarshal(b.ConfigJSON, &bv)
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return string(ab) == string(bb)
}
