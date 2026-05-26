package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestCanonicalJSON_KeyOrderStable(t *testing.T) {
	a := []byte(`{"b":1,"a":2,"c":{"y":1,"x":2}}`)
	b := []byte(`{"a":2,"c":{"x":2,"y":1},"b":1}`)
	ca, err := CanonicalJSON(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := CanonicalJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != string(cb) {
		t.Fatalf("canonical mismatch:\n a=%s\n b=%s", ca, cb)
	}
	if ContentHash(a) != ContentHash(b) {
		t.Fatalf("content hash mismatch")
	}
}

func TestMemRuleVersionStore_RecordIdempotent(t *testing.T) {
	s := NewMemRuleVersionStore()
	ctx := context.Background()

	spec1 := []byte(`{"id":"r1","enabled":true}`)
	v1, isNew, err := s.Record(ctx, "r1", spec1, "init", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if v1 != 1 || !isNew {
		t.Fatalf("first record: got version=%d isNew=%v, want 1/true", v1, isNew)
	}
	// 同内容（即使键序不同）→ 复用旧 version，不写新行
	spec1Reorder := []byte(`{"enabled":true,"id":"r1"}`)
	v1b, isNewB, err := s.Record(ctx, "r1", spec1Reorder, "noop", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if v1b != v1 || isNewB {
		t.Fatalf("idempotent record: got version=%d isNew=%v, want %d/false", v1b, isNewB, v1)
	}
	// 不同内容 → 新 version
	spec2 := []byte(`{"id":"r1","enabled":false}`)
	v2, isNew2, err := s.Record(ctx, "r1", spec2, "disabled", "carol")
	if err != nil {
		t.Fatal(err)
	}
	if v2 != 2 || !isNew2 {
		t.Fatalf("second record: got version=%d isNew=%v, want 2/true", v2, isNew2)
	}
}

func TestMemRuleVersionStore_ActivateAndGetActive(t *testing.T) {
	s := NewMemRuleVersionStore()
	ctx := context.Background()

	v1, _, _ := s.Record(ctx, "r1", []byte(`{"v":1}`), "", "")
	v2, _, _ := s.Record(ctx, "r1", []byte(`{"v":2}`), "", "")

	// activate v2
	if err := s.Activate(ctx, "r1", v2, "op"); err != nil {
		t.Fatal(err)
	}
	ver, spec, err := s.GetActive(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if ver != v2 {
		t.Fatalf("active version: got %d, want %d", ver, v2)
	}
	if !strings.Contains(string(spec), `"v":2`) {
		t.Fatalf("active spec mismatch: %s", spec)
	}
	// rollback：切回 v1
	if err := s.Activate(ctx, "r1", v1, "op"); err != nil {
		t.Fatal(err)
	}
	ver, _, _ = s.GetActive(ctx, "r1")
	if ver != v1 {
		t.Fatalf("rollback failed: active=%d, want %d", ver, v1)
	}
	// activate 不存在的版本 → ErrVersionNotFound
	if err := s.Activate(ctx, "r1", 999, "op"); err == nil {
		t.Fatal("activate non-existent version should fail")
	}
}

func TestMemRuleVersionStore_ListVersionsDescending(t *testing.T) {
	s := NewMemRuleVersionStore()
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		spec := []byte(`{"v":` + itoa(i) + `}`)
		if _, _, err := s.Record(ctx, "r1", spec, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListVersions(ctx, "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("want 5 versions; got %d", len(got))
	}
	// 降序：5,4,3,2,1
	for i, rv := range got {
		want := int64(5 - i)
		if rv.Version != want {
			t.Fatalf("idx %d: version=%d want %d", i, rv.Version, want)
		}
	}
	// limit=2 → 只返回最新 2 条
	got, _ = s.ListVersions(ctx, "r1", 2)
	if len(got) != 2 || got[0].Version != 5 || got[1].Version != 4 {
		t.Fatalf("limit=2 mismatch: %+v", got)
	}
}

func TestMemRuleVersionStore_Diff(t *testing.T) {
	s := NewMemRuleVersionStore()
	ctx := context.Background()
	v1, _, _ := s.Record(ctx, "r1", []byte(`{"name":"old","enabled":true}`), "", "")
	v2, _, _ := s.Record(ctx, "r1", []byte(`{"name":"new","enabled":true}`), "", "")

	d, err := s.Diff(ctx, "r1", v1, v2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d, "--- r1@1") || !strings.Contains(d, "+++ r1@2") {
		t.Fatalf("diff missing headers:\n%s", d)
	}
	if !strings.Contains(d, `-  "name": "old"`) || !strings.Contains(d, `+  "name": "new"`) {
		t.Fatalf("diff missing name change:\n%s", d)
	}
}

func TestMemRuleVersionStore_GetVersion(t *testing.T) {
	s := NewMemRuleVersionStore()
	ctx := context.Background()
	v1, _, _ := s.Record(ctx, "r1", []byte(`{"x":1}`), "first", "alice")
	rv, err := s.GetVersion(ctx, "r1", v1)
	if err != nil {
		t.Fatal(err)
	}
	if rv.RuleID != "r1" || rv.Version != v1 || rv.Author != "alice" || rv.ChangeSummary != "first" {
		t.Fatalf("version mismatch: %+v", rv)
	}
	if _, err := s.GetVersion(ctx, "r1", 999); err == nil {
		t.Fatal("get non-existent version should fail")
	}
}

func TestEngine_UpdateRuleVersioned_RecordsAndActivates(t *testing.T) {
	eng := New(zap.NewNop())
	eng.RegisterFactory("deny", func(id, name string, enabled bool, _ json.RawMessage) (Rule, error) {
		return idDenyRule{id: id}, nil
	})
	if err := eng.LoadRules([]RuleDef{
		{ID: "r1", Name: "r1", Type: "deny", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	vs := NewMemRuleVersionStore()
	eng.SetRuleVersionStore(vs)

	ctx := context.Background()
	// 模拟 admin 端：BuildRule → UpdateRuleVersioned
	d := RuleDef{ID: "r1", Name: "renamed", Type: "deny", Enabled: true}
	built, err := eng.BuildRule(d)
	if err != nil {
		t.Fatal(err)
	}
	updated, ver, isNew, err := eng.UpdateRuleVersioned(ctx, d, built, "rename", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !updated || ver != 1 || !isNew {
		t.Fatalf("first update: updated=%v ver=%d isNew=%v, want true/1/true", updated, ver, isNew)
	}
	// 同内容再发一次 → 复用 version 1，isNew=false，但 engine 内存仍然替换
	updated, ver, isNew, err = eng.UpdateRuleVersioned(ctx, d, built, "noop", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !updated || ver != 1 || isNew {
		t.Fatalf("idempotent update: updated=%v ver=%d isNew=%v, want true/1/false", updated, ver, isNew)
	}
	// GetActive 应该指向 v1
	v, spec, _ := vs.GetActive(ctx, "r1")
	if v != 1 {
		t.Fatalf("active version: got %d want 1", v)
	}
	if !strings.Contains(string(spec), `"name":"renamed"`) {
		t.Fatalf("active spec mismatch: %s", spec)
	}
}

func TestEngine_RuleSetHash_StableAndChanges(t *testing.T) {
	eng := New(zap.NewNop())
	eng.RegisterFactory("deny", func(id, name string, enabled bool, _ json.RawMessage) (Rule, error) {
		return idDenyRule{id: id}, nil
	})
	if err := eng.LoadRules([]RuleDef{
		{ID: "r1", Name: "r1", Type: "deny", Enabled: true},
		{ID: "r2", Name: "r2", Type: "deny", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	vs := NewMemRuleVersionStore()
	eng.SetRuleVersionStore(vs)
	// 给 r1 / r2 各 Record + Activate
	for _, id := range []string{"r1", "r2"} {
		v, _, _ := vs.Record(context.Background(), id, []byte(`{"v":1}`), "", "")
		_ = vs.Activate(context.Background(), id, v, "")
	}
	h1 := eng.RuleSetHash()
	if h1 == 0 {
		t.Fatal("hash should be non-zero for non-empty ruleset")
	}
	// 再算一次应该相同
	if h2 := eng.RuleSetHash(); h2 != h1 {
		t.Fatalf("hash not stable: %d vs %d", h1, h2)
	}
	// r1 进一个新版本 + activate → hash 应改变
	v, _, _ := vs.Record(context.Background(), "r1", []byte(`{"v":2}`), "", "")
	_ = vs.Activate(context.Background(), "r1", v, "")
	h3 := eng.RuleSetHash()
	if h3 == h1 {
		t.Fatal("hash should change after r1 version bump")
	}
}

// idDenyRule 复用 engine_test.go 里定义的辅助类型（同 package 编译）。

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
