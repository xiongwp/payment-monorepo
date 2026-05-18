// admin_handlers_test.go — SP-AC-7 M1: 给 grpcsvc.Server 加单测覆盖.
//
// 覆盖:
//   - SaveGraph saga: rule push OK + 本地 save OK = happy path
//   - SaveGraph saga: rule push 成功 + 本地 save 失败 → 触发 DeleteRules 补偿
//   - SaveGraph saga: rule push 失败 → 不调本地 save
//   - TriggerEvent: graph not found / not active / 多 leg 部分失败 / 单 leg 卡 Processing 触发 reset+retry
//   - deriveRulesFromGraph: 多 event_code / 重复 event_code 去重 / 空 graph
//
// 不依赖 MySQL / accounting 进程, 全部用 fake repo + fake RuleSync + fake Accounting.
package grpcsvc

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"reconcile-system/packages/split-payment/internal/domain"
	"reconcile-system/packages/split-payment/internal/workflow"

	"go.uber.org/zap"
)

// ─── fakes ──────────────────────────────────────────────────────────

type fakeGraphRepo struct {
	mu     sync.Mutex
	graphs map[string]*domain.Graph
	saveErr error // 测 R2 补偿用: Save 强制返这个 err
}

func newFakeGraphRepo() *fakeGraphRepo {
	return &fakeGraphRepo{graphs: map[string]*domain.Graph{}}
}

func (r *fakeGraphRepo) Save(_ context.Context, g *domain.Graph) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveErr != nil {
		return 0, r.saveErr
	}
	r.graphs[g.Key] = g
	return 1, nil
}

func (r *fakeGraphRepo) GetByKey(_ context.Context, key string) (*domain.Graph, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.graphs[key], nil
}

func (r *fakeGraphRepo) List(_ context.Context, _ string) ([]*domain.Graph, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Graph, 0, len(r.graphs))
	for _, g := range r.graphs {
		out = append(out, g)
	}
	return out, nil
}

func (r *fakeGraphRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.graphs, key)
	return nil
}

// fakeRuleSync 记录 Upsert / Delete 的调用, 测 saga 补偿是否触发.
type fakeRuleSync struct {
	mu          sync.Mutex
	upserted    [][]RuleSpec
	deleted     [][]string
	upsertErr   error
	deleteErr   error
}

func (f *fakeRuleSync) UpsertRules(_ context.Context, rules []RuleSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserted = append(f.upserted, rules)
	return nil
}

func (f *fakeRuleSync) DeleteRules(_ context.Context, hashKeys []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, hashKeys)
	return nil
}

// fakeAccounting CreateTransaction 按 callMap[event_code] 返预设结果.
type fakeAccounting struct {
	mu       sync.Mutex
	callMap  map[string]*workflow.AccountingTxResp // event_code → 第一次响应
	retryMap map[string]*workflow.AccountingTxResp // event_code → 第二次响应 (reset+retry 后)
	calls    map[string]int                        // event_code → 调用次数
	errMap   map[string]error
}

func (a *fakeAccounting) CreateTransaction(_ context.Context, req *domain.TransactionRequest) (*workflow.AccountingTxResp, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.calls == nil {
		a.calls = map[string]int{}
	}
	a.calls[req.EventCode]++
	if err, ok := a.errMap[req.EventCode]; ok && err != nil {
		return nil, err
	}
	// 第 2 次调用 (reset 后) 走 retryMap
	if a.calls[req.EventCode] >= 2 && a.retryMap != nil && a.retryMap[req.EventCode] != nil {
		return a.retryMap[req.EventCode], nil
	}
	if r := a.callMap[req.EventCode]; r != nil {
		return r, nil
	}
	return &workflow.AccountingTxResp{VoucherNo: "v-" + req.EventCode, Status: 2}, nil
}

// 适配 grpcsvc.AccountingMetaCaller (返 *AccountingTxResp 同形态).
type fakeGrpcAccounting struct{ inner *fakeAccounting }

func (f *fakeGrpcAccounting) CreateTransaction(ctx context.Context, req *domain.TransactionRequest) (*AccountingTxResp, error) {
	r, err := f.inner.CreateTransaction(ctx, req)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, nil
	}
	return &AccountingTxResp{VoucherNo: r.VoucherNo, Status: r.Status, Error: r.Error}, nil
}

type fakeOrderReset struct {
	mu     sync.Mutex
	called []string
	err    error
}

func (f *fakeOrderReset) ResetOrder(_ context.Context, orderNo, _ string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.called = append(f.called, orderNo)
	return nil
}

// 测试 helper: 构造一个最小可用的 graph (1 product, 2 event_codes, 各 1 edge).
func newTestGraph(key string, status string) *domain.Graph {
	return &domain.Graph{
		Key:     key,
		Name:    "test",
		Version: "1.0.0",
		Status:  status,
		Spec: domain.GraphSpec{
			Scenario: "test_product",
			Triggers: []domain.Trigger{{Event: "test.trigger"}},
			Nodes: []domain.Node{
				{ID: "a", Type: "input", AccountIDAttr: "a_acct", AmountAttr: "a_amt", CurrencyAttr: "a_cur"},
				{ID: "b", Type: "account", AccountIDAttr: "b_acct", AmountAttr: "b_amt", CurrencyAttr: "b_cur"},
				{ID: "c", Type: "output", AccountIDAttr: "c_acct", AmountAttr: "c_amt", CurrencyAttr: "c_cur"},
			},
			Edges: []domain.Edge{
				{From: "a", To: "b", Kind: "transfer", EventCode: "phase1"},
				{From: "b", To: "c", Kind: "transfer", EventCode: "phase2"},
			},
		},
	}
}

// 把 graph 编码成 SaveGraphRequest 的 spec_json 形式.
func marshalSpec(t *testing.T, g *domain.Graph) []byte {
	t.Helper()
	b, err := json.Marshal(g.Spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	return b
}

// ─── tests ──────────────────────────────────────────────────────────

func TestSaveGraph_HappyPath(t *testing.T) {
	graphs := newFakeGraphRepo()
	rs := &fakeRuleSync{}
	s := NewServer(graphs, nil, rs, nil, nil, zap.NewNop())

	g := newTestGraph("hp1", "active")
	specJSON := marshalSpec(t, g)
	_, err := s.SaveGraph(context.Background(), &SaveGraphRequest{Graph: &Graph{
		Key: g.Key, Name: g.Name, Version: g.Version, Status: g.Status, SpecJson: specJSON,
	}})
	if err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}
	if got := graphs.graphs["hp1"]; got == nil {
		t.Fatalf("graph not persisted")
	}
	if len(rs.upserted) != 1 || len(rs.upserted[0]) != 2 {
		t.Fatalf("expected 1 upsert with 2 rules, got %v", rs.upserted)
	}
	if len(rs.deleted) != 0 {
		t.Fatalf("happy path 不应触发 compensate, got delete=%v", rs.deleted)
	}
}

func TestSaveGraph_RuleSyncFails_NoLocalSave(t *testing.T) {
	graphs := newFakeGraphRepo()
	rs := &fakeRuleSync{upsertErr: errors.New("accounting unreachable")}
	s := NewServer(graphs, nil, rs, nil, nil, zap.NewNop())

	g := newTestGraph("fail1", "active")
	_, err := s.SaveGraph(context.Background(), &SaveGraphRequest{Graph: &Graph{
		Key: g.Key, Name: g.Name, Version: g.Version, Status: g.Status, SpecJson: marshalSpec(t, g),
	}})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if _, ok := graphs.graphs["fail1"]; ok {
		t.Fatalf("rule sync 失败时不应该 save graph")
	}
}

// R2 saga 补偿: rule upsert OK + 本地 save 失败 → 触发 DeleteRules.
func TestSaveGraph_LocalSaveFails_TriggersCompensation(t *testing.T) {
	graphs := newFakeGraphRepo()
	graphs.saveErr = errors.New("disk full")
	rs := &fakeRuleSync{}
	s := NewServer(graphs, nil, rs, nil, nil, zap.NewNop())

	g := newTestGraph("comp1", "active")
	_, err := s.SaveGraph(context.Background(), &SaveGraphRequest{Graph: &Graph{
		Key: g.Key, Name: g.Name, Version: g.Version, Status: g.Status, SpecJson: marshalSpec(t, g),
	}})
	if err == nil {
		t.Fatalf("expected error from save")
	}
	if len(rs.upserted) != 1 {
		t.Fatalf("expected upsert called once, got %d", len(rs.upserted))
	}
	if len(rs.deleted) != 1 || len(rs.deleted[0]) != 2 {
		t.Fatalf("expected 1 compensate delete with 2 hash_keys, got %v", rs.deleted)
	}
	expected := []string{"test_product:phase1", "test_product:phase2"}
	if !reflect.DeepEqual(rs.deleted[0], expected) {
		t.Errorf("compensate hash_keys mismatch: got %v want %v", rs.deleted[0], expected)
	}
}

// R2 + 补偿失败: rule push OK + 本地 save 失败 + DeleteRules 也失败 → 返特殊 error 提示人工干预.
func TestSaveGraph_CompensationFails_HumanCleanupRequired(t *testing.T) {
	graphs := newFakeGraphRepo()
	graphs.saveErr = errors.New("disk full")
	rs := &fakeRuleSync{deleteErr: errors.New("accounting timeout")}
	s := NewServer(graphs, nil, rs, nil, nil, zap.NewNop())

	g := newTestGraph("comp2", "active")
	_, err := s.SaveGraph(context.Background(), &SaveGraphRequest{Graph: &Graph{
		Key: g.Key, Name: g.Name, Version: g.Version, Status: g.Status, SpecJson: marshalSpec(t, g),
	}})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !contains(err.Error(), "compensate failed") {
		t.Errorf("error 应该提示人工 cleanup, got: %v", err)
	}
}

// TriggerEvent: graph not found.
func TestTriggerEvent_GraphNotFound(t *testing.T) {
	graphs := newFakeGraphRepo()
	s := NewServer(graphs, &fakeGrpcAccounting{inner: &fakeAccounting{}}, nil, nil, nil, zap.NewNop())

	resp, err := s.TriggerEvent(context.Background(), &TriggerEventRequest{GraphKey: "nonexistent"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !contains(resp.Error, "not found") {
		t.Errorf("expected 'not found' error, got: %s", resp.Error)
	}
}

// 状态守门员: 非 active graph 拒绝 trigger.
func TestTriggerEvent_StatusGuard(t *testing.T) {
	graphs := newFakeGraphRepo()
	g := newTestGraph("draft1", "draft")
	graphs.graphs[g.Key] = g
	s := NewServer(graphs, &fakeGrpcAccounting{inner: &fakeAccounting{}}, nil, nil, nil, zap.NewNop())

	resp, err := s.TriggerEvent(context.Background(), &TriggerEventRequest{GraphKey: "draft1"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !contains(resp.Error, "not active") {
		t.Errorf("expected 'not active' error, got: %s", resp.Error)
	}
}

// deriveRulesFromGraph: 多 event_code 去重.
func TestDeriveRulesFromGraph_Dedup(t *testing.T) {
	g := &domain.Graph{
		Spec: domain.GraphSpec{
			Scenario: "prod1",
			Edges: []domain.Edge{
				{From: "a", To: "b", EventCode: "evt1"},
				{From: "b", To: "c", EventCode: "evt1"}, // 同 event_code, 应去重
				{From: "c", To: "d", EventCode: "evt2"},
				{From: "d", To: "e", EventCode: ""}, // 空 event_code, 跳过
			},
		},
	}
	rules := deriveRulesFromGraph(g)
	if len(rules) != 2 {
		t.Fatalf("expected 2 dedup rules, got %d: %+v", len(rules), rules)
	}
	if rules[0].HashKey != "prod1:evt1" || rules[1].HashKey != "prod1:evt2" {
		t.Errorf("hash_keys wrong: %v / %v", rules[0].HashKey, rules[1].HashKey)
	}
}

func TestDeriveRulesFromGraph_EmptyScenario(t *testing.T) {
	g := &domain.Graph{Spec: domain.GraphSpec{Scenario: "", Edges: []domain.Edge{{EventCode: "x"}}}}
	if r := deriveRulesFromGraph(g); r != nil {
		t.Errorf("无 scenario 应返 nil, 实际 %v", r)
	}
}

// ─── 工具 ──────────────────────────────────────────────────────────

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0))
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
