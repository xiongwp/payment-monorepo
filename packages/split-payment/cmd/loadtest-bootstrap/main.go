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
	"github.com/cloudwego/kitex/transport"

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
// accounting 的 owner_id 严格分段（accounting_service.go:2464）：
//   [1, 10_000]                       系统内部账户 (平台 / 中转 / 手续费), HTTP /admin/platform-accounts
//   [100_000_000, 899_999_999]        普通用户账户 (base 1e8),               gRPC CreateAccount
//   [900_000_000, ...]                商户账户 (base 9e8),                   gRPC CreateAccount
//
// ⚡ 优化 #3: shard 分散 —— accounting Router 的规则是 dbIdx = (id % 1000) / 100。
// 旧版用连续 id（1..100, 100_000_001..100_000_100），结果 mod 1000 全落在 [0, 99]
// → dbIdx=0 → 100% 数据挤在 shard-0。
//
// 渠道账户 owner_id 受 accounting 的 platform reserved range [1, 10_000] 约束。
//   第 i 个 channel 的 owner_id = channelOwnerStart + sub_offset + i*channelOwnerStep
//
// 设计原则：
//   - step 跟 10 (shard 数) 互质 → 散布到 10 个 shard 均匀（不只 mod-10 一种 routing
//     可能性，但互质始终安全）
//   - 4 个 sub_offset {0,10,20,30} 跟 step 模数都不同 → 4 段子表互不冲突
//   - 最大 channel 数 N 受 reserved range 限制：
//        channelOwnerStart + 30 + (N-1)*step < 10_000
//        N=1000, step=9 → max = 1+30+999*9 = 9002 ✓
//        N=100,  step=100 → max = 9931 ✓（老配置，留给小规模兼容）
//   - 想要更大规模？要扩 accounting 的 reserved range（>10_000）或换 step。
const (
	channelRecvOffset     = 0
	channelSuspenseOffset = 10
	channelFeeOffset      = 20
	channelPayableOffset  = 30
	// step=9 跟 shard 数 10 互质 → 散布均匀；1000 channel × 9 < 9000 < 10_000 reserved。
	// 历史值 100 只够 100 channel（卡 reserved range）；中规模 1000 channel 必须切到 9。
	channelOwnerStep  = 9
	channelOwnerStart = 1

	// 平台账户放到 reserved range 顶部附近，离 channel 段（1..9002）够远，不会撞
	platformFeeClearingOwnerID     = 9101
	platformFeeRevenueOwnerID      = 9201
	platformWithdrawPendingOwnerID = 9301

	userOwnerBase     = 100_000_000 // user_i = 100_000_000 + i*100
	merchantOwnerBase = 900_000_000 // merchant_i = 900_000_000 + i*100
	bizOwnerStep      = 100         // user/merchant 没有 reserved range 限制，保留 step=100
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
		// 必填：Kitex 默认 netpoll transport 会把 "host:port" 字符串当成 unix socket
		// 路径，dial 时报 "dial unix accounting-service:50051: no such file or directory"。
		// 强制走 gRPC over HTTP/2 over TCP（跟 internal/clients/accounting_grpc.go 同款）。
		client.WithTransportProtocol(transport.GRPC),
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
// owner_id = userOwnerBase + (i+1)*100 → 相邻 id 间隔 100，使 accounting Router
// 的 dbIdx = (id % 1000)/100 在 i=0..9 循环时均匀落到 db=0..9，100 用户 = 每 db 10 个。
func createUsers(gClient acctsvc.Client, pool *AccountPool) error {
	return parallelCreate(*flagNumUsers, *flagWorkers, func(i int) error {
		userID := int64(userOwnerBase + (i+1)*bizOwnerStep)
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
		// merchant_id = merchantOwnerBase + (i+1)*100 → 散到 10 shard
		merchantID := int64(merchantOwnerBase + (i+1)*bizOwnerStep)
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
		merchantID := int64(merchantOwnerBase + (i+1)*bizOwnerStep)
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
		offset  int // sub_offset (0/10/20/30) — 4 类同一渠道避免 owner_id 冲突
		accType int // AccountType 枚举值
		setter  func(*ChannelAccounts, string)
	}{
		{"recv", channelRecvOffset, 5, func(c *ChannelAccounts, no string) { c.Receivable = no }},
		{"suspense", channelSuspenseOffset, 9, func(c *ChannelAccounts, no string) { c.Suspense = no }},
		{"fee", channelFeeOffset, 7, func(c *ChannelAccounts, no string) { c.Fee = no }},
		{"payable", channelPayableOffset, 6, func(c *ChannelAccounts, no string) { c.Payable = no }},
	}
	for _, s := range subs {
		s := s // capture
		first := channelOwnerStart + s.offset                                    // i=0 时的 owner_id
		last := channelOwnerStart + s.offset + (*flagNumChannels-1)*channelOwnerStep // i=N-1 时的 owner_id
		fmt.Printf("    channel-%s: type=%d  owner_id step=%d, %d..%d (散到 10 shards)\n",
			s.name, s.accType, channelOwnerStep, first, last)
		if err := parallelCreate(*flagNumChannels, *flagWorkers, func(i int) error {
			// owner_id = start + offset + i*step → 相邻 channel 间隔 100，循环遍历 db=0..9
			reservedID := int64(channelOwnerStart + s.offset + i*channelOwnerStep)
			acctNo, err := httpCreatePlatform(httpC, reservedID, s.accType)
			if err != nil {
				return fmt.Errorf("channel-%s ch=%d (owner_id=%d): %w", s.name, i, reservedID, err)
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
// owner_id 选 9101 / 9201 / 9301 — 不同 mod-1000 → 落不同 shard，热点平台账户
// 分散到 3 个 shard 而不是挤在 1 个。
func createPlatform(httpC *http.Client, pool *AccountPool) error {
	feeClearing, err := httpCreatePlatform(httpC, platformFeeClearingOwnerID, 4)
	if err != nil {
		return fmt.Errorf("fee_clearing: %w", err)
	}
	feeRevenue, err := httpCreatePlatform(httpC, platformFeeRevenueOwnerID, 4)
	if err != nil {
		return fmt.Errorf("fee_revenue: %w", err)
	}
	withdrawPending, err := httpCreatePlatform(httpC, platformWithdrawPendingOwnerID, 9)
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
