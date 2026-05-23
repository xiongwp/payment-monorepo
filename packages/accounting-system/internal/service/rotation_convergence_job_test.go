package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// Fakes
// ============================================================================

type fakeDrainingLister struct {
	instances []*model.Account
	err       error
}

func (f *fakeDrainingLister) ListDraining(_ context.Context, _ int) ([]*model.Account, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.instances, nil
}

type fakePromoter struct {
	promotions []string
	err        error
}

func (f *fakePromoter) PromoteToFrozen(_ context.Context, accountNo string, _ int64, _ time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.promotions = append(f.promotions, accountNo)
	return nil
}

func newConvergenceFixture(now time.Time) (
	*ConvergenceJob, *fakeDrainingLister, *fakePromoter, *fakeLockManager,
	*fakeAccountReaderForPhase, *fakeAnchorReaderForPhaseStub, *fakePolicyReader,
) {
	dl := &fakeDrainingLister{}
	pm := &fakePromoter{}
	lm := &fakeLockManager{heldBy: map[int64]string{}, failFor: map[int64]bool{}}
	ar := &fakeAccountReaderForPhase{
		activeCountByLogical: map[int64]int{},
		accountsByNo:         map[string]*model.Account{},
	}
	anr := &fakeAnchorReaderForPhaseStub{
		openByAccount:  map[string]int64{},
		stuckByAccount: map[string]int64{},
	}
	pr := &fakePolicyReader{
		policy: &model.LogicalAccountRotationPolicy{
			LogicalAccountID:     42,
			PeriodUnit:           model.PeriodUnitMonth,
			DrainP99Seconds:      7 * 86400,
			DrainHardTimeoutSecs: 14 * 86400,
		},
	}
	sm := NewInstanceStateMachine(ar, anr, pr, func() time.Time { return now })
	cj := NewConvergenceJob(dl, sm, pm, lm, "test-worker", func() time.Time { return now })
	return cj, dl, pm, lm, ar, anr, pr
}

func mkDrainingInstance(no string, laID int64, drainStartedAt time.Time, balance int64) *model.Account {
	la := laID
	return &model.Account{
		AccountNo:         no,
		LogicalAccountID:  &la,
		LifecyclePhase:    model.LifecyclePhaseDraining,
		Balance:           balance,
		DrainingStartedAt: &drainStartedAt,
		Version:           1,
	}
}

// ============================================================================
// 基本路径
// ============================================================================

func TestConvergenceJob_NoDrainingInstances(t *testing.T) {
	cj, _, _, _, _, _, _ := newConvergenceFixture(time.Now())
	res, err := cj.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 0 {
		t.Errorf("expected 0 scanned, got %d", res.Scanned)
	}
}

// happy path：所有条件满足 → 推进
func TestConvergenceJob_HappyPath_Converges(t *testing.T) {
	now := time.Now()
	cj, dl, pm, _, _, _, _ := newConvergenceFixture(now)
	// drain age = 8 天，> p99 7 天
	inst := mkDrainingInstance("A001", 42, now.Add(-8*24*time.Hour), 0)
	dl.instances = []*model.Account{inst}

	res, _ := cj.Tick(context.Background())
	if res.Converged != 1 {
		t.Errorf("expected 1 converged, got %d", res.Converged)
	}
	if len(pm.promotions) != 1 || pm.promotions[0] != "A001" {
		t.Errorf("promotion list wrong: %v", pm.promotions)
	}
}

// 余额非零 → 不推进
func TestConvergenceJob_BalanceNonZero_NotReady(t *testing.T) {
	now := time.Now()
	cj, dl, pm, _, _, _, _ := newConvergenceFixture(now)
	inst := mkDrainingInstance("A001", 42, now.Add(-8*24*time.Hour), 100) // balance=100
	dl.instances = []*model.Account{inst}

	res, _ := cj.Tick(context.Background())
	if res.Converged != 0 {
		t.Errorf("should not converge with balance!=0")
	}
	if res.NotReady != 1 {
		t.Errorf("expected not_ready=1")
	}
	if len(pm.promotions) != 0 {
		t.Errorf("should not promote")
	}
}

// open anchor 存在 → 不推进
func TestConvergenceJob_OpenAnchor_NotReady(t *testing.T) {
	now := time.Now()
	cj, dl, pm, _, _, anr, _ := newConvergenceFixture(now)
	inst := mkDrainingInstance("A001", 42, now.Add(-8*24*time.Hour), 0)
	anr.openByAccount["A001"] = 1
	dl.instances = []*model.Account{inst}

	res, _ := cj.Tick(context.Background())
	if res.Converged != 0 {
		t.Errorf("should not converge with open anchor")
	}
	if len(pm.promotions) != 0 {
		t.Error("should not promote")
	}
}

// stuck anchor → 不推进
func TestConvergenceJob_StuckAnchor_NotReady(t *testing.T) {
	now := time.Now()
	cj, dl, pm, _, _, anr, _ := newConvergenceFixture(now)
	inst := mkDrainingInstance("A001", 42, now.Add(-8*24*time.Hour), 0)
	anr.stuckByAccount["A001"] = 1
	dl.instances = []*model.Account{inst}

	res, _ := cj.Tick(context.Background())
	if res.Converged != 0 {
		t.Errorf("stuck blocks frozen")
	}
	if len(pm.promotions) != 0 {
		t.Error("stuck should prevent promote")
	}
}

// drain age < p99 → 不推进
func TestConvergenceJob_DrainAgeYoung_NotReady(t *testing.T) {
	now := time.Now()
	cj, dl, _, _, _, _, _ := newConvergenceFixture(now)
	inst := mkDrainingInstance("A001", 42, now.Add(-1*24*time.Hour), 0) // 1 天 < 7 天
	dl.instances = []*model.Account{inst}

	res, _ := cj.Tick(context.Background())
	if res.Converged != 0 {
		t.Errorf("age<p99 blocks")
	}
}

// CAS 冲突 → 视作幂等成功
func TestConvergenceJob_CASConflictTreatedAsSuccess(t *testing.T) {
	now := time.Now()
	cj, dl, pm, _, _, _, _ := newConvergenceFixture(now)
	pm.err = ErrInstanceVersionConflict
	inst := mkDrainingInstance("A001", 42, now.Add(-8*24*time.Hour), 0)
	dl.instances = []*model.Account{inst}

	res, _ := cj.Tick(context.Background())
	if res.Converged != 1 {
		t.Errorf("CAS conflict should be counted as converged (idempotent)")
	}
	if len(res.Errors) != 0 {
		t.Errorf("CAS conflict should not error: %v", res.Errors)
	}
}

// 真实推进错误 → 报错
func TestConvergenceJob_PromoteError(t *testing.T) {
	now := time.Now()
	cj, dl, pm, _, _, _, _ := newConvergenceFixture(now)
	pm.err = errors.New("db down")
	inst := mkDrainingInstance("A001", 42, now.Add(-8*24*time.Hour), 0)
	dl.instances = []*model.Account{inst}

	res, _ := cj.Tick(context.Background())
	if len(res.Errors) != 1 {
		t.Errorf("real promote error should be reported")
	}
}

// 锁竞争 → 跳过（不报错）
func TestConvergenceJob_LockContention_SkipNoError(t *testing.T) {
	now := time.Now()
	cj, dl, pm, lm, _, _, _ := newConvergenceFixture(now)
	inst := mkDrainingInstance("A001", 42, now.Add(-8*24*time.Hour), 0)
	dl.instances = []*model.Account{inst}
	lm.failFor[42] = true

	res, _ := cj.Tick(context.Background())
	if len(pm.promotions) != 0 {
		t.Error("locked instance should be skipped")
	}
	if len(res.Errors) != 0 {
		t.Errorf("lock contention should not error: %v", res.Errors)
	}
}

// 多 instance：一个失败不影响另一个
func TestConvergenceJob_MultipleInstances_PartialFailureIsolated(t *testing.T) {
	now := time.Now()
	cj, dl, _, _, _, _, _ := newConvergenceFixture(now)
	// 第一个 happy，第二个 logical_account_id 缺失
	inst1 := mkDrainingInstance("A001", 42, now.Add(-8*24*time.Hour), 0)
	inst2 := &model.Account{
		AccountNo:      "A002",
		LifecyclePhase: model.LifecyclePhaseDraining,
		// LogicalAccountID nil
	}
	dl.instances = []*model.Account{inst1, inst2}

	res, _ := cj.Tick(context.Background())
	if res.Scanned != 2 {
		t.Errorf("expected scan both")
	}
	if res.Converged != 1 {
		t.Errorf("first should converge, got %d", res.Converged)
	}
	if len(res.Errors) != 1 {
		t.Errorf("second should error")
	}
}

func TestConvergenceJob_ListError(t *testing.T) {
	cj, dl, _, _, _, _, _ := newConvergenceFixture(time.Now())
	dl.err = errors.New("db down")
	_, err := cj.Tick(context.Background())
	if err == nil {
		t.Fatal("list error should propagate")
	}
}
