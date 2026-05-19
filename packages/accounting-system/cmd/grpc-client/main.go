// accounting-grpc-client: gRPC 测试客户端
// 用法: go run ./cmd/grpc-client [--addr=localhost:50051] [--case=all|tc01~tc12]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	accountingv1 "reconcile-system/packages/accounting-system/kitex_gen/accounting/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// 平台 fleet account_no 直接走数学公式计算（与 EncodeAccountID 对齐）。
// shadow=0, currency=608(PHP), seq=1。
//   shadow * 1e18 + currency * 1e15 + accountType * 1e13 + globalTbl * 1e11 + businessType * 1e7 + seq
func fleetAcct(globalTbl, accountType, businessType int) string {
	const (
		currencyPHP = int64(608)
		seq         = int64(1)
	)
	id := currencyPHP*1_000_000_000_000_000 +
		int64(accountType)*10_000_000_000_000 +
		int64(globalTbl)*100_000_000_000 +
		int64(businessType)*10_000_000 +
		seq
	return strconv.FormatInt(id, 10)
}

var (
	addr     = flag.String("addr", "localhost:53667", "gRPC server address")
	runCase  = flag.String("case", "tc01", "test case: all|tc01~tc12")
	timeout  = flag.Duration("timeout", 10*time.Second, "per-call timeout")
	shadowFl = flag.Bool("shadow", false, "shadow=1 — server 路由到 *_shadow 表 + Redis _shadow key + Kafka _shadow topic")
)

func main() {
	flag.Parse()

	conn, err := grpc.Dial(*addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		log.Fatalf("dial %s failed: %v", *addr, err)
	}
	defer conn.Close()

	c := accountingv1.NewAccountingServiceClient(conn)
	fmt.Printf("✓ connected to %s\n\n", *addr)

	tc := &testClient{c: c, timeout: *timeout, shadow: *shadowFl}
	if *shadowFl {
		fmt.Fprintln(log.Writer(), "🌑 shadow=1 — 所有 RPC 走 *_shadow 路径")
	}

	switch *runCase {
	case "tc00":
		tc.TC0O_DoBalanceTest()
	case "tc01":
		tc.TC01_CreateUserAccount()
	case "tc02":
		tc.TC02_CreatePlatformAccount()
	case "tc03":
		tc.TC03_CreateMerchantAccount()
	case "tc04":
		tc.TC04_GetAccount()
	case "tc05":
		tc.TC055_UserDeposit()
	case "tc06":
		tc.TC06_UserWithdraw()
	case "tc07":
		tc.TC07_UserPayMerchant()
	case "tc08":
		tc.TC08_BatchBooking()
	case "tc09":
		tc.TC09_UserTransfer()
	case "tc10":
		tc.TC10_Idempotency()
	case "tc11":
		tc.TC11_TriggerDayCut()
	case "tc12":
		tc.TC12_DuplicateAccount()
	case "tc13":
		tc.TC13_ComputeBalance()
	default:
		tc.RunAll()
	}
}

// ─── 测试客户端 ───────────────────────────────────────────────────────────────

type testClient struct {
	c       accountingservice.Client
	timeout time.Duration
	shadow  bool // 影子流量开关（-shadow flag 注入）
	// 跨 case 共享的账户号
	userAccNo     string
	platformAccNo string
	merchantAccNo string
}

// ctx 构造每次 RPC 用的 ctx。shadow=true 时挂 x-shadow=1 metadata，
// server 端 interceptor 翻进 ctx 后所有 SQL/Redis/Kafka 自动走 _shadow 路径。
func (tc *testClient) ctx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
	if tc.shadow {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-shadow", "1")
	}
	return ctx, cancel
}
func (tc *testClient) TC0O_DoBalanceTest() {
	fmt.Println("\n[TC00] 余额整体测试（创建账户 → 充值 → 提现 → 支付 → 转账）")
	tc.TC01_CreateUserAccountMutilUser()
	tc.TC03_CreateMerchantAccountMutilUser()
	tc.TC05_UserDepositForMutilUser()
	tc.TC06_UserWithdrawMutilUser()
	tc.TC07_UserPayMerchantMutilUser()
	//tc.TC09_UserTransferMutilUser()
}

func (tc *testClient) RunAll() {
	fmt.Println("═══════════════════════════════════════════")
	fmt.Println("  Accounting gRPC Test Client")
	fmt.Println("═══════════════════════════════════════════")
	tc.TC01_CreateUserAccount()
	tc.TC02_CreatePlatformAccount()
	tc.TC03_CreateMerchantAccount()
	tc.TC04_GetAccount()
	tc.TC055_UserDeposit()
	tc.TC06_UserWithdraw()
	tc.TC07_UserPayMerchant()
	tc.TC08_BatchBooking()
	tc.TC09_UserTransfer()
	tc.TC10_Idempotency()
	tc.TC11_TriggerDayCut()
	tc.TC12_DuplicateAccount()
	fmt.Println("\n═══════════════════════════════════════════")
	fmt.Println("  All tests completed")
	fmt.Println("═══════════════════════════════════════════")
}

// createOrGetAccount 创建账户；若已存在（409）则通过 account_no 查询并返回
// account_no 格式：{dbIdx:02d}{tableIdx:02d}{userId}-{businessType:03d}
func (tc *testClient) createOrGetAccount(
	userID int64,
	accType accountingv1.AccountType,
	category accountingv1.AccountCategory,
	bizType accountingv1.AccountBusinessType,
) (accountNo string, existed bool, err error) {
	ctx, cancel := tc.ctx()
	defer cancel()

	resp, err := tc.c.CreateAccount(ctx, &accountingv1.CreateAccountRequest{
		UserId:              userID,
		AccountType:         accType,
		Category:            category,
		Currency:            "PHP",
		AccountBusinessType: bizType,
	})
	if err != nil {
		return "", false, err
	}
	if resp.Code == 0 {
		return resp.Account.AccountNo, false, nil
	}
	if resp.Code == 409 {
		// 账户已存在，通过 userId + businessType 查询
		ctx2, cancel2 := tc.ctx()
		defer cancel2()
		gr, err2 := tc.c.GetAccount(ctx2, &accountingv1.GetAccountRequest{
			Identifier: &accountingv1.GetAccountRequest_UserIdAndBusinessType{
				UserIdAndBusinessType: &accountingv1.UserIdAndBusinessTypeQuery{
					UserId:              userID,
					AccountBusinessType: bizType,
				},
			},
		})
		if err2 != nil {
			return "", true, fmt.Errorf("get existing account failed: %w", err2)
		}
		if gr.Code != 0 || gr.Account == nil {
			return "", true, fmt.Errorf("get existing account: code=%d msg=%s", gr.Code, gr.Message)
		}
		return gr.Account.AccountNo, true, nil
	}
	return "", false, fmt.Errorf("create account failed: code=%d msg=%s", resp.Code, resp.Message)
}

// ─── TC01: 创建用户余额账户 ───────────────────────────────────────────────────
func (tc *testClient) TC01_CreateUserAccountMutilUser() {
	fmt.Println("\n[TC01] 创建用户余额账户 (user_id=100000002, business_type=USER_BALANCE)")
	for i := range 1000 {
		no, existed, err := tc.createOrGetAccount(
			int64(100000000+i),
			accountingv1.AccountType_ACCOUNT_TYPE_USER,
			accountingv1.AccountCategory_ACCOUNT_CATEGORY_LIABILITY,
			accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_USER_BALANCE,
		)
		if err != nil {
			fmt.Printf("  ✗ %v\n", err)
			continue
		}
		tc.userAccNo = no
		if existed {
			fmt.Printf("  ℹ already exists, account_no=%s\n", no)
		} else {
			fmt.Printf("  ✓ account_no=%s\n", no)
		}
	}
	return
}

func (tc *testClient) TC01_CreateUserAccount() {
	fmt.Println("\n[TC01] 创建用户余额账户 (user_id=100000002, business_type=USER_BALANCE)")
	no, existed, err := tc.createOrGetAccount(
		100000002,
		accountingv1.AccountType_ACCOUNT_TYPE_USER,
		accountingv1.AccountCategory_ACCOUNT_CATEGORY_LIABILITY,
		accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_USER_BALANCE,
	)
	if err != nil {
		fmt.Printf("  ✗ %v\n", err)
		return
	}
	tc.userAccNo = no
	if existed {
		fmt.Printf("  ℹ already exists, account_no=%s\n", no)
	} else {
		fmt.Printf("  ✓ account_no=%s\n", no)
	}
}

// ─── TC02: 创建平台账户 ───────────────────────────────────────────────────────

func (tc *testClient) TC02_CreatePlatformAccount() {
	fmt.Println("\n[TC02] 创建平台损益账户 (user_id=2, business_type=USER_BALANCE)")
	no, existed, err := tc.createOrGetAccount(
		2,
		accountingv1.AccountType_ACCOUNT_TYPE_PLATFORM,
		accountingv1.AccountCategory_ACCOUNT_CATEGORY_EQUITY,
		accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_PLATFORM_PNL,
	)
	if err != nil {
		fmt.Printf("  ✗ %v\n", err)
		return
	}
	tc.platformAccNo = no
	if existed {
		fmt.Printf("  ℹ already exists, account_no=%s\n", no)
	} else {
		fmt.Printf("  ✓ account_no=%s\n", no)
	}
}

// ─── TC03: 创建商户账户 ───────────────────────────────────────────────────────

func (tc *testClient) TC03_CreateMerchantAccountMutilUser() {
	fmt.Println("\n[TC03] 创建商户待结算账户 (user_id=20001, business_type=MERCHANT_PENDING_SETTLE)")
	for i := range 100 {
		no, existed, err := tc.createOrGetAccount(
			int64(20000+i),
			accountingv1.AccountType_ACCOUNT_TYPE_MERCHANT_PENDING_SETTLE,
			accountingv1.AccountCategory_ACCOUNT_CATEGORY_LIABILITY,
			accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_MERCHANT_PENDING_SETTLE,
		)
		if err != nil {
			fmt.Printf("  ✗ %v\n", err)
			continue
		}
		tc.merchantAccNo = no
		if existed {
			fmt.Printf("  ℹ already exists, account_no=%s\n", no)
		} else {
			fmt.Printf("  ✓ account_no=%s\n", no)
		}
	}
}

func (tc *testClient) TC03_CreateMerchantAccount() {
	fmt.Println("\n[TC03] 创建商户待结算账户 (user_id=20001, business_type=MERCHANT_PENDING_SETTLE)")
	no, existed, err := tc.createOrGetAccount(
		20001,
		accountingv1.AccountType_ACCOUNT_TYPE_MERCHANT_PENDING_SETTLE,
		accountingv1.AccountCategory_ACCOUNT_CATEGORY_LIABILITY,
		accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_MERCHANT_PENDING_SETTLE,
	)
	if err != nil {
		fmt.Printf("  ✗ %v\n", err)
		return
	}
	tc.merchantAccNo = no
	if existed {
		fmt.Printf("  ℹ already exists, account_no=%s\n", no)
	} else {
		fmt.Printf("  ✓ account_no=%s\n", no)
	}
}

// ─── TC04: 查询账户 ───────────────────────────────────────────────────────────

func (tc *testClient) TC04_GetAccount() {
	if tc.userAccNo == "" {
		fmt.Println("\n[TC04] skip: run TC01 first")
		return
	}
	fmt.Printf("\n[TC04] 查询账户 account_no=%s\n", tc.userAccNo)
	ctx, cancel := tc.ctx()
	defer cancel()

	resp, err := tc.c.GetAccount(ctx, &accountingv1.GetAccountRequest{
		Identifier: &accountingv1.GetAccountRequest_AccountNo{AccountNo: tc.userAccNo},
	})
	if err != nil {
		fmt.Printf("  ✗ RPC error: %v\n", err)
		return
	}
	if resp.Code != 0 {
		fmt.Printf("  ✗ code=%d msg=%s\n", resp.Code, resp.Message)
		return
	}
	a := resp.Account
	fmt.Printf("  ✓ account_no=%s type=%v business_type=%v balance=%s status=%v\n",
		a.AccountNo, a.AccountType, a.AccountBusinessType, a.Balance, a.Status)
}

func (tc *testClient) TC05_UserDepositForMutilUser() {
	for j := 0; j < 1000; j++ {
		tableindex := j % 100
		account := fmt.Sprintf("%01d%02d%5d-001", tableindex/10, tableindex, 11000+j)
		fmt.Printf("\n[TC05] 用户充值 500 CNY  user=%s  platform=%s\n", account, tc.platformAccNo)
		ctx, cancel := tc.ctx()
		defer cancel()
		tc.userAccNo = account
		accountstr := fleetAcct(j%100, 5, 5) // accountType=5(TRANSITRECEIVE), biz=5
		tc.platformAccNo = accountstr
		resp, err := tc.c.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
			BusinessNo:   fmt.Sprintf("DEP_%d", time.Now().UnixNano()),
			RequestId:    fmt.Sprintf("REQ_%d", time.Now().UnixNano()),
			BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_DEPOSIT,
			Currency:     "PHP",
			Description:  "用户充值",
			Entries: []*accountingv1.AccountingEntry{
				{AccountNo: tc.userAccNo, CreditAmount: "50000", DebitAmount: "0", Description: "充值借"},
				{AccountNo: tc.platformAccNo, CreditAmount: "0", DebitAmount: "50000", Description: "充值贷"},
			},
		})
		if err != nil {
			fmt.Printf("  ✗ RPC error: %v\n", err)
			continue
		}
		if resp.Code != 0 {
			fmt.Printf("  ✗ code=%d msg=%s\n", resp.Code, resp.Message)
			continue
		}
		fmt.Printf("  ✓ voucher_no=%s txs=%v\n", resp.VoucherNo, resp.TransactionIds)

	}

}

func (tc *testClient) TC055_UserDeposit() {
	const (
		workers           = 100
		requestsPerWorker = 100
		amountMinor       = int64(50000) // 500.00 PHP（minor = cents）
	)

	tc.userAccNo = "608010200010000002" // shadow=0|PHP|USER|gtbl=02|biz=001|seq=2
	tc.platformAccNo = "608050000050000001" // shadow=0|PHP|TRANSITRECEIVE|gtbl=00|biz=5|seq=1
	if tc.userAccNo == "" || tc.platformAccNo == "" {
		fmt.Println("\n[TC05] skip: run TC01+TC02 first")
		return
	}

	fmt.Printf("\n[TC05] 压测：%d goroutines × %d 次充值 (%d PHP each)  user=%s  platform=%s\n",
		workers, requestsPerWorker, amountMinor/100, tc.userAccNo, tc.platformAccNo)

	var (
		ok, fail, codeFail int64
		latencySumNs       int64
		latencyMaxNs       int64
		wg                 sync.WaitGroup
	)

	makeBusinessNo := func(wid, i int) string {
		return fmt.Sprintf("DEP-%d-w%03d-%06d", time.Now().UnixNano(), wid, i)
	}
	makeRequestID := func(wid, i int) string {
		return fmt.Sprintf("REQ-%d-w%03d-%06d", time.Now().UnixNano(), wid, i)
	}

	start := time.Now()
	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < requestsPerWorker; i++ {
				ctx, cancel := tc.ctx()
				t0 := time.Now()
				resp, err := tc.c.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
					BusinessNo:   makeBusinessNo(w, i),
					RequestId:    makeRequestID(w, i),
					BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_DEPOSIT,
					Currency:     "PHP",
					Description:  "压测充值",
					Entries: []*accountingv1.AccountingEntry{
						{
							AccountNo:   tc.userAccNo,
							DebitMoney:  &accountingv1.Money{MinorUnits: int64(0), Currency: "PHP"},
							CreditMoney: &accountingv1.Money{MinorUnits: amountMinor, Currency: "PHP"},
							Description: "credit 用户账户",
						},
						{
							AccountNo:   tc.platformAccNo,
							DebitMoney:  &accountingv1.Money{MinorUnits: amountMinor, Currency: "PHP"},
							CreditMoney: &accountingv1.Money{MinorUnits: int64(0), Currency: "PHP"},
							Description: "debit 平台应收",
						},
					},
				})
				cancel()
				dur := time.Since(t0).Nanoseconds()
				atomic.AddInt64(&latencySumNs, dur)
				for {
					cur := atomic.LoadInt64(&latencyMaxNs)
					if dur <= cur || atomic.CompareAndSwapInt64(&latencyMaxNs, cur, dur) {
						break
					}
				}
				switch {
				case err != nil:
					if atomic.AddInt64(&fail, 1) <= 5 {
						fmt.Printf("  ✗ RPC err (w=%d i=%d): %v\n", w, i, err)
					}
				case resp.Code != 0:
					if atomic.AddInt64(&codeFail, 1) <= 5 {
						fmt.Printf("  ✗ code=%d msg=%s (w=%d i=%d)\n", resp.Code, resp.Message, w, i)
					}
				default:
					atomic.AddInt64(&ok, 1)
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := int64(workers * requestsPerWorker)
	avgMs := float64(latencySumNs) / float64(total) / 1e6
	maxMs := float64(latencyMaxNs) / 1e6
	fmt.Printf("\n[TC05] done in %s  ok=%d fail=%d code_fail=%d  qps=%.0f  avg=%.2fms  max=%.2fms\n",
		elapsed.Round(time.Millisecond),
		ok, fail, codeFail,
		float64(total)/elapsed.Seconds(),
		avgMs, maxMs,
	)

}

// ─── TC05: 用户充值 ───────────────────────────────────────────────────────────
func (tc *testClient) TC05_UserDeposit() {

	tc.userAccNo = "608010200010000002" // shadow=0|PHP|USER|gtbl=02|biz=001|seq=2
	tc.platformAccNo = "608050000050000001" // shadow=0|PHP|TRANSITRECEIVE|gtbl=00|biz=5|seq=1
	if tc.userAccNo == "" || tc.platformAccNo == "" {
		fmt.Println("\n[TC05] skip: run TC01+TC02 first")
		return
	}
	fmt.Printf("\n[TC05] 用户充值 500 CNY  user=%s  platform=%s\n", tc.userAccNo, tc.platformAccNo)
	ctx, cancel := tc.ctx()
	defer cancel()

	resp, err := tc.c.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
		BusinessNo:   fmt.Sprintf("DEP_%d", time.Now().UnixNano()),
		RequestId:    fmt.Sprintf("REQ_%d", time.Now().UnixNano()),
		BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_DEPOSIT,
		Currency:     "PHP",
		Description:  "用户充值",
		Entries: []*accountingv1.AccountingEntry{
			{
				AccountNo:   tc.userAccNo,
				DebitMoney:  &accountingv1.Money{Currency: "PHP"},                    // 0
				CreditMoney: &accountingv1.Money{MinorUnits: 50000, Currency: "PHP"}, // 500 PHP
				Description: "充值 credit 用户账户",
			},
			{
				AccountNo:   tc.platformAccNo,
				DebitMoney:  &accountingv1.Money{MinorUnits: 50000, Currency: "PHP"},
				CreditMoney: &accountingv1.Money{Currency: "PHP"},
				Description: "充值 debit 平台应收",
			},
		},
	})
	if err != nil {
		fmt.Printf("  ✗ RPC error: %v\n", err)
		return
	}
	if resp.Code != 0 {
		fmt.Printf("  ✗ code=%d msg=%s\n", resp.Code, resp.Message)
		return
	}
	fmt.Printf("  ✓ voucher_no=%s txs=%v\n", resp.VoucherNo, resp.TransactionIds)

}

func (tc *testClient) TC06_UserWithdrawMutilUser() {
	for j := 0; j < 1000; j++ {
		tableindex := j % 100
		account := fmt.Sprintf("%01d%02d%5d-001", tableindex/10, tableindex, 11000+j)
		tc.userAccNo = account
		tc.platformAccNo = fleetAcct(j%100, 6, 6) // accountType=6(TRANSITPAYABLE), biz=6
		if tc.userAccNo == "" || tc.platformAccNo == "" {
			fmt.Println("\n[TC06] skip: run TC01+TC02 first")
			return
		}
		fmt.Println("\n[TC06] 用户提现 800 CNY")
		ctx, cancel := tc.ctx()
		defer cancel()

		resp, err := tc.c.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
			BusinessNo:   fmt.Sprintf("WD_%d", time.Now().UnixNano()),
			RequestId:    fmt.Sprintf("REQ_%d", time.Now().UnixNano()),
			BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_WITHDRAW,
			Currency:     "PHP",
			Entries: []*accountingv1.AccountingEntry{
				{AccountNo: tc.platformAccNo, CreditAmount: "14000", DebitAmount: "0"},
				{AccountNo: tc.userAccNo, CreditAmount: "0", DebitAmount: "14000"},
			},
		})
		if err != nil {
			fmt.Printf("  ✗ RPC error: %v\n", err)
			continue
		}
		if resp.Code != 0 {
			fmt.Printf("  ✗ code=%d msg=%s\n", resp.Code, resp.Message)
			continue
		}
		fmt.Printf("  ✓ voucher_no=%s\n", resp.VoucherNo)
	}
}

// ─── TC06: 用户提现 ───────────────────────────────────────────────────────────

func (tc *testClient) TC06_UserWithdraw() {
	tc.userAccNo = "608010200010000002" // shadow=0|PHP|USER|gtbl=02|biz=001|seq=2
	tc.platformAccNo = "608060200060000001" // shadow=0|PHP|TRANSITPAYABLE|gtbl=02|biz=6|seq=1

	if tc.userAccNo == "" || tc.platformAccNo == "" {
		fmt.Println("\n[TC06] skip: run TC01+TC02 first")
		return
	}
	fmt.Println("\n[TC06] 用户提现 800 CNY")
	ctx, cancel := tc.ctx()
	defer cancel()

	resp, err := tc.c.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
		BusinessNo:   fmt.Sprintf("WD_%d", time.Now().UnixNano()),
		RequestId:    fmt.Sprintf("REQ_%d", time.Now().UnixNano()),
		BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_WITHDRAW,
		Currency:     "PHP",
		Entries: []*accountingv1.AccountingEntry{
			{AccountNo: tc.platformAccNo, CreditAmount: "140", DebitAmount: "0"},
			{AccountNo: tc.userAccNo, CreditAmount: "0", DebitAmount: "140"},
		},
	})
	if err != nil {
		fmt.Printf("  ✗ RPC error: %v\n", err)
		return
	}
	if resp.Code != 0 {
		fmt.Printf("  ✗ code=%d msg=%s\n", resp.Code, resp.Message)
		return
	}
	fmt.Printf("  ✓ voucher_no=%s\n", resp.VoucherNo)

}
func (tc *testClient) TC07_UserPayMerchantMutilUser() {

	for j := 0; j < 1000; j++ {
		tableindex := j % 100
		userAccount := fmt.Sprintf("%01d%02d%5d-001", tableindex/10, tableindex, 11000+j)
		merchantAccount := "00320003-003"
		tc.userAccNo = userAccount
		tc.platformAccNo = fleetAcct(j%100, 7, 7) // accountType=7(TRANSACTIONFEE), biz=7
		tc.merchantAccNo = merchantAccount
		if tc.userAccNo == "" || tc.merchantAccNo == "" || tc.platformAccNo == "" {
			fmt.Println("\n[TC07] skip: run TC01+TC02+TC03 first")
			return
		}

		fmt.Println("\n[TC07] 用户支付商户 200 CNY（含 5 CNY 手续费）")
		ctx, cancel := tc.ctx()
		defer cancel()

		resp1, err := tc.c.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
			RequestId:    fmt.Sprintf("REQ_%d", time.Now().UnixNano()),
			BusinessNo:   fmt.Sprintf("PAY_%d", time.Now().UnixNano()),
			BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_PAYMENT,
			Currency:     "PHP",
			Description:  "用户支付主流水",
			Entries: []*accountingv1.AccountingEntry{
				{AccountNo: tc.userAccNo, DebitAmount: "200", CreditAmount: "0"},
				{AccountNo: "608090200090000001" /* PHP|TRANSIT|gtbl=02|biz=9|seq=1 */, CreditAmount: "200", DebitAmount: "0"},
			},
		})

		if err != nil || resp1.Code != 0 {
			fmt.Printf("  ✗ 主流水失败: %v %v\n", err, resp1.GetMessage())

			continue
		}
		resp3, err := tc.c.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
			RequestId:    fmt.Sprintf("REQ_%d", time.Now().UnixNano()),
			BusinessNo:   fmt.Sprintf("PAY_%d", time.Now().UnixNano()),
			BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_PAYMENT,
			Currency:     "PHP",
			Description:  "用户支付主流水",
			Entries: []*accountingv1.AccountingEntry{
				{AccountNo: "608090200090000001" /* PHP|TRANSIT|gtbl=02|biz=9|seq=1 */, DebitAmount: "200", CreditAmount: "0"},
				{AccountNo: tc.merchantAccNo, CreditAmount: "200", DebitAmount: "0"},
			},
		})
		if err != nil || resp3.Code != 0 {

			fmt.Printf("  ✗ 主流水失败: %v %v\n", err, resp3.GetMessage())
			continue
		}

		ctx2, cancel2 := tc.ctx()
		defer cancel2()
		resp2, err := tc.c.DoubleEntryBooking(ctx2, &accountingv1.DoubleEntryBookingRequest{
			RequestId:    fmt.Sprintf("REQ_%d", time.Now().UnixNano()),
			BusinessNo:   fmt.Sprintf("FEE_%d", time.Now().UnixNano()),
			BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_COMMISSION,
			Currency:     "PHP",
			Description:  "平台手续费",
			Entries: []*accountingv1.AccountingEntry{
				{AccountNo: tc.merchantAccNo, DebitAmount: "5", CreditAmount: "0"},
				{AccountNo: tc.platformAccNo, CreditAmount: "5", DebitAmount: "0"},
			},
		})
		if err != nil || resp2.Code != 0 {
			fmt.Printf("  ✗ 手续费失败: %v %v\n", err, resp2.GetMessage())
			continue
		}
		fmt.Printf("  ✓ 主流水=%s  手续费=%s\n", resp1.VoucherNo, resp2.VoucherNo)
	}
}

// ─── TC07: 用户支付商户（含手续费）──────────────────────────────────────────

func (tc *testClient) TC07_UserPayMerchant() {

	tc.userAccNo = "608010200010000002" // shadow=0|PHP|USER|gtbl=02|biz=001|seq=2
	tc.platformAccNo = "608070200070000001" // shadow=0|PHP|TRANSACTIONFEE|gtbl=02|biz=7|seq=1
	tc.merchantAccNo = "00120001-003"
	if tc.userAccNo == "" || tc.merchantAccNo == "" || tc.platformAccNo == "" {
		fmt.Println("\n[TC07] skip: run TC01+TC02+TC03 first")
		return
	}
	fmt.Println("\n[TC07] 用户支付商户 200 CNY（含 5 CNY 手续费）")
	ctx, cancel := tc.ctx()
	defer cancel()

	resp1, err := tc.c.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
		RequestId:    fmt.Sprintf("REQ_%d", time.Now().UnixNano()),
		BusinessNo:   fmt.Sprintf("PAY_%d", time.Now().UnixNano()),
		BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_PAYMENT,
		Currency:     "PHP",
		Description:  "用户支付主流水",
		Entries: []*accountingv1.AccountingEntry{
			{AccountNo: tc.userAccNo, DebitAmount: "2000", CreditAmount: "0"},
			{AccountNo: "608090200090000001" /* PHP|TRANSIT|gtbl=02|biz=9|seq=1 */, CreditAmount: "2000", DebitAmount: "0"},
		},
	})
	if err != nil || resp1.Code != 0 {
		fmt.Printf("  ✗ 主流水失败: %v %v\n", err, resp1.GetMessage())
		return
	}
	resp3, err := tc.c.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
		RequestId:    fmt.Sprintf("REQ_%d", time.Now().UnixNano()),
		BusinessNo:   fmt.Sprintf("PAY_%d", time.Now().UnixNano()),
		BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_PAYMENT,
		Currency:     "PHP",
		Description:  "用户支付主流水",
		Entries: []*accountingv1.AccountingEntry{
			{AccountNo: "608090200090000001" /* PHP|TRANSIT|gtbl=02|biz=9|seq=1 */, DebitAmount: "2000", CreditAmount: "0"},
			{AccountNo: tc.merchantAccNo, CreditAmount: "2000", DebitAmount: "0"},
		},
	})
	if err != nil || resp3.Code != 0 {
		fmt.Printf("  ✗ 主流水失败: %v %v\n", err, resp3.GetMessage())
		return
	}

	ctx2, cancel2 := tc.ctx()
	defer cancel2()
	resp2, err := tc.c.DoubleEntryBooking(ctx2, &accountingv1.DoubleEntryBookingRequest{
		RequestId:    fmt.Sprintf("REQ_%d", time.Now().UnixNano()),
		BusinessNo:   fmt.Sprintf("FEE_%d", time.Now().UnixNano()),
		BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_COMMISSION,
		Currency:     "PHP",
		Description:  "平台手续费",
		Entries: []*accountingv1.AccountingEntry{
			{AccountNo: tc.merchantAccNo, DebitAmount: "500", CreditAmount: "0"},
			{AccountNo: tc.platformAccNo, CreditAmount: "500", DebitAmount: "0"},
		},
	})
	if err != nil || resp2.Code != 0 {
		fmt.Printf("  ✗ 手续费失败: %v %v\n", err, resp2.GetMessage())
		return
	}
	fmt.Printf("  ✓ 主流水=%s  手续费=%s\n", resp1.VoucherNo, resp2.VoucherNo)
}

// ─── TC08: 批量记账 ───────────────────────────────────────────────────────────

func (tc *testClient) TC08_BatchBooking() {

	tc.userAccNo = "608010000010000001" // shadow=0|PHP|USER|gtbl=00|biz=001|seq=1
	tc.platformAccNo = "608040000040000001" // shadow=0|PHP|PLATFORM|gtbl=00|biz=4|seq=1

	if tc.userAccNo == "" || tc.platformAccNo == "" {
		fmt.Println("\n[TC08] skip: run TC01+TC02 first")
		return
	}
	fmt.Println("\n[TC08] 批量记账 (3笔)")
	ctx, cancel := tc.ctx()
	defer cancel()

	reqs := make([]*accountingv1.DoubleEntryBookingRequest, 3)
	for i := 0; i < 3; i++ {
		switch i {
		case 0:
			tc.userAccNo = "608010000010000001" // shadow=0|PHP|USER|gtbl=00|biz=001|seq=1
			tc.platformAccNo = "608090000090000001" // shadow=0|PHP|TRANSIT|gtbl=00|biz=9|seq=1
		case 1:
			tc.userAccNo = "608014400010000001" // shadow=0|PHP|USER|gtbl=44|biz=001|seq=1
			tc.platformAccNo = "608094000090000001" // shadow=0|PHP|TRANSIT|gtbl=40|biz=9|seq=1
		case 2:
			tc.userAccNo = "608015500010000001" // shadow=0|PHP|USER|gtbl=55|biz=001|seq=1
			tc.platformAccNo = "608095000090000001" // shadow=0|PHP|TRANSIT|gtbl=50|biz=9|seq=1
		}

		reqs[i] = &accountingv1.DoubleEntryBookingRequest{
			BusinessNo:   fmt.Sprintf("BATCH_%d_%d", i, time.Now().UnixNano()),
			BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_DEPOSIT,
			Currency:     "PHP",
			Entries: []*accountingv1.AccountingEntry{
				{AccountNo: tc.userAccNo, DebitAmount: "1000", CreditAmount: "0"},
				{AccountNo: tc.platformAccNo, CreditAmount: "1000", DebitAmount: "0"},
			},
		}
	}

	resp, err := tc.c.BatchBooking(ctx, &accountingv1.BatchBookingRequest{Requests: reqs})
	if err != nil {
		fmt.Printf("  ✗ RPC error: %v\n", err)
		return
	}
	fmt.Printf("  ✓ total=%d success=%d failed=%d\n", resp.Total, resp.Success, resp.Failed)
}

// ─── TC09: 用户间转账 ─────────────────────────────────────────────────────────

func (tc *testClient) TC09_UserTransfer() {
	fmt.Println("\n[TC09] 创建第二用户账户并转账 50 CNY")

	// 创建/获取接收方账户
	receiverNo, existed, err := tc.createOrGetAccount(
		100000002,
		accountingv1.AccountType_ACCOUNT_TYPE_USER,
		accountingv1.AccountCategory_ACCOUNT_CATEGORY_ASSET,
		accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_USER_BALANCE,
	)
	if err != nil {
		fmt.Printf("  ✗ create receiver failed: %v\n", err)
		return
	}
	if existed {
		fmt.Printf("  ℹ receiver already exists, account_no=%s\n", receiverNo)
	} else {
		fmt.Printf("  ✓ receiver account_no=%s\n", receiverNo)
	}

	if tc.userAccNo == "" || tc.platformAccNo == "" {
		fmt.Println("  skip transfer: run TC01+TC02 first")
		return
	}

	// 先给发送方充值确保余额充足
	ctx1, cancel1 := tc.ctx()
	defer cancel1()
	tc.c.DoubleEntryBooking(ctx1, &accountingv1.DoubleEntryBookingRequest{ //nolint
		BusinessNo:   fmt.Sprintf("DEP_PRE_%d", time.Now().UnixNano()),
		BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_DEPOSIT,
		Currency:     "PHP",
		Entries: []*accountingv1.AccountingEntry{
			{AccountNo: tc.userAccNo, DebitAmount: "10000", CreditAmount: "0"},
			{AccountNo: tc.platformAccNo, CreditAmount: "10000", DebitAmount: "0"},
		},
	})

	ctx2, cancel2 := tc.ctx()
	defer cancel2()
	resp, err := tc.c.DoubleEntryBooking(ctx2, &accountingv1.DoubleEntryBookingRequest{
		BusinessNo:   fmt.Sprintf("TRF_%d", time.Now().UnixNano()),
		BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_TRANSFER,
		Currency:     "PHP",
		Description:  "用户间转账",
		Entries: []*accountingv1.AccountingEntry{
			{AccountNo: tc.userAccNo, CreditAmount: "5000", DebitAmount: "0", Description: "转出"},
			{AccountNo: receiverNo, DebitAmount: "5000", CreditAmount: "0", Description: "转入"},
		},
	})
	if err != nil || resp.Code != 0 {
		fmt.Printf("  ✗ transfer failed: %v %v\n", err, resp.GetMessage())
		return
	}
	fmt.Printf("  ✓ voucher_no=%s  %s → %s\n", resp.VoucherNo, tc.userAccNo, receiverNo)
}

// ─── TC10: 幂等性验证 ─────────────────────────────────────────────────────────

func (tc *testClient) TC10_Idempotency() {
	if tc.userAccNo == "" || tc.platformAccNo == "" {
		fmt.Println("\n[TC10] skip: run TC01+TC02 first")
		return
	}
	bizNo := fmt.Sprintf("IDEM_%d", time.Now().UnixNano())
	fmt.Printf("\n[TC10] 幂等测试 business_no=%s (提交两次)\n", bizNo)

	req := &accountingv1.DoubleEntryBookingRequest{
		BusinessNo:   bizNo,
		BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_DEPOSIT,
		Currency:     "PHP",
		Entries: []*accountingv1.AccountingEntry{
			{AccountNo: tc.userAccNo, DebitAmount: "100", CreditAmount: "0"},
			{AccountNo: tc.platformAccNo, CreditAmount: "100", DebitAmount: "0"},
		},
	}

	ctx1, cancel1 := tc.ctx()
	defer cancel1()
	r1, err1 := tc.c.DoubleEntryBooking(ctx1, req)

	ctx2, cancel2 := tc.ctx()
	defer cancel2()
	r2, err2 := tc.c.DoubleEntryBooking(ctx2, req)

	if err1 != nil || err2 != nil {
		fmt.Printf("  ✗ RPC error: %v / %v\n", err1, err2)
		return
	}
	if r1.VoucherNo == r2.VoucherNo {
		fmt.Printf("  ✓ 幂等通过: voucher=%s\n", r1.VoucherNo)
	} else {
		fmt.Printf("  ✗ 幂等失败: voucher1=%s voucher2=%s\n", r1.VoucherNo, r2.VoucherNo)
	}
}

// ─── TC11: 触发日切 ───────────────────────────────────────────────────────────

func (tc *testClient) TC11_TriggerDayCut() {
	date := time.Now().Format("2006-01-02")
	fmt.Printf("\n[TC11] 触发日切 cut_date=%s\n", date)
	ctx, cancel := tc.ctx()
	defer cancel()

	resp, err := tc.c.TriggerDayCut(ctx, &accountingv1.TriggerDayCutRequest{CutDate: date})
	if err != nil {
		fmt.Printf("  ✗ RPC error: %v\n", err)
		return
	}
	if resp.Code != 0 {
		fmt.Printf("  ✗ code=%d msg=%s\n", resp.Code, resp.Message)
		return
	}
	fmt.Printf("  ✓ day cut triggered for %s\n", date)
}

// ─── TC12: 重复创建账户（期望 409）───────────────────────────────────────────

func (tc *testClient) TC12_DuplicateAccount() {
	fmt.Println("\n[TC12] 重复创建 user_id=10001 + USER_BALANCE → 期望 code=409")
	ctx, cancel := tc.ctx()
	defer cancel()

	resp, err := tc.c.CreateAccount(ctx, &accountingv1.CreateAccountRequest{
		UserId:              10001,
		AccountType:         accountingv1.AccountType_ACCOUNT_TYPE_USER,
		Category:            accountingv1.AccountCategory_ACCOUNT_CATEGORY_ASSET,
		Currency:            "PHP",
		AccountBusinessType: accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_USER_BALANCE,
	})
	if err != nil {
		fmt.Printf("  ✗ RPC error: %v\n", err)
		return
	}
	if resp.Code == 409 {
		fmt.Printf("  ✓ 正确返回 409: %s\n", resp.Message)
		return
	}
	fmt.Printf("  ✗ 期望 409，实际 code=%d msg=%s\n", resp.Code, resp.Message)
}

func (tc *testClient) TC13_ComputeBalance() {
	fmt.Println("\n[TC13] 计算余额")
	ctx, cancel := tc.ctx()
	defer cancel()
	resp, err := tc.c.RunTrialBalance(ctx, &accountingv1.RunTrialBalanceRequest{
		SnapshotDate: "2026-04-10",
	})
	if err != nil {
		fmt.Printf("  ✗ RPC error: %v\n", err)
		return
	}
	if resp.Code == 409 {
		fmt.Printf("  ✓ 正确返回 409: %s\n", resp.Message)
		return
	}
	fmt.Printf("  ✓ 计算完成: %s\n", resp.Message)

}
