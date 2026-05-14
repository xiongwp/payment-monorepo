// Package bench — 性能基准测试 + 基线追踪.
//
// 跑法:
//   make bench
//   # 或
//   go test -bench=. -benchmem -benchtime=2s ./internal/bench/
//
// 每次改动后跑一遍, diff 数字 ≥ 10% 倒退要写到 PR description 解释.
// 关键 op:
//   - CompileCache hit / miss
//   - candidate.Layer.Put (内存版,衡量 Go 代码本身开销)
//   - matcher.Registry.EvalAll (内置规则 + Starlark 规则)
//   - sha256hash (compile cache key)
package bench

import (
	"context"
	"fmt"
	"testing"
	"time"

	"reconcile-system/internal/pipeline/candidate"
	"reconcile-system/internal/pipeline/matcher"
	"reconcile-system/internal/script"
	"reconcile-system/internal/store"
)

// ─── CompileCache hit / miss benchmark ────────────────────────

func BenchmarkCompileCache_Miss(b *testing.B) {
	c := script.NewCompileCache(256)
	cs := &script.CompiledScript{ID: "x"}
	codes := make([]string, 1000)
	for i := range codes {
		codes[i] = fmt.Sprintf("def check(ctx): return [] # %d", i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("rule_%d", i%1000)
		c.Put(id, codes[i%1000], cs)
	}
}

func BenchmarkCompileCache_Hit(b *testing.B) {
	c := script.NewCompileCache(256)
	cs := &script.CompiledScript{ID: "x"}
	c.Put("rule_a", "def check(ctx): return []", cs)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get("rule_a", "def check(ctx): return []")
	}
}

// ─── candidate.Layer (memory) Put benchmark ───────────────────

func BenchmarkCandidatePut(b *testing.B) {
	l := candidate.NewMemoryLayer(candidate.Config{DefaultTriggerThreshold: 3})
	ctx := context.Background()
	evt := &store.Event{
		Service: "payment-channel", Table: "acquirer_tx",
		PK: "tx_x", Op: "INSERT", Timestamp: time.Now(),
		Indexes: map[string]string{
			"pi_id":           "pi_a",
			"idempotency_key": "idem_a",
		},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		evt.PK = fmt.Sprintf("tx_%d", i)
		evt.Indexes["pi_id"] = fmt.Sprintf("pi_%d", i%1000)
		_, _ = l.Put(ctx, evt)
	}
}

// ─── Matcher Registry.EvalAll benchmark ───────────────────────

func BenchmarkMatcherEvalAll_PresenceRule(b *testing.B) {
	reg := matcher.NewRegistry()
	reg.MustRegister(&matcher.CrossServicePresenceRule{
		RuleName: "presence",
		BizKey:   "pi_id",
		ExpectedServices: []string{
			"order-core", "payment-channel", "accounting-system",
		},
	})
	events := []*store.Event{
		{Service: "order-core", Table: "payment_intent", PK: "pi_a"},
		{Service: "payment-channel", Table: "acquirer_tx", PK: "tx_a"},
		{Service: "accounting-system", Table: "ledger_entry", PK: "le_a"},
	}
	tk := candidate.TriggerKey{BizKey: "pi_id", Value: "pi_a"}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = reg.EvalAll(ctx, tk, events)
	}
}

func BenchmarkMatcherEvalAll_AmountEqualityRule(b *testing.B) {
	reg := matcher.NewRegistry()
	reg.MustRegister(&matcher.AmountEqualityRule{
		RuleName: "amount",
		BizKey:   "pi_id",
		Pairs: []matcher.AmountPair{
			{Service: "order-core", Table: "payment_intent"},
			{Service: "payment-channel", Table: "acquirer_tx"},
			{Service: "accounting-system", Table: "ledger_entry"},
		},
	})
	events := []*store.Event{
		{Service: "order-core", Table: "payment_intent", PK: "pi_a",
			After: map[string]any{"amount": float64(1000)}},
		{Service: "payment-channel", Table: "acquirer_tx", PK: "tx_a",
			After: map[string]any{"amount": float64(1000)}},
		{Service: "accounting-system", Table: "ledger_entry", PK: "le_a",
			After: map[string]any{"amount": float64(1000)}},
	}
	tk := candidate.TriggerKey{BizKey: "pi_id", Value: "pi_a"}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = reg.EvalAll(ctx, tk, events)
	}
}

// ─── Starlark engine.Run benchmark (compile + run) ────────────

func BenchmarkStarlarkEngine_RunSimple(b *testing.B) {
	engine := script.NewEngine(1_000_000)
	cs, err := engine.Compile("simple", `
def check(ctx):
    return []
`)
	if err != nil {
		b.Fatal(err)
	}
	fs := store.NewFixtureSearcher(nil)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sctx := script.NewContext(ctx, fs, nil, nil)
		_, _ = engine.Run(ctx, cs, sctx)
	}
}

func BenchmarkStarlarkEngine_RunCheckWithLoop(b *testing.B) {
	engine := script.NewEngine(1_000_000)
	cs, err := engine.Compile("loop", `
def check(ctx):
    diffs = []
    for r in ctx.scan("payment-channel", "acquirer_tx"):
        if r.str("status") == "succeeded":
            diffs.append({"type": "ok", "key": r.pk})
    return diffs
`)
	if err != nil {
		b.Fatal(err)
	}
	// 100 条事件喂进去
	events := make([]store.Event, 100)
	for i := range events {
		events[i] = store.Event{
			Service: "payment-channel", Table: "acquirer_tx",
			PK: fmt.Sprintf("tx_%d", i),
			After: map[string]any{"status": "succeeded", "amount": float64(1000)},
			Indexes: map[string]string{"pi_id": fmt.Sprintf("pi_%d", i)},
		}
	}
	fs := store.NewFixtureSearcher(events)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sctx := script.NewContext(ctx, fs, nil, nil)
		_, _ = engine.Run(ctx, cs, sctx)
	}
}
