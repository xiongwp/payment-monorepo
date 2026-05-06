// Package routing — fallback.go：熔断/不可用时的备用渠道降级路由 + 异步重试队列。
//
// 设计：
//   1. 每个 (BIN/currency) 支持多个 adapter 优先级链（config-center key="payment-core/routing.fallback"）
//   2. 主路由 Route() 失败（circuit open / unavailable）时试 fallback；全部失败 → 置 processing 写 outbox
//   3. retry worker 按 [30s/2m/15m/2h] 退避异步重试
//   4. 复用 idempotency_key 防重复扣款
package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// FallbackConfig 备用渠道链配置。
// 从 config-center 的 "payment-core/routing.fallback" key 读取，格式：
//
//	{
//	  "rules": [
//	    {
//	      "country": "PH",
//	      "payment_method": "GCASH",
//	      "bin": "",
//	      "currency": "PHP",
//	      "priority": [
//	        {"adapter": "gcash", "weight": 100},
//	        {"adapter": "paymongo", "weight": 50}
//	      ]
//	    }
//	  ]
//	}
type FallbackConfig struct {
	Rules []FallbackRule `json:"rules"`
}

type FallbackRule struct {
	Country       string           `json:"country"`
	PaymentMethod string           `json:"payment_method"`
	BIN           string           `json:"bin"`          // 卡BIN，可空
	Currency      string           `json:"currency"`    // 币种，可空
	Priority      []FallbackAdapter `json:"priority"`   // 优先级链
}

type FallbackAdapter struct {
	Adapter string `json:"adapter"`
	Weight  int    `json:"weight"` // 权重，暂未用，留作后续灰度用
}

// FallbackRouter 备用渠道负责人。
type FallbackRouter struct {
	rules atomic.Pointer[[]FallbackRule]
	logger *zap.Logger
}

// NewFallbackRouter 初始化
func NewFallbackRouter(logger *zap.Logger) *FallbackRouter {
	fr := &FallbackRouter{logger: logger}
	fr.rules.Store(&[]FallbackRule{})
	return fr
}

// UpdateConfig 原子更新配置（config-center OnChange 回调用）
func (fr *FallbackRouter) UpdateConfig(raw []byte) error {
	var cfg FallbackConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("fallback config unmarshal: %w", err)
	}
	fr.rules.Store(&cfg.Rules)
	if fr.logger != nil {
		fr.logger.Info("fallback config updated",
			zap.Int("rules_count", len(cfg.Rules)))
	}
	return nil
}

// FallbackInput 备用渠道查询入参
type FallbackInput struct {
	Country       string
	PaymentMethod string
	BIN           string
	Currency      string
}

// GetFallbackChain 返回该笔交易的 fallback adapter 链
// 返回值：[primary, fallback1, fallback2, ...]
func (fr *FallbackRouter) GetFallbackChain(primary string, in FallbackInput) []string {
	if primary == "" {
		return nil
	}

	rules := fr.rules.Load()
	if rules == nil {
		return nil
	}

	// 匹配规则：country + payment_method 必须，bin/currency 非必需
	for _, r := range *rules {
		if r.Country != "" && r.Country != in.Country {
			continue
		}
		if r.PaymentMethod != "" && r.PaymentMethod != in.PaymentMethod {
			continue
		}
		if r.BIN != "" && r.BIN != in.BIN {
			continue
		}
		if r.Currency != "" && r.Currency != in.Currency {
			continue
		}

		// 找到规则，构造链
		chain := make([]string, 0, len(r.Priority)+1)
		chain = append(chain, primary)
		for _, fa := range r.Priority {
			if fa.Adapter != primary {
				chain = append(chain, fa.Adapter)
			}
		}
		return chain
	}
	return nil
}

// ─── 重试队列 ───────────────────────────────────────────────

// RetryTask outbox 表对应的重试任务
type RetryTask struct {
	ID            string            `db:"id" json:"id"`
	PaymentIntentID string          `db:"payment_intent_id" json:"payment_intent_id"`
	IdempotencyKey string          `db:"idempotency_key" json:"idempotency_key"`
	Amount        int64            `db:"amount" json:"amount"`
	Currency      string           `db:"currency" json:"currency"`
	PaymentMethod string           `db:"payment_method" json:"payment_method"`
	Country       string           `db:"country" json:"country"`
	BIN           string           `db:"bin" json:"bin"`
	FailedAdapter string           `db:"failed_adapter" json:"failed_adapter"` // 上次失败的 adapter
	Reason        string           `db:"reason" json:"reason"`               // 失败原因：circuit_open / unavailable
	Attempt       int              `db:"attempt" json:"attempt"`             // 重试次数（从 0 开始）
	NextRetryAt   time.Time        `db:"next_retry_at" json:"next_retry_at"`
	LastErrorMsg  string           `db:"last_error_msg" json:"last_error_msg"`
	Metadata      map[string]string `db:"metadata" json:"metadata"` // 透传的业务 metadata
	CreatedAt     time.Time        `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time        `db:"updated_at" json:"updated_at"`
}

// RetryScheduler 重试调度器：决定下次重试时间
type RetryScheduler struct{}

// Backoffs 退避策略：[30s, 2m, 15m, 2h]
var Backoffs = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	15 * time.Minute,
	2 * time.Hour,
}

// NextRetryTime 根据当前 attempt 计算下次重试时间
func (rs *RetryScheduler) NextRetryTime(attempt int) time.Time {
	backoff := Backoffs[len(Backoffs)-1] // 最后一个
	if attempt >= 0 && attempt < len(Backoffs) {
		backoff = Backoffs[attempt]
	}
	return time.Now().Add(backoff)
}

// RetryQueue 重试队列（生产环境用 DB outbox pattern；本实现为内存接口）
type RetryQueue interface {
	// Enqueue 入队一个重试任务
	Enqueue(ctx context.Context, task *RetryTask) error
	// Dequeue 取出即将重试的任务（next_retry_at <= now）
	Dequeue(ctx context.Context, limit int) ([]*RetryTask, error)
	// MarkRetry 标记一次重试（更新 attempt / next_retry_at / last_error_msg）
	MarkRetry(ctx context.Context, taskID string, attempt int, nextRetryAt time.Time, errorMsg string) error
	// MarkSuccess 标记成功，删除任务
	MarkSuccess(ctx context.Context, taskID string) error
}

// RetryWorker 后台 worker：定期从队列取任务重试
type RetryWorker struct {
	queue   RetryQueue
	router  *Router
	fallback *FallbackRouter
	client  interface{} // payment-channel client，实际类型由上游注入
	scheduler *RetryScheduler
	logger  *zap.Logger
	ticker  *time.Ticker
	stopCh  chan struct{}
}

// NewRetryWorker 创建后台 worker
func NewRetryWorker(
	queue RetryQueue,
	router *Router,
	fallback *FallbackRouter,
	logger *zap.Logger,
) *RetryWorker {
	return &RetryWorker{
		queue:     queue,
		router:    router,
		fallback:  fallback,
		scheduler: &RetryScheduler{},
		logger:    logger,
		ticker:    time.NewTicker(30 * time.Second), // 每 30s 轮一次队列
		stopCh:    make(chan struct{}),
	}
}

// Start 启动 worker，异步运行
func (rw *RetryWorker) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-rw.stopCh:
				return
			case <-rw.ticker.C:
				rw.pollAndRetry(ctx)
			}
		}
	}()
}

// Stop 优雅停止
func (rw *RetryWorker) Stop() {
	rw.ticker.Stop()
	close(rw.stopCh)
}

// pollAndRetry 轮询一批待重试任务并重试
func (rw *RetryWorker) pollAndRetry(ctx context.Context) {
	tasks, err := rw.queue.Dequeue(ctx, 100)
	if err != nil {
		if rw.logger != nil {
			rw.logger.Error("retry queue dequeue failed", zap.Error(err))
		}
		return
	}

	for _, task := range tasks {
		rw.retryTask(ctx, task)
	}
}

// retryTask 重试单个任务
// TODO: 实现具体的重试逻辑（调 payment-channel client）
func (rw *RetryWorker) retryTask(ctx context.Context, task *RetryTask) {
	// 1. 从 fallback config 取 chain（如果有的话）
	chain := rw.fallback.GetFallbackChain(task.FailedAdapter, FallbackInput{
		Country:       task.Country,
		PaymentMethod: task.PaymentMethod,
		BIN:           task.BIN,
		Currency:      task.Currency,
	})

	// 2. 尝试 chain 里的下一个 adapter（跳过已失败的）
	var nextAdapter string
	for _, adapter := range chain {
		if adapter != task.FailedAdapter {
			nextAdapter = adapter
			break
		}
	}

	// 3. 如果找不到 next adapter（无 fallback），标记失败回滚
	if nextAdapter == "" {
		if rw.logger != nil {
			rw.logger.Error("retry task exhausted: no fallback chain",
				zap.String("pi_id", task.PaymentIntentID),
				zap.String("adapter", task.FailedAdapter),
			)
		}
		// TODO: 调 order-core 回调告知交易失败
		_ = rw.queue.MarkSuccess(ctx, task.ID)
		return
	}

	// 4. 调 payment-channel Charge（用原 idempotency_key）
	// TODO: 这里需要实际调用 payment-channel client
	// resp, err := rw.client.Charge(ctx, ...)

	// 模拟逻辑示例（实际需注入真实 client）
	if rw.logger != nil {
		rw.logger.Info("retry task executing",
			zap.String("pi_id", task.PaymentIntentID),
			zap.String("from_adapter", task.FailedAdapter),
			zap.String("to_adapter", nextAdapter),
			zap.Int("attempt", task.Attempt),
		)
	}

	// 5. 重试逻辑
	// - 成功 → MarkSuccess 删队列
	// - 继续失败 → MarkRetry 更新下次时间
	_ = rw.queue.MarkRetry(ctx, task.ID, task.Attempt+1,
		rw.scheduler.NextRetryTime(task.Attempt+1),
		"retrying with fallback adapter")
}
