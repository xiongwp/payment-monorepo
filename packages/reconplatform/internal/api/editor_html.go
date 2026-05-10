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
<!-- Cytoscape: 跨服务事件关联图 -->
<script src="https://cdn.jsdelivr.net/npm/cytoscape@3.28.1/dist/cytoscape.min.js"></script>
<!-- Chart.js: dashboard 跨年趋势柱状 -->
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js"></script>
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
  /* Generic full-screen modal (dashboard / graph / dsl / live) */
  .full-modal { position: fixed; inset: 0; background: rgba(0,0,0,0.4); display: none; z-index: 120; }
  .full-modal.open { display: flex; }
  .full-modal-inner { background: #fff; border-radius: 8px; max-width: 1400px; max-height: 92vh; width: 95%; margin: auto; padding: 16px; overflow: hidden; display: flex; flex-direction: column; }
  .full-modal-inner .header { display: flex; align-items: center; gap: 12px; padding-bottom: 8px; border-bottom: 1px solid #e5e7eb; margin-bottom: 12px; }
  .full-modal-inner .header h2 { font-size: 16px; flex: 1; }
  .full-modal-inner .header .close-btn { background: #6b7280; color: #fff; border: 0; padding: 6px 10px; border-radius: 4px; cursor: pointer; }
  .full-modal-inner .body { flex: 1; min-height: 0; overflow: auto; }
  /* dashboard cards */
  .kpi-grid { display: grid; grid-template-columns: repeat(5, 1fr); gap: 12px; margin-bottom: 16px; }
  .kpi-card { background: #f9fafb; border: 1px solid #e5e7eb; padding: 14px; border-radius: 6px; }
  .kpi-card .label { font-size: 11px; color: #6b7280; text-transform: uppercase; letter-spacing: 0.5px; }
  .kpi-card .value { font-size: 24px; font-weight: 600; color: #111827; margin-top: 4px; }
  .kpi-card.alert { border-left: 3px solid #f59e0b; background: #fef3c7; }
  .kpi-card.danger { border-left: 3px solid #dc2626; background: #fee2e2; }
  .kpi-card.ok { border-left: 3px solid #10b981; background: #dcfce7; }
  .kpi-row-2col { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
  .kpi-bar-list .bar { display: flex; align-items: center; gap: 8px; padding: 4px 0; font-size: 12px; }
  .kpi-bar-list .bar .name { width: 200px; color: #111827; font-family: ui-monospace, monospace; font-size: 11px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .kpi-bar-list .bar .fill { height: 14px; background: #1677ff; border-radius: 2px; }
  .kpi-bar-list .bar .count { width: 40px; text-align: right; color: #6b7280; }
  /* cytoscape graph */
  #graphCanvas { width: 100%; height: 600px; border: 1px solid #e5e7eb; border-radius: 4px; }
  .graph-controls { display: flex; gap: 8px; align-items: center; margin-bottom: 12px; padding: 12px; background: #f9fafb; border-radius: 4px; }
  .graph-controls input, .graph-controls select { padding: 5px 8px; border: 1px solid #d1d5db; border-radius: 4px; font-size: 12px; }
  .graph-controls button { background: #1677ff; color: #fff; border: 0; border-radius: 4px; padding: 6px 12px; cursor: pointer; }
  /* DSL form */
  .dsl-grid { display: grid; grid-template-columns: 280px 1fr; gap: 16px; height: 70vh; }
  .dsl-templates { border: 1px solid #e5e7eb; border-radius: 4px; overflow: auto; }
  .dsl-templates .tpl { padding: 12px; cursor: pointer; border-bottom: 1px solid #f3f4f6; }
  .dsl-templates .tpl:hover { background: #f3f4f6; }
  .dsl-templates .tpl.active { background: #e0f2fe; border-left: 3px solid #1677ff; }
  .dsl-templates .tpl .name { font-weight: 500; }
  .dsl-templates .tpl .desc { font-size: 11px; color: #6b7280; margin-top: 4px; }
  .dsl-form { display: flex; flex-direction: column; gap: 12px; padding: 12px; }
  .dsl-form .field { display: grid; grid-template-columns: 160px 1fr; gap: 12px; align-items: center; }
  .dsl-form .field label { font-size: 12px; color: #4b5563; font-weight: 500; }
  .dsl-form .field input, .dsl-form .field select { padding: 6px 8px; border: 1px solid #d1d5db; border-radius: 4px; font-size: 12px; }
  .dsl-form .field .hint { grid-column: 2; font-size: 10px; color: #9ca3af; }
  .dsl-form .footer { margin-top: auto; display: flex; gap: 8px; }
  .dsl-form .footer button { background: #1677ff; color: #fff; border: 0; padding: 8px 16px; border-radius: 4px; cursor: pointer; }
  .dsl-form .footer button.secondary { background: #6b7280; }
  /* SSE live stream */
  #liveLog { font-family: ui-monospace, monospace; font-size: 11px; line-height: 1.5; height: 70vh; overflow: auto; background: #0f172a; color: #e2e8f0; padding: 12px; border-radius: 4px; }
  #liveLog .ev { padding: 2px 0; }
  #liveLog .ev .ts { color: #64748b; margin-right: 8px; }
  #liveLog .ev .svc { color: #38bdf8; }
  #liveLog .ev .table { color: #a78bfa; }
  #liveLog .ev .op { color: #fbbf24; }
  #liveLog .ev .pk { color: #4ade80; }
  .live-status { display: flex; gap: 12px; align-items: center; margin-bottom: 12px; }
  .live-status .dot { width: 10px; height: 10px; border-radius: 50%; background: #6b7280; }
  .live-status .dot.connected { background: #10b981; animation: pulse 2s infinite; }
  @keyframes pulse { 0%,100% { opacity: 1; } 50% { opacity: 0.4; } }
  /* Diffs search table */
  .diffs-table { width: 100%; border-collapse: collapse; font-size: 12px; }
  .diffs-table th { background: #f3f4f6; padding: 8px; text-align: left; border-bottom: 2px solid #e5e7eb; font-weight: 500; color: #4b5563; position: sticky; top: 0; }
  .diffs-table td { padding: 8px; border-bottom: 1px solid #f3f4f6; vertical-align: top; }
  .diffs-table tr:hover { background: #f9fafb; }
  .diffs-table .col-id { font-family: ui-monospace, monospace; font-size: 10px; color: #6b7280; }
  .diffs-table .col-state { font-weight: 500; }
  .diffs-table .col-state.open { color: #f59e0b; }
  .diffs-table .col-state.acked { color: #1677ff; }
  .diffs-table .col-state.resolved { color: #10b981; }
  .diffs-table .col-state.false_positive { color: #6b7280; }
  .diffs-table .col-state.expired { color: #dc2626; }
  .diffs-table .tier-badge { display: inline-block; padding: 1px 6px; border-radius: 3px; font-size: 10px; margin-left: 4px; }
  .diffs-table .tier-badge.hot { background: #fee2e2; color: #dc2626; }
  .diffs-table .tier-badge.cold { background: #dbeafe; color: #1677ff; }
  .diffs-table .jaeger-btn { background: #8b5cf6; color: #fff; border: 0; padding: 3px 8px; border-radius: 3px; cursor: pointer; font-size: 11px; }
  .diffs-table .jaeger-btn:disabled { background: #d1d5db; cursor: not-allowed; }
</style>
</head>
<body>
<header>
  <h1>reconplatform · 对账脚本</h1>
  <div class="nav">
    <button class="secondary" id="btnDashboard">📊 Dashboard</button>
    <button class="secondary" id="btnDiffs">🔍 Diffs</button>
    <button class="secondary" id="btnGraph">🔗 Graph</button>
    <button class="secondary" id="btnTemplates">🧩 Templates</button>
    <button class="secondary" id="btnLive">📡 Live</button>
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

<!-- Dashboard modal -->
<div class="full-modal" id="dashboardModal">
  <div class="full-modal-inner">
    <div class="header">
      <h2>📊 对账平台 Dashboard</h2>
      <button class="close-btn" onclick="closeModal('dashboardModal')">关闭</button>
    </div>
    <div class="body" id="dashboardBody">加载中...</div>
  </div>
</div>

<!-- Graph modal (cytoscape 跨服务事件关联图) -->
<div class="full-modal" id="graphModal">
  <div class="full-modal-inner">
    <div class="header">
      <h2>🔗 跨服务事件关联图</h2>
      <button class="close-btn" onclick="closeModal('graphModal')">关闭</button>
    </div>
    <div class="body">
      <div class="graph-controls">
        <select id="graphIdx"></select>
        <input id="graphVal" placeholder="value (例 pi_xxx)" style="flex:1">
        <input id="graphDepth" type="number" placeholder="depth" value="3" style="width:80px">
        <button onclick="loadGraph()">查询关联图</button>
        <span id="graphStats" style="color:#6b7280;font-size:12px"></span>
      </div>
      <div id="graphCanvas"></div>
    </div>
  </div>
</div>

<!-- DSL templates modal -->
<div class="full-modal" id="dslModal">
  <div class="full-modal-inner">
    <div class="header">
      <h2>🧩 DSL 模板 — 不写代码生成 Starlark</h2>
      <button class="close-btn" onclick="closeModal('dslModal')">关闭</button>
    </div>
    <div class="body">
      <div class="dsl-grid">
        <div class="dsl-templates" id="dslTplList"></div>
        <div class="dsl-form" id="dslForm">
          <p style="color:#6b7280;text-align:center;margin-top:80px">← 请先在左侧选择一个模板</p>
        </div>
      </div>
    </div>
  </div>
</div>

<!-- Live SSE event stream modal -->
<div class="full-modal" id="liveModal">
  <div class="full-modal-inner">
    <div class="header">
      <h2>📡 Live 事件流 — 实时 binlog</h2>
      <button class="close-btn" onclick="closeModal('liveModal');stopLive()">关闭</button>
    </div>
    <div class="body">
      <div class="live-status">
        <span class="dot" id="liveDot"></span>
        <span id="liveState">未连接</span>
        <span style="flex:1"></span>
        <span style="color:#6b7280;font-size:11px">已收 <span id="liveCount">0</span> 条</span>
        <button onclick="clearLive()" style="background:#6b7280;color:#fff;border:0;padding:4px 10px;border-radius:4px;cursor:pointer">清屏</button>
      </div>
      <div id="liveLog"></div>
    </div>
  </div>
</div>

<!-- Diffs search modal (跨 Redis + ClickHouse + Jaeger 跳转) -->
<div class="full-modal" id="diffsModal">
  <div class="full-modal-inner">
    <div class="header">
      <h2>🔍 Diffs · 冷热分层查询 + Trace 跳转</h2>
      <button class="close-btn" onclick="closeModal('diffsModal')">关闭</button>
    </div>
    <div class="body">
      <div class="graph-controls">
        <select id="diffState">
          <option value="">所有状态</option>
          <option value="open">open</option>
          <option value="acked">acked</option>
          <option value="resolved">resolved</option>
          <option value="false_positive">false_positive</option>
          <option value="expired">expired</option>
        </select>
        <input id="diffScript" placeholder="script_id 过滤" style="width:200px">
        <input id="diffType" placeholder="type 过滤" style="width:150px">
        <input id="diffFrom" type="datetime-local" style="width:180px">
        <input id="diffTo" type="datetime-local" style="width:180px">
        <button onclick="loadDiffs()">查询</button>
        <span id="diffsStats" style="color:#6b7280;font-size:12px"></span>
      </div>
      <div id="diffsTable" style="overflow:auto"></div>
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

// ─── 全局：generic full-modal 打开/关闭 ─────────────────────────────────
function openModal(id) { document.getElementById(id).classList.add('open'); }
function closeModal(id) { document.getElementById(id).classList.remove('open'); }

// ─── Dashboard view ───────────────────────────────────────────────────
async function openDashboard() {
  openModal('dashboardModal');
  const body = document.getElementById('dashboardBody');
  body.innerHTML = '<p style="text-align:center;color:#6b7280;padding:40px">加载中...</p>';
  try {
    const [stats, cdcStatus] = await Promise.all([
      api('/api/v1/diffs/_stats'),
      api('/api/v1/cdc/status?detailed=1').catch(() => ({})),
    ]);
    body.innerHTML = renderDashboard(stats, cdcStatus);
    // 异步加载跨年趋势 — 失败不阻塞主面板
    loadTrendChart();
  } catch (e) {
    body.innerHTML = '<p style="color:#dc2626">加载失败: ' + e + '</p>';
  }
}

function renderDashboard(stats, cdcStatus) {
  const c = stats.counts || {};
  const open = c.open || 0;
  const acked = c.acked || 0;
  const resolved = c.resolved || 0;
  const fp = c.false_positive || 0;
  const today = stats.last_24h_new || 0;
  const cdcSummary = cdcStatus.summary || {};

  let html = '<div class="kpi-grid">';
  html += kpiCard('Open', open, open > 0 ? 'alert' : 'ok');
  html += kpiCard('Acked', acked);
  html += kpiCard('Resolved', resolved, 'ok');
  html += kpiCard('False Positive', fp);
  html += kpiCard('今日新增', today, today > 50 ? 'danger' : '');
  html += '</div>';

  // CDC status row
  if (cdcSummary.total) {
    html += '<div class="kpi-grid" style="grid-template-columns:repeat(4,1fr)">';
    html += kpiCard('CDC Runners', cdcSummary.total);
    html += kpiCard('Running', cdcSummary.running, cdcSummary.running === cdcSummary.total ? 'ok' : 'alert');
    html += kpiCard('Stopped', cdcSummary.stopped, cdcSummary.stopped > 0 ? 'danger' : '');
    html += kpiCard('Max Lag', (cdcSummary.max_lag_seconds||0).toFixed(1) + 's',
      cdcSummary.max_lag_seconds > 60 ? 'danger' : cdcSummary.max_lag_seconds > 5 ? 'alert' : 'ok');
    html += '</div>';
  }

  // by_type / by_script bar lists
  html += '<div class="kpi-row-2col">';
  html += '<div><h3 style="font-size:14px;margin-bottom:8px">Top Diff Types</h3>' + renderBars(stats.by_type) + '</div>';
  html += '<div><h3 style="font-size:14px;margin-bottom:8px">Top Scripts</h3>' + renderBars(stats.by_script) + '</div>';
  html += '</div>';

  // 跨年趋势图 — 走 ClickHouse agg_by_day
  html += '<div style="margin-top:24px"><h3 style="font-size:14px;margin-bottom:8px">📈 跨年 Diff 趋势 (ClickHouse 冷归档)</h3>';
  html += '<div id="trendChartWrap" style="height:200px;background:#fafafa;border:1px solid #e5e7eb;border-radius:4px;padding:8px"><canvas id="trendChart"></canvas></div></div>';

  html += '<p style="margin-top:16px;font-size:11px;color:#9ca3af">Generated at ' + (stats.generated_at || '') + '</p>';
  return html;
}

// ─── 跨年趋势 Chart.js（接 ClickHouse agg_by_day）────────────────────
let trendChartInstance = null;
async function loadTrendChart() {
  const wrap = document.getElementById('trendChartWrap');
  if (!wrap) return;
  // 默认查最近 90 天
  const to = new Date();
  const from = new Date(to.getTime() - 90 * 24 * 3600 * 1000);
  const params = new URLSearchParams({
    from: from.toISOString().slice(0, 19),
    to: to.toISOString().slice(0, 19),
  });
  try {
    const r = await api('/api/v1/diffs/_agg_by_day?' + params.toString());
    const buckets = r.buckets || {};
    const days = Object.keys(buckets).sort();
    if (!days.length) {
      wrap.innerHTML = '<p style="text-align:center;color:#9ca3af;padding:60px">无冷归档数据 — 启用 CLICKHOUSE_URL + 等 7 天</p>';
      return;
    }
    if (trendChartInstance) trendChartInstance.destroy();
    const ctx = document.getElementById('trendChart').getContext('2d');
    trendChartInstance = new Chart(ctx, {
      type: 'bar',
      data: {
        labels: days,
        datasets: [{
          label: 'Diffs / day',
          data: days.map(d => buckets[d]),
          backgroundColor: 'rgba(22, 119, 255, 0.7)',
          borderColor: 'rgba(22, 119, 255, 1)',
          borderWidth: 1,
        }],
      },
      options: {
        responsive: true, maintainAspectRatio: false,
        plugins: { legend: { display: false } },
        scales: {
          y: { beginAtZero: true, ticks: { font: { size: 10 } } },
          x: { ticks: { font: { size: 9 }, maxRotation: 60, minRotation: 0 } },
        },
      },
    });
  } catch (e) {
    if ((e + '').indexOf('501') >= 0 || (e + '').indexOf('Not Implemented') >= 0) {
      wrap.innerHTML = '<p style="text-align:center;color:#9ca3af;padding:60px">📦 ClickHouse 冷归档未启用 — 设 CLICKHOUSE_URL 后启用</p>';
    } else {
      wrap.innerHTML = '<p style="text-align:center;color:#dc2626;padding:60px">趋势加载失败: ' + e + '</p>';
    }
  }
}

function kpiCard(label, value, kind) {
  return '<div class="kpi-card ' + (kind || '') + '"><div class="label">' + label + '</div><div class="value">' + value + '</div></div>';
}

function renderBars(items) {
  if (!items || !items.length) return '<p style="color:#9ca3af;font-size:12px">无数据</p>';
  const max = Math.max(...items.map(x => x.count), 1);
  return '<div class="kpi-bar-list">' +
    items.map(x => {
      const w = Math.max(2, (x.count / max) * 280);
      return '<div class="bar"><div class="name">' + x.key + '</div>' +
        '<div class="fill" style="width:' + w + 'px"></div>' +
        '<div class="count">' + x.count + '</div></div>';
    }).join('') + '</div>';
}

// ─── Graph view (cytoscape.js) ────────────────────────────────────────
let cyInstance = null;
async function openGraph() {
  openModal('graphModal');
  // 第一次打开时初始化 idx 选项
  const sel = document.getElementById('graphIdx');
  if (!sel.options.length) {
    try {
      const r = await api('/api/v1/meta/idx_keys');
      (r.index_keys || ['pi_id', 'order_id', 'merchant_id', 'trace_id']).forEach(k => {
        const opt = document.createElement('option');
        opt.value = k; opt.textContent = k;
        sel.appendChild(opt);
      });
    } catch (e) {
      ['pi_id', 'order_id', 'merchant_id', 'trace_id'].forEach(k => {
        const opt = document.createElement('option');
        opt.value = k; opt.textContent = k; sel.appendChild(opt);
      });
    }
  }
}

async function loadGraph() {
  const idx = document.getElementById('graphIdx').value;
  const val = document.getElementById('graphVal').value.trim();
  const depth = document.getElementById('graphDepth').value || 3;
  if (!val) { alert('请填 value (如 pi_xxx)'); return; }
  try {
    const r = await api('/api/v1/graph?index=' + encodeURIComponent(idx) +
      '&value=' + encodeURIComponent(val) + '&depth=' + depth);
    document.getElementById('graphStats').textContent =
      r.stats.nodes + ' 节点 / ' + r.stats.edges + ' 边 (depth=' + r.stats.depth_reached + ')' +
      (r.stats.truncated ? ' [TRUNCATED]' : '');
    renderGraphCytoscape(r);
  } catch (e) {
    document.getElementById('graphStats').textContent = '加载失败: ' + e;
  }
}

function renderGraphCytoscape(g) {
  const container = document.getElementById('graphCanvas');
  // 按服务名分配颜色
  const palette = ['#1677ff', '#10b981', '#f59e0b', '#dc2626', '#8b5cf6', '#06b6d4', '#ec4899'];
  const svcColor = {};
  let cIdx = 0;
  (g.nodes || []).forEach(n => {
    if (!svcColor[n.svc]) { svcColor[n.svc] = palette[cIdx % palette.length]; cIdx++; }
  });
  const elements = [];
  (g.nodes || []).forEach(n => {
    elements.push({
      data: {
        id: n.id,
        label: n.svc + '\\n' + n.table + '\\n' + n.pk,
        color: svcColor[n.svc],
        title: JSON.stringify(n.data, null, 2),
      },
    });
  });
  (g.edges || []).forEach(e => {
    elements.push({ data: { source: e.src, target: e.dst, label: e.via } });
  });
  if (cyInstance) cyInstance.destroy();
  cyInstance = cytoscape({
    container, elements, layout: { name: 'breadthfirst', directed: false, spacingFactor: 1.5 },
    style: [
      { selector: 'node',
        style: {
          'background-color': 'data(color)',
          'label': 'data(label)',
          'text-wrap': 'wrap',
          'color': '#fff',
          'font-size': 10,
          'text-halign': 'center',
          'text-valign': 'center',
          'width': 90, 'height': 60,
          'shape': 'roundrectangle',
          'border-width': 1, 'border-color': '#fff',
        } },
      { selector: 'edge',
        style: {
          'curve-style': 'bezier',
          'line-color': '#9ca3af',
          'target-arrow-color': '#9ca3af',
          'target-arrow-shape': 'triangle',
          'label': 'data(label)',
          'font-size': 9,
          'color': '#4b5563',
          'text-background-color': '#fff',
          'text-background-opacity': 0.8,
          'text-background-padding': 2,
        } },
    ],
  });
  cyInstance.on('tap', 'node', evt => {
    const n = evt.target.data();
    alert(n.id + '\\n\\n' + n.title);
  });
}

// ─── DSL templates view ───────────────────────────────────────────────
let dslTemplatesCache = null;
async function openTemplates() {
  openModal('dslModal');
  if (!dslTemplatesCache) {
    const r = await api('/api/v1/dsl/templates');
    dslTemplatesCache = r.templates || [];
  }
  const list = document.getElementById('dslTplList');
  list.innerHTML = dslTemplatesCache.map(t =>
    '<div class="tpl" data-id="' + t.id + '"><div class="name">' + t.name + '</div>' +
    '<div class="desc">' + t.description + '</div></div>'
  ).join('');
  list.querySelectorAll('.tpl').forEach(el => {
    el.onclick = () => selectTemplate(el.dataset.id);
  });
}

function selectTemplate(id) {
  document.querySelectorAll('#dslTplList .tpl').forEach(el => {
    el.classList.toggle('active', el.dataset.id === id);
  });
  const t = dslTemplatesCache.find(x => x.id === id);
  const form = document.getElementById('dslForm');
  let html = '<h3 style="font-size:14px">' + t.name + '</h3>';
  html += '<p style="font-size:12px;color:#6b7280">' + t.description + '</p>';
  t.fields.forEach(f => {
    let input;
    if (f.type === 'select') {
      input = '<select name="' + f.name + '">' +
        f.options.map(o => '<option value="' + o + '">' + o + '</option>').join('') + '</select>';
    } else if (f.type === 'number') {
      input = '<input type="number" name="' + f.name + '" placeholder="' + (f.hint || '') + '" value="' + (f.default || '') + '">';
    } else {
      input = '<input type="text" name="' + f.name + '" placeholder="' + (f.hint || '') + '" value="' + (f.default || '') + '">';
    }
    html += '<div class="field"><label>' + f.label + (f.required ? ' *' : '') + '</label>' + input + '</div>';
    if (f.hint) html += '<div class="field"><span></span><span class="hint">' + f.hint + '</span></div>';
  });
  html += '<div class="footer">' +
    '<button onclick="renderTemplate(\\'' + id + '\\')">生成代码 → 编辑器</button>' +
    '<button class="secondary" onclick="closeModal(\\'dslModal\\')">取消</button></div>';
  form.innerHTML = html;
}

async function renderTemplate(id) {
  const form = document.getElementById('dslForm');
  const params = {};
  form.querySelectorAll('input,select').forEach(inp => {
    params[inp.name] = inp.value;
  });
  try {
    const r = await apiPost('/api/v1/dsl/render', { template_id: id, params });
    if (r.error) { alert('生成失败: ' + r.error); return; }
    // 把代码塞进 Monaco editor + 关闭 modal
    if (window.editor) {
      editorValue.set(r.code);
      // starter 名字也覆盖一下
      document.getElementById('metaName').value = id + ' (从模板生成)';
    }
    closeModal('dslModal');
    status('✓ 模板代码已填入编辑器，调整后保存', 'ok');
  } catch (e) {
    alert('生成失败: ' + e);
  }
}

// ─── Live SSE stream ──────────────────────────────────────────────────
let sseSource = null;
let sseCount = 0;
function openLive() {
  openModal('liveModal');
  startLive();
}

function startLive() {
  if (sseSource) return;
  document.getElementById('liveState').textContent = '连接中...';
  document.getElementById('liveDot').classList.remove('connected');
  sseSource = new EventSource('/api/v1/events/stream');
  sseSource.addEventListener('connect', e => {
    document.getElementById('liveState').textContent = '已连接';
    document.getElementById('liveDot').classList.add('connected');
  });
  sseSource.addEventListener('binlog', e => {
    sseCount++;
    document.getElementById('liveCount').textContent = sseCount;
    let data;
    try { data = JSON.parse(e.data); } catch { data = { raw: e.data }; }
    const log = document.getElementById('liveLog');
    const div = document.createElement('div');
    div.className = 'ev';
    const ts = new Date().toISOString().slice(11, 23);
    div.innerHTML = '<span class="ts">' + ts + '</span>' +
      '<span class="svc">' + (data.svc || '?') + '</span>/<span class="table">' + (data.table || '?') + '</span> ' +
      '<span class="op">' + (data.op || '?') + '</span> pk=<span class="pk">' + (data.pk || '?') + '</span>';
    log.appendChild(div);
    // 保留最近 500 条
    while (log.children.length > 500) log.removeChild(log.firstChild);
    log.scrollTop = log.scrollHeight;
  });
  sseSource.onerror = () => {
    document.getElementById('liveState').textContent = '连接断开';
    document.getElementById('liveDot').classList.remove('connected');
    // 浏览器自动重连
  };
}

function stopLive() {
  if (sseSource) { sseSource.close(); sseSource = null; }
  document.getElementById('liveState').textContent = '未连接';
  document.getElementById('liveDot').classList.remove('connected');
}

function clearLive() {
  document.getElementById('liveLog').innerHTML = '';
  sseCount = 0;
  document.getElementById('liveCount').textContent = '0';
}

// ─── Diffs search (跨 Redis hot + ClickHouse cold + Jaeger 跳) ────────
async function openDiffs() {
  openModal('diffsModal');
  // 默认查最近 7d（Redis hot）
  const now = new Date();
  const from = new Date(now.getTime() - 7 * 24 * 3600 * 1000);
  document.getElementById('diffFrom').value = toLocalInput(from);
  document.getElementById('diffTo').value = toLocalInput(now);
  loadDiffs();
}

function toLocalInput(d) {
  const pad = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + pad(d.getMonth()+1) + '-' + pad(d.getDate()) +
    'T' + pad(d.getHours()) + ':' + pad(d.getMinutes());
}

async function loadDiffs() {
  const params = new URLSearchParams();
  const st = document.getElementById('diffState').value;
  const sc = document.getElementById('diffScript').value.trim();
  const ty = document.getElementById('diffType').value.trim();
  const from = document.getElementById('diffFrom').value;
  const to = document.getElementById('diffTo').value;
  if (st) params.set('state', st);
  if (sc) params.set('script_id', sc);
  if (ty) params.set('type', ty);
  if (from) params.set('from', from + ':00');
  if (to) params.set('to', to + ':00');
  params.set('limit', '200');
  document.getElementById('diffsTable').innerHTML = '<p style="text-align:center;color:#6b7280;padding:20px">查询中...</p>';
  try {
    const r = await api('/api/v1/diffs/_search?' + params.toString());
    document.getElementById('diffsStats').textContent =
      r.total + ' 条 (热=' + r.hot_count + ' 冷=' + r.cold_count + ')';
    renderDiffsTable(r);
  } catch (e) {
    // archiver 没配置时返 501 → 降级 hot only via /api/v1/diffs?state=
    if ((e + '').indexOf('501') >= 0 || (e + '').indexOf('Not Implemented') >= 0) {
      document.getElementById('diffsStats').textContent = '冷归档未启用 — 仅查 hot 7d';
      try {
        const r2 = await api('/api/v1/diffs?state=' + (st || 'open') + '&limit=200');
        const fake = { diffs: r2.diffs || [], hot_count: (r2.diffs || []).length, cold_count: 0 };
        renderDiffsTable(fake);
      } catch (e2) {
        document.getElementById('diffsTable').innerHTML = '<p style="color:#dc2626">查询失败: ' + e2 + '</p>';
      }
      return;
    }
    document.getElementById('diffsTable').innerHTML = '<p style="color:#dc2626">查询失败: ' + e + '</p>';
  }
}

function renderDiffsTable(r) {
  const diffs = r.diffs || [];
  if (!diffs.length) {
    document.getElementById('diffsTable').innerHTML = '<p style="text-align:center;color:#9ca3af;padding:30px">无匹配 diff</p>';
    return;
  }
  // 简单 tier 标记：cold 按 cold_count 推断，但准确做法是按 updated_at < now-7d 标 cold
  const hotCutoff = Date.now() - 7 * 24 * 3600 * 1000;
  let html = '<table class="diffs-table"><thead><tr>' +
    '<th>ID</th><th>State</th><th>Type</th><th>Script</th><th>Key</th>' +
    '<th>Updated</th><th>Trace</th></tr></thead><tbody>';
  diffs.forEach(d => {
    const upd = d.updated_at ? new Date(d.updated_at) : null;
    const tier = upd && upd.getTime() < hotCutoff ? 'cold' : 'hot';
    const tid = (d.detail && (d.detail.trace_id || d.detail.x_trace_id)) || '';
    html += '<tr>' +
      '<td class="col-id">' + d.id + '</td>' +
      '<td class="col-state ' + d.state + '">' + d.state +
        '<span class="tier-badge ' + tier + '">' + tier + '</span></td>' +
      '<td>' + (d.type || '') + '</td>' +
      '<td>' + (d.script_id || '') + '</td>' +
      '<td>' + (d.key || '') + '</td>' +
      '<td>' + (upd ? upd.toISOString().slice(0,19).replace('T',' ') : '') + '</td>' +
      '<td>' + (tid
        ? '<button class="jaeger-btn" onclick="jumpJaeger(\\'' + d.id + '\\')">🔍 Jaeger</button>'
        : '<button class="jaeger-btn" disabled title="此 diff 无 trace_id">🔍 Jaeger</button>') +
      '</td>' +
      '</tr>';
  });
  html += '</tbody></table>';
  document.getElementById('diffsTable').innerHTML = html;
}

async function jumpJaeger(diffID) {
  try {
    const r = await api('/api/v1/diffs/' + encodeURIComponent(diffID) + '/trace');
    if (!r.jaeger_url) {
      alert('该 diff 无 trace_id：' + (r.reason || '请脚本作者在 detail 里加 trace_id'));
      return;
    }
    window.open(r.jaeger_url, '_blank', 'noopener');
  } catch (e) {
    alert('跳转失败: ' + e);
  }
}

// ─── Bind 4 navbar buttons ─────────────────────────────────────────────
document.getElementById('btnDashboard').onclick = openDashboard;
document.getElementById('btnDiffs').onclick = openDiffs;
document.getElementById('btnGraph').onclick = openGraph;
document.getElementById('btnTemplates').onclick = openTemplates;
document.getElementById('btnLive').onclick = openLive;

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
          { value: '**ctx** — *recon.Context*' },
          { value: '当前对账脚本运行的上下文。属性：' + (symbolsCache.ctx || []).map(s => s.name).join(', ') },
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
            { value: (mod.members || []).map(m => '- ' + m.name + ' (' + m.type + ')').join('\n') },
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
        { value: '**' + sym.name + '** — *' + sym.type + '*' + (owner ? '  \n_owner: ' + owner + '_' : '') },
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
