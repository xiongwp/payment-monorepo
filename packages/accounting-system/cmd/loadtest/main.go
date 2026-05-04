// cmd/loadtest/main.go
//
// 压测脚本：创建10万账户 + 执行100万笔交易
//
// 账户分布：
//   - 90,000 用户账户（userID 100000-189999，分散在 10库×100表）
//   - 10,000 商户账户（userID 200000-209999）
//   - 1 充值中间账户（系统账户）
//   - 1 服务手续费账户（平台，费率 1%）
//   - 1 平台手续费账户（平台，费率 2%）
//
// 交易类型（100万笔）：
//   - 30% 充值（充值中间账户 → 用户账户）
//   - 30% 用户转账（用户 → 用户）
//   - 40% 支付（用户 → 商户，含手续费）
//     每笔支付：商户收 97%，服务费账户收 1%，平台费账户收 2%
//
// 用法：
//
//	go run -mod=vendor ./cmd/loadtest \
//	  -addr localhost:50051 \
//	  -users 90000 -merchants 10000 -txns 100000 -workers 300
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	accountingv1 "github.com/xiongwp/accounting-grpc-api/gen/accounting/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
)

// ─── flags ────────────────────────────────────────────────────────────────────

var (
	flagAddr        = flag.String("addrs", "localhost:50053,localhost:50054", "accounting-system gRPC 地址")
	flagUsers       = flag.Int("users", 1000, "用户账户数量")
	flagMerchants   = flag.Int("merchants", 100, "商户账户数量")
	flagTxns        = flag.Int("txns", 100000, "压测交易总笔数")
	flagWorkers     = flag.Int("workers", 50, "并发 worker 数")
	flagInitBalance = flag.String("init-balance", "100000", "每个用户的初始充值金额（CNY）")
	flagFee1        = flag.Float64("fee1", 0.01, "固定服务费费率（默认 1%）")
	flagFee2        = flag.Float64("fee2", 0.02, "平台手续费费率（默认 2%）")
	flagMinAmt      = flag.Float64("min-amt", 100, "单笔最小金额（CNY）")
	flagMaxAmt      = flag.Float64("max-amt", 50000, "单笔最大金额（CNY）")
	flagSetupOnly   = flag.Bool("setup-only", false, "仅执行账户创建和充值，不跑压测")
	flagTxnOnly     = flag.Bool("txn-only", false, "跳过账户创建，直接读取账户文件执行压测")
	flagAccountFile = flag.String("account-file", "/tmp/loadtest_accounts.txt", "账户号持久化文件")
)

// ─── metrics ──────────────────────────────────────────────────────────────────

type metrics struct {
	success   atomic.Int64
	failure   atomic.Int64
	recharge  atomic.Int64
	transfer  atomic.Int64
	payment   atomic.Int64
	startTime time.Time
}

func (m *metrics) tps() float64 {
	elapsed := time.Since(m.startTime).Seconds()
	if elapsed == 0 {
		return 0
	}
	return float64(m.success.Load()) / elapsed
}

// ─── global state ─────────────────────────────────────────────────────────────

var (
	userAccNos     []string
	merchantAccNos []string
	transitAccNo   string
	feeAcc1No      string // 服务手续费
	feeAcc2No      string // 平台手续费
	accMu          sync.RWMutex
)

// ─── main ─────────────────────────────────────────────────────────────────────

func main() {
	flag.Parse()

	conn, err := grpc.Dial(
		"static:///"+*flagAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`),
	)
	if err != nil {
		log.Fatalf("连接 gRPC 失败 %s: %v", *flagAddr, err)
	}
	defer conn.Close()
	cli := accountingv1.NewAccountingServiceClient(conn)

	if *flagTxnOnly {
		if err := loadAccountsFromFile(*flagAccountFile); err != nil {
			log.Fatalf("加载账户文件失败: %v", err)
		}
	} else {
		runSetup(cli)
		if err := saveAccountsToFile(*flagAccountFile); err != nil {
			log.Printf("警告：账户文件保存失败: %v", err)
		}
	}

	if *flagSetupOnly {
		log.Println("--setup-only 模式：跳过压测，退出")
		return
	}
	runLoadTest(cli)
}

// ─── setup phase ──────────────────────────────────────────────────────────────

func runSetup(cli accountingv1.AccountingServiceClient) {
	log.Println("═══ Phase 1: 创建系统账户 ═══")
	mustCreateSystemAccounts(cli)

	log.Printf("═══ Phase 2: 创建 %d 用户账户 ═══", *flagUsers)
	createAccounts(cli, *flagUsers, 100_000_000, // 用户 ID 从 1e8 起（[1,10000] 已预留给系统）
		accountingv1.AccountType_ACCOUNT_TYPE_USER,
		accountingv1.AccountCategory_ACCOUNT_CATEGORY_LIABILITY,
		accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_USER_BALANCE,
		&userAccNos,
	)

	log.Printf("═══ Phase 3: 创建 %d 商户账户 ═══", *flagMerchants)
	createAccounts(cli, *flagMerchants, 900_000_000, // 商户 ID 从 9e8 起
		accountingv1.AccountType_ACCOUNT_TYPE_MERCHANT_PENDING_SETTLE,
		accountingv1.AccountCategory_ACCOUNT_CATEGORY_LIABILITY,
		accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_MERCHANT_PENDING_SETTLE,
		&merchantAccNos,
	)

	log.Printf("═══ Phase 4: 为 %d 个用户充值（初始余额 %s CNY）═══",
		len(userAccNos), *flagInitBalance)
	initialRecharge(cli)
}

func mustCreateSystemAccounts(cli accountingv1.AccountingServiceClient) {
	ctx := context.Background()

	create := func(userID int64,
		bizType accountingv1.AccountBusinessType, label string,
	) string {

		qResp, err := cli.GetAccount(ctx, &accountingv1.GetAccountRequest{
			Identifier: &accountingv1.GetAccountRequest_UserIdAndBusinessType{
				UserIdAndBusinessType: &accountingv1.UserIdAndBusinessTypeQuery{
					UserId: userID, AccountBusinessType: bizType,
				},
			},
		})
		if err != nil || qResp.Account == nil {
			log.Fatalf("查询%s失败", label)
		}
		log.Printf("  %s (已存在): %s", label, qResp.Account.AccountNo)
		return qResp.Account.AccountNo
	}

	transitAccNo = "001_PLATFORM_TCHANNEL_RECEIVABLE"
	feeAcc1No = create(2,
		accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE,
		"服务手续费账户(1%)")
	feeAcc2No = create(3,
		accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE,
		"平台手续费账户(2%)")
}

func createAccounts(
	cli accountingv1.AccountingServiceClient,
	count int,
	userIDStart int64,
	accType accountingv1.AccountType,
	cat accountingv1.AccountCategory,
	bizType accountingv1.AccountBusinessType,
	result *[]string,
) {
	type item struct {
		idx    int
		userID int64
	}
	jobs := make(chan item, 1000)
	results := make([]string, count)
	var wg sync.WaitGroup
	var ok, fail atomic.Int64

	workers := *flagWorkers
	if workers > 500 {
		workers = 500
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for j := range jobs {
				resp, err := cli.CreateAccount(ctx, &accountingv1.CreateAccountRequest{
					UserId: j.userID, AccountType: accType,
					Category: cat, AccountBusinessType: bizType, Currency: "PHP",
				})
				if err != nil || (resp.Code != 0 && resp.Code != 409) {
					fail.Add(1)
					continue
				}
				accNo := ""
				if resp.Account != nil {
					accNo = resp.Account.AccountNo
				} else if resp.Code == 409 {
					// 已存在：查一次
					qr, qErr := cli.GetAccount(ctx, &accountingv1.GetAccountRequest{
						Identifier: &accountingv1.GetAccountRequest_UserIdAndBusinessType{
							UserIdAndBusinessType: &accountingv1.UserIdAndBusinessTypeQuery{
								UserId: j.userID, AccountBusinessType: bizType,
							},
						},
					})
					if qErr == nil && qr.Account != nil {
						accNo = qr.Account.AccountNo
					}
				}
				if accNo != "" {
					results[j.idx] = accNo
					ok.Add(1)
				} else {
					fail.Add(1)
				}
			}
		}()
	}

	// 进度打印
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			done := ok.Load() + fail.Load()
			if done >= int64(count) {
				return
			}
			log.Printf("  进度: %d/%d (失败 %d)", done, count, fail.Load())
		}
	}()

	for i := 0; i < count; i++ {
		jobs <- item{idx: i, userID: userIDStart + int64(i)}
	}
	close(jobs)
	wg.Wait()

	// 只保留成功创建的账户
	for _, no := range results {
		if no != "" {
			*result = append(*result, no)
		}
	}
	log.Printf("  完成: 成功 %d, 失败 %d", ok.Load(), fail.Load())
}

func initialRecharge(cli accountingv1.AccountingServiceClient) {
	jobs := make(chan string, 1000)
	var wg sync.WaitGroup
	var ok, fail atomic.Int64
	workers := 1
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			ctx := context.Background()
			for accNo := range jobs {
				bno := bizNo("RCG")
				val := rand.Int63()
				str := strconv.FormatInt(val, 10)
				accountstr := fmt.Sprintf("0%02d_PLATFORM_TCHANNEL_RECEIVABLE", i%100)
				_, err := cli.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
					RequestId:    str,
					BusinessNo:   bno,
					BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_DEPOSIT,
					Currency:     "PHP",
					Description:  "用户充值",
					Entries: []*accountingv1.AccountingEntry{
						{AccountNo: accNo, CreditAmount: "500000", DebitAmount: "0", Description: "充值借"},
						{AccountNo: accountstr, CreditAmount: "0", DebitAmount: "500000", Description: "充值贷"},
					},
				})
				if err != nil {
					fail.Add(1)
				} else {
					ok.Add(1)
				}
			}
		}(int64(i) * time.Now().UnixNano())
	}

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		total := int64(len(userAccNos))
		for range ticker.C {
			done := ok.Load() + fail.Load()
			if done >= total {
				return
			}
			log.Printf("  充值进度: %d/%d (失败 %d)", done, total, fail.Load())
		}
	}()

	for _, no := range userAccNos {
		jobs <- no
	}
	close(jobs)
	wg.Wait()
	log.Printf("  初始充值完成: 成功 %d, 失败 %d", ok.Load(), fail.Load())
}

// ─── load test ────────────────────────────────────────────────────────────────

func runLoadTest(cli accountingv1.AccountingServiceClient) {
	log.Printf("═══ Phase 5: 压测开始 (%d 笔 / %d 并发) ═══", *flagTxns, *flagWorkers)

	m := &metrics{startTime: time.Now()}
	remaining := atomic.Int64{}
	remaining.Store(int64(*flagTxns))

	var wg sync.WaitGroup
	for i := 0; i < *flagWorkers; i++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			ctx := context.Background()
			for remaining.Add(-1) >= 0 {
				var err error
				pick := rng.Float64()
				switch {
				case pick < 0.30:
					err = txnRecharge(ctx, cli, rng)
					if err == nil {
						m.recharge.Add(1)
					}
				case pick < 0.60:
					err = txnTransfer(ctx, cli, rng)
					if err == nil {
						m.transfer.Add(1)
					}
				default:
					err = txnPayment(ctx, cli, rng)
					if err == nil {
						m.payment.Add(1)
					}
				}
				if err != nil {
					m.failure.Add(1)
				} else {
					m.success.Add(1)
				}
			}
		}(int64(i) * time.Now().UnixNano())
	}

	// 实时进度打印（每秒）
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		prev := int64(0)
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				cur := m.success.Load()
				instTPS := cur - prev
				prev = cur
				total := int64(*flagTxns)
				log.Printf("  TPS: %-8d | 累计: %-10d / %-10d | 失败: %d",
					instTPS, cur, total, m.failure.Load())
			}
		}
	}()

	wg.Wait()
	close(done)
	elapsed := time.Since(m.startTime)

	printSummary(m, elapsed)
}

// ─── transaction helpers ──────────────────────────────────────────────────────

func randomAmount(rng *rand.Rand) string {
	// 生成 min~max 范围内、精确到分的随机金额
	cents := int(*flagMinAmt*100) + rng.Intn(int((*flagMaxAmt-*flagMinAmt)*100)+1)
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func randomUserAcc(rng *rand.Rand) string {
	accMu.RLock()
	defer accMu.RUnlock()
	if len(userAccNos) == 0 {
		return ""
	}
	return userAccNos[rng.Intn(len(userAccNos))]
}

func randomMerchantAcc(rng *rand.Rand) string {
	accMu.RLock()
	defer accMu.RUnlock()
	if len(merchantAccNos) == 0 {
		return ""
	}
	return merchantAccNos[rng.Intn(len(merchantAccNos))]
}

func bizNo(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()+rand.Int63n(1e9))
}

// txnRecharge: 充值中间账户 → 用户账户
func txnRecharge(ctx context.Context, cli accountingv1.AccountingServiceClient, rng *rand.Rand) error {
	userAcc := randomUserAcc(rng)
	if userAcc == "" {
		return fmt.Errorf("no user account")
	}
	amt := randomAmount(rng)
	bno := bizNo("RCG")
	val := rand.Int63()
	str := strconv.FormatInt(val, 10)
	resp, err := cli.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
		RequestId:    str,
		BusinessNo:   bno,
		BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_DEPOSIT,
		Currency:     "PHP",
		Description:  "用户充值",
		Entries: []*accountingv1.AccountingEntry{
			{AccountNo: transitAccNo, DebitAmount: amt, CreditAmount: "0"},
			{AccountNo: userAcc, CreditAmount: amt, DebitAmount: "0"},
		},
	})
	if err != nil {
		fmt.Printf("  ✗ RPC error: %v\n", err)
	}
	if resp.Code != 0 {
		fmt.Printf("  ✗ code=%d msg=%s\n", resp.Code, resp.Message)
	}
	return err
}

// txnTransfer: 用户A → 用户B
func txnTransfer(ctx context.Context, cli accountingv1.AccountingServiceClient, rng *rand.Rand) error {
	sender := randomUserAcc(rng)
	receiver := randomUserAcc(rng)
	if sender == "" || receiver == "" || sender == receiver {
		return fmt.Errorf("invalid accounts")
	}
	amt := randomAmount(rng)
	bno := bizNo("TFR")
	val := rand.Int63()
	str := strconv.FormatInt(val, 10)
	resp, err := cli.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
		RequestId:    str,
		BusinessNo:   bno,
		BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_TRANSFER,
		Currency:     "PHP",
		Description:  "用户转账",
		Entries: []*accountingv1.AccountingEntry{
			{AccountNo: sender, CreditAmount: amt, DebitAmount: "0", Description: "转出"},
			{AccountNo: receiver, DebitAmount: amt, CreditAmount: "0", Description: "转入"},
		},
	})
	if err != nil {
		fmt.Printf("  ✗ RPC error: %v\n", err)
	}
	if resp.Code != 0 {
		fmt.Printf("  ✗ code=%d msg=%s\n", resp.Code, resp.Message)
	}
	return err
}

// txnPayment: 用户 → 商户（含 1% 服务费 + 2% 平台手续费）
//
// 金额分配（以 X 为例）：
//
//	商户到账：X × 97%
//	服务手续费账户：X × 1%
//	平台手续费账户：X × 2%
//	借方合计 = 贷方合计 = X  （复式记账平衡）
func txnPayment(ctx context.Context, cli accountingv1.AccountingServiceClient, rng *rand.Rand) error {
	userAcc := randomUserAcc(rng)
	merchantAcc := randomMerchantAcc(rng)
	if userAcc == "" || merchantAcc == "" {
		return fmt.Errorf("no accounts")
	}

	// 金额精确到分计算
	cents := 10000
	fee1Cents := int(float64(cents) * *flagFee1)
	fee2Cents := int(float64(cents) * *flagFee2)
	merchantCents := cents - fee1Cents - fee2Cents

	merchantAmt := fmt.Sprintf("%d.%02d", merchantCents/100, merchantCents%100)
	fee1Amt := fmt.Sprintf("%d.%02d", fee1Cents/100, fee1Cents%100)
	fee2Amt := fmt.Sprintf("%d.%02d", fee2Cents/100, fee2Cents%100)
	merchantAmtint := strings.ReplaceAll(merchantAmt, ".", "")
	fee1Amtint := strings.ReplaceAll(fee1Amt, ".", "")
	fee2Amtint := strings.ReplaceAll(fee2Amt, ".", "")

	bno := bizNo("PAY")
	val := rand.Int63()
	str := strconv.FormatInt(val, 10)
	resp, err := cli.DoubleEntryBooking(ctx, &accountingv1.DoubleEntryBookingRequest{
		RequestId:    str,
		BusinessNo:   bno,
		BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_PAYMENT,
		Currency:     "PHP",
		Description:  "用户支付",
		Entries: []*accountingv1.AccountingEntry{
			// 贷方：用户支付全额
			{AccountNo: userAcc, DebitAmount: strconv.Itoa(cents), CreditAmount: "0"},
			// 借方：商户收款（97%）
			{AccountNo: merchantAcc, CreditAmount: merchantAmtint, DebitAmount: "0"},
			// 借方：服务手续费（1%）
			{AccountNo: feeAcc1No, CreditAmount: fee1Amtint, DebitAmount: "0"},
			// 借方：平台手续费（2%）
			{AccountNo: feeAcc2No, CreditAmount: fee2Amtint, DebitAmount: "0"},
		},
	})
	if err != nil || resp.Code != 0 {
		fmt.Printf(" 支付: %v %v\n", err, resp.GetMessage())
	}
	return err
}

// ─── account file I/O ─────────────────────────────────────────────────────────

func saveAccountsToFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(f, "transit=%s\n", transitAccNo)
	fmt.Fprintf(f, "fee1=%s\n", feeAcc1No)
	fmt.Fprintf(f, "fee2=%s\n", feeAcc2No)
	for _, no := range userAccNos {
		fmt.Fprintf(f, "user=%s\n", no)
	}
	for _, no := range merchantAccNos {
		fmt.Fprintf(f, "merchant=%s\n", no)
	}
	log.Printf("账户信息已保存到 %s", path)
	return nil
}

func loadAccountsFromFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var kind, val string
	for {
		_, err := fmt.Fscanf(f, "%s\n", &val)
		if err != nil {
			break
		}
		// parse key=value
		for i, c := range val {
			if c == '=' {
				kind = val[:i]
				val = val[i+1:]
				break
			}
		}
		switch kind {
		case "transit":
			transitAccNo = val
		case "fee1":
			feeAcc1No = val
		case "fee2":
			feeAcc2No = val
		case "user":
			userAccNos = append(userAccNos, val)
		case "merchant":
			merchantAccNos = append(merchantAccNos, val)
		}
	}
	log.Printf("从文件加载账户: user=%d merchant=%d transit=%s fee1=%s fee2=%s",
		len(userAccNos), len(merchantAccNos), transitAccNo, feeAcc1No, feeAcc2No)
	return nil
}

// ─── summary ──────────────────────────────────────────────────────────────────

func printSummary(m *metrics, elapsed time.Duration) {
	total := m.success.Load() + m.failure.Load()
	avgTPS := float64(m.success.Load()) / elapsed.Seconds()
	successRate := float64(m.success.Load()) / float64(total) * 100

	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "═══════════════════════════════════════")
	fmt.Fprintln(w, "           压 测 结 果 汇 总")
	fmt.Fprintln(w, "═══════════════════════════════════════")
	fmt.Fprintf(w, "总耗时\t%v\n", elapsed.Round(time.Millisecond))
	fmt.Fprintf(w, "总请求\t%d\n", total)
	fmt.Fprintf(w, "成功\t%d\n", m.success.Load())
	fmt.Fprintf(w, "失败\t%d\n", m.failure.Load())
	fmt.Fprintf(w, "成功率\t%.2f%%\n", successRate)
	fmt.Fprintf(w, "平均 TPS\t%.0f\n", avgTPS)
	fmt.Fprintln(w, "───────────────────────────────────────")
	fmt.Fprintf(w, "充值笔数\t%d\n", m.recharge.Load())
	fmt.Fprintf(w, "转账笔数\t%d\n", m.transfer.Load())
	fmt.Fprintf(w, "支付笔数\t%d\n", m.payment.Load())
	fmt.Fprintln(w, "───────────────────────────────────────")
	fmt.Fprintf(w, "用户账户数\t%d\n", len(userAccNos))
	fmt.Fprintf(w, "商户账户数\t%d\n", len(merchantAccNos))
	fmt.Fprintf(w, "充值中间账户\t%s\n", transitAccNo)
	fmt.Fprintf(w, "服务手续费账户(1%%)\t%s\n", feeAcc1No)
	fmt.Fprintf(w, "平台手续费账户(2%%)\t%s\n", feeAcc2No)
	fmt.Fprintln(w, "═══════════════════════════════════════")
	w.Flush()
}

type staticResolverBuilder struct{}

func (*staticResolverBuilder) Scheme() string {
	return "static"
}

func (*staticResolverBuilder) Build(
	target resolver.Target,
	cc resolver.ClientConn,
	opts resolver.BuildOptions,
) (resolver.Resolver, error) {

	addrs := strings.Split(target.Endpoint(), ",")

	state := resolver.State{
		Addresses: make([]resolver.Address, 0, len(addrs)),
	}

	for _, a := range addrs {
		state.Addresses = append(state.Addresses, resolver.Address{Addr: a})
	}

	_ = cc.UpdateState(state)
	return &staticResolver{}, nil
}

type staticResolver struct{}

func (*staticResolver) ResolveNow(resolver.ResolveNowOptions) {}
func (*staticResolver) Close()                                {}

func init() {
	resolver.Register(&staticResolverBuilder{})
}
