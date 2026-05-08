package api

// editorHTMLContent reconplatform admin web 的脚本编辑器页面（单文件 SPA）。
//
// 三栏布局：
//   - 左：脚本列表（CRUD）
//   - 中：CodeMirror Go 编辑器（语法高亮 + autocomplete from /api/v1/meta）
//   - 右：实时搜索面板（输入 pi_id=xxx 立即返跨服务关联事件）
//
// 顶部工具条：
//   - 语法检查（POST /scripts/<id>/validate）
//   - 试运行（POST /scripts/<id>/run）
//   - 保存（PUT /scripts/<id>）
//   - 历史结果（GET /scripts/<id>/results）
//
// 资源全部用 CDN（codemirror 6 + alpine.js）让单进程二进制不带前端构建链。
const editorHTMLContent = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>reconplatform · 对账脚本编辑器</title>
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
</style>
</head>
<body>
<header>
  <h1>reconplatform · 对账脚本</h1>
  <div class="nav">
    <button class="secondary" id="btnValidate">语法检查 (Cmd+K)</button>
    <button id="btnRun">试运行 (Cmd+R)</button>
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
    <textarea class="code" id="codeEditor" placeholder="// 在此处编写对账脚本
//
// package main
//
// import (
//     &quot;fmt&quot;
//     &quot;recon&quot;
// )
//
// func Check(ctx *recon.Context) (*recon.Result, error) {
//     piIDs := ctx.ScanIndex(&quot;pi_id&quot;, &quot;&quot;, 1000)
//     for _, pi := range piIDs {
//         events := ctx.GetByIndex(&quot;pi_id&quot;, pi)
//         order := events.Find(&quot;order-core&quot;, &quot;payment_intents&quot;)
//         chg   := events.Find(&quot;payment-channel&quot;, &quot;card_charges&quot;)
//         if order.Int(&quot;amount&quot;) != chg.Int(&quot;amount&quot;) {
//             ctx.AddCompare(&quot;amount_mismatch&quot;, pi, order.Int(&quot;amount&quot;), chg.Int(&quot;amount&quot;))
//         }
//     }
//     return &amp;recon.Result{}, nil
// }
"></textarea>
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

<!-- 运行结果 modal -->
<div class="run-modal" id="runModal">
  <div class="run-modal-inner">
    <button class="close" id="btnCloseModal">关闭</button>
    <h2 id="modalTitle">运行结果</h2>
    <div class="stats-grid" id="modalStats"></div>
    <div class="diff-list" id="modalDiffs"></div>
  </div>
</div>

<script>
const api = path => fetch(path).then(r => r.json());
const apiPost = (path, body) => fetch(path, {
  method:'POST', headers:{'Content-Type':'application/json'}, body: body ? JSON.stringify(body) : undefined,
}).then(r => r.json());
const apiPut  = (path, body) => fetch(path, {
  method:'PUT', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body),
}).then(r => r.json());
const apiDel  = path => fetch(path, {method:'DELETE'}).then(r => r.json());

let currentID = null;

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
  document.getElementById('codeEditor').value = r.Code || '';
  status('已加载 ' + id, 'ok');
  loadScripts();
}

function newScript() {
  currentID = null;
  document.getElementById('metaName').value = '';
  document.getElementById('metaSchedule').value = '';
  document.getElementById('metaTriggers').value = '[]';
  document.getElementById('codeEditor').value = 'package main\n\nimport (\n    "fmt"\n    "recon"\n)\n\nfunc Check(ctx *recon.Context) (*recon.Result, error) {\n    fmt.Println("hello recon")\n    return &recon.Result{}, nil\n}\n';
  status('新脚本（未保存）');
  loadScripts();
}

async function validate() {
  const code = document.getElementById('codeEditor').value;
  const r = await apiPost(currentID ? '/api/v1/scripts/' + currentID + '/validate' : '/api/v1/scripts/_validate', { code });
  if (r.ok) status('✓ 语法 OK', 'ok');
  else      status('✗ ' + (r.error || JSON.stringify(r)), 'error');
}

async function save() {
  const body = {
    name: document.getElementById('metaName').value,
    code: document.getElementById('codeEditor').value,
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
document.getElementById('btnValidate').onclick = validate;
document.getElementById('btnSearch').onclick = searchEvents;

document.addEventListener('keydown', e => {
  if ((e.metaKey || e.ctrlKey) && e.key === 's') { e.preventDefault(); save(); }
  if ((e.metaKey || e.ctrlKey) && e.key === 'r') { e.preventDefault(); runScript(); }
  if ((e.metaKey || e.ctrlKey) && e.key === 'k') { e.preventDefault(); validate(); }
});

loadScripts();
loadIdxKeys();
</script>
</body>
</html>`
