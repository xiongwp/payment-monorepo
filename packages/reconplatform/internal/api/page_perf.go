// page_perf.go — /admin/perf 性能监控页 + /api/v1/perf/slow 端点 (REL-3).
//
// 端点:
//   GET /api/v1/perf/slow    返 matcher 慢日志快照 (SlowLogSnapshot JSON)
//   GET /admin/perf          HTML 页面
//
// 数据源:
//   matcher 进程周期 SET recon:perf:slowlog (60s TTL), admin 端 GET 出来透传给前端.
//   admin 与 matcher 不在同进程时也工作 (跨 pod 通过 Redis 共享).
package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"reconcile-system/internal/pipeline/matcher"
)

// perfSlow GET /api/v1/perf/slow.
//
// 没挂 redis → 503.
// Redis key 不存在 → 返空 snapshot (200, 让前端正常渲染空态).
func (s *Server) perfSlow(w http.ResponseWriter, r *http.Request) {
	if s.rdb == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("redis not configured"))
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	body, err := s.rdb.Get(ctx, matcher.SlowLogRedisKey).Bytes()
	if err != nil || len(body) == 0 {
		// 没数据返空快照, 200
		writeJSON(w, http.StatusOK, matcher.SlowLogSnapshot{
			ThresholdMS: 100,
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// pagePerf /admin/perf HTML.
func (s *Server) pagePerf(w http.ResponseWriter, _ *http.Request) {
	body := `
<div x-data="perfModel()" x-init="load(); setInterval(load, 10000)" class="space-y-6">

  <div class="flex items-center justify-between">
    <div>
      <h1 class="text-xl font-semibold text-slate-900">规则性能</h1>
      <p class="text-sm text-slate-500 mt-1">慢日志 / Top-N 慢规则. 每 10s 自动刷新.</p>
    </div>
    <button @click="load()" class="btn btn-outline text-xs">
      <i data-lucide="refresh-cw" class="icon"></i> 刷新
    </button>
  </div>

  <!-- Top-N -->
  <div class="stat-card">
    <h2 class="text-sm font-semibold text-slate-900 mb-3">
      Top 慢规则
      <span class="text-xs text-slate-400 font-normal" x-text="'按 P99 降排 · 共 ' + top.length + ' 条'"></span>
    </h2>
    <div class="overflow-x-auto">
      <table class="min-w-full text-sm">
        <thead class="text-xs text-slate-500 border-b border-slate-200">
          <tr>
            <th class="text-left py-2 pr-4">规则</th>
            <th class="text-right py-2 px-2">次数</th>
            <th class="text-right py-2 px-2">P50</th>
            <th class="text-right py-2 px-2">P99</th>
            <th class="text-right py-2 px-2">最慢</th>
            <th class="text-right py-2 pl-2">平均</th>
          </tr>
        </thead>
        <tbody>
          <template x-for="r in top" :key="r.rule">
            <tr class="border-b border-slate-100 hover:bg-slate-50">
              <td class="py-2 pr-4 mono text-xs" x-text="r.rule"></td>
              <td class="py-2 px-2 text-right text-slate-600" x-text="fmtNum(r.count)"></td>
              <td class="py-2 px-2 text-right" x-text="r.p50_ms + ' ms'"></td>
              <td class="py-2 px-2 text-right font-semibold"
                  :class="r.p99_ms > 500 ? 'text-red-600' : r.p99_ms > 100 ? 'text-amber-600' : 'text-slate-700'"
                  x-text="r.p99_ms + ' ms'"></td>
              <td class="py-2 px-2 text-right text-slate-600" x-text="r.slowest_ms + ' ms'"></td>
              <td class="py-2 pl-2 text-right text-slate-500" x-text="r.avg_ms + ' ms'"></td>
            </tr>
          </template>
          <tr x-show="top.length === 0">
            <td colspan="6" class="py-8 text-center text-slate-400 text-sm">
              暂无数据 · matcher 还没跑过规则,或 SlowLog 未启用
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>

  <!-- 最近慢样本 -->
  <div class="stat-card">
    <h2 class="text-sm font-semibold text-slate-900 mb-3">
      最近慢样本
      <span class="text-xs text-slate-400 font-normal"
            x-text="'阈值 ' + thresholdMS + ' ms · 共 ' + recent.length + ' 条 (环形缓冲)'"></span>
    </h2>
    <div class="space-y-2">
      <template x-for="(e, i) in recent" :key="i">
        <div class="flex items-center gap-3 text-sm border-l-2 pl-3 py-1"
             :class="e.duration_ms > 500 ? 'border-red-400' : 'border-amber-400'">
          <span class="mono text-xs text-slate-700" x-text="e.rule"></span>
          <span class="text-xs text-slate-400" x-text="e.trigger"></span>
          <span class="ml-auto text-xs"
                :class="e.duration_ms > 500 ? 'text-red-600 font-semibold' : 'text-amber-600'"
                x-text="e.duration_ms + ' ms'"></span>
          <span class="text-xs text-slate-400" x-text="e.event_count + ' events'"></span>
          <span class="badge" :class="badgeClass(e.verdict)" x-text="e.verdict"></span>
          <span class="text-xs text-slate-400" x-text="fmtTime(e.observed_at)"></span>
        </div>
      </template>
      <div x-show="recent.length === 0" class="text-center text-slate-400 text-sm py-4">
        没有超过阈值的慢样本
      </div>
    </div>
  </div>
</div>

<script>
function perfModel() {
  return {
    top: [],
    recent: [],
    thresholdMS: 100,
    async load() {
      try {
        const r = await fetch('/api/v1/perf/slow');
        if (!r.ok) return;
        const j = await r.json();
        this.top = j.top || [];
        this.recent = j.recent || [];
        this.thresholdMS = j.threshold_ms || 100;
        if (window.lucide) lucide.createIcons();
      } catch (e) {
        console.warn('perf load failed', e);
      }
    },
    fmtNum(n) { return (n || 0).toLocaleString(); },
    fmtTime(s) {
      try {
        const d = new Date(s);
        return d.toLocaleTimeString();
      } catch { return s; }
    },
    badgeClass(v) {
      switch (v) {
        case 'matched': return 'badge-ok';
        case 'mismatched': return 'badge-critical';
        case 'orphan': return 'badge-warning';
        case 'pending': return 'badge-info';
        case 'error': return 'bg-purple-100 text-purple-700';
        default: return 'bg-slate-100 text-slate-500';
      }
    },
  };
}
</script>
`
	adminPage(w, "规则性能", "perf", body, "", "")
}
