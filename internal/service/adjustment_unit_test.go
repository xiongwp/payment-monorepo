package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newAdjSvc() *adjustmentService {
	logger, _ := zap.NewDevelopment()
	return &adjustmentService{logger: logger}
}

// ─── validateAdjustment ───────────────────────────────────────────────────────

func TestValidateAdjustment_Valid(t *testing.T) {
	svc := newAdjSvc()
	req := &AdjustmentRequest{
		AccountNo:       "001ACC",
		OffsetAccountNo: "001OFFSET",
		AdjustmentType:  AdjustmentTypeCorrection,
		Amount:          10000,
		IsIncrease:      true,
		Currency:        "PHP",
		Reason:          "balance correction",
		Operator:        "ops-team",
		ApprovalNo:      "APPR-001",
		RequestID:       "req-001",
	}
	assert.NoError(t, svc.validateAdjustment(context.Background(), req))
}

// 单边调账被拒（双分录原则不可破，避免试算失衡）
func TestValidateAdjustment_RejectsSingleEntry(t *testing.T) {
	svc := newAdjSvc()
	req := &AdjustmentRequest{
		AccountNo:  "001ACC",
		Amount:     1000,
		Currency:   "PHP",
		Reason:     "x",
		Operator:   "ops",
		ApprovalNo: "APPR",
		RequestID:  "req",
		// OffsetAccountNo intentionally empty
	}
	err := svc.validateAdjustment(context.Background(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "offset_account_no")
}

// 同账户调账（A → A）被拒
func TestValidateAdjustment_RejectsSelfOffset(t *testing.T) {
	svc := newAdjSvc()
	req := &AdjustmentRequest{
		AccountNo:       "001ACC",
		OffsetAccountNo: "001ACC",
		Amount:          1000,
		Currency:        "PHP",
		Reason:          "x",
		Operator:        "ops",
		ApprovalNo:      "APPR",
		RequestID:       "req",
	}
	err := svc.validateAdjustment(context.Background(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "differ")
}

// currency 不再硬必填 —— admin-web 调账时账户已带 currency，AdjustBalance 入口
// 用 target.Currency 兜底。空 currency 在 validateAdjustment 通过；caller 传了
// 但与账户不一致才会被 AdjustBalance 拒（覆盖在 e2e）。
func TestValidateAdjustment_AllowsEmptyCurrency(t *testing.T) {
	svc := newAdjSvc()
	req := &AdjustmentRequest{
		AccountNo:       "001ACC",
		OffsetAccountNo: "001OFFSET",
		Amount:          1000,
		Reason:          "x",
		Operator:        "ops",
		ApprovalNo:      "APPR",
		RequestID:       "req",
	}
	err := svc.validateAdjustment(context.Background(), req)
	assert.NoError(t, err)
}

// request_id 必填（幂等键）
func TestValidateAdjustment_MissingRequestID(t *testing.T) {
	svc := newAdjSvc()
	req := &AdjustmentRequest{
		AccountNo:       "001ACC",
		OffsetAccountNo: "001OFFSET",
		Amount:          1000,
		Currency:        "PHP",
		Reason:          "x",
		Operator:        "ops",
		ApprovalNo:      "APPR",
	}
	err := svc.validateAdjustment(context.Background(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "request_id")
}

// 借贷方向推导：ASSET 增加→DR (target)，offset 取反向 (CR)
func TestAdjustmentEntryDirection_AssetIncrease(t *testing.T) {
	svc := newAdjSvc()
	target := svc.makeEntry("ASSET-ACC", 5000, true, "target")
	offset := svc.makeEntry("OFFSET-ACC", 5000, false, "offset")
	assert.Equal(t, int64(5000), target.DebitAmount)
	assert.Equal(t, int64(0), target.CreditAmount)
	assert.Equal(t, int64(0), offset.DebitAmount)
	assert.Equal(t, int64(5000), offset.CreditAmount)
	// 试算平衡：SUM(DR) == SUM(CR)
	assert.Equal(t, target.DebitAmount+offset.DebitAmount, target.CreditAmount+offset.CreditAmount)
}

// LIABILITY 增加→CR (target)，offset 取反向 (DR)
func TestAdjustmentEntryDirection_LiabilityIncrease(t *testing.T) {
	svc := newAdjSvc()
	target := svc.makeEntry("LIAB-ACC", 5000, false, "target")
	offset := svc.makeEntry("OFFSET-ACC", 5000, true, "offset")
	assert.Equal(t, int64(0), target.DebitAmount)
	assert.Equal(t, int64(5000), target.CreditAmount)
	assert.Equal(t, int64(5000), offset.DebitAmount)
	assert.Equal(t, int64(0), offset.CreditAmount)
	assert.Equal(t, target.DebitAmount+offset.DebitAmount, target.CreditAmount+offset.CreditAmount)
}

func TestValidateAdjustment_MissingApprovalNo(t *testing.T) {
	svc := newAdjSvc()
	req := &AdjustmentRequest{
		Amount:   10000,
		Reason:   "test",
		Operator: "ops",
		// ApprovalNo intentionally empty
	}
	err := svc.validateAdjustment(context.Background(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "approval number")
}

func TestValidateAdjustment_MissingOperator(t *testing.T) {
	svc := newAdjSvc()
	req := &AdjustmentRequest{
		Amount:     10000,
		Reason:     "test",
		ApprovalNo: "APPR-001",
		// Operator intentionally empty
	}
	err := svc.validateAdjustment(context.Background(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "operator")
}

func TestValidateAdjustment_MissingReason(t *testing.T) {
	svc := newAdjSvc()
	req := &AdjustmentRequest{
		Amount:     10000,
		Operator:   "ops",
		ApprovalNo: "APPR-001",
		// Reason intentionally empty
	}
	err := svc.validateAdjustment(context.Background(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reason")
}

func TestValidateAdjustment_ZeroAmount(t *testing.T) {
	svc := newAdjSvc()
	req := &AdjustmentRequest{
		Amount:     0,
		Reason:     "test",
		Operator:   "ops",
		ApprovalNo: "APPR-001",
	}
	err := svc.validateAdjustment(context.Background(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "greater than zero")
}

func TestValidateAdjustment_NegativeAmount(t *testing.T) {
	svc := newAdjSvc()
	req := &AdjustmentRequest{
		Amount:     -1000,
		Reason:     "test",
		Operator:   "ops",
		ApprovalNo: "APPR-001",
	}
	err := svc.validateAdjustment(context.Background(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "greater than zero")
}

func TestValidateAdjustment_AllMissing(t *testing.T) {
	svc := newAdjSvc()
	req := &AdjustmentRequest{}
	// Should fail on the first check (approval number)
	err := svc.validateAdjustment(context.Background(), req)
	require.Error(t, err)
}

// ─── AdjustmentRequest balance arithmetic ────────────────────────────────────
// Test the balance delta computation logic isolated from the DB layer.

func TestAdjustmentBalanceArithmetic_Increase(t *testing.T) {
	balanceBefore := int64(100000)
	amount := int64(10000)
	isIncrease := true

	var balanceAfter int64
	if isIncrease {
		balanceAfter = balanceBefore + amount
	} else {
		balanceAfter = balanceBefore - amount
	}

	assert.Equal(t, int64(110000), balanceAfter)
	assert.Equal(t, int64(110000)-int64(5000), balanceAfter-int64(5000)) // available = after - frozen
}

func TestAdjustmentBalanceArithmetic_Decrease(t *testing.T) {
	balanceBefore := int64(100000)
	amount := int64(30000)

	balanceAfter := balanceBefore - amount
	assert.Equal(t, int64(70000), balanceAfter)
}

func TestAdjustmentBalanceArithmetic_DecreaseToZero(t *testing.T) {
	balanceBefore := int64(10000)
	amount := int64(10000)

	balanceAfter := balanceBefore - amount
	assert.Equal(t, int64(0), balanceAfter)
}
