// Package accounting — 临时 STUB.
//
// 原版通过 Kitex 调 accounting-system. cross-service kitex_gen 还没在
// docker build 流程里 wire 进去 (需要 additional_contexts + replace).
// 暂改 stub: DoubleEntryBooking 永远返 ErrNotWired, 上游 outbox worker
// 会按重试策略持续重试 (符合 outbox 语义).
//
// 等 sibling sourcing 接通后恢复原版.
package accounting

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/xiongwp/order-core/internal/domain"
)

var (
	// ErrCounterChannelNotConfigured payment_method 未配置.
	ErrCounterChannelNotConfigured = errors.New("accounting: counter channel business_type not configured for payment_method")
	// ErrNotWired stub 模式标记 — caller 走 outbox retry 路径.
	ErrNotWired = errors.New("accounting STUB: accounting-system kitex_gen not wired in build")
)

// Config 初始化参数 (保持原 signature 让 main.go 编过).
type Config struct {
	Addr                 string
	RegistryEndpoints    []string
	ServiceName          string
	Timeout              time.Duration
	CounterBusinessTypes map[string]int32
}

// Client 线程安全 stub.
type Client struct {
	cfg   Config
	mapMu sync.RWMutex
	btMap map[string]int32
}

const defaultRPCTimeout = 5 * time.Second

// New 构造 stub client (不真连 accounting-system).
func New(cfg Config) (*Client, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultRPCTimeout
	}
	bt := make(map[string]int32, len(cfg.CounterBusinessTypes))
	for k, v := range cfg.CounterBusinessTypes {
		bt[strings.ToLower(k)] = v
	}
	return &Client{cfg: cfg, btMap: bt}, nil
}

// PrewarmFleetAccounts no-op (no Kitex connection to warm).
func (c *Client) PrewarmFleetAccounts(_ context.Context, _ map[string]int32, _ string) error {
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

// Close no-op.
func (c *Client) Close() error { return nil }

// DoubleEntryBooking stub: 永远返 ErrNotWired, outbox worker 会重试.
func (c *Client) DoubleEntryBooking(_ context.Context, _ *domain.AccountingOutbox) error {
	return ErrNotWired
}
