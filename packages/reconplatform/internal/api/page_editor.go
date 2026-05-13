// page_editor.go — /admin/editor 升级版规则编辑器.
//
// 在共用 layout 内嵌入 Monaco + 现有 editor 功能,加:
//   - 左侧 outline (脚本符号 + 历史版本树)
//   - 右侧试运行 inline 预览
//   - 顶部:lint / dry-run / save / fork / 版本 diff
//
// 旧 editor_html.go (1627 行 SPA) 保留为 /admin/editor/legacy 兼容直接 bookmark.
package api

import "net/http"

func (s *Server) pageEditor(w http.ResponseWriter, r *http.Request) {
	scriptID := r.URL.Query().Get("id")
	body := `
<div x-data="editorModel('` + scriptID + `')" x-init="load()" class="grid grid-cols-12 gap-4 h-[calc(100vh-7rem)]">

  <!-- 左:outline + 历史 -->
  <aside class="col-span-2 bg-white rounded-lg border border-slate-200 overflow-hidden flex flex-col">
    <div class="px-3 py-2 border-b border-slate-200 bg-slate-50">
      <h3 class="text-xs font-semibold uppercase tracking-wider text-slate-500">大纲</h3>
    </div>
    <ul class="px-2 py-2 flex-1 overflow-y-auto space-y-0.5 text-xs">
      <template x-for="sym in outline" :key="sym.name">
        <li class="flex items-center gap-1.5 px-2 py-1 hover:bg-slate-50 rounded cursor-pointer"
            @click="jumpTo(sym.line)">
          <i :data-lucide="sym.kind === 'function' ? 'square-function' : 'square-code'"
             class="icon w-3 h-3 text-slate-400"></i>
          <span class="mono" x-text="sym.name"></span>
          <span class="text-slate-400 ml-auto" x-text="sym.line"></span>
        </li>
      </template>
      <li x-show="outline.length===0" class="text-slate-400 px-2 py-2">解析中...</li>
    </ul>

    <div class="border-t border-slate-200 bg-slate-50 px-3 py-2">
      <h3 class="text-xs font-semibold uppercase tracking-wider text-slate-500">版本历史</h3>
    </div>
    <ul class="px-2 py-2 flex-1 overflow-y-auto space-y-1 text-xs">
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
  </aside>

  <!-- 中:编辑器 -->
  <section class="col-span-7 flex flex-col">
    <div class="bg-white rounded-t-lg border border-b-0 border-slate-200 px-3 py-2 flex items-center gap-2 shrink-0">
      <input type="text" x-model="meta.name" placeholder="rule_name"
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
    </div>
    <div id="monaco" class="flex-1 border border-slate-200 bg-white rounded-b-lg overflow-hidden"></div>
  </section>

  <!-- 右:试运行结果 / lint 输出 -->
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
require.config({ paths: { vs: 'https://cdn.jsdelivr.net/npm/monaco-editor@0.46.0/min/vs' } });
require(['vs/editor/editor.main'], function() {
  monacoEditor = monaco.editor.create(document.getElementById('monaco'), {
    value: '# 选择左侧规则或编写新规则\ndef check(ctx):\n    return []\n',
    language: 'python',  // Starlark 与 Python 90% 同语法
    theme: 'vs',
    minimap: { enabled: false },
    fontFamily: 'JetBrains Mono, Menlo, monospace',
    fontSize: 13,
    automaticLayout: true,
  });
});

function editorModel(scriptID) {
  return {
    scriptID,
    meta: { name: '', severity: 'warning' },
    outline: [], versions: [],
    diffs: [], errorMsg: '',
    status: '', diffsCount: 0,

    async load() {
      if (!this.scriptID) return;
      try {
        const r = await fetch('/api/v1/scripts/' + this.scriptID).then(r => r.json());
        this.meta.name     = r.name;
        this.meta.severity = r.severity || 'warning';
        const waitMonaco = setInterval(() => {
          if (monacoEditor) {
            monacoEditor.setValue(r.code || '');
            this.parseOutline(r.code || '');
            clearInterval(waitMonaco);
          }
        }, 50);
        // 历史
        try {
          const v = await fetch('/api/v1/scripts/' + this.scriptID + '/versions').then(r => r.json());
          this.versions = v || [];
        } catch (_) {}
      } catch (e) { console.error(e); }
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
      const code = monacoEditor.getValue();
      try {
        const url = this.scriptID
          ? '/api/v1/scripts/' + this.scriptID
          : '/api/v1/scripts';
        const method = this.scriptID ? 'PUT' : 'POST';
        await fetch(url, {
          method,
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ ...this.meta, code }),
        });
        alert('保存成功');
        this.parseOutline(code);
      } catch (e) { alert('保存失败: ' + e); }
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
</script>
`
	adminPage(w, "规则编辑器", "editor", body, extraHead, script)
}
