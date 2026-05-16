package seed

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"reconcile-system/internal/script"
)

// 编译期自检: 所有 BuiltinRules 都有对应 .star
func TestMustValidate(t *testing.T) {
	// 调 MustValidate, 失败会 panic
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("MustValidate panic: %v", r)
		}
	}()
	MustValidate()
}

// 所有 .star 在 starlark engine 里能编译通过
func TestAllStarFilesCompile(t *testing.T) {
	files, err := loadAllStarFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no .star files embedded")
	}
	engine := script.NewEngine(1_000_000)
	for name, code := range files {
		_, err := engine.Compile(strings.TrimSuffix(name, ".star"), code)
		if err != nil {
			t.Errorf("compile %s: %v", name, err)
		}
	}
}

// SeedBuiltins 在空 store 上把 8 条都创建.
//
// 注: 实际 store 测试需要 Redis,这里跳过 (集成测试在 ../api/ 跑)
func TestBuiltinRules_HasExpectedSet(t *testing.T) {
	expected := []string{
		"order_in_channel",
		"channel_in_accounting",
		"three_way_amount",
		"three_way_status",
		"orphan_channel_tx",
		"orphan_accounting_entry",
		"refund_three_way",
		"three_way_sync_lag",
	}
	got := map[string]bool{}
	for _, r := range BuiltinRules {
		got[r.ID] = true
	}
	for _, id := range expected {
		if !got[id] {
			t.Errorf("missing builtin rule: %s", id)
		}
	}
	if len(BuiltinRules) != len(expected) {
		t.Errorf("BuiltinRules count = %d, want %d", len(BuiltinRules), len(expected))
	}
}

func TestSha256Hash_Deterministic(t *testing.T) {
	a := sha256hash("hello")
	b := sha256hash("hello")
	c := sha256hash("hello!")
	if a != b {
		t.Error("hash not deterministic")
	}
	if a == c {
		t.Error("different strings should hash differently")
	}
}

func TestIsUserModified(t *testing.T) {
	cases := []struct {
		updatedBy string
		want      bool
	}{
		{"", false},
		{"system:seeder", false},
		{"system:upgrade", false},
		{"alice@example.com", true},
		{"user:bob", true},
	}
	for _, c := range cases {
		got := isUserModified(&script.Script{UpdatedBy: c.updatedBy})
		if got != c.want {
			t.Errorf("isUserModified(%q) = %v, want %v", c.updatedBy, got, c.want)
		}
	}
}

func TestEachRuleHasNonTrivialCode(t *testing.T) {
	files, err := loadAllStarFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range BuiltinRules {
		code, ok := files[r.ID+".star"]
		if !ok {
			t.Errorf("%s: source missing", r.ID)
			continue
		}
		// 检查关键字: 必须有 def check(ctx), 必须 return diffs
		if !strings.Contains(code, "def check(ctx)") {
			t.Errorf("%s: missing def check(ctx)", r.ID)
		}
		if !strings.Contains(code, "return diffs") {
			t.Errorf("%s: missing return diffs", r.ID)
		}
		// meta-version header
		if !strings.Contains(code, "meta-version:") {
			t.Errorf("%s: missing meta-version header (升级追踪用)", r.ID)
		}
	}
}

// SeedBuiltins 用 nil store 应返 error
func TestSeedBuiltins_NilStore(t *testing.T) {
	_, err := SeedBuiltins(context.Background(), nil, zap.NewNop(), "x")
	if err == nil {
		t.Error("expected error for nil store")
	}
}
