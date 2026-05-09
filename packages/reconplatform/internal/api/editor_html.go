package api

// editorHTMLContent reconplatform admin web 的脚本编辑器页面（单文件 SPA）。
//
// 三栏布局：
//   - 左：脚本列表（CRUD）
//   - 中：Monaco editor（Starlark 语法高亮 + 自动补齐）
//   - 右：实时搜索面板（输入 pi_id=xxx 立即返跨服务关联事件）
//
// 顶部工具条：
//   - 语法检查（POST /scripts/<id>/validate）
//   - 试运行（POST /scripts/<id>/run）
//   - 保存（PUT /scripts/<id>）
//   - 历史结果（GET /scripts/<id>/results）
//
// 自动补齐由 /api/v1/script/symbols 端点驱动：
//   - load("@<TAB>          → 已注册的所有 module
//   - load("@json", "<TAB>  → 该 module 的成员
//   - ctx.<TAB>             → ctx 对象上的属性 / 方法
//   - .find(/.int(/.str(    → 配套提示参数
//
// engine.RegisterModule 动态加新包后，编辑器下次加载会立刻识别（/symbols 反射）。
//
// 资源全部用 CDN（Monaco editor），单进程二进制不带前端构建链。
const editorHTMLContent = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>reconplatform · 对账脚本编辑器</title>
<!-- Monaco editor: AMD loader 拉 0.46 (LTS-ish)。版本固定，避免 CDN 兜底劣化。-->
<link rel="stylesheet" data-name="vs/editor/editor.main"
      href="https://cdn.jsdelivr.net/npm/monaco-editor@0.46.0/min/vs/editor/editor.main.css">
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, BlinkMacSystemFont, "PingFang SC", "Microsoft YaHei", sans-serif; font-size: 13px; color: #1f2937; background: #f3f4f6; }
  header { background: #001529; color: #fff; padding: 10px 16px; display: flex; align-items: center; gap: 16px; }
  header h1 { font-size: 16px; font-weight: 500; }
  header .nav { margin-left: auto; display: flex; gap: 8px; }
  header button { background: #1677ff; color: #fff; border: 0; border-radius: 4px; padding: 6px 12px; cursor: pointer; font-size: 12px; }
  header button.secondary { background: #4b5563; }
  header button.danger { background: #dc2626; }
  header button:hover { opacity: 0.9; }
  .layout { display: grid; grid-template-columns: 280px 1fr 360px; height: calc(100vh - 44px); }
  .pane { background: #fff; border-right: 1px solid #e5e7eb; overflow: auto; }
  .pane-title { font-size: 11px; text-transform: uppercase; color: #6b7280; padding: 10px 16px; border-bottom: 1px solid #f3f4f6; background: #fafafa; font-weight: 600; letter-spacing: 0.5px; }
  /* 左：脚本列表 */
  .script-item { padding: 10px 16px; border-bottom: 1px solid #f3f4f6; cursor: pointer; }
  .script-item:hover { background: #f3f4f6; }
  .script-item.active { background: #e0f2fe; border-left: 3px solid #1677ff; }
  .script-item .name { font-weight: 500; color: #111827; }
  .script-item .meta { font-size: 11px; color: #6b7280; margin-top: 2px; }
  .new-btn { width: 100%; padding: 10px; background: #1677ff; color: #fff; border: 0; cursor: pointer; }
  .new-btn:hover { background: #146ce0; }
  /* 中：编辑器 */
  .editor-wrap { display: flex; flex-direction: column; }
  .editor-meta { padding: 10px 16px; background: #fafafa; border-bottom: 1px solid #e5e7eb; display: flex; gap: 12px; align-items: center; flex-wrap: wrap; }
  .editor-meta input { padding: 5px 8px; border: 1px solid #d1d5db; border-radius: 4px; font-size: 12px; }
  .editor-meta input.name { flex: 1; min-width: 200px; }
  .editor-meta input.cron { width: 140px; font-family: ui-monospace, monospace; }
  .editor-meta label { font-size: 11px; color: #6b7280; }
  textarea.code { flex: 1; width: 100%; border: 0; resize: none; padding: 12px; font-family: "SF Mono", Consolas, ui-monospace, monospace; font-size: 13px; line-height: 1.5; outline: none; tab-size: 4; background: #fafafa; color: #1f2937; }
  .status-bar { padding: 6px 16px; background: #f9fafb; border-top: 1px solid #e5e7eb; font-size: 11px; color: #6b7280; min-height: 24px; }
  .status-bar.error { background: #fee2e2; color: #991b1b; }
  .status-bar.ok { background: #dcfce7; color: #166534; }
  /* 右：搜索 + meta browser */
  .right-tabs { display: flex; border-bottom: 1px solid #e5e7eb; background: #fafafa; }
  .right-tabs button { flex: 1; padding: 8px; border: 0; background: transparent; cursor: pointer; font-size: 12px; color: #4b5563; }
  .right-tabs button.active { background: #fff; color: #1677ff; border-bottom: 2px solid #1677ff; }
  .search-box { padding: 10px 16px; border-bottom: 1px solid #f3f4f6; }
  .search-box select, .search-box input { width: 100%; padding: 6px 8px; border: 1px solid #d1d5db; border-radius: 4px; font-size: 12px; margin-bottom: 6px; }
  .results { padding: 10px 16px; overflow: auto; }
  .result-event { background: #f9fafb; border: 1px solid #e5e7eb; border-radius: 4px; padding: 8px; margin-bottom: 8px; }
  .result-event .head { font-size: 11px; color: #6b7280; margin-bottom: 4px; }
  .result-event .head .svc { color: #1677ff; font-weight: 500; }
  .result-event pre { font-family: ui-monospace, monospace; font-size: 11px; line-height: 1.5; max-height: 200px; overflow: auto; white-space: pre-wrap; word-break: break-all; }
  .meta-browser .table { padding: 6px 16px; cursor: pointer; font-family: ui-monospace, monospace; font-size: 11px; border-bottom: 1px solid #f3f4f6; }
  .meta-browser .table:hover { background: #f3f4f6; }
  .meta-browser .table .svc { color: #1677ff; }
  .meta-browser .columns { padding: 8px 16px; background: #fafafa; font-size: 11px; }
  .meta-browser .columns .col { padding: 2px 0; }
  .meta-browser .columns .col .name { font-family: ui-monospace, monospace; color: #111827; font-weight: 500; }
  .meta-browser .columns .col .type { color: #6b7280; margin-left: 8px; }
  .meta-browser .columns .col.pri .name::after { content: " 🔑"; }
  /* run result panel */
  .run-modal { position: fixed; inset: 0; background: rgba(0,0,0,0.4); display: none; align-items: center; justify-content: center; z-index: 100; }
  .run-modal.open { display: flex; }
  .run-modal-inner { background: #fff; border-radius: 8px; max-width: 800px; max-height: 80vh; width: 90%; padding: 20px; overflow: auto; }
  .run-modal-inner h2 { margin-bottom: 12px; font-size: 16px; }
  .run-modal-inner button.close { float: right; background: #6b7280; color: #fff; border: 0; padding: 6px 10px; border-radius: 4px; cursor: pointer; }
  .diff-list { margin-top: 12px; }
  .diff-item { background: #fef3c7; padding: 8px; margin-bottom: 6px; border-radius: 4px; border-left: 4px solid #f59e0b; }
  .diff-item .type { font-weight: 500; color: #92400e; }
  .diff-item pre { font-size: 11px; color: #4b5563; margin-top: 4px; white-space: pre-wrap; }
  .stats-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 10px; margin-top: 8px; }
  .stat { background: #f3f4f6; padding: 8px; border-radius: 4px; }
  .stat .label { font-size: 10px; color: #6b7280; text-transform: uppercase; }
  .stat .value { font-size: 16px; font-weight: 500; color: #111827; }
  /* version history modal */
  .history-modal { position: fixed; inset: 0; background: rgba(0,0,0,0.4); display: none; z-index: 110; }
  .history-modal.open { display: flex; }
  .history-modal-inner { background: #fff; border-radius: 8px; max-width: 1400px; max-height: 90vh; width: 95%; height: 85vh; margin: auto; padding: 16px; overflow: hidden; display: flex; flex-direction: column; }
  .history-header { display: flex; align-items: center; gap: 12px; padding-bottom: 8px; border-bottom: 1px solid #e5e7eb; }
  .history-header h2 { font-size: 16px; flex: 1; }
  .history-header .close-btn { background: #6b7280; color: #fff; border: 0; padding: 6px 10px; border-radius: 4px; cursor: pointer; }
  .history-body { display: grid; grid-template-columns: 220px 1fr; gap: 12px; flex: 1; min-height: 0; padding-top: 12px; }
  .history-versions { overflow: auto; border: 1px solid #e5e7eb; border-radius: 4px; }
  .history-versions .ver { padding: 10px 12px; cursor: pointer; border-bottom: 1px solid #f3f4f6; }
  .history-versions .ver:hover { background: #f3f4f6; }
  .history-versions .ver.active { background: #e0f2fe; border-left: 3px solid #1677ff; }
  .history-versions .ver .num { font-weight: 500; }
  .history-versions .ver .meta { font-size: 11px; color: #6b7280; margin-top: 2px; }
  .history-diff { border: 1px solid #e5e7eb; border-radius: 4px; overflow: hidden; }
</style>
</head>
<body>
<header>
  <h1>reconplatform · 对账脚本</h1>
  <div class="nav">
    <button class="secondary" id="btnValidate">语法检查 (Cmd+K)</button>
    <button class="secondary" id="btnDryRun">Dry Run (Cmd+D)</button>
    <button class="secondary" id="btnHistory">历史版本</button>
    <button id="btnRun">运行已保存版本 (Cmd+R)</button>
    <button id="btnSave">保存 (Cmd+S)</button>
  </div>
</header>

<div class="layout">
  <!-- 左：脚本列表 -->
  <div class="pane" id="leftPane">
    <div class="pane-title">脚本列表</div>
    <button class="new-btn" id="btnNew">+ 新建脚本</button>
    <div id="scriptList"></div>
  </div>

  <!-- 中：编辑器 -->
  <div class="pane editor-wrap">
    <div class="editor-meta">
      <input class="name" id="metaName" placeholder="脚本名称">
      <label>定时:</label>
      <input class="cron" id="metaSchedule" placeholder="*/15 * * * *">
      <label>事件触发:</label>
      <input class="cron" id="metaTriggers" placeholder='["svc:table"]' style="width:200px">
    </div>
    <div class="code" id="editorRoot" style="flex:1;width:100%;background:#fafafa;"></div>
    <div class="status-bar" id="statusBar">就绪</div>
  </div>

  <!-- 右：搜索 + meta -->
  <div class="pane">
    <div class="right-tabs">
      <button class="active" data-tab="search">实时搜索</button>
      <button data-tab="meta">表 schema</button>
    </div>
    <div id="tabSearch">
      <div class="search-box">
        <select id="searchIdx"></select>
        <input id="searchVal" placeholder="value（pi_xxx / ord_yyy / 留空模糊）">
        <button class="new-btn" id="btnSearch" style="background:#10b981">搜索</button>
      </div>
      <div class="results" id="searchResults"></div>
    </div>
    <div id="tabMeta" style="display:none" class="meta-browser">
      <div id="tableList"></div>
    </div>
  </div>
</div>

<!-- 历史版本 diff modal -->
<div class="history-modal" id="historyModal">
  <div class="history-modal-inner">
    <div class="history-header">
      <h2>版本历史 — <span id="historyTitle"></span></h2>
      <span style="color:#6b7280;font-size:12px">左：选中历史版本　→　右：当前编辑器代码</span>
      <button class="close-btn" id="btnCloseHistory">关闭</button>
    </div>
    <div class="history-body">
      <div class="history-versions" id="historyList"></div>
      <div class="history-diff" id="historyDiff"></div>
    </div>
  </div>
</div>

<!-- 运行结果 modal -->
<div class="run-modal" id="runModal">
  <div class="run-modal-inner">
    <button class="close" id="btnCloseModal">关闭</button>
    <h2 id="modalTitle">运行结果</h2>
    <div class="stats-grid" id="modalStats"></div>
    <div class="diff-list" id="modalDiffs"></div>
  </div>
</div>

<!-- Monaco AMD loader：用 require() 加载 vs/editor/editor.main 后才能 monaco.* -->
<script src="https://cdn.jsdelivr.net/npm/monaco-editor@0.46.0/min/vs/loader.js"></script>
<script>
require.config({ paths: { vs: 'https://cdn.jsdelivr.net/npm/monaco-editor@0.46.0/min/vs' } });

const api = path => fetch(path).then(r => r.json());
const apiPost = (path, body) => fetch(path, {
  method:'POST', headers:{'Content-Type':'application/json'}, body: body ? JSON.stringify(body) : undefined,
}).then(r => r.json());
const apiPut  = (path, body) => fetch(path, {
  method:'PUT', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body),
}).then(r => r.json());
const apiDel  = path => fetch(path, {method:'DELETE'}).then(r => r.json());

let currentID = null;
let editor = null;            // monaco editor 实例
let symbolsCache = null;       // /api/v1/script/symbols 返回的 schema
const editorReady = new Promise(resolve => { window._editorResolve = resolve; });

// editorValue: 统一抽象 monaco.getValue / setValue，避免每处都 if(editor)
const editorValue = {
  get: () => editor ? editor.getValue() : '',
  set: (v) => { if (editor) editor.setValue(v || ''); },
};

// 默认 starter Starlark 脚本（新建脚本时填充）
const starterStarlark = ` + "`" + `# 对账脚本入口：def check(ctx) → return [diff, ...]
#
# 引入包：
#   load("@json", "encode", "decode")
#   load("@time", "now", "parse_time")
#   load("@strings", "split", "to_lower")
#   load("@regex", "match")
#   load("@recon", "last_n_hours")

def check(ctx):
    diffs = []
    pi_ids = ctx.scan_index("pi_id", "", 1000)
    for pi in pi_ids:
        events = ctx.get_by_index("pi_id", pi)
        order = events.find("order-core", "payment_intents")
        chg = events.find("payment-channel", "card_charges")
        if order and chg and order.int("amount") != chg.int("amount"):
            diffs.append({
                "type": "amount_mismatch",
                "key": pi,
                "want": order.int("amount"),
                "got": chg.int("amount"),
            })
    ctx.log_info("done", "scanned", len(pi_ids), "diffs", len(diffs))
    return diffs
` + "`" + `;

function status(msg, kind='') {
  const el = document.getElementById('statusBar');
  el.textContent = msg;
  el.className = 'status-bar ' + kind;
}

async function loadScripts() {
  const r = await api('/api/v1/scripts');
  const list = document.getElementById('scriptList');
  list.innerHTML = '';
  (r.scripts || []).forEach(s => {
    const div = document.createElement('div');
    div.className = 'script-item' + (s.id === currentID ? ' active' : '');
    div.innerHTML = '<div class="name">' + (s.name || s.id) + '</div><div class="meta">' + (s.schedule || '手动') + ' · ' + new Date(s.updated_at).toLocaleString() + '</div>';
    div.onclick = () => selectScript(s.id);
    list.appendChild(div);
  });
}

async function selectScript(id) {
  currentID = id;
  const r = await api('/api/v1/scripts/' + id);
  document.getElementById('metaName').value = r.Name || '';
  document.getElementById('metaSchedule').value = r.Schedule || '';
  document.getElementById('metaTriggers').value = JSON.stringify(r.Triggers || []);
  await editorReady;
  editorValue.set(r.Code || '');
  status('已加载 ' + id, 'ok');
  loadScripts();
}

async function newScript() {
  currentID = null;
  document.getElementById('metaName').value = '';
  document.getElementById('metaSchedule').value = '';
  document.getElementById('metaTriggers').value = '[]';
  await editorReady;
  editorValue.set(starterStarlark);
  status('新脚本（未保存）');
  loadScripts();
}

async function validate() {
  const code = editorValue.get();
  const r = await apiPost(currentID ? '/api/v1/scripts/' + currentID + '/validate' : '/api/v1/scripts/_validate', { code });
  // 把后端 lint issues 转 monaco markers，左侧栏直接显示
  applyLintMarkers(r.issues || []);
  if (r.ok) {
    const warns = (r.issues || []).filter(i => i.severity !== 'info').length;
    status(warns ? '✓ 语法 OK · ' + warns + ' 条 lint 提示' : '✓ 语法 OK', 'ok');
  } else {
    status('✗ ' + (r.error || JSON.stringify(r)), 'error');
  }
}

// applyLintMarkers 把后端 LintIssue 转 monaco IMarkerData 显示在编辑器左侧栏。
//   error → 红，warn → 黄，info → 蓝
function applyLintMarkers(issues) {
  if (!editor || !window.monaco) return;
  const sevMap = {
    error: monaco.MarkerSeverity.Error,
    warn:  monaco.MarkerSeverity.Warning,
    info:  monaco.MarkerSeverity.Info,
  };
  const markers = issues.map(i => ({
    severity: sevMap[i.severity] || monaco.MarkerSeverity.Info,
    message:  '[' + i.code + '] ' + i.message,
    startLineNumber: i.line || 1,
    startColumn:     i.col || 1,
    endLineNumber:   i.line || 1,
    endColumn:       (i.col || 1) + (i.snippet ? i.snippet.length : 1),
  }));
  monaco.editor.setModelMarkers(editor.getModel(), 'recon-lint', markers);
}

async function save() {
  const body = {
    name: document.getElementById('metaName').value,
    code: editorValue.get(),
    schedule: document.getElementById('metaSchedule').value,
    triggers: JSON.parse(document.getElementById('metaTriggers').value || '[]'),
  };
  let r;
  if (currentID) r = await apiPut('/api/v1/scripts/' + currentID, body);
  else           r = await apiPost('/api/v1/scripts', body);
  if (r.error) { status('保存失败: ' + r.error, 'error'); return; }
  currentID = r.id;
  status('✓ 已保存 v' + r.version, 'ok');
  loadScripts();
}

async function runScript() {
  if (!currentID) { status('先保存再运行', 'error'); return; }
  status('运行中…');
  const r = await apiPost('/api/v1/scripts/' + currentID + '/run');
  if (r.error) { status('运行失败: ' + r.error, 'error'); return; }
  showResultModal(r);
  status(r.status === 'success' ? '✓ 运行成功，差异 ' + (r.diffs||[]).length + ' 条' : '✗ ' + r.error, r.status === 'success' ? 'ok' : 'error');
}

// dryRun: 把编辑器当前未保存代码丢给 /scripts/_dry_run，不写 Redis 不污染历史。
// 用途：写完一段先看 diff 输出符不符合预期，再决定保存。
async function dryRun() {
  const code = editorValue.get();
  if (!code.trim()) { status('编辑器为空', 'error'); return; }
  status('Dry Run 中…');
  const r = await apiPost('/api/v1/scripts/_dry_run', { code });
  if (r.error && !r.status) { status('Dry Run 失败: ' + r.error, 'error'); return; }
  showResultModal({ ...r, scriptID: '_dryrun' });
  if (r.status === 'success') {
    status('✓ Dry Run 完成，差异 ' + (r.diffs||[]).length + ' 条 (未保存)', 'ok');
  } else {
    status('✗ Dry Run: ' + (r.error || ''), 'error');
  }
}

function showResultModal(r) {
  document.getElementById('modalTitle').textContent = '运行结果 — ' + (r.status||'');
  const stats = r.stats || {};
  const dur = (new Date(r.finished_at) - new Date(r.started_at)) || 0;
  document.getElementById('modalStats').innerHTML =
    '<div class="stat"><div class="label">耗时</div><div class="value">' + dur + 'ms</div></div>' +
    '<div class="stat"><div class="label">差异</div><div class="value">' + (r.diffs||[]).length + '</div></div>' +
    '<div class="stat"><div class="label">索引查询</div><div class="value">' + (stats.index_lookups||0) + '</div></div>' +
    '<div class="stat"><div class="label">主存读取</div><div class="value">' + (stats.point_lookups||0) + '</div></div>';
  const diffsEl = document.getElementById('modalDiffs');
  if (!r.diffs || !r.diffs.length) {
    diffsEl.innerHTML = '<p style="text-align:center;color:#10b981;padding:20px">✓ 无差异</p>';
  } else {
    diffsEl.innerHTML = r.diffs.map(d =>
      '<div class="diff-item"><div class="type">' + d.type + ' · ' + d.key + '</div>' +
      '<pre>' + JSON.stringify(d.detail || {want: d.want, got: d.got}, null, 2) + '</pre></div>'
    ).join('');
  }
  document.getElementById('runModal').classList.add('open');
}

document.getElementById('btnCloseModal').onclick = () => document.getElementById('runModal').classList.remove('open');

async function loadIdxKeys() {
  const r = await api('/api/v1/meta/idx_keys');
  const sel = document.getElementById('searchIdx');
  sel.innerHTML = '';
  (r.index_keys || []).forEach(k => {
    const opt = document.createElement('option');
    opt.value = k;
    opt.textContent = k;
    sel.appendChild(opt);
  });
}

async function searchEvents() {
  const idx = document.getElementById('searchIdx').value;
  const val = document.getElementById('searchVal').value;
  const r = await api('/api/v1/search?index=' + encodeURIComponent(idx) + '&value=' + encodeURIComponent(val));
  const out = document.getElementById('searchResults');
  if (r.error) { out.textContent = r.error; return; }
  if (val) {
    out.innerHTML = (r.events || []).map(e =>
      '<div class="result-event"><div class="head"><span class="svc">' + e.svc + '</span> · ' + e.table + ' · pk=' + e.pk + ' · ' + e.op +
      '</div><pre>' + JSON.stringify(e.before || e.after || {}, null, 2) + '</pre></div>'
    ).join('') || '<p style="color:#6b7280">无结果</p>';
  } else {
    out.innerHTML = (r.values || []).map(v =>
      '<div class="result-event"><a href="#" onclick="document.getElementById(\'searchVal\').value=\'' + v + '\';searchEvents();return false">' + v + '</a></div>'
    ).join('') || '<p style="color:#6b7280">无结果</p>';
  }
}

async function loadTables() {
  const r = await api('/api/v1/meta/tables');
  const list = document.getElementById('tableList');
  list.innerHTML = '';
  (r.tables || []).sort().forEach(t => {
    const [svc, table] = t.split(':');
    const div = document.createElement('div');
    div.className = 'table';
    div.innerHTML = '<span class="svc">' + svc + '</span>.' + table;
    div.onclick = () => loadColumns(div, svc, table);
    list.appendChild(div);
  });
}

async function loadColumns(rowEl, svc, table) {
  const next = rowEl.nextElementSibling;
  if (next && next.classList.contains('columns')) { next.remove(); return; }
  const cols = await fetch('/api/v1/meta/schema/' + svc + '/' + table).then(r => r.json());
  const wrap = document.createElement('div');
  wrap.className = 'columns';
  wrap.innerHTML = cols.map(c =>
    '<div class="col' + (c.key === 'PRI' ? ' pri' : '') + '"><span class="name">' + c.name + '</span><span class="type">' + c.type + '</span></div>'
  ).join('');
  rowEl.after(wrap);
}

document.querySelectorAll('.right-tabs button').forEach(b => b.onclick = () => {
  document.querySelectorAll('.right-tabs button').forEach(x => x.classList.remove('active'));
  b.classList.add('active');
  document.getElementById('tabSearch').style.display = b.dataset.tab === 'search' ? '' : 'none';
  document.getElementById('tabMeta').style.display = b.dataset.tab === 'meta' ? '' : 'none';
  if (b.dataset.tab === 'meta') loadTables();
});

document.getElementById('btnNew').onclick = newScript;
document.getElementById('btnSave').onclick = save;
document.getElementById('btnRun').onclick = runScript;
document.getElementById('btnDryRun').onclick = dryRun;
document.getElementById('btnValidate').onclick = validate;
document.getElementById('btnSearch').onclick = searchEvents;
document.getElementById('btnHistory').onclick = openHistory;
document.getElementById('btnCloseHistory').onclick = () => {
  document.getElementById('historyModal').classList.remove('open');
  if (diffEditor) { diffEditor.dispose(); diffEditor = null; }
};

// ─── 版本历史 + diff view ────────────────────────────────────
//
// 复用后端已有端点：
//   GET /api/v1/scripts/:id/versions    返 [{version, saved_at, updated_by}]
//   GET /api/v1/scripts/:id/versions/:n 返 {version, code, ...}
//
// 用 monaco.editor.createDiffEditor 双栏对比：
//   左 = 选中的历史版本     右 = 当前编辑器代码（live；用户没保存的改动也能看出来）
let diffEditor = null;            // monaco diff editor 实例
let historyVersions = [];          // 缓存当前脚本的版本列表

async function openHistory() {
  if (!currentID) { status('请先选中或保存一个脚本', 'error'); return; }
  document.getElementById('historyTitle').textContent = currentID;
  const modal = document.getElementById('historyModal');
  modal.classList.add('open');

  try {
    const r = await api('/api/v1/scripts/' + currentID + '/versions');
    historyVersions = r.versions || [];
  } catch (e) {
    historyVersions = [];
  }
  renderVersionList();
  // 默认选最新历史版本（list[0]）vs 当前编辑器
  if (historyVersions.length > 0) {
    selectVersion(historyVersions[0].version);
  } else {
    document.getElementById('historyDiff').innerHTML =
      '<p style="padding:20px;color:#6b7280;text-align:center">暂无历史版本（首次保存后才会有）</p>';
  }
}

function renderVersionList() {
  const list = document.getElementById('historyList');
  if (!historyVersions.length) {
    list.innerHTML = '<p style="padding:12px;color:#6b7280">无历史版本</p>';
    return;
  }
  list.innerHTML = historyVersions.map(v =>
    '<div class="ver" data-ver="' + v.version + '">' +
    '<div class="num">v' + v.version + '</div>' +
    '<div class="meta">' + (v.updated_by || '?') + ' · ' +
    (v.saved_at ? new Date(v.saved_at).toLocaleString() : '') + '</div></div>'
  ).join('');
  list.querySelectorAll('.ver').forEach(el => {
    el.onclick = () => selectVersion(parseInt(el.dataset.ver, 10));
  });
}

async function selectVersion(ver) {
  // 高亮选中
  document.querySelectorAll('.history-versions .ver').forEach(el => {
    el.classList.toggle('active', parseInt(el.dataset.ver, 10) === ver);
  });
  // 拉版本详情
  let oldCode = '';
  try {
    const r = await api('/api/v1/scripts/' + currentID + '/versions/' + ver);
    oldCode = r.code || r.Code || '';
  } catch (e) {
    oldCode = '// 拉取版本失败: ' + e;
  }
  // 当前编辑器代码（live）作为对照
  const newCode = editorValue.get();
  // 销毁前一个 diff editor 防内存泄漏
  if (diffEditor) { diffEditor.dispose(); diffEditor = null; }
  // 起新 diff editor
  diffEditor = monaco.editor.createDiffEditor(document.getElementById('historyDiff'), {
    language: 'python',
    theme: 'vs',
    fontSize: 12,
    automaticLayout: true,
    renderSideBySide: true,
    readOnly: true,
    originalEditable: false,
    minimap: { enabled: false },
  });
  diffEditor.setModel({
    original: monaco.editor.createModel(oldCode, 'python'),
    modified: monaco.editor.createModel(newCode, 'python'),
  });
}

document.addEventListener('keydown', e => {
  if ((e.metaKey || e.ctrlKey) && e.key === 's') { e.preventDefault(); save(); }
  if ((e.metaKey || e.ctrlKey) && e.key === 'r') { e.preventDefault(); runScript(); }
  if ((e.metaKey || e.ctrlKey) && e.key === 'k') { e.preventDefault(); validate(); }
  if ((e.metaKey || e.ctrlKey) && e.key === 'd') { e.preventDefault(); dryRun(); }
});

// ─── Monaco 初始化 + Starlark 自动补齐 ────────────────────────────────
//
// Starlark 是 Python 子集，直接复用 monaco 内置 'python' 语言做语法高亮 +
// 缩进自动跟。补齐通过 completionItemProvider 调 /api/v1/script/symbols
// 拿 schema：
//
//   - load("@<TAB>          → modules
//   - load("@<mod>", "<TAB> → 该 module 的 members
//   - ctx.<TAB>             → ctx 上的属性 / 方法
//   - .find( / .int(        → events / event 的方法
//
// engine.RegisterModule 后下次 ctrl+r 重载页面立即识别（symbolsCache 重新拉）。

require(['vs/editor/editor.main'], async function () {
  // 拉一次 symbols，给 completion 用
  try {
    symbolsCache = await api('/api/v1/script/symbols');
  } catch (e) {
    console.warn('symbols load failed', e);
    symbolsCache = { modules: [], ctx: [], event: [], event_list: [] };
  }

  // 直接复用 monaco 内置 'python' 做语法高亮；Starlark 是 Python 子集，
  // python tokenizer 的 def / for / if / dict / list / 字符串 / 注释规则
  // 都对得上。未来若需要更精细（区分 Starlark 不允许的 class / yield 等），
  // 再注册独立 Monarch tokens provider。
  // ── hover provider ────────────────────────────────────────
  // 把 /symbols 端点的 type 注释做成光标悬停弹窗。
  //
  //   ctx       → "recon.Context"
  //   ctx.now   → "string (RFC3339)"
  //   ctx.scan_index → "method (idx_name, prefix='', limit=1000) → list[str]"
  //   load("@json", "encode") 中的 encode → "builtin_function_or_method"
  monaco.languages.registerHoverProvider('python', {
    provideHover: (model, position) => {
      const word = model.getWordAtPosition(position);
      if (!word) return null;
      const lineText = model.getLineContent(position.lineNumber);
      const before = lineText.slice(0, word.startColumn - 1);

      // 1) ctx.<word> → 找 ctx symbol
      if (/\bctx\.$/.test(before)) {
        const sym = (symbolsCache.ctx || []).find(c => c.name === word.word);
        if (sym) return makeHover(sym, 'ctx');
      }

      // 2) word 本身就是 ctx
      if (word.word === 'ctx') {
        return { range: rangeOfWord(word, position), contents: [
          { value: '**ctx** — `recon.Context`' },
          { value: '当前对账脚本运行的上下文。属性：' + (symbolsCache.ctx || []).map(s => '`' + s.name + '`').join(', ') },
        ]};
      }

      // 3) 启发式 var.<word>：events / event 类对象
      const varDot = /(\w+)\.$/.exec(before);
      if (varDot) {
        const v = varDot[1];
        let pool = null, ownerType = '';
        if (v === 'events' || v.endsWith('_events')) {
          pool = symbolsCache.event_list || []; ownerType = 'recon.EventList';
        } else if (v === 'event' || ['order', 'txn', 'chg', 'pi', 'charge', 'transaction'].indexOf(v) >= 0) {
          pool = symbolsCache.event || []; ownerType = 'recon.Event';
        }
        if (pool) {
          const sym = pool.find(s => s.name === word.word);
          if (sym) return makeHover(sym, ownerType);
        }
      }

      // 4) load("@<word>") → 模块名提示
      if (/load\s*\(\s*"@$/.test(before)) {
        const mod = (symbolsCache.modules || []).find(m => m.name === word.word);
        if (mod) {
          return { range: rangeOfWord(word, position), contents: [
            { value: '**module @' + mod.name + '** — ' + (mod.members || []).length + ' 个成员' },
            { value: (mod.members || []).map(m => '- `' + m.name + '` (' + m.type + ')').join('\n') },
          ]};
        }
      }

      // 5) load("@mod", "<word>") → module 成员
      const loadMember = /load\s*\(\s*"@([a-zA-Z0-9_]+)"\s*,\s*"$/.exec(before);
      if (loadMember) {
        const mod = (symbolsCache.modules || []).find(m => m.name === loadMember[1]);
        if (mod) {
          const sym = (mod.members || []).find(s => s.name === word.word);
          if (sym) return makeHover(sym, '@' + mod.name);
        }
      }
      return null;
    },
  });
  function rangeOfWord(word, pos) {
    return new monaco.Range(pos.lineNumber, word.startColumn, pos.lineNumber, word.endColumn);
  }
  function makeHover(sym, owner) {
    return {
      // 不传 range → monaco 用当前 word 自动算高亮
      contents: [
        { value: '**' + sym.name + '** — `' + sym.type + '`' + (owner ? '  \n_owner: `' + owner + '`_' : '') },
      ],
    };
  }

  // ── completion provider ───────────────────────────────────
  monaco.languages.registerCompletionItemProvider('python', {
    triggerCharacters: ['.', '"', '@', '('],
    provideCompletionItems: (model, position) => {
      const lineText = model.getLineContent(position.lineNumber).slice(0, position.column - 1);
      const word = model.getWordUntilPosition(position);
      const range = {
        startLineNumber: position.lineNumber,
        endLineNumber: position.lineNumber,
        startColumn: word.startColumn,
        endColumn: word.endColumn,
      };
      const Kind = monaco.languages.CompletionItemKind;
      const kindFor = t => {
        if (!t) return Kind.Property;
        if (t.indexOf('method') === 0 || t.indexOf('function') >= 0) return Kind.Method;
        return Kind.Property;
      };

      // 1) load("@<prefix>  → modules
      let m = /load\s*\(\s*"@([a-zA-Z0-9_]*)$/.exec(lineText);
      if (m) {
        return {
          suggestions: (symbolsCache.modules || []).map(mod => ({
            label: mod.name,
            kind: Kind.Module,
            insertText: mod.name,
            detail: 'module',
            documentation: (mod.members || []).map(x => x.name).join(', '),
            range,
          })),
        };
      }

      // 2) load("@json", "<prefix>  → members of that module
      m = /load\s*\(\s*"@([a-zA-Z0-9_]+)"\s*,\s*"([a-zA-Z0-9_]*)$/.exec(lineText);
      if (m) {
        const mod = (symbolsCache.modules || []).find(x => x.name === m[1]);
        if (!mod) return { suggestions: [] };
        return {
          suggestions: (mod.members || []).map(s => ({
            label: s.name,
            kind: kindFor(s.type),
            insertText: s.name,
            detail: s.type,
            range,
          })),
        };
      }

      // 3) ctx.<prefix>  → ctx 属性 / 方法
      if (/\bctx\.\w*$/.test(lineText)) {
        return {
          suggestions: (symbolsCache.ctx || []).map(c => ({
            label: c.name,
            kind: kindFor(c.type),
            insertText: c.name,
            detail: c.type,
            range,
          })),
        };
      }

      // 4) <var>.<prefix> 启发式：var 名是 events / event 时给对应清单。
      // 实际项目可以做得更聪明（解析赋值 events = ctx.get_by_index 链），
      // 但 95% 的脚本里变量名就叫 events / event / order / chg / txn。
      const varDot = /(\w+)\.\w*$/.exec(lineText);
      if (varDot) {
        const v = varDot[1];
        let pool = null;
        if (v === 'events' || v.endsWith('_events')) {
          pool = symbolsCache.event_list || [];
        } else if (v === 'event' || ['order', 'txn', 'chg', 'pi', 'charge', 'transaction'].indexOf(v) >= 0) {
          pool = symbolsCache.event || [];
        }
        if (pool) {
          return {
            suggestions: pool.map(s => ({
              label: s.name,
              kind: kindFor(s.type),
              insertText: s.name,
              detail: s.type,
              range,
            })),
          };
        }
      }

      // 默认无补齐 → monaco 走自带 keyword 补齐
      return { suggestions: [] };
    },
  });

  // ── 创建 editor 实例 ───────────────────────────────────────
  editor = monaco.editor.create(document.getElementById('editorRoot'), {
    value: starterStarlark,
    language: 'python',          // Starlark = Python 子集，复用高亮规则
    theme: 'vs',                  // 浅色主题；想暗色改 'vs-dark'
    fontSize: 13,
    automaticLayout: true,        // 容器尺寸变 → editor 自适应
    minimap: { enabled: false },
    scrollBeyondLastLine: false,
    tabSize: 4,
    insertSpaces: true,
    renderWhitespace: 'boundary',
    smoothScrolling: true,
    wordWrap: 'off',
    fixedOverflowWidgets: true,   // 补齐弹窗 z-index 跟 modal 不打架
  });

  // 快捷键：Cmd+S 保存 / Cmd+R 运行 / Cmd+K 校验
  editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, save);
  editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyR, runScript);
  editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyK, validate);

  window._editorResolve();
  status('编辑器就绪 · ' + ((symbolsCache.modules || []).length) + ' 个模块可补齐', 'ok');
});

loadScripts();
loadIdxKeys();
</script>
</body>
</html>`
