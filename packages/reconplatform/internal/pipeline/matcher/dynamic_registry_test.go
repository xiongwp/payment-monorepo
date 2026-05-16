package matcher

import (
	"context"
	"testing"

	"reconcile-system/internal/pipeline/candidate"
	"reconcile-system/internal/script"
	"reconcile-system/internal/store"
)

const happyStar = `
def check(ctx):
    return []
`

const findDupStar = `
def check(ctx):
    diffs = []
    seen = {}
    rows = ctx.scan("payment-channel", "acquirer_tx", 100)
    for r in rows:
        idk = r.after.get("idempotency_key", "")
        if idk == "":
            continue
        if idk in seen:
            diffs.append({
                "type": "duplicate_charge",
                "key": idk,
                "detail": {"count": 2}
            })
        else:
            seen[idk] = 1
    return diffs
`

const brokenStar = `
def check(ctx):
    this_is_not_valid_syntax!!
`

func TestDynamicRegistry_Compile(t *testing.T) {
	d := NewDynamicRegistry(script.NewEngine(1_000_000))
	if err := d.Compile("happy", happyStar, "pi_id"); err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := d.List(); len(got) != 1 || got[0] != "happy" {
		t.Errorf("list wrong: %v", got)
	}
	// 重复注册 → error
	if err := d.Compile("happy", happyStar, "pi_id"); err == nil {
		t.Error("expect error on duplicate")
	}
}

func TestDynamicRegistry_CompileBrokenSyntax(t *testing.T) {
	d := NewDynamicRegistry(script.NewEngine(1_000_000))
	if err := d.Compile("bad", brokenStar, "pi_id"); err == nil {
		t.Error("expect compile error for broken syntax")
	}
	if len(d.List()) != 0 {
		t.Error("failed compile should not register")
	}
}

func TestDynamicRegistry_Replace(t *testing.T) {
	d := NewDynamicRegistry(script.NewEngine(1_000_000))
	_ = d.Compile("rule1", happyStar, "pi_id")
	if err := d.Replace("rule1", findDupStar, "pi_id"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := d.List(); len(got) != 1 {
		t.Error("after replace should still have 1 rule")
	}
}

func TestDynamicRegistry_ReplaceBrokenSyntax_KeepsOld(t *testing.T) {
	d := NewDynamicRegistry(script.NewEngine(1_000_000))
	_ = d.Compile("rule1", happyStar, "pi_id")
	// 试图用坏代码替换 → 应失败,旧规则保留
	if err := d.Replace("rule1", brokenStar, "pi_id"); err == nil {
		t.Error("expect compile error")
	}
	if len(d.List()) != 1 {
		t.Error("old rule should remain after failed replace")
	}
}

func TestDynamicRegistry_Remove(t *testing.T) {
	d := NewDynamicRegistry(script.NewEngine(1_000_000))
	_ = d.Compile("rule1", happyStar, "pi_id")
	if err := d.Remove("rule1"); err != nil {
		t.Fatal(err)
	}
	if len(d.List()) != 0 {
		t.Error("should be empty after remove")
	}
	if err := d.Remove("rule1"); err == nil {
		t.Error("remove non-existent should error")
	}
}

func TestDynamicRegistry_EndToEnd_StarlarkRule(t *testing.T) {
	d := NewDynamicRegistry(script.NewEngine(1_000_000))
	if err := d.Compile("dup", findDupStar, ""); err != nil {
		t.Fatal(err)
	}
	// 喂 2 条 idempotency_key 相同的 charge
	events := []*store.Event{
		{Service: "payment-channel", Table: "acquirer_tx", PK: "a",
			After: map[string]any{"idempotency_key": "idem_dup"}},
		{Service: "payment-channel", Table: "acquirer_tx", PK: "b",
			After: map[string]any{"idempotency_key": "idem_dup"}},
	}
	res := d.EvalAll(context.Background(),
		candidate.TriggerKey{BizKey: "idempotency_key", Value: "idem_dup"},
		events)
	if len(res) != 1 {
		t.Fatalf("want 1 result, got %d", len(res))
	}
	if res[0].Verdict != VerdictMismatched {
		t.Errorf("non-empty diffs should be mismatched, got %s", res[0].Verdict)
	}
}
