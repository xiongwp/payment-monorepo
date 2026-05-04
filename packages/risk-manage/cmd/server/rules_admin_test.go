package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/engine"
)

// adminTestDenyRule 给 admin 端点测试用的最小 rule：必命中 deny。
type adminTestDenyRule struct{ id string }

func (r adminTestDenyRule) ID() string                                   { return r.id }
func (r adminTestDenyRule) Name() string                                 { return r.id }
func (r adminTestDenyRule) Type() string                                 { return "test" }
func (r adminTestDenyRule) Enabled() bool                                { return true }
func (r adminTestDenyRule) Evaluate(_ context.Context, _ *engine.TxnContext) *engine.Hit {
	return &engine.Hit{RuleID: r.id, RuleName: r.id, Decision: engine.Deny}
}

func newAdminTestEngine(t *testing.T) *engine.Engine {
	t.Helper()
	eng := engine.New(zap.NewNop())
	eng.RegisterFactory("good", func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		return adminTestDenyRule{id: id}, nil
	})
	eng.RegisterFactory("bad", func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		return nil, &adminTestErr{msg: "bad config"}
	})
	if err := eng.LoadRules([]engine.RuleDef{
		{ID: "r_seed", Name: "r_seed", Type: "good", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	return eng
}

type adminTestErr struct{ msg string }

func (e *adminTestErr) Error() string { return e.msg }

func TestAdminRulesList_ReturnsCurrentDefs(t *testing.T) {
	eng := newAdminTestEngine(t)
	mux := http.NewServeMux()
	registerRulesReload(mux, eng, viper.New(), audit.NewMemRuleAuditStore(0), zap.NewNop())

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/rules/list")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got []engine.RuleDef
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "r_seed" {
		t.Fatalf("expected [r_seed]; got %+v", got)
	}
}

func TestAdminRulesUpdate_CreateAndUpdate(t *testing.T) {
	eng := newAdminTestEngine(t)
	ra := audit.NewMemRuleAuditStore(0)
	mux := http.NewServeMux()
	registerRulesReload(mux, eng, viper.New(), ra, zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 1) create 新规则
	body, _ := json.Marshal(engine.RuleDef{ID: "r_new", Name: "n", Type: "good", Enabled: true})
	resp, _ := http.Post(srv.URL+"/admin/rules/update", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: status=%d", resp.StatusCode)
	}
	var created struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Action != "create" {
		t.Fatalf("expected action=create; got %q", created.Action)
	}

	// 2) update 已有
	body, _ = json.Marshal(engine.RuleDef{ID: "r_seed", Name: "renamed", Type: "good", Enabled: true})
	resp, _ = http.Post(srv.URL+"/admin/rules/update", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update: status=%d", resp.StatusCode)
	}
	var updated struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&updated)
	resp.Body.Close()
	if updated.Action != "update" {
		t.Fatalf("expected action=update; got %q", updated.Action)
	}
	// 审计应有 2 条 create+update（验证 actor / before/after 落进了 store）
	if got := ra.Recent(0); len(got) != 2 {
		t.Fatalf("expected 2 audit entries; got %d", len(got))
	}
}

func TestAdminRulesUpdate_RejectsInvalidConfig(t *testing.T) {
	// factory "bad" 总报错；admin 应返 400 + 不动 engine 状态。
	eng := newAdminTestEngine(t)
	mux := http.NewServeMux()
	registerRulesReload(mux, eng, viper.New(), audit.NewMemRuleAuditStore(0), zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(engine.RuleDef{ID: "r_bad", Name: "x", Type: "bad", Enabled: true})
	resp, _ := http.Post(srv.URL+"/admin/rules/update", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", resp.StatusCode)
	}
	resp.Body.Close()
	// engine 状态不应该被改
	defs := eng.RuleDefs()
	for _, d := range defs {
		if d.ID == "r_bad" {
			t.Fatal("invalid rule should not have been written to engine")
		}
	}
}

func TestAdminRulesUpdate_RejectsUnknownType(t *testing.T) {
	eng := newAdminTestEngine(t)
	mux := http.NewServeMux()
	registerRulesReload(mux, eng, viper.New(), audit.NewMemRuleAuditStore(0), zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(engine.RuleDef{ID: "r_x", Name: "x", Type: "ghost", Enabled: true})
	resp, _ := http.Post(srv.URL+"/admin/rules/update", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminRulesDelete_RemovesRule(t *testing.T) {
	eng := newAdminTestEngine(t)
	ra := audit.NewMemRuleAuditStore(0)
	mux := http.NewServeMux()
	registerRulesReload(mux, eng, viper.New(), ra, zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{"id": "r_seed", "reason": "testing"})
	resp, _ := http.Post(srv.URL+"/admin/rules/delete", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200; got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if eng.RuleCount() != 0 {
		t.Fatalf("expected 0 rules after delete; got %d", eng.RuleCount())
	}
	// 审计：1 条 delete + reason
	got := ra.Recent(0)
	if len(got) != 1 || got[0].Action != "delete" || got[0].Reason != "testing" {
		t.Fatalf("expected delete audit with reason; got %+v", got)
	}
}

func TestAdminRulesDelete_NotFound(t *testing.T) {
	eng := newAdminTestEngine(t)
	mux := http.NewServeMux()
	registerRulesReload(mux, eng, viper.New(), audit.NewMemRuleAuditStore(0), zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{"id": "ghost"})
	resp, _ := http.Post(srv.URL+"/admin/rules/delete", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404; got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
