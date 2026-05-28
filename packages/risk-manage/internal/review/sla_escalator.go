// sla_escalator.go: 后台 goroutine 周期扫描超过 SLA 的 L1 case 自动升 L2。
//
// 动机：原 Workbench 已经能 GET /admin/review/overdue 看 overdue list，靠
// SRE / on-call 看 Slack alert 手动 escalate。问题：
//   - 夜班 / 周末没人盯 alert
//   - L1 同事请假，没人 release，case 死锁在 in_review
//   - 客诉 / regulator 投诉 → SLA 破窗后赔款
//
// 解决：每 5min 跑一次 AutoEscalateOverdue。条件：
//   - status ∈ {pending, in_review}    （decided 不动；escalated 已经在 L2 队列）
//   - level == 1                       （只升一次 L1→L2，不无限升）
//   - SLADeadline < now                （deadline 已过）
//   - SLAEscalated == false            （之前没自动升过，幂等）
//
// 触发后：level=2, status=escalated, assigned_to="" → 进 L2 队列等 senior claim。
// 同时 +1 metric risk_review_auto_escalated_total。
//
// 异常处理：goroutine panic 自动 recover；store error 写日志但不停 ticker。
// 启动失败（store==nil）不阻塞主服务（main 用 if escalator != nil 就 Start）。
//
// **TODO(chain audit)**: EscalateTo 走的是 store.EscalateTo 而非 store.Escalate；
// chain.go 没 wrap 新方法，所以 SLA 自动升级当前不写 tamper-evident chain。
// 跟监管承诺要补；v2 拆分时同步加 chainStore.EscalateTo / Transfer / DecideWithCode wrapper。
//
// **TODO(postgres migration)**: postgres_review.go 还没实现新方法 + 新列。
// 上 PG 时需要先 ALTER（安全幂等，老数据 level 默认 1）：
//
//	ALTER TABLE risk_review ADD COLUMN IF NOT EXISTS level INT NOT NULL DEFAULT 1;
//	ALTER TABLE risk_review ADD COLUMN IF NOT EXISTS reason_code TEXT;
//	ALTER TABLE risk_review ADD COLUMN IF NOT EXISTS sla_escalated BOOLEAN NOT NULL DEFAULT FALSE;
//	ALTER TABLE risk_review ADD COLUMN IF NOT EXISTS transfer_hist JSONB NOT NULL DEFAULT '[]';
//	ALTER TABLE risk_review ADD COLUMN IF NOT EXISTS escalate_hist JSONB NOT NULL DEFAULT '[]';
//	CREATE INDEX IF NOT EXISTS risk_review_level_sla ON risk_review (level, sla_deadline)
//	  WHERE status IN ('pending','in_review') AND sla_escalated = FALSE;
//
// 然后扩展 PGReviewStore 实现 Transfer / EscalateTo / DecideWithCode / AutoEscalateOverdue
// （UPDATE WHERE id=? AND level=expected 保证原子）。
package review

import (
	"context"
	"runtime/debug"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// AutoEscalatedTotal 自动升级计数。labels：trigger 复用 EscalateTrigger
// 字符串（目前只有 sla_timeout，但留扩展位给 future high_risk_score / 大额）。
//
// 用 sync.Once 注册避免多次启动 (test) 时 duplicate register panic。
var (
	autoEscalatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "risk_review_auto_escalated_total",
		Help: "Number of review cases auto-escalated by background workers (sla / risk threshold).",
	}, []string{"trigger"})
	autoEscalatedRegister sync.Once
)

func ensureMetricsRegistered() {
	autoEscalatedRegister.Do(func() {
		// MustRegister panic 时（同名已注册）忽略：测试场景多次 init 不致命。
		defer func() { _ = recover() }()
		prometheus.MustRegister(autoEscalatedTotal)
	})
}

// SLAEscalator 后台升级器配置。Interval <= 0 用默认 5min。
type SLAEscalator struct {
	Store    Store
	Interval time.Duration
	Logger   *zap.Logger
	// NowFunc 可注入用于测试（mock 时钟）。nil → time.Now().UTC()。
	NowFunc func() time.Time
}

// Start 起一个 goroutine 周期跑 AutoEscalateOverdue。
// 返回的 cancel 调一下即可优雅停（也可以直接 ctx cancel）。
//
// 失败兜底：store==nil 不启动，返 noop cancel；不 panic 阻塞主服务。
func (e *SLAEscalator) Start(ctx context.Context) context.CancelFunc {
	if e == nil || e.Store == nil {
		// 没 store 直接 noop（dev / 测试 / 配置 disable 时）
		if e != nil && e.Logger != nil {
			e.Logger.Info("sla_escalator disabled (no store)")
		}
		return func() {}
	}
	ensureMetricsRegistered()

	interval := e.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	now := e.NowFunc
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	logger := e.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	subCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("sla_escalator goroutine panicked",
					zap.Any("recover", rec),
					zap.String("stack", string(debug.Stack())))
			}
		}()
		t := time.NewTicker(interval)
		defer t.Stop()
		logger.Info("sla_escalator started",
			zap.Duration("interval", interval))
		// 立刻跑一次，不等第一个 tick（启动后 5min 死等不友好）。
		runOnce(subCtx, e.Store, now(), logger)
		for {
			select {
			case <-subCtx.Done():
				logger.Info("sla_escalator stopped")
				return
			case <-t.C:
				runOnce(subCtx, e.Store, now(), logger)
			}
		}
	}()
	return cancel
}

// runOnce 单次扫描 + 升级。错误只记日志不抛（goroutine 不该死）。
func runOnce(ctx context.Context, store Store, now time.Time, logger *zap.Logger) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Error("sla_escalator runOnce panicked",
				zap.Any("recover", rec))
		}
	}()
	n, err := store.AutoEscalateOverdue(ctx, now)
	if err != nil {
		logger.Warn("sla_escalator scan error", zap.Error(err))
		return
	}
	if n > 0 {
		autoEscalatedTotal.WithLabelValues(string(EscalateTriggerSLATimeout)).Add(float64(n))
		logger.Info("sla_escalator escalated cases",
			zap.Int("count", n),
			zap.Time("at", now))
	}
}

// StartDefaultSLAEscalator 一键启动：内嵌 ctx + 默认配置。
//
// main.go wire 示例（保持 5 行内不污染主流程）：
//
//	if reviewQ != nil {
//	    review.StartDefaultSLAEscalator(ctx, reviewQ, logger)
//	}
//
// 返 cancel；fx OnStop 里调一下停。
func StartDefaultSLAEscalator(ctx context.Context, store Store, logger *zap.Logger) context.CancelFunc {
	return (&SLAEscalator{
		Store:    store,
		Interval: 5 * time.Minute,
		Logger:   logger,
	}).Start(ctx)
}
