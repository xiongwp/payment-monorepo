// Package accounting wraps the grpc-go client for accounting-system.
//
// order-core only talks to accounting-system over gRPC:
//   - Hot path: HybridDoubleEntryBooking. main + counter (channel platform)
//     accountNos resolved via GetAccount(user_id, business_type); accounting
//     shards them so both legs land in the same physical shard. (user,bt,curr)
//     → accountNo cached in-process.
//   - Channel onboarding (register business_type + build fleet) is operator
//     work on accounting-admin-web; order-core does not call admin HTTP.
//   - Daily reconciliation (charges vs fleet balances) lives in the standalone
//     reconplatform system, not in order-core.
//
// Channel business_type ids come from CounterBusinessTypes — populated from
// static yaml at boot (accounting.channels[].business_type).
package accounting

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xiongwp/payment-util/kitexutil"

	accountingv1 "reconcile-system/packages/accounting-system/kitex_gen/accounting/v1"
	"reconcile-system/packages/accounting-system/kitex_gen/accounting/v1/accountingservice"

	"github.com/xiongwp/order-core/internal/domain"
	_ "github.com/xiongwp/order-core/internal/shadow"
	_ "github.com/xiongwp/order-core/internal/trace"
)

var (
	// ErrCounterChannelNotConfigured payment_method 未在 counter_business_types
	// 里登记（onboarding 漏了 / config 没列）。视作配置错误，worker 标 failed。
	ErrCounterChannelNotConfigured = errors.New("accounting: counter channel business_type not configured for payment_method")
)

// Config 初始化参数。
type Config struct {
	// Addr accounting-system gRPC 地址（直连 fallback），如 "accounting-system:50051"。
	// 仅在 RegistryEndpoints 空时使用；非空时走 etcd resolver 拨号。
	Addr string
	// RegistryEndpoints etcd 集群地址，非空时启用 etcd resolver。
	// 联栈模式下必须配置（容器删了 container_name 后跨 compose 项目无法 DNS 解析）。
	RegistryEndpoints []string
	// ServiceName etcd 注册名，默认 "accounting-service"。
	ServiceName string
	// Timeout 单次 RPC 超时；0 = 由调用方 ctx 决定。
	Timeout time.Duration
	// CounterBusinessTypes payment_method → 该渠道的 channel business_type i32。
	// 启动期由 cmd/server/main.go 从 yaml accounting.channels 读静态映射填入；
	// onboarding（注册 business_type + 建 fleet）由运维通过 accounting-admin-web
	// 完成，order-core 不再主动调 admin HTTP onboard。
	// 例：{"gcash": 101, "shopeepay": 102}
	CounterBusinessTypes map[string]int32
}

// Client 线程安全的 accounting 客户端 (Kitex).
type Client struct {
	cfg Config
	cli accountingservice.Client

	mapMu sync.RWMutex
	btMap map[string]int32 // payment_method → business_type i32（运行时可热更新）

	acctCacheMu sync.RWMutex
	acctCache   map[string]string // "{userId}:{businessType}:{currency}" → accountNo
}

// 默认 RPC 超时。P1-6 资损保护：未配置 yaml accounting.timeout 时强制兜底，
// 避免一次慢 booking（accounting-system 卡 30s+）把整个 order-core handler
// goroutine 池吃光，引起 cascade（payment-channel 已发出 charge 但 order-core
// 不知道结果，重试又触发幂等冲突）。
//
// 5s 是粗粒度估计：accounting-system DoubleEntryBooking 正常 P99 < 200ms；
// 5s 留 20× 安全余量，足够应对 metaDB / shard 偶发 GC 抖动。
const defaultRPCTimeout = 5 * time.Second

// New 构造。
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" && len(cfg.RegistryEndpoints) == 0 {
		return nil, errors.New("accounting: empty addr and empty registry endpoints")
	}
	service := cfg.ServiceName
	if service == "" {
		service = "accounting-service"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultRPCTimeout
	}
	// Kitex client — etcd resolver / trace+shadow middleware 由 kitexutil 内置.
	// 老 gRPC dial / mtls / serviceregistry.DialWithFallback 已删.
	cli := kitexutil.MustKitexClient(accountingservice.NewClient(service))
	bt := make(map[string]int32, len(cfg.CounterBusinessTypes))
	for k, v := range cfg.CounterBusinessTypes {
		bt[strings.ToLower(k)] = v
	}
	return &Client{
		cfg:       cfg,
		cli:       cli,
		btMap:     bt,
		acctCache: make(map[string]string),
	}, nil
}

// PrewarmFleetAccounts 在 onboarding 之后一次性把每个 channel 的 100 个 fleet
// 平台账户（user_id = 0..99，slot 对应 real_userID % 100）查一遍，把 accountNo
// 写入本地 acctCache。运行时 DoubleEntryBooking 的 counter 分支就不用再打
// GetAccount RPC（cache miss → 15~50ms RTT），首次记账也走热路径。
//
// 幂等：重复调用只是覆盖已有缓存条目，不影响正确性。
// 容错：个别账户查询失败不终止——返回首个错误供启动期 Warn 日志，该 slot 的 cache
// 留空，运行时再按需 lookupOrCreateAccount 补。
func (c *Client) PrewarmFleetAccounts(ctx context.Context, btMap map[string]int32, currency string) error {
	if len(btMap) == 0 {
		return nil
	}
	// Kitex client lazy dial (Kitex eager init by default — no waitReady step needed).
	// 老 grpc 时代的 waitReady 已删 (Kitex 自带 resolver + 启动期 endpoint 解析).
	// 复用 counterBT 去重：不同 channel 可能映射到同一个 business_type（理论上不应，
	// 但 config 重复就别重复拉 100 次）。
	seen := make(map[int32]struct{}, len(btMap))
	var firstErr error
	var firstErrMu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16) // 限制并发，保护 accounting-system 连接池
	for _, bt := range btMap {
		if bt <= 0 {
			continue
		}
		if _, dup := seen[bt]; dup {
			continue
		}
		seen[bt] = struct{}{}
		for uid := int64(0); uid < 100; uid++ {
			uid := uid
			bt := bt
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				key := fmt.Sprintf("%d:%d:%s", uid, bt, currency)
				c.acctCacheMu.RLock()
				_, hit := c.acctCache[key]
				c.acctCacheMu.RUnlock()
				if hit {
					return
				}
				resp, err := c.getAccount(ctx, uid, bt)
				if err != nil || resp == nil || resp.GetCode() != 0 || resp.GetAccount() == nil {
					if err != nil {
						firstErrMu.Lock()
						if firstErr == nil {
							firstErr = fmt.Errorf("prewarm user=%d bt=%d: %w", uid, bt, err)
						}
						firstErrMu.Unlock()
					}
					return
				}
				c.acctCacheMu.Lock()
				c.acctCache[key] = resp.GetAccount().GetAccountNo()
				c.acctCacheMu.Unlock()
			}()
		}
	}
	wg.Wait()
	return firstErr
}

// SetCounterBusinessTypes 替换 payment_method → business_type i32 映射。
// 用于启动期 onboarding 完成后刷新（覆盖 static config 给的初始值）。
func (c *Client) SetCounterBusinessTypes(m map[string]int32) {
	c.mapMu.Lock()
	defer c.mapMu.Unlock()
	c.btMap = make(map[string]int32, len(m))
	for k, v := range m {
		c.btMap[strings.ToLower(k)] = v
	}
}

// Close no-op — Kitex client 没有显式 Close (resolver 由 Kitex runtime 管).
// 保留方法签名让 fx.OnStop 平滑.
func (c *Client) Close() error { return nil }

// DoubleEntryBooking 把 outbox 行翻成一笔双分录并提交给 accounting-system。
//
// 主账户 = GetAccount(user_id, USER_BALANCE / MERCHANT_PENDING_SETTLE)
// 对端账户 = GetAccount(user_id, channel_business_type)  ← 同一 user_id 让
//   accounting-system 把对端 platform account 路由到与主账户同一物理分片，
//   双分录天然落单库事务。
func (c *Client) DoubleEntryBooking(ctx context.Context, row *domain.AccountingOutbox) error {
	if row == nil {
		return errors.New("accounting: nil outbox row")
	}

	// btMap 的 key 来自 config.channels[].name（通常写成小写 "gcash"），
	// 而 PI.PaymentMethod 是调用方传来的 "GCASH"，所以大小写敏感查会漏。
	pmKey := strings.ToLower(row.PaymentMethod)
	c.mapMu.RLock()
	counterBT, ok := c.btMap[pmKey]
	c.mapMu.RUnlock()
	if !ok || counterBT <= 0 {
		return fmt.Errorf("%w: %s", ErrCounterChannelNotConfigured, row.PaymentMethod)
	}

	mainBT, bizType, err := mapOutboxToEnums(row)
	if err != nil {
		return err
	}

	userID, err := parseOwnerID(row.OwnerID)
	if err != nil {
		return fmt.Errorf("parse owner_id: %w", err)
	}

	mainAccount, err := c.getAccountNo(ctx, userID, int32(mainBT), row.Currency)
	if err != nil {
		return fmt.Errorf("resolve main accountNo: %w", err)
	}
	// 渠道 fleet 平台账户用 user_id % 100 这个 slot 去查（fleet 一共 100 个，
	// user_id 0..99）；用 real userID 直接调 GetAccount 一定 404。
	counterUserID := userID % 100
	counterAccount, err := c.getAccountNo(ctx, counterUserID, counterBT, row.Currency)
	if err != nil {
		return fmt.Errorf("resolve counter accountNo: %w", err)
	}

	// row.Amount 是 PI 侧 minor units（ISO 最小货币单位，PHP cents / JPY yen）。
	// 用 Money 字段送给 accounting，服务端按币种 precision 换成内部存储值，
	// 杜绝「两端单位解读不一致导致金额虚大 10^N 倍」这条老坑。
	amount := row.Amount
	zeroMoney := &accountingv1.Money{MinorUnits: 0, Currency: row.Currency}
	fullMoney := &accountingv1.Money{MinorUnits: amount, Currency: row.Currency}

	var mainDebit, mainCredit, counterDebit, counterCredit *accountingv1.Money
	switch row.EventType {
	case domain.AccountingEventChargeSucceeded:
		// 主账户贷方，对端借方（资金从渠道应收流入 user/merchant 余额）
		mainDebit, mainCredit = zeroMoney, fullMoney
		counterDebit, counterCredit = fullMoney, zeroMoney
	case domain.AccountingEventRefundSucceeded:
		// 反向
		mainDebit, mainCredit = fullMoney, zeroMoney
		counterDebit, counterCredit = zeroMoney, fullMoney
	default:
		return fmt.Errorf("accounting: unhandled event_type %s", row.EventType)
	}

	businessNo := row.PaymentIntentID
	if row.ChargeID != "" {
		businessNo = row.PaymentIntentID + ":" + row.ChargeID
	}
	if row.RefundID != "" {
		businessNo = row.PaymentIntentID + ":" + row.RefundID
	}

	req := &accountingv1.HybridDoubleEntryBookingRequest{
		RequestId:    row.RequestID,
		BusinessNo:   businessNo,
		BusinessType: bizType,
		Currency:     row.Currency,
		Entries: []*accountingv1.AccountingEntry{
			{AccountNo: mainAccount, DebitMoney: mainDebit, CreditMoney: mainCredit},
			{AccountNo: counterAccount, DebitMoney: counterDebit, CreditMoney: counterCredit},
		},
	}

	callCtx := ctx
	if c.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	}
	resp, err := c.cli.HybridDoubleEntryBooking(callCtx, req)
	if err != nil {
		return err
	}
	if resp.GetCode() != 0 {
		return fmt.Errorf("accounting: HybridDoubleEntryBooking code=%d msg=%s", resp.GetCode(), resp.GetMessage())
	}
	return nil
}

// mapOutboxToEnums 静态路由：
//   owner=user                → AccountBusinessType_USER_BALANCE              + DEPOSIT / REFUND
//   owner=merchant            → AccountBusinessType_MERCHANT_PENDING_SETTLE   + PAYMENT / REFUND
func mapOutboxToEnums(row *domain.AccountingOutbox) (accountingv1.AccountBusinessType, accountingv1.BusinessType, error) {
	switch row.OwnerType {
	case domain.AccountingOwnerUser:
		bt := accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_USER_BALANCE
		biz := accountingv1.BusinessType_BUSINESS_TYPE_DEPOSIT
		if row.EventType == domain.AccountingEventRefundSucceeded {
			biz = accountingv1.BusinessType_BUSINESS_TYPE_REFUND
		}
		return bt, biz, nil
	case domain.AccountingOwnerMerchant:
		bt := accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_MERCHANT_PENDING_SETTLE
		biz := accountingv1.BusinessType_BUSINESS_TYPE_PAYMENT
		if row.EventType == domain.AccountingEventRefundSucceeded {
			biz = accountingv1.BusinessType_BUSINESS_TYPE_REFUND
		}
		return bt, biz, nil
	}
	return 0, 0, fmt.Errorf("accounting: unknown owner_type %s", row.OwnerType)
}

func (c *Client) getAccountNo(ctx context.Context, userID int64, bt int32, currency string) (string, error) {
	key := fmt.Sprintf("%d:%d:%s", userID, bt, currency)
	c.acctCacheMu.RLock()
	if v, ok := c.acctCache[key]; ok {
		c.acctCacheMu.RUnlock()
		return v, nil
	}
	c.acctCacheMu.RUnlock()

	acctNo, err := c.lookupOrCreateAccount(ctx, userID, bt, currency)
	if err != nil {
		return "", err
	}

	c.acctCacheMu.Lock()
	c.acctCache[key] = acctNo
	c.acctCacheMu.Unlock()
	return acctNo, nil
}

// lookupOrCreateAccount 先 GetAccount；若账户不存在（code=404）且是用户/商户主账户，
// 触发一次 CreateAccount 再 Get。平台渠道 fleet 账户必须预建，这里不尝试创建。
func (c *Client) lookupOrCreateAccount(ctx context.Context, userID int64, bt int32, currency string) (string, error) {
	resp, err := c.getAccount(ctx, userID, bt)
	if err == nil && resp.GetCode() == 0 && resp.GetAccount() != nil {
		return resp.GetAccount().GetAccountNo(), nil
	}
	// 只对 "not found" + 主账户类型兜底创建。其它错误直接抛。
	if err != nil {
		return "", err
	}
	if resp.GetCode() != 404 || !isMainBusinessType(accountingv1.AccountBusinessType(bt)) {
		return "", fmt.Errorf("accounting: GetAccount code=%d msg=%s", resp.GetCode(), resp.GetMessage())
	}
	if err := c.createMainAccount(ctx, userID, bt, currency); err != nil {
		return "", fmt.Errorf("accounting: create user=%d bt=%d: %w", userID, bt, err)
	}
	resp2, err := c.getAccount(ctx, userID, bt)
	if err != nil {
		return "", err
	}
	if resp2.GetCode() != 0 || resp2.GetAccount() == nil {
		return "", fmt.Errorf("accounting: GetAccount-after-Create code=%d msg=%s", resp2.GetCode(), resp2.GetMessage())
	}
	return resp2.GetAccount().GetAccountNo(), nil
}

// waitReady 已删 (Kitex 切换后).

func (c *Client) getAccount(ctx context.Context, userID int64, bt int32) (*accountingv1.GetAccountResponse, error) {
	callCtx := ctx
	if c.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	}
	return c.cli.GetAccount(callCtx, &accountingv1.GetAccountRequest{
		Identifier: &accountingv1.GetAccountRequest_UserIdAndBusinessType{
			UserIdAndBusinessType: &accountingv1.UserIdAndBusinessTypeQuery{
				UserId:              userID,
				AccountBusinessType: accountingv1.AccountBusinessType(bt),
			},
		},
	})
}

// createMainAccount 对 USER_BALANCE / MERCHANT_PENDING_SETTLE 调 CreateAccount。
// 幂等：accounting-system 对 (user_id, business_type) 有唯一约束，重复调
// 拿错误但 GetAccount 能查到，调用方已兜底二次 Get。
func (c *Client) createMainAccount(ctx context.Context, userID int64, bt int32, currency string) error {
	callCtx := ctx
	if c.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	}
	acctType := accountingv1.AccountType_ACCOUNT_TYPE_USER
	if accountingv1.AccountBusinessType(bt) == accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_MERCHANT_PENDING_SETTLE {
		acctType = accountingv1.AccountType_ACCOUNT_TYPE_MERCHANT_PENDING_SETTLE
	}
	resp, err := c.cli.CreateAccount(callCtx, &accountingv1.CreateAccountRequest{
		UserId:              userID,
		AccountType:         acctType,
		Category:            accountingv1.AccountCategory_ACCOUNT_CATEGORY_LIABILITY,
		Currency:            currency,
		AccountBusinessType: accountingv1.AccountBusinessType(bt),
	})
	if err != nil {
		return err
	}
	if resp.GetCode() != 0 {
		return fmt.Errorf("CreateAccount code=%d msg=%s", resp.GetCode(), resp.GetMessage())
	}
	return nil
}

func isMainBusinessType(bt accountingv1.AccountBusinessType) bool {
	switch bt {
	case accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_USER_BALANCE,
		accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_MERCHANT_PENDING_SETTLE:
		return true
	}
	return false
}

// parseOwnerID 把 "cus_42" / "mch_1" / "100000042" 之类的字符串解析成 i64。
func parseOwnerID(s string) (int64, error) {
	if id, err := strconv.ParseInt(s, 10, 64); err == nil {
		return id, nil
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '_' {
			rest := s[i+1:]
			if id, err := strconv.ParseInt(rest, 10, 64); err == nil {
				return id, nil
			}
			break
		}
	}
	return 0, fmt.Errorf("owner_id %q not numeric", s)
}
