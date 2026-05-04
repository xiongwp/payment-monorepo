package main

import (
	"reflect"
	"testing"
)

// rulesAsAny 帮 test 构造 viper 解出的等价结构。
func rule(id string, kv ...string) any {
	m := map[string]any{"id": id}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestMergeRulesByID_BothNil(t *testing.T) {
	got := mergeRulesByID(nil, nil)
	if len(got) != 0 {
		t.Errorf("expected 0 rules, got %d", len(got))
	}
}

func TestMergeRulesByID_RecipeOnly(t *testing.T) {
	got := mergeRulesByID(nil, []any{rule("a"), rule("b")})
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
	if got[0].(map[string]any)["id"] != "a" || got[1].(map[string]any)["id"] != "b" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestMergeRulesByID_BaseOverridesRecipe(t *testing.T) {
	// 同 id "x"，base 的 enabled=false 应该胜出（运维想关某条用 base 强制覆盖）
	base := []any{rule("x", "enabled", "false")}
	recipe := []any{rule("x", "enabled", "true"), rule("y")}
	got := mergeRulesByID(base, recipe)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
	first, ok := got[0].(map[string]any)
	if !ok || first["id"] != "x" || first["enabled"] != "false" {
		t.Errorf("base rule should win for id=x, got %+v", first)
	}
	if got[1].(map[string]any)["id"] != "y" {
		t.Errorf("recipe-only y should be appended, got %+v", got[1])
	}
}

func TestMergeRulesByID_SkipsEmptyID(t *testing.T) {
	// 空 id 的项被跳过（防意外把无名 stub 进 engine）
	got := mergeRulesByID(nil, []any{map[string]any{"name": "no id"}, rule("ok")})
	if len(got) != 1 || got[0].(map[string]any)["id"] != "ok" {
		t.Errorf("expected only 'ok', got %+v", got)
	}
}

func TestMergeRulesByID_Order(t *testing.T) {
	// base 顺序保留 + 新 recipe append 在尾
	base := []any{rule("a"), rule("b")}
	recipe := []any{rule("a"), rule("c"), rule("d")}
	got := mergeRulesByID(base, recipe)
	ids := []string{}
	for _, r := range got {
		ids = append(ids, r.(map[string]any)["id"].(string))
	}
	want := []string{"a", "b", "c", "d"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("expected %v, got %v", want, ids)
	}
}
