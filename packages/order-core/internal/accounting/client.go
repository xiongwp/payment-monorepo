// Package accounting — Kitex client to accounting-system.
//
// order-core 的 outbox worker 在 charge / refund / dispute 等事件落 DB 后, 异步把
// AccountingOutbox 翻译成 DoubleEntryBookingRequest 推给 accounting-system. 失败由
// outbox 重试.
//
// TECH-DEBT-5 实装: 这里负责把 (PaymentMethod / OwnerType / OwnerID / Amount /
// Currency) 解析到两个具体 account_no (借/贷), 再调 DoubleEntryBooking 原子落账.
// account_no 解析走 ListAccountsByUserAndBusinessType, 结果 5min cache 减抖.
package accounting

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/kitex/client"
	accv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
	accountingservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingservice"

	"github.com/xiongwp/order-core/internal/domain"
)

var (
	// ErrCounterChannelNotConfigured payment_method 未配置.
	ErrCounterChannelNotConfigured = errors.New("accounting: counter channel business_type not configured for payment_method")
	// ErrNotConfigured 没配 endpoint 时构造返此 sentinel.
	ErrNotConfigured = errors.New("accounting: endpoint required")
	// ErrAccountNotFound (user_id, business_type, currency) 三元组在 accounting 侧没建账户.
	// outbox worker 拿到这个错应该写 LastError, 不要无限重试 (因为账户没建是
	// fleet 预热 / 商户开通的事, 不是临时问题).
	ErrAccountNotFound = errors.New("accounting: account not found for tuple (user_id, business_type, currency)")
)

// Config 初始化参数. ServiceName 给 Kitex client name 用 (etcd resolver 时是 service key).
type Config struct {
	Addr                 string
	RegistryEndpoints    []string
	ServiceName          string
	Timeout              time.Duration
	CounterBusinessTypes map[string]int32
	// PlatformUserID 平台账户的 owner user_id. 默认 0; 业务方可以改成自有平台 user.
	PlatformUserID int64
}

// accCacheEntry 账户号解析缓存条目. 5min TTL, 按 (user_id, BT, currency) 三元组.
type accCacheEntry struct {
	accountNo string
	exp       time.Time
}

// Client 线程安全 Kitex accountingservice client wrapper.
type Client struct {
	cli accountingservice.Client
	cfg Config

	mapMu sync.RWMutex
	btMap map[string]int32

	// account_no resolution cache. 5min TTL.
	accCacheMu sync.RWMutex
	accCache   map[string]accCacheEntry
}

const (
	defaultRPCTimeout = 5 * time.Second
	accCacheTTL       = 5 * time.Minute
)

// New 构造 Kitex client to accounting-system.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" && len(cfg.RegistryEndpoints) == 0 {
		return nil, ErrNotConfigured
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultRPCTimeout
	}
	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = "accounting-system"
	}
	opts := []client.Option{client.WithRPCTimeout(cfg.Timeout)}
	if cfg.Addr != "" {
		opts = append(opts, client.WithHostPorts(cfg.Addr))
	}
	cli, err := accountingservice.NewClient(serviceName, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial accounting-system (kitex): %w", err)
	}
	bt := make(map[string]int32, len(cfg.CounterBusinessTypes))
	for k, v := range cfg.CounterBusinessTypes {
		bt[strings.ToLower(k)] = v
	}
	return &Client{
		cli:      cli,
		cfg:      cfg,
		btMap:    bt,
		accCache: make(map[string]accCacheEntry),
	}, nil
}

// PrewarmFleetAccounts 启动时把 payment-channel 维度的 platform 渠道账户预创建,
// 防止首笔交易因为 buffer 账户不存在而失败.
//
// channelBT: payment_method (lowercase) → buffer business_type (例: stripe→7001).
// currency: PHP / USD / ... 全大写.
//
// 当前只热缓存 BT 映射; 真预创建账户由 ops 走 /admin/platform-accounts/fleet 完成.
func (c *Client) PrewarmFleetAccounts(_ context.Context, channelBT map[string]int32, _ string) error {
	c.SetCounterBusinessTypes(channelBT)
	return nil
}

// SetCounterBusinessTypes 热更新 payment_method → business_type 映射.
func (c *Client) SetCounterBusinessTypes(m map[string]int32) {
	c.mapMu.Lock()
	defer c.mapMu.Unlock()
	c.btMap = make(map[string]int32, len(m))
	for k, v := range m {
		c.btMap[strings.ToLower(k)] = v
	}
}

// counterBTFor 查 payment_method → 渠道 buffer business_type 映射.
func (c *Client) counterBTFor(paymentMethod string) (int32, bool) {
	c.mapMu.RLock()
	defer c.mapMu.RUnlock()
	bt, ok := c.btMap[strings.ToLower(paymentMethod)]
	return bt, ok
}

// Close — Kitex 自带 connection pool, no-op.
func (c *Client) Close() error { return nil }

// DoubleEntryBooking 把 AccountingOutbox 翻译成 DoubleEntryBookingRequest 调
// accounting-system.
//
// 实装流程 (TECH-DEBT-5):
//  1. 拿 payment_method → 渠道 buffer BT (c.btMap), 失败 → ErrCounterChannelNotConfigured
//  2. 推 owner_type → 对端 BT (merchant → MERCHANT_PENDING_SETTLE, user → USER_BALANCE)
//  3. 分别 ListAccountsByUserAndBusinessType 解 channel + owner 两个 account_no
//  4. 按 EventType 决定借贷方向, 构造 2 行 AccountingEntry
//  5. 调 DoubleEntryBooking (accounting 侧已用 RequestId 做幂等)
//
// 借贷方向:
//
//	charge.succeeded:  DEBIT  channel-buffer (asset+),  CREDIT merchant-pending (liability+)
//	refund.succeeded:  DEBIT  merchant-pending (liability-), CREDIT channel-buffer (asset-)
//
// 任何一边账户没建 → ErrAccountNotFound; outbox worker 应当不重试此类错 (走人工干预).
func (c *Client) DoubleEntryBooking(ctx context.Context, ob *domain.AccountingOutbox) error {
	if ob == nil {
		return errors.New("accounting outbox required")
	}
	if ob.Amount <= 0 {
		return fmt.Errorf("invalid amount %d", ob.Amount)
	}
	if ob.Currency == "" {
		return errors.New("currency required")
	}

	// 1. 渠道 buffer BT
	counterBT, ok := c.counterBTFor(ob.PaymentMethod)
	if !ok {
		return fmt.Errorf("%w: payment_method=%s", ErrCounterChannelNotConfigured, ob.PaymentMethod)
	}

	// 2. owner 侧 BT (merchant → 3, user → 1)
	ownerBT, err := ownerBTFor(ob.OwnerType)
	if err != nil {
		return err
	}

	// 3. 解 2 个 account_no
	channelNo, err := c.resolveAccountNo(ctx, c.cfg.PlatformUserID, counterBT, ob.Currency)
	if err != nil {
		return fmt.Errorf("resolve channel account (user=%d bt=%d): %w", c.cfg.PlatformUserID, counterBT, err)
	}
	ownerID, err := parseInt64(ob.OwnerID)
	if err != nil {
		return fmt.Errorf("parse owner_id %q: %w", ob.OwnerID, err)
	}
	ownerNo, err := c.resolveAccountNo(ctx, ownerID, int32(ownerBT), ob.Currency)
	if err != nil {
		return fmt.Errorf("resolve owner account (user=%d bt=%d): %w", ownerID, ownerBT, err)
	}

	// 4. entries
	debitNo, creditNo, err := entriesDirection(ob.EventType, channelNo, ownerNo)
	if err != nil {
		return err
	}
	// 用 DebitMoney/CreditMoney 走 minor_units 路径, 避开 string-decimal major-units 路径
	// 在不同 currency exponent 下的转换噪声. server resolveEntryAmounts 对每行都需
	// 拿到借 / 贷两个 Money (任一缺失会 fallback 到空字符串 string-decimal → 报错),
	// 所以每行都把另一侧塞 0 minor_units 显式占位.
	desc := describeOutbox(ob)
	zero := &accv1.Money{Currency: ob.Currency, MinorUnits: 0}
	mon := &accv1.Money{Currency: ob.Currency, MinorUnits: ob.Amount}
	entries := []*accv1.AccountingEntry{
		{
			AccountNo:   debitNo,
			DebitMoney:  mon,
			CreditMoney: zero,
			Description: desc,
		},
		{
			AccountNo:   creditNo,
			DebitMoney:  zero,
			CreditMoney: mon,
			Description: desc,
		},
	}

	// 5. 发请求
	businessNo := firstNonEmpty(ob.ChargeID, ob.RefundID, ob.PaymentIntentID)
	req := &accv1.DoubleEntryBookingRequest{
		BusinessNo:   businessNo,
		RequestId:    ob.RequestID,
		BusinessType: eventTypeToBusinessType(ob.EventType),
		Entries:      entries,
		Currency:     ob.Currency,
		Description:  describeOutbox(ob),
	}
	resp, err := c.cli.DoubleEntryBooking(ctx, req)
	if err != nil {
		return err
	}
	if resp.GetCode() != 0 {
		return fmt.Errorf("accounting: code=%d msg=%s", resp.GetCode(), resp.GetMessage())
	}
	return nil
}

// resolveAccountNo 用 (user_id, BT, currency) 查 account_no, 5min cache.
// resp.Accounts 空 → ErrAccountNotFound (账户没建, 人工排查).
func (c *Client) resolveAccountNo(ctx context.Context, userID int64, bt int32, currency string) (string, error) {
	cur := strings.ToUpper(currency)
	key := fmt.Sprintf("%d:%d:%s", userID, bt, cur)

	c.accCacheMu.RLock()
	e, ok := c.accCache[key]
	c.accCacheMu.RUnlock()
	if ok && time.Now().Before(e.exp) {
		return e.accountNo, nil
	}

	resp, err := c.cli.ListAccountsByUserAndBusinessType(ctx, &accv1.ListAccountsByUserAndBusinessTypeRequest{
		UserId:              userID,
		AccountBusinessType: accv1.AccountBusinessType(bt),
		Currency:            cur,
	})
	if err != nil {
		return "", err
	}
	if resp.GetCode() != 0 {
		return "", fmt.Errorf("ListAccountsByUserAndBusinessType: code=%d msg=%s", resp.GetCode(), resp.GetMessage())
	}
	if len(resp.GetAccounts()) == 0 {
		return "", fmt.Errorf("%w: user=%d bt=%d currency=%s", ErrAccountNotFound, userID, bt, cur)
	}
	no := resp.GetAccounts()[0].GetAccountNo()

	c.accCacheMu.Lock()
	c.accCache[key] = accCacheEntry{accountNo: no, exp: time.Now().Add(accCacheTTL)}
	c.accCacheMu.Unlock()
	return no, nil
}

// InvalidateAccountCache 强制刷新 account_no 缓存 (admin 操作后调).
func (c *Client) InvalidateAccountCache() {
	c.accCacheMu.Lock()
	defer c.accCacheMu.Unlock()
	c.accCache = make(map[string]accCacheEntry)
}

// ownerBTFor owner_type → AccountBusinessType.
//   - merchant → MERCHANT_PENDING_SETTLE (3): charge 收款先入待结算; payout 时再 → 余额
//   - user     → USER_BALANCE (1): 用户钱包余额
func ownerBTFor(ot domain.AccountingOwnerType) (accv1.AccountBusinessType, error) {
	switch ot {
	case domain.AccountingOwnerMerchant:
		return accv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_MERCHANT_PENDING_SETTLE, nil
	case domain.AccountingOwnerUser:
		return accv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_USER_BALANCE, nil
	default:
		return 0, fmt.Errorf("unsupported owner_type %q", ot)
	}
}

// entriesDirection 按 EventType 决定借贷方向, 返 (debitAccountNo, creditAccountNo).
//
// charge.succeeded:
//
//	Money flow:    channel → platform → merchant
//	Bookkeeping:   DEBIT channel-buffer (asset+ "应收渠道款")
//	               CREDIT owner-pending (liability+ "欠商户/用户")
//
// refund.succeeded:
//
//	Money flow:    merchant → platform → channel (退给买家)
//	Bookkeeping:   DEBIT owner-pending (liability-)
//	               CREDIT channel-buffer (asset-)
func entriesDirection(et domain.AccountingEventType, channelNo, ownerNo string) (debit, credit string, err error) {
	switch et {
	case domain.AccountingEventChargeSucceeded:
		return channelNo, ownerNo, nil
	case domain.AccountingEventRefundSucceeded:
		return ownerNo, channelNo, nil
	default:
		return "", "", fmt.Errorf("unsupported event_type %q", et)
	}
}

// eventTypeToBusinessType 把 outbox EventType 映射到 accounting BusinessType.
// 未识别的 EventType 落 PAYMENT (charge 默认), 保守不阻断.
func eventTypeToBusinessType(et domain.AccountingEventType) accv1.BusinessType {
	switch et {
	case domain.AccountingEventChargeSucceeded:
		return accv1.BusinessType_BUSINESS_TYPE_PAYMENT
	case domain.AccountingEventRefundSucceeded:
		return accv1.BusinessType_BUSINESS_TYPE_REFUND
	default:
		return accv1.BusinessType_BUSINESS_TYPE_PAYMENT
	}
}

// describeOutbox 拼一个对账可读的描述: "<event>:<pi>:<charge|refund>".
// accounting 侧 transaction.description 落库, 排错时不用反查 outbox.
func describeOutbox(ob *domain.AccountingOutbox) string {
	tail := firstNonEmpty(ob.ChargeID, ob.RefundID, ob.PaymentIntentID)
	return fmt.Sprintf("%s:%s:%s", ob.EventType, ob.PaymentIntentID, tail)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// parseInt64 把 owner_id 字符串解析成 int64. 业务层 ownerID 是 varchar(64) 给字母
// 数字 ID (e.g. "mch_xxx") 留余地; 但目前 accounting 侧 user_id 是 int64, 所以
// outbox 写入处必须用纯数字 ID; 这里 strconv 严格解析, 拒接 trailing garbage.
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}
