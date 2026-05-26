package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/engine"
)

// adminVersionsDenyRule 复用 rules_admin_test.go 同思路：随便一个能 build
// 的 rule。这里独立类型避免跨 _test.go 文件耦合。
type adminVersionsRule struct{ id string }

func (r adminVersionsRule) ID() string                                       { return r.id }
func (r adminVersionsRule) Name() string                                     { return r.id }
func (r adminVersionsRule) Type() string                                     { return "test" }
func (r adminVersionsRule) Enabled() bool                                    { return true }
func (r adminVersionsRule) Evaluate(_ context.Context, _ *engine.TxnContext) *engine.Hit {
	return nil
}

func newVersionsTestEngine(t *testing.T) (*engine.Engine, *engine.MemRuleVersionStore) {
	t.Helper()
	eng := engine.New(zap.NewNop())
	eng.RegisterFactory("good", func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		return adminVersionsRule{id: id}, nil
	})
	if err := eng.LoadRules([]engine.RuleDef{
		{ID: "r_seed", Name: "r_seed", Type: "good", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	vs := engine.NewMemRuleVersionStore()
	eng.SetRuleVersionStore(vs)
	// 给 seed rule record + activate v1
	spec, _ := json.Marshal(engine.RuleDef{ID: "r_seed", Name: "r_seed", Type: "good", Enabled: true})
	v, _, _ := vs.Record(context.Background(), "r_seed", spec, "seed", "system")
	_ = vs.Activate(context.Background(), "r_seed", v, "system")
	return eng, vs
}

// drainResp 读响应 body 转成字符串（避免重复 boilerplate）。
func drainResp(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return string(b)
}

func TestAdminVersions_UpdateRecordsVersion(t *testing.T) {
	eng, vs := newVersionsTestEngine(t)
	ra := audit.NewMemRuleAuditStore(0)
	mux := http.NewServeMux()
	registerRulesReload(mux, eng, viper.New(), ra, zap.NewNop())
	registerRuleVersionsHandlers(mux, eng, ra, zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// update：改 r_seed name → 应该写出 v2
	body, _ := json.Marshal(engine.RuleDef{ID: "r_seed", Name: "renamed", Type: "good", Enabled: true})
	resp, err := http.Post(srv.URL+"/admin/rules/update", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update: status=%d body=%s", resp.StatusCode, drainResp(t, resp))
	}
	var got struct {
		Action  string `json:"action"`
		Version int64  `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Action != "update" || got.Version != 2 {
		t.Fatalf("expected action=update version=2; got %+v", got)
	}
	// 直接查 store 验
	vList, _ := vs.ListVersions(context.Background(), "r_seed", 0)
	if len(vList) != 2 {
		t.Fatalf("expected 2 versions; got %d", len(vList))
	}
}

func TestAdminVersions_ListEndpoint(t *testing.T) {
	eng, _ := newVersionsTestEngine(t)
	ra := audit.NewMemRuleAuditStore(0)
	mux := http.NewServeMux()
	registerRulesReload(mux, eng, viper.New(), ra, zap.NewNop())
	registerRuleVersionsHandlers(mux, eng, ra, zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 加一个 v2
	body, _ := json.Marshal(engine.RuleDef{ID: "r_seed", Name: "renamed", Type: "good", Enabled: true})
	resp, _ := http.Post(srv.URL+"/admin/rules/update", "application/json", bytes.NewReader(body))
	resp.Body.Close()

	resp, err := http.Get(srv.URL + "/admin/rules/versions?rule_id=r_seed")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status=%d", resp.StatusCode)
	}
	var out struct {
		RuleID        string               `json:"rule_id"`
		ActiveVersion int64                `json:"active_version"`
		Versions      []engine.RuleVersion `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.RuleID != "r_seed" || out.ActiveVersion != 2 || len(out.Versions) != 2 {
		t.Fatalf("list mismatch: %+v", out)
	}
	if out.Versions[0].Version != 2 || out.Versions[1].Version != 1 {
		t.Fatalf("expected descending; got %d,%d", out.Versions[0].Version, out.Versions[1].Version)
	}
}

func TestAdminVersions_Rollback(t *testing.T) {
	eng, vs := newVersionsTestEngine(t)
	ra := audit.NewMemRuleAuditStore(0)
	mux := http.NewServeMux()
	registerRulesReload(mux, eng, viper.New(), ra, zap.NewNop())
	registerRuleVersionsHandlers(mux, eng, ra, zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 写 v2
	body, _ := json.Marshal(engine.RuleDef{ID: "r_seed", Name: "renamed", Type: "good", Enabled: true})
	resp, _ := http.Post(srv.URL+"/admin/rules/update", "application/json", bytes.NewReader(body))
	resp.Body.Close()
	// rollback 到 v1
	rb, _ := json.Marshal(map[string]any{"rule_id": "r_seed", "to": 1, "reason": "regression"})
	resp, err := http.Post(srv.URL+"/admin/rules/rollback", "application/json", bytes.NewReader(rb))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rollback: status=%d body=%s", resp.StatusCode, drainResp(t, resp))
	}
	resp.Body.Close()
	// active 应该 = 1
	ver, _, _ := vs.GetActive(context.Background(), "r_seed")
	if ver != 1 {
		t.Fatalf("rollback failed: active=%d want 1", ver)
	}
	// engine 内存里的 def 也应该是 v1 的 name = r_seed
	defs := eng.RuleDefs()
	if len(defs) != 1 || defs[0].Name != "r_seed" {
		t.Fatalf("engine def not rolled back: %+v", defs)
	}
	// 审计应有 rollback 行
	entries := ra.Recent(0)
	foundRollback := false
	for _, e := range entries {
		if e.Action == "rollback" && e.RuleID == "r_seed" && e.Reason == "regression" {
			foundRollback = true
		}
	}
	if !foundRollback {
		t.Fatalf("rollback audit entry missing; entries=%+v", entries)
	}
}

func TestAdminVersions_RollbackNotFound(t *testing.T) {
	eng, _ := newVersionsTestEngine(t)
	ra := audit.NewMemRuleAuditStore(0)
	mux := http.NewServeMux()
	registerRuleVersionsHandlers(mux, eng, ra, zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rb, _ := json.Marshal(map[string]any{"rule_id": "r_seed", "to": 99})
	resp, _ := http.Post(srv.URL+"/admin/rules/rollback", "application/json", bytes.NewReader(rb))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404; got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminVersions_GetVersion(t *testing.T) {
	eng, _ := newVersionsTestEngine(t)
	ra := audit.NewMemRuleAuditStore(0)
	mux := http.NewServeMux()
	registerRulesReload(mux, eng, viper.New(), ra, zap.NewNop())
	registerRuleVersionsHandlers(mux, eng, ra, zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/rules/version?rule_id=r_seed&version=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get version: status=%d", resp.StatusCode)
	}
	var rv engine.RuleVersion
	if err := json.NewDecoder(resp.Body).Decode(&rv); err != nil {
		t.Fatal(err)
	}
	if rv.Version != 1 || rv.RuleID != "r_seed" {
		t.Fatalf("version mismatch: %+v", rv)
	}
}

func TestAdminVersions_Diff(t *testing.T) {
	eng, _ := newVersionsTestEngine(t)
	ra := audit.NewMemRuleAuditStore(0)
	mux := http.NewServeMux()
	registerRulesReload(mux, eng, viper.New(), ra, zap.NewNop())
	registerRuleVersionsHandlers(mux, eng, ra, zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 写 v2
	body, _ := json.Marshal(engine.RuleDef{ID: "r_seed", Name: "renamed", Type: "good", Enabled: true})
	resp, _ := http.Post(srv.URL+"/admin/rules/update", "application/json", bytes.NewReader(body))
	resp.Body.Close()

	resp, err := http.Get(srv.URL + "/admin/rules/diff?rule_id=r_seed&from=1&to=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("diff: status=%d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	out := string(b)
	if !strings.Contains(out, "r_seed@1") || !strings.Contains(out, "r_seed@2") {
		t.Fatalf("diff missing headers:\n%s", out)
	}
	if !strings.Contains(out, "renamed") || !strings.Contains(out, "r_seed") {
		t.Fatalf("diff missing name change:\n%s", out)
	}
}

func TestAdminVersions_StoreNotConfigured(t *testing.T) {
	// engine 不挂 versionStore → endpoint 返 501
	eng := engine.New(zap.NewNop())
	eng.RegisterFactory("good", func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		return adminVersionsRule{id: id}, nil
	})
	if err := eng.LoadRules(nil); err != nil {
		t.Fatal(err)
	}
	ra := audit.NewMemRuleAuditStore(0)
	mux := http.NewServeMux()
	registerRuleVersionsHandlers(mux, eng, ra, zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/admin/rules/versions?rule_id=r_seed")
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501; got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
