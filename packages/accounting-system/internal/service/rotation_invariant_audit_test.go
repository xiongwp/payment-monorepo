package service

import (
	"context"
	"testing"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

type fakeAuditReader struct {
	las                  []*model.LogicalAccount
	activeCountByLA      map[int64]int
	actualActiveByLA     map[int64]string
	archivedBad []*model.Account
	deepAnchors []*model.TxAccountAnchor
}

func (f *fakeAuditReader) ListAllRotatingLAs(_ context.Context, _ int) ([]*model.LogicalAccount, error) {
	return f.las, nil
}
func (f *fakeAuditReader) CountInstancesByPhase(_ context.Context, laID int64, phase model.LifecyclePhase) (int, error) {
	if phase == model.LifecyclePhaseActive {
		return f.activeCountByLA[laID], nil
	}
	return 0, nil
}
func (f *fakeAuditReader) GetActiveAccountNoForLogical(_ context.Context, laID int64) (string, int, error) {
	return f.actualActiveByLA[laID], f.activeCountByLA[laID], nil
}
func (f *fakeAuditReader) ListArchivedNonZeroBalance(_ context.Context, _ int) ([]*model.Account, error) {
	return f.archivedBad, nil
}
func (f *fakeAuditReader) ListAnchorsWithChainDepthExceeded(_ context.Context, _ int8, _ int) ([]*model.TxAccountAnchor, error) {
	return f.deepAnchors, nil
}

type fakeAuditHealer struct {
	realigned    []int64
	quarantined  []string
	realignErr   error
	quarantineErr error
}

func (f *fakeAuditHealer) RealignLogicalActive(_ context.Context, laID int64, _ string) error {
	if f.realignErr != nil {
		return f.realignErr
	}
	f.realigned = append(f.realigned, laID)
	return nil
}
func (f *fakeAuditHealer) QuarantineInstance(_ context.Context, accountNo string, _ string) error {
	if f.quarantineErr != nil {
		return f.quarantineErr
	}
	f.quarantined = append(f.quarantined, accountNo)
	return nil
}

func newAuditFixture() (*InvariantAuditJob, *fakeAuditReader, *fakeAuditHealer) {
	r := &fakeAuditReader{
		activeCountByLA:  map[int64]int{},
		actualActiveByLA: map[int64]string{},
	}
	h := &fakeAuditHealer{}
	j := NewInvariantAuditJob(r, h, "test-auditor", func() time.Time {
		return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	})
	return j, r, h
}

// 健康系统 → 无 violations
func TestAudit_HealthySystem_NoViolations(t *testing.T) {
	j, r, _ := newAuditFixture()
	la := mkLogicalAccount(42, "transit:foo:USD", true, "A001")
	r.las = []*model.LogicalAccount{la}
	r.activeCountByLA[42] = 1
	r.actualActiveByLA[42] = "A001"

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 0 {
		t.Errorf("healthy should have 0 violations, got %v", res.Violations)
	}
}

// I1 双 active → 仅 legacy 模式（rotation_enabled=false）才报；fleet 模式下双 active
// 是正常（fleet 100 sub 全 active），所以不报。
//
// 新设计：fleet 模式下"跨 group 双 active"才是真违反；reader 暂不暴露 group 维度
// → fleet I1 检查留 TODO（rotation_invariant_audit.go 里有注释）。
func TestAudit_I1MultipleActive_LegacyMode(t *testing.T) {
	j, r, _ := newAuditFixture()
	la := mkLogicalAccount(42, "transit:foo:USD", false, "A001") // rotation_enabled=false → legacy 模式
	r.las = []*model.LogicalAccount{la}
	r.activeCountByLA[42] = 2 // 双 active
	r.actualActiveByLA[42] = "A001"

	res, _ := j.Run(context.Background())
	found := false
	for _, v := range res.Violations {
		if v.Type == ViolationI1MultipleActive {
			found = true
			if v.Severity != "P0" {
				t.Errorf("I1 severity should be P0, got %s", v.Severity)
			}
		}
	}
	if !found {
		t.Error("missing I1 violation in legacy mode")
	}
}

// Fleet 模式下双 active 不报 I1（设计）
func TestAudit_FleetModeMultipleActive_NoViolation(t *testing.T) {
	j, r, _ := newAuditFixture()
	la := mkLogicalAccount(42, "transit:foo:USD", true, "A001") // rotation_enabled=true → fleet 模式
	r.las = []*model.LogicalAccount{la}
	r.activeCountByLA[42] = 100 // fleet 全 active
	r.actualActiveByLA[42] = "A001"

	res, _ := j.Run(context.Background())
	for _, v := range res.Violations {
		if v.Type == ViolationI1MultipleActive {
			t.Errorf("fleet mode should NOT report I1 with %d active: %+v", r.activeCountByLA[42], v)
		}
	}
}

// LA mismatch (legacy 模式) → P1, auto-heal
func TestAudit_LAMismatch_AutoHealed(t *testing.T) {
	j, r, h := newAuditFixture()
	la := mkLogicalAccount(42, "transit:foo:USD", false, "A001") // legacy 模式才严格 1:1 比较
	r.las = []*model.LogicalAccount{la}
	r.activeCountByLA[42] = 1
	r.actualActiveByLA[42] = "A002" // 实际 phase=active 是 A002

	res, _ := j.Run(context.Background())
	if len(h.realigned) != 1 || h.realigned[0] != 42 {
		t.Errorf("expected realign for la=42, got %v", h.realigned)
	}
	if res.AutoHealedNum != 1 {
		t.Errorf("expected 1 auto-healed, got %d", res.AutoHealedNum)
	}
}

// LA mismatch + 双 active (legacy 模式) → 不自愈
func TestAudit_LAMismatchWithMultipleActive_NotHealed(t *testing.T) {
	j, r, h := newAuditFixture()
	la := mkLogicalAccount(42, "transit:foo:USD", false, "A001") // legacy 模式
	r.las = []*model.LogicalAccount{la}
	r.activeCountByLA[42] = 2
	r.actualActiveByLA[42] = "A002"

	_, _ = j.Run(context.Background())
	if len(h.realigned) != 0 {
		t.Error("LA mismatch should NOT auto-heal when active count != 1")
	}
}

// I4 archived 余额非零 → P0
func TestAudit_ArchivedNonZero(t *testing.T) {
	j, r, _ := newAuditFixture()
	laRef := int64(42)
	r.archivedBad = []*model.Account{{
		AccountNo:        "A999",
		LogicalAccountID: &laRef,
		LifecyclePhase:   model.LifecyclePhaseArchived,
		Balance:          100,
	}}

	res, _ := j.Run(context.Background())
	found := false
	for _, v := range res.Violations {
		if v.Type == ViolationI4ArchivedNonZeroBalance {
			found = true
			if v.Severity != "P0" {
				t.Errorf("severity should be P0, got %s", v.Severity)
			}
		}
	}
	if !found {
		t.Error("should report I4 violation")
	}
}

// TestAudit_MigrationSuspenseNonZero 已删除（MigrationSuspense business_type 移除，I-MS 不变量随之删除）

// Chain depth 超限 → P1, auto-quarantine
func TestAudit_ChainDepthExceeded_AutoQuarantines(t *testing.T) {
	j, r, h := newAuditFixture()
	r.deepAnchors = []*model.TxAccountAnchor{
		{ID: 100, FlowID: "F1", AccountNo: "A001", MigrationChainDepth: 6},
		{ID: 101, FlowID: "F2", AccountNo: "A002", MigrationChainDepth: 7},
	}

	res, _ := j.Run(context.Background())
	if len(h.quarantined) != 2 {
		t.Errorf("expected 2 quarantines, got %d", len(h.quarantined))
	}
	if res.AutoHealedNum != 2 {
		t.Errorf("expected 2 auto-healed, got %d", res.AutoHealedNum)
	}
}

// 多违反复合：I1 + LA mismatch + I4 + 全局 + chain → 全部报告
func TestAudit_MultipleViolations_AllReported(t *testing.T) {
	j, r, _ := newAuditFixture()
	la := mkLogicalAccount(42, "transit:foo:USD", true, "A001")
	r.las = []*model.LogicalAccount{la}
	r.activeCountByLA[42] = 2 // I1 violation
	r.actualActiveByLA[42] = "A002"
	laRef := int64(42)
	r.archivedBad = []*model.Account{
		{AccountNo: "OLD", LogicalAccountID: &laRef, Balance: 1, LifecyclePhase: model.LifecyclePhaseArchived},
	}
	// migrationSuspenseSum 已删除（MigrationSuspense business_type 移除）
	r.deepAnchors = []*model.TxAccountAnchor{
		{ID: 1, MigrationChainDepth: 6},
	}

	res, _ := j.Run(context.Background())
	// 期望：I1 + LA mismatch + I4 + Chain = 4 violations (原来还有 MS = 5，MS 删除后剩 4)
	if len(res.Violations) < 4 {
		t.Errorf("expected at least 4 violations, got %d: %v", len(res.Violations), res.Violations)
	}
}
