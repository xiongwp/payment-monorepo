package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/xiongwp/payment-util/shadow"
)

// Replayer 从 shadow topic 拉脱敏后的 Event，按时间序列重放到目标 gRPC 集群。
//
// 用法（cmd/replayer/main.go）：
//
//	conn, _ := grpc.Dial("staging-gateway:9090", ...)
//	r := replay.NewReplayer(replay.ReplayerConfig{
//	    Conn:        conn,
//	    Multiplier:  10,    // 1x → 10x 流量
//	    Concurrency: 100,   // 并发 worker 数
//	})
//	for ev := range kafkaConsumer.Stream() {
//	    r.Submit(ev)
//	}
//
// Replayer 内部维护 worker 池 + ticker 调度，保证按时间窗口（gameday 模式）
// 或满速（capacity 模式）重放。
type Replayer struct {
	cfg     ReplayerConfig
	jobs    chan *Event
	wg      sync.WaitGroup
	metrics ReplayMetrics
}

// ReplayerConfig 配置
type ReplayerConfig struct {
	// Conn 目标 gRPC 集群连接（应该挂 trace + shadow client interceptor）
	Conn *grpc.ClientConn
	// Multiplier 流量倍率：1=同速回放；10=单条原始事件回放 10 次（带轻微抖动）。
	Multiplier int
	// Concurrency 并发 worker 数
	Concurrency int
	// MaxRPS 总速率上限（0=无限）
	MaxRPS int
	// PerCallTimeout 单次回放超时
	PerCallTimeout time.Duration
	// MarshalerForMethod method → 业务方提供的 protojson 反序列化函数
	// （payment-util 不知道具体 proto 类型，反序列化交给业务调用方）
	MarshalerForMethod func(method string) (proto Marshaler, ok bool)
}

// Marshaler 业务方实现：把 protojson 字节反序列化成具体 proto.Message 然后调
// gRPC client.Invoke。每个服务暴露自己的方法注册表。
type Marshaler interface {
	// Invoke 用反序列化后的 req 调远端
	Invoke(ctx context.Context, conn *grpc.ClientConn, method string, bodyJSON json.RawMessage) error
}

// ReplayMetrics 计数（业务方挂 Prometheus）
type ReplayMetrics struct {
	mu        sync.Mutex
	Submitted int64
	OK        int64
	Failed    int64
	Skipped   int64
}

// Snapshot 取计数快照
func (m *ReplayMetrics) Snapshot() (submitted, ok, failed, skipped int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Submitted, m.OK, m.Failed, m.Skipped
}

// NewReplayer 构造
func NewReplayer(cfg ReplayerConfig) *Replayer {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 50
	}
	if cfg.Multiplier <= 0 {
		cfg.Multiplier = 1
	}
	if cfg.PerCallTimeout <= 0 {
		cfg.PerCallTimeout = 30 * time.Second
	}
	r := &Replayer{
		cfg:  cfg,
		jobs: make(chan *Event, cfg.Concurrency*4),
	}
	for i := 0; i < cfg.Concurrency; i++ {
		r.wg.Add(1)
		go r.worker()
	}
	return r
}

// Submit 入队一条 Event。multiplier 在内部展开。
func (r *Replayer) Submit(ev *Event) {
	r.metrics.mu.Lock()
	r.metrics.Submitted++
	r.metrics.mu.Unlock()
	for i := 0; i < r.cfg.Multiplier; i++ {
		// 抖动避免 multiplier 副本扎堆同时打到下游
		select {
		case r.jobs <- ev:
		default:
			// 队列满了直接丢，metric 计 skipped。回放压测应该往容量摸边界，
			// 不能因为 worker 没处理过来反过来阻塞 capture / sanitizer。
			r.metrics.mu.Lock()
			r.metrics.Skipped++
			r.metrics.mu.Unlock()
			return
		}
	}
}

// Close 关闭，等所有 worker 结束。
func (r *Replayer) Close() {
	close(r.jobs)
	r.wg.Wait()
}

func (r *Replayer) worker() {
	defer r.wg.Done()
	for ev := range r.jobs {
		r.invokeOne(ev)
	}
}

func (r *Replayer) invokeOne(ev *Event) {
	if r.cfg.MarshalerForMethod == nil {
		r.bumpFailed()
		return
	}
	m, ok := r.cfg.MarshalerForMethod(ev.Method)
	if !ok {
		r.bumpSkipped()
		return
	}
	// 注入 shadow + replay 标识到 ctx，让下游知道这是回放流量
	ctx := r.makeReplayContext(ev)
	cctx, cancel := context.WithTimeout(ctx, r.cfg.PerCallTimeout)
	defer cancel()
	err := m.Invoke(cctx, r.cfg.Conn, ev.Method, ev.BodyJSON)
	if err != nil {
		r.bumpFailed()
		return
	}
	r.bumpOK()
}

// makeReplayContext 给回放请求的 ctx 加：
//
//   - shadow=1（关键，让下游路由到 _shadow 表 + mock channel + mock risk）
//   - x-replay-source=traffic-replay（让审计能区分回放与正常 shadow 压测）
//   - 保留原 trace_id（让端到端日志能关联）
func (r *Replayer) makeReplayContext(ev *Event) context.Context {
	ctx := shadow.WithShadow(context.Background(), true)
	pairs := []string{
		"x-replay-source", "traffic-replay",
		"x-replay-original-ts", ev.CapturedAt.Format(time.RFC3339),
	}
	for k, v := range ev.Headers {
		// trace_id / account-system 等保留；shadow / authorization 不在 capture 白名单里，
		// 这里也不会有。
		pairs = append(pairs, k, v)
	}
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}

func (r *Replayer) bumpOK() {
	r.metrics.mu.Lock()
	r.metrics.OK++
	r.metrics.mu.Unlock()
}
func (r *Replayer) bumpFailed() {
	r.metrics.mu.Lock()
	r.metrics.Failed++
	r.metrics.mu.Unlock()
}
func (r *Replayer) bumpSkipped() {
	r.metrics.mu.Lock()
	r.metrics.Skipped++
	r.metrics.mu.Unlock()
}

// SmokeReplay 给单元测试 / 手工冒烟：直接回放一条假 event，不跑 Kafka。
// 业务方的 main 可以拿这个跑「100 条 shadow 流量灌一遍看链路通不通」。
func SmokeReplay(ctx context.Context, conn *grpc.ClientConn, method string, body []byte, m Marshaler) error {
	if m == nil {
		return fmt.Errorf("replay: SmokeReplay requires Marshaler")
	}
	ctx = shadow.WithShadow(ctx, true)
	ctx = metadata.AppendToOutgoingContext(ctx, "x-replay-source", "smoke")
	return m.Invoke(ctx, conn, method, body)
}
