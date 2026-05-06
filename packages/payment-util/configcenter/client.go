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

	// **双版本本地 cache** — 每个 key 同时保存：
	//   active   当前生效版本
	//   pending  未来某时刻才生效的版本（effective_at > now）
	// 业务 Get 永远只读 active；后台 swapper goroutine 到点把 pending 提到 active。
	// 这样：
	//   1. SCHEDULED 推送提前到达，本地待命；到点零延迟切换（不依赖 server 重推）
	//   2. server 推送丢失或网络断了，本地按 pending 的 effective_at 自动切，业务无感
	//   3. 切换是原子（atomic.Pointer 整体替换），业务读纳秒级
	cache      atomic.Pointer[cacheSnapshot]
	maxVersion atomic.Int64 // 已收到的最大 version（watch resume 用）

	// 监听者：key → 回调函数 list（变更时全部叫一次）
	listenersMu sync.RWMutex
	listeners   map[string][]listener

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// cacheEntry 单 key 的双版本槽。
type cacheEntry struct {
	// active 业务读到的当前生效值（永远满足 IsEffective(now) 或 nil）
	active *ConfigValue
	// pending 已收到但 effective_at > now 的未来值；到点后被 swapper 提到 active
	pending *ConfigValue
}

// cacheSnapshot 整个 namespace 的 key 集合。整体替换实现 lock-free 读。
type cacheSnapshot struct {
	entries map[string]*cacheEntry
	// fallback 上一份快照；当 active 因 expire 已过不可用时找上一个有效值兜底
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
//
// **启动期阻塞**：
//   1. 同步调 rpc.Watch（since_version=0）拿全量 snapshot；caller 必须在
//      cfg.InitTimeout（默认 10s）内全部收到，否则返 error 让 fx fail-fast。
//   2. 收到 snapshot 后初始化 cache，再起常驻 watchLoop / swapperLoop。
//   3. caller (main.go) 应该判断 error：
//        if err != nil { logger.Fatal("config-center 起不来，服务拒绝启动") }
//      或退化策略（dev only）：用 hardcoded fallback 继续。
//      生产**必须** fatal — 防止服务带空 cache 上线读 default 行为不符合预期。
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

	// **同步初始化**：调一次 Watch 拉 SNAPSHOT，超时 → 失败。
	if err := c.initialLoad(); err != nil {
		return nil, fmt.Errorf("configcenter: initial load: %w", err)
	}

	// 初始 snapshot 已写 cache；现在起常驻 goroutine 接增量。
	c.wg.Add(1)
	go c.watchLoop()
	c.wg.Add(1)
	go c.swapperLoop()
	return c, nil
}

// initialLoad 同步阻塞：调一次 Watch 拉全量 snapshot 写本地 cache。
//
// 实现要点：
//   - since_version=0 → server 推 EventType=SNAPSHOT 给本 namespace 全量
//   - server 推完 snapshot 会发一个 sentinel 事件（type=SNAPSHOT, Config=nil 表示 done）
//     或直接保持 stream open 进入 realtime 模式 — SDK 用 InitTimeout 兜底
//   - timeout 内没收到任何事件（namespace 空也算 OK）→ 仍认为初始化成功
//   - 网络 / RPC 错 → 返 error 让 caller fail-fast
//
// 改进点（生产可加）：
//   - server stream 加 explicit "snapshot_done" 事件让 client 精准判定
//   - 给 ctx 加 deadline；超时直接 error 返
func (c *Client) initialLoad() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.InitTimeout)
	defer cancel()

	// 用 channel 收事件，超时 / 收满 / 错误任一退出
	done := make(chan error, 1)
	receivedAtLeastOne := false

	go func() {
		err := c.rpc.Watch(ctx, c.cfg.Namespace, c.cfg.InstanceID, 0, func(ev *WatchEvent) {
			if ev != nil && ev.Config != nil {
				c.applyEvent(ev) // 写 cache，但不触发 listeners（启动期没有 listeners）
				receivedAtLeastOne = true
			}
		})
		done <- err
	}()

	select {
	case err := <-done:
		// 短连接情况：server 推完 snapshot 立即关 stream；这是 OK 的
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			return err
		}
	case <-ctx.Done():
		// timeout：空 namespace（0 个 key）也算成功 — 业务用 hardcoded
		// 默认值跑，admin 后续在 config-center admin web 加 key 即可热更新。
		// 早期版本严格要求至少 1 个 key 太死板：seed 还没跑、新服务首次上线
		// 都会卡这里，让所有业务服务集体启动失败。
		if receivedAtLeastOne {
			c.logger.Info("configcenter: snapshot phase ended on timeout (received some)",
				zap.Duration("timeout", c.cfg.InitTimeout))
		} else {
			c.logger.Info("configcenter: namespace empty (no keys yet); SDK ready, business uses hardcoded defaults",
				zap.String("namespace", c.cfg.Namespace),
				zap.Duration("waited", c.cfg.InitTimeout))
		}
	}
	c.logger.Info("configcenter: initial snapshot loaded",
		zap.String("namespace", c.cfg.Namespace),
		zap.Int64("max_version", c.maxVersion.Load()))
	return nil
}

// applyEvent onEvent 的纯写 cache 版本（不触发 listeners）。
// 启动期没人监听，省去 fireListeners 调用。
func (c *Client) applyEvent(ev *WatchEvent) {
	cfg := ev.Config
	old := c.cache.Load()
	newEntries := make(map[string]*cacheEntry)
	if old != nil {
		for k, e := range old.entries {
			newEntries[k] = e
		}
	}
	now := time.Now()
	prev := newEntries[cfg.Key]
	if prev == nil {
		prev = &cacheEntry{}
	}
	if cfg.IsEffective(now) {
		newEntries[cfg.Key] = &cacheEntry{active: cfg}
	} else if !cfg.EffectiveAt.IsZero() && cfg.EffectiveAt.After(now) {
		newEntries[cfg.Key] = &cacheEntry{active: prev.active, pending: cfg}
	} else {
		newEntries[cfg.Key] = prev
	}
	c.cache.Store(&cacheSnapshot{entries: newEntries, fallback: old})
	if cfg.Version > c.maxVersion.Load() {
		c.maxVersion.Store(cfg.Version)
	}
}

// Close 优雅停止 watch goroutine + 释放资源。
func (c *Client) Close() {
	close(c.stopCh)
	c.wg.Wait()
}

// Get 取一个 key 当前**按时间**最该生效的值。永不阻塞 IO（纯本地 cache）。
//
// 选择优先级（高 → 低，遇到第一个 IsEffective(now) 返）：
//   1. **entry.pending** — 如果 pending.EffectiveAt 已经到点，pending 是"最新生效"
//      （即使 swapper 还没跑到把它提到 active；swapper 是 1Hz tick，可能滞后 ≤1s）
//      这一步保证读到的总是按时间最准的值，不受 swapper 节奏影响。
//   2. **entry.active** — 当前在跑的版本，IsEffective 仍成立
//   3. **fallback.active** — 上一份快照的 active（active 已 expire 时兜底）
//
// 返回：
//   - 命中 1/2/3：返 *ConfigValue + nil
//   - 全部不可用 / key 不存在：返 nil + ErrNotFound / ErrNotEffective
//
// 业务热路径调用：纳秒级（atomic.Pointer.Load + map lookup + 1-3 次 IsEffective）。
//
// 用法：
//
//	val, err := cli.Get(ctx, "rate_limit.rps")
//	if err != nil { val = defaultVal }
func (c *Client) Get(ctx context.Context, key string) (*ConfigValue, error) {
	snap := c.cache.Load()
	if snap == nil {
		return nil, ErrNotFound
	}
	now := time.Now()
	if e := snap.entries[key]; e != nil {
		// 1) pending 已到点：它就是最新生效。哪怕 swapper 还没把它提到 active，
		//    Get 也直接返 pending — 时间精度由 Get 自己负责，不依赖 swapper 节奏。
		if e.pending != nil && e.pending.IsEffective(now) {
			return e.pending, nil
		}
		// 2) active 仍在生效窗口
		if e.active != nil && e.active.IsEffective(now) {
			return e.active, nil
		}
	}
	// 3) fallback 兜底（active 已 expire 时找上一个有效）
	if snap.fallback != nil {
		if fe := snap.fallback.entries[key]; fe != nil && fe.active != nil && fe.active.IsEffective(now) {
			return fe.active, nil
		}
	}
	return nil, ErrNotEffective
}

// GetPending 取该 key 即将生效的版本（如果有）。
// 业务一般不需要；admin / debug 工具用，可视化"还有 N 秒切换"。
func (c *Client) GetPending(key string) *ConfigValue {
	snap := c.cache.Load()
	if snap == nil {
		return nil
	}
	if e := snap.entries[key]; e != nil {
		return e.pending
	}
	return nil
}

// swapperLoop 后台 ticker：每秒扫所有 pending，到点 atomic swap 到 active。
//
// 1Hz 的精度满足业务需求（"凌晨 2 点切换 rate_limit"差几秒可接受）。
// 高频精度的场景（毫秒级）可调到 100Hz；目前 1Hz 是合理默认。
//
// 切换过程：
//   1. 检查 entry.pending != nil && pending.IsEffective(now)
//   2. CoW 一份新 map，被影响的 entry 复制：active=pending; pending=nil
//   3. cache.Store(newSnap)
//   4. 触发 listeners（onChange / Bind）让业务知道切了
//
// **关键纪律**：swap 时也校验 pending 是否仍然 IsEffective —— 防止 server 撤回
// 了 pending 但本地还在跑（撤回流程：server 推 update 替换原 pending → onEvent
// 已经更新了 entry；这里是 race 兜底）。
func (c *Client) swapperLoop() {
	defer c.wg.Done()
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-t.C:
			c.maybeSwap()
		}
	}
}

// maybeSwap 单次扫描；有 pending 到点就 atomic 替换 cache 整体。
func (c *Client) maybeSwap() {
	old := c.cache.Load()
	if old == nil {
		return
	}
	now := time.Now()
	swapped := false
	newEntries := make(map[string]*cacheEntry, len(old.entries))
	for k, e := range old.entries {
		if e.pending != nil && e.pending.IsEffective(now) {
			// 切！pending → active
			newEntries[k] = &cacheEntry{active: e.pending}
			swapped = true
			// 通知 listeners
			c.fireListeners(k, e.pending)
		} else {
			// 不变（pending 还没到点 / 没 pending）
			newEntries[k] = e
		}
	}
	if !swapped {
		return
	}
	c.cache.Store(&cacheSnapshot{entries: newEntries, fallback: old})
	c.logger.Info("configcenter: pending → active swap committed",
		zap.Time("at", now))
}

// fireListeners 抽到独立函数让 swapper 和 onEvent 共用。
func (c *Client) fireListeners(key string, val *ConfigValue) {
	c.listenersMu.RLock()
	ls := c.listeners[key]
	c.listenersMu.RUnlock()
	for _, l := range ls {
		if l.bind != nil {
			if err := l.bind([]byte(val.Value)); err != nil {
				c.logger.Warn("configcenter bind unmarshal failed",
					zap.String("key", key), zap.Error(err))
			}
		}
		if l.onChange != nil {
			l.onChange(val)
		}
	}
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
//
// **路由到 active / pending 槽**：
//   - cfg.EffectiveAt == zero 或 <= now → 立即生效，写 entry.active；
//     如果 entry 已经有 pending 且 pending.Version < cfg.Version：清掉 pending
//     （新 active 比 pending 还新，pending 失效）
//   - cfg.EffectiveAt > now → 写 entry.pending（不动 active；business 还读老的）；
//     swapper 会到点提
//
// EventDelete 同时清 active + pending。
func (c *Client) onEvent(ev *WatchEvent) {
	if ev == nil || ev.Config == nil {
		return
	}
	cfg := ev.Config

	old := c.cache.Load()
	newEntries := make(map[string]*cacheEntry)
	if old != nil {
		for k, e := range old.entries {
			newEntries[k] = e
		}
	}

	now := time.Now()
	switch ev.Type {
	case EventDelete:
		delete(newEntries, cfg.Key)
		c.cache.Store(&cacheSnapshot{entries: newEntries, fallback: old})

	default:
		// active vs pending 路由
		var nextEntry *cacheEntry
		prev := newEntries[cfg.Key]
		if prev == nil {
			prev = &cacheEntry{}
		}
		if cfg.IsEffective(now) {
			// 立即生效
			nextEntry = &cacheEntry{active: cfg}
			// 如果有更老的 pending（version 更低），它已被覆盖
		} else if !cfg.EffectiveAt.IsZero() && cfg.EffectiveAt.After(now) {
			// 未来生效：放 pending；保留 active
			nextEntry = &cacheEntry{
				active:  prev.active,
				pending: cfg,
			}
		} else {
			// 已 expire 不入 cache（保持 prev）
			nextEntry = prev
		}
		newEntries[cfg.Key] = nextEntry
		c.cache.Store(&cacheSnapshot{entries: newEntries, fallback: old})

		// 推进 max version（不管 active 还是 pending 都要推）
		if cfg.Version > c.maxVersion.Load() {
			c.maxVersion.Store(cfg.Version)
		}

		// 触发 listeners：仅当 active 真的变了才叫；pending 落地时是 swapper 叫
		if nextEntry.active == cfg {
			c.fireListeners(cfg.Key, cfg)
		} else if cfg.IsEffective(now) {
			// IsEffective 但走到 else 分支（极端：cfg 跟 prev.active 同一个）；
			// 仍触发一次保 idempotent
			c.fireListeners(cfg.Key, cfg)
		} else {
			// pending 入库；不通知业务，等 swap 时再通知
			c.logger.Debug("configcenter: pending received, awaits effective_at",
				zap.String("key", cfg.Key),
				zap.Time("effective_at", cfg.EffectiveAt),
				zap.Int64("version", cfg.Version))
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
