// page_catalog.go — /admin/catalog 内置规则浏览.
//
// 按 severity 分组卡片 + 命中预览 + 一键 fork + 操作菜单 (查看 / 运行 / Fork / 删除).
//
// 数据源:
//   - GET /api/v1/scripts         拉所有已注册规则 (内置 + 用户)
//   - 内置规则按 name 前缀分类:
//       <pkg>:builtin:*          已内置 Go 规则
//       <pkg>:catalog:*          仓库 catalog/scripts 内的 .star 规则
//       <pkg>:user:*             用户编辑器创建的规则
package api

import "net/http"

func (s *Server) pageCatalog(w http.ResponseWriter, _ *http.Request) {
	body := `
<div x-data="catalogModel()" x-init="load()" class="space-y-6">

  <!-- 顶部:筛选 + 搜索 -->
  <div class="flex items-center justify-between gap-4 flex-wrap">
    <div class="flex items-center gap-2 flex-wrap">
      <template x-for="opt in filters" :key="opt.value">
        <button @click="filter = opt.value"
                class="px-3 py-1.5 text-sm rounded-md border"
                :class="filter === opt.value
                  ? 'bg-brand-600 text-white border-brand-600'
                  : 'bg-white text-slate-700 border-slate-300 hover:bg-slate-50'">
          <span x-text="opt.label"></span>
          <span class="ml-1 text-[10px] opacity-75" x-text="'(' + countBy(opt.value) + ')'"></span>
        </button>
      </template>
    </div>
    <div class="flex items-center gap-2">
      <div class="relative">
        <input type="search" x-model="q" placeholder="搜索规则名 / 描述 / 标签..."
               class="pl-9 pr-3 py-1.5 text-sm border border-slate-300 rounded-md w-64 focus:ring-2 focus:ring-brand-500 focus:border-brand-500">
        <i data-lucide="search" class="icon absolute left-3 top-1/2 -translate-y-1/2 text-slate-400"></i>
      </div>
      <a href="/admin/editor" class="btn btn-primary text-sm">
        <i data-lucide="plus" class="icon w-3 h-3"></i> 新建
      </a>
    </div>
  </div>

  <!-- FEAT-1: tag 过滤芯片 -->
  <div x-show="allTags.length > 0" class="flex items-center gap-2 flex-wrap">
    <span class="text-xs text-slate-500">标签:</span>
    <template x-for="tag in allTags" :key="tag">
      <button @click="toggleTag(tag)"
              class="text-xs px-2 py-1 rounded-full border transition"
              :class="selectedTags.includes(tag)
                ? 'bg-brand-100 border-brand-300 text-brand-700 font-medium'
                : 'bg-white border-slate-200 text-slate-600 hover:bg-slate-50'">
        <i data-lucide="tag" class="icon w-3 h-3 inline-block align-text-bottom mr-0.5"></i>
        <span x-text="tag"></span>
      </button>
    </template>
    <button x-show="selectedTags.length > 0" @click="selectedTags = []"
            class="text-xs text-slate-500 hover:text-slate-700 ml-1">
      <i data-lucide="x" class="icon w-3 h-3 inline-block"></i> 清除
    </button>
  </div>

  <!-- 调试信息: 直接显示数据是否加载 -->
  <div class="text-[10px] text-slate-400 px-1">
    debug: 已加载 <span class="mono font-bold" x-text="rules.length"></span> 条规则
    (critical: <span x-text="rules.filter(r=>r.severity==='critical').length"></span>,
     warning: <span x-text="rules.filter(r=>r.severity==='warning').length"></span>,
     info: <span x-text="rules.filter(r=>r.severity==='info').length"></span>)
  </div>

  <!-- 三组卡片: critical / warning / info -->
  <template x-for="grp in groups" :key="grp.severity">
    <section x-show="visibleInGroup(grp).length > 0">
      <div class="flex items-center gap-2 mb-3">
        <span class="badge"
              :class="grp.severity==='critical' ? 'badge-critical' :
                       grp.severity==='warning' ? 'badge-warning' : 'badge-info'"
              x-text="grp.severity.toUpperCase()"></span>
        <h2 class="text-sm font-semibold text-slate-900" x-text="grp.label"></h2>
        <span class="text-xs text-slate-500" x-text="'· ' + visibleInGroup(grp).length + ' 条'"></span>
      </div>
      <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
        <template x-for="r in visibleInGroup(grp)" :key="r.id">
          <div class="bg-white rounded-lg border border-slate-200 p-4 shadow-sm hover:shadow-md transition group relative">
            <!-- 卡片头 -->
            <div class="flex items-start justify-between mb-2">
              <h3 class="text-sm font-semibold text-slate-900 mono truncate" x-text="r.name"></h3>
              <span class="badge"
                    :class="r.lang === 'starlark' ? 'badge-info' : 'badge-ok'"
                    x-text="r.lang"></span>
            </div>
            <!-- 描述 -->
            <p class="text-xs text-slate-600 line-clamp-2 mb-2 min-h-[2rem]"
               x-text="r.description || '(暂无描述)'"></p>
            <!-- FEAT-1: tags 列 -->
            <div x-show="r.tags && r.tags.length > 0" class="flex flex-wrap gap-1 mb-2">
              <template x-for="tag in r.tags || []" :key="tag">
                <span @click.stop="toggleTag(tag)"
                      class="text-[10px] px-1.5 py-0.5 rounded border border-slate-200 bg-slate-50 text-slate-600 hover:bg-brand-50 hover:text-brand-700 cursor-pointer"
                      x-text="tag"></span>
              </template>
            </div>
            <!-- shadow 标 -->
            <div x-show="r.mode === 'shadow'" class="mb-2">
              <span class="badge bg-amber-100 text-amber-700 text-[10px]">
                <i data-lucide="eye-off" class="icon w-3 h-3 inline-block"></i> SHADOW
              </span>
            </div>
            <!-- 元信息 -->
            <div class="flex items-center justify-between text-xs text-slate-500 mb-3">
              <span class="flex items-center gap-1">
                <i data-lucide="zap" class="icon w-3 h-3"></i>
                <span x-text="(r.hits_24h || 0) + ' 24h 命中'"></span>
              </span>
              <span class="flex items-center gap-1">
                <i data-lucide="clock" class="icon w-3 h-3"></i>
                <span x-text="r.last_run || '从未运行'"></span>
              </span>
            </div>
            <!-- 操作按钮组 -->
            <div class="flex items-center gap-1.5 pt-2 border-t border-slate-100">
              <button @click="open(r.id)"
                      class="btn btn-outline text-xs flex-1">
                <i data-lucide="pen-line" class="icon w-3 h-3"></i> 编辑
              </button>
              <button @click="runNow(r)"
                      class="btn btn-outline text-xs flex-1">
                <i data-lucide="play" class="icon w-3 h-3"></i> 试运行
              </button>
              <div x-data="{ open: false }" class="relative">
                <button @click="open = !open" class="btn btn-outline text-xs px-2">
                  <i data-lucide="more-vertical" class="icon w-3 h-3"></i>
                </button>
                <div x-show="open" @click.outside="open = false"
                     class="absolute right-0 mt-1 w-36 bg-white rounded shadow-lg border border-slate-200 z-10 text-xs">
                  <button @click="fork(r); open = false" class="block w-full text-left px-3 py-1.5 hover:bg-slate-50">
                    <i data-lucide="git-fork" class="icon w-3 h-3 inline-block mr-1"></i> Fork
                  </button>
                  <button @click="exportCode(r); open = false" class="block w-full text-left px-3 py-1.5 hover:bg-slate-50">
                    <i data-lucide="download" class="icon w-3 h-3 inline-block mr-1"></i> 导出
                  </button>
                  <button @click="viewDiffs(r); open = false" class="block w-full text-left px-3 py-1.5 hover:bg-slate-50">
                    <i data-lucide="alert-triangle" class="icon w-3 h-3 inline-block mr-1"></i> 看 diffs
                  </button>
                  <hr class="my-0.5 border-slate-100">
                  <button @click="del(r); open = false" class="block w-full text-left px-3 py-1.5 text-red-600 hover:bg-red-50">
                    <i data-lucide="trash-2" class="icon w-3 h-3 inline-block mr-1"></i> 删除
                  </button>
                </div>
              </div>
            </div>
          </div>
        </template>
      </div>
    </section>
  </template>

  <div x-show="rules.length === 0" class="text-center py-12 text-sm text-slate-400">
    <i data-lucide="package-x" class="w-8 h-8 mx-auto mb-2 opacity-50"></i>
    <div>暂无规则。</div>
    <a href="/admin/editor" class="text-brand-600 hover:underline mt-2 inline-block">创建第一条 →</a>
  </div>
</div>
`

	script := `
<script>
function catalogModel() {
  return {
    rules: [],
    filter: 'all',
    q: '',
    selectedTags: [],
    filters: [
      { value: 'all',      label: '全部' },
      { value: 'critical', label: '严重' },
      { value: 'warning',  label: '警告' },
      { value: 'info',     label: '提示' },
      { value: 'starlark', label: 'Starlark' },
      { value: 'go',       label: 'Go 内建' },
    ],
    groups: [
      { severity: 'critical', label: '严重 — 资金安全 / 合规阻断' },
      { severity: 'warning',  label: '警告 — 业务异常需复核' },
      { severity: 'info',     label: '提示 — 长尾监控 / 容量' },
    ],

    async load() {
      try {
        const resp = await fetch('/api/v1/scripts').then(r => r.json());
        // API 返 {"scripts": [...]} 包装,兼容裸数组也行
        const list = Array.isArray(resp) ? resp
                    : Array.isArray(resp && resp.scripts) ? resp.scripts
                    : [];
        this.rules = list;
        this.rules.forEach(r => {
          // 字段大小写兼容 (id/ID, name/Name 等)
          r.id   = r.id   || r.ID   || '';
          r.name = r.name || r.Name || r.id;
          r.code = r.code || r.Code || '';
          // severity: 根据 ID 关键词推断 (Name 可能是中文,关键词在 ID 里)
          const idLower = (r.id || '').toLowerCase();
          if (!r.severity) {
            if (idLower.includes('three_way') || idLower.includes('excess') ||
                idLower.includes('duplicate') || idLower.includes('refund') ||
                idLower.includes('order_in_channel') || idLower.includes('channel_in_account')) {
              r.severity = 'critical';
            } else if (idLower.includes('orphan') || idLower.includes('lag') ||
                       idLower.includes('mismatch') || idLower.includes('stuck')) {
              r.severity = 'warning';
            } else {
              r.severity = 'info';
            }
          }
          // lang: 内建规则全是 starlark (list 接口不返 code,用其他线索)
          if (!r.lang) {
            const hasStarlarkCode = (r.code || '').includes('def check');
            const hasStarlarkSchedule = !!(r.schedule || r.Schedule);
            r.lang = (hasStarlarkCode || hasStarlarkSchedule || r.id) ? 'starlark' : 'go';
          }
          if (!r.description) {
            r.description = r.Description || '';
          }
          // FEAT-1 tags + UX-2 mode 字段, JSON tag 已配, 兼容 Capital fallback.
          r.tags = Array.isArray(r.tags) ? r.tags
                 : Array.isArray(r.Tags) ? r.Tags : [];
          r.mode = r.mode || r.Mode || 'live';
        });
      } catch (e) {
        console.error('[catalog] load failed:', e);
      }
    },

    countBy(filter) {
      if (filter === 'all') return this.rules.length;
      return this.rules.filter(r =>
        r.severity === filter || r.lang === filter).length;
    },

    visibleInGroup(grp) {
      const q = this.q.toLowerCase();
      const tags = this.selectedTags;
      return this.rules.filter(r => {
        if (r.severity !== grp.severity) return false;
        if (!(this.filter === 'all' || this.filter === grp.severity || r.lang === this.filter)) return false;
        // FEAT-1: tag 过滤 (AND, 选中的所有 tag 必须都有)
        if (tags.length > 0) {
          const rt = r.tags || [];
          for (const t of tags) {
            if (!rt.includes(t)) return false;
          }
        }
        if (q) {
          const hay = (r.name + ' ' + (r.description || '') + ' ' + (r.tags || []).join(' ')).toLowerCase();
          if (!hay.includes(q)) return false;
        }
        return true;
      });
    },

    // FEAT-1: 所有规则上出现过的 tag, 去重 + 排序.
    get allTags() {
      const set = new Set();
      for (const r of this.rules) {
        for (const t of (r.tags || [])) set.add(t);
      }
      return Array.from(set).sort();
    },
    toggleTag(tag) {
      const i = this.selectedTags.indexOf(tag);
      if (i >= 0) this.selectedTags.splice(i, 1);
      else this.selectedTags.push(tag);
    },

    open(id) { window.location = '/admin/editor?id=' + encodeURIComponent(id); },

    async runNow(r) {
      try {
        const resp = await fetch('/api/v1/scripts/' + r.id + '/run', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: '{}',
        });
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        const data = await resp.json();
        alert(r.name + ' 完成 — ' + (data.diffs_count || (data.diffs || []).length || 0) + ' diffs');
      } catch (e) { alert('试运行失败: ' + e); }
    },

    async fork(r) {
      const name = prompt('Fork 为新规则名 (snake_case):', r.name + '_copy');
      if (!name) return;
      try {
        await fetch('/api/v1/scripts', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            name, description: (r.description || '') + ' (forked from ' + r.name + ')',
            severity: r.severity, code: r.code,
          }),
        });
        this.load();
      } catch (e) { alert('Fork 失败: ' + e); }
    },

    exportCode(r) {
      const code = r.code || '';
      const blob = new Blob([code], { type: 'text/plain' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url; a.download = r.name + '.star';
      a.click();
      URL.revokeObjectURL(url);
    },

    viewDiffs(r) {
      window.location = '/admin/diffs?rule=' + encodeURIComponent(r.name);
    },

    async del(r) {
      if (!confirm('确认删除规则 ' + r.name + '?\n该操作需要 4-eyes 审批 (会发起待审批流).')) return;
      try {
        const resp = await fetch('/api/v1/scripts/' + r.id, { method: 'DELETE' });
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        this.load();
      } catch (e) { alert('删除失败: ' + e); }
    },
  };
}
</script>
`
	adminPage(w, "规则库", "catalog", body, "", script)
}
