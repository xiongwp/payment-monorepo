// Package jobqueue — 基于 Redis 的轻量 async job queue。
//
// 用途: webhook 投递重试 / refund 渠道提交 / statement aggregation /
//       audit-log 异步落盘 等耗时操作。
//
// 跟 Kafka 区别:
//   - Kafka 适合 event-streaming, fan-out, 长期保留
//   - asynq 适合 任务-工人模型, 单工人消费, 重试 + 延迟 + 优先级 + scheduled
//
// 特性:
//   - 优先级 queue (critical / default / low)
//   - 自动重试 + 指数退避 + DLQ
//   - 延迟任务 (cron-style schedule 也支持)
//   - 任务唯一键 (幂等)
//   - 可观测: 内置 web UI (asynqmon) + Prometheus metrics
//
// 用例:
//   client.Enqueue(jobqueue.NewTask("refund.submit", payload),
//       jobqueue.Queue("critical"),
//       jobqueue.MaxRetry(5),
//       jobqueue.Timeout(30*time.Second),
//       jobqueue.Unique(time.Hour))   // 1h 内同 payload hash 仅入队 1 次
//
//   srv.HandleFunc("refund.submit", func(ctx context.Context, t *Task) error {
//       var p RefundSubmitPayload
//       json.Unmarshal(t.Payload(), &p)
//       return refundService.SubmitToChannel(ctx, p.RefundID)
//   })

package jobqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Priority queue 名 (按优先级降序)。
const (
	QueueCritical = "critical"
	QueueDefault  = "default"
	QueueLow      = "low"
)

// Task 一个待执行任务。
type Task struct {
	Type    string
	Payload []byte
	Options []Option
}

// NewTask 构造。
func NewTask(typ string, payload any, opts ...Option) (*Task, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	return &Task{Type: typ, Payload: b, Options: opts}, nil
}

// Option 任务选项。
type Option func(*taskOpts)

type taskOpts struct {
	Queue      string
	MaxRetry   int
	Timeout    time.Duration
	ProcessAt  time.Time     // 延迟到指定时间
	ProcessIn  time.Duration // 相对延迟
	UniqueTTL  time.Duration // 幂等窗口
	Deadline   time.Time     // 超时直接进 DLQ
}

// Queue 指定 queue。
func Queue(q string) Option { return func(o *taskOpts) { o.Queue = q } }

// MaxRetry 最大重试次数。
func MaxRetry(n int) Option { return func(o *taskOpts) { o.MaxRetry = n } }

// Timeout 单次执行超时。
func Timeout(d time.Duration) Option { return func(o *taskOpts) { o.Timeout = d } }

// ProcessAt 延迟到指定时间执行。
func ProcessAt(t time.Time) Option { return func(o *taskOpts) { o.ProcessAt = t } }

// ProcessIn 相对延迟。
func ProcessIn(d time.Duration) Option { return func(o *taskOpts) { o.ProcessIn = d } }

// Unique 任务幂等键: 相同 type+payload 在窗口内只入队 1 次。
func Unique(ttl time.Duration) Option { return func(o *taskOpts) { o.UniqueTTL = ttl } }

// Deadline 任务总执行时长上限 (重试加起来), 超过进 DLQ。
func Deadline(t time.Time) Option { return func(o *taskOpts) { o.Deadline = t } }

// ─── Client interface (调用方用) ─────────────────────────────────────

// Client 入队接口。
type Client interface {
	Enqueue(ctx context.Context, t *Task) (string, error) // 返 task_id
	Close() error
}

// ─── Handler / Server (worker 用) ────────────────────────────────────

// Handler 任务处理函数。
type Handler func(ctx context.Context, t *Task) error

// Server worker 接口。
type Server interface {
	HandleFunc(typ string, h Handler)
	Run() error
	Shutdown(ctx context.Context) error
}

// ─── 错误 ─────────────────────────────────────────────────────────────

// ErrTaskNotFound ...
var ErrTaskNotFound = errors.New("task not found")

// ErrSkipRetry handler 返这个 → 任务不重试直接失败 (e.g. payload 非法)。
var ErrSkipRetry = errors.New("skip retry")

// ─── In-process implementation (默认, dev/test 用) ────────────────────

// MemoryClient + MemoryServer 都是同一个 memoryQueue 实例。
//
// 生产替换成 RedisClient + RedisServer (用 github.com/hibiken/asynq), 接口不变。

type memoryQueue struct {
	tasks   chan envelope
	uniques map[string]time.Time // type+payload_hash → expire
	dlq     []envelope
	log     *zap.Logger
	handlers map[string]Handler
}

type envelope struct {
	taskID    string
	task      *Task
	opts      taskOpts
	attempts  int
}

// NewMemoryQueue 单实例 client + server (dev / test).
func NewMemoryQueue(log *zap.Logger) (*MemoryClient, *MemoryServer) {
	q := &memoryQueue{
		tasks:    make(chan envelope, 1024),
		uniques:  map[string]time.Time{},
		log:      log,
		handlers: map[string]Handler{},
	}
	return &MemoryClient{q: q}, &MemoryServer{q: q}
}

// MemoryClient 内存版 Client。
type MemoryClient struct{ q *memoryQueue }

// Enqueue 入队。
func (c *MemoryClient) Enqueue(_ context.Context, t *Task) (string, error) {
	o := taskOpts{Queue: QueueDefault, MaxRetry: 3, Timeout: 30 * time.Second}
	for _, opt := range t.Options {
		opt(&o)
	}
	// 幂等
	if o.UniqueTTL > 0 {
		key := t.Type + ":" + hashHex(t.Payload)
		if exp, ok := c.q.uniques[key]; ok && time.Now().Before(exp) {
			return "", errors.New("duplicate task (unique window)")
		}
		c.q.uniques[key] = time.Now().Add(o.UniqueTTL)
	}
	id := fmt.Sprintf("task-%d", time.Now().UnixNano())
	c.q.tasks <- envelope{taskID: id, task: t, opts: o}
	return id, nil
}

// Close ...
func (c *MemoryClient) Close() error {
	close(c.q.tasks)
	return nil
}

// MemoryServer 内存版 Server。
type MemoryServer struct{ q *memoryQueue }

// HandleFunc 注册 handler。
func (s *MemoryServer) HandleFunc(typ string, h Handler) {
	s.q.handlers[typ] = h
}

// Run 阻塞跑 worker。
func (s *MemoryServer) Run() error {
	for env := range s.q.tasks {
		h, ok := s.q.handlers[env.task.Type]
		if !ok {
			if s.q.log != nil {
				s.q.log.Warn("unknown task type, drop", zap.String("type", env.task.Type))
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), env.opts.Timeout)
		err := h(ctx, env.task)
		cancel()
		if err == nil {
			continue
		}
		if errors.Is(err, ErrSkipRetry) || env.attempts >= env.opts.MaxRetry {
			s.q.dlq = append(s.q.dlq, env)
			if s.q.log != nil {
				s.q.log.Error("task -> DLQ",
					zap.String("type", env.task.Type),
					zap.Int("attempts", env.attempts),
					zap.Error(err))
			}
			continue
		}
		// 重试: 指数退避后重入队
		env.attempts++
		go func(e envelope) {
			backoff := time.Duration(1<<e.attempts) * time.Second
			if backoff > 10*time.Minute {
				backoff = 10 * time.Minute
			}
			time.Sleep(backoff)
			s.q.tasks <- e
		}(env)
	}
	return nil
}

// Shutdown 优雅停。
func (s *MemoryServer) Shutdown(_ context.Context) error { return nil }

// DLQ 取 dead-letter queue 内容 (debug / admin)。
func (s *MemoryServer) DLQ() []envelope { return s.q.dlq }

// ─── helpers ──────────────────────────────────────────────────────────

func hashHex(b []byte) string {
	// simplistic FNV-style; 不参与跨实例所以不用强 hash
	const seed = 14695981039346656037
	h := uint64(seed)
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return fmt.Sprintf("%016x", h)
}
