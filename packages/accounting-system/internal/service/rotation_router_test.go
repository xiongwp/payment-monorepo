package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// 方向 B Fakes
// ============================================================================

type fakeLogicalReader struct {
	byKey  map[string]*model.LogicalAccount
	byID   map[int64]*model.LogicalAccount
	errKey string
	calls  int32
}

func (f *fakeLogicalReader) GetByKey(_ context.Context, key string) (*model.LogicalAccount, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.errKey == key {
		return nil, errors.New("simulated GetByKey error")
	}
	la, ok := f.byKey[key]
	if !ok {
		return nil, fmt.Errorf("%w: %q", model.ErrLogicalAccountNotRegistered, key)
	}
	return la, nil
}
func (f *fakeLogicalReader) GetByID(_ context.Context, id int64) (*model.LogicalAccount, error) {
	return f.byID[id], nil
}
func (f *fakeLogicalReader) callCount() int32 { return atomic.LoadInt32(&f.calls) }

// 方向 B 的 anchor reader：按 (flow_id, account_no) 索引
type fakeAnchorReaderB struct {
	// key: "{flow_id}|{account_no}" → anchor
	byKey map[string]*model.TxAccountAnchor
	err   error
}

func anchorKeyB(flowID, accountNo string) string {
	return flowID + "|" + accountNo
}

func (f *fakeAnchorReaderB) GetByFlowAndAccount(_ context.Context, flowID, accountNo string) (*model.TxAccountAnchor, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byKey[anchorKeyB(flowID, accountNo)], nil
}
func (f *fakeAnchorReaderB) RouteByAccountNo(accountNo string) (int, int) {
	sum := 0
	for _, c := range accountNo {
		sum += int(c)
	}
	gtbl := sum % 100
	return gtbl / 10, gtbl
}

// 方向 B 的 route reader：按 (flow_id, logical_account_id) 索引
type fakeRouteReader struct {
	byKey map[string]*model.FlowAnchorRoute
	err   error
}

func routeKey(flowID string, laID int64) string {
	return fmt.Sprintf("%s|%d", flowID, laID)
}

func (f *fakeRouteReader) GetByFlowAndLogical(_ context.Context, flowID string, laID int64) (*model.FlowAnchorRoute, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byKey[routeKey(flowID, laID)], nil
}
func (f *fakeRouteReader) RouteByFlowID(flowID string) (int, int) {
	sum := 0
	for _, c := range flowID {
		sum += int(c)
	}
	gtbl := sum % 100
	return gtbl / 10, gtbl
}

type fakeAccountReader struct {
	byNo map[string]*model.Account
	err  error
}

func (f *fakeAccountReader) GetByAccountNo(_ context.Context, no string) (*model.Account, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byNo[no], nil
}

type fakeTransactionReader struct {
	flowByTxID map[string]string
	err        error
}

func (f *fakeTransactionReader) GetFlowIDByTransactionID(_ context.Context, txID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.flowByTxID[txID], nil
}

// 工厂：标准 fixture（不带 transactionReader → 方向 2 拒绝）
func newRouterFixture(now time.Time) (
	Router, *fakeLogicalReader, *fakeAnchorReaderB, *fakeRouteReader, *fakeAccountReader,
) {
	lr := &fakeLogicalReader{
		byKey: make(map[string]*model.LogicalAccount),
		byID:  make(map[int64]*model.LogicalAccount),
	}
	ar := &fakeAnchorReaderB{byKey: make(map[string]*model.TxAccountAnchor)}
	rr := &fakeRouteReader{byKey: make(map[string]*model.FlowAnchorRoute)}
	accR := &fakeAccountReader{byNo: make(map[string]*model.Account)}
	r := NewRouter(lr, ar, rr, accR, nil, func() time.Time { return now })
	return r, lr, ar, rr, accR
}

// 带 TransactionReader 的 fixture（用于方向 2）
func newRouterFixtureWithTx(now time.Time, tx *fakeTransactionReader) (
	Router, *fakeLogicalReader, *fakeAnchorReaderB, *fakeRouteReader, *fakeAccountReader,
) {
	lr := &fakeLogicalReader{
		byKey: make(map[string]*model.LogicalAccount),
		byID:  make(map[int64]*model.LogicalAccount),
	}
	ar := &fakeAnchorReaderB{byKey: make(map[string]*model.TxAccountAnchor)}
	rr := &fakeRouteReader{byKey: make(map[string]*model.FlowAnchorRoute)}
	accR := &fakeAccountReader{byNo: make(map[string]*model.Account)}
	r := NewRouter(lr, ar, rr, accR, tx, func() time.Time { return now })
	return r, lr, ar, rr, accR
}

func mkLogicalAccount(id int64, key string, rotating bool, currentActive string) *model.LogicalAccount {
	la := &model.LogicalAccount{
		ID:                  id,
		LogicalAccountKey:   key,
		AccountType:         model.AccountTypeTransitChannelPayable,
		AccountBusinessType: 101,
		Currency:            "USD",
		Status:              model.LogicalAccountStatusEnabled,
		RegisteredBy:        "ops",
	}
	if rotating {
		la.RotationEnabled = 1
	}
	if currentActive != "" {
		la.CurrentActiveAccountNo = &currentActive
	}
	return la
}

func mkAccountActive(accNo string, laID int64) *model.Account {
	la := laID
	return &model.Account{
		AccountNo: accNo, LogicalAccountID: &la,
		LifecyclePhase: model.LifecyclePhaseActive,
	}
}
func mkAccountPhase(accNo string, laID int64, phase model.LifecyclePhase) *model.Account {
	la := laID
	return &model.Account{
		AccountNo: accNo, LogicalAccountID: &la,
		LifecyclePhase: phase,
	}
}

func mkRequest(key, flowID string, dir BookingDirection) *ResolveRequest {
	return &ResolveRequest{
		LogicalAccountKey: key,
		FlowID:            flowID,
		Direction:         dir,
		OccurredAt:        time.Now(),
		BookingType:       BookingTypeNormal,
	}
}

func ptr64B(v int64) *int64           { return &v }
func ptrTimeB(v time.Time) *time.Time { return &v }

var _ = ptr64B
var _ = ptrTimeB

// ============================================================================
// 输入校验
// ============================================================================

func TestResolveRequest_Validate(t *testing.T) {
	cases := []struct {
		name   string
		req    *ResolveRequest
		wantOK bool
	}{
		{"nil", nil, false},
		{"empty all", &ResolveRequest{}, false},
		{"missing key", &ResolveRequest{FlowID: "x", Direction: BookingDirectionDebit}, false},
		{"missing flow", &ResolveRequest{LogicalAccountKey: "k", Direction: BookingDirectionDebit}, false},
		{"invalid direction 0", &ResolveRequest{LogicalAccountKey: "k", FlowID: "x", Direction: 0}, false},
		{"invalid direction 3", &ResolveRequest{LogicalAccountKey: "k", FlowID: "x", Direction: 3}, false},
		{"happy debit", &ResolveRequest{LogicalAccountKey: "k", FlowID: "x", Direction: BookingDirectionDebit}, true},
		{"happy credit", &ResolveRequest{LogicalAccountKey: "k", FlowID: "x", Direction: BookingDirectionCredit}, true},
		{"both Original set", &ResolveRequest{
			LogicalAccountKey: "k", FlowID: "x", Direction: 1,
			OriginalFlowID: "y", OriginalTransactionID: "z",
			ReuseSource: model.AnchorReuseSourceRefundOf,
		}, false},
		{"OriginalFlow without ReuseSource", &ResolveRequest{
			LogicalAccountKey: "k", FlowID: "x", Direction: 1,
			OriginalFlowID: "y",
		}, false},
		{"OriginalFlow + RefundOf OK", &ResolveRequest{
			LogicalAccountKey: "k", FlowID: "x", Direction: 1,
			OriginalFlowID: "y",
			ReuseSource:    model.AnchorReuseSourceRefundOf,
		}, true},
		{"OriginalTransactionID + ReverseOf OK", &ResolveRequest{
			LogicalAccountKey: "k", FlowID: "x", Direction: 1,
			OriginalTransactionID: "z",
			ReuseSource:           model.AnchorReuseSourceReverseOf,
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.req.Validate()
			if c.wantOK && err != nil {
				t.Errorf("expected ok, got %v", err)
			}
			if !c.wantOK && err == nil {
				t.Error("expected error")
			}
		})
	}
}

// ============================================================================
// Logical account loading
// ============================================================================

func TestResolve_LogicalAccountNotRegistered(t *testing.T) {
	r, _, _, _, _ := newRouterFixture(time.Now())
	req := mkRequest("transit:none:USD", "F1", BookingDirectionDebit)
	_, err := r.Resolve(context.Background(), req)
	if !errors.Is(err, model.ErrLogicalAccountNotRegistered) {
		t.Errorf("expected ErrLogicalAccountNotRegistered, got %v", err)
	}
}

func TestResolve_LegacyLAReturnsImmediately(t *testing.T) {
	r, lr, _, _, _ := newRouterFixture(time.Now())
	lr.byKey["transit:legacy:USD"] = mkLogicalAccount(1, "transit:legacy:USD", false, "")
	res, err := r.Resolve(context.Background(), mkRequest("transit:legacy:USD", "F1", BookingDirectionDebit))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsLegacy {
		t.Fatal("legacy LA should set IsLegacy=true")
	}
	if res.AnchorPlan != nil || res.RoutePlan != nil {
		t.Error("legacy must produce no plans")
	}
}

func TestResolve_LogicalAccountReadError(t *testing.T) {
	r, lr, _, _, _ := newRouterFixture(time.Now())
	lr.errKey = "transit:foo:USD"
	_, err := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F1", BookingDirectionDebit))
	if err == nil {
		t.Fatal("LA read error must propagate")
	}
}

// ============================================================================
// Cache 行为
// ============================================================================

func TestResolve_CacheHits(t *testing.T) {
	r, lr, _, _, _ := newRouterFixture(time.Now())
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(1, "transit:foo:USD", false, "")
	for i := 0; i < 5; i++ {
		_, _ = r.Resolve(context.Background(), mkRequest("transit:foo:USD", fmt.Sprintf("F%d", i), BookingDirectionDebit))
	}
	if lr.callCount() != 1 {
		t.Errorf("expected 1 GetByKey, got %d", lr.callCount())
	}
}

func TestResolve_CacheSingleflight(t *testing.T) {
	r, lr, _, _, _ := newRouterFixture(time.Now())
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(1, "transit:foo:USD", false, "")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = r.Resolve(context.Background(), mkRequest("transit:foo:USD", fmt.Sprintf("F%d", i), BookingDirectionDebit))
		}(i)
	}
	wg.Wait()
	if lr.callCount() != 1 {
		t.Errorf("singleflight failed: expected 1 GetByKey, got %d", lr.callCount())
	}
}

// ============================================================================
// 首次锚定 — 新 flow，routing 不存在
// ============================================================================

func TestResolve_NewFlow_FirstAnchor(t *testing.T) {
	now := time.Now()
	r, lr, _, _, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A001")
	accR.byNo["A001"] = mkAccountActive("A001", 42)

	res, err := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F_NEW", BookingDirectionCredit))
	if err != nil {
		t.Fatal(err)
	}

	if res.IsLegacy {
		t.Fatal("rotating LA should not be legacy")
	}
	if res.AccountNo != "A001" {
		t.Errorf("expected A001, got %q", res.AccountNo)
	}
	if res.AnchorPlan == nil || res.AnchorPlan.Op != AnchorOpInsert {
		t.Fatal("expected Insert anchor plan")
	}
	if res.RoutePlan == nil || res.RoutePlan.Op != RouteOpInsert {
		t.Fatal("expected Insert route plan")
	}
	if res.RoutePlan.NewRoute.FlowID != "F_NEW" {
		t.Errorf("route flow_id should be F_NEW, got %s", res.RoutePlan.NewRoute.FlowID)
	}
	if res.RoutePlan.NewRoute.AccountNo != "A001" {
		t.Errorf("route account_no should be A001, got %s", res.RoutePlan.NewRoute.AccountNo)
	}
	if res.AnchorPlan.NewAnchor.AccountNo != "A001" {
		t.Errorf("anchor account_no should be A001, got %s", res.AnchorPlan.NewAnchor.AccountNo)
	}
	// 校验两个分片号一致性（fake 算法）
	expectedAnchorShard := simpleSumHash("A001")
	if res.AnchorGlobalTableIndex != expectedAnchorShard {
		t.Errorf("anchor shard mismatch")
	}
	expectedRouteShard := simpleSumHash("F_NEW")
	if res.RouteGlobalTableIndex != expectedRouteShard {
		t.Errorf("route shard mismatch")
	}
}

func TestResolve_NewFlow_NoActiveInstance(t *testing.T) {
	now := time.Now()
	r, lr, _, _, _ := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "")
	_, err := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F1", BookingDirectionDebit))
	if !errors.Is(err, model.ErrNoActiveInstance) {
		t.Errorf("expected ErrNoActiveInstance, got %v", err)
	}
}

func TestResolve_NewFlow_CurrentActiveNotFound(t *testing.T) {
	now := time.Now()
	r, lr, _, _, _ := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A001")
	// accR.byNo["A001"] 没设置
	_, err := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F1", BookingDirectionDebit))
	if !errors.Is(err, model.ErrNoActiveInstance) {
		t.Errorf("expected ErrNoActiveInstance, got %v", err)
	}
}

func TestResolve_NewFlow_CurrentActiveStale(t *testing.T) {
	now := time.Now()
	r, lr, _, _, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A001")
	accR.byNo["A001"] = mkAccountPhase("A001", 42, model.LifecyclePhaseDraining) // 不再 active

	_, err := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F1", BookingDirectionDebit))
	if !errors.Is(err, model.ErrNoActiveInstance) {
		t.Errorf("expected ErrNoActiveInstance for stale active phase, got %v", err)
	}
}

// ============================================================================
// 已有 flow — routing 存在
// ============================================================================

func TestResolve_ExistingFlow_UpdatePlan(t *testing.T) {
	now := time.Now()
	r, lr, ar, rr, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A002")
	accR.byNo["A001"] = mkAccountPhase("A001", 42, model.LifecyclePhaseDraining)

	// 已有 routing: F1 → A001
	rr.byKey[routeKey("F1", 42)] = &model.FlowAnchorRoute{
		ID: 10, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
	}
	// 已有 anchor
	ar.byKey[anchorKeyB("F1", "A001")] = &model.TxAccountAnchor{
		ID: 100, FlowID: "F1", LogicalAccountID: 42,
		AccountNo: "A001", Status: model.AnchorStatusActive,
		DirectionMask: model.AnchorDirectionDebit, Version: 3,
	}

	res, err := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F1", BookingDirectionCredit))
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountNo != "A001" {
		t.Errorf("should follow routing to A001, got %q", res.AccountNo)
	}
	if res.AnchorPlan.Op != AnchorOpUpdatePosting {
		t.Errorf("expected UpdatePosting")
	}
	if res.AnchorPlan.UpdateTargetID != 100 || res.AnchorPlan.UpdateExpectedVersion != 3 {
		t.Errorf("update target wrong")
	}
	if res.RoutePlan != nil {
		t.Error("RoutePlan should be nil (routing already exists)")
	}
	// mask 应该 = debit | credit
	if !res.AnchorPlan.UpdateNewMask.HasDebit() || !res.AnchorPlan.UpdateNewMask.HasCredit() {
		t.Errorf("mask should be debit|credit")
	}
}

// 跨期：3 天 flow 跨过轮换，仍落原 instance（I0 不变量）
func TestResolve_FlowConsistency_CrossPeriod(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 6, d, 12, 0, 0, 0, time.UTC) }
	r, lr, ar, rr, accR := newRouterFixture(day(1))
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(7, "transit:foo:USD", true, "A001")
	accR.byNo["A001"] = mkAccountActive("A001", 7)

	// Day 1: 首次锚定
	res1, _ := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F_X", BookingDirectionCredit))
	if res1.AccountNo != "A001" {
		t.Fatalf("Day1 routes to A001, got %s", res1.AccountNo)
	}
	// caller 落 route + anchor
	rr.byKey[routeKey("F_X", 7)] = res1.RoutePlan.NewRoute
	rr.byKey[routeKey("F_X", 7)].ID = 1001
	ar.byKey[anchorKeyB("F_X", "A001")] = res1.AnchorPlan.NewAnchor
	ar.byKey[anchorKeyB("F_X", "A001")].ID = 2001

	// Day 3: 轮换 — active 变 A002，A001 进 draining
	la := lr.byKey["transit:foo:USD"]
	a002 := "A002"
	la.CurrentActiveAccountNo = &a002
	accR.byNo["A001"] = mkAccountPhase("A001", 7, model.LifecyclePhaseDraining)
	accR.byNo["A002"] = mkAccountActive("A002", 7)
	if impl, ok := r.(*router); ok {
		impl.cache.invalidate("transit:foo:USD")
		impl.clock = func() time.Time { return day(3) }
	}

	// Day 3: 同 flow 继续操作 — 必须仍落 A001（routing 锁着）
	res2, err := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F_X", BookingDirectionDebit))
	if err != nil {
		t.Fatal(err)
	}
	if res2.AccountNo != "A001" {
		t.Fatalf("FLOW CONSISTENCY VIOLATION: expected A001, got %s", res2.AccountNo)
	}
	if res2.AnchorPlan.Op != AnchorOpUpdatePosting {
		t.Error("should be UpdatePosting on existing anchor")
	}
}

// ============================================================================
// Phase 守卫
// ============================================================================

func TestResolve_PhaseGuard_FrozenRejectsNormal(t *testing.T) {
	now := time.Now()
	r, lr, ar, rr, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A002")
	accR.byNo["A001"] = mkAccountPhase("A001", 42, model.LifecyclePhaseFrozen)

	rr.byKey[routeKey("F1", 42)] = &model.FlowAnchorRoute{
		ID: 1, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
	}
	ar.byKey[anchorKeyB("F1", "A001")] = &model.TxAccountAnchor{
		ID: 1, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
		Status: model.AnchorStatusActive,
	}

	req := mkRequest("transit:foo:USD", "F1", BookingDirectionDebit)
	req.BookingType = BookingTypeNormal
	_, err := r.Resolve(context.Background(), req)
	if !errors.Is(err, model.ErrPhaseGuardRejected) {
		t.Errorf("frozen+normal should be rejected, got %v", err)
	}
}

func TestResolve_PhaseGuard_FrozenAllowsTCCCancel(t *testing.T) {
	now := time.Now()
	r, lr, ar, rr, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A002")
	accR.byNo["A001"] = mkAccountPhase("A001", 42, model.LifecyclePhaseFrozen)

	rr.byKey[routeKey("F1", 42)] = &model.FlowAnchorRoute{
		ID: 1, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
	}
	ar.byKey[anchorKeyB("F1", "A001")] = &model.TxAccountAnchor{
		ID: 1, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
		Status: model.AnchorStatusTrying,
	}

	req := mkRequest("transit:foo:USD", "F1", BookingDirectionDebit)
	req.BookingType = BookingTypeTCCCancel
	res, err := r.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("frozen TCC Cancel should pass: %v", err)
	}
	if res.AccountNo != "A001" {
		t.Errorf("must route to frozen instance, got %s", res.AccountNo)
	}
}

func TestResolve_PhaseGuard_ArchivedAlwaysRejects(t *testing.T) {
	now := time.Now()
	r, lr, ar, rr, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A002")
	accR.byNo["A001"] = mkAccountPhase("A001", 42, model.LifecyclePhaseArchived)

	rr.byKey[routeKey("F1", 42)] = &model.FlowAnchorRoute{
		ID: 1, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
	}
	ar.byKey[anchorKeyB("F1", "A001")] = &model.TxAccountAnchor{
		ID: 1, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
		Status: model.AnchorStatusActive,
	}
	for _, bt := range []BookingType{
		BookingTypeNormal, BookingTypeTCCTry, BookingTypeTCCConfirm, BookingTypeTCCCancel,
	} {
		req := mkRequest("transit:foo:USD", "F1", BookingDirectionDebit)
		req.BookingType = bt
		_, err := r.Resolve(context.Background(), req)
		if !errors.Is(err, model.ErrPhaseGuardRejected) {
			t.Errorf("archived should reject bt=%d, got %v", bt, err)
		}
	}
}

// ============================================================================
// 退款继承（方向 1 + 方向 2）
// ============================================================================

func TestResolve_RefundInheritsSourceAccount_Direction1(t *testing.T) {
	now := time.Now()
	r, lr, _, rr, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A002")
	accR.byNo["A001"] = mkAccountPhase("A001", 42, model.LifecyclePhaseDraining)

	// 源 flow 锁在 A001
	rr.byKey[routeKey("PAY_ORIG", 42)] = &model.FlowAnchorRoute{
		ID: 10, FlowID: "PAY_ORIG", LogicalAccountID: 42, AccountNo: "A001",
	}

	req := &ResolveRequest{
		LogicalAccountKey: "transit:foo:USD",
		FlowID:            "RFD_NEW",
		Direction:         BookingDirectionDebit,
		OriginalFlowID:    "PAY_ORIG",
		ReuseSource:       model.AnchorReuseSourceRefundOf,
		BookingType:       BookingTypeNormal,
	}
	res, err := r.Resolve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountNo != "A001" {
		t.Errorf("refund should inherit source's instance A001, got %s", res.AccountNo)
	}
	if res.AnchorPlan.NewAnchor.ReuseSource != model.AnchorReuseSourceRefundOf {
		t.Errorf("ReuseSource should be RefundOf")
	}
	if res.RoutePlan == nil {
		t.Error("new flow should have RoutePlan")
	}
	if res.RoutePlan.NewRoute.AccountNo != "A001" {
		t.Errorf("new route should point to source's A001")
	}
}

func TestResolve_RefundDirection2_TxIDResolves(t *testing.T) {
	now := time.Now()
	tx := &fakeTransactionReader{flowByTxID: map[string]string{"TX_ORIG_X": "PAY_ORIG"}}
	r, lr, _, rr, accR := newRouterFixtureWithTx(now, tx)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A002")
	accR.byNo["A001"] = mkAccountPhase("A001", 42, model.LifecyclePhaseDraining)

	rr.byKey[routeKey("PAY_ORIG", 42)] = &model.FlowAnchorRoute{
		ID: 10, FlowID: "PAY_ORIG", LogicalAccountID: 42, AccountNo: "A001",
	}

	req := &ResolveRequest{
		LogicalAccountKey:     "transit:foo:USD",
		FlowID:                "RED_NEW",
		Direction:             BookingDirectionCredit,
		OriginalTransactionID: "TX_ORIG_X",
		ReuseSource:           model.AnchorReuseSourceReverseOf,
		BookingType:           BookingTypeNormal,
	}
	res, err := r.Resolve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountNo != "A001" {
		t.Errorf("direction 2 should resolve to source flow's A001, got %s", res.AccountNo)
	}
}

func TestResolve_Direction2_TxNotFound(t *testing.T) {
	now := time.Now()
	tx := &fakeTransactionReader{flowByTxID: map[string]string{}}
	r, lr, _, _, accR := newRouterFixtureWithTx(now, tx)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A001")
	accR.byNo["A001"] = mkAccountActive("A001", 42)

	req := &ResolveRequest{
		LogicalAccountKey:     "transit:foo:USD",
		FlowID:                "F1",
		Direction:             BookingDirectionDebit,
		OriginalTransactionID: "TX_NONE",
		ReuseSource:           model.AnchorReuseSourceReverseOf,
	}
	_, err := r.Resolve(context.Background(), req)
	if err == nil {
		t.Fatal("non-existent tx_id should error")
	}
	if !strings.Contains(err.Error(), "source transaction not found") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestResolve_Direction2_RequiresTransactionReader(t *testing.T) {
	now := time.Now()
	r, lr, _, _, accR := newRouterFixture(now) // 不带 tx reader
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A001")
	accR.byNo["A001"] = mkAccountActive("A001", 42)
	req := &ResolveRequest{
		LogicalAccountKey:     "transit:foo:USD",
		FlowID:                "F1",
		Direction:             BookingDirectionDebit,
		OriginalTransactionID: "TX_X",
		ReuseSource:           model.AnchorReuseSourceReverseOf,
	}
	_, err := r.Resolve(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "TransactionReader not configured") {
		t.Errorf("expected TransactionReader not configured, got %v", err)
	}
}

func TestResolve_RefundSourceFlowNotFound(t *testing.T) {
	now := time.Now()
	r, lr, _, _, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A001")
	accR.byNo["A001"] = mkAccountActive("A001", 42)
	// 没有 routing for "MISSING_SRC"

	req := &ResolveRequest{
		LogicalAccountKey: "transit:foo:USD",
		FlowID:            "RFD_NEW",
		Direction:         BookingDirectionDebit,
		OriginalFlowID:    "MISSING_SRC",
		ReuseSource:       model.AnchorReuseSourceRefundOf,
	}
	_, err := r.Resolve(context.Background(), req)
	if err == nil {
		t.Fatal("source flow without routing should error")
	}
	if !strings.Contains(err.Error(), "has no anchor") {
		t.Errorf("wrong error: %v", err)
	}
}

// 互斥：OriginalFlowID + OriginalTransactionID 同时设置
func TestResolve_MutexOriginalFields(t *testing.T) {
	r, _, _, _, _ := newRouterFixture(time.Now())
	req := &ResolveRequest{
		LogicalAccountKey:     "transit:foo:USD",
		FlowID:                "F_NEW",
		Direction:             BookingDirectionDebit,
		OriginalFlowID:        "X",
		OriginalTransactionID: "Y",
		ReuseSource:           model.AnchorReuseSourceRefundOf,
	}
	_, err := r.Resolve(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutex error, got %v", err)
	}
}

// ============================================================================
// 不变量：分片号正确性
// ============================================================================

func TestResolve_ShardConsistency(t *testing.T) {
	now := time.Now()
	r, lr, _, _, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A001")
	accR.byNo["A001"] = mkAccountActive("A001", 42)

	res, _ := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F_HASH_X", BookingDirectionDebit))

	// anchor 分片 = hash(A001), route 分片 = hash(F_HASH_X)
	expectedAnchor := simpleSumHash("A001")
	expectedRoute := simpleSumHash("F_HASH_X")
	if res.AnchorGlobalTableIndex != expectedAnchor {
		t.Errorf("anchor shard=%d expected %d", res.AnchorGlobalTableIndex, expectedAnchor)
	}
	if res.RouteGlobalTableIndex != expectedRoute {
		t.Errorf("route shard=%d expected %d", res.RouteGlobalTableIndex, expectedRoute)
	}
	// 这两个分片大概率不同（这正是方向 B 的核心：不同分片各自本地事务）
}

func simpleSumHash(s string) int {
	sum := 0
	for _, c := range s {
		sum += int(c)
	}
	return sum % 100
}

// ============================================================================
// nil clock 安全
// ============================================================================

func TestNewRouter_NilClockFallback(t *testing.T) {
	r := NewRouter(
		&fakeLogicalReader{byKey: map[string]*model.LogicalAccount{}, byID: map[int64]*model.LogicalAccount{}},
		&fakeAnchorReaderB{byKey: map[string]*model.TxAccountAnchor{}},
		&fakeRouteReader{byKey: map[string]*model.FlowAnchorRoute{}},
		&fakeAccountReader{byNo: map[string]*model.Account{}},
		nil, // tx reader
		nil, // clock
	)
	_, _ = r.Resolve(context.Background(), &ResolveRequest{
		LogicalAccountKey: "transit:x:USD",
		FlowID:            "X",
		Direction:         BookingDirectionDebit,
	})
}

// ============================================================================
// Direction valid/invalid
// ============================================================================

// ============================================================================
// 关键 edge case 补充
// ============================================================================

// 恢复路径：routing 已存在但 anchor 缺失（两步事务中间崩溃后重试）
// → 应返回 Insert 类型的 AnchorPlan，但 RoutePlan 为 nil（routing 已存在不重写）
func TestResolve_RecoveryPath_RoutingExistsButAnchorMissing(t *testing.T) {
	now := time.Now()
	r, lr, ar, rr, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A002")
	accR.byNo["A001"] = mkAccountActive("A001", 42)

	// routing 存在，但 anchor map 为空 → 模拟两步事务中间崩溃
	rr.byKey[routeKey("F_RECOVERY", 42)] = &model.FlowAnchorRoute{
		ID: 100, FlowID: "F_RECOVERY", LogicalAccountID: 42,
		AccountNo: "A001",
	}
	// ar.byKey[anchorKeyB("F_RECOVERY", "A001")] 不设置 → anchor 缺失

	res, err := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F_RECOVERY", BookingDirectionCredit))
	if err != nil {
		t.Fatalf("recovery path should succeed: %v", err)
	}
	if res.AnchorPlan == nil || res.AnchorPlan.Op != AnchorOpInsert {
		t.Errorf("recovery should produce Insert anchor plan, got %+v", res.AnchorPlan)
	}
	if res.RoutePlan != nil {
		t.Errorf("recovery: RoutePlan should be nil (routing exists), got %+v", res.RoutePlan)
	}
	if res.AccountNo != "A001" {
		t.Errorf("recovery should route to routing's account A001, got %s", res.AccountNo)
	}
	if res.AnchorPlan.NewAnchor.AccountNo != "A001" {
		t.Errorf("anchor plan account_no should match routing")
	}
}

// 恢复路径在 instance 已经 frozen 时应拒（除非是 TCC Cancel）
func TestResolve_RecoveryPath_RespectsPhaseGuard(t *testing.T) {
	now := time.Now()
	r, lr, _, rr, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A002")
	accR.byNo["A001"] = mkAccountPhase("A001", 42, model.LifecyclePhaseArchived)
	rr.byKey[routeKey("F_OLD", 42)] = &model.FlowAnchorRoute{
		ID: 100, FlowID: "F_OLD", LogicalAccountID: 42, AccountNo: "A001",
	}

	req := mkRequest("transit:foo:USD", "F_OLD", BookingDirectionDebit)
	_, err := r.Resolve(context.Background(), req)
	if !errors.Is(err, model.ErrPhaseGuardRejected) {
		t.Errorf("recovery on archived should be rejected, got %v", err)
	}
}

// quarantined instance 永远拒绝（包括恢复路径）
func TestResolve_PhaseGuard_QuarantinedAlwaysRejects(t *testing.T) {
	now := time.Now()
	r, lr, ar, rr, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A002")
	accR.byNo["A001"] = mkAccountPhase("A001", 42, model.LifecyclePhaseQuarantined)
	rr.byKey[routeKey("F1", 42)] = &model.FlowAnchorRoute{
		ID: 1, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
	}
	ar.byKey[anchorKeyB("F1", "A001")] = &model.TxAccountAnchor{
		ID: 1, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
		Status: model.AnchorStatusActive,
	}
	for _, bt := range []BookingType{
		BookingTypeNormal, BookingTypeTCCTry, BookingTypeTCCConfirm, BookingTypeTCCCancel,
	} {
		req := mkRequest("transit:foo:USD", "F1", BookingDirectionDebit)
		req.BookingType = bt
		_, err := r.Resolve(context.Background(), req)
		if !errors.Is(err, model.ErrPhaseGuardRejected) {
			t.Errorf("quarantined should reject bt=%d, got %v", bt, err)
		}
	}
}

// 迁移单跳：anchor.status=migrated → 跟随 MigratedToAccountNo
func TestResolve_MigrationSingleHop(t *testing.T) {
	now := time.Now()
	r, lr, ar, rr, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A003")
	accR.byNo["A002"] = mkAccountActive("A002", 42)

	rr.byKey[routeKey("F1", 42)] = &model.FlowAnchorRoute{
		ID: 1, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
	}
	target := "A002"
	ar.byKey[anchorKeyB("F1", "A001")] = &model.TxAccountAnchor{
		ID: 10, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
		Status: model.AnchorStatusMigrated, MigratedToAccountNo: &target,
		MigrationChainDepth: 1,
	}

	res, err := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F1", BookingDirectionDebit))
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountNo != "A002" {
		t.Errorf("migration follow should land on A002, got %s", res.AccountNo)
	}
}

// 迁移异常：status=migrated 但 MigratedToAccountNo=nil → 回退到原 AccountNo（防御性）
func TestResolve_MigrationNilTargetFallback(t *testing.T) {
	now := time.Now()
	r, lr, ar, rr, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A003")
	accR.byNo["A001"] = mkAccountActive("A001", 42)

	rr.byKey[routeKey("F1", 42)] = &model.FlowAnchorRoute{
		ID: 1, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
	}
	ar.byKey[anchorKeyB("F1", "A001")] = &model.TxAccountAnchor{
		ID: 10, FlowID: "F1", LogicalAccountID: 42, AccountNo: "A001",
		Status: model.AnchorStatusMigrated, MigratedToAccountNo: nil,
	}

	res, err := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F1", BookingDirectionDebit))
	if err != nil {
		t.Fatalf("nil migrated_to should not panic: %v", err)
	}
	if res.AccountNo != "A001" {
		t.Errorf("nil migrated_to should fallback to original A001, got %s", res.AccountNo)
	}
}

// 退款继承：源 flow 已发生迁移，新退款 flow 应继承到迁移后的 instance
// （routing 表里 source flow 的 account_no 自然指向了最新 instance）
func TestResolve_RefundInheritsAfterSourceMigration(t *testing.T) {
	now := time.Now()
	r, lr, _, rr, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A005")
	accR.byNo["A003"] = mkAccountActive("A003", 42) // 源已迁移到这里

	// 源 routing 表已经被强制迁移更新过：account_no=A003（不再是 A001），chain_depth=2
	rr.byKey[routeKey("PAY_OLD", 42)] = &model.FlowAnchorRoute{
		ID: 10, FlowID: "PAY_OLD", LogicalAccountID: 42, AccountNo: "A003",
		MigrationChainDepth: 2,
	}

	req := &ResolveRequest{
		LogicalAccountKey: "transit:foo:USD",
		FlowID:            "RFD_NEW",
		Direction:         BookingDirectionDebit,
		OriginalFlowID:    "PAY_OLD",
		ReuseSource:       model.AnchorReuseSourceRefundOf,
	}
	res, err := r.Resolve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// 新退款继承到 A003（不是被迁移之前的 A001）
	if res.AccountNo != "A003" {
		t.Errorf("refund should inherit source's CURRENT instance A003, got %s", res.AccountNo)
	}
	// 新 routing 也应该带 chain_depth=2 继承
	if res.RoutePlan.NewRoute.MigrationChainDepth != 2 {
		t.Errorf("refund route chain depth should inherit source's depth=2, got %d",
			res.RoutePlan.NewRoute.MigrationChainDepth)
	}
}

// Cache invalidate 强制重读
func TestResolve_CacheInvalidate(t *testing.T) {
	now := time.Now()
	r, lr, _, _, _ := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", false, "")

	_, _ = r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F1", BookingDirectionDebit))
	_, _ = r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F2", BookingDirectionDebit))
	if lr.callCount() != 1 {
		t.Errorf("expected 1 call within TTL, got %d", lr.callCount())
	}

	if impl, ok := r.(*router); ok {
		impl.cache.invalidate("transit:foo:USD")
	}
	_, _ = r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F3", BookingDirectionDebit))
	if lr.callCount() != 2 {
		t.Errorf("invalidate should force reload, got %d calls", lr.callCount())
	}
}

// Flow 一致性强测试：同一 flow 经过多轮调用必须落同一 account
// （即使发生：routing 路由表分片、anchor 分片、phase 切换）
func TestResolve_FlowConsistency_MultipleOperationsSameFlow(t *testing.T) {
	now := time.Now()
	r, lr, ar, rr, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A001")
	accR.byNo["A001"] = mkAccountActive("A001", 42)

	// 第 1 次：建立 routing + anchor
	res1, err := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F_MULTI", BookingDirectionDebit))
	if err != nil {
		t.Fatal(err)
	}
	if res1.AccountNo != "A001" || res1.AnchorPlan.Op != AnchorOpInsert {
		t.Fatal("first call should land A001 with Insert plan")
	}
	// caller 落 route + anchor
	rr.byKey[routeKey("F_MULTI", 42)] = res1.RoutePlan.NewRoute
	rr.byKey[routeKey("F_MULTI", 42)].ID = 7001
	ar.byKey[anchorKeyB("F_MULTI", "A001")] = res1.AnchorPlan.NewAnchor
	ar.byKey[anchorKeyB("F_MULTI", "A001")].ID = 7002

	// 第 2 次：UpdatePosting
	res2, _ := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F_MULTI", BookingDirectionCredit))
	if res2.AccountNo != "A001" || res2.AnchorPlan.Op != AnchorOpUpdatePosting {
		t.Fatal("second call should land A001 with UpdatePosting plan")
	}

	// 第 3 次：TCC Confirm
	req3 := mkRequest("transit:foo:USD", "F_MULTI", BookingDirectionDebit)
	req3.BookingType = BookingTypeTCCConfirm
	res3, _ := r.Resolve(context.Background(), req3)
	if res3.AccountNo != "A001" {
		t.Fatal("third call still locked to A001")
	}

	// 第 4 次：TCC Cancel
	req4 := mkRequest("transit:foo:USD", "F_MULTI", BookingDirectionCredit)
	req4.BookingType = BookingTypeTCCCancel
	res4, _ := r.Resolve(context.Background(), req4)
	if res4.AccountNo != "A001" {
		t.Fatal("TCC Cancel still locked to A001")
	}
}

// 并发首次锚定（race）：多个 goroutine 同时为新 flow 调用 Resolve
// 路由层应稳定返回相同的 AccountNo 和 RoutePlan
func TestResolve_ConcurrentFirstAnchoringSameFlow(t *testing.T) {
	now := time.Now()
	r, lr, _, _, accR := newRouterFixture(now)
	lr.byKey["transit:foo:USD"] = mkLogicalAccount(42, "transit:foo:USD", true, "A001")
	accR.byNo["A001"] = mkAccountActive("A001", 42)

	const N = 20
	var wg sync.WaitGroup
	results := make([]string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := r.Resolve(context.Background(), mkRequest("transit:foo:USD", "F_RACE", BookingDirectionDebit))
			if err == nil {
				results[i] = res.AccountNo
			}
		}(i)
	}
	wg.Wait()
	// 所有结果都应该指向 A001（即使他们都是首次锚定 — repo 层 UK 会让只有一个真正写入成功）
	for i, acc := range results {
		if acc != "A001" {
			t.Errorf("goroutine %d got %q, all must be A001", i, acc)
		}
	}
}

// LogicalAccount disabled 在生产 repo 层会过滤；fake 不模拟，但路由层应当依赖
// repo 行为。此测试文档化这个约定。
func TestResolve_DisabledLAFilteredByRepo_Documentation(t *testing.T) {
	t.Log("生产 repo.GetByKey 对 disabled LA 返回 ErrLogicalAccountNotRegistered")
	t.Log("Router 不重复校验 status，依赖 repo 层过滤")
}

func TestBookingDirection_IsValid(t *testing.T) {
	for _, d := range []BookingDirection{BookingDirectionDebit, BookingDirectionCredit} {
		if !d.IsValid() {
			t.Errorf("%d should be valid", d)
		}
	}
	for _, d := range []BookingDirection{0, 3, 4, 127, -1, -128} {
		if BookingDirection(d).IsValid() {
			t.Errorf("%d should NOT be valid", d)
		}
	}
}
