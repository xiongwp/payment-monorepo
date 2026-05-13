// admin_layout.go — 新一代 admin web 的共享 layout.
//
// 旧 SPA (editor_html.go, 1627 行单文件) 改造为多页应用:
//
//	/admin                  → 重定向到 /admin/dashboard
//	/admin/dashboard        → KPI 总览
//	/admin/catalog          → 内置规则浏览
//	/admin/editor           → 脚本编辑器 (兼容旧 URL)
//	/admin/diffs            → diff 列表 + 搜索
//	/admin/incidents/{id}   → 单笔 diff 全景
//	/admin/approvals        → 4-eyes 审批工作流
//
// 技术栈:
//   - Tailwind Play CDN (浏览器实时编译,无需 build chain)
//   - Alpine.js (轻量响应式,3 KB)
//   - Lucide icons (CDN, svg sprite)
//   - Chart.js (沿用)
//
// 设计:
//   - 共用 layout: 顶 nav + 侧 sidebar + main 内容区
//   - 一致的色板 (slate-50 底 / blue-600 强调 / 风险用 amber/red/green)
//   - Inter 字体
package api

import (
	"net/http"
	"strings"
)

// adminPage 渲染一个完整页面 (layout + 内容).
//
// title:浏览器 tab + 顶 nav.
// currentNav:高亮的导航项 (e.g. "dashboard", "catalog"...)
// bodyHTML:主内容区 HTML 片段 (不含 <body>).
// extraHead:页面专属 head (e.g. Cytoscape lib),可空.
// extraScript:页面专属 inline JS,可空.
func adminPage(w http.ResponseWriter, title, currentNav, bodyHTML, extraHead, extraScript string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	page := `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + title + ` · reconplatform</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
<script src="https://cdn.tailwindcss.com"></script>
<script>
  tailwind.config = {
    theme: {
      extend: {
        fontFamily: {
          sans: ['Inter', 'system-ui', 'sans-serif'],
          mono: ['JetBrains Mono', 'Menlo', 'monospace'],
        },
        colors: {
          brand: { 50: '#eff6ff', 500: '#3b82f6', 600: '#2563eb', 700: '#1d4ed8' },
        },
      }
    }
  }
</script>
<script defer src="https://unpkg.com/alpinejs@3.x.x/dist/cdn.min.js"></script>
<script src="https://unpkg.com/lucide@latest/dist/umd/lucide.js"></script>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js"></script>
<style>
  body { font-family: 'Inter', system-ui, sans-serif; -webkit-font-smoothing: antialiased; }
  code, pre, .mono { font-family: 'JetBrains Mono', Menlo, monospace; }
  .nav-link { @apply flex items-center gap-2 px-3 py-2 text-sm rounded-md text-slate-600 hover:bg-slate-100 hover:text-slate-900 transition; }
  .nav-link.active { @apply bg-brand-50 text-brand-700 font-medium; }
  .stat-card { @apply bg-white rounded-lg border border-slate-200 p-4 shadow-sm; }
  .badge { @apply inline-flex items-center gap-1 px-2 py-0.5 rounded text-xs font-medium; }
  .badge-critical { @apply bg-red-100 text-red-700; }
  .badge-warning  { @apply bg-amber-100 text-amber-700; }
  .badge-info     { @apply bg-blue-100 text-blue-700; }
  .badge-ok       { @apply bg-emerald-100 text-emerald-700; }
  .table-th { @apply text-left text-xs font-medium text-slate-500 uppercase tracking-wider px-4 py-2; }
  .table-td { @apply px-4 py-3 text-sm text-slate-700; }
  .btn        { @apply inline-flex items-center gap-1.5 px-3 py-1.5 text-sm font-medium rounded-md border transition; }
  .btn-primary{ @apply bg-brand-600 text-white border-brand-600 hover:bg-brand-700; }
  .btn-ghost  { @apply text-slate-700 border-transparent hover:bg-slate-100; }
  .btn-outline{ @apply bg-white text-slate-700 border-slate-300 hover:bg-slate-50; }
  .icon { @apply w-4 h-4 stroke-current; }
</style>
` + extraHead + `
</head>
<body class="bg-slate-50 text-slate-900 min-h-screen">

<div class="flex h-screen">
  ` + sidebarHTML(currentNav) + `

  <!-- main column -->
  <div class="flex-1 flex flex-col overflow-hidden">
    ` + topbarHTML(title) + `

    <main class="flex-1 overflow-auto p-6">
      ` + bodyHTML + `
    </main>
  </div>
</div>

<script>
  // 初始化 Lucide icons
  if (window.lucide) lucide.createIcons();
</script>
` + extraScript + `

</body>
</html>`

	_, _ = w.Write([]byte(page))
}

// sidebarHTML 左侧导航.
func sidebarHTML(current string) string {
	link := func(href, label, icon, key string) string {
		cls := "nav-link"
		if key == current {
			cls += " active"
		}
		return `<a href="` + href + `" class="` + cls + `">
            <i data-lucide="` + icon + `" class="icon"></i>
            <span>` + label + `</span>
          </a>`
	}
	return `<aside class="w-56 bg-white border-r border-slate-200 flex flex-col">
  <div class="h-14 flex items-center px-4 border-b border-slate-200">
    <div class="flex items-center gap-2">
      <div class="w-7 h-7 rounded-lg bg-gradient-to-br from-brand-500 to-brand-700 flex items-center justify-center">
        <i data-lucide="git-compare" class="w-4 h-4 text-white"></i>
      </div>
      <div>
        <div class="text-sm font-semibold leading-tight">reconplatform</div>
        <div class="text-[10px] text-slate-500 leading-tight">对账中台</div>
      </div>
    </div>
  </div>
  <nav class="flex-1 px-2 py-3 space-y-0.5">
    ` + link("/admin/dashboard", "总览", "layout-dashboard", "dashboard") + `
    ` + link("/admin/events", "实时事件", "activity", "events") + `
    ` + link("/admin/diffs", "差异", "alert-triangle", "diffs") + `
    ` + link("/admin/incidents", "事件复盘", "search-code", "incidents") + `
    ` + link("/admin/catalog", "规则库", "library", "catalog") + `
    ` + link("/admin/editor", "编辑器", "code", "editor") + `
    ` + link("/admin/approvals", "审批", "shield-check", "approvals") + `
  </nav>
  <div class="px-3 py-3 border-t border-slate-200 text-xs text-slate-500">
    <div>v2 · 多页架构</div>
    <div class="mt-1 text-[10px]">© reconplatform</div>
  </div>
</aside>`
}

// topbarHTML 顶部导航条 (含全局 + 新建规则 + 新建 fixture 操作).
func topbarHTML(title string) string {
	return `<header class="h-14 bg-white border-b border-slate-200 flex items-center px-6 gap-4 shrink-0"
        x-data="topbarActions()">
  <h1 class="text-base font-semibold text-slate-900">` + title + `</h1>
  <div class="flex-1"></div>

  <!-- 全局操作: + 新建 -->
  <button @click="openNewRule = true" class="btn btn-primary text-xs">
    <i data-lucide="plus" class="icon w-3 h-3"></i> 新建规则
  </button>

  <div class="text-xs text-slate-500 flex items-center gap-3">
    <span class="flex items-center gap-1">
      <span class="w-2 h-2 rounded-full bg-emerald-500"></span>
      <span>online</span>
    </span>
    <a href="/healthz" target="_blank" class="text-slate-400 hover:text-slate-600">health</a>
  </div>

  <!-- "+ 新建规则" modal -->
  <div x-show="openNewRule" x-cloak class="fixed inset-0 bg-black/40 z-50 flex items-center justify-center p-4"
       @click="openNewRule = false">
    <div class="bg-white rounded-lg shadow-xl max-w-md w-full" @click.stop>
      <div class="px-5 py-3 border-b border-slate-200 flex items-center justify-between">
        <h2 class="text-sm font-semibold">新建对账规则</h2>
        <button @click="openNewRule = false" class="text-slate-400 hover:text-slate-600">
          <i data-lucide="x" class="icon"></i>
        </button>
      </div>
      <form @submit.prevent="createRule()" class="p-5 space-y-3">
        <div>
          <label class="block text-xs font-medium text-slate-600 mb-1">规则名 *</label>
          <input type="text" x-model="newRule.name" required pattern="[a-z][a-z0-9_]*"
                 placeholder="snake_case_only"
                 class="w-full text-sm border border-slate-300 rounded px-2 py-1.5 mono focus:ring-2 focus:ring-brand-500 focus:border-brand-500">
          <p class="text-[10px] text-slate-500 mt-1">仅小写字母 + 数字 + 下划线;创建后不可改</p>
        </div>
        <div>
          <label class="block text-xs font-medium text-slate-600 mb-1">严重程度</label>
          <select x-model="newRule.severity" class="w-full text-sm border border-slate-300 rounded px-2 py-1.5">
            <option value="info">info — 长尾监控</option>
            <option value="warning">warning — 业务异常</option>
            <option value="critical">critical — 资金 / 合规</option>
          </select>
        </div>
        <div>
          <label class="block text-xs font-medium text-slate-600 mb-1">语言</label>
          <div class="flex gap-2">
            <label class="flex items-center gap-1 text-sm cursor-pointer">
              <input type="radio" x-model="newRule.lang" value="starlark">
              <span>Starlark (动态热更新)</span>
            </label>
            <label class="flex items-center gap-1 text-sm cursor-pointer">
              <input type="radio" x-model="newRule.lang" value="go" disabled class="opacity-50">
              <span class="opacity-50">Go (需 PR + 重启)</span>
            </label>
          </div>
        </div>
        <div>
          <label class="block text-xs font-medium text-slate-600 mb-1">起始模板</label>
          <select x-model="newRule.template" class="w-full text-sm border border-slate-300 rounded px-2 py-1.5">
            <option value="empty">空白</option>
            <option value="duplicate">重复检测 (idempotency)</option>
            <option value="amount_equality">跨服务金额相等</option>
            <option value="missing_leg">跨服务存在性</option>
            <option value="timing_lag">时间窗 / lag</option>
          </select>
        </div>
        <div>
          <label class="block text-xs font-medium text-slate-600 mb-1">一句话描述 (可选)</label>
          <input type="text" x-model="newRule.description"
                 class="w-full text-sm border border-slate-300 rounded px-2 py-1.5">
        </div>
        <div class="flex items-center justify-end gap-2 pt-2">
          <button type="button" @click="openNewRule = false" class="btn btn-outline text-sm">取消</button>
          <button type="submit" class="btn btn-primary text-sm">
            <i data-lucide="check" class="icon w-3 h-3"></i> 创建 + 打开编辑器
          </button>
        </div>
      </form>
    </div>
  </div>
</header>
<style>[x-cloak]{display:none!important}</style>
<script>
function topbarActions() {
  return {
    openNewRule: false,
    newRule: { name: '', severity: 'warning', lang: 'starlark', template: 'empty', description: '' },
    async createRule() {
      const tpls = {
        empty: '# ' + this.newRule.name + '\n# ' + this.newRule.description + '\n\n' +
               'def check(ctx):\n    diffs = []\n    # TODO\n    return diffs\n',
        duplicate:
'# ' + this.newRule.name + ' — 同 idempotency_key 多笔 charge 检测.\n' +
'\n' +
'def check(ctx):\n' +
'    diffs = []\n' +
'    seen = {}\n' +
'    for r in ctx.scan("payment-channel", "acquirer_tx", 1000):\n' +
'        idk = r.after.get("idempotency_key", "")\n' +
'        if not idk: continue\n' +
'        if idk in seen:\n' +
'            seen[idk].append(r.pk)\n' +
'        else:\n' +
'            seen[idk] = [r.pk]\n' +
'    for k, ids in seen.items():\n' +
'        if len(ids) > 1:\n' +
'            diffs.append({"type":"' + this.newRule.name + '","key":k,"detail":{"charge_ids":ids,"count":len(ids)}})\n' +
'    return diffs\n',
        amount_equality:
'# ' + this.newRule.name + ' — 跨服务金额相等.\n' +
'\n' +
'def check(ctx):\n' +
'    diffs = []\n' +
'    for pi in ctx.scan("order-core", "payment_intent", 1000):\n' +
'        related = ctx.get_by_index("pi_id", pi.pk)\n' +
'        tx = related.find("payment-channel", "acquirer_tx")\n' +
'        if not tx or tx.after.get("amount") != pi.after.get("amount"):\n' +
'            diffs.append({"type":"' + this.newRule.name + '","key":pi.pk,\n' +
'                          "want":pi.after.get("amount"),\n' +
'                          "got":(tx and tx.after.get("amount")) or None})\n' +
'    return diffs\n',
        missing_leg:
'# ' + this.newRule.name + ' — 跨服务存在性.\n' +
'\n' +
'def check(ctx):\n' +
'    diffs = []\n' +
'    EXPECTED = ["order-core", "payment-channel", "accounting-system"]\n' +
'    for pi in ctx.scan("order-core", "payment_intent", 1000):\n' +
'        if pi.after.get("status") != "succeeded": continue\n' +
'        related = ctx.get_by_index("pi_id", pi.pk)\n' +
'        present = set(e.svc for e in related)\n' +
'        missing = [s for s in EXPECTED if s not in present]\n' +
'        if missing:\n' +
'            diffs.append({"type":"' + this.newRule.name + '","key":pi.pk,"detail":{"missing":missing}})\n' +
'    return diffs\n',
        timing_lag:
'# ' + this.newRule.name + ' — 时间窗 / lag 检测.\n' +
'\n' +
'def check(ctx):\n' +
'    diffs = []\n' +
'    threshold_sec = int(ctx.params.get("threshold_sec", "300"))\n' +
'    # TODO: 对比相关事件 timestamp,超阈值则记 diff\n' +
'    return diffs\n',
      };
      const code = tpls[this.newRule.template] || tpls.empty;
      try {
        const r = await fetch('/api/v1/scripts', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            name: this.newRule.name,
            severity: this.newRule.severity,
            description: this.newRule.description,
            code,
          }),
        });
        if (!r.ok) { alert('创建失败: HTTP ' + r.status); return; }
        const data = await r.json().catch(() => ({}));
        const id = data.id || this.newRule.name;
        location.href = '/admin/editor?id=' + encodeURIComponent(id);
      } catch (e) { alert('创建失败: ' + e); }
    },
  };
}
</script>`
}

// adminIndexRedirect 默认 /admin 跳转到 dashboard.
func (s *Server) adminIndexRedirect(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSuffix(r.URL.Path, "/") == "/admin" || r.URL.Path == "/" {
		http.Redirect(w, r, "/admin/dashboard", http.StatusSeeOther)
		return
	}
	http.NotFound(w, r)
}
