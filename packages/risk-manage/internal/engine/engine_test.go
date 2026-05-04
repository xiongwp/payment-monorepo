package engine

import (
	"context"
	"encoding/json"
	"testing"

	"go.uber.org/zap"
)

type alwaysAllowRule struct{}

func (alwaysAllowRule) ID() string    { return "r_allow" }
func (alwaysAllowRule) Name() string  { return "always allow" }
func (alwaysAllowRule) Type() string  { return "test" }
func (alwaysAllowRule) Enabled() bool { return true }
func (alwaysAllowRule) Evaluate(_ context.Context, _ *TxnContext) *Hit { return nil }

type alwaysDenyRule struct{ detail string }

func (r alwaysDenyRule) ID() string    { return "r_deny" }
func (r alwaysDenyRule) Name() string  { return "always deny" }
func (r alwaysDenyRule) Type() string  { return "test" }
func (r alwaysDenyRule) Enabled() bool { return true }
func (r alwaysDenyRule) Evaluate(_ context.Context, _ *TxnContext) *Hit {
	return &Hit{RuleID: "r_deny", RuleName: "always deny", Decision: Deny, Detail: r.detail}
}

type reviewRule struct{}

func (reviewRule) ID() string    { return "r_review" }
func (reviewRule) Name() string  { return "review" }
func (reviewRule) Type() string  { return "test" }
func (reviewRule) Enabled() bool { return true }
func (reviewRule) Evaluate(_ context.Context, _ *TxnContext) *Hit {
	return &Hit{RuleID: "r_review", RuleName: "review", Decision: Review, Detail: "needs review"}
}

func TestEngine_AllAllow(t *testing.T) {
	eng := New(zap.NewNop())
	eng.LoadRules(nil)
	eng.mu.Lock()
	eng.rules = []Rule{alwaysAllowRule{}}
	eng.mu.Unlock()

	res := eng.Evaluate(context.Background(), &TxnContext{Amount: 1000})
	if res.Decision != Allow {
		t.Fatalf("want Allow got %s", res.Decision)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("want 0 hits got %d", len(res.Hits))
	}
}

func TestEngine_DenyWins(t *testing.T) {
	eng := New(zap.NewNop())
	eng.mu.Lock()
	eng.rules = []Rule{alwaysAllowRule{}, reviewRule{}, alwaysDenyRule{"blocked"}}
	eng.mu.Unlock()

	res := eng.Evaluate(context.Background(), &TxnContext{Amount: 1000})
	if res.Decision != Deny {
		t.Fatalf("want Deny got %s", res.Decision)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("want 2 hits (review+deny) got %d", len(res.Hits))
	}
	if res.RiskScore < 60 {
		t.Fatalf("deny+review should score >= 60, got %d", res.RiskScore)
	}
}

func TestEngine_ReviewOnly(t *testing.T) {
	eng := New(zap.NewNop())
	eng.mu.Lock()
	eng.rules = []Rule{alwaysAllowRule{}, reviewRule{}}
	eng.mu.Unlock()

	res := eng.Evaluate(context.Background(), &TxnContext{Amount: 1000})
	if res.Decision != Review {
		t.Fatalf("want Review got %s", res.Decision)
	}
}

// 模拟 yaml-loaded 规则（recipes 路径）：1 条 DENY hit + 5 条 REVIEW hit。
// 这是用户实测同 IP 批量注册场景：reg_device_interval_30s 返 DENY，其它 5 条
// 返 REVIEW。要求 engine 最终 verdict = Deny（hitLevel.Worse 让 DENY 胜出）。
//
// 之前线上audit 看到 "verdict":"REVIEW" + 6 hits 含 DENY hit + risk_score=0
// → 怀疑 LoadRules 入口某处把 hit-level DENY 吃掉。本测试就是 regression
// 用例：LoadRules + RuleDef 走完整路径，不是裸 Rule 注入。
func TestEngine_DenyHitFromYAML_PromotesVerdictToDeny(t *testing.T) {
	eng := New(zap.NewNop())
	eng.RegisterFactory("deny", func(id, name string, enabled bool, _ json.RawMessage) (Rule, error) {
		return alwaysDenyRule{detail: id}, nil
	})
	eng.RegisterFactory("review", func(id, name string, enabled bool, _ json.RawMessage) (Rule, error) {
		return reviewRule{}, nil
	})
	defs := []RuleDef{
		// 5 条 REVIEW，模拟 reg_ip_10min_5 / reg_device_3 / reg_burst_seconds /
		// multi_hop_ring / sanction_check（其 yaml 顶层 decision 字段都没填，
		// 全靠 JSON config 内的 decision 字段，所以 RuleDef.Decision="" → weight=0
		// → 走 inferWeightDecision 路径）。
		{ID: "rev1", Name: "rev1", Type: "review", Enabled: true},
		{ID: "rev2", Name: "rev2", Type: "review", Enabled: true},
		{ID: "rev3", Name: "rev3", Type: "review", Enabled: true},
		{ID: "rev4", Name: "rev4", Type: "review", Enabled: true},
		{ID: "rev5", Name: "rev5", Type: "review", Enabled: true},
		// 1 条 DENY（对应 reg_device_interval_30s）
		{ID: "deny1", Name: "deny1", Type: "deny", Enabled: true},
	}
	if err := eng.LoadRules(defs); err != nil {
		t.Fatal(err)
	}

	res := eng.Evaluate(context.Background(), &TxnContext{Amount: 0, MerchantID: "platform"})

	if res.Decision != Deny {
		t.Errorf("hit-level DENY 必须升级到 verdict-level DENY；got %s", res.Decision)
	}
	if len(res.Hits) != 6 {
		t.Errorf("want 6 hits, got %d: %+v", len(res.Hits), res.Hits)
	}
	if res.RiskScore == 0 {
		t.Errorf("score=0 with 6 hits is impossible (each hit weight ≥ 20); got %d", res.RiskScore)
	}
}

func TestWorse(t *testing.T) {
	if Worse(Allow, Allow) != Allow {
		t.Fatal()
	}
	if Worse(Allow, Review) != Review {
		t.Fatal()
	}
	if Worse(Review, Deny) != Deny {
		t.Fatal()
	}
	if Worse(Deny, Allow) != Deny {
		t.Fatal()
	}
}

func TestFactory(t *testing.T) {
	eng := New(zap.NewNop())
	called := false
	eng.RegisterFactory("test", func(id, name string, enabled bool, raw json.RawMessage) (Rule, error) {
		called = true
		return alwaysAllowRule{}, nil
	})
	eng.LoadRules([]RuleDef{{ID: "t1", Name: "t", Type: "test", Enabled: true}})
	if !called {
		t.Fatal("factory not called")
	}
	if eng.RuleCount() != 1 {
		t.Fatalf("want 1 rule got %d", eng.RuleCount())
	}
}

// shadow 模式：deny 规则配 mode=shadow → 命中只进 ShadowHits，不影响最终
// Decision（仍是 Allow）。
func TestEngine_ShadowMode_DoesNotAffectVerdict(t *testing.T) {
	eng := New(zap.NewNop())
	eng.RegisterFactory("deny", func(id, name string, enabled bool, _ json.RawMessage) (Rule, error) {
		return alwaysDenyRule{detail: "shadow test"}, nil
	})
	if err := eng.LoadRules([]RuleDef{
		{ID: "d1", Name: "deny shadow", Type: "deny", Enabled: true, Mode: "shadow"},
	}); err != nil {
		t.Fatal(err)
	}
	res := eng.Evaluate(context.Background(), &TxnContext{})
	if res.Decision != Allow {
		t.Fatalf("shadow deny should NOT change verdict; want Allow got %v", res.Decision)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("shadow hit should not enter Hits; got %d", len(res.Hits))
	}
	if len(res.ShadowHits) != 1 {
		t.Fatalf("expected 1 shadow hit, got %d", len(res.ShadowHits))
	}
	if res.ShadowHits[0].Mode != ModeShadow {
		t.Fatalf("expected shadow mode tag, got %v", res.ShadowHits[0].Mode)
	}
	if res.RiskScore != 0 {
		t.Fatalf("shadow hit should NOT add to score; got %d", res.RiskScore)
	}
}

// 同时跑一条 enforce + 一条 shadow：enforce 影响 verdict，shadow 仅观察。
func TestEngine_ShadowAndEnforce_Coexist(t *testing.T) {
	eng := New(zap.NewNop())
	eng.RegisterFactory("review", func(id, name string, enabled bool, _ json.RawMessage) (Rule, error) {
		return reviewRule{}, nil
	})
	eng.RegisterFactory("deny", func(id, name string, enabled bool, _ json.RawMessage) (Rule, error) {
		return alwaysDenyRule{detail: "shadow path"}, nil
	})
	if err := eng.LoadRules([]RuleDef{
		{ID: "rv", Name: "rv", Type: "review", Enabled: true},                    // enforce review
		{ID: "dn", Name: "dn", Type: "deny", Enabled: true, Mode: "shadow"},      // shadow deny
	}); err != nil {
		t.Fatal(err)
	}
	res := eng.Evaluate(context.Background(), &TxnContext{})
	if res.Decision != Review {
		t.Fatalf("expected Review (enforce wins; shadow ignored); got %v", res.Decision)
	}
	if len(res.Hits) != 1 || res.Hits[0].RuleID != "r_review" {
		t.Fatalf("expected only review in Hits; got %+v", res.Hits)
	}
	if len(res.ShadowHits) != 1 || res.ShadowHits[0].RuleID != "r_deny" {
		t.Fatalf("expected deny in ShadowHits; got %+v", res.ShadowHits)
	}
}

// shadow 标签大小写不敏感（"Shadow" / "SHADOW" 都视作 shadow）。
func TestEngine_ShadowMode_CaseInsensitive(t *testing.T) {
	eng := New(zap.NewNop())
	eng.RegisterFactory("deny", func(id, name string, enabled bool, _ json.RawMessage) (Rule, error) {
		return alwaysDenyRule{}, nil
	})
	for _, mode := range []string{"shadow", "Shadow", "SHADOW"} {
		eng.LoadRules([]RuleDef{{ID: "d", Name: "d", Type: "deny", Enabled: true, Mode: mode}})
		res := eng.Evaluate(context.Background(), &TxnContext{})
		if res.Decision != Allow {
			t.Fatalf("mode=%q should be shadow; got verdict %v", mode, res.Decision)
		}
		if len(res.ShadowHits) != 1 {
			t.Fatalf("mode=%q expected 1 shadow hit, got %d", mode, len(res.ShadowHits))
		}
	}
}

// idDenyRule 跟 alwaysDenyRule 同行为，但 ID() 跟 LoadRules 传入的 id 对齐
// （alwaysDenyRule 是固定 "r_deny"，无法被 SetRuleMode 按 yaml id 找到）。
type idDenyRule struct{ id string }

func (r idDenyRule) ID() string    { return r.id }
func (r idDenyRule) Name() string  { return r.id }
func (r idDenyRule) Type() string  { return "test" }
func (r idDenyRule) Enabled() bool { return true }
func (r idDenyRule) Evaluate(_ context.Context, _ *TxnContext) *Hit {
	return &Hit{RuleID: r.id, RuleName: r.id, Decision: Deny}
}

func TestSetRuleMode_TogglesShadowAndBack(t *testing.T) {
	eng := New(zap.NewNop())
	eng.RegisterFactory("deny", func(id, name string, enabled bool, _ json.RawMessage) (Rule, error) {
		return idDenyRule{id: id}, nil
	})
	if err := eng.LoadRules([]RuleDef{
		{ID: "r1", Name: "r1", Type: "deny", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	// 初始 enforce → 命中走 Decision
	res := eng.Evaluate(context.Background(), &TxnContext{})
	if res.Decision != Deny {
		t.Fatalf("enforce: expected Deny, got %s", res.Decision)
	}

	// 切 shadow → 命中只进 ShadowHits，verdict 应为 Allow
	if !eng.SetRuleMode("r1", true) {
		t.Fatal("SetRuleMode shadow=true should succeed")
	}
	res = eng.Evaluate(context.Background(), &TxnContext{})
	if res.Decision != Allow {
		t.Fatalf("shadow: verdict should be Allow, got %s", res.Decision)
	}
	if len(res.ShadowHits) != 1 || res.ShadowHits[0].RuleID != "r1" {
		t.Fatalf("shadow: expected 1 shadow hit; got %+v", res.ShadowHits)
	}

	// 切回 enforce
	if !eng.SetRuleMode("r1", false) {
		t.Fatal("SetRuleMode shadow=false should succeed")
	}
	res = eng.Evaluate(context.Background(), &TxnContext{})
	if res.Decision != Deny {
		t.Fatalf("after toggle back: expected Deny, got %s", res.Decision)
	}
}

func TestSetRuleMode_UnknownIDReturnsFalse(t *testing.T) {
	eng := New(zap.NewNop())
	if eng.SetRuleMode("ghost", true) {
		t.Fatal("unknown rule should return false")
	}
}

// TestBuildRule_UnknownTypeRejected admin /admin/rules/update 收到运营把 type
// 拼错的请求时应该 400，而不是默默 build 出垃圾规则。
func TestBuildRule_UnknownTypeRejected(t *testing.T) {
	eng := New(zap.NewNop())
	_, err := eng.BuildRule(RuleDef{ID: "x", Type: "nonexistent", Enabled: true})
	if err == nil {
		t.Fatal("unknown rule type should fail")
	}
}

// TestBuildRule_FactoryConfigErrorPropagates 工厂自身拒绝 config（json 字段错 /
// 阈值越界）时，BuildRule 应把原 error 透传给 admin 端点显示。
func TestBuildRule_FactoryConfigErrorPropagates(t *testing.T) {
	eng := New(zap.NewNop())
	eng.RegisterFactory("strict", func(id, name string, enabled bool, raw json.RawMessage) (Rule, error) {
		var cfg struct {
			Threshold int `json:"threshold"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		if cfg.Threshold <= 0 {
			return nil, errBadThreshold
		}
		return idDenyRule{id: id}, nil
	})
	_, err := eng.BuildRule(RuleDef{
		ID: "s", Type: "strict", Enabled: true,
		ConfigJSON: json.RawMessage(`{"threshold":-1}`),
	})
	if err == nil {
		t.Fatal("factory error should propagate")
	}
}

func TestUpdateRule_UpdateExistingThenCreateNew(t *testing.T) {
	eng := New(zap.NewNop())
	eng.RegisterFactory("deny", func(id, name string, enabled bool, _ json.RawMessage) (Rule, error) {
		return idDenyRule{id: id}, nil
	})
	if err := eng.LoadRules([]RuleDef{
		{ID: "r1", Name: "r1", Type: "deny", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	// update 现有
	r1, _ := eng.BuildRule(RuleDef{ID: "r1", Name: "r1-renamed", Type: "deny", Enabled: true})
	if !eng.UpdateRule(RuleDef{ID: "r1", Name: "r1-renamed", Type: "deny", Enabled: true}, r1) {
		t.Fatal("update existing should return true")
	}
	if got := eng.RuleDefs(); len(got) != 1 || got[0].Name != "r1-renamed" {
		t.Fatalf("update did not replace def: %+v", got)
	}
	// 新增
	r2, _ := eng.BuildRule(RuleDef{ID: "r2", Name: "r2", Type: "deny", Enabled: true})
	if eng.UpdateRule(RuleDef{ID: "r2", Name: "r2", Type: "deny", Enabled: true}, r2) {
		t.Fatal("inserting new id should return false (created, not updated)")
	}
	if got := eng.RuleDefs(); len(got) != 2 {
		t.Fatalf("expected 2 rules after insert; got %d", len(got))
	}
}

func TestRemoveRule(t *testing.T) {
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
	if !eng.RemoveRule("r1") {
		t.Fatal("RemoveRule existing should return true")
	}
	if eng.RemoveRule("ghost") {
		t.Fatal("RemoveRule missing should return false")
	}
	if got := eng.RuleDefs(); len(got) != 1 || got[0].ID != "r2" {
		t.Fatalf("RemoveRule left wrong state: %+v", got)
	}
}

var errBadThreshold = errStr("threshold must be > 0")

type errStr string

func (e errStr) Error() string { return string(e) }
