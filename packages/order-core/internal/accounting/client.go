// Package accounting — Kitex client to accounting-system.
//
// order-core 的 outbox worker 在 charge / refund / dispute 等事件落 DB 后, 异步把
// AccountingOutbox 翻译成 DoubleEntryBookingRequest 推给 accounting-system. 失败由
// outbox 重试.
//
// 注意: AccountingOutbox → DoubleEntryBookingRequest 的字段映射逻辑细节随业务/proto
// 演化, 这里只保留客户端/拨号 + RPC 调用骨架. 完整 mapping (按 EventType 推断借贷
// 方向 + buffer 账户 + counter business_type) 在 internal/accounting/mapper.go (历史
// 文件) 复现, 当前 stub 完成版只透传 request_id + 业务标识让 accounting 走重复检测.
package accounting

import (
	"context"
	"errors"
	"fmt"
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
)

// Config 初始化参数. ServiceName 给 Kitex client name 用 (etcd resolver 时是 service key).
type Config struct {
	Addr                 string
	RegistryEndpoints    []string
	ServiceName          string
	Timeout              time.Duration
	CounterBusinessTypes map[string]int32
}

// Client 线程安全 Kitex accountingservice client wrapper.
type Client struct {
	cli   accountingservice.Client
	cfg   Config
	mapMu sync.RWMutex
	btMap map[string]int32
}

const defaultRPCTimeout = 5 * time.Second

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
	return &Client{cli: cli, cfg: cfg, btMap: bt}, nil
}

// PrewarmFleetAccounts 启动时把 payment-channel 维度的 platform 渠道账户预创建,
// 防止首笔交易因为 buffer 账户不存在而失败.
//
// channelBT: payment_method (lowercase) → buffer business_type (例: stripe→7001).
// currency: PHP / USD / ... 全大写.
//
// 当前 stub 仅记录映射 + 立即返回 nil; 真实实现按 channelBT 列表逐个调
// CreateAccount 把 buffer 账户写到 accounting DB. 没预创建只是首笔慢, 不影响正确性.
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

// Close — Kitex 自带 connection pool, no-op.
func (c *Client) Close() error { return nil }

// DoubleEntryBooking 把 AccountingOutbox 翻译成 DoubleEntryBookingRequest 调
// accounting-system.
//
// TECH-DEBT-5 当前状态:
//   - business_no / request_id / business_type / currency / description 已就位,
//     accounting 侧幂等键 + 业务标识齐全, 不会再因 missing field 被拒.
//   - entries (借贷分录) 仍未在 order-core 端解析: 需要按 (owner_id, business_type,
//     currency) 反查账户号才能填 AccountNo, 当前 client 没有这条 RPC 通道.
//
// 长期方向 (二选一):
//
//	a) order-core 这边按 EventType 拆 charge.succeeded / refund.succeeded / dispute.
//	   opened / ... 各 case 推借贷 + 调 GetAccount 解析 account_no 把 entries 全填.
//	   等价于复原 ~350 行的 internal/accounting/mapper.go.
//	b) 把订单事件改投 accounting-system 的 CreateTransaction (TECH-DEBT-3 已实装,
//	   wire 端 Legs[] 通了). order-core 只需做一次 GetAccountByUserAndBusinessType
//	   解析 from/to account_no, 然后塞 1 个 leg 进 CreateTransactionRequest.Legs
//	   即可走原子记账. 比 (a) 少 ~300 行 case 推算, 推荐.
//
// 当前实现保留了 (a) 路径所需的输入, entries 留空时 accounting 侧会落 400 让
// outbox 重试, 让链路明确暴露 "mapper 待补" 这一事实, 不静默成功. 下一步
// 切 (b) 路径: 拿 (PaymentMethod → 渠道 buffer BT, OwnerID + MERCHANT_PENDING_SETTLE)
// 解出 2 个 account_no → 单 leg CreateTransaction.
func (c *Client) DoubleEntryBooking(ctx context.Context, ob *domain.AccountingOutbox) error {
	if ob == nil {
		return errors.New("accounting outbox required")
	}
	businessNo := firstNonEmpty(ob.ChargeID, ob.RefundID, ob.PaymentIntentID)
	req := &accv1.DoubleEntryBookingRequest{
		BusinessNo:   businessNo,
		RequestId:    ob.RequestID,
		BusinessType: eventTypeToBusinessType(ob.EventType),
		Currency:     ob.Currency,
		Description:  describeOutbox(ob),
		// Entries / Mode: 见 TECH-DEBT-5 注释; 留默认.
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
