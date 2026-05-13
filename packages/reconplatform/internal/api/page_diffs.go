// page_diffs.go — /admin/diffs 差异列表 + 搜索.
//
// 这是 incident list 的简版聚合视图,按 day + rule 维度切片。
package api

import "net/http"

func (s *Server) pageDiffs(w http.ResponseWriter, _ *http.Request) {
	body := `
<div x-data="diffsModel()" x-init="load()" class="space-y-4">
  <div class="flex items-center justify-between gap-3">
    <div class="flex items-center gap-2">
      <input type="search" x-model="q" placeholder="key / rule_name / detail..."
             class="pl-3 pr-3 py-1.5 text-sm border border-slate-300 rounded-md w-72"
             @keyup.enter="load()">
      <select x-model="rule" class="text-sm border border-slate-300 rounded-md px-2 py-1.5">
        <option value="">所有规则</option>
        <template x-for="r in rules" :key="r"><option :value="r" x-text="r"></option></template>
      </select>
      <select x-model="severity" class="text-sm border border-slate-300 rounded-md px-2 py-1.5">
        <option value="">所有 severity</option>
        <option>critical</option><option>warning</option><option>info</option>
      </select>
      <button @click="load()" class="btn btn-outline text-sm">
        <i data-lucide="refresh-cw" class="icon"></i> 应用
      </button>
    </div>
    <div class="text-xs text-slate-500" x-text="'共 ' + items.length + ' 条'"></div>
  </div>

  <!-- 按天分组的小柱状 -->
  <div class="stat-card">
    <h3 class="text-sm font-semibold mb-2">按日分布</h3>
    <canvas id="byDay" height="40"></canvas>
  </div>

  <!-- 列表 -->
  <div class="bg-white rounded-lg border border-slate-200 overflow-hidden">
    <table class="w-full">
      <thead class="bg-slate-50">
        <tr>
          <th class="table-th">ID</th>
          <th class="table-th">Rule</th>
          <th class="table-th">Key</th>
          <th class="table-th">Severity</th>
          <th class="table-th">State</th>
          <th class="table-th">Created</th>
        </tr>
      </thead>
      <tbody class="divide-y divide-slate-100">
        <template x-for="d in items" :key="d.id">
          <tr class="hover:bg-slate-50 cursor-pointer" @click="open(d.id)">
            <td class="table-td mono text-xs" x-text="d.id"></td>
            <td class="table-td font-medium" x-text="d.rule_name"></td>
            <td class="table-td mono text-xs text-slate-500" x-text="d.key"></td>
            <td class="table-td">
              <span class="badge"
                    :class="d.severity==='critical' ? 'badge-critical' :
                            d.severity==='warning' ? 'badge-warning' : 'badge-info'"
                    x-text="d.severity || 'info'"></span>
            </td>
            <td class="table-td">
              <span class="badge"
                    :class="d.state==='RESOLVED' ? 'badge-ok' :
                            d.state==='NEW' ? 'badge-critical' : 'badge-warning'"
                    x-text="d.state || 'NEW'"></span>
            </td>
            <td class="table-td text-xs text-slate-500" x-text="d.created_at"></td>
          </tr>
        </template>
        <tr x-show="items.length===0">
          <td colspan="6" class="table-td text-center text-slate-400 py-8">暂无 diff</td>
        </tr>
      </tbody>
    </table>
  </div>
</div>
`
	script := `
<script>
function diffsModel() {
  return {
    items: [], rules: [], q: '', rule: '', severity: '',
    async load() {
      const params = new URLSearchParams();
      if (this.q)        params.set('q', this.q);
      if (this.rule)     params.set('rule', this.rule);
      if (this.severity) params.set('severity', this.severity);
      params.set('limit', '200');
      const r = await fetch('/api/v1/diffs?' + params.toString()).then(r => r.json()).catch(() => []);
      this.items = Array.isArray(r) ? r : [];
      if (this.rules.length === 0) {
        const rs = await fetch('/api/v1/scripts').then(r => r.json()).catch(() => []);
        this.rules = Array.isArray(rs) ? rs.map(s => s.name) : [];
      }
      this.renderByDay();
    },
    open(id) { window.location = '/admin/incidents/' + encodeURIComponent(id); },
    renderByDay() {
      // 按天聚合
      const by = {};
      for (const d of this.items) {
        const day = (d.created_at || '').slice(0, 10) || 'unknown';
        by[day] = (by[day] || 0) + 1;
      }
      const labels = Object.keys(by).sort();
      const data = labels.map(l => by[l]);
      const el = document.getElementById('byDay');
      if (!el) return;
      if (this._chart) this._chart.destroy();
      this._chart = new Chart(el, {
        type: 'bar',
        data: { labels, datasets: [{ data, backgroundColor: '#3b82f6', borderRadius: 2 }] },
        options: { plugins: { legend: { display: false } },
                   scales: { y: { beginAtZero: true } } },
      });
    },
  };
}
</script>
`
	adminPage(w, "差异列表", "diffs", body, "", script)
}
