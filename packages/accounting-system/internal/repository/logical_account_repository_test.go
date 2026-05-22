package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// LogicalAccountRepository.Register: 入参校验测试（无需 DB）
//
// repo 在调 DB 之前做一系列 fail-fast 校验，确保不会把脏数据落库。
// 这些是纯逻辑路径，可以在无 DB 环境下测全。
// ============================================================================

// 使用 nil dbManager 来构造仓储——只要测试触发的代码路径不到 DB 就 OK。
// 所有 Register 在传入 nil/empty/invalid 时应在 db 调用前返回。

func newRepoNoDB() LogicalAccountRepository {
	return &logicalAccountRepository{dbManager: nil}
}

func TestRegister_NilInput(t *testing.T) {
	r := newRepoNoDB()
	_, err := r.Register(context.Background(), nil)
	if err == nil {
		t.Fatal("nil LogicalAccount must error")
	}
}

func TestRegister_EmptyKey(t *testing.T) {
	r := newRepoNoDB()
	la := &model.LogicalAccount{
		LogicalAccountKey:   "",
		AccountType:         5,
		AccountBusinessType: 5,
		Currency:            "USD",
		RegisteredBy:        "ops",
	}
	_, err := r.Register(context.Background(), la)
	if err == nil {
		t.Fatal("empty key must error")
	}
	if !errors.Is(err, model.ErrInvalidLogicalAccountKey) {
		t.Errorf("expected ErrInvalidLogicalAccountKey, got %v", err)
	}
}

func TestRegister_BadKeyPrefix(t *testing.T) {
	r := newRepoNoDB()
	la := &model.LogicalAccount{
		LogicalAccountKey:   "unknown-prefix:alipay:USD",
		AccountType:         5,
		AccountBusinessType: 5,
		Currency:            "USD",
		RegisteredBy:        "ops",
	}
	_, err := r.Register(context.Background(), la)
	if err == nil {
		t.Fatal("bad prefix must error")
	}
	if !errors.Is(err, model.ErrInvalidLogicalAccountKey) {
		t.Errorf("expected ErrInvalidLogicalAccountKey, got %v", err)
	}
}

func TestRegister_MissingRegisteredBy(t *testing.T) {
	r := newRepoNoDB()
	la := &model.LogicalAccount{
		LogicalAccountKey:   "transit:foo:USD",
		AccountType:         5,
		AccountBusinessType: 5,
		Currency:            "USD",
		// RegisteredBy: "" — must error
	}
	_, err := r.Register(context.Background(), la)
	if err == nil {
		t.Fatal("missing registered_by must error")
	}
}

func TestRegister_MissingAccountType(t *testing.T) {
	r := newRepoNoDB()
	la := &model.LogicalAccount{
		LogicalAccountKey:   "transit:foo:USD",
		AccountType:         0, // missing
		AccountBusinessType: 5,
		Currency:            "USD",
		RegisteredBy:        "ops",
	}
	_, err := r.Register(context.Background(), la)
	if err == nil {
		t.Fatal("missing account_type must error")
	}
}

func TestRegister_MissingBusinessType(t *testing.T) {
	r := newRepoNoDB()
	la := &model.LogicalAccount{
		LogicalAccountKey:   "transit:foo:USD",
		AccountType:         5,
		AccountBusinessType: 0, // missing
		Currency:            "USD",
		RegisteredBy:        "ops",
	}
	_, err := r.Register(context.Background(), la)
	if err == nil {
		t.Fatal("missing business_type must error")
	}
}

func TestRegister_MissingCurrency(t *testing.T) {
	r := newRepoNoDB()
	la := &model.LogicalAccount{
		LogicalAccountKey:   "transit:foo:USD",
		AccountType:         5,
		AccountBusinessType: 5,
		Currency:            "",
		RegisteredBy:        "ops",
	}
	_, err := r.Register(context.Background(), la)
	if err == nil {
		t.Fatal("missing currency must error")
	}
}

// 所有 Register 错误路径在 dbManager=nil 时都不会 NPE
// 这是隐性的稳健性测试：所有 fail-fast 路径在 db 调用前返回
func TestRegister_NoDBCallWhenValidationFails(t *testing.T) {
	r := newRepoNoDB()
	// 所有上面的 case 都成功（即 nil dbManager 时不 panic）就证明
	// validation 在 db 调用前返回。再覆盖一些组合：
	bads := []*model.LogicalAccount{
		nil,
		{},
		{LogicalAccountKey: "transit:x:USD"},
		{LogicalAccountKey: "transit:x:USD", AccountType: 5},
		{LogicalAccountKey: "transit:x:USD", AccountType: 5, AccountBusinessType: 5},
		{LogicalAccountKey: "transit:x:USD", AccountType: 5, AccountBusinessType: 5, Currency: "USD"},
	}
	for i, b := range bads {
		_, err := r.Register(context.Background(), b)
		if err == nil {
			t.Errorf("case %d: expected validation error, got nil", i)
		}
	}
}

// ============================================================================
// GetByKey 边界
// ============================================================================

func TestGetByKey_EmptyKey(t *testing.T) {
	r := newRepoNoDB()
	_, err := r.GetByKey(context.Background(), "")
	if err == nil {
		t.Fatal("empty key must error")
	}
	if !errors.Is(err, model.ErrInvalidLogicalAccountKey) {
		t.Errorf("expected ErrInvalidLogicalAccountKey, got %v", err)
	}
}

// ============================================================================
// GetByID 边界
// ============================================================================

func TestGetByID_InvalidID(t *testing.T) {
	r := newRepoNoDB()
	for _, id := range []int64{0, -1, -100} {
		_, err := r.GetByID(context.Background(), id)
		if err == nil {
			t.Errorf("id=%d must error", id)
		}
	}
}

// ============================================================================
// UpdateCurrentActive 边界
// ============================================================================

func TestUpdateCurrentActive_InvalidArgs(t *testing.T) {
	r := newRepoNoDB()
	now := time.Now()
	// id <= 0
	err := r.UpdateCurrentActive(context.Background(), 0, "A001", "", now, 0)
	if err == nil {
		t.Error("id=0 must error")
	}
	// empty account_no
	err = r.UpdateCurrentActive(context.Background(), 1, "", "", now, 0)
	if err == nil {
		t.Error("empty account_no must error")
	}
}

// ============================================================================
// SetRotationEnabled 边界
// ============================================================================

func TestSetRotationEnabled_InvalidID(t *testing.T) {
	r := newRepoNoDB()
	err := r.SetRotationEnabled(context.Background(), 0, true, 0)
	if err == nil {
		t.Error("id=0 must error")
	}
}

// ============================================================================
// UpsertPolicy 边界
// ============================================================================

func TestUpsertPolicy_NilInput(t *testing.T) {
	r := newRepoNoDB()
	err := r.UpsertPolicy(context.Background(), nil)
	if err == nil {
		t.Fatal("nil policy must error")
	}
}

func TestUpsertPolicy_InvalidPolicy(t *testing.T) {
	r := newRepoNoDB()
	p := &model.LogicalAccountRotationPolicy{
		LogicalAccountID: 1,
		PeriodUnit:       "FORTNIGHT", // invalid
		PeriodCount:      1,
		RotationAnchorTZ: "UTC",
		DrainP99Seconds:  1, DrainHardTimeoutSecs: 1,
	}
	err := r.UpsertPolicy(context.Background(), p)
	if err == nil {
		t.Fatal("invalid PeriodUnit must error")
	}
}

// ============================================================================
// GetPolicy 边界
// ============================================================================

func TestGetPolicy_InvalidID(t *testing.T) {
	r := newRepoNoDB()
	for _, id := range []int64{0, -1} {
		_, err := r.GetPolicy(context.Background(), id)
		if err == nil {
			t.Errorf("GetPolicy(%d) must error", id)
		}
	}
}
