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
// 当前最小实现: request_id 透传 (走 accounting 侧幂等), business_no 用 ChargeID/
// RefundID/PIID 顺位填. entries 暂留空 — 由 accounting-system 侧按 business_no +
// business_type 查规则补齐 (Stripe-like 资金链路在 split-payment moneyflow 引擎
// 里维护规则, accounting 端只执行 rule).
//
// 真要在 order-core 这边做完整 entries 映射, 需要按 EventType 拆 charge.success /
// refund.success / dispute.opened / ... 各 case 推算借贷, 这部分代码在原始
// internal/accounting/mapper.go (~350 行). 接下来 RESTORE-2 后续 commit 补.
func (c *Client) DoubleEntryBooking(ctx context.Context, ob *domain.AccountingOutbox) error {
	if ob == nil {
		return errors.New("accounting outbox required")
	}
	businessNo := firstNonEmpty(ob.ChargeID, ob.RefundID, ob.PaymentIntentID)
	req := &accv1.DoubleEntryBookingRequest{
		BusinessNo: businessNo,
		RequestId:  ob.RequestID,
		// BusinessType / Entries / Currency / Mode 由 mapper.go 补齐, 这里暂留默认.
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

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
