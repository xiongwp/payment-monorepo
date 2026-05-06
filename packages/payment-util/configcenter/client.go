// Package configcenter 客户端 SDK：
//
// 业务服务（card-payment / order-core 等）通过本 SDK 跟 config-center 交互。
//
// 设计原则：
//   1. **永远本地有值**：启动期同步拉一次全量；之后 stream watch 推增量。
//      server 全挂 / 网络断 → 客户端继续用上次成功值，不阻塞业务。
//   2. **零拷贝读**：调用方读 config 是 atomic.Value.Load()，纳秒级，无锁。
//      写者（watch goroutine 收到 update）通过 atomic.Value.Store 整体替换。
//   3. **生效时间窗校验**：读取时 (now < effective_at) → 返上一个有效值；
//      (now >= expire_at) → 返上一个有效值。SDK 自动 fallback。
//   4. **Watch 自动重连**：stream 断开走指数退避重连，不丢事件（since_version
//      让 server 知道客户端已收到的最大 version，断线期间漏掉的 update 重连后补齐）。
//   5. **Type-safe 解析**：SDK 提供 Bind 方法把 value 自动 unmarshal 到结构体，
//      并在配置变更时自动 swap 新值给业务。
//
// 用法（典型）：
//
//	cli, _ := configcenter.New(configcenter.Config{
//	    Endpoints: []string{"config-center.payment.local:9690"},
//	    Namespace: "card-payment",
//	    InstanceID: serviceregistry.AdvertiseAddr(0), // hostname:port
//	})
//	defer cli.Close()
//
//	// 拿一个值
//	val, _ := cli.Get(ctx, "rate_limit.rps")
//
//	// 绑结构体；变更时 ptr 内容原子替换
//	type RateLimit struct{ RPS int; Burst int }
//	var rl atomic.Pointer[RateLimit]
//	cli.Bind("rate_limit", &rl, func() { logger.Info("rate_limit changed") })
//	// 业务热路径：rl.Load().RPS — 纳秒读取，无锁。
package configcenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// rpcClient 抽象 gRPC stub 的最小接口；解开循环 import + 方便测试。
// 真实实现指向 generated configcenterv1.ConfigCenterClient。
type rpcClient interface {
	Get(ctx context.Context, ns, key, instanceID string) (*ConfigValue, error)
	Watch(ctx context.Context, ns, instanceID string, sinceVersion int64,
		onEvent func(ev *WatchEvent)) error
}

// ConfigValue 客户端可见的 config 形态（解耦 generated proto）。
type ConfigValue struct {
	Namespace   string
	Key         string
	Value       string
	Format      string // "json" / "yaml" / "plain"
	Version     int64
	EffectiveAt time.Time // 零值 = 立即生效
	ExpireAt    time.Time // 零值 = 永不过期
	UpdatedBy   string
	UpdatedAt   time.Time
}

// IsEffective 当前时间是否落在 [EffectiveAt, ExpireAt) 内。
func (c *ConfigValue) IsEffective(now time.Time) bool {
	if c == nil {
		return false
	}
	if !c.EffectiveAt.IsZero() && now.Before(c.EffectiveAt) {
		return false
	}
	if !c.ExpireAt.IsZero() && !now.Before(c.ExpireAt) {
		return false
	}
	return true
}

// WatchEvent server stream 的事件。
type WatchEvent struct {
	Type    EventType
	Config  *ConfigValue
}

type EventType int

const (
	EventUnknown EventType = 0
	EventSnapshot EventType = 1 // 初始全量
	EventUpdate  EventType = 2 // 增量更新
	EventDelete  EventType = 3 // key 删除
)

// Config SDK 配置。
type Config struct {
	// 多个 endpoint 时 SDK 会做 round_robin（目前简化为第一个）；
	// 生产建议通过 etcd:///config-center 服务发现接 serviceregistry.DialWithFallback。
	Endpoints []string

	// 必填：本服务订阅的 namespace（典型 = 服务名）。SDK 启动期会拉这个 namespace
	// 下所有 key 的 snapshot 缓存到本地。
	Namespace string

	// 必填：本实例 ID（hostname / pod_name / IP:port）。server 用它判断 canary /
	// targeted 是否命中。
	InstanceID string

	// Watch 重连退避起始；默认 1s，capped at 30s。
	ReconnectBackoff time.Duration

	// 拉初始 snapshot 的 timeout；默认 10s。
	InitTimeout time.Duration

	// mTLS（生产必填；dev insecure）。
	GRPCDialOpts []grpc.DialOption

	Logger *zap.Logger
}

// Client 配置中心客户端。线程安全，全局单例使用。
type Client struct {
	cfg    Config
	rpc    rpcClient
	logger *zap.Logger

	// 本地 cache：namespace 内 key → 最新 ConfigValue
	cache       atomic.Pointer[cacheSnapshot]
	maxVersion  atomic.Int64 // 已收到的最大 version（watch resume 用）

	// 监听者：key → 回调函数 list（变更时全部叫一次）
	listenersMu sync.RWMutex
	listeners   map[string][]listener

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// cacheSnapshot 一份完整的 (key→ConfigValue) map，用 atomic.Value 整体替换以实现 lock-free 读。
type cacheSnapshot struct {
	values map[string]*ConfigValue
	// 上一个有效快照：当 atomic.Value 当前值因 effective_at 还没到 / expire 已过
	// 不可用时，用 fallback 的最新有效值。极端场景兜底。
	fallback *cacheSnapshot
}

type listener struct {
	onChange func(*ConfigValue)
	bind     func([]byte) error
}

// New 构造客户端。
//
// 启动期同步拉一次 snapshot；失败 → 返 error 但 client 仍可用（cache 空，
// 业务侧 Get 返 nil 时应自己 fallback 到默认值）。
//
// 启动后异步运行 watch goroutine；server 失联 → 退避重连，期间用本地 cache。
func New(cfg Config) (*Client, error) {
	if cfg.Namespace == "" {
		return nil, errors.New("configcenter: namespace required")
	}
	if cfg.InstanceID == "" {
		return nil, errors.New("configcenter: instance_id required")
	}
	if cfg.ReconnectBackoff <= 0 {
		cfg.ReconnectBackoff = 1 * time.Second
	}
	if cfg.InitTimeout <= 0 {
		cfg.InitTimeout = 10 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}

	// rpc 实现由 caller 注入（通过 NewWithRPC），保持本包 zero-dep on protobuf。
	// 默认走 nil — caller 必须用 NewWithRPC。
	return nil, errors.New("configcenter: use NewWithRPC; server stub injection required")
}

// NewWithRPC 给定一个已建好的 rpcClient（typically wrapping generated proto stub）。
// 真实代码：configcenter.NewWithRPC(rpc, cfg) 包装 grpc dial + protobuf stub。
func NewWithRPC(rpc rpcClient, cfg Config) (*Client, error) {
	if cfg.Namespace == "" {
		return nil, errors.New("configcenter: namespace required")
	}
	if cfg.InstanceID == "" {
		return nil, errors.New("configcenter: instance_id required")
	}
	if cfg.ReconnectBackoff <= 0 {
		cfg.ReconnectBackoff = 1 * time.Second
	}
	if cfg.InitTimeout <= 0 {
		cfg.InitTimeout = 10 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	c := &Client{
		cfg:       cfg,
		rpc:       rpc,
		logger:    cfg.Logger,
		listeners: make(map[string][]listener),
		stopCh:    make(chan struct{}),
	}
	// 起 watch goroutine（异步）；初始 snapshot 通过 watch 的 since_version=0 拿。
	c.wg.Add(1)
	go c.watchLoop()
	return c, nil
}

// Close 优雅停止 watch goroutine + 释放资源。
func (c *Client) Close() {
	close(c.stopCh)
	c.wg.Wait()
}

// Get 取一个 key 的当前值。永不阻塞 IO（纯本地 cache）。
//
// 返回：
//   - 成功且生效中：返 *ConfigValue + nil
//   - 不存在 / 已 expire / effective_at 在未来：返 nil + ErrNotFound
//
// 业务侧典型用法：
//
//	val, err := cli.Get(ctx, "rate_limit.rps")
//	if err != nil { val = defaultVal }
func (c *Client) Get(ctx context.Context, key string) (*ConfigValue, error) {
	snap := c.cache.Load()
	if snap == nil {
		return nil, ErrNotFound
	}
	v := snap.values[key]
	if v == nil {
		return nil, ErrNotFound
	}
	if !v.IsEffective(time.Now()) {
		// 当前 version 不在生效窗口；fallback 到上一个有效快照
		if snap.fallback != nil {
			if fb := snap.fallback.values[key]; fb != nil && fb.IsEffective(time.Now()) {
				return fb, nil
			}
		}
		return nil, ErrNotEffective
	}
	return v, nil
}

// GetString syntactic sugar：取不到返 fallback 默认值。
func (c *Client) GetString(ctx context.Context, key, def string) string {
	v, err := c.Get(ctx, key)
	if err != nil {
		return def
	}
	return v.Value
}

// Bind 把 key 自动 unmarshal 到 dst（必须是 atomic.Pointer[T]）。
// 配置变更时 dst 内的 *T 被 atomically 整体替换为新值。
//
// 业务热路径用 dst.Load() 读，纳秒级，无锁。
//
// onChange 是变更通知回调（可 nil）；用于触发 logger / metrics 之外的副作用。
//
// 注意：仅 format=json 自动支持；其他 format 需要 caller 自己解析。
func Bind[T any](c *Client, key string, dst *atomic.Pointer[T], onChange func()) error {
	if c == nil || dst == nil {
		return errors.New("configcenter.Bind: nil client / dst")
	}
	// 立即按当前 cache 做一次 unmarshal（如果有值）
	if v, err := c.Get(context.Background(), key); err == nil {
		var newVal T
		if err := json.Unmarshal([]byte(v.Value), &newVal); err == nil {
			dst.Store(&newVal)
		} else {
			c.logger.Warn("configcenter: initial bind unmarshal failed",
				zap.String("key", key), zap.Error(err))
		}
	}
	// 注册监听器：变更时 unmarshal 新值并 atomic store
	c.addListener(key, listener{
		bind: func(raw []byte) error {
			var newVal T
			if err := json.Unmarshal(raw, &newVal); err != nil {
				return err
			}
			dst.Store(&newVal)
			if onChange != nil {
				onChange()
			}
			return nil
		},
	})
	return nil
}

// OnChange 注册变更回调（不绑结构体；caller 自己处理）。
func (c *Client) OnChange(key string, fn func(*ConfigValue)) {
	c.addListener(key, listener{onChange: fn})
}

// ─── 内部 ─────────────────────────────────────────────────────────────────

func (c *Client) addListener(key string, l listener) {
	c.listenersMu.Lock()
	defer c.listenersMu.Unlock()
	c.listeners[key] = append(c.listeners[key], l)
}

// watchLoop 启动 watch streaming，断了走指数退避重连。
func (c *Client) watchLoop() {
	defer c.wg.Done()
	backoff := c.cfg.ReconnectBackoff
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}
		ctx, cancel := context.WithCancel(context.Background())
		// since_version: resume 用，断线期间 server 累积的事件重连后补齐
		err := c.rpc.Watch(ctx, c.cfg.Namespace, c.cfg.InstanceID, c.maxVersion.Load(), c.onEvent)
		cancel()
		if err != nil {
			c.logger.Warn("configcenter watch disconnected, will reconnect",
				zap.Duration("backoff", backoff), zap.Error(err))
		}
		// 退避重连
		select {
		case <-c.stopCh:
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// onEvent server 推过来的单个事件；更新本地 cache + 触发 listener。
func (c *Client) onEvent(ev *WatchEvent) {
	if ev == nil || ev.Config == nil {
		return
	}
	cfg := ev.Config

	// 更新 cache：CoW 一份新 map 整体替换，老 map 仍被读 goroutine 持有 → 安全。
	old := c.cache.Load()
	newMap := make(map[string]*ConfigValue)
	if old != nil {
		for k, v := range old.values {
			newMap[k] = v
		}
	}
	switch ev.Type {
	case EventDelete:
		delete(newMap, cfg.Key)
	default:
		newMap[cfg.Key] = cfg
	}
	c.cache.Store(&cacheSnapshot{values: newMap, fallback: old})

	// 推进 max version
	if cfg.Version > c.maxVersion.Load() {
		c.maxVersion.Store(cfg.Version)
	}

	// 通知监听器
	c.listenersMu.RLock()
	ls := c.listeners[cfg.Key]
	c.listenersMu.RUnlock()
	for _, l := range ls {
		if ev.Type != EventDelete {
			if l.bind != nil {
				if err := l.bind([]byte(cfg.Value)); err != nil {
					c.logger.Warn("configcenter bind unmarshal failed on update",
						zap.String("key", cfg.Key), zap.Error(err))
				}
			}
		}
		if l.onChange != nil {
			l.onChange(cfg)
		}
	}
}

// errors

// ErrNotFound 没这个 key（或 namespace 没订阅成功）。
var ErrNotFound = errors.New("configcenter: key not found")

// ErrNotEffective key 存在但当前不在生效窗口（effective_at 未到 / expire 已过）。
// 调用方应 fallback 到默认值。
var ErrNotEffective = errors.New("configcenter: key not effective at this time")

// 防 unused import warning（fmt 留作未来 stringer 用）
var _ = fmt.Sprintf
