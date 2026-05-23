package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// Fakes — Scheduler 依赖注入
// ============================================================================

type fakeLogicalLister struct {
	rotating []*model.LogicalAccount
	err      error
}

func (f *fakeLogicalLister) ListRotating(_ context.Context, _ int) ([]*model.LogicalAccount, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rotating, nil
}

type fakeInstanceManager struct {
	activeByLA       map[int64]*model.Account
	provisionedByLA  map[int64]*model.Account
	createdInstances []*model.Account
	promoteParams    []PromoteAndDrainParams
	getActiveErr     error
	createErr        error
	promoteErr       error
	mu               sync.Mutex
}

func (f *fakeInstanceManager) GetActiveInstance(_ context.Context, laID int64) (*model.Account, error) {
	if f.getActiveErr != nil {
		return nil, f.getActiveErr
	}
	return f.activeByLA[laID], nil
}

func (f *fakeInstanceManager) GetProvisionedInstance(_ context.Context, laID int64) (*model.Account, error) {
	return f.provisionedByLA[laID], nil
}

func (f *fakeInstanceManager) CreateProvisioned(_ context.Context, acc *model.Account) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.createdInstances = append(f.createdInstances, acc)
	if acc.LogicalAccountID != nil {
		f.provisionedByLA[*acc.LogicalAccountID] = acc
	}
	return nil
}

func (f *fakeInstanceManager) PromoteAndDrain(_ context.Context, p PromoteAndDrainParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.promoteErr != nil {
		return f.promoteErr
	}
	f.promoteParams = append(f.promoteParams, p)
	// 模拟：旧 active → draining，provisioned → active
	if old := f.activeByLA[p.LogicalAccountID]; old != nil && old.AccountNo == p.OldActiveAccountNo {
		old.LifecyclePhase = model.LifecyclePhaseDraining
	}
	if next := f.provisionedByLA[p.LogicalAccountID]; next != nil && next.AccountNo == p.NewActiveAccountNo {
		next.LifecyclePhase = model.LifecyclePhaseActive
		f.activeByLA[p.LogicalAccountID] = next
		delete(f.provisionedByLA, p.LogicalAccountID)
	}
	return nil
}

// PromoteAndDrainFleet fake：跟 PromoteAndDrain 一样的简化模型（fleet 在 test 里
// 视作单 instance 切换，方便复用既有断言；实际 prod 跑 100 个 sub 并行）
func (f *fakeInstanceManager) PromoteAndDrainFleet(_ context.Context, p PromoteAndDrainFleetParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.promoteErr != nil {
		return f.promoteErr
	}
	if old := f.activeByLA[p.LogicalAccountID]; old != nil {
		old.LifecyclePhase = model.LifecyclePhaseDraining
	}
	if next := f.provisionedByLA[p.LogicalAccountID]; next != nil {
		next.LifecyclePhase = model.LifecyclePhaseActive
		f.activeByLA[p.LogicalAccountID] = next
		delete(f.provisionedByLA, p.LogicalAccountID)
	}
	return nil
}

type fakePolicyReaderForSched struct {
	policyByLA map[int64]*model.LogicalAccountRotationPolicy
	err        error
}

func (f *fakePolicyReaderForSched) GetPolicy(_ context.Context, laID int64) (*model.LogicalAccountRotationPolicy, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.policyByLA[laID], nil
}

type fakeLockManager struct {
	heldBy  map[int64]string // 已持有的锁，记录 owner
	failFor map[int64]bool   // 指定 LA 直接拒绝获取
	mu      sync.Mutex
	count   int32
}

func (f *fakeLockManager) AcquireForLogical(_ context.Context, laID int64, owner string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	atomic.AddInt32(&f.count, 1)
	if f.failFor[laID] {
		return nil, errors.New("lock contention")
	}
	if _, held := f.heldBy[laID]; held {
		return nil, errors.New("already locked")
	}
	f.heldBy[laID] = owner
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.heldBy, laID)
	}, nil
}

type fakeAccountIDGen struct {
	counter int64
	mu      sync.Mutex
}

func (f *fakeAccountIDGen) NewProvisionedAccountNo(_ context.Context, la *model.LogicalAccount, _ time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	return formatAccountNo(la.ID, f.counter), nil
}

func (f *fakeAccountIDGen) NewProvisionedFleetAccountNos(_ context.Context, la *model.LogicalAccount, _ time.Time) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 100)
	for i := 0; i < 100; i++ {
		f.counter++
		out[i] = formatAccountNo(la.ID, f.counter)
	}
	return out, nil
}

func formatAccountNo(laID, n int64) string {
	return "ACC_" + intToStr(laID) + "_" + intToStr(n)
}

func intToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	s := ""
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}

func newSchedulerFixture(now time.Time) (
	*Scheduler, *fakeLogicalLister, *fakeInstanceManager,
	*fakePolicyReaderForSched, *fakeLockManager, *fakeAccountIDGen,
) {
	ll := &fakeLogicalLister{}
	im := &fakeInstanceManager{
		activeByLA:      map[int64]*model.Account{},
		provisionedByLA: map[int64]*model.Account{},
	}
	pr := &fakePolicyReaderForSched{policyByLA: map[int64]*model.LogicalAccountRotationPolicy{}}
	lm := &fakeLockManager{heldBy: map[int64]string{}, failFor: map[int64]bool{}}
	idg := &fakeAccountIDGen{}
	s := NewScheduler(ll, im, pr, lm, idg, "test-host:1234", func() time.Time { return now })
	return s, ll, im, pr, lm, idg
}

func mkRotatingLA(id int64, key string) *model.LogicalAccount {
	return &model.LogicalAccount{
		ID:                  id,
		LogicalAccountKey:   key,
		AccountType:         model.AccountTypeTransitChannelPayable,
		AccountBusinessType: 101,
		Currency:            "USD",
		RotationEnabled:     1,
		Status:              model.LogicalAccountStatusEnabled,
		Version:             1,
	}
}

func mkPolicy(la int64) *model.LogicalAccountRotationPolicy {
	return &model.LogicalAccountRotationPolicy{
		LogicalAccountID:     la,
		PeriodUnit:           model.PeriodUnitMonth,
		PeriodCount:          1,
		RotationAnchorTZ:     "UTC",
		DrainP99Seconds:      7 * 86400,
		DrainHardTimeoutSecs: 14 * 86400,
		ArchiveGraceSecs:     7 * 86400,
		ProvisionLeadSecs:    86400, // 24h lead
		ConfigVersion:        1,
	}
}

func mkActive(no string, la int64, periodEnd time.Time) *model.Account {
	laRef := la
	ps := periodEnd.AddDate(0, -1, 0)
	return &model.Account{
		AccountNo: no, LogicalAccountID: &laRef,
		LifecyclePhase: model.LifecyclePhaseActive,
		PeriodStart:    &ps,
		PeriodEnd:      &periodEnd,
		Version:        1,
	}
}

// ============================================================================
// Tick: 决策树
// ============================================================================

// 无 LA → Processed=0, no error
func TestScheduler_Tick_NoLAs(t *testing.T) {
	s, _, _, _, _, _ := newSchedulerFixture(time.Now())
	res, err := s.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Processed != 0 {
		t.Errorf("expected 0 processed, got %d", res.Processed)
	}
}

// LA 没有 active instance（首次启用）→ 创建 provisioned
func TestScheduler_Tick_NoActive_CreatesProvisioned(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s, ll, im, pr, _, _ := newSchedulerFixture(now)
	la := mkRotatingLA(42, "transit:foo:USD")
	ll.rotating = []*model.LogicalAccount{la}
	pr.policyByLA[42] = mkPolicy(42)

	res, err := s.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.ProvisionedNew != 1 {
		t.Errorf("expected 1 provisioned, got %d", res.ProvisionedNew)
	}
	if len(im.createdInstances) != 1 {
		t.Fatalf("expected 1 created, got %d", len(im.createdInstances))
	}
	created := im.createdInstances[0]
	if created.LifecyclePhase != model.LifecyclePhaseProvisioned {
		t.Errorf("phase should be provisioned, got %s", created.LifecyclePhase)
	}
}

// PeriodEnd 还远 → no action
func TestScheduler_Tick_PeriodEndFarAway_NoAction(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s, ll, im, pr, _, _ := newSchedulerFixture(now)
	la := mkRotatingLA(42, "transit:foo:USD")
	ll.rotating = []*model.LogicalAccount{la}
	pr.policyByLA[42] = mkPolicy(42)
	im.activeByLA[42] = mkActive("A001", 42, now.Add(30*24*time.Hour))

	res, _ := s.Tick(context.Background())
	if res.ProvisionedNew != 0 || res.Activated != 0 {
		t.Errorf("expected no action, got provisioned=%d activated=%d", res.ProvisionedNew, res.Activated)
	}
	if res.NoActionNeeded != 1 {
		t.Errorf("expected 1 no-action, got %d", res.NoActionNeeded)
	}
}

// PeriodEnd 临近（24h lead）→ 创建 provisioned
func TestScheduler_Tick_PeriodEndApproaching_Provisions(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s, ll, im, pr, _, _ := newSchedulerFixture(now)
	la := mkRotatingLA(42, "transit:foo:USD")
	ll.rotating = []*model.LogicalAccount{la}
	pr.policyByLA[42] = mkPolicy(42)
	// PeriodEnd 在 12 小时后 → 在 lead_time (24h) 内
	im.activeByLA[42] = mkActive("A001", 42, now.Add(12*time.Hour))

	res, _ := s.Tick(context.Background())
	if res.ProvisionedNew != 1 {
		t.Errorf("expected provisioned 1, got %d", res.ProvisionedNew)
	}
}

// 已有 provisioned → 不重复
func TestScheduler_Tick_ProvisionedAlreadyExists_NoDup(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s, ll, im, pr, _, _ := newSchedulerFixture(now)
	la := mkRotatingLA(42, "transit:foo:USD")
	ll.rotating = []*model.LogicalAccount{la}
	pr.policyByLA[42] = mkPolicy(42)
	im.activeByLA[42] = mkActive("A001", 42, now.Add(12*time.Hour))
	im.provisionedByLA[42] = &model.Account{AccountNo: "A002", LifecyclePhase: model.LifecyclePhaseProvisioned}

	res, _ := s.Tick(context.Background())
	if res.ProvisionedNew != 0 {
		t.Errorf("should not create duplicate, got %d", res.ProvisionedNew)
	}
}

// PeriodEnd 已过 + provisioned 就位 → swap
func TestScheduler_Tick_PastPeriodEnd_Swaps(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s, ll, im, pr, _, _ := newSchedulerFixture(now)
	la := mkRotatingLA(42, "transit:foo:USD")
	ll.rotating = []*model.LogicalAccount{la}
	pr.policyByLA[42] = mkPolicy(42)
	// PeriodEnd 已过 1 小时
	im.activeByLA[42] = mkActive("A001", 42, now.Add(-1*time.Hour))
	periodEndNext := now.Add(30 * 24 * time.Hour)
	im.provisionedByLA[42] = &model.Account{
		AccountNo: "A002", LifecyclePhase: model.LifecyclePhaseProvisioned,
		PeriodStart: &now, PeriodEnd: &periodEndNext,
		Version: 0,
	}

	res, err := s.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Activated != 1 {
		t.Errorf("expected 1 activated, got %d", res.Activated)
	}
	if len(im.promoteParams) != 1 {
		t.Fatalf("expected 1 promote call, got %d", len(im.promoteParams))
	}
	p := im.promoteParams[0]
	if p.OldActiveAccountNo != "A001" || p.NewActiveAccountNo != "A002" {
		t.Errorf("promote params wrong: %+v", p)
	}
}

// PeriodEnd 已过 + 没有 provisioned → 兜底创建（下一 tick 再激活）
func TestScheduler_Tick_PastPeriodEnd_NoProvisioned_FallsBackToCreate(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s, ll, im, pr, _, _ := newSchedulerFixture(now)
	la := mkRotatingLA(42, "transit:foo:USD")
	ll.rotating = []*model.LogicalAccount{la}
	pr.policyByLA[42] = mkPolicy(42)
	im.activeByLA[42] = mkActive("A001", 42, now.Add(-1*time.Hour))

	res, _ := s.Tick(context.Background())
	if res.Activated != 0 {
		t.Errorf("should not activate without provisioned, got %d", res.Activated)
	}
	if res.ProvisionedNew != 1 {
		t.Errorf("should fall back to create, got %d", res.ProvisionedNew)
	}
}

// 锁被占 → 该 LA 跳过（不算错误）
func TestScheduler_Tick_LockContention_SkipsLA(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s, ll, im, pr, lm, _ := newSchedulerFixture(now)
	la := mkRotatingLA(42, "transit:foo:USD")
	ll.rotating = []*model.LogicalAccount{la}
	pr.policyByLA[42] = mkPolicy(42)
	im.activeByLA[42] = mkActive("A001", 42, now.Add(-1*time.Hour))
	lm.failFor[42] = true

	res, _ := s.Tick(context.Background())
	// 跳过但 Processed 仍计入 (我们 increment Processed before lock attempt)
	if res.Processed != 1 {
		t.Errorf("expected Processed=1, got %d", res.Processed)
	}
	if res.Activated != 0 || res.ProvisionedNew != 0 {
		t.Errorf("locked LA should produce no action")
	}
	if len(res.Errors) != 0 {
		t.Errorf("lock contention should NOT be reported as error, got %v", res.Errors)
	}
}

// 多个 LA，一个失败不影响另一个
func TestScheduler_Tick_OneFailDoesntStopOthers(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s, ll, im, pr, _, _ := newSchedulerFixture(now)
	la1 := mkRotatingLA(1, "transit:la1:USD")
	la2 := mkRotatingLA(2, "transit:la2:USD")
	ll.rotating = []*model.LogicalAccount{la1, la2}
	pr.policyByLA[1] = mkPolicy(1)
	// la2 missing policy → 触发错误
	im.activeByLA[1] = mkActive("A001", 1, now.Add(30*24*time.Hour))
	im.activeByLA[2] = mkActive("B001", 2, now.Add(30*24*time.Hour))

	res, err := s.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Processed != 2 {
		t.Errorf("both LAs processed, got %d", res.Processed)
	}
	if len(res.Errors) != 1 {
		t.Errorf("expected 1 error for la2, got %d", len(res.Errors))
	}
	if res.Errors[0].LogicalAccountID != 2 {
		t.Errorf("error should be for la2")
	}
}

// ============================================================================
// ForceSwitch（admin-web 手动切换入口）
// ============================================================================

func TestScheduler_ForceSwitch_RequiresOperatorAndReason(t *testing.T) {
	s, _, _, _, _, _ := newSchedulerFixture(time.Now())
	if err := s.ForceSwitch(context.Background(), 42, "", "x"); err == nil {
		t.Error("missing operator should error")
	}
	if err := s.ForceSwitch(context.Background(), 42, "ops", ""); err == nil {
		t.Error("missing reason should error")
	}
}

func TestScheduler_ForceSwitch_NoActive(t *testing.T) {
	s, ll, _, pr, _, _ := newSchedulerFixture(time.Now())
	ll.rotating = []*model.LogicalAccount{mkRotatingLA(42, "transit:foo:USD")}
	pr.policyByLA[42] = mkPolicy(42)
	if err := s.ForceSwitch(context.Background(), 42, "ops", "test"); err == nil {
		t.Fatal("should error when no active")
	}
}

func TestScheduler_ForceSwitch_HappyPath(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s, ll, im, pr, _, _ := newSchedulerFixture(now)
	la := mkRotatingLA(42, "transit:foo:USD")
	ll.rotating = []*model.LogicalAccount{la}
	pr.policyByLA[42] = mkPolicy(42)
	// active 还远着 (60 天)，但 force switch 不看时间
	im.activeByLA[42] = mkActive("A001", 42, now.Add(60*24*time.Hour))
	periodEndNext := now.Add(90 * 24 * time.Hour)
	im.provisionedByLA[42] = &model.Account{
		AccountNo: "A002", LifecyclePhase: model.LifecyclePhaseProvisioned,
		PeriodStart: &now, PeriodEnd: &periodEndNext,
	}

	if err := s.ForceSwitch(context.Background(), 42, "ops-alice", "promo to fix issue X"); err != nil {
		t.Fatal(err)
	}
	if len(im.promoteParams) != 1 {
		t.Errorf("expected 1 promote, got %d", len(im.promoteParams))
	}
}

// ForceProvision 入参校验 + 创建
func TestScheduler_ForceProvision_HappyPath(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s, ll, im, pr, _, _ := newSchedulerFixture(now)
	la := mkRotatingLA(42, "transit:foo:USD")
	ll.rotating = []*model.LogicalAccount{la}
	pr.policyByLA[42] = mkPolicy(42)
	im.activeByLA[42] = mkActive("A001", 42, now.Add(30*24*time.Hour))

	if err := s.ForceProvision(context.Background(), 42, "ops-bob", "pre-create"); err != nil {
		t.Fatal(err)
	}
	if len(im.createdInstances) != 1 {
		t.Errorf("expected 1 created, got %d", len(im.createdInstances))
	}
}

// ============================================================================
// computeNextPeriod: 周期计算
// ============================================================================

func TestComputeNextPeriod_FirstTime(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	s, _, _, _, _, _ := newSchedulerFixture(now)
	policy := &model.LogicalAccountRotationPolicy{
		PeriodUnit:       model.PeriodUnitMonth,
		PeriodCount:      1,
		RotationAnchorTZ: "UTC",
	}
	start, end, err := s.computeNextPeriod(nil, policy, now)
	if err != nil {
		t.Fatal(err)
	}
	if !start.Equal(now) {
		t.Errorf("first time start should be now")
	}
	// 一个月后
	expectedEnd := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	if !end.Equal(expectedEnd) {
		t.Errorf("expected end=%v, got %v", expectedEnd, end)
	}
}

func TestComputeNextPeriod_QuarterUnit(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	s, _, _, _, _, _ := newSchedulerFixture(now)
	policy := &model.LogicalAccountRotationPolicy{
		PeriodUnit:       model.PeriodUnitQuarter,
		PeriodCount:      1,
		RotationAnchorTZ: "UTC",
	}
	_, end, _ := s.computeNextPeriod(nil, policy, now)
	expectedEnd := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if !end.Equal(expectedEnd) {
		t.Errorf("expected quarter end %v, got %v", expectedEnd, end)
	}
}

func TestComputeNextPeriod_DayUnit(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	s, _, _, _, _, _ := newSchedulerFixture(now)
	policy := &model.LogicalAccountRotationPolicy{
		PeriodUnit:       model.PeriodUnitDay,
		PeriodCount:      7,
		RotationAnchorTZ: "UTC",
	}
	_, end, _ := s.computeNextPeriod(nil, policy, now)
	expectedEnd := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	if !end.Equal(expectedEnd) {
		t.Errorf("expected day*7 end %v, got %v", expectedEnd, end)
	}
}

// 跨期：第二期起点 = 第一期终点（无 gap）
func TestComputeNextPeriod_ConsecutivePeriods(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	s, _, _, _, _, _ := newSchedulerFixture(now)
	policy := &model.LogicalAccountRotationPolicy{
		PeriodUnit:       model.PeriodUnitMonth,
		PeriodCount:      1,
		RotationAnchorTZ: "UTC",
	}
	prevEnd := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	prev := mkActive("A001", 42, prevEnd)
	start, end, _ := s.computeNextPeriod(prev, policy, now)
	if !start.Equal(prevEnd) {
		t.Errorf("next period start should = prev period end, got %v vs %v", start, prevEnd)
	}
	expectedEnd := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	if !end.Equal(expectedEnd) {
		t.Errorf("expected next end %v, got %v", expectedEnd, end)
	}
}

func TestComputeNextPeriod_InvalidTZ(t *testing.T) {
	now := time.Now()
	s, _, _, _, _, _ := newSchedulerFixture(now)
	policy := &model.LogicalAccountRotationPolicy{
		PeriodUnit:       model.PeriodUnitMonth,
		PeriodCount:      1,
		RotationAnchorTZ: "Mars/Olympus",
	}
	_, _, err := s.computeNextPeriod(nil, policy, now)
	if err == nil {
		t.Fatal("invalid TZ should error")
	}
}

func TestComputeNextPeriod_InvalidUnit(t *testing.T) {
	now := time.Now()
	s, _, _, _, _, _ := newSchedulerFixture(now)
	policy := &model.LogicalAccountRotationPolicy{
		PeriodUnit:       "FORTNIGHT",
		PeriodCount:      1,
		RotationAnchorTZ: "UTC",
	}
	_, _, err := s.computeNextPeriod(nil, policy, now)
	if err == nil {
		t.Fatal("invalid PeriodUnit should error")
	}
}

// ============================================================================
// 默认 owner / nil clock
// ============================================================================

func TestNewScheduler_DefaultOwnerAndClock(t *testing.T) {
	s := NewScheduler(
		&fakeLogicalLister{},
		&fakeInstanceManager{},
		&fakePolicyReaderForSched{},
		&fakeLockManager{heldBy: map[int64]string{}},
		&fakeAccountIDGen{},
		"", // empty owner
		nil, // nil clock
	)
	if s.owner == "" {
		t.Error("empty owner should fallback to non-empty")
	}
	if s.clock() == (time.Time{}) {
		t.Error("nil clock should fallback to time.Now")
	}
}

// 并发 Tick：多个 goroutine 同时 Tick 同一 LA，锁机制确保只有一个成功 promote
func TestScheduler_ConcurrentTick_OnlyOnePromotes(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s, ll, im, pr, _, _ := newSchedulerFixture(now)
	la := mkRotatingLA(42, "transit:foo:USD")
	ll.rotating = []*model.LogicalAccount{la}
	pr.policyByLA[42] = mkPolicy(42)
	im.activeByLA[42] = mkActive("A001", 42, now.Add(-1*time.Hour))
	periodEndNext := now.Add(30 * 24 * time.Hour)
	im.provisionedByLA[42] = &model.Account{
		AccountNo: "A002", LifecyclePhase: model.LifecyclePhaseProvisioned,
		PeriodStart: &now, PeriodEnd: &periodEndNext,
	}

	const G = 10
	var wg sync.WaitGroup
	for i := 0; i < G; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Tick(context.Background())
		}()
	}
	wg.Wait()
	// 锁机制：只有一个 goroutine 真的执行了 promote
	if len(im.promoteParams) != 1 {
		t.Errorf("only 1 promote should succeed under concurrent Ticks, got %d", len(im.promoteParams))
	}
}

// ============================================================================
// 错误传播
// ============================================================================

func TestScheduler_Tick_ListError(t *testing.T) {
	s, ll, _, _, _, _ := newSchedulerFixture(time.Now())
	ll.err = errors.New("db down")
	_, err := s.Tick(context.Background())
	if err == nil {
		t.Fatal("list error should propagate")
	}
}

func TestScheduler_Tick_NoPolicyReportsError(t *testing.T) {
	now := time.Now()
	s, ll, im, _, _, _ := newSchedulerFixture(now)
	la := mkRotatingLA(42, "transit:foo:USD")
	ll.rotating = []*model.LogicalAccount{la}
	im.activeByLA[42] = mkActive("A001", 42, now.Add(30*24*time.Hour))
	// policy not set

	res, _ := s.Tick(context.Background())
	if len(res.Errors) != 1 {
		t.Errorf("missing policy should be reported as error, got %d", len(res.Errors))
	}
}

func TestScheduler_Tick_ActiveMissingPeriodEnd(t *testing.T) {
	now := time.Now()
	s, ll, im, pr, _, _ := newSchedulerFixture(now)
	la := mkRotatingLA(42, "transit:foo:USD")
	ll.rotating = []*model.LogicalAccount{la}
	pr.policyByLA[42] = mkPolicy(42)
	laRef := int64(42)
	im.activeByLA[42] = &model.Account{
		AccountNo: "A001", LogicalAccountID: &laRef,
		LifecyclePhase: model.LifecyclePhaseActive,
		// PeriodEnd 缺失
	}

	res, _ := s.Tick(context.Background())
	if len(res.Errors) != 1 {
		t.Errorf("missing period_end should error, got %d", len(res.Errors))
	}
	if !strings.Contains(res.Errors[0].Err.Error(), "period_end") {
		t.Errorf("error should mention period_end, got %v", res.Errors[0].Err)
	}
}
