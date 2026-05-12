// refresher.go — 定时刷新名单源.
//
// 每个 source 一个 Refresher; 启动后跑 Loop() — 立即 refresh 一次, 然后按 Interval 周期.
// 出错日志记录, 不打断 server 主流程.
//
// 调度间隔参考:
//   OFAC SDN   每日 (官方每天发)
//   EU cons    每日
//   UN SC      每周 (变动慢)
//   PEP        每周 (商用库一般 incremental)

package sources

import (
	"context"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/aml-screening/internal/domain"
	"reconcile-system/packages/aml-screening/internal/store"
)

// Refresher 单个源的刷新器
type Refresher struct {
	Source   domain.ListSource
	Interval time.Duration
	Fetch    func() ([]byte, error)
	Parse    func([]byte) ([]domain.ListEntry, error)
	Store    store.Store
	Log      *zap.Logger
}

// Loop 阻塞 — 调用方 go r.Loop(ctx).
// 第一次跑 RefreshOnce 立即获取; 之后按 Interval 周期.
func (r *Refresher) Loop(ctx context.Context) {
	// 第一次启动延迟 5s — 让 server 接入流量正常 healthz 后再去拉大文件
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}

	for {
		t0 := time.Now()
		n, err := r.RefreshOnce()
		dur := time.Since(t0)
		if err != nil {
			r.Log.Error("aml source refresh failed",
				zap.String("source", string(r.Source)),
				zap.Duration("duration", dur),
				zap.Error(err))
		} else {
			r.Log.Info("aml source refresh ok",
				zap.String("source", string(r.Source)),
				zap.Int("entries", n),
				zap.Duration("duration", dur))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.Interval):
		}
	}
}

// RefreshOnce 同步跑一次 fetch+parse+upsert.
func (r *Refresher) RefreshOnce() (int, error) {
	raw, err := r.Fetch()
	if err != nil {
		return 0, err
	}
	entries, err := r.Parse(raw)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if err := r.Store.UpsertEntry(e); err != nil {
			return 0, err
		}
	}
	// 删除本次同步前的过期条目 (这次没出现在源里 = OFAC 已经把它移除了)
	cutoff := time.Now().Add(-1 * time.Hour)
	_, _ = r.Store.PurgeStaleEntries(r.Source, cutoff)
	return len(entries), nil
}
