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

// ============================================================================
// EDGE CASES — 补充覆盖
// ============================================================================

// nil clock 必须 fallback 到 time.Now
func TestNewInstanceStateMachine_NilClockFallsBack(t *testing.T) {
	sm := NewInstanceStateMachine(
		&fakeAccountReader{},
		&fakeAnchorReader{},
		&fakePolicyReader{},
		&fakeAnchorShards{},
		nil, // 不能 panic
	)
	now := sm.clock()
	if now.IsZero() {
		t.Fatal("nil clock fallback should return real time, got zero")
	}
	// 应该在合理范围内（最近 10 秒）
	if time.Since(now) > 10*time.Second || time.Since(now) < 0 {
		t.Errorf("clock fallback returned suspicious time: %v", now)
	}
}

// guardEnterFrozen: drain age 精确边界
// age == drain_p99 → 允许（>= 不是严格 >）
// age == drain_p99 - 1 second → 拒
func TestGuardEnterFrozen_DrainAgePrecision(t *testing.T) {
	now := time.Now()
	sm, _, _, pr := newFixture(now)
	pr.policy.DrainP99Seconds = 86400 // 1 day

	// 恰好 1 天前
	acc := mkAccount("A001", 42, model.LifecyclePhaseDraining)
	acc.DrainingStartedAt = ptrTime(now.Add(-86400 * time.Second))
	acc.Balance = 0
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if !r.Allowed {
		t.Errorf("age == drain_p99 must be allowed (boundary >=), got %q", r.Reason)
	}

	// 比 1 天少 1 秒
	acc2 := mkAccount("A001", 42, model.LifecyclePhaseDraining)
	acc2.DrainingStartedAt = ptrTime(now.Add(-86399 * time.Second))
	acc2.Balance = 0
	r2, _ := sm.CheckTransition(context.Background(), acc2, model.LifecyclePhaseFrozen)
	if r2.Allowed {
		t.Errorf("age = drain_p99 - 1s must be blocked")
	}
}

// guardEnterFrozen: balance 边界
//  balance=0 → 允（其他条件满足时）
//  balance=1 → 拒
//  balance=-1 → 拒（负余额也是非零，是 anomaly）
//  balance=MaxInt64 → 拒
func TestGuardEnterFrozen_BalanceBoundaries(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	for _, b := range []int64{1, -1, 100, -100, 1 << 62} {
		acc := mkDrainingAccount(now, 8, b)
		r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
		if r.Allowed {
			t.Errorf("balance=%d should block frozen", b)
		}
	}
	// balance=0 满足其他条件 → 允
	accZero := mkDrainingAccount(now, 8, 0)
	r, _ := sm.CheckTransition(context.Background(), accZero, model.LifecyclePhaseFrozen)
	if !r.Allowed {
		t.Errorf("balance=0 should allow (when other conditions met), got %q", r.Reason)
	}
}

// guardEnterFrozen: 部分 shard IO 失败必须传播错误（不能用"部分数据"判定收敛）
func TestGuardEnterFrozen_PartialShardFailurePropagates(t *testing.T) {
	now := time.Now()

	failingAnchor := &flakyAnchorReader{
		// shard 50 失败，其余成功
		failOnShard: 50,
	}
	sm := NewInstanceStateMachine(
		&fakeAccountReader{},
		failingAnchor,
		&fakePolicyReader{
			policy: &model.LogicalAccountRotationPolicy{
				LogicalAccountID:     42,
				DrainP99Seconds:      1,
				DrainHardTimeoutSecs: 100,
				ArchiveGraceSecs:     0,
				PeriodUnit:           model.PeriodUnitMonth,
				PeriodCount:          1,
				RotationAnchorTZ:     "UTC",
			},
		},
		&fakeAnchorShards{ids: []int{0, 1, 50, 99}},
		func() time.Time { return now },
	)
	acc := mkDrainingAccount(now, 100, 0)
	_, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if err == nil {
		t.Fatal("partial shard failure must propagate; absence implies dangerous default")
	}
}

// flakyAnchorReader 实现 AnchorReaderForPhase：在指定 shard 报错。
type flakyAnchorReader struct {
	failOnShard int
}

func (f *flakyAnchorReader) CountOpenByAccountNo(_ context.Context, _ string, gtbl int) (int64, error) {
	if gtbl == f.failOnShard {
		return 0, errors.New("shard read timeout")
	}
	return 0, nil
}

func (f *flakyAnchorReader) CountStuckByAccountNo(_ context.Context, _ string, gtbl int) (int64, error) {
	if gtbl == f.failOnShard {
		return 0, errors.New("shard read timeout")
	}
	return 0, nil
}

func (f *flakyAnchorReader) OldestOpenAnchoredAt(_ context.Context, _ string, _ int) (*time.Time, error) {
	return nil, nil
}

// guardEnterActive: PeriodStart 精确等于 now → 允许（不超前即允）
func TestGuardEnterActive_PeriodStartExactlyNow(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkAccount("A001", 42, model.LifecyclePhaseProvisioned)
	acc.PeriodStart = ptrTime(now)
	r, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseActive)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Allowed {
		t.Errorf("period_start == now should be allowed, got %q", r.Reason)
	}
}

// guardEnterActive: I1 不变量 — 更高 active 计数也要拒
func TestGuardEnterActive_I1MultipleActiveBlocks(t *testing.T) {
	now := time.Now()
	for _, cnt := range []int{1, 2, 5, 100} {
		sm, ar, _, _ := newFixture(now)
		ar.activeCountByLogical[42] = cnt
		acc := mkAccount("A002", 42, model.LifecyclePhaseProvisioned)
		r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseActive)
		if r.Allowed {
			t.Errorf("active count=%d must block (I1)", cnt)
		}
	}
}

// guardEnterArchived: archive_grace=0 → 即时归档允许
func TestGuardEnterArchived_ZeroGraceImmediateArchive(t *testing.T) {
	now := time.Now()
	sm, _, _, pr := newFixture(now)
	pr.policy.ArchiveGraceSecs = 0

	acc := mkFrozenAccount(now, 0, 0) // frozen 0 hours ago, balance=0, grace=0
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseArchived)
	if !r.Allowed {
		t.Errorf("archive_grace=0 should allow immediate archive, got %q", r.Reason)
	}
}

// guardEnterArchived: grace 精确边界（now == frozen + grace → 允；now < → 拒）
func TestGuardEnterArchived_GracePrecision(t *testing.T) {
	now := time.Now()
	sm, _, _, pr := newFixture(now)
	pr.policy.ArchiveGraceSecs = 3600 // 1 hour

	// 恰好 1 小时前 frozen
	acc := mkAccount("A001", 42, model.LifecyclePhaseFrozen)
	acc.FrozenAt = ptrTime(now.Add(-3600 * time.Second))
	acc.Balance = 0
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseArchived)
	if !r.Allowed {
		t.Errorf("now == frozen+grace boundary should allow, got %q", r.Reason)
	}

	// 1 小时 - 1 毫秒前 frozen
	acc2 := mkAccount("A001", 42, model.LifecyclePhaseFrozen)
	acc2.FrozenAt = ptrTime(now.Add(-3600*time.Second + time.Millisecond))
	acc2.Balance = 0
	r2, _ := sm.CheckTransition(context.Background(), acc2, model.LifecyclePhaseArchived)
	if r2.Allowed {
		t.Errorf("grace - 1ms should block")
	}
}

// 没有 anchor shard 配置（空切片）—— sumAnchorCounters 返回 0,0
// 验证 frozen 允许（其他条件满足时）。这是 ops 在极端情况下手动配置的可能。
func TestGuardEnterFrozen_EmptyShardList(t *testing.T) {
	now := time.Now()
	emptyShards := &fakeAnchorShards{ids: []int{}}
	sm := NewInstanceStateMachine(
		&fakeAccountReader{},
		&fakeAnchorReader{},
		&fakePolicyReader{
			policy: &model.LogicalAccountRotationPolicy{
				LogicalAccountID: 42, DrainP99Seconds: 1, DrainHardTimeoutSecs: 1,
				ArchiveGraceSecs: 0, PeriodUnit: model.PeriodUnitDay, PeriodCount: 1,
				RotationAnchorTZ: "UTC",
			},
		},
		emptyShards,
		func() time.Time { return now },
	)
	acc := mkDrainingAccount(now, 100, 0)
	r, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !r.Allowed {
		t.Errorf("empty shard list should allow (vacuously zero anchors), got %q", r.Reason)
	}
}

// CheckTransition 自环（from==to）必须拒（不允许自反转换，与 model 层一致）
func TestCheckTransition_SelfLoopRejected(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	for _, phase := range []model.LifecyclePhase{
		model.LifecyclePhaseActive,
		model.LifecyclePhaseDraining,
		model.LifecyclePhaseFrozen,
	} {
		acc := mkAccount("A001", 42, phase)
		r, _ := sm.CheckTransition(context.Background(), acc, phase)
		if r.Allowed {
			t.Errorf("self-loop %s->%s must be rejected", phase, phase)
		}
	}
}

// WrapGuardError: 验证可被 errors.Is 检测且保留 Reason
func TestWrapGuardError_PreservesChain(t *testing.T) {
	err := WrapGuardError(PhaseGuardResult{Allowed: false, Reason: "some detail"})
	// 可被 errors.Is 识别
	if !errors.Is(err, ErrTransitionGuardRejected) {
		t.Error("must be detectable via errors.Is")
	}
	// Reason 保留
	if !contains(err.Error(), "some detail") {
		t.Errorf("error should preserve Reason, got %q", err.Error())
	}
	// 可以再 wrap 一层不丢
	wrapped := fmt.Errorf("upstream context: %w", err)
	if !errors.Is(wrapped, ErrTransitionGuardRejected) {
		t.Error("wrap chain should preserve ErrTransitionGuardRejected detection")
	}
}

// AnchorStateMachine 全 5x5 矩阵穷举
func TestAnchorStateMachine_FullMatrix(t *testing.T) {
	sm := NewAnchorStateMachine()
	all := []model.AnchorStatus{
		model.AnchorStatusTrying, model.AnchorStatusActive,
		model.AnchorStatusSettled, model.AnchorStatusMigrated, model.AnchorStatusStuck,
	}
	for _, f := range all {
		for _, to := range all {
			expected := model.CanTransitionAnchor(f, to)
			got := sm.CheckTransition(f, to)
			if got.Allowed != expected {
				t.Errorf("service.CheckTransition(%s,%s)=%v but model.CanTransitionAnchor=%v",
					f, to, got.Allowed, expected)
			}
		}
	}
}

// ============================================================================
// 组合守卫：多个 blocker 同时存在
// ============================================================================

// 当 balance+age 同时违反时，至少一个原因被报出（不要求两个都报，但不能两个都漏）
func TestGuardEnterFrozen_MultipleBlockers(t *testing.T) {
	now := time.Now()
	sm, _, _, _ := newFixture(now)
	acc := mkDrainingAccount(now, 3, 100) // age 3d < p99 7d, balance != 0
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if r.Allowed {
		t.Fatal("multiple blockers must reject frozen")
	}
	// 至少有一个原因被报出
	if r.Reason == "" {
		t.Error("Reason must not be empty when blocked")
	}
}

// I1 + 时序同时违反：I1 优先（无论 PeriodStart 是否到位，I1 优先拒）
func TestGuardEnterActive_I1AndTimingBothBlockers(t *testing.T) {
	now := time.Now()
	sm, ar, _, _ := newFixture(now)
	ar.activeCountByLogical[42] = 1
	acc := mkAccount("A002", 42, model.LifecyclePhaseProvisioned)
	acc.PeriodStart = ptrTime(now.Add(1 * time.Hour)) // also too early
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseActive)
	if r.Allowed {
		t.Fatal("both I1 and timing should block")
	}
	if !contains(r.Reason, "I1") {
		// I1 第一个查，且永远应该报 I1 先
		t.Errorf("I1 should be reported first; got %q", r.Reason)
	}
}

// ============================================================================
// 跨多个 active 数：I1 在 1/2/5/100 全部值上拒
// ============================================================================
// (already covered by TestGuardEnterActive_I1MultipleActiveBlocks)

// ============================================================================
// PROPERTY-BASED：随机生成大量场景，状态机 result 必须与 model.CanTransition* 一致
// 即"无业务守卫时"service 层判定 = model 层判定
// 当账户满足"无业务约束"配置时（无 logical_account_id 限制下的合法路径仅 quarantined）
// 业务守卫之外的纯图判定必须一致。
// ============================================================================

func TestPropertyBased_ServiceMatchesModelForGraphLegality(t *testing.T) {
	now := time.Now()

	allPhases := []model.LifecyclePhase{
		model.LifecyclePhaseLegacy, model.LifecyclePhaseProvisioned,
		model.LifecyclePhaseActive, model.LifecyclePhaseDraining,
		model.LifecyclePhaseFrozen, model.LifecyclePhaseArchived,
		model.LifecyclePhaseQuarantined,
	}

	for _, from := range allPhases {
		for _, to := range allPhases {
			modelAllowed := model.CanTransitionPhase(from, to)
			// 图非法时，service 必拒
			if !modelAllowed {
				sm, _, _, _ := newFixture(now)
				acc := mkAccount("A001", 42, from)
				r, err := sm.CheckTransition(context.Background(), acc, to)
				if err != nil {
					t.Errorf("from=%s to=%s unexpected err %v", from, to, err)
				}
				if r.Allowed {
					t.Errorf("model.CanTransition(%s,%s)=false but service allowed", from, to)
				}
			}
		}
	}
}

// ============================================================================
// 错误传播深度
// ============================================================================

// 多层错误包装下仍能识别 ErrTransitionGuardRejected
func TestWrapGuardError_DeepWrap(t *testing.T) {
	err := WrapGuardError(PhaseGuardResult{Allowed: false, Reason: "root cause"})
	// 经 5 层包装
	for i := 0; i < 5; i++ {
		err = fmt.Errorf("layer %d: %w", i, err)
	}
	if !errors.Is(err, ErrTransitionGuardRejected) {
		t.Error("must detect ErrTransitionGuardRejected through 5 wrap layers")
	}
	if !contains(err.Error(), "root cause") {
		t.Error("must preserve original Reason in deeply wrapped chain")
	}
}

// ============================================================================
// 时钟边界
// ============================================================================

// 闰秒附近 / unix 0 / 远未来 — clock 不能拒绝奇怪的时间值
func TestClockInjection_AcceptsExtremeValues(t *testing.T) {
	extremes := []time.Time{
		time.Unix(0, 0),                              // unix epoch
		time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC),  // pre-epoch
		time.Date(2099, 12, 31, 23, 59, 59, 999999999, time.UTC), // far future
	}
	for _, t0 := range extremes {
		sm, _, _, _ := newFixture(t0)
		acc := mkAccount("A001", 42, model.LifecyclePhaseProvisioned)
		acc.PeriodStart = ptrTime(t0) // exactly now (extreme time)
		r, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseActive)
		if err != nil {
			t.Errorf("clock=%v unexpected err %v", t0, err)
		}
		_ = r
	}
}

// ============================================================================
// 业务守卫边界：drain age + balance + open + stuck 全 0 (完美 happy path)
// ============================================================================

func TestGuardEnterFrozen_PerfectHappyPath(t *testing.T) {
	now := time.Now()
	sm, _, anr, pr := newFixture(now)
	// 严格的最小 P99: 1 秒
	pr.policy.DrainP99Seconds = 1

	acc := mkAccount("A001", 42, model.LifecyclePhaseDraining)
	acc.DrainingStartedAt = ptrTime(now.Add(-1 * time.Second))
	acc.Balance = 0
	// 每个 shard 都 0
	for _, gtbl := range []int{0, 1, 2, 50, 99} {
		anr.openByShard[gtbl] = 0
		anr.stuckByShard[gtbl] = 0
	}

	r, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if err != nil {
		t.Fatalf("unexpected err %v", err)
	}
	if !r.Allowed {
		t.Errorf("perfect happy path should allow; got %q", r.Reason)
	}
}

// ============================================================================
// 大量 anchor shard：100 个全 0 也要正确求和
// ============================================================================

func TestGuardEnterFrozen_All100ShardsZero(t *testing.T) {
	now := time.Now()

	allShards := make([]int, 100)
	for i := range allShards {
		allShards[i] = i
	}
	shards := &fakeAnchorShards{ids: allShards}

	sm := NewInstanceStateMachine(
		&fakeAccountReader{},
		&fakeAnchorReader{
			openByShard:  map[int]int64{}, // all default 0
			stuckByShard: map[int]int64{},
		},
		&fakePolicyReader{
			policy: &model.LogicalAccountRotationPolicy{
				LogicalAccountID: 42, DrainP99Seconds: 1, DrainHardTimeoutSecs: 1,
				ArchiveGraceSecs: 0, PeriodUnit: model.PeriodUnitDay, PeriodCount: 1,
				RotationAnchorTZ: "UTC",
			},
		},
		shards,
		func() time.Time { return now },
	)
	acc := mkDrainingAccount(now, 10, 0)
	r, err := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Allowed {
		t.Errorf("all 100 shards zero should allow; got %q", r.Reason)
	}
}

// 一个 shard 有 1 笔 open，其他 99 全 0 —— 必须拒
func TestGuardEnterFrozen_SingleAnchorAcrossManyShards(t *testing.T) {
	now := time.Now()
	allShards := make([]int, 100)
	for i := range allShards {
		allShards[i] = i
	}

	sm := NewInstanceStateMachine(
		&fakeAccountReader{},
		&fakeAnchorReader{
			openByShard:  map[int]int64{42: 1}, // shard 42 has 1 open
			stuckByShard: map[int]int64{},
		},
		&fakePolicyReader{
			policy: &model.LogicalAccountRotationPolicy{
				LogicalAccountID: 42, DrainP99Seconds: 1, DrainHardTimeoutSecs: 1,
				ArchiveGraceSecs: 0, PeriodUnit: model.PeriodUnitDay, PeriodCount: 1,
				RotationAnchorTZ: "UTC",
			},
		},
		&fakeAnchorShards{ids: allShards},
		func() time.Time { return now },
	)
	acc := mkDrainingAccount(now, 10, 0)
	r, _ := sm.CheckTransition(context.Background(), acc, model.LifecyclePhaseFrozen)
	if r.Allowed {
		t.Fatal("1 open anchor anywhere must block")
	}
	if !contains(r.Reason, "1 open anchor") {
		t.Errorf("reason should mention count, got %q", r.Reason)
	}
}
