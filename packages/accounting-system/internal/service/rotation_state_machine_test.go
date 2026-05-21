package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// Fakes — 注入式依赖，不需要真实 DB
// ============================================================================

type fakeAccountReader struct {
	activeCountByLogical map[int64]int
	accountsByNo         map[string]*model.Account
	errOnCount           error
}

func (f *fakeAccountReader) CountActiveByLogical(_ context.Context, la int64) (int, error) {
	if f.errOnCount != nil {
		return 0, f.errOnCount
	}
	return f.activeCountByLogical[la], nil
}

func (f *fakeAccountReader) GetByAccountNo(_ context.Context, no string) (*model.Account, error) {
	return f.accountsByNo[no], nil
}

type fakeAnchorReader struct {
	openByShard  map[int]int64
	stuckByShard map[int]int64
	errOnCount   error
}

func (f *fakeAnchorReader) CountOpenByAccountNo(_ context.Context, _ string, gtbl int) (int64, error) {
	if f.errOnCount != nil {
		return 0, f.errOnCount
	}
	return f.openByShard[gtbl], nil
}

func (f *fakeAnchorReader) CountStuckByAccountNo(_ context.Context, _ string, gtbl int) (int64, error) {
	if f.errOnCount != nil {
		return 0, f.errOnCount
	}
	return f.stuckByShard[gtbl], nil
}

func (f *fakeAnchorReader) OldestOpenAnchoredAt(_ context.Context, _ string, _ int) (*time.Time, error) {
	return nil, nil
}

type fakePolicyReader struct {
	policy *model.LogicalAccountRotationPolicy
	err    error
}

func (f *fakePolicyReader) GetPolicy(_ context.Context, _ int64) (*model.LogicalAccountRotationPolicy, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.policy, nil
}

type fakeAnchorShards struct{ ids []int }

func (f *fakeAnchorShards) AllAnchorShards() []int { return f.ids }

// 工厂：返回一个能跑大部分测试的 fixture
func newFixture(now time.Time) (
	*InstanceStateMachine,
	*fakeAccountReader,
	*fakeAnchorReader,
	*fakePolicyReader,
) {
	ar := &fakeAccountReader{
		activeCountByLogical: map[int64]int{},
		accountsByNo:         map[string]*model.Account{},
	}
	anr := &fakeAnchorReader{
		openByShard:  map[int]int64{},
		stuckByShard: map[int]int64{},
	}
	pr := &fakePolicyReader{
		policy: &model.LogicalAccountRotationPolicy{
			LogicalAccountID:     42,
			PeriodUnit:           model.PeriodUnitMonth,
			PeriodCount:          1,
			RotationAnchorTZ:     "UTC",
			DrainP99Seconds:      7 * 86400,
			DrainHardTimeoutSecs: 14 * 86400,
			ArchiveGraceSecs:     7 * 86400,
			ProvisionLeadSecs:    86400,
		},
	}
	shards := &fakeAnchorShards{ids: []int{0, 1, 2, 50, 99}}
	sm := NewInstanceStateMachine(ar, anr, pr, shards, func() time.Time { return now })
	return sm, ar, anr, pr
}

func ptr64(v int64) *int64       { return &v }
func ptrInt(v int) *int           { return &v }
func ptrStr(v string) *string     { return &v }
func ptrTime(v time.Time) *time.Time { return &v }

func mkAccount(no string, la int64, phase model.LifecyclePhase) *model.Account {
	return &model.Account{
		AccountNo:        no,
		LogicalAccountID: ptr64(la),
		LifecyclePhase:   phase,
	}
}

// ============================================================================
// CheckTransition: 图非法性 + 守卫
// ============================================================================

func TestCheckTransition_NilAccountErrors(t *testing.T) {
	sm, _, _, _ := newFixture(time.Now())
	if _, err := sm.CheckTransition(context.Background(), nil, model.LifecyclePhaseActive); err == nil {
		t.Fatal("nil account must error")
	}
}

func TestCheckTransition_RejectsIllegalGraphEdges(t *testing.T) {
	sm, _, _, _ := newFixture(time.Now())
	// active -> archived 是非法的（要经 draining + frozen）
	acc := mkAccount("A001", 42, model.LifecyclePhaseActive)
	r, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseArchived)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.Allowed {
		t.Fatal("active->archived must be rejected at graph level")
	}
	if r.Reason == "" {
		t.Error("rejection must include Reason for ops debugging")
	}
}

func TestCheckTransition_LegacyAccountOnlyQuarantine(t *testing.T) {
	sm, _, _, _ := newFixture(time.Now())
	acc := &model.Account{AccountNo: "A001", LifecyclePhase: model.LifecyclePhaseLegacy}
	// 没有 logical_account_id 的 legacy 不能进 active
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseActive)
	if r.Allowed {
		t.Fatal("legacy w/o logical_account_id must NOT enter active")
	}
	// 但可以进 quarantined（兜底）
	r2, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseQuarantined)
	if !r2.Allowed {
		t.Fatalf("legacy must allow quarantined escape, got reason=%s", r2.Reason)
	}
}

// ============================================================================
// guardEnterActive: I1 不变量
// ============================================================================

func TestGuardEnterActive_BlocksWhenAnotherActiveExists(t *testing.T) {
	now := time.Now()
	sm, ar, _, _ := newFixture(now)
	ar.activeCountByLogical[42] = 1 // 已有 1 个 active
	acc := mkAccount("A002", 42, model.LifecyclePhaseProvisioned)
	r, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseActive)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.Allowed {
		t.Fatal("must block when another active exists under same logical (I1)")
	}
	if !contains(r.Reason, "I1") {
		t.Errorf("reason should mention I1 invariant, got %q", r.Reason)
	}
}

func TestGuardEnterActive_AllowsWhenNoExistingActive(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now) // active count = 0
	acc := mkAccount("A002", 42, model.LifecyclePhaseProvisioned)
	acc.PeriodStart = ptrTime(now.Add(-1 * time.Hour)) // 已经到了 period_start
	r, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseActive)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !r.Allowed {
		t.Fatalf("should allow when no other active, got reason=%s", r.Reason)
	}
}

func TestGuardEnterActive_BlocksFuturePeriodStart(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkAccount("A002", 42, model.LifecyclePhaseProvisioned)
	acc.PeriodStart = ptrTime(now.Add(1 * time.Hour)) // 未来
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseActive)
	if r.Allowed {
		t.Fatal("must block enter active when period_start is in the future")
	}
	if !contains(r.Reason, "too early") {
		t.Errorf("reason should mention 'too early', got %q", r.Reason)
	}
}

func TestGuardEnterActive_RepoError(t *testing.T) {
	sm, ar, _, _ := newFixture(time.Now())
	ar.errOnCount = errors.New("db down")
	acc := mkAccount("A001", 42, model.LifecyclePhaseProvisioned)
	_, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseActive)
	if err == nil {
		t.Fatal("repo error must bubble up; not silently treated as Allowed=false")
	}
}

// ============================================================================
// guardEnterDraining: 不能太早
// ============================================================================

func TestGuardEnterDraining_BlocksTooEarly(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkAccount("A001", 42, model.LifecyclePhaseActive)
	acc.PeriodEnd = ptrTime(now.Add(1 * time.Hour))
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseDraining)
	if r.Allowed {
		t.Fatal("must block draining when period_end > now")
	}
}

func TestGuardEnterDraining_AllowsAtBoundary(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkAccount("A001", 42, model.LifecyclePhaseActive)
	acc.PeriodEnd = ptrTime(now.Add(-1 * time.Second)) // 已经过期 1 秒
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseDraining)
	if !r.Allowed {
		t.Fatalf("must allow draining when period_end passed, got %q", r.Reason)
	}
}

func TestGuardEnterDraining_MissingPeriodEnd(t *testing.T) {
	sm, _, _, _ := newFixture(time.Now())
	acc := mkAccount("A001", 42, model.LifecyclePhaseActive)
	// PeriodEnd 为 nil
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseDraining)
	if r.Allowed {
		t.Fatal("missing period_end must block draining")
	}
}

// ============================================================================
// guardEnterFrozen: 四个守卫全部覆盖
// ============================================================================

func mkDrainingAccount(now time.Time, ageDays int, balance int64) *model.Account {
	a := mkAccount("A001", 42, model.LifecyclePhaseDraining)
	a.DrainingStartedAt = ptrTime(now.Add(-time.Duration(ageDays) * 24 * time.Hour))
	a.Balance = balance
	return a
}

func TestGuardEnterFrozen_HappyPath(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkDrainingAccount(now, 8, 0) // 8 天 > drain_p99 7 天
	r, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !r.Allowed {
		t.Fatalf("happy path should allow frozen, got %q", r.Reason)
	}
}

func TestGuardEnterFrozen_BlockedByBalance(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkDrainingAccount(now, 8, 100) // balance != 0
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if r.Allowed {
		t.Fatal("balance != 0 must block frozen (I4)")
	}
	if !contains(r.Reason, "balance") {
		t.Errorf("reason should mention balance, got %q", r.Reason)
	}
}

func TestGuardEnterFrozen_BlockedByYoungDrain(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkDrainingAccount(now, 3, 0) // 3 天 < drain_p99 7 天
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if r.Allowed {
		t.Fatal("draining age < drain_p99 must block frozen")
	}
	if !contains(r.Reason, "drain age") {
		t.Errorf("reason should mention drain age, got %q", r.Reason)
	}
}

func TestGuardEnterFrozen_BlockedByOpenAnchor(t *testing.T) {
	now := time.Now()
	sm, _, anr, _ := newFixture(now)
	anr.openByShard[1] = 1 // 一个 shard 还有 1 笔 open
	acc := mkDrainingAccount(now, 8, 0)
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if r.Allowed {
		t.Fatal("open anchor > 0 must block frozen")
	}
	if !contains(r.Reason, "open anchor") {
		t.Errorf("reason should mention open anchor, got %q", r.Reason)
	}
}

func TestGuardEnterFrozen_BlockedByStuckAnchor(t *testing.T) {
	now := time.Now()
	sm, _, anr, _ := newFixture(now)
	anr.stuckByShard[50] = 1
	acc := mkDrainingAccount(now, 8, 0)
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if r.Allowed {
		t.Fatal("stuck anchor > 0 must block frozen (must quarantine first)")
	}
	if !contains(r.Reason, "stuck") {
		t.Errorf("reason should mention stuck, got %q", r.Reason)
	}
}

func TestGuardEnterFrozen_MissingPolicy(t *testing.T) {
	now := time.Now()
	sm, _, _, pr := newFixture(now)
	pr.policy = nil
	acc := mkDrainingAccount(now, 8, 0)
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if r.Allowed {
		t.Fatal("missing policy must block")
	}
}

func TestGuardEnterFrozen_PolicyReadError(t *testing.T) {
	now := time.Now()
	sm, _, _, pr := newFixture(now)
	pr.err = errors.New("policy db error")
	acc := mkDrainingAccount(now, 8, 0)
	_, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if err == nil {
		t.Fatal("policy IO error must propagate")
	}
}

func TestGuardEnterFrozen_AnchorReadError(t *testing.T) {
	now := time.Now()
	sm, _, anr, _ := newFixture(now)
	anr.errOnCount = errors.New("shard read failed")
	acc := mkDrainingAccount(now, 8, 0)
	_, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if err == nil {
		t.Fatal("anchor shard read error must propagate; partial reads are unsafe")
	}
}

// ============================================================================
// guardEnterArchived
// ============================================================================

func mkFrozenAccount(now time.Time, hoursSinceFrozen int, balance int64) *model.Account {
	a := mkAccount("A001", 42, model.LifecyclePhaseFrozen)
	a.FrozenAt = ptrTime(now.Add(-time.Duration(hoursSinceFrozen) * time.Hour))
	a.Balance = balance
	return a
}

func TestGuardEnterArchived_HappyPath(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now) // archive_grace 7 days
	acc := mkFrozenAccount(now, 8*24, 0)
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseArchived)
	if !r.Allowed {
		t.Fatalf("happy path should allow archive, got %q", r.Reason)
	}
}

func TestGuardEnterArchived_BlockedByBalance(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkFrozenAccount(now, 8*24, 1) // 1 cent residual
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseArchived)
	if r.Allowed {
		t.Fatal("balance non-zero must block archive (I4)")
	}
}

func TestGuardEnterArchived_BlockedByGrace(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkFrozenAccount(now, 3*24, 0) // 3 days < 7-day grace
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseArchived)
	if r.Allowed {
		t.Fatal("frozen age < archive_grace must block")
	}
}

func TestGuardEnterArchived_MissingFrozenAt(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkAccount("A001", 42, model.LifecyclePhaseFrozen) // FrozenAt nil
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseArchived)
	if r.Allowed {
		t.Fatal("missing FrozenAt must block")
	}
}

// ============================================================================
// Quarantined 总是允许（运维兜底）
// ============================================================================

func TestQuarantinedAllowedFromAnyState(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	for _, phase := range []model.LifecyclePhase{
		model.LifecyclePhaseProvisioned,
		model.LifecyclePhaseActive,
		model.LifecyclePhaseDraining,
		model.LifecyclePhaseFrozen,
		model.LifecyclePhaseArchived,
		model.LifecyclePhaseLegacy,
	} {
		acc := mkAccount("A001", 42, phase)
		r, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseQuarantined)
		if err != nil {
			t.Fatalf("phase=%s err=%v", phase, err)
		}
		if !r.Allowed {
			t.Errorf("quarantined must be reachable from %s, got reason=%s", phase, r.Reason)
		}
	}
}

// ============================================================================
// 反向验证 — 不允许的转换不会被"开后门"绕过
// ============================================================================

func TestNoBackdoor_ActiveToFrozen(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkAccount("A001", 42, model.LifecyclePhaseActive)
	acc.Balance = 0
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if r.Allowed {
		t.Fatal("active->frozen must be illegal; bypass would defeat draining")
	}
}

func TestNoBackdoor_DrainingToActive(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkAccount("A001", 42, model.LifecyclePhaseDraining)
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseActive)
	if r.Allowed {
		t.Fatal("draining->active must be illegal (revival forbidden)")
	}
}

// ============================================================================
// AnchorStateMachine — wrapper validation
// ============================================================================

func TestAnchorStateMachine_DelegatesToModel(t *testing.T) {
	sm := NewAnchorStateMachine()
	cases := []struct {
		from, to model.AnchorStatus
		allow    bool
	}{
		{model.AnchorStatusTrying, model.AnchorStatusActive, true},
		{model.AnchorStatusTrying, model.AnchorStatusSettled, true},
		{model.AnchorStatusActive, model.AnchorStatusMigrated, true},
		{model.AnchorStatusSettled, model.AnchorStatusActive, false}, // 终态不可逆
		{model.AnchorStatusStuck, model.AnchorStatusActive, false},  // 必须经运维
	}
	for _, c := range cases {
		got := sm.CheckTransition(c.from, c.to)
		if got.Allowed != c.allow {
			t.Errorf("CheckTransition(%s,%s) allowed=%v want %v reason=%q",
				c.from, c.to, got.Allowed, c.allow, got.Reason)
		}
	}
}

// ============================================================================
// WrapGuardError 桥接
// ============================================================================

func TestWrapGuardError(t *testing.T) {
	// Allowed=true → nil
	if err := WrapGuardError(PhaseGuardResult{Allowed: true}); err != nil {
		t.Errorf("allowed must wrap to nil, got %v", err)
	}
	// Allowed=false → wraps ErrTransitionGuardRejected
	err := WrapGuardError(PhaseGuardResult{Allowed: false, Reason: "some reason"})
	if !errors.Is(err, ErrTransitionGuardRejected) {
		t.Errorf("rejected must wrap ErrTransitionGuardRejected, got %v", err)
	}
	if err == nil || !contains(err.Error(), "some reason") {
		t.Errorf("error must carry Reason, got %v", err)
	}
}

func TestWrapAnchorGuardError(t *testing.T) {
	if err := WrapAnchorGuardError(AnchorTransitionResult{Allowed: true}); err != nil {
		t.Errorf("allowed must wrap to nil, got %v", err)
	}
	err := WrapAnchorGuardError(AnchorTransitionResult{Allowed: false, Reason: "x"})
	if !errors.Is(err, ErrTransitionGuardRejected) {
		t.Errorf("must wrap ErrTransitionGuardRejected, got %v", err)
	}
}

// ============================================================================
// 工具
// ============================================================================

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// 防 unused-imports warnings for ptrInt / ptrStr if 后续测试不用
var _ = ptrInt
var _ = ptrStr
var _ = fmt.Sprint
