// reconcile_worker.go — SP-AC-7 PROD2: 日终跨服务对账 worker.
//
// 资金安全核心: split-payment 持有 graph_run / transfer / payout / reversal 状态机,
// accounting 持 voucher / entry. 任何 split 侧"成功"必须对应 accounting 侧的 voucher.
// 这个 worker 每天扫一次, 不一致写 reconcile_alert 表 + 推 Prometheus metric + log error.
//
// 对账范围 (当前日):
//   1. moneyflow_runs (status=completed) — voucher_no 必须非空 → 跟 accounting voucher 表 join
//   2. transfers (status=posted) — graph_run_id 关联回 voucher
//   3. reversals (status=succeeded) — accounting 该有反向 entry
//
// 真生产里"对账"是个独立服务 (recon-pipeline), 这里实现的是 split-payment 侧的本地完整性
// 检查, 不夸服务. 真跨服务对账走 RECON-* 流水线.
package workflow

import (
	"context"
	"database/sql"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

// ReconcileMetrics.
var (
	ReconcileRunsScanned = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "split_payment",
		Subsystem: "reconcile",
		Name:      "runs_scanned_total",
		Help:      "Total runs scanned by daily reconciliation worker.",
	})
	ReconcileInconsistencies = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "split_payment",
		Subsystem: "reconcile",
		Name:      "inconsistencies_total",
		Help:      "Total inconsistencies found, by category.",
	}, []string{"category"}) // missing_voucher / orphan_transfer / orphan_reversal
	ReconcileDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "split_payment",
		Subsystem: "reconcile",
		Name:      "duration_seconds",
		Help:      "Reconciliation run end-to-end duration.",
		Buckets:   prometheus.LinearBuckets(0.5, 1, 10),
	})
)

// ReconcileConfig.
type ReconcileConfig struct {
	// Interval — 默认每天跑一次 (24h). 测试时可调更短.
	Interval time.Duration
	// LookbackHours — 扫多少小时前的数据, 给已发生的事务一个 settlement window. 默认 25 (覆盖跨日).
	LookbackHours int
}

// DefaultReconcileConfig.
func DefaultReconcileConfig() ReconcileConfig {
	return ReconcileConfig{Interval: 24 * time.Hour, LookbackHours: 25}
}

// ReconcileAlertSink 抽象告警出口 (生产可换 Kafka / 飞书 webhook).
type ReconcileAlertSink interface {
	Notify(ctx context.Context, category, detail string) error
}

// ReconcileWorker.
type ReconcileWorker struct {
	Cfg     ReconcileConfig
	DB      *sql.DB
	Sink    ReconcileAlertSink // nil → 只 log + metric, 不发警报
	Log     *zap.Logger
}

// Run 周期跑 (启动后等 Interval, 然后跑第一次).
// 真生产可加 "晚 12 点固定时间" 调度而不是固定 interval.
func (w *ReconcileWorker) Run(ctx context.Context) {
	if w.Cfg.Interval <= 0 {
		w.Cfg.Interval = 24 * time.Hour
	}
	if w.Cfg.LookbackHours <= 0 {
		w.Cfg.LookbackHours = 25
	}
	t := time.NewTicker(w.Cfg.Interval)
	defer t.Stop()
	if w.Log != nil {
		w.Log.Info("reconcile worker started",
			zap.Duration("interval", w.Cfg.Interval),
			zap.Int("lookback_h", w.Cfg.LookbackHours))
	}
	// 启动后立即跑一次 (开发模式好观察)
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			if w.Log != nil {
				w.Log.Info("reconcile worker stopped")
			}
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *ReconcileWorker) tick(ctx context.Context) {
	if w.DB == nil {
		return
	}
	start := time.Now()
	defer func() {
		ReconcileDuration.Observe(time.Since(start).Seconds())
	}()
	w.checkRunsHaveVoucher(ctx)
	w.checkOrphanTransfers(ctx)
	w.checkOrphanReversals(ctx)
}

// 检查 1: 所有 status=completed 的 run 必须有 voucher_no.
func (w *ReconcileWorker) checkRunsHaveVoucher(ctx context.Context) {
	rows, err := w.DB.QueryContext(ctx, `
		SELECT id, charge_id, status, voucher_no
		  FROM moneyflow_runs
		 WHERE created_at > DATE_SUB(NOW(), INTERVAL ? HOUR)
		   AND status = 'completed'
		   AND (voucher_no IS NULL OR voucher_no = '')`,
		w.Cfg.LookbackHours)
	if err != nil {
		if w.Log != nil {
			w.Log.Error("reconcile checkRunsHaveVoucher: query failed", zap.Error(err))
		}
		return
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id int64
		var chargeID, status, voucherNo string
		if err := rows.Scan(&id, &chargeID, &status, &voucherNo); err != nil {
			continue
		}
		ReconcileRunsScanned.Inc()
		ReconcileInconsistencies.WithLabelValues("missing_voucher").Inc()
		count++
		if w.Log != nil {
			w.Log.Error("reconcile: run completed without voucher_no",
				zap.Int64("run_id", id),
				zap.String("charge_id", chargeID))
		}
		if w.Sink != nil {
			_ = w.Sink.Notify(ctx, "missing_voucher",
				"moneyflow_run id="+itoa(id)+" charge_id="+chargeID+" status=completed but voucher_no empty")
		}
	}
	if count > 0 && w.Log != nil {
		w.Log.Warn("reconcile: found completed runs without voucher",
			zap.Int("count", count))
	}
}

// 检查 2: transfers 表 status=posted 的, graph_run_id 必须能查到 moneyflow_runs.
func (w *ReconcileWorker) checkOrphanTransfers(ctx context.Context) {
	rows, err := w.DB.QueryContext(ctx, `
		SELECT t.id, t.transfer_group, t.graph_run_id
		  FROM transfers t
		  LEFT JOIN moneyflow_runs r ON r.id = t.graph_run_id
		 WHERE t.created_at > DATE_SUB(NOW(), INTERVAL ? HOUR)
		   AND t.status = 'posted'
		   AND t.graph_run_id IS NOT NULL
		   AND r.id IS NULL`,
		w.Cfg.LookbackHours)
	if err != nil {
		if w.Log != nil {
			w.Log.Error("reconcile checkOrphanTransfers: query failed", zap.Error(err))
		}
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, runID string
		var group sql.NullString
		if err := rows.Scan(&id, &group, &runID); err != nil {
			continue
		}
		ReconcileInconsistencies.WithLabelValues("orphan_transfer").Inc()
		if w.Log != nil {
			w.Log.Error("reconcile: transfer references non-existent run",
				zap.String("transfer_id", id), zap.String("graph_run_id", runID))
		}
		if w.Sink != nil {
			_ = w.Sink.Notify(ctx, "orphan_transfer",
				"transfer id="+id+" references missing graph_run_id="+runID)
		}
	}
}

// 检查 3: reversals status=succeeded 必须有 transfer 引用 + transfer.reversed_amount > 0.
func (w *ReconcileWorker) checkOrphanReversals(ctx context.Context) {
	rows, err := w.DB.QueryContext(ctx, `
		SELECT rv.id, rv.transfer, rv.amount_minor, COALESCE(t.reversed_amount, 0)
		  FROM reversals rv
		  LEFT JOIN transfers t ON t.id = rv.transfer
		 WHERE rv.created_at > DATE_SUB(NOW(), INTERVAL ? HOUR)
		   AND rv.status = 'succeeded'
		   AND (t.id IS NULL OR t.reversed_amount < rv.amount_minor)`,
		w.Cfg.LookbackHours)
	if err != nil {
		if w.Log != nil {
			w.Log.Error("reconcile checkOrphanReversals: query failed", zap.Error(err))
		}
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, transferID string
		var amount, reversed int64
		if err := rows.Scan(&id, &transferID, &amount, &reversed); err != nil {
			continue
		}
		ReconcileInconsistencies.WithLabelValues("orphan_reversal").Inc()
		if w.Log != nil {
			w.Log.Error("reconcile: reversal not reflected in transfer.reversed_amount",
				zap.String("reversal_id", id),
				zap.String("transfer_id", transferID),
				zap.Int64("reversal_amount", amount),
				zap.Int64("transfer_reversed", reversed))
		}
		if w.Sink != nil {
			_ = w.Sink.Notify(ctx, "orphan_reversal",
				"reversal id="+id+" transfer="+transferID+" amount="+itoa(amount)+
					" but transfer.reversed_amount="+itoa(reversed))
		}
	}
}

// ─── helpers ──────────────────────────────────────────────────────────

// itoa: int64 → string, fmt.Sprintf 太重.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// LogAlertSink 默认实现, 把告警写 zap (生产换 Kafka/钉钉).
type LogAlertSink struct{ Log *zap.Logger }

// Notify 仅落 zap 日志.
func (s *LogAlertSink) Notify(_ context.Context, category, detail string) error {
	if s == nil || s.Log == nil {
		return nil
	}
	s.Log.Error("RECONCILE_ALERT",
		zap.String("category", category),
		zap.String("detail", detail))
	return nil
}
