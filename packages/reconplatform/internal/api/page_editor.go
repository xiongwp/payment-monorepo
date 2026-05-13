// page_editor.go — /admin/editor 升级版规则编辑器.
//
// 在共用 layout 内嵌入 Monaco + 现有 editor 功能,加:
//   - 左侧栏 3 段: outline / 版本历史 / **DB Schema 浏览** (auto-load from /api/v1/meta/*)
//   - 右侧:试运行 inline 预览 + lint 错误
//   - 顶部:lint / dry-run / save / fork / 版本 diff
//   - Monaco completion 自动注入 svc 名 / table 名 / column 名 / index key
//     在脚本里输入 `ctx.scan(` / `ctx.get_by_index(` / `e.after["` 时弹补全
//
// 旧 editor_html.go (1627 行 SPA) 保留为 /admin/legacy 兼容旧 bookmark.
package api

import "net/http"

func (s *Server) pageEditor(w http.ResponseWriter, r *http.Request) {
	scriptID := r.URL.Query().Get("id")
	body := `
<div x-data="editorModel('` + scriptID + `')" x-init="load()" class="grid grid-cols-12 gap-4 h-[calc(100vh-7rem)]">

  <!-- 左:outline + 历史 + DB schema (三段叠) -->
  <aside class="col-span-2 bg-white rounded-lg border border-slate-200 overflow-hidden flex flex-col">

    <!-- ▶ outline -->
    <details open class="border-b border-slate-200">
      <summary class="px-3 py-2 bg-slate-50 cursor-pointer text-xs font-semibold uppercase tracking-wider text-slate-500 select-none">
        大纲 <span class="ml-1 text-slate-400 normal-case font-normal" x-text="'(' + outline.length + ')'"></span>
      </summary>
      <ul class="px-2 py-1 max-h-48 overflow-y-auto space-y-0.5 text-xs">
        <template x-for="sym in outline" :key="sym.line">
          <li class="flex items-center gap-1.5 px-2 py-1 hover:bg-slate-50 rounded cursor-pointer"
              @click="jumpTo(sym.line)">
            <i :data-lucide="sym.kind === 'function' ? 'square-function' : 'square-code'"
               class="icon w-3 h-3 text-slate-400"></i>
            <span class="mono truncate" x-text="sym.name"></span>
            <span class="text-slate-400 ml-auto" x-text="sym.line"></span>
          </li>
        </template>
        <li x-show="outline.length===0" class="text-slate-400 px-2 py-2">解析中...</li>
      </ul>
    </details>

    <!-- ▶ 版本历史 -->
    <details class="border-b border-slate-200">
      <summary class="px-3 py-2 bg-slate-50 cursor-pointer text-xs font-semibold uppercase tracking-wider text-slate-500 select-none">
        版本 <span class="ml-1 text-slate-400 normal-case font-normal" x-text="'(' + versions.length + ')'"></span>
      </summary>
      <ul class="px-2 py-1 max-h-40 overflow-y-auto space-y-1 text-xs">
        <template x-for="v in versions" :key="v.id">
          <li class="px-2 py-1 hover:bg-slate-50 rounded">
            <div class="flex items-center justify-between">
              <span class="mono" x-text="'v' + v.version"></span>
              <span class="text-slate-400" x-text="v.age"></span>
            </div>
            <div class="flex items-center gap-2 mt-0.5 text-[10px]">
              <button @click="diffWith(v)" class="text-brand-600 hover:underline">diff</button>
              <button @click="rollback(v)" class="text-slate-500 hover:underline">rollback</button>
            </div>
          </li>
        </template>
        <li x-show="versions.length===0" class="text-slate-400 px-2 py-2">无历史</li>
      </ul>
    </details>

    <!-- ▶ DB Schema (auto-load) -->
    <details open class="flex-1 flex flex-col overflow-hidden">
      <summary class="px-3 py-2 bg-slate-50 cursor-pointer text-xs font-semibold uppercase tracking-wider text-slate-500 select-none flex items-center">
        <span>DB Schema</span>
        <span class="ml-1 text-slate-400 normal-case font-normal" x-text="'(' + schemaTables.length + ')'"></span>
        <button @click.prevent.stop="loadSchema()" class="ml-auto text-slate-400 hover:text-slate-600" title="刷新 schema">
          <i data-lucide="refresh-cw" class="icon w-3 h-3"></i>
        </button>
      </summary>
      <div class="px-2 py-1 border-b border-slate-100 shrink-0">
        <input x-model="schemaFilter" type="search" placeholder="过滤 svc / table..."
               class="w-full text-xs border border-slate-200 rounded px-2 py-1 focus:ring-1 focus:ring-brand-500 focus:border-brand-500">
      </div>
      <div class="flex-1 overflow-y-auto text-xs">
        <template x-for="(grp, svc) in groupedSchema" :key="svc">
          <div class="border-b border-slate-100">
            <button @click="toggleSvc(svc)"
                    class="w-full flex items-center px-2 py-1 hover:bg-slate-50 text-left">
              <i :data-lucide="schemaOpen[svc] === false ? 'chevron-right' : 'chevron-down'"
                 class="icon w-3 h-3 text-slate-400 mr-1"></i>
              <span class="font-medium text-slate-700 truncate" x-text="svc"></span>
              <span class="ml-auto text-slate-400" x-text="grp.length"></span>
            </button>
            <ul x-show="schemaOpen[svc] !== false" class="px-1 pb-1">
              <template x-for="t in grp" :key="t.key">
                <li class="group">
                  <button @click="loadColumns(t)" @dblclick="insertScan(t)"
                          class="w-full flex items-center px-3 py-1 hover:bg-slate-50 text-left mono text-[11px]"
                          :class="t.key === activeTable && 'bg-brand-50'">
                    <i data-lucide="table-2" class="icon w-3 h-3 text-slate-400 mr-1"></i>
                    <span class="truncate" x-text="t.table"></span>
                    <i data-lucide="plus" class="icon w-3 h-3 ml-auto opacity-0 group-hover:opacity-100 text-brand-600"
                       title="双击插入 ctx.scan(...)"></i>
                  </button>
                  <ul x-show="t.columns && t.key === activeTable" class="ml-6 mb-1">
                    <template x-for="c in (t.columns || [])" :key="c.name">
                      <li class="flex items-center text-[10px] text-slate-500 py-0.5 mono cursor-pointer hover:text-slate-900"
                          @click="insertColumn(c)" :title="'click 插入 ' + c.name">
                        <i data-lucide="dot" class="icon w-3 h-3"></i>
                        <span x-text="c.name"></span>
                        <span class="ml-1 text-slate-400" x-text="c.type"></span>
                      </li>
                    </template>
                  </ul>
                </li>
              </template>
            </ul>
          </div>
        </template>
        <div x-show="schemaTables.length === 0" class="text-slate-400 px-3 py-3">
          schema 加载中...
        </div>
      </div>
      <div class="px-2 py-1.5 border-t border-slate-100 text-[10px] text-slate-400 shrink-0">
        <i data-lucide="info" class="icon w-3 h-3 inline-block align-text-bottom"></i>
        点 table 看列;双击插入 <span class="mono">ctx.scan(...)</span>
      </div>
    </details>
  </aside>

  <!-- 中:编辑器 -->
  <section class="col-span-7 flex flex-col">
    <div class="bg-white rounded-t-lg border border-b-0 border-slate-200 px-3 py-2 flex items-center gap-2 shrink-0">
      <input type="text" x-model="meta.name" placeholder="rule_name (snake_case)"
             class="mono text-sm border-0 outline-none px-1 py-0.5 flex-1 focus:bg-slate-50 rounded">
      <select x-model="meta.severity" class="text-xs border border-slate-300 rounded px-2 py-1">
        <option value="info">info</option>
        <option value="warning">warning</option>
        <option value="critical">critical</option>
      </select>
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

  <!-- 右:试运行 / lint -->
  <aside class="col-span-3 bg-white rounded-lg border border-slate-200 overflow-hidden flex flex-col">
    <div class="px-3 py-2 border-b border-slate-200 bg-slate-50 flex items-center justify-between">
      <h3 class="text-xs font-semibold uppercase tracking-wider text-slate-500">运行预览</h3>
      <span class="text-xs"
            :class="status === 'ok' ? 'text-emerald-600' :
                    status === 'err' ? 'text-red-600' : 'text-slate-400'"
            x-text="status === 'ok' ? '✓ ' + diffsCount + ' diffs' :
                    status === 'err' ? '✗ 错误' : '未运行'"></span>
    </div>
    <div class="flex-1 overflow-y-auto p-3">
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
    language: 'python',  // Starlark 与 Python 同语法
    theme: 'vs',
    minimap: { enabled: false },
    fontFamily: 'JetBrains Mono, Menlo, monospace',
    fontSize: 13,
    automaticLayout: true,
    quickSuggestions: { other: true, comments: false, strings: true },
  });
});

// 起始模板
function starterCode() {
  return [
    '# 新规则模板。',
    '# 输入: ctx.scan(svc, table) / ctx.get_by_index(idx, val) / ctx.get(svc, table, pk)',
    '# 输出: return [{"type": "...", "key": "...", "detail": {...}}, ...]',
    '',
    'def check(ctx):',
    '    diffs = []',
    '    # TODO',
    '    return diffs',
    '',
  ].join('\n');
}

function editorModel(scriptID) {
  return {
    scriptID,
    meta: { name: '', severity: 'warning' },
    outline: [], versions: [],
    diffs: [], errorMsg: '',
    status: '', diffsCount: 0,

    // schema 状态
    schemaTables: [],          // [{svc, table, key, columns?}]
    schemaOpen: {},            // svc -> bool (折叠)
    schemaFilter: '',
    activeTable: '',           // 当前展开列的 table key
    indexKeys: [],

    get groupedSchema() {
      const f = this.schemaFilter.toLowerCase();
      const out = {};
      for (const t of this.schemaTables) {
        if (f && !(t.svc.toLowerCase().includes(f) || t.table.toLowerCase().includes(f))) continue;
        (out[t.svc] = out[t.svc] || []).push(t);
      }
      return out;
    },

    async load() {
      // 并行: 1) 拉脚本 (若有 id) 2) 拉 schema 3) 拉 idx_keys
      await Promise.all([this.loadScript(), this.loadSchema(), this.loadIndexKeys()]);
      // 把 schema 注入 Monaco completion
      this.registerCompletion();
    },

    async loadScript() {
      if (!this.scriptID) {
        // 新建模式: 等 Monaco 起来,把起始模板放进去
        return;
      }
      try {
        const r = await fetch('/api/v1/scripts/' + this.scriptID).then(r => r.json());
        this.meta.name     = r.name;
        this.meta.severity = r.severity || 'warning';
        const wait = setInterval(() => {
          if (monacoEditor) {
            monacoEditor.setValue(r.code || starterCode());
            this.parseOutline(r.code || '');
            clearInterval(wait);
          }
        }, 50);
        // 历史
        try {
          const v = await fetch('/api/v1/scripts/' + this.scriptID + '/versions').then(r => r.json());
          this.versions = v || [];
        } catch (_) {}
      } catch (e) { console.error(e); }
    },

    async loadSchema() {
      try {
        const list = await fetch('/api/v1/meta/tables').then(r => r.json()).catch(() => []);
        // /api/v1/meta/tables 返 ["<svc>:<table>", ...]
        const norm = (Array.isArray(list) ? list : []).map(s => {
          const [svc, table] = String(s).split(':');
          return { svc: svc || 'unknown', table: table || s, key: s };
        }).sort((a, b) => a.svc.localeCompare(b.svc) || a.table.localeCompare(b.table));
        this.schemaTables = norm;
        // 默认折叠超过 3 个 service 的展示
        const svcs = new Set(norm.map(t => t.svc));
        if (svcs.size > 3) {
          for (const s of svcs) this.schemaOpen[s] = false;
        }
      } catch (e) { console.error(e); }
    },

    async loadIndexKeys() {
      try {
        const k = await fetch('/api/v1/meta/idx_keys').then(r => r.json()).catch(() => []);
        this.indexKeys = Array.isArray(k) ? k : [];
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
        // Alpine 不会感知数组成员属性变化,touch 一下
        this.schemaTables = [...this.schemaTables];
        this.registerCompletion();
      } catch (e) { console.error(e); }
    },

    toggleSvc(svc) {
      this.schemaOpen[svc] = this.schemaOpen[svc] === false;
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

    // 注册 Monaco completion provider, 把 svc / table / column / idx 都喂进去
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
          // ctx.scan("<svc>", "<table>")
          if (/ctx\.scan\(\s*["']$/.test(before)) {
            const seen = new Set();
            for (const t of self.schemaTables) {
              if (seen.has(t.svc)) continue;
              seen.add(t.svc);
              items.push({ label: t.svc, kind: monaco.languages.CompletionItemKind.Module, insertText: t.svc });
            }
          } else if (/ctx\.scan\(\s*["'][^"']+["']\s*,\s*["']$/.test(before)) {
            const svcMatch = before.match(/ctx\.scan\(\s*["']([^"']+)["']/);
            if (svcMatch) {
              for (const t of self.schemaTables) {
                if (t.svc === svcMatch[1]) {
                  items.push({ label: t.table, kind: monaco.languages.CompletionItemKind.Struct, insertText: t.table });
                }
              }
            }
          } else if (/ctx\.get_by_index\(\s*["']$/.test(before)) {
            for (const k of self.indexKeys) {
              items.push({ label: k, kind: monaco.languages.CompletionItemKind.Field, insertText: k });
            }
          } else if (/\.(after|before)\[\s*["']$/.test(before) || /\.get\(\s*["']$/.test(before)) {
            // 列名补全 (从当前展开 table 或所有列汇总)
            const colSet = new Set();
            for (const t of self.schemaTables) {
              if (t.columns) for (const c of t.columns) colSet.add(c.name);
            }
            for (const c of colSet) {
              items.push({ label: c, kind: monaco.languages.CompletionItemKind.Property, insertText: c });
            }
          } else if (/ctx\.$/.test(before)) {
            // ctx 上的方法补全
            for (const m of ['scan(', 'get(', 'get_by_index(', 'scan_index(', 'now', 'params']) {
              items.push({ label: m, kind: monaco.languages.CompletionItemKind.Method, insertText: m });
            }
          }
          // Monaco 要求 range
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
      const code = monacoEditor.getValue();
      try {
        const r = await fetch('/api/v1/scripts/_dry_run', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ code }),
        }).then(r => r.json());
        if (r.error) {
          this.status = 'err'; this.errorMsg = r.error;
        } else {
          this.status = 'ok';
          this.diffs = r.diffs || [];
          this.diffsCount = this.diffs.length;
        }
      } catch (e) { this.status = 'err'; this.errorMsg = String(e); }
    },

    async save() {
      if (!this.meta.name) { alert('请填规则名'); return; }
      const code = monacoEditor.getValue();
      try {
        const url = this.scriptID
          ? '/api/v1/scripts/' + this.scriptID
          : '/api/v1/scripts';
        const method = this.scriptID ? 'PUT' : 'POST';
        const r = await fetch(url, {
          method,
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ ...this.meta, code }),
        });
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const data = await r.json().catch(() => ({}));
        if (!this.scriptID && data.id) {
          // 新建后跳到带 id URL
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
      if (!confirm('确认删除 ' + this.meta.name + '?该操作走 4-eyes 审批.')) return;
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

// 简易 toast
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
