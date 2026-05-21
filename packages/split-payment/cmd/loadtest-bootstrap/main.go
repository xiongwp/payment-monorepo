// Package main — loadtest 账户池预创建工具
//
// 为啥单独一个 binary：accounting-system 不 lazy-create 账户。loadtest 的 event
// 里 attrs[*_account] 必须是 accounting 已经存在的 account_no（19 位数字字符串，
// 内部 generateAccountNo 编码出来的，调用方不能指定）。所以压测前需要：
//
//   1. 通过 accounting 提供的 API 真实创建出一批账户（每个 owner_id × business_type
//      × currency 一笔，幂等）；
//   2. 把每笔创建返回的 account_no 收集起来；
//   3. 写到一个 JSON pool 文件，让 loadtest 启动期 mmap 进内存，dispatch 时按
//      (channel_id / user_id / merchant_id) 索引到真实 account_no，填进 event.attrs。
//
// API 选择：
//   - user / merchant / merchant_pending（业务账户）：accounting.gRPC.CreateAccount
//     （HTTP 没暴露这条；business_type 1/2/3 是预置）
//   - channel-* / platform-*（系统内部账户）：HTTP POST /admin/platform-accounts
//     （reserved_id ∈ [1, 10_000]，account_type ∈ {4,5,6,7,8,9}）
//
// 调用方式：
//   loadtest-bootstrap \
//     --accounting-grpc=accounting-service:50051 \
//     --accounting-http=http://accounting-service:8888 \
//     --output=/output/account_pool.json \
//     --num-users=100 --num-merchants=100 --num-channels=100 --currency=PHP
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/cloudwego/kitex/client"

	acctv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
	acctsvc "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingservice"
)

// ─── Pool 结构 ────────────────────────────────────────────────────────────

// AccountPool 是 loadtest 启动期读的 JSON pool。
// 字段名跟 loadtest event.attrs 里的 key 完全对应，方便 dispatch 直接索引。
type AccountPool struct {
	Currency         string             `json:"currency"`
	Users            []string           `json:"users"`             // 100 entries, indexed by user_id-1
	Merchants        []string           `json:"merchants"`         // 100 entries, indexed by merchant_id-1
	MerchantPendings []string           `json:"merchant_pendings"` // 同上，商户待结算
	Channels         []ChannelAccounts  `json:"channels"`          // 100 entries, indexed by channel_id-1
	Platform         PlatformAccounts   `json:"platform"`
}

// ChannelAccounts 一个渠道下的 4 类账户。
// 对应 loadtest topup/withdraw 资金流里的 channel_* attrs。
type ChannelAccounts struct {
	Receivable string `json:"recv"`     // channel_receivable_account → type=5
	Suspense   string `json:"suspense"` // channel_suspense_account   → type=9
	Fee        string `json:"fee"`      // channel_fee_account        → type=7
	Payable    string `json:"payable"`  // channel_payable_account    → type=6
}

// PlatformAccounts 3 个全局平台账户。
type PlatformAccounts struct {
	FeeClearing     string `json:"fee_clearing"`     // platform/fee_clearing     → type=4
	FeeRevenue      string `json:"fee_revenue"`      // platform/fee_revenue      → type=4
	WithdrawPending string `json:"withdraw_pending"` // platform/withdraw_pending → type=9
}

// ─── Owner ID 分配 ────────────────────────────────────────────────────────
//
// 系统内部账户 owner_id ∈ [1, ReservedOwnerIDMax=10000]，业务账户必须 > 10000。
// 为了同一通道下 4 个子账户在不同 (owner_id, biz_type, currency) 槽位里都唯一，
// 给每个子账户类型留 1000 段 owner_id。
const (
	channelRecvBase     = 1     // recv:     reserved_id 1..numChannels
	channelSuspenseBase = 1000  // suspense: 1001..1000+numChannels
	channelFeeBase      = 2000  // fee:      2001..2000+numChannels
	channelPayableBase  = 3000  // payable:  3001..3000+numChannels
	platformBase        = 9000  // 平台账户 9001/9002/9003

	userOwnerBase     = 20000 // user:     20001..20000+numUsers
	merchantOwnerBase = 30000 // merchant: 30001..30000+numMerchants
)

// ─── flags ────────────────────────────────────────────────────────────────

var (
	flagAcctGRPC    = flag.String("accounting-grpc", "accounting-service:50051", "accounting gRPC endpoint")
	flagAcctHTTP    = flag.String("accounting-http", "http://accounting-service:8888", "accounting admin HTTP endpoint")
	flagOutput      = flag.String("output", "/output/account_pool.json", "output pool JSON path")
	flagNumUsers    = flag.Int("num-users", 100, "用户账户数量")
	flagNumMerchant = flag.Int("num-merchants", 100, "商户账户数量")
	flagNumChannels = flag.Int("num-channels", 100, "渠道数量")
	flagCurrency    = flag.String("currency", "PHP", "币种")
	flagWorkers     = flag.Int("workers", 16, "并发 worker 数")
	flagRPCTimeout  = flag.Duration("rpc-timeout", 10*time.Second, "每次 RPC 超时")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("=== loadtest-bootstrap ===")
	fmt.Printf("  accounting gRPC: %s\n", *flagAcctGRPC)
	fmt.Printf("  accounting HTTP: %s\n", *flagAcctHTTP)
	fmt.Printf("  output:          %s\n", *flagOutput)
	fmt.Printf("  users=%d merchants=%d channels=%d currency=%s workers=%d\n",
		*flagNumUsers, *flagNumMerchant, *flagNumChannels, *flagCurrency, *flagWorkers)

	// gRPC client (用户 / 商户 / 商户待结算 走这条)
	gClient, err := acctsvc.NewClient(
		"accounting",
		client.WithHostPorts(*flagAcctGRPC),
		client.WithRPCTimeout(*flagRPCTimeout),
	)
	if err != nil {
		return fmt.Errorf("create accounting gRPC client: %w", err)
	}

	// HTTP client (平台/中转/费用 走这条)
	httpC := &http.Client{Timeout: *flagRPCTimeout}

	pool := &AccountPool{
		Currency:         *flagCurrency,
		Users:            make([]string, *flagNumUsers),
		Merchants:        make([]string, *flagNumMerchant),
		MerchantPendings: make([]string, *flagNumMerchant),
		Channels:         make([]ChannelAccounts, *flagNumChannels),
	}

	// 全部 5 段并行创建（每段内部 worker pool）
	t0 := time.Now()
	if err := createUsers(gClient, pool); err != nil {
		return fmt.Errorf("create users: %w", err)
	}
	fmt.Printf(">>> users 完成 (%d 个, %.1fs)\n", len(pool.Users), time.Since(t0).Seconds())

	t1 := time.Now()
	if err := createMerchants(gClient, pool); err != nil {
		return fmt.Errorf("create merchants: %w", err)
	}
	fmt.Printf(">>> merchants + pendings 完成 (%d 个 × 2, %.1fs)\n",
		len(pool.Merchants), time.Since(t1).Seconds())

	t2 := time.Now()
	if err := createChannels(httpC, pool); err != nil {
		return fmt.Errorf("create channels: %w", err)
	}
	fmt.Printf(">>> channels 完成 (%d 个 × 4 子类, %.1fs)\n",
		len(pool.Channels), time.Since(t2).Seconds())

	t3 := time.Now()
	if err := createPlatform(httpC, pool); err != nil {
		return fmt.Errorf("create platform accounts: %w", err)
	}
	fmt.Printf(">>> platform 完成 (3 个, %.1fs)\n", time.Since(t3).Seconds())

	if err := writePool(*flagOutput, pool); err != nil {
		return fmt.Errorf("write pool: %w", err)
	}
	fmt.Printf(">>> pool 已写到 %s（总耗时 %.1fs）\n", *flagOutput, time.Since(t0).Seconds())
	return nil
}

// ─── gRPC: 用户 / 商户 / 待结算 ───────────────────────────────────────────

// createUsers 创建 N 个用户余额账户。
func createUsers(gClient acctsvc.Client, pool *AccountPool) error {
	return parallelCreate(*flagNumUsers, *flagWorkers, func(i int) error {
		userID := int64(userOwnerBase + 1 + i)
		acctNo, err := grpcCreate(gClient, userID,
			acctv1.AccountType_ACCOUNT_TYPE_USER,
			acctv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_USER_BALANCE,
			acctv1.AccountCategory_ACCOUNT_CATEGORY_LIABILITY)
		if err != nil {
			return err
		}
		pool.Users[i] = acctNo
		return nil
	})
}

// createMerchants 同时创建 100 个商户主账户 + 100 个商户待结算账户。
// 用同一段 owner_id（biz_type 不同就 idempotency-key 不同了）。
func createMerchants(gClient acctsvc.Client, pool *AccountPool) error {
	if err := parallelCreate(*flagNumMerchant, *flagWorkers, func(i int) error {
		merchantID := int64(merchantOwnerBase + 1 + i)
		acctNo, err := grpcCreate(gClient, merchantID,
			acctv1.AccountType_ACCOUNT_TYPE_MERCHANT,
			acctv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_MERCHANT_BALANCE,
			acctv1.AccountCategory_ACCOUNT_CATEGORY_LIABILITY)
		if err != nil {
			return err
		}
		pool.Merchants[i] = acctNo
		return nil
	}); err != nil {
		return err
	}
	return parallelCreate(*flagNumMerchant, *flagWorkers, func(i int) error {
		merchantID := int64(merchantOwnerBase + 1 + i)
		acctNo, err := grpcCreate(gClient, merchantID,
			acctv1.AccountType_ACCOUNT_TYPE_MERCHANT_PENDING_SETTLE,
			acctv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_MERCHANT_PENDING_SETTLE,
			acctv1.AccountCategory_ACCOUNT_CATEGORY_LIABILITY)
		if err != nil {
			return err
		}
		pool.MerchantPendings[i] = acctNo
		return nil
	})
}

// grpcCreate 调 accounting.CreateAccount，返回 account_no。
// CreateAccount 本身幂等：同 (user_id, business_type, currency) 重复调返回原账户。
func grpcCreate(c acctsvc.Client, userID int64,
	at acctv1.AccountType, bt acctv1.AccountBusinessType, cat acctv1.AccountCategory) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), *flagRPCTimeout)
	defer cancel()
	resp, err := c.CreateAccount(ctx, &acctv1.CreateAccountRequest{
		UserId:              userID,
		AccountType:         at,
		AccountBusinessType: bt,
		Category:            cat,
		Currency:            *flagCurrency,
	})
	if err != nil {
		return "", fmt.Errorf("CreateAccount(uid=%d bt=%d): %w", userID, bt, err)
	}
	if resp.Code != 0 {
		return "", fmt.Errorf("CreateAccount(uid=%d bt=%d): code=%d msg=%s", userID, bt, resp.Code, resp.Message)
	}
	if resp.Account == nil || resp.Account.AccountNo == "" {
		return "", fmt.Errorf("CreateAccount(uid=%d bt=%d): empty account_no in resp", userID, bt)
	}
	return resp.Account.AccountNo, nil
}

// ─── HTTP: 渠道子账户 + 平台账户 ──────────────────────────────────────────

// createChannels 每个渠道 4 个子账户（recv/suspense/fee/payable）。
// 4 个子类用不同的 reserved_id 段，避开 (owner_id, biz_type) 唯一键冲突。
func createChannels(httpC *http.Client, pool *AccountPool) error {
	subs := []struct {
		name    string
		base    int
		accType int // AccountType 枚举值
		setter  func(*ChannelAccounts, string)
	}{
		{"recv", channelRecvBase, 5, func(c *ChannelAccounts, no string) { c.Receivable = no }},
		{"suspense", channelSuspenseBase, 9, func(c *ChannelAccounts, no string) { c.Suspense = no }},
		{"fee", channelFeeBase, 7, func(c *ChannelAccounts, no string) { c.Fee = no }},
		{"payable", channelPayableBase, 6, func(c *ChannelAccounts, no string) { c.Payable = no }},
	}
	for _, s := range subs {
		s := s // capture
		fmt.Printf("    channel-%s: type=%d  reserved_id %d..%d ...\n", s.name, s.accType, s.base+1, s.base+*flagNumChannels)
		if err := parallelCreate(*flagNumChannels, *flagWorkers, func(i int) error {
			reservedID := int64(s.base + 1 + i)
			acctNo, err := httpCreatePlatform(httpC, reservedID, s.accType)
			if err != nil {
				return fmt.Errorf("channel-%s ch=%d: %w", s.name, i+1, err)
			}
			s.setter(&pool.Channels[i], acctNo)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// createPlatform 3 个全局平台账户：fee_clearing / fee_revenue / withdraw_pending。
func createPlatform(httpC *http.Client, pool *AccountPool) error {
	// type=4 (Platform) 对 biz=4 (PLATFORM_PNL). 两个不同 reserved_id 即可区分。
	feeClearing, err := httpCreatePlatform(httpC, platformBase+1, 4)
	if err != nil {
		return fmt.Errorf("fee_clearing: %w", err)
	}
	feeRevenue, err := httpCreatePlatform(httpC, platformBase+2, 4)
	if err != nil {
		return fmt.Errorf("fee_revenue: %w", err)
	}
	withdrawPending, err := httpCreatePlatform(httpC, platformBase+3, 9)
	if err != nil {
		return fmt.Errorf("withdraw_pending: %w", err)
	}
	pool.Platform = PlatformAccounts{
		FeeClearing:     feeClearing,
		FeeRevenue:      feeRevenue,
		WithdrawPending: withdrawPending,
	}
	return nil
}

// httpCreatePlatform POST /admin/platform-accounts，返回 account_no。
// 端点幂等：同 (reserved_id, account_type→biz_type, currency) 重复调返原账户。
func httpCreatePlatform(httpC *http.Client, reservedID int64, accountType int) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"reserved_id":  reservedID,
		"account_type": accountType,
		"currency":     *flagCurrency,
	})
	req, err := http.NewRequest("POST", *flagAcctHTTP+"/admin/platform-accounts", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpC.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var acc struct {
		AccountNo string `json:"account_no"`
	}
	if err := json.Unmarshal(respBody, &acc); err != nil {
		return "", fmt.Errorf("parse response: %w (body=%s)", err, string(respBody))
	}
	if acc.AccountNo == "" {
		return "", fmt.Errorf("empty account_no in response: %s", string(respBody))
	}
	return acc.AccountNo, nil
}

// ─── 并发辅助 ─────────────────────────────────────────────────────────────

// parallelCreate 起 N 个 worker 跑 total 个任务；任一失败立即停。
func parallelCreate(total, workers int, fn func(i int) error) error {
	if workers < 1 {
		workers = 1
	}
	if workers > total {
		workers = total
	}
	idx := make(chan int, total)
	for i := 0; i < total; i++ {
		idx <- i
	}
	close(idx)

	var wg sync.WaitGroup
	var (
		mu      sync.Mutex
		firstErr error
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				if err := fn(i); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// writePool 把 pool 写成 pretty-printed JSON 到 path。
// 父目录不存在就先创。
func writePool(path string, pool *AccountPool) error {
	if dir := dirOf(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	body, err := json.MarshalIndent(pool, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return err
	}
	return nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return ""
}
