package script

import (
	"strings"
	"testing"
)

func hasIssue(t *testing.T, code, wantCode string) []LintIssue {
	t.Helper()
	issues, err := Lint("test", code)
	if err != nil {
		t.Fatalf("lint err: %v", err)
	}
	for _, i := range issues {
		if i.Code == wantCode {
			return issues
		}
	}
	t.Fatalf("expected issue %s, got %+v", wantCode, issues)
	return nil
}

func noIssue(t *testing.T, code, dontWantCode string) {
	t.Helper()
	issues, err := Lint("test", code)
	if err != nil {
		t.Fatalf("lint err: %v", err)
	}
	for _, i := range issues {
		if i.Code == dontWantCode {
			t.Fatalf("unexpected issue %s in: %s\nall: %+v", dontWantCode, code, issues)
		}
	}
}

// 干净脚本 — 无 issue
func TestLint_Clean(t *testing.T) {
	code := `
def check(ctx):
    diffs = []
    for pi in ctx.scan_index("pi_id", "", 1000):
        events = ctx.get_by_index("pi_id", pi)
        if events.len > 0:
            diffs.append({"type": "ok", "key": pi})
    return diffs
`
	issues, err := Lint("clean", code)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range issues {
		if i.Severity == SeverityError {
			t.Errorf("clean script had error: %+v", i)
		}
	}
}

// E001: ctx 必须叫 ctx
func TestLint_E001_WrongParamName(t *testing.T) {
	code := `def check(c):
    return []
`
	hasIssue(t, code, "E001")
}

// E001: 参数个数不对
func TestLint_E001_WrongArity(t *testing.T) {
	code := `def check(ctx, extra):
    return []
`
	hasIssue(t, code, "E001")
}

// E002: 没 return
func TestLint_E002_NoReturn(t *testing.T) {
	code := `def check(ctx):
    x = 1
`
	hasIssue(t, code, "E002")
}

// E002: return None / 数字 / 字符串
func TestLint_E002_BadReturnType(t *testing.T) {
	cases := []string{
		`def check(ctx):
    return
`,
		`def check(ctx):
    return None
`,
		`def check(ctx):
    return 42
`,
		`def check(ctx):
    return "hello"
`,
	}
	for _, c := range cases {
		hasIssue(t, c, "E002")
	}
}

// E002: return dict（写漏 [...]）
func TestLint_E002_ReturnDict(t *testing.T) {
	code := `def check(ctx):
    return {"type": "x", "key": "k"}
`
	hasIssue(t, code, "E002")
}

// 不报错：return list 字面量 / list comprehension / 变量 / call
func TestLint_E002_OK(t *testing.T) {
	cases := []string{
		`def check(ctx):
    return []
`,
		`def check(ctx):
    return [{"type": "x", "key": "k"}]
`,
		`def check(ctx):
    return [d for d in [] if d]
`,
		`def check(ctx):
    diffs = []
    return diffs
`,
		`def check(ctx):
    return collect()
`,
	}
	for _, c := range cases {
		noIssue(t, c, "E002")
	}
}

// W001: 嵌套 for
func TestLint_W001_NestedLoop(t *testing.T) {
	code := `def check(ctx):
    for a in [1,2,3]:
        for b in [4,5,6]:
            print(a, b)
    return []
`
	hasIssue(t, code, "W001")
}

// 不嵌套 → 不报
func TestLint_W001_FlatLoop(t *testing.T) {
	code := `def check(ctx):
    for a in [1,2,3]:
        print(a)
    return []
`
	noIssue(t, code, "W001")
}

// W002: 循环里调 expensive op
func TestLint_W002_ExpensiveInLoop(t *testing.T) {
	code := `def check(ctx):
    for pi in [1,2,3]:
        ctx.http_get("http://x")
    return []
`
	hasIssue(t, code, "W002")
}

func TestLint_W002_DotPath_ScanIndex(t *testing.T) {
	code := `def check(ctx):
    for x in [1,2,3]:
        ctx.scan_index("k", "", 100)
    return []
`
	hasIssue(t, code, "W002")
}

// 在循环外调 expensive op → 不报
func TestLint_W002_OutsideLoop(t *testing.T) {
	code := `def check(ctx):
    keys = ctx.scan_index("k", "", 100)
    for x in keys:
        pass
    return []
`
	noIssue(t, code, "W002")
}

// W004: ctx 未用 → 警告
func TestLint_W004_UnusedCtx(t *testing.T) {
	code := `def check(ctx):
    return []
`
	hasIssue(t, code, "W004")
}

// ctx 用了 → 不报
func TestLint_W004_UsedCtx(t *testing.T) {
	code := `def check(ctx):
    ctx.log_info("hi")
    return []
`
	noIssue(t, code, "W004")
}

// I001: 长脚本
func TestLint_I001_LongScript(t *testing.T) {
	body := strings.Repeat("    pass\n", 250)
	code := "def check(ctx):\n    ctx.log_info(\"hi\")\n" + body + "    return []\n"
	hasIssue(t, code, "I001")
}

// 语法错 → 返 error 不返 issues
func TestLint_SyntaxError(t *testing.T) {
	_, err := Lint("bad", `def check(ctx`)
	if err == nil {
		t.Fatal("expected parse error")
	}
}
