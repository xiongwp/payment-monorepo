package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newEngine() *moneyFlowEngine {
	logger, _ := zap.NewDevelopment()
	return &moneyFlowEngine{
		flowConfigs: make(map[string]*FlowConfig),
		logger:      logger,
	}
}

// ─── calculateFee ─────────────────────────────────────────────────────────────

func TestCalculateFee_Fixed(t *testing.T) {
	e := newEngine()
	rule := FeeRule{FeeType: "FIXED", Amount: 20000} // 2 CNY stored
	assert.Equal(t, int64(20000), e.calculateFee(rule, 100000))
	assert.Equal(t, int64(20000), e.calculateFee(rule, 0))
	assert.Equal(t, int64(20000), e.calculateFee(rule, 1))
}

func TestCalculateFee_Percent(t *testing.T) {
	e := newEngine()
	rule := FeeRule{FeeType: "PERCENT", Percent: 0.006} // 0.6%

	// 1,000,000 stored units × 0.6% = 6000
	assert.Equal(t, int64(6000), e.calculateFee(rule, 1_000_000))

	// 34200 stored units × 0.006 = 205.2 → rounds to 205
	assert.Equal(t, int64(205), e.calculateFee(rule, 34200))

	// 0 × 0.6% = 0
	assert.Equal(t, int64(0), e.calculateFee(rule, 0))
}

func TestCalculateFee_Percent_Rounding(t *testing.T) {
	e := newEngine()
	// Exactly half → round-half-up behaviour via math.Round
	rule := FeeRule{FeeType: "PERCENT", Percent: 0.5}
	// 1 × 0.5 = 0.5 → rounds to 1
	assert.Equal(t, int64(1), e.calculateFee(rule, 1))
	// 3 × 0.5 = 1.5 → rounds to 2
	assert.Equal(t, int64(2), e.calculateFee(rule, 3))
}

func TestCalculateFee_MinAmount(t *testing.T) {
	e := newEngine()
	rule := FeeRule{
		FeeType:   "PERCENT",
		Percent:   0.001, // 0.1%
		MinAmount: 5000,  // minimum 0.50 USD
	}
	// 100 × 0.1% = 0.1 → below minimum → return 5000
	assert.Equal(t, int64(5000), e.calculateFee(rule, 100))
	// 10_000_000 × 0.1% = 10000 → above minimum → return 10000
	assert.Equal(t, int64(10000), e.calculateFee(rule, 10_000_000))
}

func TestCalculateFee_MaxAmount(t *testing.T) {
	e := newEngine()
	rule := FeeRule{
		FeeType:   "PERCENT",
		Percent:   0.006,
		MaxAmount: 100000, // cap at 10 USD
	}
	// 1_000_000_000 × 0.6% = 6_000_000 → exceeds cap → return 100000
	assert.Equal(t, int64(100000), e.calculateFee(rule, 1_000_000_000))
	// 100_000 × 0.6% = 600 → below cap → return 600
	assert.Equal(t, int64(600), e.calculateFee(rule, 100_000))
}

func TestCalculateFee_MinAndMaxAmount(t *testing.T) {
	e := newEngine()
	rule := FeeRule{
		FeeType:   "PERCENT",
		Percent:   0.01, // 1%
		MinAmount: 1000,
		MaxAmount: 50000,
	}
	// 100 × 1% = 1 → below min → 1000
	assert.Equal(t, int64(1000), e.calculateFee(rule, 100))
	// 200_000 × 1% = 2000 → within range → 2000
	assert.Equal(t, int64(2000), e.calculateFee(rule, 200_000))
	// 10_000_000 × 1% = 100000 → above max → 50000
	assert.Equal(t, int64(50000), e.calculateFee(rule, 10_000_000))
}

func TestCalculateFee_UnknownType_ReturnsZero(t *testing.T) {
	e := newEngine()
	rule := FeeRule{FeeType: "UNKNOWN", Amount: 5000}
	// Unknown fee type → feeAmount stays 0; MinAmount=0 so no floor applied
	assert.Equal(t, int64(0), e.calculateFee(rule, 100_000))
}

func TestCalculateFee_ZeroPercent(t *testing.T) {
	e := newEngine()
	rule := FeeRule{FeeType: "PERCENT", Percent: 0.0}
	assert.Equal(t, int64(0), e.calculateFee(rule, 1_000_000))
}

// ─── calculateAmount ──────────────────────────────────────────────────────────

func TestCalculateAmount(t *testing.T) {
	e := newEngine()
	req := &FlowExecutionRequest{Amount: 34200}

	assert.Equal(t, int64(34200), e.calculateAmount("amount", req))
	// Any non-"amount" formula falls back to req.Amount
	assert.Equal(t, int64(34200), e.calculateAmount("amount*2", req))
	assert.Equal(t, int64(34200), e.calculateAmount("", req))
}

// ─── validateRequest ──────────────────────────────────────────────────────────

func TestValidateRequest_NoRules(t *testing.T) {
	e := newEngine()
	req := &FlowExecutionRequest{Amount: 100}
	config := &FlowConfig{}
	assert.NoError(t, e.validateRequest(req, config))
}

func TestValidateRequest_AmountGT_Pass(t *testing.T) {
	e := newEngine()
	req := &FlowExecutionRequest{Amount: 1001}
	config := &FlowConfig{
		ValidateRules: []ValidateRule{
			{Field: "amount", Operator: "GT", Value: "1000", Message: "amount too small"},
		},
	}
	assert.NoError(t, e.validateRequest(req, config))
}

func TestValidateRequest_AmountGT_Fail_Equal(t *testing.T) {
	e := newEngine()
	req := &FlowExecutionRequest{Amount: 1000}
	config := &FlowConfig{
		ValidateRules: []ValidateRule{
			{Field: "amount", Operator: "GT", Value: "1000", Message: "amount too small"},
		},
	}
	err := e.validateRequest(req, config)
	require.Error(t, err)
	assert.Equal(t, "amount too small", err.Error())
}

func TestValidateRequest_AmountGT_Fail_Below(t *testing.T) {
	e := newEngine()
	req := &FlowExecutionRequest{Amount: 500}
	config := &FlowConfig{
		ValidateRules: []ValidateRule{
			{Field: "amount", Operator: "GT", Value: "1000", Message: "minimum is 1000"},
		},
	}
	err := e.validateRequest(req, config)
	require.Error(t, err)
	assert.Equal(t, "minimum is 1000", err.Error())
}

func TestValidateRequest_InvalidRuleValue(t *testing.T) {
	e := newEngine()
	req := &FlowExecutionRequest{Amount: 1000}
	config := &FlowConfig{
		ValidateRules: []ValidateRule{
			{Field: "amount", Operator: "GT", Value: "not-a-number", Message: "bad rule"},
		},
	}
	err := e.validateRequest(req, config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid validate rule value")
}

func TestValidateRequest_NonAmountFieldIgnored(t *testing.T) {
	e := newEngine()
	req := &FlowExecutionRequest{Amount: 100}
	config := &FlowConfig{
		ValidateRules: []ValidateRule{
			{Field: "currency", Operator: "GT", Value: "0", Message: "ignored"},
		},
	}
	// Non-amount fields are not evaluated in the simplified implementation
	assert.NoError(t, e.validateRequest(req, config))
}

// ─── Default config fee rules ─────────────────────────────────────────────────

func TestDefaultConfig_WithdrawalFee(t *testing.T) {
	// "提现手续费2元" FIXED fee = 20000 (2 CNY × 10000 storage factor)
	e := newEngine()
	e.initDefaultConfigs()

	config, err := e.GetFlowConfig("PAYMENT", "WITHDRAW")
	require.NoError(t, err)
	require.Len(t, config.FeeRules, 1)

	rule := config.FeeRules[0]
	assert.Equal(t, "FIXED", rule.FeeType)
	assert.Equal(t, int64(20000), rule.Amount)
	assert.Equal(t, int64(20000), e.calculateFee(rule, 1_000_000))
}

func TestDefaultConfig_MerchantFee(t *testing.T) {
	// "平台手续费0.6%" PERCENT fee
	e := newEngine()
	e.initDefaultConfigs()

	config, err := e.GetFlowConfig("PAYMENT", "PAY_MERCHANT")
	require.NoError(t, err)
	require.Len(t, config.FeeRules, 1)

	rule := config.FeeRules[0]
	assert.Equal(t, "PERCENT", rule.FeeType)
	assert.InDelta(t, 0.006, rule.Percent, 1e-9)

	// 1,000,000 × 0.006 = 6000
	assert.Equal(t, int64(6000), e.calculateFee(rule, 1_000_000))
}
