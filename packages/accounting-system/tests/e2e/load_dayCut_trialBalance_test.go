package e2e

// ─── 高并发 端到端 压测：充值 / 转账 / 支付 / 提现 + 日切 + 试算平衡 ────────
//
// 目的：复现并防止"用户账户期末余额与借贷不一致"问题（并发充值 / 转账时
// transaction_time 在 Confirm 间乱序导致快照取错 balance_after 的回归）。
//
// 校验点：
//   1. 借贷平衡 (TotalDebit == TotalCredit)
//   2. 会计恒等式 (净变动版本)
//   3. 每个用户账户：snapshot.ending_balance == 当前 account.balance（同步账户）
//   4. 每个用户账户：snapshot.beginning + (credit - debit) == snapshot.ending
//      （用户账户为负债类，贷增借减）
//   5. 每个中转/平台账户：满足类别符号关系（资产借增贷减、负债贷增借减）
//
// 通过高并发对同一用户账户重复充值/转账，构造多笔 confirm 顺序与
// transaction_time 顺序不一致的场景；修复前会出现 ending_balance < 实际，
// 修复后所有校验必通过。
//
// 运行（需要本地 MySQL 已 init 测试库 accounting_db_0_test ~ _2_test）：
//   go test -v -mod=vendor -timeout 5m \
//     -run TestE2E_LoadDayCutTrialBalance ./tests/e2e/...
//
// 调节规模：环境变量
//   E2E_USERS    用户账户数            (默认 200)
//   E2E_TXNS     压测总笔数             (默认 5000)
//   E2E_WORKERS  并发 worker 数         (默认 64)

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envIntDefault(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return def
}

// TestE2E_LoadDayCutTrialBalance 高并发端到端压测：
// 大量并发充值/转账/支付/提现 → 触发日切 → 校验试算平衡恒等式 + 每账户对账。
func TestE2E_LoadDayCutTrialBalance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in -short mode")
	}

	users := envIntDefault("E2E_USERS", 200)
	merchants := envIntDefault("E2E_MERCHANTS", 30)
	totalTxns := envIntDefault("E2E_TXNS", 5000)
	workers := envIntDefault("E2E_WORKERS", 64)
	initialBalance := int64(1_000_000) // 每用户初始余额 (ISO最小单位×100)

	t.Logf("scale: users=%d merchants=%d txns=%d workers=%d initBalance=%d",
		users, merchants, totalTxns, workers, initialBalance)

	ctx := context.Background()
	env := setupTrialBalanceEnv(t)
	defer env.cleanup()

	// ── Phase 1: 创建账户 ────────────────────────────────────────────────────
	// 用户账户为负债类（按业务约定：用户余额是平台对用户的负债）
	// 商户账户同为负债类
	// 中转账户为资产类（充值入账方）
	// 平台 Equity 账户为权益类（充值的另一方）
	// 平台手续费账户为收入类
	t.Logf("creating accounts...")
	// 用户 ID 从 1e9 起（[1,10000] 已预留给系统账户），避免误入保留段
	userAccs := make([]*model.Account, users)
	for i := 0; i < users; i++ {
		userAccs[i] = env.createAccount(t, ctx, int64(100_000_000+i),
			model.AccountTypeUser, model.AccountCategoryLiability)
	}
	// 商户 ID 从 9e8 起
	merchantAccs := make([]*model.Account, merchants)
	for i := 0; i < merchants; i++ {
		merchantAccs[i] = env.createAccount(t, ctx, int64(900_000_000+i),
			model.AccountTypeMerchant, model.AccountCategoryLiability)
	}
	// 系统账户走保留段（CreateAccount 现在对 platform 类型拒绝 ≥1e9 的 owner_id，
	// 这里用预留区间 1/2/3 创建单个 e2e-scope 的 transit / equity / fee 账户。
	// 生产中应通过 CreatePlatformAccountFleet 一次性创建 100 个分片账户）。
	transitAcc := env.createPlatformAccount(t, ctx, 1,
		model.AccountTypeTransitChannelReceivable)
	platformEquity := env.createPlatformAccount(t, ctx, 2,
		model.AccountTypePlatform)
	feeAcc := env.createPlatformAccount(t, ctx, 3,
		model.AccountTypeChargeFee)

	// ── Phase 2: 用户初始充值 ────────────────────────────────────────────────
	// 用户账户期初为 0；充值后期末 = initialBalance
	t.Logf("seeding initial balances...")
	for _, ua := range userAccs {
		bizNo := uniqueBizNo("INIT")
		env.doubleEntry(t, ctx, bizNo, model.BusinessTypeDeposit,
			service.AccountingEntry{AccountNo: transitAcc.AccountNo, DebitAmount: initialBalance, Description: "初始充值借"},
			service.AccountingEntry{AccountNo: ua.AccountNo, CreditAmount: initialBalance, Description: "用户余额贷"},
		)
	}

	// 验证初始余额
	for _, ua := range userAccs {
		require.Equal(t, initialBalance, env.balance(t, ctx, ua.AccountNo),
			"user %s initial balance", ua.AccountNo)
	}

	// ── Phase 3: 高并发混合交易 ──────────────────────────────────────────────
	t.Logf("running %d concurrent transactions across %d workers...", totalTxns, workers)

	var (
		successCnt atomic.Int64
		failCnt    atomic.Int64
		depositCnt atomic.Int64
		transferCnt atomic.Int64
		paymentCnt atomic.Int64
		withdrawCnt atomic.Int64
	)

	remaining := atomic.Int64{}
	remaining.Store(int64(totalTxns))

	startTPS := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for remaining.Add(-1) >= 0 {
				op := rng.Float64()
				switch {
				case op < 0.30:
					// 充值
					ua := userAccs[rng.Intn(users)]
					amt := int64(100 + rng.Intn(5000))
					_, _, err := env.accountingService.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
						BusinessNo: uniqueBizNo("DEP"), BusinessType: model.BusinessTypeDeposit,
						Currency: "PHP", Description: "并发充值",
						Entries: []service.AccountingEntry{
							{AccountNo: transitAcc.AccountNo, DebitAmount: amt},
							{AccountNo: ua.AccountNo, CreditAmount: amt},
						},
					})
					if err != nil {
						failCnt.Add(1)
					} else {
						successCnt.Add(1)
						depositCnt.Add(1)
					}
				case op < 0.60:
					// 转账（用户A → 用户B）
					a := rng.Intn(users)
					b := rng.Intn(users)
					if a == b {
						b = (a + 1) % users
					}
					amt := int64(50 + rng.Intn(2000))
					_, _, err := env.accountingService.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
						BusinessNo: uniqueBizNo("TFR"), BusinessType: model.BusinessTypeTransfer,
						Currency: "PHP", Description: "用户转账",
						Entries: []service.AccountingEntry{
							{AccountNo: userAccs[a].AccountNo, DebitAmount: amt, Description: "转出"},
							{AccountNo: userAccs[b].AccountNo, CreditAmount: amt, Description: "转入"},
						},
					})
					if err != nil {
						failCnt.Add(1)
					} else {
						successCnt.Add(1)
						transferCnt.Add(1)
					}
				case op < 0.85:
					// 支付（用户 → 商户 + 1% 平台手续费）
					ua := userAccs[rng.Intn(users)]
					ma := merchantAccs[rng.Intn(merchants)]
					total := int64(200 + rng.Intn(8000))
					fee := total / 100
					if fee < 1 {
						fee = 1
					}
					merchantAmt := total - fee
					_, _, err := env.accountingService.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
						BusinessNo: uniqueBizNo("PAY"), BusinessType: model.BusinessTypePayment,
						Currency: "PHP", Description: "用户支付",
						Entries: []service.AccountingEntry{
							{AccountNo: ua.AccountNo, DebitAmount: total, Description: "支付扣款"},
							{AccountNo: ma.AccountNo, CreditAmount: merchantAmt, Description: "商户收款"},
							{AccountNo: feeAcc.AccountNo, CreditAmount: fee, Description: "手续费"},
						},
					})
					if err != nil {
						failCnt.Add(1)
					} else {
						successCnt.Add(1)
						paymentCnt.Add(1)
					}
				default:
					// 提现（用户提现到平台 equity）
					ua := userAccs[rng.Intn(users)]
					amt := int64(50 + rng.Intn(1000))
					_, _, err := env.accountingService.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
						BusinessNo: uniqueBizNo("WD"), BusinessType: model.BusinessTypeWithdraw,
						Currency: "PHP", Description: "用户提现",
						Entries: []service.AccountingEntry{
							{AccountNo: ua.AccountNo, DebitAmount: amt, Description: "用户余额扣减"},
							{AccountNo: platformEquity.AccountNo, CreditAmount: amt, Description: "平台支出"},
						},
					})
					if err != nil {
						failCnt.Add(1)
					} else {
						successCnt.Add(1)
						withdrawCnt.Add(1)
					}
				}
			}
		}(int64(w+1) * time.Now().UnixNano())
	}
	wg.Wait()

	tps := float64(successCnt.Load()) / time.Since(startTPS).Seconds()
	t.Logf("load done: success=%d fail=%d  (deposit=%d transfer=%d payment=%d withdraw=%d) tps=%.0f duration=%v",
		successCnt.Load(), failCnt.Load(),
		depositCnt.Load(), transferCnt.Load(), paymentCnt.Load(), withdrawCnt.Load(),
		tps, time.Since(startTPS).Round(time.Millisecond))

	// 平台/中转账户使用缓冲记账，flush 后再日切
	env.flushBuffer(ctx)

	// ── Phase 4: 日切 ───────────────────────────────────────────────────────
	t.Logf("triggering day cut for %s ...", env.cutDate)
	cutStart := time.Now()
	env.runDayCutAndWait(t, ctx)
	t.Logf("day cut completed in %v", time.Since(cutStart).Round(time.Millisecond))

	// ── Phase 5: 试算平衡 ──────────────────────────────────────────────────
	t.Logf("running trial balance ...")
	// runID=0 跳过版本过滤（只跑过一次日切；生产场景传具体 run_id）。
	result, err := env.trialBalanceSvc.RunTrialBalance(ctx, env.cutDate, 0)
	require.NoError(t, err)
	require.NotNil(t, result)

	t.Logf("trial balance: totalDebit=%d totalCredit=%d imbalance=%d isBalanced=%v",
		result.TotalDebit, result.TotalCredit, result.Imbalance, result.IsBalanced)
	t.Logf("  asset=%d liability=%d equity=%d revenue=%d expense=%d",
		result.AssetEndingBalance, result.LiabilityEndingBalance,
		result.EquityEndingBalance, result.RevenueEndingBalance, result.ExpenseEndingBalance)
	for _, sm := range result.Summaries {
		t.Logf("  [%s/%d] count=%d begin=%d end=%d debit=%d credit=%d",
			sm.Category, sm.Type, sm.AccountCount,
			sm.SumBeginning, sm.SumEnding, sm.SumDebit, sm.SumCredit)
	}

	// 校验 1：借贷平衡
	assert.True(t, result.IsBalanced,
		"trial balance imbalance: debit=%d credit=%d diff=%d",
		result.TotalDebit, result.TotalCredit, result.Imbalance)
	assert.Equal(t, int64(0), result.Imbalance)

	// 校验 2：会计恒等式（净变动版本）
	assert.True(t, result.IsEquationValid,
		"accounting equation invalid: diff=%d", result.EquationDiff)

	// ── Phase 6: 每账户对账 ──────────────────────────────────────────────────
	// 同步账户：snapshot.ending == account.balance；snapshot.begin + signedDelta == ending
	t.Logf("per-account reconciliation (%d users)...", users)
	mismatches := 0
	for i, ua := range userAccs {
		acc, err := env.accountingService.GetAccount(ctx, ua.AccountNo)
		require.NoError(t, err)
		snap, err := env.getBalanceSnapshot(ctx, ua.AccountNo, env.cutDate)
		if err != nil {
			// 该用户本日无活动 → 无快照属正常（初始充值未跨日时算今日活动）
			t.Logf("user %d %s: no snapshot (err=%v) balance=%d", i, ua.AccountNo, err, acc.Balance)
			continue
		}

		// 同步账户 ending == account.balance
		if snap.EndingBalance != acc.Balance {
			mismatches++
			t.Errorf("user %s: snapshot.ending=%d != account.balance=%d (begin=%d debit=%d credit=%d)",
				ua.AccountNo, snap.EndingBalance, acc.Balance,
				snap.BeginningBalance, snap.TotalDebit, snap.TotalCredit)
		}

		// 用户账户为负债类：begin + credit - debit == ending
		expectedEnd := snap.BeginningBalance + snap.TotalCredit - snap.TotalDebit
		if expectedEnd != snap.EndingBalance {
			mismatches++
			t.Errorf("user %s: begin(%d) + credit(%d) - debit(%d) = %d != ending(%d)",
				ua.AccountNo, snap.BeginningBalance, snap.TotalCredit, snap.TotalDebit,
				expectedEnd, snap.EndingBalance)
		}
	}
	assert.Equal(t, 0, mismatches, "per-account reconciliation mismatches")

	// 商户账户对账
	for _, ma := range merchantAccs {
		acc, err := env.accountingService.GetAccount(ctx, ma.AccountNo)
		require.NoError(t, err)
		snap, err := env.getBalanceSnapshot(ctx, ma.AccountNo, env.cutDate)
		if err != nil {
			continue
		}
		if snap.EndingBalance != acc.Balance {
			t.Errorf("merchant %s: snapshot.ending=%d != account.balance=%d",
				ma.AccountNo, snap.EndingBalance, acc.Balance)
		}
		expectedEnd := snap.BeginningBalance + snap.TotalCredit - snap.TotalDebit
		if expectedEnd != snap.EndingBalance {
			t.Errorf("merchant %s: begin+credit-debit=%d != ending=%d",
				ma.AccountNo, expectedEnd, snap.EndingBalance)
		}
	}

	// 平台 / 中转账户对账（缓冲记账经 fixBufferedSnapshots 修正后应等于 acc.balance）
	for _, sysAcc := range []*model.Account{transitAcc, platformEquity, feeAcc} {
		acc, err := env.accountingService.GetAccount(ctx, sysAcc.AccountNo)
		require.NoError(t, err)
		snap, err := env.getBalanceSnapshot(ctx, sysAcc.AccountNo, env.cutDate)
		if err != nil {
			continue
		}
		if snap.EndingBalance != acc.Balance {
			t.Errorf("system %s [%s]: snapshot.ending=%d != account.balance=%d",
				sysAcc.AccountNo, acc.AccountCategory, snap.EndingBalance, acc.Balance)
		}

		// 类别符号关系
		var expectedEnd int64
		if acc.AccountCategory == model.AccountCategoryAsset || acc.AccountCategory == model.AccountCategoryExpense {
			expectedEnd = snap.BeginningBalance + snap.TotalDebit - snap.TotalCredit
		} else {
			expectedEnd = snap.BeginningBalance + snap.TotalCredit - snap.TotalDebit
		}
		if expectedEnd != snap.EndingBalance {
			t.Errorf("system %s [%s]: begin+signedDelta=%d != ending=%d",
				sysAcc.AccountNo, acc.AccountCategory, expectedEnd, snap.EndingBalance)
		}
	}

	t.Logf("[OK] all %d users + %d merchants + 3 system accounts reconciled", users, merchants)

	// ── Phase 7: 资金守恒（用户 + 商户净增加 == 中转/equity 净流出）─────────
	// 核心守恒律：所有用户 + 商户余额变化 = 资金净流入（充值） - 资金净流出（提现 + 手续费）
	// 借此再次交叉校验试算结果。
	totalUserBalance := int64(0)
	for _, ua := range userAccs {
		totalUserBalance += env.balance(t, ctx, ua.AccountNo)
	}
	totalMerchantBalance := int64(0)
	for _, ma := range merchantAccs {
		totalMerchantBalance += env.balance(t, ctx, ma.AccountNo)
	}
	transitBal := env.balance(t, ctx, transitAcc.AccountNo)
	equityBal := env.balance(t, ctx, platformEquity.AccountNo)
	feeBal := env.balance(t, ctx, feeAcc.AccountNo)
	t.Logf("balances: users=%d merchants=%d transit=%d equity=%d fee=%d",
		totalUserBalance, totalMerchantBalance, transitBal, equityBal, feeBal)

	// 资产 = 负债 + 权益 + 收入 - 费用
	// transit(资产) = (users + merchants)(负债) + equity(权益) + fee(收入)
	lhs := transitBal
	rhs := totalUserBalance + totalMerchantBalance + equityBal + feeBal
	assert.Equal(t, lhs, rhs,
		"asset (transit=%d) must equal liability+equity+revenue (users=%d + merchants=%d + equity=%d + fee=%d = %d)",
		transitBal, totalUserBalance, totalMerchantBalance, equityBal, feeBal, rhs)
}

// uniqueBizNo64 used internally by the load test to guarantee uniqueness even
// when many goroutines call within the same nanosecond. Falls back to the
// shared uniqueBizNo helper in accounting_test.go for cases where collision
// risk is low.
//
//nolint:unused // kept for future high-contention test variants
func uniqueBizNo64(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), rand.Int63())
}
