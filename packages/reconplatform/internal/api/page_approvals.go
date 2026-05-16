// page_approvals.go — /admin/approvals 4-eyes 审批工作流.
//
// 三 tab:待我审 / 我发起的 / 历史. 拒绝原因模板下拉.
package api

import "net/http"

func (s *Server) pageApprovals(w http.ResponseWriter, _ *http.Request) {
	body := `
<div x-data="approvalsModel()" x-init="load()" class="space-y-4">

  <!-- tabs -->
  <div class="flex items-center border-b border-slate-200">
    <template x-for="t in tabs" :key="t.value">
      <button @click="tab = t.value; load()"
              class="px-4 py-2 text-sm font-medium border-b-2 -mb-px transition"
              :class="tab === t.value
                ? 'border-brand-600 text-brand-700'
                : 'border-transparent text-slate-500 hover:text-slate-700'">
        <span x-text="t.label"></span>
        <span class="ml-1 text-xs opacity-75" x-text="'(' + (counts[t.value] || 0) + ')'"></span>
      </button>
    </template>
  </div>

  <!-- 列表 -->
  <div class="space-y-2">
    <template x-for="a in items" :key="a.id">
      <div class="bg-white rounded-lg border border-slate-200 p-4">
        <div class="flex items-start justify-between gap-4">
          <div class="flex-1">
            <div class="flex items-center gap-2 mb-1">
              <h3 class="text-sm font-semibold" x-text="a.title"></h3>
              <span class="badge"
                    :class="a.kind==='rule_change' ? 'badge-info' :
                             a.kind==='diff_dismiss' ? 'badge-warning' : 'badge-critical'"
                    x-text="a.kind"></span>
            </div>
            <div class="text-xs text-slate-500 mb-2">
              由 <span class="font-medium text-slate-700" x-text="a.requester"></span> 于
              <span x-text="a.created_at"></span> 发起
            </div>
            <p class="text-sm text-slate-700 mb-3" x-text="a.summary"></p>
            <div x-show="a.diff_summary" class="mono text-xs bg-slate-50 border border-slate-200 rounded p-2 mb-3"
                 x-text="a.diff_summary"></div>
          </div>
          <div class="flex flex-col gap-1.5 shrink-0" x-show="tab === 'pending'">
            <button @click="approve(a)" class="btn btn-primary text-xs">
              <i data-lucide="check" class="icon w-3 h-3"></i> Approve
            </button>
            <div x-data="{ open: false }" class="relative">
              <button @click="open = !open" class="btn btn-outline text-xs w-full">
                <i data-lucide="x" class="icon w-3 h-3"></i> Reject
              </button>
              <div x-show="open" @click.outside="open = false"
                   class="absolute right-0 mt-1 w-56 bg-white rounded shadow-lg border border-slate-200 z-10">
                <template x-for="t in rejectReasons" :key="t">
                  <button @click="reject(a, t); open = false"
                          class="block w-full text-left px-3 py-1.5 text-xs hover:bg-slate-50"
                          x-text="t"></button>
                </template>
              </div>
            </div>
          </div>
          <div class="text-xs" x-show="tab !== 'pending'">
            <span class="badge"
                  :class="a.state==='approved' ? 'badge-ok' :
                          a.state==='rejected' ? 'badge-critical' : 'badge-warning'"
                  x-text="a.state"></span>
          </div>
        </div>
      </div>
    </template>
    <div x-show="items.length === 0" class="text-center py-12 text-sm text-slate-400">
      <i data-lucide="inbox" class="w-8 h-8 mx-auto mb-2 opacity-50"></i>
      <div>暂无</div>
    </div>
  </div>
</div>
`
	script := `
<script>
function approvalsModel() {
  return {
    tab: 'pending',
    tabs: [
      { value: 'pending', label: '待我审' },
      { value: 'mine',    label: '我发起的' },
      { value: 'history', label: '历史' },
    ],
    counts: {},
    items: [],
    rejectReasons: [
      '风险评估不足',
      '理由描述不清',
      '需补充测试 fixture',
      '当前流量窗口风险高',
      '其他 (请补充评论)',
    ],
    async load() {
      const r = await fetch('/api/v1/approvals?tab=' + this.tab).then(r => r.json()).catch(() => []);
      this.items = Array.isArray(r) ? r : (r.items || []);
      this.counts = r.counts || {};
    },
    async approve(a) {
      await fetch('/api/v1/approvals/' + a.id, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ action: 'approve' }),
      });
      this.load();
    },
    async reject(a, reason) {
      await fetch('/api/v1/approvals/' + a.id, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ action: 'reject', reason }),
      });
      this.load();
    },
  };
}
</script>
`
	adminPage(w, "审批工作流", "approvals", body, "", script)
}
