package script

import (
	"context"
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// 最小可跑：def check(ctx) → return []，无 module / 无 ctx 调用。
func TestEngine_TrivialScript(t *testing.T) {
	e := NewEngine(0)
	cs, err := e.Compile("trivial", `
def check(ctx):
    return []
`)
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := e.Run(context.Background(), cs, NewContext(context.Background(), nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("expected 0 diffs, got %d", len(diffs))
	}
}

// 缺 check 函数 → 编译期失败。
func TestEngine_MissingCheckFunction(t *testing.T) {
	e := NewEngine(0)
	_, err := e.Compile("no_check", `def other(ctx): return []`)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "check") {
		t.Fatalf("error should mention check, got: %v", err)
	}
}

// 语法错 → 编译期失败。
func TestEngine_SyntaxError(t *testing.T) {
	e := NewEngine(0)
	_, err := e.Compile("bad", `def check(ctx): return [`)
	if err == nil {
		t.Fatal("expected syntax error")
	}
}

// 返一条 diff dict → engine 解析为 Diff struct。
func TestEngine_ReturnDiffList(t *testing.T) {
	e := NewEngine(0)
	cs, err := e.Compile("diff", `
def check(ctx):
    return [
        {"type": "missing", "key": "pi_xxx"},
        {"type": "amount_mismatch", "key": "pi_yyy", "want": 100, "got": 99},
    ]
`)
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := e.Run(context.Background(), cs, NewContext(context.Background(), nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 2 {
		t.Fatalf("expected 2 diffs, got %d", len(diffs))
	}
	if diffs[0].Type != "missing" || diffs[0].Key != "pi_xxx" {
		t.Errorf("diff 0 wrong: %+v", diffs[0])
	}
	if diffs[1].Type != "amount_mismatch" {
		t.Errorf("diff 1 wrong: %+v", diffs[1])
	}
	// want 是 int64
	if w, ok := diffs[1].Want.(int64); !ok || w != 100 {
		t.Errorf("diff 1 want should be int64=100, got %T %v", diffs[1].Want, diffs[1].Want)
	}
}

// load("@json", "encode") 能用。
func TestEngine_BuiltinJSONModule(t *testing.T) {
	e := NewEngine(0)
	cs, err := e.Compile("json_use", `
load("@json", "encode")
def check(ctx):
    s = encode({"a": 1, "b": "hi"})
    return [{"type": "json_test", "key": "k1", "detail": s}]
`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	diffs, err := e.Run(context.Background(), cs, NewContext(context.Background(), nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}
	s, _ := diffs[0].Detail.(string)
	if !strings.Contains(s, `"a":1`) {
		t.Errorf("expected json with a:1, got %q", s)
	}
}

// load("@strings", "split") + "@regex" 也都能用。
func TestEngine_StringsAndRegex(t *testing.T) {
	e := NewEngine(0)
	cs, err := e.Compile("str", `
load("@strings", "split", "to_upper")
load("@regex", "match")

def check(ctx):
    parts = split("a,b,c", ",")
    upper = to_upper(parts[1])
    is_pi = match("^pi_[a-z0-9]+$", "pi_abc123")
    return [{"type": "ok", "key": upper, "detail": {"parts_len": len(parts), "is_pi": is_pi}}]
`)
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := e.Run(context.Background(), cs, NewContext(context.Background(), nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Key != "B" {
		t.Fatalf("unexpected diffs: %+v", diffs)
	}
	det := diffs[0].Detail.(map[string]any)
	if det["parts_len"].(int64) != 3 {
		t.Errorf("parts_len: %v", det["parts_len"])
	}
	if det["is_pi"].(bool) != true {
		t.Errorf("is_pi: %v", det["is_pi"])
	}
}

// 动态 RegisterModule 后立刻可用 — 验"运行时加包不重启"路径。
func TestEngine_RegisterModule_Runtime(t *testing.T) {
	e := NewEngine(0)
	// 注册一个 fake "kafka" module 暴露 publish()
	e.RegisterModule("kafka", starlark.StringDict{
		"publish": starlark.NewBuiltin("kafka.publish", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
			return starlark.String("ok"), nil
		}),
	})

	cs, err := e.Compile("kafka_use", `
load("@kafka", "publish")
def check(ctx):
    r = publish("topic-x", "payload")
    return [{"type": "published", "key": r}]
`)
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := e.Run(context.Background(), cs, NewContext(context.Background(), nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Key != "ok" {
		t.Fatalf("unexpected diffs: %+v", diffs)
	}
}

// 未知 module → load 失败。
func TestEngine_UnknownModule(t *testing.T) {
	e := NewEngine(0)
	_, err := e.Compile("unknown_mod", `
load("@nonexistent", "foo")
def check(ctx):
    return []
`)
	if err == nil {
		t.Fatal("expected error for unknown module")
	}
}

// 脚本 panic 不能拖死 engine。
func TestEngine_ScriptError_NoPanic(t *testing.T) {
	e := NewEngine(0)
	cs, err := e.Compile("err_in_check", `
def check(ctx):
    fail("boom")
`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Run(context.Background(), cs, NewContext(context.Background(), nil, nil, nil))
	if err == nil {
		t.Fatal("expected error from fail()")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should mention 'boom', got: %v", err)
	}
}

// CollectSymbols 给 admin web autocomplete 用。
func TestCollectSymbols_HasBuiltinModules(t *testing.T) {
	e := NewEngine(0)
	syms := CollectSymbols(e)

	// 必有 6 个 builtin module
	mods := map[string]bool{}
	for _, m := range syms.Modules {
		mods[m.Name] = true
	}
	for _, name := range []string{"json", "time", "math", "strings", "regex", "recon"} {
		if !mods[name] {
			t.Errorf("missing builtin module: %s", name)
		}
	}
	// ctx 必有这些方法
	ctxNames := map[string]bool{}
	for _, c := range syms.Ctx {
		ctxNames[c.Name] = true
	}
	for _, m := range []string{"scan_index", "get_by_index", "get", "http_get", "log_info"} {
		if !ctxNames[m] {
			t.Errorf("ctx missing method: %s", m)
		}
	}
}

// Loader.Validate 是 admin "语法检查" 按钮的入口。
func TestLoader_Validate(t *testing.T) {
	l := NewLoader(nil)
	if err := l.Validate("def check(ctx): return []"); err != nil {
		t.Fatalf("good script should validate: %v", err)
	}
	if err := l.Validate("def other(): pass"); err == nil {
		t.Fatal("missing check() should fail")
	}
	if err := l.Validate(""); err == nil {
		t.Fatal("empty code should fail")
	}
}
