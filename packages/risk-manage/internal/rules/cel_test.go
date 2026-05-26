package rules

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// newCEL helper：构造一条 CEL 规则；编译失败 → t.Fatal。
func newCEL(t *testing.T, body string) engine.Rule {
	t.Helper()
	r, err := CELFactory()("c1", "cel-test", true, json.RawMessage(body))
	if err != nil {
		t.Fatalf("CELFactory build: %v", err)
	}
	return r
}

// celCfg helper：拼配置 JSON。extra 是 "key", "literal_json_value" 对。
func celCfg(expr string, extra ...string) string {
	parts := []string{`"expression":` + jsonQuote(expr)}
	for i := 0; i+1 < len(extra); i += 2 {
		parts = append(parts, jsonQuote(extra[i])+":"+extra[i+1])
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ── 基础：标量比较 ─────────────────────────────────────────────────

func TestCEL_AmountAndCountry(t *testing.T) {
	r := newCEL(t, celCfg(`amount > 50000 && customer.country == "PH"`))
	hit := r.Evaluate(context.Background(), &engine.TxnContext{Amount: 60000, Country: "PH"})
	if hit == nil {
		t.Fatal("amount=60000 country=PH should hit")
	}
	if hit.RuleID != "c1" || hit.Decision != engine.Review {
		t.Fatalf("bad hit: %+v", hit)
	}
	// 不命中分支
	if r.Evaluate(context.Background(), &engine.TxnContext{Amount: 60000, Country: "US"}) != nil {
		t.Fatal("country=US should not hit")
	}
	if r.Evaluate(context.Background(), &engine.TxnContext{Amount: 100, Country: "PH"}) != nil {
		t.Fatal("amount=100 should not hit")
	}
}

// ── 嵌套布尔 (AND/OR 混合) ─────────────────────────────────────────

func TestCEL_NestedBoolean(t *testing.T) {
	r := newCEL(t, celCfg(`(amount > 50000 || method == "card") && customer.paid_count_90d < 3`))
	cases := []struct {
		name    string
		txn     *engine.TxnContext
		wantHit bool
	}{
		{"amount-only, new-customer", &engine.TxnContext{Amount: 60000, CustomerPaidCount90d: 0}, true},
		{"card-only, new-customer", &engine.TxnContext{PaymentMethod: "card", CustomerPaidCount90d: 1}, true},
		{"amount-high, repeat-customer", &engine.TxnContext{Amount: 99999, CustomerPaidCount90d: 10}, false},
		{"low-amount, other-method", &engine.TxnContext{Amount: 100, PaymentMethod: "wire", CustomerPaidCount90d: 0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hit := r.Evaluate(context.Background(), c.txn)
			if (hit != nil) != c.wantHit {
				t.Fatalf("want hit=%v, got %+v", c.wantHit, hit)
			}
		})
	}
}

// ── 列表 in 操作 ──────────────────────────────────────────────────

func TestCEL_ListIn(t *testing.T) {
	r := newCEL(t, celCfg(`customer.country in ["RU","NG","ID"]`))
	for _, c := range []string{"RU", "NG", "ID"} {
		if r.Evaluate(context.Background(), &engine.TxnContext{Country: c}) == nil {
			t.Fatalf("country=%s should hit", c)
		}
	}
	if r.Evaluate(context.Background(), &engine.TxnContext{Country: "US"}) != nil {
		t.Fatal("country=US should not hit")
	}
}

// ── 内置函数：has() + startsWith ─────────────────────────────────

func TestCEL_HasAndStartsWith(t *testing.T) {
	r := newCEL(t, celCfg(`has(metadata.referrer) && metadata.referrer.startsWith("http://")`))
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"referrer": "http://evil.test/x"},
	})
	if hit == nil {
		t.Fatal("http referrer should hit")
	}
	// metadata key 不存在 → has() false → 整体 false（不能 crash）
	if r.Evaluate(context.Background(), &engine.TxnContext{Metadata: map[string]string{}}) != nil {
		t.Fatal("missing referrer should not hit")
	}
	// metadata 整张 map 为 nil 也不能 crash（fillActivation 兜底）
	if r.Evaluate(context.Background(), &engine.TxnContext{Metadata: nil}) != nil {
		t.Fatal("nil metadata should not hit and not panic")
	}
	// https referrer → prefix 不匹配
	if r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"referrer": "https://ok.test"},
	}) != nil {
		t.Fatal("https referrer should not hit")
	}
}

// ── 列表 comprehension（CEL macro）──────────────────────────────

func TestCEL_ListComprehension(t *testing.T) {
	// behavior.pasted_fields 至少含 card_number 或 cvc → 命中
	r := newCEL(t, celCfg(`behavior.pasted_fields.exists(f, f == "card_number" || f == "cvc")`))
	if r.Evaluate(context.Background(), &engine.TxnContext{PastedFields: []string{"email", "card_number"}}) == nil {
		t.Fatal("pasted card_number should hit")
	}
	if r.Evaluate(context.Background(), &engine.TxnContext{PastedFields: []string{"email"}}) != nil {
		t.Fatal("no sensitive paste should miss")
	}
	if r.Evaluate(context.Background(), &engine.TxnContext{PastedFields: nil}) != nil {
		t.Fatal("nil PastedFields should miss not panic")
	}
}

// ── decision: deny ──────────────────────────────────────────────

func TestCEL_DenyDecision(t *testing.T) {
	r := newCEL(t, celCfg(`amount >= 1000000`, "decision", `"deny"`, "weight", "50"))
	hit := r.Evaluate(context.Background(), &engine.TxnContext{Amount: 1000000})
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatalf("expected DENY, got %+v", hit)
	}
	if w := r.(engine.WeightedRule).Weight(); w != 50 {
		t.Fatalf("weight expected 50, got %d", w)
	}
}

// ── 拒收：表达式解析错误 ────────────────────────────────────────

func TestCEL_RejectInvalidParse(t *testing.T) {
	_, err := CELFactory()("c1", "x", true, json.RawMessage(`{"expression":"amount >>> 50"}`))
	if err == nil {
		t.Fatal("invalid syntax should fail factory build")
	}
}

func TestCEL_RejectUnknownVariable(t *testing.T) {
	// non_existent_var 没声明 → Check 阶段失败
	_, err := CELFactory()("c1", "x", true, json.RawMessage(`{"expression":"non_existent_var > 1"}`))
	if err == nil {
		t.Fatal("unknown variable should fail factory build")
	}
}

func TestCEL_RejectNonBoolReturn(t *testing.T) {
	// 表达式返 int → 拒收（必须返 bool）
	_, err := CELFactory()("c1", "x", true, json.RawMessage(`{"expression":"amount + 1"}`))
	if err == nil {
		t.Fatal("non-bool expression should fail factory build")
	}
}

func TestCEL_RejectEmptyExpression(t *testing.T) {
	_, err := CELFactory()("c1", "x", true, json.RawMessage(`{"expression":""}`))
	if err == nil {
		t.Fatal("empty expression should fail factory build")
	}
}

func TestCEL_RejectInvalidJSON(t *testing.T) {
	_, err := CELFactory()("c1", "x", true, json.RawMessage(`{not json}`))
	if err == nil {
		t.Fatal("invalid json should fail factory build")
	}
}

// ── 风险点：nil txn / 各种边缘输入不能 panic ──────────────────

func TestCEL_NilTxnContextSafe(t *testing.T) {
	r := newCEL(t, celCfg(`amount > 0`))
	if h := r.Evaluate(context.Background(), nil); h != nil {
		t.Fatal("nil txn should be a no-op, got hit")
	}
}

func TestCEL_AllZeroTxnNotPanic(t *testing.T) {
	r := newCEL(t, celCfg(`customer.country == "X" && device.id != "" && metadata.foo == "bar"`))
	if h := r.Evaluate(context.Background(), &engine.TxnContext{}); h != nil {
		t.Fatal("zero txn should not hit")
	}
}

// ── 接口契约 ─────────────────────────────────────────────────────

func TestCEL_RuleInterface(t *testing.T) {
	r := newCEL(t, celCfg(`amount > 0`))
	if r.ID() != "c1" {
		t.Fatalf("ID=%s", r.ID())
	}
	if r.Name() != "cel-test" {
		t.Fatalf("Name=%s", r.Name())
	}
	if r.Type() != "cel" {
		t.Fatalf("Type=%s", r.Type())
	}
	if !r.Enabled() {
		t.Fatal("Enabled should be true")
	}
}

func TestCEL_DisabledRule(t *testing.T) {
	r, err := CELFactory()("c1", "x", false, json.RawMessage(`{"expression":"amount > 0"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Enabled() {
		t.Fatal("Enabled should be false")
	}
	// Engine 会跳 disabled，但 Evaluate 直接调也应该返回正常结果（不抛错）
	_ = r.Evaluate(context.Background(), &engine.TxnContext{Amount: 1})
}

// ── 复杂复合规则（覆盖多个对象访问）────────────────────────────

func TestCEL_MultiObjectAccess(t *testing.T) {
	expr := `ip.proxy || ip.vpn || (device.rooted && customer.is_new_device)`
	r := newCEL(t, celCfg(expr))
	if r.Evaluate(context.Background(), &engine.TxnContext{IPProxy: true}) == nil {
		t.Fatal("proxy should hit")
	}
	if r.Evaluate(context.Background(), &engine.TxnContext{IPVPN: true}) == nil {
		t.Fatal("vpn should hit")
	}
	if r.Evaluate(context.Background(), &engine.TxnContext{DeviceRooted: true, IsNewDevice: true}) == nil {
		t.Fatal("rooted+new should hit")
	}
	if r.Evaluate(context.Background(), &engine.TxnContext{DeviceRooted: true, IsNewDevice: false}) != nil {
		t.Fatal("rooted alone should miss")
	}
}

// ── Benchmark：100K Eval 验证 program 缓存有效 ──────────────────

func BenchmarkCEL_SimpleEval(b *testing.B) {
	r, err := CELFactory()("c1", "bench", true,
		json.RawMessage(`{"expression":"amount > 50000 && customer.country == \"PH\""}`))
	if err != nil {
		b.Fatal(err)
	}
	txn := &engine.TxnContext{Amount: 60000, Country: "PH", CustomerPaidCount90d: 0}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Evaluate(ctx, txn)
	}
}

func BenchmarkCEL_NestedEval(b *testing.B) {
	r, err := CELFactory()("c1", "bench", true,
		json.RawMessage(`{"expression":"(amount > 50000 || method == \"card\") && customer.paid_count_90d < 3 && !ip.proxy"}`))
	if err != nil {
		b.Fatal(err)
	}
	txn := &engine.TxnContext{Amount: 60000, PaymentMethod: "card", CustomerPaidCount90d: 1}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Evaluate(ctx, txn)
	}
}

// Benchmark100K 显式 100K 次调用确认编译只发生一次（如果每次重新 parse，
// 该 benchmark 会暴露巨大 alloc/op；缓存正常时 alloc < 50/op）。
func BenchmarkCEL_100K(b *testing.B) {
	r, err := CELFactory()("c1", "bench", true,
		json.RawMessage(`{"expression":"customer.country in [\"RU\",\"NG\",\"ID\"]"}`))
	if err != nil {
		b.Fatal(err)
	}
	txn := &engine.TxnContext{Country: "RU"}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 100000; j++ {
			_ = r.Evaluate(ctx, txn)
		}
	}
}
