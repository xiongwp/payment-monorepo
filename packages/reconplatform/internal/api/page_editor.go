// page_editor.go — /admin/editor 升级版规则编辑器.
//
// 左侧栏 4 段 (Tab 切换,不用 details 折叠以免布局塌陷):
//   - 大纲 outline (代码符号)
//   - 数据库 schema (auto-load + fallback 默认表)
//   - 版本历史
//   - 实时事件 mini (last 10 SSE events)
//
// 中:Monaco editor + 顶 toolbar (lint / dry-run / save / more menu)
// 右:运行预览 + 实时活动 mini (合并)
package api

import "net/http"

func (s *Server) pageEditor(w http.ResponseWriter, r *http.Request) {
	scriptID := r.URL.Query().Get("id")
	body := `
<div x-data="editorModel('` + scriptID + `')" x-init="load()" class="grid grid-cols-12 gap-4 h-[calc(100vh-7rem)]">

  <!-- 左栏: 4 个 Tab -->
  <aside class="col-span-3 bg-white rounded-lg border border-slate-200 flex flex-col overflow-hidden">

    <!-- Tab header -->
    <div class="flex border-b border-slate-200 shrink-0 bg-slate-50">
      <template x-for="t in leftTabs" :key="t.value">
        <button @click="leftTab = t.value"
                class="flex-1 px-2 py-2 text-xs font-medium border-b-2 -mb-px transition relative"
                :class="leftTab === t.value
                  ? 'border-brand-600 text-brand-700 bg-white'
                  : 'border-transparent text-slate-500 hover:text-slate-700'">
          <i :data-lucide="t.icon" class="icon w-3 h-3 inline-block align-text-bottom mr-1"></i>
          <span x-text="t.label"></span>
          <span x-show="t.badge" class="ml-1 text-[10px] bg-slate-200 text-slate-600 rounded-full px-1.5"
                x-text="t.badge"></span>
        </button>
      </template>
    </div>

    <!-- Tab content -->
    <div class="flex-1 overflow-hidden">

      <!-- ▶ Tab: 大纲 -->
      <section x-show="leftTab === 'outline'" class="h-full overflow-y-auto p-2">
        <ul class="space-y-0.5 text-xs">
          <template x-for="sym in outline" :key="sym.line">
            <li class="flex items-center gap-1.5 px-2 py-1 hover:bg-slate-50 rounded cursor-pointer"
                @click="jumpTo(sym.line)">
              <i :data-lucide="sym.kind === 'function' ? 'square-function' : 'square-code'"
                 class="icon w-3 h-3 text-slate-400"></i>
              <span class="mono truncate" x-text="sym.name"></span>
              <span class="text-slate-400 ml-auto" x-text="'L' + sym.line"></span>
            </li>
          </template>
          <li x-show="outline.length === 0" class="text-slate-400 px-2 py-2">
            (脚本内无 def function)
          </li>
        </ul>
      </section>

      <!-- ▶ Tab: 数据库 Schema -->
      <section x-show="leftTab === 'schema'" class="h-full flex flex-col">
        <!-- 状态条 -->
        <div class="px-2 py-1.5 border-b border-slate-100 shrink-0 flex items-center gap-2">
          <input x-model="schemaFilter" type="search" placeholder="过滤 svc / table..."
                 class="flex-1 text-xs border border-slate-200 rounded px-2 py-1 focus:ring-1 focus:ring-brand-500 focus:border-brand-500">
          <button @click="loadSchema(true)" class="text-slate-400 hover:text-slate-600" title="刷新">
            <i data-lucide="refresh-cw" class="icon w-3 h-3"
               :class="schemaState === 'loading' && 'animate-spin'"></i>
          </button>
        </div>

        <!-- 状态指示 -->
        <div class="px-2 py-1 text-[10px] flex items-center gap-1 shrink-0"
             :class="{
               'text-slate-400': schemaState === 'loading',
               'text-emerald-600': schemaState === 'ok',
               'text-amber-600': schemaState === 'empty',
               'text-red-600': schemaState === 'err',
             }">
          <span x-show="schemaState === 'loading'">⏳ 加载中...</span>
          <span x-show="schemaState === 'ok'"
                x-text="'✓ 已加载 ' + schemaTables.length + ' 张表 (' + Object.keys(groupedSchema).length + ' 个 service)'"></span>
          <span x-show="schemaState === 'empty'">⚠ meta syncer 无数据 — 显示内置默认表</span>
          <span x-show="schemaState === 'err'" x-text="'✗ ' + schemaErr"></span>
        </div>

        <!-- 调试信息: 直接显示 tables 数量 -->
        <div class="px-2 py-1 text-[10px] text-slate-500 shrink-0 border-b border-slate-100">
          debug: schemaTables.length = <span class="mono font-bold" x-text="schemaTables.length"></span>,
          groups = <span class="mono font-bold" x-text="groupedSchema.length"></span>
        </div>

        <!-- 表列表 -->
        <div class="flex-1 overflow-y-auto text-xs">
          <template x-for="grp in groupedSchema" :key="grp.svc">
            <div class="border-b border-slate-100">
              <button @click="toggleSvc(grp.svc)"
                      class="w-full flex items-center px-2 py-1.5 hover:bg-slate-50 text-left sticky top-0 bg-white border-b border-slate-100">
                <i :data-lucide="schemaClosed[grp.svc] ? 'chevron-right' : 'chevron-down'"
                   class="icon w-3 h-3 text-slate-400 mr-1"></i>
                <span class="font-medium text-slate-700 truncate" x-text="grp.svc"></span>
                <span class="ml-auto text-slate-400" x-text="grp.tables.length"></span>
              </button>
              <ul x-show="!schemaClosed[grp.svc]" class="pb-1">
                <template x-for="t in grp.tables" :key="t.key">
                  <li class="group">
                    <div class="flex items-center px-2 py-1 hover:bg-slate-50">
                      <button @click="loadColumns(t)" @dblclick="insertScan(t)"
                              class="flex-1 flex items-center text-left mono text-[11px]"
                              :class="t.key === activeTable && 'text-brand-700 font-medium'">
                        <i :data-lucide="t.key === activeTable ? 'folder-open' : 'table-2'"
                           class="icon w-3 h-3 mr-1"
                           :class="t.key === activeTable ? 'text-brand-600' : 'text-slate-400'"></i>
                        <span class="truncate" x-text="t.table"></span>
                      </button>
                      <button @click="insertScan(t)"
                              class="opacity-0 group-hover:opacity-100 text-brand-600 hover:bg-brand-50 rounded px-1"
                              title="插入 ctx.scan(...)">
                        <i data-lucide="plus" class="icon w-3 h-3"></i>
                      </button>
                    </div>
                    <ul x-show="t.columns && t.key === activeTable" class="ml-5 pb-1">
                      <template x-for="c in (t.columns || [])" :key="c.name">
                        <li @click="insertColumn(c)"
                            class="flex items-center text-[10px] py-0.5 px-2 mono cursor-pointer hover:bg-slate-50"
                            :title="'click 插入 \"' + c.name + '\"'">
                          <i data-lucide="minus" class="icon w-2.5 h-2.5 text-slate-300"></i>
                          <span class="text-slate-700 ml-1" x-text="c.name"></span>
                          <span class="text-slate-400 ml-auto" x-text="c.type"></span>
                        </li>
                      </template>
                      <li x-show="!t.columns || t.columns.length === 0"
                          class="text-[10px] text-slate-400 px-2 py-1">(列加载中或为空)</li>
                    </ul>
                  </li>
                </template>
              </ul>
            </div>
          </template>
          <div x-show="groupedSchema.length === 0 && schemaTables.length > 0"
               class="px-3 py-4 text-center text-[10px] text-slate-400">
            ⚠ 渲染错误: 有 <span x-text="schemaTables.length"></span> 张表但分组失败
          </div>
          <div x-show="schemaTables.length === 0"
               class="px-3 py-4 text-center text-[10px] text-slate-400">
            ⚠ schemaTables 为空 (检查 FALLBACK_TABLES 常量)
          </div>
        </div>
        <div class="px-2 py-1.5 border-t border-slate-100 text-[10px] text-slate-400 shrink-0">
          点 table 看列;双击 / + 按钮插入 <span class="mono">ctx.scan(...)</span>
        </div>
      </section>

      <!-- ▶ Tab: 版本历史 -->
      <section x-show="leftTab === 'versions'" class="h-full overflow-y-auto p-2">
        <ul class="space-y-1 text-xs">
          <template x-for="v in versions" :key="v.id">
            <li class="px-2 py-1.5 border border-slate-200 rounded">
              <div class="flex items-center justify-between">
                <span class="mono font-medium" x-text="'v' + v.version"></span>
                <span class="text-slate-400" x-text="v.age"></span>
              </div>
              <div class="text-slate-500 mt-0.5" x-text="v.actor || '?'"></div>
              <div class="flex items-center gap-2 mt-1">
                <button @click="diffWith(v)" class="text-brand-600 hover:underline text-[10px]">diff</button>
                <button @click="rollback(v)" class="text-slate-500 hover:underline text-[10px]">rollback</button>
              </div>
            </li>
          </template>
          <li x-show="versions.length === 0" class="text-slate-400 px-2 py-2">
            (新建规则保存后会出现历史)
          </li>
        </ul>
      </section>

      <!-- ▶ Tab: Test (UX-1 单元测试) -->
      <section x-show="leftTab === 'test'" class="h-full flex flex-col">
        <!-- 用例列表 + 新增 -->
        <div class="px-2 py-1.5 border-b border-slate-100 flex items-center gap-1 shrink-0 text-[10px]">
          <select x-model.number="activeCaseIdx" class="flex-1 text-xs border border-slate-300 rounded px-1 py-0.5">
            <template x-for="(c, i) in testCases" :key="i">
              <option :value="i" x-text="c.name + (c.result ? (c.result.passed ? ' ✓' : ' ✗') : '')"></option>
            </template>
          </select>
          <button @click="addTestCase()" class="text-slate-500 hover:text-slate-700">
            <i data-lucide="plus" class="icon w-3 h-3"></i>
          </button>
          <button @click="removeTestCase()" x-show="testCases.length > 1"
                  class="text-slate-500 hover:text-red-600">
            <i data-lucide="trash" class="icon w-3 h-3"></i>
          </button>
        </div>

        <div class="flex-1 overflow-y-auto px-2 py-2 space-y-2">
          <div>
            <label class="text-[10px] text-slate-500 uppercase">用例名</label>
            <input type="text" x-model="activeCase.name"
                   class="w-full text-xs border border-slate-300 rounded px-1.5 py-0.5 mono">
          </div>
          <div>
            <label class="text-[10px] text-slate-500 uppercase flex items-center justify-between">
              <span>Fixtures (events JSON)</span>
              <span class="text-[9px] text-slate-400">喂给 FixtureSearcher</span>
            </label>
            <textarea x-model="activeCase.fixtures" rows="6"
                      class="w-full text-[11px] mono border border-slate-300 rounded px-1.5 py-1 leading-tight"
                      spellcheck="false"></textarea>
          </div>
          <div>
            <label class="text-[10px] text-slate-500 uppercase flex items-center justify-between">
              <span>Expected diffs JSON</span>
              <span class="text-[9px] text-slate-400">subset 匹配</span>
            </label>
            <textarea x-model="activeCase.expected" rows="4"
                      class="w-full text-[11px] mono border border-slate-300 rounded px-1.5 py-1 leading-tight"
                      spellcheck="false"></textarea>
          </div>
          <div class="flex items-center gap-1.5 pt-1">
            <button @click="runTest()" class="btn btn-primary text-xs px-2 py-1">
              <i data-lucide="play" class="icon w-3 h-3"></i> Run Test
            </button>
            <button @click="runAllTests()" class="btn btn-outline text-xs px-2 py-1" x-show="testCases.length > 1">
              <i data-lucide="list-checks" class="icon w-3 h-3"></i> Run All
            </button>
          </div>

          <!-- 结果 -->
          <div x-show="activeCase.result" class="border rounded p-2 text-xs"
               :class="activeCase.result && activeCase.result.passed ? 'bg-emerald-50 border-emerald-200' : 'bg-red-50 border-red-200'">
            <div class="flex items-center gap-2 mb-1">
              <span class="font-semibold"
                    :class="activeCase.result && activeCase.result.passed ? 'text-emerald-700' : 'text-red-700'"
                    x-text="activeCase.result && activeCase.result.passed ? '✓ Passed' : '✗ Failed'"></span>
              <span class="text-[10px] text-slate-500"
                    x-text="activeCase.result ? activeCase.result.exec_ms + ' ms' : ''"></span>
            </div>
            <div x-show="activeCase.result && activeCase.result.error"
                 class="mono text-[10px] text-red-700 mb-1"
                 x-text="activeCase.result && activeCase.result.error"></div>
            <div x-show="activeCase.result && activeCase.result.missing && activeCase.result.missing.length > 0">
              <div class="text-[10px] text-slate-600 mt-1">缺失 (expected 没匹配上):</div>
              <pre class="mono text-[10px] text-slate-700 bg-white p-1 rounded mt-1 max-h-40 overflow-y-auto"
                   x-text="activeCase.result ? JSON.stringify(activeCase.result.missing, null, 2) : ''"></pre>
            </div>
            <div>
              <div class="text-[10px] text-slate-600 mt-1">实际产出:</div>
              <pre class="mono text-[10px] text-slate-700 bg-white p-1 rounded mt-1 max-h-40 overflow-y-auto"
                   x-text="activeCase.result ? JSON.stringify(activeCase.result.actual, null, 2) : ''"></pre>
            </div>
          </div>
        </div>
      </section>

      <!-- ▶ Tab: 实时事件 (mini) -->
      <section x-show="leftTab === 'events'" class="h-full flex flex-col">
        <div class="px-2 py-1.5 border-b border-slate-100 text-[10px] flex items-center gap-2 shrink-0">
          <span class="flex items-center gap-1">
            <span class="w-1.5 h-1.5 rounded-full"
                  :class="liveStatus === 'open' ? 'bg-emerald-500 animate-pulse' : 'bg-slate-300'"></span>
            <span class="text-slate-500" x-text="liveStatus === 'open' ? 'streaming' : 'connecting'"></span>
          </span>
          <button @click="liveBuffer = []" class="ml-auto text-slate-400 hover:text-slate-600 text-[10px]">clear</button>
        </div>
        <ul class="flex-1 overflow-y-auto divide-y divide-slate-100 text-xs">
          <template x-for="e in liveBuffer.slice(0, 50)" :key="e._uid">
            <li class="px-2 py-1.5 hover:bg-slate-50">
              <div class="flex items-center gap-1">
                <span class="badge text-[9px] py-0"
                      :class="e.op === 'INSERT' ? 'badge-ok' : e.op === 'UPDATE' ? 'badge-info' : 'badge-critical'"
                      x-text="e.op || '?'"></span>
                <span class="mono text-[10px] truncate flex-1" x-text="e.svc + ':' + e.table"></span>
                <span class="text-slate-400 text-[9px]" x-text="formatTs(e.ts)"></span>
              </div>
              <div class="mono text-[10px] text-slate-500 truncate" x-text="e.pk"></div>
            </li>
          </template>
          <li x-show="liveBuffer.length === 0 && liveStatus === 'open'"
              class="px-2 py-4 text-center text-slate-400 text-[10px]">
            等待 binlog 事件...
          </li>
          <li x-show="liveStatus !== 'open'"
              class="px-2 py-4 text-center text-slate-400 text-[10px]">
            <span x-text="liveStatus === 'err' ? '✗ 断开,自动重连中' : '⏳ 连接中'"></span>
          </li>
        </ul>
        <div class="px-2 py-1.5 border-t border-slate-100 text-[10px] text-slate-400 shrink-0">
          监听 /api/v1/events/stream — <a href="/admin/events" class="text-brand-600 hover:underline">查看全部</a>
        </div>
      </section>
    </div>
  </aside>

  <!-- 中:编辑器 -->
  <section class="col-span-6 flex flex-col">
    <div class="bg-white rounded-t-lg border border-b-0 border-slate-200 px-3 py-2 flex items-center gap-2 shrink-0">
      <input type="text" x-model="meta.name" placeholder="rule_name (snake_case)"
             class="mono text-sm border-0 outline-none px-1 py-0.5 flex-1 focus:bg-slate-50 rounded">
      <select x-model="meta.severity" class="text-xs border border-slate-300 rounded px-2 py-1">
        <option value="info">info</option>
        <option value="warning">warning</option>
        <option value="critical">critical</option>
      </select>
      <!-- UX-2: shadow mode toggle. shadow 时主 Kafka topic 不发 diff,
           只写 Redis stream 给运营对比, 验证完再切 live. -->
      <label class="flex items-center gap-1 text-xs"
             title="Shadow 模式: 跑规则但 diff 不发主 Kafka topic,仅写 Redis shadow stream 供对比">
        <input type="checkbox" :checked="meta.mode === 'shadow'"
               @change="meta.mode = $event.target.checked ? 'shadow' : 'live'"
               class="w-3 h-3">
        <span class="text-slate-600">Shadow</span>
        <span x-show="meta.mode === 'shadow'"
              class="badge bg-amber-100 text-amber-700 text-[9px] py-0">SHADOW</span>
      </label>
      <button @click="lint()" class="btn btn-outline text-xs">
        <i data-lucide="check-circle-2" class="icon w-3 h-3"></i> Lint
      </button>
      <button @click="dryRun()" class="btn btn-outline text-xs">
        <i data-lucide="play" class="icon w-3 h-3"></i> Dry-run
      </button>
      <button @click="save()" class="btn btn-primary text-xs">
        <i data-lucide="save" class="icon w-3 h-3"></i> Save
      </button>
      <div x-data="{ open: false }" class="relative">
        <button @click="open = !open" class="btn btn-ghost text-xs">
          <i data-lucide="more-vertical" class="icon w-3 h-3"></i>
        </button>
        <div x-show="open" @click.outside="open = false"
             class="absolute right-0 mt-1 w-44 bg-white rounded shadow-lg border border-slate-200 z-10 text-sm">
          <button @click="fork(); open = false" class="block w-full text-left px-3 py-1.5 hover:bg-slate-50">
            <i data-lucide="git-fork" class="icon w-3 h-3 inline-block mr-1"></i>Fork
          </button>
          <button @click="exportCode(); open = false" class="block w-full text-left px-3 py-1.5 hover:bg-slate-50">
            <i data-lucide="download" class="icon w-3 h-3 inline-block mr-1"></i>导出 .star
          </button>
          <button @click="del(); open = false" class="block w-full text-left px-3 py-1.5 text-red-600 hover:bg-red-50">
            <i data-lucide="trash-2" class="icon w-3 h-3 inline-block mr-1"></i>删除
          </button>
        </div>
      </div>
    </div>
    <div id="monaco" class="flex-1 border border-slate-200 bg-white rounded-b-lg overflow-hidden"></div>
  </section>

  <!-- 右:运行预览 + Console -->
  <aside class="col-span-3 bg-white rounded-lg border border-slate-200 overflow-hidden flex flex-col">
    <!-- Tab header -->
    <div class="flex border-b border-slate-200 shrink-0 bg-slate-50">
      <button @click="rightTab = 'diffs'"
              class="flex-1 px-3 py-2 text-xs font-medium border-b-2 -mb-px transition"
              :class="rightTab === 'diffs'
                ? 'border-brand-600 text-brand-700 bg-white'
                : 'border-transparent text-slate-500 hover:text-slate-700'">
        <i data-lucide="alert-triangle" class="icon w-3 h-3 inline-block align-text-bottom mr-1"></i>
        Diffs
        <span class="ml-1 text-[10px] bg-slate-200 text-slate-600 rounded-full px-1.5"
              x-text="diffsCount"></span>
      </button>
      <button @click="rightTab = 'console'"
              class="flex-1 px-3 py-2 text-xs font-medium border-b-2 -mb-px transition"
              :class="rightTab === 'console'
                ? 'border-brand-600 text-brand-700 bg-white'
                : 'border-transparent text-slate-500 hover:text-slate-700'">
        <i data-lucide="terminal" class="icon w-3 h-3 inline-block align-text-bottom mr-1"></i>
        Console
        <span class="ml-1 text-[10px] bg-slate-200 text-slate-600 rounded-full px-1.5"
              x-text="logs.length"></span>
      </button>
    </div>

    <!-- 状态 strip (running 状态可见) -->
    <div class="px-3 py-1 border-b border-slate-100 text-xs flex items-center gap-2"
         :class="status === 'ok' ? 'text-emerald-600 bg-emerald-50' :
                 status === 'err' ? 'text-red-600 bg-red-50' :
                 status === 'running' ? 'text-blue-600 bg-blue-50' : 'text-slate-400 bg-slate-50'">
      <span x-show="status === 'running'" class="inline-block animate-spin">⟳</span>
      <span x-text="status === 'ok' ? '✓ ' + diffsCount + ' diffs · ' + logs.length + ' logs' :
                    status === 'err' ? '✗ ' + (errorMsg || '错误') :
                    status === 'running' ? '运行中...' : '未运行 — 点 Dry-run'"></span>
    </div>

    <!-- 内容区 -->
    <div class="flex-1 overflow-y-auto p-3">

      <!-- Diffs tab -->
      <div x-show="rightTab === 'diffs'">
        <div x-show="status === 'err'" class="mono text-xs bg-red-50 border border-red-200 text-red-700 rounded p-2 whitespace-pre-wrap"
             x-text="errorMsg"></div>
        <template x-for="(d, i) in diffs" :key="i">
          <div class="border-b border-slate-100 py-2">
            <div class="flex items-center justify-between">
              <span class="mono text-xs font-medium" x-text="d.type || 'diff'"></span>
              <span class="text-xs text-slate-400" x-text="'#' + (i+1)"></span>
            </div>
            <pre class="mono text-[11px] text-slate-600 mt-1 whitespace-pre-wrap"
                 x-text="JSON.stringify(d, null, 2)"></pre>
          </div>
        </template>
        <div x-show="status === 'ok' && diffs.length === 0" class="text-xs text-slate-400 text-center py-4">
          0 diffs (规则跑通,无命中)
        </div>
        <div x-show="status === ''" class="text-xs text-slate-400 text-center py-4">
          点 Dry-run 试跑这条规则
        </div>
      </div>

      <!-- Console tab: 捕获 print() / ctx.log_* 输出 -->
      <div x-show="rightTab === 'console'">
        <div x-show="logs.length === 0 && status === 'ok'"
             class="text-xs text-slate-400 text-center py-4">
          <p>无输出</p>
          <p class="mt-2 mono text-[10px]">在脚本里加 <span class="bg-slate-100 px-1 rounded">print("...")</span> 或 <span class="bg-slate-100 px-1 rounded">ctx.log_info("msg", k="v")</span></p>
        </div>
        <div x-show="logs.length === 0 && status === ''"
             class="text-xs text-slate-400 text-center py-4">
          点 Dry-run 后,print() / ctx.log_* 输出会显示在这里
        </div>
        <template x-for="(log, i) in logs" :key="i">
          <div class="flex items-start gap-2 py-1.5 border-b border-slate-100">
            <span class="badge text-[9px] py-0 shrink-0 mt-0.5"
                  :class="log.level === 'error' ? 'badge-critical' :
                          log.level === 'warn' ? 'badge-warning' :
                          log.level === 'print' ? 'bg-slate-200 text-slate-700' : 'badge-info'"
                  x-text="log.level"></span>
            <div class="flex-1 min-w-0">
              <div class="mono text-[11px] text-slate-800 whitespace-pre-wrap break-words"
                   x-text="log.msg"></div>
              <template x-if="log.kv && Object.keys(log.kv).length > 0">
                <pre class="mono text-[10px] text-slate-500 mt-0.5"
                     x-text="JSON.stringify(log.kv, null, 2)"></pre>
              </template>
            </div>
          </div>
        </template>
      </div>
    </div>
  </aside>
</div>
`
	extraHead := `
<link rel="stylesheet" data-name="vs/editor/editor.main"
      href="https://cdn.jsdelivr.net/npm/monaco-editor@0.46.0/min/vs/editor/editor.main.css">
`

	script := `
<script src="https://cdn.jsdelivr.net/npm/monaco-editor@0.46.0/min/vs/loader.js"></script>
<script>
let monacoEditor;
let _completionRegistered = false;

require.config({ paths: { vs: 'https://cdn.jsdelivr.net/npm/monaco-editor@0.46.0/min/vs' } });
require(['vs/editor/editor.main'], function() {
  monacoEditor = monaco.editor.create(document.getElementById('monaco'), {
    value: starterCode(),
    language: 'python',
    theme: 'vs',
    minimap: { enabled: false },
    fontFamily: 'JetBrains Mono, Menlo, monospace',
    fontSize: 13,
    automaticLayout: true,
    quickSuggestions: { other: true, comments: false, strings: true },
  });
});

function starterCode() {
  return [
    '# 新规则模板',
    '# - 输入: ctx.scan(svc, table) / ctx.get_by_index(idx, val) / ctx.get(svc, table, pk)',
    '# - 输出: return [{"type": "...", "key": "...", "detail": {...}}, ...]',
    '',
    'def check(ctx):',
    '    diffs = []',
    '    # TODO',
    '    return diffs',
    '',
  ].join('\n');
}

// Fallback tables: meta API 没数据时也能用 autocomplete
const FALLBACK_TABLES = [
  // order-core
  { svc: 'order-core', table: 'payment_intent' },
  { svc: 'order-core', table: 'payment_method' },
  { svc: 'order-core', table: 'refund_request' },
  // payment-channel
  { svc: 'payment-channel', table: 'acquirer_tx' },
  { svc: 'payment-channel', table: 'channel_callback' },
  { svc: 'payment-channel', table: 'refund' },
  // accounting-system
  { svc: 'accounting-system', table: 'ledger_entry' },
  { svc: 'accounting-system', table: 'voucher' },
  { svc: 'accounting-system', table: 'tcc_record' },
  // user-merchant-core
  { svc: 'user-merchant-core', table: 'merchant' },
  { svc: 'user-merchant-core', table: 'user_card' },
  // billing-system
  { svc: 'billing-system', table: 'fee_event' },
  { svc: 'billing-system', table: 'statement' },
  // clearing-settlement
  { svc: 'clearing-settlement', table: 'settlement_run' },
  { svc: 'clearing-settlement', table: 'settlement_record' },
  // risk-manage
  { svc: 'risk-manage', table: 'decision_audit' },
  // merchant-webhook
  { svc: 'merchant-webhook', table: 'delivery' },
  { svc: 'merchant-webhook', table: 'delivery_attempt' },
  // dispute-service
  { svc: 'dispute-service', table: 'dispute' },
];

const FALLBACK_IDX_KEYS = [
  'pi_id', 'order_id', 'merchant_id', 'idempotency_key',
  'transaction_id', 'user_id', 'customer_id', 'dispute_id',
];

function editorModel(scriptID) {
  return {
    scriptID,
    meta: { name: '', severity: 'warning', mode: 'live', tags: [] },
    outline: [], versions: [],
    diffs: [], errorMsg: '',
    status: '', diffsCount: 0,
    // 右侧 tab: 'diffs' | 'console'; logs 捕获 print() + ctx.log_*
    rightTab: 'diffs',
    logs: [],

    // 左栏 tab
    leftTab: 'schema',  // 默认显示 schema (用户最关心)
    get leftTabs() {
      return [
        { value: 'outline',  label: '大纲',     icon: 'list-tree',     badge: this.outline.length },
        { value: 'schema',   label: 'Schema',  icon: 'database',      badge: this.schemaTables.length },
        { value: 'test',     label: 'Test',    icon: 'beaker',        badge: this.testCases.length },
        { value: 'versions', label: '版本',     icon: 'git-commit',    badge: this.versions.length },
        { value: 'events',   label: '实时',     icon: 'activity',      badge: '' },
      ];
    },

    // UX-1: 测试用例集 + 上一次测试结果. 多用例本地存,不持久化 (refresh 后清空).
    // 一条 case = { name, fixtures (JSON string), expected (JSON string), result }
    // 用 string 而非 object 让 textarea 双向绑定平滑.
    testCases: [
      {
        name: 'sample_case_1',
        fixtures: '[\n  {\n    "svc": "order-core", "table": "payment_intent", "pk": "pi_1",\n    "op": "INSERT", "indexes": { "pi_id": "pi_1" },\n    "after": { "id": "pi_1", "amount": 1000, "status": "CAPTURED" }\n  }\n]',
        expected: '[\n  { "type": "missing_leg", "key": "pi_1" }\n]',
        result: null,
      },
    ],
    activeCaseIdx: 0,
    get activeCase() { return this.testCases[this.activeCaseIdx]; },

    // schema 状态 — 立即用 fallback 填充,保证第一帧就有内容,
    // loadSchema() 跑完后再合并真实数据 (若有).
    schemaTables: FALLBACK_TABLES.map(t => ({ ...t, key: t.svc + ':' + t.table })),
    schemaClosed: {},
    schemaFilter: '',
    activeTable: '',
    indexKeys: FALLBACK_IDX_KEYS,
    schemaState: 'empty',  // 默认 empty (展示 fallback),loadSchema 后变 ok / err
    schemaErr: '',

    // live events (mini SSE,跟 /admin/events 共用 stream)
    liveBuffer: [],
    liveStatus: 'init',
    liveSse: null,

    // groupedSchema 返数组形式 [{svc, tables}, ...] (排序好),
    // 比 {svc: [tables]} 对象更可靠 — Alpine x-for 对 Object.keys() 反应不稳.
    get groupedSchema() {
      const f = (this.schemaFilter || '').toLowerCase();
      const map = {};
      for (const t of this.schemaTables) {
        if (f && !(t.svc.toLowerCase().includes(f) || t.table.toLowerCase().includes(f))) continue;
        (map[t.svc] = map[t.svc] || []).push(t);
      }
      return Object.entries(map)
        .map(([svc, tables]) => ({ svc, tables }))
        .sort((a, b) => a.svc.localeCompare(b.svc));
    },

    async load() {
      await Promise.all([this.loadScript(), this.loadSchema(false), this.loadIndexKeys()]);
      this.registerCompletion();
      this.connectLiveStream();
    },

    async loadScript() {
      if (!this.scriptID) return;
      try {
        const r = await fetch('/api/v1/scripts/' + this.scriptID).then(r => r.json());
        // 双重防御:JSON tag 加了之后正常字段是小写,但保留大写兜底
        this.meta.name     = r.name || r.Name || this.scriptID;
        this.meta.severity = r.severity || r.Severity || 'warning';
        this.meta.mode     = r.mode || r.Mode || 'live';
        this.meta.tags     = Array.isArray(r.tags) ? r.tags : (Array.isArray(r.Tags) ? r.Tags : []);
        const code = r.code || r.Code || '';
        const wait = setInterval(() => {
          if (monacoEditor) {
            monacoEditor.setValue(code || starterCode());
            this.parseOutline(code);
            clearInterval(wait);
          }
        }, 50);
        try {
          const vResp = await fetch('/api/v1/scripts/' + this.scriptID + '/versions').then(r => r.json());
          this.versions = Array.isArray(vResp) ? vResp : (vResp.versions || []);
        } catch (_) {}
      } catch (e) { console.error('[editor] load failed:', e); }
    },

    async loadSchema(force) {
      const prev = this.schemaState;
      // 不重置 schemaTables — 让 fallback 一直可见,只在 ok 时替换
      try {
        const res = await fetch('/api/v1/meta/tables', { cache: force ? 'no-cache' : 'default' });
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const list = await res.json();
        const norm = (Array.isArray(list) ? list : []).map(s => {
          const [svc, table] = String(s).split(':');
          return { svc: svc || 'unknown', table: table || s, key: s };
        });
        if (norm.length === 0) {
          // meta 没数据 → 保持 fallback,state empty
          this.schemaState = 'empty';
        } else {
          // 合并: 真实数据优先,fallback 补差
          const realKeys = new Set(norm.map(t => t.key));
          const fallbackExtra = FALLBACK_TABLES
            .filter(t => !realKeys.has(t.svc + ':' + t.table))
            .map(t => ({ ...t, key: t.svc + ':' + t.table }));
          this.schemaTables = [...norm, ...fallbackExtra];
          this.schemaState = 'ok';
        }
      } catch (e) {
        console.warn('schema load failed:', e);
        // 已经有 fallback,只更新 state
        this.schemaState = 'err';
        this.schemaErr = String(e).slice(0, 60);
      }
      this.$nextTick(() => { if (window.lucide) lucide.createIcons(); });
    },

    async loadIndexKeys() {
      try {
        const k = await fetch('/api/v1/meta/idx_keys').then(r => r.json()).catch(() => []);
        if (Array.isArray(k) && k.length > 0) this.indexKeys = k;
      } catch (_) {}
    },

    async loadColumns(t) {
      this.activeTable = t.key;
      if (t.columns) return;
      try {
        const cols = await fetch('/api/v1/meta/schema/' + t.svc + '/' + t.table).then(r => r.json()).catch(() => []);
        t.columns = (Array.isArray(cols) ? cols : []).map(c => ({
          name: c.name || c.column || c, type: c.type || ''
        }));
        if (t.columns.length === 0) {
          // 没列也加点占位 (用 fallback)
          t.columns = [
            { name: 'id', type: 'varchar' },
            { name: 'created_at', type: 'datetime' },
            { name: 'updated_at', type: 'datetime' },
          ];
        }
        this.schemaTables = [...this.schemaTables];  // touch
      } catch (e) { console.error(e); }
    },

    toggleSvc(svc) {
      this.schemaClosed = { ...this.schemaClosed, [svc]: !this.schemaClosed[svc] };
    },

    insertScan(t) {
      if (!monacoEditor) return;
      const snippet = 'ctx.scan("' + t.svc + '", "' + t.table + '")';
      const sel = monacoEditor.getSelection();
      monacoEditor.executeEdits('insert-scan', [
        { range: sel, text: snippet, forceMoveMarkers: true },
      ]);
      monacoEditor.focus();
    },

    insertColumn(c) {
      if (!monacoEditor) return;
      const sel = monacoEditor.getSelection();
      monacoEditor.executeEdits('insert-col', [
        { range: sel, text: '"' + c.name + '"', forceMoveMarkers: true },
      ]);
      monacoEditor.focus();
    },

    registerCompletion() {
      if (_completionRegistered || !window.monaco) return;
      _completionRegistered = true;
      const self = this;
      monaco.languages.registerCompletionItemProvider('python', {
        triggerCharacters: ['"', "'", '(', '[', '.'],
        provideCompletionItems(model, position) {
          const line = model.getLineContent(position.lineNumber);
          const before = line.substring(0, position.column - 1);
          const items = [];
          if (/ctx\.scan\(\s*["']$/.test(before)) {
            const seen = new Set();
            for (const t of self.schemaTables) {
              if (seen.has(t.svc)) continue;
              seen.add(t.svc);
              items.push({ label: t.svc, kind: monaco.languages.CompletionItemKind.Module, insertText: t.svc });
            }
          } else if (/ctx\.scan\(\s*["'][^"']+["']\s*,\s*["']$/.test(before)) {
            const m = before.match(/ctx\.scan\(\s*["']([^"']+)["']/);
            if (m) {
              for (const t of self.schemaTables) {
                if (t.svc === m[1]) {
                  items.push({ label: t.table, kind: monaco.languages.CompletionItemKind.Struct, insertText: t.table });
                }
              }
            }
          } else if (/ctx\.get_by_index\(\s*["']$/.test(before)) {
            for (const k of self.indexKeys) {
              items.push({ label: k, kind: monaco.languages.CompletionItemKind.Field, insertText: k });
            }
          } else if (/\.(after|before)\[\s*["']$/.test(before) || /\.get\(\s*["']$/.test(before)) {
            const colSet = new Set();
            for (const t of self.schemaTables) {
              if (t.columns) for (const c of t.columns) colSet.add(c.name);
            }
            for (const c of colSet) {
              items.push({ label: c, kind: monaco.languages.CompletionItemKind.Property, insertText: c });
            }
          } else if (/ctx\.$/.test(before)) {
            for (const m of ['scan', 'get', 'get_by_index', 'scan_index', 'now', 'params']) {
              items.push({ label: m, kind: monaco.languages.CompletionItemKind.Method, insertText: m });
            }
          }
          const word = model.getWordUntilPosition(position);
          const range = {
            startLineNumber: position.lineNumber, endLineNumber: position.lineNumber,
            startColumn: word.startColumn, endColumn: word.endColumn,
          };
          for (const it of items) it.range = range;
          return { suggestions: items };
        }
      });
    },

    connectLiveStream() {
      try {
        if (this.liveSse) this.liveSse.close();
        this.liveStatus = 'init';
        this.liveSse = new EventSource('/api/v1/events/stream');
        this.liveSse.onopen  = () => { this.liveStatus = 'open'; };
        this.liveSse.onerror = () => {
          this.liveStatus = 'err';
          setTimeout(() => this.connectLiveStream(), 5000);
        };
        this.liveSse.addEventListener('connect', () => { this.liveStatus = 'open'; });
        // 关键修复: server 端用 event: binlog 命名事件,onmessage 不会触发
        const onBinlog = (m) => {
          try {
            const e = JSON.parse(m.data);
            e._uid = (e.svc || '?') + ':' + (e.table || '?') + ':' + (e.pk || '?') + ':' + (e.binlog_pos || Date.now());
            this.liveBuffer.unshift(e);
            if (this.liveBuffer.length > 100) this.liveBuffer.length = 100;
          } catch (_) {}
        };
        this.liveSse.addEventListener('binlog', onBinlog);
        this.liveSse.onmessage = onBinlog;  // 兜底
      } catch (e) { this.liveStatus = 'err'; }
    },

    formatTs(ts) {
      if (!ts) return '';
      try {
        const d = new Date(ts);
        return d.toTimeString().slice(0, 8);
      } catch (_) { return String(ts).slice(11, 19); }
    },

    parseOutline(code) {
      const lines = code.split('\n');
      const out = [];
      lines.forEach((l, i) => {
        const m = l.match(/^def\s+(\w+)\s*\(/);
        if (m) out.push({ name: m[1], kind: 'function', line: i + 1 });
      });
      this.outline = out;
    },

    jumpTo(line) {
      if (!monacoEditor) return;
      monacoEditor.revealLineInCenter(line);
      monacoEditor.setPosition({ lineNumber: line, column: 1 });
      monacoEditor.focus();
    },

    async lint() {
      const code = monacoEditor.getValue();
      try {
        const r = await fetch('/api/v1/scripts/' + (this.scriptID || '_') + '/validate', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ code }),
        }).then(r => r.json());
        this.status = r.ok ? 'ok' : 'err';
        this.errorMsg = r.error || '';
        if (r.ok) { this.diffs = []; this.diffsCount = 0; }
      } catch (e) { this.status = 'err'; this.errorMsg = String(e); }
    },

    async dryRun() {
      console.log('[editor] dryRun clicked');
      // 1) Monaco 准备好了吗
      if (!monacoEditor) {
        this.status = 'err';
        this.errorMsg = 'Monaco editor not ready yet (try refresh)';
        return;
      }
      const code = monacoEditor.getValue();
      console.log('[editor] code length:', code.length);
      if (!code || code.trim() === '') {
        this.status = 'err';
        this.errorMsg = '代码为空 — 在 Monaco 里写点东西再 Dry-run';
        return;
      }

      // 2) 即时反馈: 跳到 Diffs tab + 显示 "Running..."
      this.status = 'running';
      this.errorMsg = '';
      this.diffs = [];
      this.diffsCount = 0;
      this.logs = [];

      try {
        // 3) 发请求 + 显式状态码检查
        const resp = await fetch('/api/v1/scripts/_dry_run', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ code }),
        });
        console.log('[editor] dryRun HTTP', resp.status);
        if (!resp.ok) {
          const text = await resp.text().catch(() => '');
          this.status = 'err';
          this.errorMsg = 'HTTP ' + resp.status + ': ' + (text || resp.statusText);
          return;
        }
        const r = await resp.json();
        console.log('[editor] dryRun result:', r);

        this.logs = r.logs || [];
        if (r.error || r.status === 'error') {
          this.status = 'err';
          this.errorMsg = r.error || 'unknown error';
          // 出错时自动跳 Console (若有 log)
          if (this.logs.length > 0) this.rightTab = 'console';
        } else {
          this.status = 'ok';
          this.diffs = r.diffs || [];
          this.diffsCount = this.diffs.length;
        }
      } catch (e) {
        console.error('[editor] dryRun exception:', e);
        this.status = 'err';
        this.errorMsg = 'JS exception: ' + (e.message || e);
      }
    },

    // UX-1: 单元测试用例管理 + 运行.
    addTestCase() {
      const n = this.testCases.length;
      this.testCases.push({
        name: 'case_' + (n + 1),
        fixtures: '[]',
        expected: '[]',
        result: null,
      });
      this.activeCaseIdx = n;
    },
    removeTestCase() {
      if (this.testCases.length <= 1) return;
      this.testCases.splice(this.activeCaseIdx, 1);
      this.activeCaseIdx = Math.max(0, this.activeCaseIdx - 1);
    },
    async runTest() {
      const c = this.activeCase;
      if (!c) return;
      let fixtures, expected;
      try {
        fixtures = JSON.parse(c.fixtures || '[]');
        expected = JSON.parse(c.expected || '[]');
      } catch (e) {
        c.result = { passed: false, error: 'JSON parse: ' + e.message, actual: [], missing: [], extra: [] };
        return;
      }
      const code = monacoEditor ? monacoEditor.getValue() : '';
      try {
        const r = await fetch('/api/v1/scripts/_test', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ code, fixtures, expected, match: 'subset' }),
        });
        c.result = await r.json();
      } catch (e) {
        c.result = { passed: false, error: 'request: ' + e.message };
      }
    },
    async runAllTests() {
      // 顺序跑避免编译重复 (compileCache 自动命中).
      for (let i = 0; i < this.testCases.length; i++) {
        this.activeCaseIdx = i;
        await this.runTest();
      }
      const passed = this.testCases.filter(c => c.result && c.result.passed).length;
      toast(passed + ' / ' + this.testCases.length + ' passed');
    },

    async save() {
      if (!this.meta.name) { alert('请填规则名'); return; }
      const code = monacoEditor.getValue();
      try {
        const url = this.scriptID ? '/api/v1/scripts/' + this.scriptID : '/api/v1/scripts';
        const method = this.scriptID ? 'PUT' : 'POST';
        const r = await fetch(url, {
          method, headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ ...this.meta, code }),
        });
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const data = await r.json().catch(() => ({}));
        if (!this.scriptID && data.id) {
          window.history.replaceState({}, '', '/admin/editor?id=' + encodeURIComponent(data.id));
          this.scriptID = data.id;
        }
        this.parseOutline(code);
        toast('保存成功');
      } catch (e) { alert('保存失败: ' + e); }
    },

    async fork() {
      const newName = prompt('Fork 为新规则名:', this.meta.name + '_copy');
      if (!newName) return;
      const code = monacoEditor.getValue();
      try {
        const r = await fetch('/api/v1/scripts', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: newName, severity: this.meta.severity, code }),
        });
        const data = await r.json();
        location.href = '/admin/editor?id=' + encodeURIComponent(data.id);
      } catch (e) { alert('Fork 失败: ' + e); }
    },

    exportCode() {
      const code = monacoEditor.getValue();
      const blob = new Blob([code], { type: 'text/plain' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url; a.download = (this.meta.name || 'rule') + '.star';
      a.click();
      URL.revokeObjectURL(url);
    },

    async del() {
      if (!this.scriptID) { alert('未保存的规则,直接关闭页签即可'); return; }
      if (!confirm('确认删除 ' + this.meta.name + '?')) return;
      await fetch('/api/v1/scripts/' + this.scriptID, { method: 'DELETE' });
      location.href = '/admin/catalog';
    },

    async diffWith(v) {
      window.open('/admin/editor/diff?id=' + this.scriptID + '&v=' + v.version, '_blank');
    },

    async rollback(v) {
      if (!confirm('回滚到 v' + v.version + '?')) return;
      await fetch('/api/v1/scripts/' + this.scriptID + '/rollback', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ version: v.version }),
      });
      this.load();
    },
  };
}

function toast(msg) {
  const t = document.createElement('div');
  t.textContent = msg;
  t.className = 'fixed bottom-4 right-4 bg-slate-900 text-white px-4 py-2 rounded shadow-lg text-sm z-50';
  document.body.appendChild(t);
  setTimeout(() => t.remove(), 2000);
}
</script>
`
	adminPage(w, "规则编辑器", "editor", body, extraHead, script)
}
