package e2e

// ─── 试算平衡 End-to-End 测试 ──────────────────────────────────────────────────
//
// 测试策略
// ─────────────────────────────────────────────────────────────────────────────
//   1. 每个测试案例独立 setup（truncate + seed），确保幂等。
//   2. 先执行一批复式记账，再触发日切，日切完成后调用 RunTrialBalance。
//   3. 验证两项会计恒等式：
//        • 借贷平衡：∑total_debit == ∑total_credit（IsBalanced）
//        • 会计恒等式：资产期末余额 == 负债 + 权益 + 收入 - 费用（IsEquationValid）
//   4. 验证明细汇总数字与账户余额一致。
//
// 测试数据库：与其他 E2E 测试共用 accounting_db_{0..2}_test（3 库 × 10 表）。
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/repository"
	"github.com/accounting-system/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trialBalanceEnv 在 testEnvironment 基础上增加试算平衡依赖
type trialBalanceEnv struct {
	*testEnvironment
	snapshotRepo    repository.BalanceSnapshotRepository
	trialBalanceSvc service.TrialBalanceService
	cutDate         string // 测试用日期（当天）
}

// setupTrialBalanceEnv 创建含试算平衡服务的完整测试环境
func setupTrialBalanceEnv(t *testing.T) *trialBalanceEnv {
	t.Helper()
	base := setupTestEnvironment(t)

	snapshotRepo := repository.NewBalanceSnapshotRepository(base.dbManager, base.router)
	trialRepo := repository.NewTrialBalanceRepository(base.dbManager, base.router)
	trialSvc := service.NewTrialBalanceService(trialRepo, base.router, base.logger)

	return &trialBalanceEnv{
		testEnvironment: base,
		snapshotRepo:    snapshotRepo,
		trialBalanceSvc: trialSvc,
		cutDate:         time.Now().Format("2006-01-02"),
	}
}

// runDayCutAndWait 触发日切并等待所有分片完成（轮询最多 10 s）
func (env *trialBalanceEnv) runDayCutAndWait(t *testing.T, ctx context.Context) {
	t.Helper()
	require.NoError(t, env.dayCutService.TriggerDayCut(ctx, env.cutDate, "PHP"))

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, err := env.dayCutService.CheckDayCutStatus(ctx, env.cutDate)
		require.NoError(t, err)
		if status != nil {
			// CheckDayCutStatus 返回 map[string]interface{}，key 为状态名，value 为 int 计数。
			// 当 pending 和 processing 计数均为 0 时视为全部完成。
			pendingCount, _ := status["pending"].(int)
			processingCount, _ := status["processing"].(int)
			if pendingCount == 0 && processingCount == 0 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Log("warning: day cut may not have fully completed within timeout")
}

// ─── TC13: 纯充值场景 — 最简单的借贷平衡验证 ─────────────────────────────────

func TestE2E_TC13_TrialBalance_SimpleDeposit(t *testing.T) {
	ctx := context.Background()
	env := setupTrialBalanceEnv(t)
	defer env.cleanup()

	// 账户：1 个用户（资产）+ 1 个平台权益账户（权益）
	user := env.createAccount(t, ctx, 13001, model.AccountTypeUser, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 13002, model.AccountTypePlatform, model.AccountCategoryEquity)

	depositAmt := int64(1000)

	// 充值：用户资产账户借 1000，平台权益账户贷 1000
	env.doubleEntry(t, ctx, uniqueBizNo("DEP13"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: depositAmt, Description: "充值借"},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: depositAmt, Description: "平台权益贷"},
	)

	// 平台使用缓冲，flush 后再日切
	env.flushBuffer(ctx)

	// 触发日切，生成快照
	env.runDayCutAndWait(t, ctx)

	// 执行试算平衡
	result, err := env.trialBalanceSvc.RunTrialBalance(ctx, env.cutDate, 0)
	require.NoError(t, err, "RunTrialBalance should succeed")
	require.NotNil(t, result)

	t.Logf("TC13 trial balance result: date=%s balanced=%v equationValid=%v",
		result.SnapshotDate, result.IsBalanced, result.IsEquationValid)
	t.Logf("  totalDebit=%d totalCredit=%d imbalance=%d",
		result.TotalDebit, result.TotalCredit, result.Imbalance)
	t.Logf("  asset=%d equity=%d equationDiff=%d",
		result.AssetEndingBalance, result.EquityEndingBalance, result.EquationDiff)

	// 借贷平衡
	assert.True(t, result.IsBalanced,
		"TC13: debit(%d) must equal credit(%d), imbalance=%d",
		result.TotalDebit, result.TotalCredit, result.Imbalance)

	// 会计恒等式：资产（1000）= 权益（1000）
	assert.True(t, result.IsEquationValid,
		"TC13: accounting equation must hold, diff=%d", result.EquationDiff)

	// 期间借贷合计各为 1000
	assert.Equal(t, int64(1000), result.TotalDebit)
	assert.Equal(t, int64(1000), result.TotalCredit)

	// 期末余额：资产账户 1000，权益账户 1000
	assert.Equal(t, depositAmt, result.AssetEndingBalance,
		"TC13: asset ending balance should equal deposit amount")
	assert.Equal(t, depositAmt, result.EquityEndingBalance,
		"TC13: equity ending balance should equal deposit amount")

	t.Log("TC13 PASS")
}

// ─── TC14: 充值 + 支付 + 手续费 — 含费用科目的会计恒等式 ─────────────────────

func TestE2E_TC14_TrialBalance_PayWithFee(t *testing.T) {
	ctx := context.Background()
	env := setupTrialBalanceEnv(t)
	defer env.cleanup()

	user := env.createAccount(t, ctx, 14001, model.AccountTypeUser, model.AccountCategoryAsset)
	merchant := env.createAccount(t, ctx, 14002, model.AccountTypeMerchant, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 14003, model.AccountTypePlatform, model.AccountCategoryEquity)
	feeAcct := env.createAccount(t, ctx, 14004, model.AccountTypePlatform, model.AccountCategoryExpense)

	// 充值 2000
	env.doubleEntry(t, ctx, uniqueBizNo("DEP14"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 2000},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 2000},
	)

	// 支付 1500，手续费 15（1%）
	env.doubleEntry(t, ctx, uniqueBizNo("PAY14"), model.BusinessTypePayment,
		service.AccountingEntry{AccountNo: user.AccountNo, CreditAmount: 1500},
		service.AccountingEntry{AccountNo: merchant.AccountNo, DebitAmount: 1500},
	)
	env.doubleEntry(t, ctx, uniqueBizNo("FEE14"), model.BusinessTypeCommission,
		service.AccountingEntry{AccountNo: merchant.AccountNo, CreditAmount: 15},
		service.AccountingEntry{AccountNo: feeAcct.AccountNo, DebitAmount: 15},
	)

	env.flushBuffer(ctx)
	env.runDayCutAndWait(t, ctx)

	result, err := env.trialBalanceSvc.RunTrialBalance(ctx, env.cutDate, 0)
	require.NoError(t, err)
	require.NotNil(t, result)

	t.Logf("TC14 result: balanced=%v equationValid=%v", result.IsBalanced, result.IsEquationValid)
	t.Logf("  debit=%d credit=%d imbalance=%d equationDiff=%d",
		result.TotalDebit, result.TotalCredit, result.Imbalance, result.EquationDiff)
	for _, s := range result.Summaries {
		t.Logf("  [%s type=%d] count=%d begin=%d end=%d debit=%d credit=%d",
			s.Category, s.Type, s.AccountCount,
			s.SumBeginning, s.SumEnding, s.SumDebit, s.SumCredit)
	}

	assert.True(t, result.IsBalanced,
		"TC14: borrowing must equal lending, imbalance=%d", result.Imbalance)
	assert.True(t, result.IsEquationValid,
		"TC14: accounting equation must hold, diff=%d", result.EquationDiff)

	// 期间总借方 = 2000(充值借) + 1500(商户借) + 15(手续费借) = 3515
	// 期间总贷方 = 2000(平台贷) + 1500(用户贷) + 15(商户贷)   = 3515
	assert.Equal(t, int64(3515), result.TotalDebit)
	assert.Equal(t, int64(3515), result.TotalCredit)

	// 期末余额验证
	// 资产：用户(500) + 商户(1485) = 1985
	assert.Equal(t, int64(1985), result.AssetEndingBalance,
		"TC14: asset ending balance")
	// 费用：手续费账户 15
	assert.Equal(t, int64(15), result.ExpenseEndingBalance,
		"TC14: expense ending balance")

	t.Log("TC14 PASS")
}

// ─── TC15: 多用户多交易 — 跨分片聚合验证 ────────────────────────────────────

func TestE2E_TC15_TrialBalance_MultiUser_CrossShard(t *testing.T) {
	ctx := context.Background()
	env := setupTrialBalanceEnv(t)
	defer env.cleanup()

	platform := env.createAccount(t, ctx, 15000, model.AccountTypePlatform, model.AccountCategoryEquity)

	const userCount = 6
	users := make([]*model.Account, userCount)
	for i := 0; i < userCount; i++ {
		users[i] = env.createAccount(t, ctx, int64(15100+i), model.AccountTypeUser, model.AccountCategoryAsset)
	}
	merchants := make([]*model.Account, 3)
	for i := 0; i < 3; i++ {
		merchants[i] = env.createAccount(t, ctx, int64(15200+i), model.AccountTypeMerchant, model.AccountCategoryAsset)
	}

	var expectedTotalDebit int64

	// 每个用户充值不同金额
	for i, u := range users {
		amt := int64((i + 1) * 300)
		env.doubleEntry(t, ctx, uniqueBizNo(fmt.Sprintf("DEP15U%d", i)), model.BusinessTypeDeposit,
			service.AccountingEntry{AccountNo: u.AccountNo, DebitAmount: amt},
			service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: amt},
		)
		expectedTotalDebit += amt
	}
	// 总充值 = (1+2+3+4+5+6)*300 = 21*300 = 6300

	// 3 笔支付：用户→商户
	payments := []struct {
		userIdx, merchantIdx int
		amt                  int64
	}{
		{0, 0, 200},
		{1, 1, 400},
		{2, 2, 600},
	}
	for _, p := range payments {
		env.doubleEntry(t, ctx, uniqueBizNo(fmt.Sprintf("PAY15_%d", p.userIdx)), model.BusinessTypePayment,
			service.AccountingEntry{AccountNo: users[p.userIdx].AccountNo, CreditAmount: p.amt},
			service.AccountingEntry{AccountNo: merchants[p.merchantIdx].AccountNo, DebitAmount: p.amt},
		)
		expectedTotalDebit += p.amt // 支付也产生借贷各一笔
	}

	// 1 笔退款：商户[0] → 用户[0]
	refundAmt := int64(100)
	env.doubleEntry(t, ctx, uniqueBizNo("REF15"), model.BusinessTypeRefund,
		service.AccountingEntry{AccountNo: merchants[0].AccountNo, CreditAmount: refundAmt},
		service.AccountingEntry{AccountNo: users[0].AccountNo, DebitAmount: refundAmt},
	)
	expectedTotalDebit += refundAmt

	env.flushBuffer(ctx)
	env.runDayCutAndWait(t, ctx)

	result, err := env.trialBalanceSvc.RunTrialBalance(ctx, env.cutDate, 0)
	require.NoError(t, err)
	require.NotNil(t, result)

	t.Logf("TC15 result: balanced=%v equationValid=%v shards=%d summaries=%d",
		result.IsBalanced, result.IsEquationValid,
		env.router.TotalTableCount(), len(result.Summaries))
	t.Logf("  debit=%d credit=%d", result.TotalDebit, result.TotalCredit)
	t.Logf("  asset=%d equity=%d liab=%d rev=%d exp=%d",
		result.AssetEndingBalance, result.EquityEndingBalance,
		result.LiabilityEndingBalance, result.RevenueEndingBalance, result.ExpenseEndingBalance)

	assert.True(t, result.IsBalanced,
		"TC15: global debit must equal credit, imbalance=%d", result.Imbalance)
	assert.True(t, result.IsEquationValid,
		"TC15: accounting equation must hold, diff=%d", result.EquationDiff)

	// 期间总借方应等于预期
	assert.Equal(t, expectedTotalDebit, result.TotalDebit,
		"TC15: total debit should match expected")
	assert.Equal(t, expectedTotalDebit, result.TotalCredit,
		"TC15: total credit should equal total debit")

	// 应该有 ASSET 和 EQUITY 两个类别汇总
	require.GreaterOrEqual(t, len(result.Summaries), 2, "TC15: expect at least 2 category summaries")

	t.Log("TC15 PASS")
}

// ─── TC16: 日切前无记账 — 空结果不报错 ──────────────────────────────────────

func TestE2E_TC16_TrialBalance_NoTransactions(t *testing.T) {
	ctx := context.Background()
	env := setupTrialBalanceEnv(t)
	defer env.cleanup()

	// 不执行任何记账，直接触发日切
	env.runDayCutAndWait(t, ctx)

	result, err := env.trialBalanceSvc.RunTrialBalance(ctx, env.cutDate, 0)
	require.NoError(t, err, "empty day should not error")
	require.NotNil(t, result)

	t.Logf("TC16 result: balanced=%v equationValid=%v summaries=%d",
		result.IsBalanced, result.IsEquationValid, len(result.Summaries))

	// 没有快照记录时，汇总应为空，两项检验默认成立（0 == 0）
	assert.True(t, result.IsBalanced, "TC16: zero debit == zero credit")
	assert.True(t, result.IsEquationValid, "TC16: zero equation should hold")
	assert.Equal(t, int64(0), result.TotalDebit)
	assert.Equal(t, int64(0), result.TotalCredit)

	t.Log("TC16 PASS")
}

// ─── TC17: 汇总明细校验 — 每个 CategorySummary 数字与实际快照一致 ────────────

func TestE2E_TC17_TrialBalance_SummaryDetails(t *testing.T) {
	ctx := context.Background()
	env := setupTrialBalanceEnv(t)
	defer env.cleanup()

	// 三种科目：资产、权益、收入
	user := env.createAccount(t, ctx, 17001, model.AccountTypeUser, model.AccountCategoryAsset)
	equity := env.createAccount(t, ctx, 17002, model.AccountTypePlatform, model.AccountCategoryEquity)
	revenue := env.createAccount(t, ctx, 17003, model.AccountTypePlatform, model.AccountCategoryRevenue)

	// 充值 500：用户资产借，平台权益贷
	env.doubleEntry(t, ctx, uniqueBizNo("DEP17a"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 500},
		service.AccountingEntry{AccountNo: equity.AccountNo, CreditAmount: 500},
	)
	// 平台手续费收入 50：从 equity 转到 revenue
	env.doubleEntry(t, ctx, uniqueBizNo("REV17"), model.BusinessTypeCommission,
		service.AccountingEntry{AccountNo: equity.AccountNo, DebitAmount: 50},
		service.AccountingEntry{AccountNo: revenue.AccountNo, CreditAmount: 50},
	)

	env.flushBuffer(ctx)
	env.runDayCutAndWait(t, ctx)

	result, err := env.trialBalanceSvc.RunTrialBalance(ctx, env.cutDate, 0)
	require.NoError(t, err)
	require.NotNil(t, result)

	t.Logf("TC17 summaries (%d):", len(result.Summaries))
	for _, s := range result.Summaries {
		t.Logf("  [%-12s type=%d] count=%d begin=%d end=%d debit=%d credit=%d",
			s.Category, s.Type, s.AccountCount,
			s.SumBeginning, s.SumEnding, s.SumDebit, s.SumCredit)
	}

	assert.True(t, result.IsBalanced, "TC17: balanced, imbalance=%d", result.Imbalance)
	assert.True(t, result.IsEquationValid, "TC17: equation valid, diff=%d", result.EquationDiff)

	// 找到 ASSET 汇总
	assetSummary := findSummaryByCategory(result, "ASSET")
	require.NotNil(t, assetSummary, "TC17: ASSET summary must exist")
	assert.Equal(t, int64(500), assetSummary.SumDebit, "TC17: asset period debit = 500 (deposit)")
	assert.Equal(t, int64(0), assetSummary.SumCredit, "TC17: asset period credit = 0")
	assert.Equal(t, int64(500), assetSummary.SumEnding, "TC17: asset ending = 500")

	// 找到 REVENUE 汇总
	revSummary := findSummaryByCategory(result, "REVENUE")
	require.NotNil(t, revSummary, "TC17: REVENUE summary must exist")
	assert.Equal(t, int64(50), revSummary.SumCredit, "TC17: revenue credit = 50")
	assert.Equal(t, int64(50), revSummary.SumEnding, "TC17: revenue ending = 50")

	// 会计恒等式：资产(500) = 权益(450) + 收入(50) = 500
	assert.Equal(t, result.AssetEndingBalance, result.EquityEndingBalance+result.RevenueEndingBalance,
		"TC17: assets == equity + revenue")

	t.Log("TC17 PASS")
}

// ─── TC18: 错误日期 — 未来日期应返回空结果而不报错 ──────────────────────────

func TestE2E_TC18_TrialBalance_FutureDate_Empty(t *testing.T) {
	ctx := context.Background()
	env := setupTrialBalanceEnv(t)
	defer env.cleanup()

	user := env.createAccount(t, ctx, 18001, model.AccountTypeUser, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 18002, model.AccountTypePlatform, model.AccountCategoryEquity)
	env.doubleEntry(t, ctx, uniqueBizNo("DEP18"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 100},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 100},
	)
	env.flushBuffer(ctx)
	env.runDayCutAndWait(t, ctx)

	futureDate := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	result, err := env.trialBalanceSvc.RunTrialBalance(ctx, futureDate, 0)
	// 未来日期快照不存在，应正常返回空结果（非报错）
	require.NoError(t, err, "future date: service should not error, just return empty")
	require.NotNil(t, result)

	assert.Empty(t, result.Summaries, "TC18: no summaries for future date")
	assert.True(t, result.IsBalanced, "TC18: empty is balanced")
	assert.True(t, result.IsEquationValid, "TC18: empty equation valid")
	t.Log("TC18 PASS")
}

// ─── TC19: 复杂场景 — 充值+支付+退款+转账，完整会计恒等式 ───────────────────

func TestE2E_TC19_TrialBalance_FullScenario(t *testing.T) {
	ctx := context.Background()
	env := setupTrialBalanceEnv(t)
	defer env.cleanup()

	// 账户矩阵
	//   资产:  user1, user2, merchant1
	//   权益:  platform (充值来源)
	//   费用:  feeAcct  (手续费支出)
	user1 := env.createAccount(t, ctx, 19001, model.AccountTypeUser, model.AccountCategoryAsset)
	user2 := env.createAccount(t, ctx, 19002, model.AccountTypeUser, model.AccountCategoryAsset)
	merchant1 := env.createAccount(t, ctx, 19003, model.AccountTypeMerchant, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 19004, model.AccountTypePlatform, model.AccountCategoryEquity)
	feeAcct := env.createAccount(t, ctx, 19005, model.AccountTypePlatform, model.AccountCategoryExpense)

	// Step1: user1 充值 3000
	env.doubleEntry(t, ctx, uniqueBizNo("D19_1"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user1.AccountNo, DebitAmount: 3000},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 3000},
	)
	// Step2: user2 充值 1000
	env.doubleEntry(t, ctx, uniqueBizNo("D19_2"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user2.AccountNo, DebitAmount: 1000},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 1000},
	)
	// Step3: user1 支付 merchant1 2000
	env.doubleEntry(t, ctx, uniqueBizNo("P19_1"), model.BusinessTypePayment,
		service.AccountingEntry{AccountNo: user1.AccountNo, CreditAmount: 2000},
		service.AccountingEntry{AccountNo: merchant1.AccountNo, DebitAmount: 2000},
	)
	// Step4: 手续费 20（merchant1 → feeAcct）
	env.doubleEntry(t, ctx, uniqueBizNo("F19_1"), model.BusinessTypeCommission,
		service.AccountingEntry{AccountNo: merchant1.AccountNo, CreditAmount: 20},
		service.AccountingEntry{AccountNo: feeAcct.AccountNo, DebitAmount: 20},
	)
	// Step5: user1 → user2 转账 500
	env.doubleEntry(t, ctx, uniqueBizNo("T19_1"), model.BusinessTypeTransfer,
		service.AccountingEntry{AccountNo: user1.AccountNo, CreditAmount: 500},
		service.AccountingEntry{AccountNo: user2.AccountNo, DebitAmount: 500},
	)
	// Step6: merchant1 部分退款给 user2 300
	env.doubleEntry(t, ctx, uniqueBizNo("R19_1"), model.BusinessTypeRefund,
		service.AccountingEntry{AccountNo: merchant1.AccountNo, CreditAmount: 300},
		service.AccountingEntry{AccountNo: user2.AccountNo, DebitAmount: 300},
	)

	// 期末余额预期：
	//   user1   = 3000 - 2000 - 500 = 500
	//   user2   = 1000 + 500 + 300  = 1800
	//   merchant1 = 2000 - 20 - 300 = 1680
	//   platform  = 3000 + 1000     = 4000  (权益)
	//   feeAcct   = 20              (费用)

	assert.Equal(t, int64(500), env.balance(t, ctx, user1.AccountNo), "TC19: user1")
	assert.Equal(t, int64(1800), env.balance(t, ctx, user2.AccountNo), "TC19: user2")
	assert.Equal(t, int64(1680), env.balance(t, ctx, merchant1.AccountNo), "TC19: merchant1")

	env.flushBuffer(ctx)
	env.runDayCutAndWait(t, ctx)

	result, err := env.trialBalanceSvc.RunTrialBalance(ctx, env.cutDate, 0)
	require.NoError(t, err)
	require.NotNil(t, result)

	t.Logf("TC19 result: balanced=%v equationValid=%v", result.IsBalanced, result.IsEquationValid)
	t.Logf("  debit=%d credit=%d imbalance=%d equationDiff=%d",
		result.TotalDebit, result.TotalCredit, result.Imbalance, result.EquationDiff)
	t.Logf("  asset=%d equity=%d expense=%d",
		result.AssetEndingBalance, result.EquityEndingBalance, result.ExpenseEndingBalance)
	for _, s := range result.Summaries {
		t.Logf("  [%-12s] end=%d debit=%d credit=%d",
			s.Category, s.SumEnding, s.SumDebit, s.SumCredit)
	}

	// 主断言
	assert.True(t, result.IsBalanced,
		"TC19: debit must equal credit, imbalance=%d", result.Imbalance)
	assert.True(t, result.IsEquationValid,
		"TC19: accounting equation must hold, diff=%d", result.EquationDiff)

	// 期末余额汇总
	// 资产总计 = user1(500) + user2(1800) + merchant1(1680) = 3980
	assert.Equal(t, int64(3980), result.AssetEndingBalance, "TC19: asset ending")
	// 权益 = platform(4000)
	assert.Equal(t, int64(4000), result.EquityEndingBalance, "TC19: equity ending")
	// 费用 = feeAcct(20)
	assert.Equal(t, int64(20), result.ExpenseEndingBalance, "TC19: expense ending")

	// 会计恒等式手工验证：资产(3980) = 权益(4000) - 费用(20) = 3980 ✓
	rightSide := result.EquityEndingBalance - result.ExpenseEndingBalance
	assert.Equal(t, result.AssetEndingBalance, rightSide,
		"TC19: asset(%d) == equity(%d) - expense(%d) = %d",
		result.AssetEndingBalance, result.EquityEndingBalance, result.ExpenseEndingBalance, rightSide)

	t.Log("TC19 PASS")
}

// ─── TC20: 跨日切连续两天 — 第二天试算只看当日增量 ──────────────────────────

func TestE2E_TC20_TrialBalance_TwoDayCut(t *testing.T) {
	ctx := context.Background()
	env := setupTrialBalanceEnv(t)
	defer env.cleanup()

	user := env.createAccount(t, ctx, 20001, model.AccountTypeUser, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 20002, model.AccountTypePlatform, model.AccountCategoryEquity)

	today := time.Now().Format("2006-01-02")
	// 模拟昨天：手动插入 yesterday 的快照（beginning_balance=0, ending_balance=800）
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	dbIdx, tblIdx := env.router.RouteByAccountNo(user.AccountNo)
	err := env.snapshotRepo.Upsert(ctx, dbIdx, tblIdx, &model.AccountBalanceSnapshot{
		AccountNo:        user.AccountNo,
		SnapshotDate:     yesterday,
		BeginningBalance: 0,
		EndingBalance:    800,
		TotalDebit:       800,
		TotalCredit:      0,
		TransactionCount: 1,
	})
	require.NoError(t, err, "setup yesterday snapshot")

	// 今天再充值 200
	env.doubleEntry(t, ctx, uniqueBizNo("DEP20"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 200},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 200},
	)

	env.flushBuffer(ctx)

	// 触发今天的日切（only today shards）
	require.NoError(t, env.dayCutService.TriggerDayCut(ctx, today, "PHP"))
	time.Sleep(3 * time.Second)

	// 查今天的试算平衡
	result, err := env.trialBalanceSvc.RunTrialBalance(ctx, today, 0)
	require.NoError(t, err)
	require.NotNil(t, result)

	t.Logf("TC20 today(%s): balanced=%v equationValid=%v debit=%d credit=%d",
		today, result.IsBalanced, result.IsEquationValid,
		result.TotalDebit, result.TotalCredit)

	// 今天的借贷各 200
	assert.True(t, result.IsBalanced, "TC20: today balanced")
	assert.Equal(t, int64(200), result.TotalDebit, "TC20: today debit = 200")
	assert.Equal(t, int64(200), result.TotalCredit, "TC20: today credit = 200")

	t.Log("TC20 PASS")
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

// findSummaryByCategory 按 category 字符串查找汇总行
func findSummaryByCategory(result *service.TrialBalanceResult, category string) *service.CategorySummary {
	for _, s := range result.Summaries {
		if string(s.Category) == category {
			return s
		}
	}
	return nil
}
