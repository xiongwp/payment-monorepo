// Package reconcile 修补 Authorize 路径的「卡组织已扣 / 本地未确认」差账。
//
// 背景：processor.Authorize 调卡组织发 HTTPS。RPC 失败 / ctx timeout 时，
// 卡组织端的状态可能是：
//
//	A. 没收到请求 → 安全，本地写 status=error 即可
//	B. 收到了，approved 中 → 钱可能扣了，但本地不知道
//	C. 收到了，已 approved，response 在网络上丢了 → 钱已扣
//
// B / C 不补 → 资金对不上账 + 用户可能再发一次 → 双扣。
//
// 本 worker 周期性扫 status ∈ {pending, error} 且 updated_at 老化的行，
// 调 network.Query(network_ref_no) 拿权威状态再 UpdateStatus。
//
// 关键纪律：
//   - 不接触 PAN（reconcile 只看 masked + network_ref_no）
//   - Query 是只读，对卡组织 idempotent，重复调用安全
//   - 多实例并发跑 OK，UpdateStatus 是 last-writer-wins 但 Query 是 deterministic
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/card-payment/internal/processor"
)

// Worker 周期扫卡死交易，调 network.Query 修订状态。
type Worker struct {
	repo     processor.CardTransactionRepo
	networks map[string]processor.Network
	logger   *zap.Logger

	// 单 cycle 单 shard 拉的最大行数。生产建议 20，dev 5。
	limit int
	// 多老的行才视为「卡死」。Authorize 默认 30s，留 2× 余量 → 60s。
	stuckAge time.Duration
	// 单笔 Query 给卡组织的 ctx timeout。卡组织 inquiry P99 一般 < 3s。
	queryTimeout time.Duration
	// 整个 cycle 的最长执行时间，避免 DB 慢查询拖整个 worker。
	cycleTimeout time.Duration

	// 监控用：上一次 cycle 修了多少行
	lastCorrected atomic.Int64
}

// Config 暴露给 fx 注入。零值有默认。
type Config struct {
	Limit        int
	StuckAge     time.Duration
	QueryTimeout time.Duration
	CycleTimeout time.Duration
}

// New 构造 worker；nil networks / nil repo 直接返 nil。
func New(repo processor.CardTransactionRepo, networks map[string]processor.Network, cfg Config, logger *zap.Logger) *Worker {
	if repo == nil || len(networks) == 0 {
		return nil
	}
	w := &Worker{
		repo:         repo,
		networks:     networks,
		logger:       logger,
		limit:        cfg.Limit,
		stuckAge:     cfg.StuckAge,
		queryTimeout: cfg.QueryTimeout,
		cycleTimeout: cfg.CycleTimeout,
	}
	if w.limit <= 0 {
		w.limit = 20
	}
	if w.stuckAge <= 0 {
		w.stuckAge = 60 * time.Second
	}
	if w.queryTimeout <= 0 {
		w.queryTimeout = 5 * time.Second
	}
	if w.cycleTimeout <= 0 {
		w.cycleTimeout = 30 * time.Second
	}
	return w
}

// Tick 跑一次 cycle，返回 (扫到行数, 修订行数, 错误)。
//
// 错误：DB / network adapter 错误会被吞到 logger，不向上层抛 —— worker
// 容错优先于「立刻报错」，毕竟下个 cycle 还会再扫。仅 ctx canceled 才返。
func (w *Worker) Tick(parent context.Context) (int, int, error) {
	cctx, cancel := context.WithTimeout(parent, w.cycleTimeout)
	defer cancel()
	rows, err := w.repo.ListStuck(cctx, w.stuckAge, w.limit)
	if err != nil {
		w.logger.Error("reconcile list stuck failed", zap.Error(err))
		return 0, 0, nil // 下 cycle 重试
	}
	corrected := 0
	for _, row := range rows {
		if cctx.Err() != nil {
			break
		}
		if w.handle(cctx, row) {
			corrected++
		}
	}
	w.lastCorrected.Store(int64(corrected))
	if len(rows) > 0 {
		w.logger.Info("reconcile cycle done",
			zap.Int("scanned", len(rows)),
			zap.Int("corrected", corrected))
	}
	return len(rows), corrected, nil
}

// handle 处理单行；返回 true 表示状态被改写。
func (w *Worker) handle(ctx context.Context, row *processor.CardTransaction) bool {
	if row == nil || row.Network == "" || row.NetworkRefNo == "" {
		// network_ref_no 为空 → 当时根本没发到卡组织，本地置 voided 安全
		if row != nil && row.NetworkRefNo == "" && row.Status != "voided" {
			// **资金安全前提**：必须能确认卡组织端没有任何状态。空 ref_no
			// 意味着 adapter.Authorize 在发 HTTPS 前就 error 了（dial fail
			// / ctx canceled before send），卡组织没记录。这里置 voided。
			//
			// adapter 实现要保证「ref_no 拿到才返回」—— 见 visa/auth.go
			// 等。如果实现没遵守这个契约，下面这个分支会误判，所以加双层
			// 校验：error + ref_no 空 才允许 voided，pending 不行。
			if row.Status == "error" {
				if err := w.tryUpdate(ctx, row.NetworkRefNo, "voided", "NO_NETWORK_REF"); err == nil {
					w.logger.Warn("reconcile: error+empty_ref → voided",
						zap.String("pi_id", row.PIID),
						zap.Int64("id", row.ID))
					return true
				}
			}
		}
		return false
	}
	adapter, ok := w.networks[row.Network]
	if !ok {
		w.logger.Error("reconcile: network adapter missing",
			zap.String("network", row.Network),
			zap.String("network_ref_no", row.NetworkRefNo))
		return false
	}
	qctx, cancel := context.WithTimeout(ctx, w.queryTimeout)
	defer cancel()
	resp, err := adapter.Query(qctx, &processor.NetworkQueryRequest{NetworkRefNo: row.NetworkRefNo})
	if err != nil {
		// Query 失败：常见 ctx timeout、卡组织维护、TLS 抖动。
		// 不改本地状态，下 cycle 再来 —— stuckAge 决定窗口。
		w.logger.Warn("reconcile query failed",
			zap.String("network", row.Network),
			zap.String("network_ref_no", row.NetworkRefNo),
			zap.Error(err))
		return false
	}
	if resp == nil || resp.Status == "" {
		return false
	}
	// 卡组织状态优先（权威）。
	canonical := canonicalStatus(resp.Status)
	if canonical == "" || canonical == row.Status {
		return false
	}
	if err := w.tryUpdate(ctx, row.NetworkRefNo, canonical, resp.DeclineCode); err != nil {
		w.logger.Error("reconcile update_status failed",
			zap.String("network_ref_no", row.NetworkRefNo),
			zap.String("from", row.Status),
			zap.String("to", canonical),
			zap.Error(err))
		return false
	}
	w.logger.Info("reconcile corrected",
		zap.String("pi_id", row.PIID),
		zap.String("network", row.Network),
		zap.String("network_ref_no", row.NetworkRefNo),
		zap.String("from", row.Status),
		zap.String("to", canonical))
	return true
}

func (w *Worker) tryUpdate(ctx context.Context, ref, status, code string) error {
	if ref == "" {
		// 没有 ref_no 时不能用 UpdateStatus（按 ref 定位），仅日志告警。
		// 实际生产建议加 UpdateByID(id) 接口，留 P2。
		return errors.New("update by id not implemented")
	}
	uctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return w.repo.UpdateStatus(uctx, ref, status, code)
}

// canonicalStatus 把卡组织各家不同的 status 字符串归一到本地 4 态：
// approved / declined / voided / pending。
//
// 各 adapter 实现自己的 Query 翻译层，理想情况下传过来已经是这 4 个之一；
// 这里再兜底一次，防止 adapter 漏映射。
func canonicalStatus(s string) string {
	switch s {
	case "approved", "captured", "settled", "success":
		return "approved"
	case "declined", "denied", "rejected":
		return "declined"
	case "voided", "reversed", "canceled", "cancelled":
		return "voided"
	case "pending", "in_progress", "processing":
		return "pending"
	}
	return ""
}

// LastCorrected 上一次 cycle 修订的行数（给 metrics / ops 用）。
func (w *Worker) LastCorrected() int64 { return w.lastCorrected.Load() }

// Run 启动后台 ticker；ctx 取消则退出。
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	w.logger.Info("reconcile worker started",
		zap.Duration("interval", interval),
		zap.Duration("stuck_age", w.stuckAge),
		zap.Int("limit_per_shard", w.limit))
	// 启动延迟 5s，避免冷启动直接打 DB
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("reconcile worker stopped", zap.Error(ctx.Err()))
			return
		case <-t.C:
			if _, _, err := w.Tick(ctx); err != nil {
				w.logger.Warn("reconcile tick err", zap.Error(err))
			}
		}
	}
}

// String 给 ops dump 用。
func (w *Worker) String() string {
	return fmt.Sprintf("reconcile{limit=%d stuck_age=%s query_timeout=%s}",
		w.limit, w.stuckAge, w.queryTimeout)
}
