// page_trends.go — /admin/trends 24h diff 趋势 + sparkline (FEAT-3).
//
// 数据源:
//   publisher.Publish 每条 diff HINCRBY recon:trend:<rule_id> field=YYYYMMDDHH +=1.
//   本端聚合: 拉所有 rule 的 HASH → 取最近 24 个小时点 → 按 rule_id 返时间序列.
//
// 端点:
//   GET /api/v1/trends            → { "rules": { "<rule_id>": [{hour, count}, ...] }, "hours": [...] }
//   GET /admin/trends             HTML 页面
//
// 24h 整点列表 + 每条规则一条折线 (Chart.js).
package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// trendsAPI GET /api/v1/trends.
//
// 输出:
//   {
//     "hours": ["2026051300", "2026051301", ..., "2026051400"],
//     "rules": {
//       "rule_a": [ 0, 3, 12, ... ],   // 与 hours 同长度
//       "rule_b": [ 0, 0, 0, ... ],
//     }
//   }
func (s *Server) trendsAPI(w http.ResponseWriter, r *http.Request) {
	if s.rdb == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("redis not configured"))
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	hoursWindow := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 168 {
			hoursWindow = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	// 1) 列出所有 trend key (SCAN MATCH recon:trend:*)
	var ruleIDs []string
	cursor := uint64(0)
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, "recon:trend:*", 200).Result()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		for _, k := range keys {
			// 排除 alert ZSET key (recon:alert:diffs:*) 等;只取 trend:
			if len(k) > len("recon:trend:") {
				ruleIDs = append(ruleIDs, k[len("recon:trend:"):])
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	sort.Strings(ruleIDs)

	// 2) 计算 hours 列表 (UTC 整点, 从 oldest 到 newest)
	now := time.Now().UTC().Truncate(time.Hour)
	hours := make([]string, hoursWindow)
	for i := 0; i < hoursWindow; i++ {
		hours[i] = now.Add(-time.Duration(hoursWindow-1-i) * time.Hour).Format("2006010215")
	}

	// 3) 每个 rule HGETALL → 按 hours 序列填值
	rulesData := make(map[string][]int64, len(ruleIDs))
	for _, rid := range ruleIDs {
		m, err := s.rdb.HGetAll(ctx, "recon:trend:"+rid).Result()
		if err != nil {
			continue
		}
		series := make([]int64, hoursWindow)
		for i, h := range hours {
			if v, ok := m[h]; ok {
				n, _ := strconv.ParseInt(v, 10, 64)
				series[i] = n
			}
		}
		rulesData[rid] = series
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"hours": hours,
		"rules": rulesData,
	})
}

// pageTrends /admin/trends.
func (s *Server) pageTrends(w http.ResponseWriter, _ *http.Request) {
	body := `
<div x-data="trendsModel()" x-init="load()" class="space-y-6">
  <div class="flex items-center justify-between">
    <div>
      <h1 class="text-xl font-semibold text-slate-900">Diff 趋势</h1>
      <p class="text-sm text-slate-500 mt-1">最近 24 小时 (UTC) 各规则的 diff 命中数.</p>
    </div>
    <div class="flex items-center gap-2">
      <select x-model="window" @change="load()" class="text-xs border border-slate-300 rounded px-2 py-1">
        <option value="24">24h</option>
        <option value="48">48h</option>
        <option value="168">7d</option>
      </select>
      <button @click="load()" class="btn btn-outline text-xs">
        <i data-lucide="refresh-cw" class="icon"></i> 刷新
      </button>
    </div>
  </div>

  <!-- 总览折线 -->
  <div class="stat-card">
    <h2 class="text-sm font-semibold text-slate-900 mb-3">Top 10 规则趋势</h2>
    <canvas id="trendsChart" height="80"></canvas>
  </div>

  <!-- 每规则 sparkline 列表 -->
  <div class="stat-card">
    <h2 class="text-sm font-semibold text-slate-900 mb-3">
      规则详情
      <span class="text-xs text-slate-400 font-normal" x-text="'共 ' + ruleNames.length + ' 条'"></span>
    </h2>
    <div class="space-y-2">
      <template x-for="r in sorted" :key="r.name">
        <div class="flex items-center gap-3 py-1.5 border-b border-slate-100">
          <span class="mono text-xs flex-1 truncate" x-text="r.name"></span>
          <span class="text-xs text-slate-500" x-text="r.total + ' diffs / ' + window + 'h'"></span>
          <canvas :id="'sp_' + r.name" width="160" height="32" class="border border-slate-100 rounded"></canvas>
        </div>
      </template>
      <div x-show="ruleNames.length === 0" class="py-6 text-center text-sm text-slate-400">
        暂无 diff 数据
      </div>
    </div>
  </div>
</div>

<script>
let trendsChart = null;
function trendsModel() {
  return {
    hours: [], rules: {}, ruleNames: [], sorted: [],
    window: 24,
    async load() {
      try {
        const r = await fetch('/api/v1/trends?hours=' + this.window);
        const j = await r.json();
        this.hours = j.hours || [];
        this.rules = j.rules || {};
        this.ruleNames = Object.keys(this.rules);
        this.sorted = this.ruleNames
          .map(n => ({ name: n, total: (this.rules[n] || []).reduce((a,b)=>a+b, 0) }))
          .sort((a,b) => b.total - a.total);
        this.$nextTick(() => {
          this.drawMain();
          this.drawSparklines();
          if (window.lucide) lucide.createIcons();
        });
      } catch (e) { console.warn('trends load', e); }
    },
    drawMain() {
      const ctx = document.getElementById('trendsChart');
      if (!ctx || !window.Chart) return;
      const top10 = this.sorted.slice(0, 10).map(s => s.name);
      const palette = ['#ef4444','#f59e0b','#10b981','#3b82f6','#8b5cf6','#ec4899','#06b6d4','#84cc16','#f97316','#6366f1'];
      const datasets = top10.map((n, i) => ({
        label: n,
        data: this.rules[n],
        borderColor: palette[i % palette.length],
        backgroundColor: 'transparent',
        tension: 0.3,
        pointRadius: 1.5,
        borderWidth: 1.5,
      }));
      if (trendsChart) trendsChart.destroy();
      trendsChart = new Chart(ctx, {
        type: 'line',
        data: {
          labels: this.hours.map(h => h.slice(8) + 'h'),
          datasets,
        },
        options: {
          maintainAspectRatio: false,
          plugins: { legend: { position: 'bottom', labels: { boxWidth: 10, font: { size: 10 } } } },
          scales: { y: { beginAtZero: true, ticks: { font: { size: 10 } } },
                    x: { ticks: { font: { size: 10 }, autoSkip: true, maxTicksLimit: 12 } } },
        },
      });
    },
    drawSparklines() {
      for (const s of this.sorted) {
        const el = document.getElementById('sp_' + s.name);
        if (!el) continue;
        const c = el.getContext('2d');
        const data = this.rules[s.name] || [];
        const max = Math.max(1, ...data);
        c.clearRect(0, 0, el.width, el.height);
        c.strokeStyle = s.total > 0 ? '#3b82f6' : '#cbd5e1';
        c.beginPath();
        for (let i = 0; i < data.length; i++) {
          const x = (i / Math.max(1, data.length - 1)) * el.width;
          const y = el.height - (data[i] / max) * (el.height - 4) - 2;
          if (i === 0) c.moveTo(x, y);
          else c.lineTo(x, y);
        }
        c.stroke();
      }
    },
  };
}
</script>
`
	adminPage(w, "Diff 趋势", "trends", body, "", "")
}
