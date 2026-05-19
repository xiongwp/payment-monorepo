package grpc

import (
	"context"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/xiongwp/accounting-system/internal/metrics"
	"go.uber.org/zap"
)

// recoveryInterceptor 捕获 handler panic，把它转成 codes.Internal error 而不是杀进程。
//
// 没有 recover() 时单条 RPC panic（nil deref / index 越界 / 第三方库 bug）会带走整个进程，
// 拖垮所有正在执行的 TCC Confirm / OutboxWorker / day-cut shard goroutine。
//
// 链顺序：放在最外层（先于 loadshed），保证 loadshed 自身的 panic 也被兜底。
// 日志含 stack trace 进 api.log，便于离线排查。
func recoveryInterceptor(logger *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				logger.Error("grpc handler panic recovered",
					zap.String("method", info.FullMethod),
					zap.Any("panic", r),
					zap.ByteString("stack", stack),
				)
				err = fmt.Errorf("internal server error: panic recovered")
			}
		}()
		return handler(ctx, req)
	}
}

// timeoutInterceptor 强制每条 RPC 的 ctx 至少有一个上限：
//   - client 给的 deadline 比 maxDuration 短 → 用 client 的（不延长）
//   - client 没给 / 给的更长 → 截断到 maxDuration
//
// 防御场景：客户端忘记设 deadline + 大日期范围 ListTransactions → 后端 goroutine
// + DB 连接被它占着不放，最终触发 load-shed 拒绝其他正常请求。
//
// maxDuration <= 0 表示禁用此拦截器。
func timeoutInterceptor(maxDuration time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if maxDuration <= 0 {
			return handler(ctx, req)
		}
		// 已有更早的 deadline 直接复用，不延长
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= maxDuration {
			return handler(ctx, req)
		}
		ctx, cancel := context.WithTimeout(ctx, maxDuration)
		defer cancel()
		return handler(ctx, req)
	}
}

// LoadShedConfig 入口级限流/过载保护配置。
//
// 两道闸门互相独立，顺序是 inflight 先 → rate 后：
//  1. MaxInflight：信号量，当前正在处理的 RPC 数，饱和后即时拒绝（ResourceExhausted）。
//     保护点：DB 连接池、goroutine 栈、锁竞争。 0 表示关闭（不做 inflight 保护）。
//  2. RatePerSecond + Burst：全局令牌桶，限制稳态 QPS，突发流量先吃 burst。
//     0 表示关闭。
//
// 两个都非零时任一触发都会 fast-fail。被拒绝的请求都会打 LoadShedDroppedTotal 计数并
// 返回 codes.ResourceExhausted，方便客户端区分 Unavailable（断连）和过载。
type LoadShedConfig struct {
	MaxInflight   int
	RatePerSecond int
	Burst         int
}

// loadShedder 实现 UnaryServerInterceptor。两道闸门：
//   - inflight 信号量：用 atomic 计数器实现，maxInflight 可动态调整（outbox backpressure 用）
//     若改用 channel，capacity 不可变，无法实现 backpressure 时收缩
//   - tokenBucket：由后台 goroutine 每 (1/rate) 秒填一个令牌；bucket 容量=burst（不变）
type loadShedder struct {
	// inflight 当前并发请求数；maxInflight 上限（0 = 不限）。两者均原子操作。
	// 设计选型：atomic counter（不是 chan）→ 上限可在运行时 SetMaxInflight 调整，
	// 让 outbox 积压时收缩 / 缓解后扩张，避免 hot path 把 OutboxWorker 压垮造成雪崩。
	inflight    atomic.Int64
	maxInflight atomic.Int64
	tokenBucket chan struct{} // nil 表示未启用
	started     atomic.Bool   // 防止 StartRefillLoop 被多次启动
}

// newLoadShedder 构造一个空 shedder（不管理 refill goroutine），由 ListenAndServe 负责
// 调用 StartRefillLoop 并把 ctx 传进来。
func newLoadShedder(cfg LoadShedConfig) *loadShedder {
	ls := &loadShedder{}
	ls.maxInflight.Store(int64(cfg.MaxInflight))
	if cfg.RatePerSecond > 0 {
		burst := cfg.Burst
		if burst <= 0 {
			burst = cfg.RatePerSecond // 默认 burst == 1s 填充量
		}
		ls.tokenBucket = make(chan struct{}, burst)
		// 初始满桶，第一秒可以立刻打光 burst 次（典型 warmup 行为）
		for i := 0; i < burst; i++ {
			ls.tokenBucket <- struct{}{}
		}
	}
	return ls
}

// SetMaxInflight 运行时调整并发上限。0 表示不限。
// 用例：OutboxWorker 积压超过阈值 → backpressure 工作器收缩 max；积压恢复后扩回原值。
func (ls *loadShedder) SetMaxInflight(n int64) {
	if n < 0 {
		n = 0
	}
	ls.maxInflight.Store(n)
}

// MaxInflight 当前生效上限（用于监控 / 状态展示）。
func (ls *loadShedder) MaxInflight() int64 { return ls.maxInflight.Load() }

// Inflight 当前真实并发数（用于监控）。
func (ls *loadShedder) Inflight() int64 { return ls.inflight.Load() }

// startRefillLoop 启动后台 goroutine 按配置速率回填 token 桶。
// ctx Done 后 goroutine 退出，所有待处理 refill 停止。
// 如果 tokenBucket 未启用则直接返回。
func (ls *loadShedder) startRefillLoop(ctx context.Context, ratePerSecond int) {
	if ls.tokenBucket == nil || ratePerSecond <= 0 {
		return
	}
	if !ls.started.CompareAndSwap(false, true) {
		return
	}
	// 每 1s/rate 填一个；rate=10000 时 100µs 一个 token，ticker 精度足够
	interval := time.Second / time.Duration(ratePerSecond)
	if interval < time.Microsecond {
		interval = time.Microsecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 非阻塞填桶：桶满就丢（符合令牌桶语义）
				select {
				case ls.tokenBucket <- struct{}{}:
				default:
				}
			}
		}
	}()
}

// unaryInterceptor 返回一个 grpc.UnaryServerInterceptor。
// 闸门顺序：先 inflight 再 rate（rate 更 expensive，尽早挡住无效 rate 消耗）。
func (ls *loadShedder) unaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		// ─── Gate 1: inflight 信号量（atomic） ───
		// max=0 表示不限。Add/Compare-then-decrement 模式：
		//   先 +1，若发现超限再 -1 拒绝（轻微"过冲"几条但 metric 准确，可接受）。
		if max := ls.maxInflight.Load(); max > 0 {
			cur := ls.inflight.Add(1)
			if cur > max {
				ls.inflight.Add(-1)
				metrics.LoadShedDroppedTotal.WithLabelValues("max_inflight").Inc()
				return nil, fmt.Errorf("server overloaded: max_inflight=%d exceeded", max)
			}
			defer ls.inflight.Add(-1)
		}

		// ─── Gate 2: 令牌桶 ───
		if ls.tokenBucket != nil {
			select {
			case <-ls.tokenBucket:
				// got token, proceed
			default:
				metrics.LoadShedDroppedTotal.WithLabelValues("rate_limit").Inc()
				return nil, fmt.Errorf("server overloaded: rate limit exceeded")
			}
		}

		return handler(ctx, req)
	}
}
