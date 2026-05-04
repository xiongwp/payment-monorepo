package ruleio

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
)

func TestMarshalUnmarshal_Roundtrip(t *testing.T) {
	in := []engine.RuleDef{
		{
			ID: "r1", Name: "n1", Type: "amount", Decision: "DENY", Enabled: true,
			Mode: "enforce", Weight: 50,
			ConfigJSON: json.RawMessage(`{"max":100}`),
			Rollout:    engine.RolloutConfig{EnablePct: 100},
		},
		{
			ID: "r2", Type: "blacklist", Enabled: false,
		},
	}
	data, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "id: r1") {
		t.Fatalf("YAML missing rule id; got:\n%s", data)
	}

	out, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 rules; got %d", len(out))
	}
	if out[0].ID != "r1" || out[0].Decision != "DENY" || out[0].Weight != 50 {
		t.Fatalf("r1 roundtrip wrong: %+v", out[0])
	}
	// config JSON 内容保留
	var cfg map[string]any
	_ = json.Unmarshal(out[0].ConfigJSON, &cfg)
	if cfg["max"].(float64) != 100 {
		t.Fatalf("config not preserved: %+v", cfg)
	}
}

func TestUnmarshal_RejectsMissingID(t *testing.T) {
	yaml := []byte(`rules:
  - type: foo
    enabled: true
`)
	if _, err := Unmarshal(yaml); err == nil {
		t.Fatal("expected error for missing id")
	}
}

func TestUnmarshal_RejectsMissingType(t *testing.T) {
	yaml := []byte(`rules:
  - id: r1
    enabled: true
`)
	if _, err := Unmarshal(yaml); err == nil {
		t.Fatal("expected error for missing type")
	}
}

func TestUnmarshal_RejectsBadConfigJSON(t *testing.T) {
	yaml := []byte(`rules:
  - id: r1
    type: amount
    config: '{not valid json'
`)
	_, err := Unmarshal(yaml)
	if err == nil || !strings.Contains(err.Error(), "valid JSON") {
		t.Fatalf("expected JSON error; got %v", err)
	}
}

func TestUnmarshal_BadYAML(t *testing.T) {
	if _, err := Unmarshal([]byte("not:valid:yaml: : :")); err == nil {
		t.Fatal("expected yaml parse error")
	}
}

func TestDiff_CreateUpdateDeleteUnchanged(t *testing.T) {
	current := []engine.RuleDef{
		{ID: "a", Type: "amount", Enabled: true},
		{ID: "b", Type: "amount", Enabled: true, Weight: 10},
		{ID: "c", Type: "amount", Enabled: true},
	}
	imported := []engine.RuleDef{
		{ID: "a", Type: "amount", Enabled: true},          // unchanged
		{ID: "b", Type: "amount", Enabled: true, Weight: 20}, // update (weight changed)
		{ID: "d", Type: "blacklist", Enabled: true},       // create
		// c missing → delete
	}
	got := Diff(current, imported)
	actions := map[string]string{}
	for _, e := range got {
		actions[e.RuleID] = e.Action
	}
	want := map[string]string{
		"a": "unchanged",
		"b": "update",
		"c": "delete",
		"d": "create",
	}
	for id, exp := range want {
		if actions[id] != exp {
			t.Fatalf("rule %s: expected %s; got %s", id, exp, actions[id])
		}
	}
}

func TestDiff_ConfigJSONWhitespaceIgnored(t *testing.T) {
	current := []engine.RuleDef{
		{ID: "a", Type: "amount", Enabled: true,
			ConfigJSON: json.RawMessage(`{"max": 100,"min": 0}`)},
	}
	imported := []engine.RuleDef{
		// 同样的 config，但格式不同（key 顺序换 + 多空格）
		{ID: "a", Type: "amount", Enabled: true,
			ConfigJSON: json.RawMessage(`{ "min" : 0, "max" : 100 }`)},
	}
	got := Diff(current, imported)
	if len(got) != 1 || got[0].Action != "unchanged" {
		t.Fatalf("expected unchanged for equivalent JSON; got %+v", got)
	}
}

func TestMarshal_OmitsEmptyConfig(t *testing.T) {
	in := []engine.RuleDef{{ID: "x", Type: "y", Enabled: true}}
	data, _ := Marshal(in)
	if strings.Contains(string(data), "config:") {
		t.Fatalf("empty config should be omitted; got:\n%s", data)
	}
}
