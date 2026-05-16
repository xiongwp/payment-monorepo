// page_audit.go — /admin/audit 审计日志页 (SEC-1).
//
// 列表展示最近 N 条 (默认 100) 写操作记录: actor / role / method / path / status / body.
// 只有 admin 能看 (后端 RBAC 已强制).
package api

import "net/http"

func (s *Server) pageAudit(w http.ResponseWriter, _ *http.Request) {
	body := `
<div x-data="auditModel()" x-init="load()" class="space-y-4">
  <div class="flex items-center justify-between">
    <div>
      <h1 class="text-xl font-semibold text-slate-900">审计日志</h1>
      <p class="text-sm text-slate-500 mt-1">最近 100 条写操作 (POST / PUT / DELETE)</p>
    </div>
    <div class="flex items-center gap-2">
      <input type="text" x-model="filter" placeholder="filter by path / actor"
             class="text-xs border border-slate-300 rounded px-2 py-1 w-56">
      <button @click="load()" class="btn btn-outline text-xs">
        <i data-lucide="refresh-cw" class="icon"></i> 刷新
      </button>
    </div>
  </div>

  <div class="stat-card">
    <div class="overflow-x-auto">
      <table class="min-w-full text-sm">
        <thead class="text-xs text-slate-500 border-b border-slate-200">
          <tr>
            <th class="text-left py-2 px-2">时间</th>
            <th class="text-left py-2 px-2">用户</th>
            <th class="text-left py-2 px-2">角色</th>
            <th class="text-left py-2 px-2">Method</th>
            <th class="text-left py-2 px-2">Path</th>
            <th class="text-right py-2 px-2">Status</th>
            <th class="text-right py-2 px-2">Body</th>
          </tr>
        </thead>
        <tbody>
          <template x-for="(e, i) in filtered" :key="i">
            <tr class="border-b border-slate-100 hover:bg-slate-50">
              <td class="py-2 px-2 mono text-xs text-slate-500" x-text="fmtTime(e.ts)"></td>
              <td class="py-2 px-2 mono text-xs" x-text="e.actor"></td>
              <td class="py-2 px-2">
                <span class="badge text-[10px]"
                      :class="e.role === 'admin' ? 'badge-critical' : e.role === 'editor' ? 'badge-info' : 'bg-slate-100 text-slate-600'"
                      x-text="e.role"></span>
              </td>
              <td class="py-2 px-2 mono text-xs"
                  :class="e.method === 'DELETE' ? 'text-red-600 font-semibold' : 'text-slate-700'"
                  x-text="e.method"></td>
              <td class="py-2 px-2 mono text-xs text-slate-700" x-text="e.path"></td>
              <td class="py-2 px-2 text-right">
                <span class="text-xs"
                      :class="e.status >= 400 ? 'text-red-600' : e.status >= 300 ? 'text-amber-600' : 'text-emerald-600'"
                      x-text="e.status"></span>
              </td>
              <td class="py-2 px-2 text-right text-xs text-slate-500" x-text="e.body_len + ' B'"></td>
            </tr>
          </template>
          <tr x-show="filtered.length === 0">
            <td colspan="7" class="py-8 text-center text-slate-400 text-sm">
              暂无审计记录
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</div>

<script>
function auditModel() {
  return {
    entries: [],
    filter: '',
    get filtered() {
      const f = (this.filter || '').toLowerCase();
      if (!f) return this.entries;
      return this.entries.filter(e =>
        (e.actor || '').toLowerCase().includes(f) ||
        (e.path || '').toLowerCase().includes(f) ||
        (e.method || '').toLowerCase().includes(f)
      );
    },
    async load() {
      try {
        const r = await fetch('/api/v1/admin/audit?limit=100');
        if (!r.ok) {
          this.entries = [];
          if (r.status === 403) {
            alert('权限不足: 仅 admin 角色可看审计日志');
          }
          return;
        }
        this.entries = await r.json();
        if (window.lucide) lucide.createIcons();
      } catch (e) { console.warn('audit load', e); }
    },
    fmtTime(s) {
      try { return new Date(s).toLocaleString(); } catch { return s; }
    },
  };
}
</script>
`
	adminPage(w, "审计日志", "audit", body, "", "")
}
