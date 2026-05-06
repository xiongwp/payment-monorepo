// background.go：给 detached goroutine（cron / job / 渠道异步回调消费 / outbox
// 异步发送 / kafka consumer 等）提供"标准 ctx 起点"，强制注入：
//
//   - 全新的 trace_id（不继承 caller，因为 caller 可能已退出）
//   - 显式 shadow=false（**关键**：防止 detached goroutine 启动时 ctx 漂移
//     把 shadow=true 漏带到主表 / 真渠道）
//   - 带 trace_id 的 logger
//
// 使用模式（cron tick）：
//
//	for {
//	    select {
//	    case <-stop: return
//	    case <-ticker.C:
//	        ctx, cancel := trace.NewBackground(rootCtx, "outbox-publisher", logger, 30*time.Second)
//	        if err := tickOnce(ctx); err != nil {
//	            trace.Logger(ctx, logger).Error("tick failed", zap.Error(err))
//	        }
//	        cancel()
//	    }
//	}
//
// 渠道回调消费 (kafka consumer / 渠道 webhook handler 转 background)：
//
//	go func() {
//	    ctx, cancel := trace.NewBackground(rootCtx, "channel-callback", logger, 60*time.Second)
//	    defer cancel()
//	    handle(ctx, msg)
//	}()
//
// **不要**：
//
//	go func() { handle(context.Background()) }()  // 没 trace_id，没 shadow guard
//	go func() { handle(parentCtx) }()             // 父 ctx 取消会把 background 带挂；shadow 也会漂移
package trace

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/payment-util/shadow"
)

// NewBackground 构造一个 detached background ctx：
//   - 不继承 parent 的 cancel（parent ctx 取消不会传到这里），但保留其 Values（trace_id / logger）
//     **若 parent 没 trace_id**：自动 Generate 一个新的
//   - 显式 shadow=false（覆盖 parent 任何残留 shadow flag）
//   - 加 timeout（建议 ≥ 单次任务最坏延迟的 1.5 倍）
//   - 把 trace_id 注进 logger
//
// 返回的 cancel 必须 defer 调，避免 ctx leak。
//
// label 是任务名，会在生成的 trace_id 之外作为日志字段，便于运维查询。
func NewBackground(parent context.Context, label string, logger *zap.Logger, timeout time.Duration) (context.Context, context.CancelFunc) {
	// detached：不继承 parent.Done()。这是有意的：cron tick 的 parent 通常是
	// fx OnStop 触发的 ctx，task 应该跑完它的工作再优雅退出，而不是 mid-flight
	// 被截断。优雅停止由调用方在 timeout 上控制。
	ctx := detachContext(parent)

	// trace_id：父 ctx 有就续用，没有就新生成（典型情况：cron tick 启动）
	tid := FromContext(parent)
	if tid == "" {
		tid = Generate()
	}
	ctx = WithTraceID(ctx, tid)

	// **关键**：显式 shadow=false。绝不继承 parent 的 shadow flag，
	// background goroutine 永远跑主流量。需要跑 shadow 的 background 任务（如
	// shadow 重放）必须在调用方显式 shadow.WithShadow(ctx, true)。
	ctx = shadow.WithShadow(ctx, false)

	// logger 自动带 trace_id + label
	if logger != nil {
		l := logger.With(
			zap.String("trace_id", tid),
			zap.String("bg_task", label),
		)
		ctx = withLogger(ctx, l)
	}

	// timeout：≤ 0 时给 5 分钟兜底（防 goroutine 永远不退）
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, cancel
}

// NewBackgroundShadow 同 NewBackground 但**显式开启 shadow**。
// 仅用于 shadow 流量重放 / 影子压测的 background worker；普通业务请用 NewBackground。
//
// 调用方必须确保所有下游写路径（SQL / Redis / Kafka / 外部 webhook）都正确处理
// shadow=true 分支，否则会污染主流量。审查时关键词：grep "NewBackgroundShadow"。
func NewBackgroundShadow(parent context.Context, label string, logger *zap.Logger, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := NewBackground(parent, label, logger, timeout)
	ctx = shadow.WithShadow(ctx, true)
	if logger != nil {
		l := logger.With(zap.Bool("shadow", true))
		ctx = withLogger(ctx, l)
	}
	return ctx, cancel
}

// detachContext 返回一个保留 parent.Values 但不再继承 parent.Done() / Err() 的 ctx。
//
// 标准库没有 detach helper（context.WithoutCancel 是 Go 1.21+，monorepo 目前
// build 1.21+ 应该有，但这里为兼容显式包一层）。
func detachContext(parent context.Context) context.Context {
	if parent == nil {
		return context.Background()
	}
	// Go 1.21+ 内置 context.WithoutCancel；monorepo go.mod 已 1.21，可直接用。
	return context.WithoutCancel(parent)
}
