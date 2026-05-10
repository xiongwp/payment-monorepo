// status.go — CDC 各 source/runner 状态详情 enrichment。
//
// 现有 Manager.Stats() 已经返 []RunnerStat（service/running/last_file/last_pos/
// last_gtid）。本文件加 lag 字段计算 — 拉 Redis recon:cdc:last_event:<svc>
// （由 publisher.go 周期写）算 last_event 到 now 的距离。
//
// 输出（admin web /api/v1/cdc/status?detailed=1 用）：
//
//   {
//     "runners": [
//       {"service": "order-core", "running": true, "last_file": "...",
//        "last_pos": 1234, "lag_seconds": 0.12, ...}
//     ],
//     "summary": {"total": 30, "running": 28, "stopped": 2, "max_lag": 1.2}
//   }

package cdc

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// EnrichedStat 扩展 RunnerStat：加 lag / last_event_ts。
type EnrichedStat struct {
	RunnerStat
	LastEventTS  string  `json:"last_event_ts,omitempty"`
	LagSeconds   float64 `json:"lag_seconds,omitempty"`
}

// StatusSummary dashboard 头部 KPI。
type StatusSummary struct {
	Total         int     `json:"total"`
	Running       int     `json:"running"`
	Stopped       int     `json:"stopped"`
	MaxLagSeconds float64 `json:"max_lag_seconds"`
}

// EnrichedStatus 整个 manager 的 enriched 状态 + summary。
type EnrichedStatus struct {
	Runners []EnrichedStat `json:"runners"`
	Summary StatusSummary  `json:"summary"`
}

// EnrichStats 给 admin /api/v1/cdc/status?detailed=1 用。
//
// 拉 Redis last_event_ts 算 lag；Redis 不可达时仍返基础 stats（lag 字段缺）。
func (m *Manager) EnrichStats(ctx context.Context, rdb redis.UniversalClient) EnrichedStatus {
	base := m.Stats(ctx)
	out := EnrichedStatus{
		Runners: make([]EnrichedStat, 0, len(base)),
	}
	now := time.Now()
	for _, s := range base {
		es := EnrichedStat{RunnerStat: s}
		if rdb != nil {
			key := "recon:cdc:last_event:" + s.Service
			if v, err := rdb.Get(ctx, key).Result(); err == nil && v != "" {
				if ms, perr := strconv.ParseInt(v, 10, 64); perr == nil {
					t := time.UnixMilli(ms)
					es.LastEventTS = t.Format(time.RFC3339)
					es.LagSeconds = now.Sub(t).Seconds()
				}
			}
		}
		out.Runners = append(out.Runners, es)
		out.Summary.Total++
		if s.Running {
			out.Summary.Running++
		} else {
			out.Summary.Stopped++
		}
		if es.LagSeconds > out.Summary.MaxLagSeconds {
			out.Summary.MaxLagSeconds = es.LagSeconds
		}
	}
	return out
}
